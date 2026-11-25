// Package agentrun defines how Venture OS asks an agent runtime to do one unit of work, and watches it.
//
// DRAFT. This is the interface from docs/AGENT-TASKS-DESIGN.md section 5, written BEFORE the Hermes spike
// (docs/HERMES-SPIKE.md) has been run. Questions 4 and 5 of the spike (per-run tokens, asynchronous and idempotent
// starts) may change it. The fake runner and the contract suite exist so that whatever runs real work, today or later,
// is held to the same rules.
//
// A Runner knows nothing about approvals, spend caps or the database. Everything an agent may do goes through the MCP
// bus, using the token in StartRequest; the bus is what enforces tenant, tool allow-list, step limit and cancellation
// (the agent_task_events trigger in db/schema_tasks.sql). A runner reports a SUMMARY; the platform never acts on it.
//
// Not to be confused with ports.AgentRuntime, which provisions Hermes profiles and skills.
package agentrun

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/mrbiz/venture-os/internal/ports"
)

// Runner starts and watches one attempt of one task.
type Runner interface {
	// Start begins the run. It MUST be idempotent on req.Ref: a second call with the same Ref and an equivalent request
	// returns nil without starting another run; the same Ref with a different request is ErrConflict. A Start that
	// returns an error wrapping ErrTransient may or may not have started the run: the caller asks Status(ref).
	Start(ctx context.Context, req StartRequest) error

	// Status reports where the run is. A Ref that was never started is ErrNotFound. Once a run is no longer Running,
	// its Status never changes again.
	Status(ctx context.Context, ref Ref) (Status, error)

	// Cancel stops the run. Idempotent: cancelling a finished, already-cancelled or unknown run is not an error. A run
	// that finished before the cancel took effect keeps its real outcome.
	Cancel(ctx context.Context, ref Ref) error
}

// Ref names one attempt of one task. The dispatcher derives it, so after a crash or a timed-out Start it can ask
// "did that start?" without having received anything back.
type Ref string

// RefFor is "<task id>.<attempt>". The same formula is used by agent_task_claim in db/schema_tasks.sql (a test keeps
// them equal).
func RefFor(task uuid.UUID, attempt int) Ref { return Ref(fmt.Sprintf("%s.%d", task, attempt)) }

// StartRequest is everything a runtime needs for one attempt.
type StartRequest struct {
	Ref      Ref
	Tenant   ports.TenantInfo
	AgentKey string
	Profile  ports.Ref // the runtime profile created at provisioning
	Goal     string
	// Untrusted means the goal embeds text typed by an outsider (a customer email, a webhook body). The runtime must
	// present that text to the agent as data, never as instructions.
	Untrusted bool
	// Token is the short-lived, attempt-scoped MCP token. The runner passes it to the agent and logs nothing of it.
	Token  string
	Limits Limits
}

// Limits bound a run. The bus enforces the same limits independently; these are the runtime's own stops.
type Limits struct {
	MaxSteps int       // tool calls
	Deadline time.Time // absolute; the runtime must stop the run itself at this time
}

// State is where a run stands.
type State int

const (
	Running State = iota + 1
	Succeeded
	Failed
	Cancelled
)

func (s State) String() string {
	switch s {
	case Running:
		return "running"
	case Succeeded:
		return "succeeded"
	case Failed:
		return "failed"
	case Cancelled:
		return "cancelled"
	}
	return fmt.Sprintf("state(%d)", int(s))
}

// Done reports whether the state is final.
func (s State) Done() bool { return s == Succeeded || s == Failed || s == Cancelled }

// Status is a snapshot of a run.
type Status struct {
	State State
	// Summary is the agent's own closing words, bounded by MaxSummary. It is shown to people, never acted on.
	Summary string
	Usage   Usage
	Err     *RunError // set when State == Failed
}

// Usage is what the run cost. Reported is false when the runtime gave no figures; zero must not look like "free".
type Usage struct {
	Reported     bool
	InputTokens  int
	OutputTokens int
	CostMinor    int64
	Currency     string
}

// RunError says why a run failed. Code is stable and machine-readable; Detail is bounded and scrubbed.
type RunError struct {
	Code   string
	Detail string
}

func (e *RunError) Error() string { return e.Code + ": " + e.Detail }

// Stable failure codes. A runtime may add its own, in lower_snake_case.
const (
	CodeDeadline    = "deadline"            // the run's deadline passed
	CodeStepLimit   = "step_limit"          // the run tried to use more tool calls than allowed
	CodeRuntime     = "runtime_unavailable" // the runtime itself could not be reached or crashed
	CodeModel       = "model_error"         // the model provider failed
	CodeAgentFailed = "agent_error"         // the agent ended the run with an error
	CodeStepDenied  = "tool_denied"         // the bus refused a call (not allowed, task cancelled, stale attempt)
)

// Bounds shared with the database (agent_tasks.result_summary and error_detail).
const (
	MaxSummary = 4000
	MaxDetail  = 2000
)

var (
	// ErrNotFound: no run with that Ref was ever started.
	ErrNotFound = errors.New("agentrun: no such run")
	// ErrInvalid: the request could never be valid. Retrying it unchanged cannot help.
	ErrInvalid = errors.New("agentrun: invalid request")
	// ErrConflict: the Ref exists with a different request.
	ErrConflict = errors.New("agentrun: ref already used for a different request")
	// ErrTransient: retry the call; the run's fate may not be known.
	ErrTransient = ports.ErrTransient
)

var agentKeyRe = regexp.MustCompile(`^[a-z][a-z0-9_-]{1,62}$`)

// Validate rejects a request that could never run. now is the caller's clock.
func (r StartRequest) Validate(now time.Time) error {
	bad := func(format string, a ...any) error {
		return fmt.Errorf("%w: "+format, append([]any{ErrInvalid}, a...)...)
	}
	switch {
	case strings.TrimSpace(string(r.Ref)) == "" || len(r.Ref) > 200:
		return bad("ref is required (at most 200 characters)")
	case r.Tenant.ID == uuid.Nil:
		return bad("tenant is required")
	case !agentKeyRe.MatchString(r.AgentKey):
		return bad("agent key %q is not valid", r.AgentKey)
	case strings.TrimSpace(r.Goal) == "":
		return bad("goal is required")
	case utf8.RuneCountInString(r.Goal) > 4000:
		return bad("goal is longer than 4000 characters")
	case strings.TrimSpace(r.Token) == "":
		return bad("token is required")
	case r.Limits.MaxSteps < 1 || r.Limits.MaxSteps > 200:
		return bad("max steps must be between 1 and 200")
	case r.Limits.Deadline.IsZero() || !r.Limits.Deadline.After(now):
		return bad("deadline must be in the future")
	}
	return nil
}

// Bound cuts s to at most n characters (not bytes), never in the middle of a character, marking the cut.
func Bound(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	if n <= 1 {
		return string(r[:n])
	}
	return string(r[:n-1]) + "…"
}

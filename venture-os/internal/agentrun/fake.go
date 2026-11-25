package agentrun

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"
)

// Goals the default script understands. They let a test (or the contract suite) ask the fake for a behaviour in the
// same way it would ask a real runtime: through the goal text.
const (
	GoalQuick       = "fake:quick"        // finishes at once with summary "ok" and reported usage
	GoalBlock       = "fake:block"        // never finishes by itself: only a cancel or the deadline ends it
	GoalChatty      = "fake:chatty"       // makes read-only tool calls until something stops it
	GoalWrite       = "fake:write"        // makes one write call, then finishes
	GoalFail        = "fake:fail"         // ends with an error
	GoalLongSummary = "fake:long-summary" // finishes with a summary far over the bound
)

// ErrToolDenied is what a step returns when the bus refuses a call (not allowed, task cancelled, stale attempt, ...).
var ErrToolDenied = errors.New("agentrun: the bus refused the tool call")

var errStepLimit = errors.New("agentrun: step limit reached")

// StepFunc is how a scripted agent makes one tool call. It enforces the run's step limit and consults Fake.OnCall.
type StepFunc func(ctx context.Context, tool string, readOnly bool) error

// Result is what a script ends with.
type Result struct {
	Summary string
	Usage   Usage
}

// Script is the behaviour of a fake agent.
type Script func(ctx context.Context, req StartRequest, step StepFunc) (Result, error)

// Call is one tool call a fake agent made.
type Call struct {
	Tool     string
	ReadOnly bool
}

// Fake is an in-memory Runner for tests. It runs a Script per Start in its own goroutine and enforces, independently of
// the script, everything the Runner contract promises: idempotent Start, the deadline, the step limit, cancellation,
// bounded summaries and final states that never change. It never starts a process and never makes a network call.
type Fake struct {
	// Script defines the agent's behaviour. Nil means DefaultScript.
	Script Script
	// OnCall plays the MCP bus: it is called for every tool call that is within the step limit, and may refuse it. Tests
	// connect it to the database to prove the database refuses a cancelled task's calls.
	OnCall func(ctx context.Context, req StartRequest, tool string, readOnly bool) error
	// Now is the clock used to validate deadlines. Nil means time.Now.
	Now func() time.Time

	// beforeFinish is a test seam: it runs after a script has ended and before its outcome is recorded.
	beforeFinish func()

	mu       sync.Mutex
	runs     map[Ref]*fakeRun
	launched map[Ref]int // how many times a run was truly started for each Ref
	wg       sync.WaitGroup
}

type fakeRun struct {
	req    StartRequest
	cancel context.CancelFunc
	status Status
	steps  int
	calls  []Call
}

var _ Runner = (*Fake)(nil)

// NewFake returns a Fake that behaves according to script (nil for DefaultScript).
func NewFake(script Script) *Fake { return &Fake{Script: script} }

func (f *Fake) now() time.Time {
	if f.Now != nil {
		return f.Now()
	}
	return time.Now()
}

// sameRequest is the idempotency comparison. The token and deadline are deliberately left out: a retried Start may
// carry a re-minted token, and the run keeps the deadline it began with.
func sameRequest(a, b StartRequest) bool {
	return a.Tenant.ID == b.Tenant.ID && a.AgentKey == b.AgentKey && a.Profile == b.Profile && a.Goal == b.Goal &&
		a.Untrusted == b.Untrusted && a.Limits.MaxSteps == b.Limits.MaxSteps
}

func (f *Fake) Start(ctx context.Context, req StartRequest) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := req.Validate(f.now()); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if run, ok := f.runs[req.Ref]; ok {
		if !sameRequest(run.req, req) {
			return ErrConflict
		}
		return nil // already started: nothing more happens
	}
	if f.runs == nil {
		f.runs = map[Ref]*fakeRun{}
	}
	// The run outlives the caller's ctx on purpose: Start returns at once and the work continues.
	runCtx, cancel := context.WithDeadline(context.Background(), req.Limits.Deadline)
	run := &fakeRun{req: req, cancel: cancel, status: Status{State: Running}}
	f.runs[req.Ref] = run
	if f.launched == nil {
		f.launched = map[Ref]int{}
	}
	f.launched[req.Ref]++

	script := f.Script
	if script == nil {
		script = DefaultScript
	}
	type outcome struct {
		res Result
		err error
	}
	done := make(chan outcome, 1)
	f.wg.Add(2)
	go func() {
		defer f.wg.Done()
		res, err := script(runCtx, req, f.stepFor(runCtx, run))
		done <- outcome{res, err}
	}()
	go func() {
		defer f.wg.Done()
		defer cancel()
		select {
		case o := <-done:
			if f.beforeFinish != nil {
				f.beforeFinish()
			}
			f.finish(run, runCtx, o.res, o.err)
		case <-runCtx.Done():
			// The deadline passed (or Cancel ran) while the script was still going. Settle the status now; the script
			// is told through its context and its goroutine is waited for in Close.
			f.finish(run, runCtx, Result{}, runCtx.Err())
		}
	}()
	return nil
}

func (f *Fake) stepFor(ctx context.Context, run *fakeRun) StepFunc {
	return func(_ context.Context, tool string, readOnly bool) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		f.mu.Lock()
		if run.steps >= run.req.Limits.MaxSteps {
			f.mu.Unlock()
			return errStepLimit
		}
		run.steps++
		run.calls = append(run.calls, Call{Tool: tool, ReadOnly: readOnly})
		f.mu.Unlock()
		if f.OnCall != nil {
			if err := f.OnCall(ctx, run.req, tool, readOnly); err != nil {
				return errors.Join(ErrToolDenied, err)
			}
		}
		return nil
	}
}

// finish records the outcome unless the run is already settled (a cancel, or an earlier finish, won).
func (f *Fake) finish(run *fakeRun, ctx context.Context, res Result, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if run.status.State.Done() {
		return
	}
	st := Status{Usage: res.Usage}
	switch {
	case err == nil:
		st.State = Succeeded
		st.Summary = Bound(res.Summary, MaxSummary)
	case errors.Is(err, errStepLimit):
		st.State = Failed
		st.Err = &RunError{Code: CodeStepLimit, Detail: "the agent used all of its tool calls"}
	case errors.Is(err, ErrToolDenied):
		st.State = Failed
		st.Err = &RunError{Code: CodeStepDenied, Detail: Bound(err.Error(), MaxDetail)}
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded):
		st.State = Failed
		st.Err = &RunError{Code: CodeDeadline, Detail: "the run did not finish before its deadline"}
	case errors.Is(err, context.Canceled):
		st.State = Cancelled
	default:
		st.State = Failed
		st.Err = &RunError{Code: CodeAgentFailed, Detail: Bound(err.Error(), MaxDetail)}
	}
	run.status = st
}

func (f *Fake) Status(ctx context.Context, ref Ref) (Status, error) {
	if err := ctx.Err(); err != nil {
		return Status{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	run, ok := f.runs[ref]
	if !ok {
		return Status{}, ErrNotFound
	}
	return run.status, nil
}

func (f *Fake) Cancel(ctx context.Context, ref Ref) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	run, ok := f.runs[ref]
	if !ok || run.status.State.Done() {
		f.mu.Unlock()
		return nil
	}
	run.status = Status{State: Cancelled, Usage: run.status.Usage}
	cancel := run.cancel
	f.mu.Unlock()
	cancel()
	return nil
}

// Launches reports how many times the run with this Ref was actually started (0 or 1 for a correct runner).
func (f *Fake) Launches(ref Ref) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.launched[ref]
}

// CallCount is how many tool calls the run has made so far.
func (f *Fake) CallCount(ref Ref) int { return len(f.Calls(ref)) }

// Calls returns the tool calls the run made, in order.
func (f *Fake) Calls(ref Ref) []Call {
	f.mu.Lock()
	defer f.mu.Unlock()
	if run, ok := f.runs[ref]; ok {
		return append([]Call(nil), run.calls...)
	}
	return nil
}

// Close cancels every run and waits for every goroutine the fake started. A script that ignores its context makes Close
// block, which is the point: it shows up as a hung test rather than a leak.
func (f *Fake) Close() {
	f.mu.Lock()
	for _, run := range f.runs {
		run.cancel()
	}
	f.mu.Unlock()
	f.wg.Wait()
}

// DefaultScript implements the Goal* constants. Any other goal finishes with a summary naming it.
func DefaultScript(ctx context.Context, req StartRequest, step StepFunc) (Result, error) {
	switch {
	case strings.HasPrefix(req.Goal, GoalQuick):
		return Result{Summary: "ok", Usage: Usage{Reported: true, InputTokens: 120, OutputTokens: 30, CostMinor: 2, Currency: "USD"}}, nil
	case strings.HasPrefix(req.Goal, GoalBlock):
		<-ctx.Done()
		return Result{}, ctx.Err()
	case strings.HasPrefix(req.Goal, GoalChatty):
		for {
			if err := step(ctx, "crm_find_deals", true); err != nil {
				return Result{}, err
			}
			time.Sleep(time.Millisecond)
		}
	case strings.HasPrefix(req.Goal, GoalWrite):
		if err := step(ctx, "crm_create_deal", false); err != nil {
			return Result{}, err
		}
		return Result{Summary: "wrote one deal"}, nil
	case strings.HasPrefix(req.Goal, GoalFail):
		return Result{}, errors.New("the agent gave up: boom")
	case strings.HasPrefix(req.Goal, GoalLongSummary):
		return Result{Summary: strings.Repeat("é", 10000)}, nil
	}
	return Result{Summary: "did: " + req.Goal}, nil
}

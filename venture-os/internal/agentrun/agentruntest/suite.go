// Package agentruntest is the contract every agentrun.Runner must pass: the fake, and (once a live runtime exists) the
// Hermes runner. It defines "correct": idempotent start, honest status, a deadline and a step limit the runtime
// enforces itself, and a cancel that really stops the work.
//
// A runner is exercised through the goal text alone, as a real runtime would be, so the Harness says which goals make it
// finish at once, never finish, or keep calling tools.
package agentruntest

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/mrbiz/venture-os/internal/agentrun"
	"github.com/mrbiz/venture-os/internal/ports"
)

// Harness is a runner under test plus how to provoke the behaviours the suite needs.
type Harness struct {
	Runner agentrun.Runner
	// Quick is a goal on which the run succeeds promptly.
	Quick string
	// Block is a goal on which the run never ends by itself.
	Block string
	// Chatty is a goal on which the run keeps making tool calls until stopped.
	Chatty string
	// LongSummary is a goal whose summary is far longer than agentrun.MaxSummary. Optional: empty skips that check.
	LongSummary string
	// Launches reports how many times the run for a Ref was truly started. Optional: nil skips the duplicate check.
	Launches func(agentrun.Ref) int
	// CallCount reports how many tool calls the run has made so far. Optional: nil skips the checks that need it.
	CallCount func(agentrun.Ref) int
	// Close releases the runner. Optional.
	Close func()
}

const wait = 3 * time.Second

func request(h Harness, goal string) agentrun.StartRequest {
	return agentrun.StartRequest{
		Ref:      agentrun.RefFor(uuid.New(), 1),
		Tenant:   ports.TenantInfo{ID: uuid.New(), Slug: "acme", Name: "Acme"},
		AgentKey: "sales",
		Profile:  "venture-acme",
		Goal:     goal,
		Token:    "task-token",
		Limits:   agentrun.Limits{MaxSteps: 10, Deadline: time.Now().Add(10 * time.Second)},
	}
}

func waitFor(t *testing.T, r agentrun.Runner, ref agentrun.Ref, pred func(agentrun.Status) bool, what string) agentrun.Status {
	t.Helper()
	deadline := time.Now().Add(wait)
	for {
		st, err := r.Status(context.Background(), ref)
		if err != nil {
			t.Fatalf("Status(%s): %v", ref, err)
		}
		if pred(st) {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("gave up waiting for %s; last status %+v", what, st)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func finished(st agentrun.Status) bool { return st.State.Done() }

// Run executes the whole suite. newHarness is called once per subtest so no test sees another's runs.
func Run(t *testing.T, newHarness func(t *testing.T) Harness) {
	sub := func(name string, fn func(t *testing.T, h Harness)) {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			if h.Close != nil {
				t.Cleanup(h.Close)
			}
			fn(t, h)
		})
	}
	ctx := context.Background()

	sub("a run that succeeds reports its summary and says it is done", func(t *testing.T, h Harness) {
		req := request(h, h.Quick)
		if err := h.Runner.Start(ctx, req); err != nil {
			t.Fatal(err)
		}
		st := waitFor(t, h.Runner, req.Ref, finished, "the run to finish")
		if st.State != agentrun.Succeeded || st.Err != nil {
			t.Fatalf("status = %+v", st)
		}
		if utf8.RuneCountInString(st.Summary) > agentrun.MaxSummary {
			t.Fatalf("summary has %d characters; the bound is %d", utf8.RuneCountInString(st.Summary), agentrun.MaxSummary)
		}
	})

	sub("a ref that was never started is not found", func(t *testing.T, h Harness) {
		if _, err := h.Runner.Status(ctx, "never-started.1"); !errors.Is(err, agentrun.ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
	})

	sub("starting the same ref twice starts one run", func(t *testing.T, h Harness) {
		req := request(h, h.Quick)
		for i := 0; i < 3; i++ {
			if err := h.Runner.Start(ctx, req); err != nil {
				t.Fatalf("start %d: %v", i+1, err)
			}
		}
		waitFor(t, h.Runner, req.Ref, finished, "the run to finish")
		if h.Launches != nil {
			if n := h.Launches(req.Ref); n != 1 {
				t.Fatalf("the run was launched %d times; a repeated Start must not launch another", n)
			}
		}
	})

	sub("a repeated start may carry a fresh token", func(t *testing.T, h Harness) {
		req := request(h, h.Quick)
		if err := h.Runner.Start(ctx, req); err != nil {
			t.Fatal(err)
		}
		req.Token = "a-freshly-minted-token"
		if err := h.Runner.Start(ctx, req); err != nil {
			t.Fatalf("the same attempt with a re-minted token is the same run: %v", err)
		}
	})

	sub("the same ref with a different request is refused", func(t *testing.T, h Harness) {
		req := request(h, h.Quick)
		if err := h.Runner.Start(ctx, req); err != nil {
			t.Fatal(err)
		}
		other := req
		other.Goal = h.Block
		if err := h.Runner.Start(ctx, other); !errors.Is(err, agentrun.ErrConflict) {
			t.Fatalf("err = %v, want ErrConflict", err)
		}
		other = req
		other.Tenant.ID = uuid.New()
		if err := h.Runner.Start(ctx, other); !errors.Is(err, agentrun.ErrConflict) {
			t.Fatalf("a different tenant under the same ref: err = %v, want ErrConflict", err)
		}
	})

	sub("requests that could never run are refused up front", func(t *testing.T, h Harness) {
		cases := map[string]func(r *agentrun.StartRequest){
			"no ref":           func(r *agentrun.StartRequest) { r.Ref = "" },
			"no tenant":        func(r *agentrun.StartRequest) { r.Tenant.ID = uuid.Nil },
			"bad agent":        func(r *agentrun.StartRequest) { r.AgentKey = "Not Valid!" },
			"no goal":          func(r *agentrun.StartRequest) { r.Goal = "   " },
			"huge goal":        func(r *agentrun.StartRequest) { r.Goal = strings.Repeat("x", 4001) },
			"no token":         func(r *agentrun.StartRequest) { r.Token = "" },
			"no steps":         func(r *agentrun.StartRequest) { r.Limits.MaxSteps = 0 },
			"too many steps":   func(r *agentrun.StartRequest) { r.Limits.MaxSteps = 201 },
			"no deadline":      func(r *agentrun.StartRequest) { r.Limits.Deadline = time.Time{} },
			"deadline in past": func(r *agentrun.StartRequest) { r.Limits.Deadline = time.Now().Add(-time.Second) },
		}
		for name, mutate := range cases {
			req := request(h, h.Quick)
			mutate(&req)
			if err := h.Runner.Start(ctx, req); !errors.Is(err, agentrun.ErrInvalid) {
				t.Errorf("%s: err = %v, want ErrInvalid", name, err)
			}
			if req.Ref != "" {
				if _, err := h.Runner.Status(ctx, req.Ref); !errors.Is(err, agentrun.ErrNotFound) {
					t.Errorf("%s: a refused request must not leave a run behind (Status err = %v)", name, err)
				}
			}
		}
	})

	sub("cancel stops a running run, and cancelling again changes nothing", func(t *testing.T, h Harness) {
		req := request(h, h.Block)
		if err := h.Runner.Start(ctx, req); err != nil {
			t.Fatal(err)
		}
		waitFor(t, h.Runner, req.Ref, func(s agentrun.Status) bool { return s.State == agentrun.Running }, "the run to be running")
		if err := h.Runner.Cancel(ctx, req.Ref); err != nil {
			t.Fatal(err)
		}
		st := waitFor(t, h.Runner, req.Ref, finished, "the run to stop")
		if st.State != agentrun.Cancelled {
			t.Fatalf("state = %v, want cancelled", st.State)
		}
		if err := h.Runner.Cancel(ctx, req.Ref); err != nil {
			t.Fatalf("second cancel: %v", err)
		}
		if st2, _ := h.Runner.Status(ctx, req.Ref); st2.State != agentrun.Cancelled {
			t.Fatalf("state after a second cancel = %v", st2.State)
		}
	})

	sub("after a cancel the agent makes no more tool calls", func(t *testing.T, h Harness) {
		if h.CallCount == nil {
			t.Skip("harness cannot count tool calls")
		}
		req := request(h, h.Chatty)
		req.Limits.MaxSteps = 200
		if err := h.Runner.Start(ctx, req); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(wait)
		for h.CallCount(req.Ref) == 0 && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		if err := h.Runner.Cancel(ctx, req.Ref); err != nil {
			t.Fatal(err)
		}
		time.Sleep(50 * time.Millisecond) // let any in-flight call land
		settled := h.CallCount(req.Ref)
		time.Sleep(100 * time.Millisecond)
		if after := h.CallCount(req.Ref); after != settled {
			t.Fatalf("the agent kept calling tools after the cancel (%d, then %d)", settled, after)
		}
	})

	sub("cancelling something unknown or already finished is not an error and not a rewrite", func(t *testing.T, h Harness) {
		if err := h.Runner.Cancel(ctx, "never-started.1"); err != nil {
			t.Fatalf("cancel of an unknown ref: %v", err)
		}
		req := request(h, h.Quick)
		if err := h.Runner.Start(ctx, req); err != nil {
			t.Fatal(err)
		}
		waitFor(t, h.Runner, req.Ref, finished, "the run to finish")
		if err := h.Runner.Cancel(ctx, req.Ref); err != nil {
			t.Fatal(err)
		}
		if st, _ := h.Runner.Status(ctx, req.Ref); st.State != agentrun.Succeeded {
			t.Fatalf("a finished run was rewritten by a late cancel: %v", st.State)
		}
	})

	sub("the runtime stops a run at its deadline by itself", func(t *testing.T, h Harness) {
		req := request(h, h.Block)
		req.Limits.Deadline = time.Now().Add(150 * time.Millisecond)
		if err := h.Runner.Start(ctx, req); err != nil {
			t.Fatal(err)
		}
		st := waitFor(t, h.Runner, req.Ref, finished, "the deadline to end the run")
		if st.State != agentrun.Failed || st.Err == nil || st.Err.Code != agentrun.CodeDeadline {
			t.Fatalf("status = %+v, want failed with code %q", st, agentrun.CodeDeadline)
		}
	})

	sub("the runtime stops a run that uses more tool calls than allowed", func(t *testing.T, h Harness) {
		req := request(h, h.Chatty)
		req.Limits.MaxSteps = 3
		if err := h.Runner.Start(ctx, req); err != nil {
			t.Fatal(err)
		}
		st := waitFor(t, h.Runner, req.Ref, finished, "the step limit to end the run")
		if st.State != agentrun.Failed || st.Err == nil || st.Err.Code != agentrun.CodeStepLimit {
			t.Fatalf("status = %+v, want failed with code %q", st, agentrun.CodeStepLimit)
		}
		if h.CallCount != nil {
			if n := h.CallCount(req.Ref); n != 3 {
				t.Fatalf("the agent made %d tool calls with a limit of 3: exactly the limit may get through", n)
			}
		}
	})

	sub("a finished run's status never changes again", func(t *testing.T, h Harness) {
		req := request(h, h.Quick)
		if err := h.Runner.Start(ctx, req); err != nil {
			t.Fatal(err)
		}
		first := waitFor(t, h.Runner, req.Ref, finished, "the run to finish")
		for i := 0; i < 5; i++ {
			time.Sleep(10 * time.Millisecond)
			again, err := h.Runner.Status(ctx, req.Ref)
			if err != nil || again.State != first.State || again.Summary != first.Summary {
				t.Fatalf("status changed after the run finished: %+v then %+v (%v)", first, again, err)
			}
		}
	})

	sub("an over-long summary is cut to the bound", func(t *testing.T, h Harness) {
		if h.LongSummary == "" {
			t.Skip("harness has no long-summary goal")
		}
		req := request(h, h.LongSummary)
		if err := h.Runner.Start(ctx, req); err != nil {
			t.Fatal(err)
		}
		st := waitFor(t, h.Runner, req.Ref, finished, "the run to finish")
		if n := utf8.RuneCountInString(st.Summary); n == 0 || n > agentrun.MaxSummary {
			t.Fatalf("summary has %d characters; want 1..%d", n, agentrun.MaxSummary)
		}
		if !utf8.ValidString(st.Summary) {
			t.Fatal("the summary was cut in the middle of a character")
		}
	})

	sub("a cancelled caller context does not start a run", func(t *testing.T, h Harness) {
		c, cancel := context.WithCancel(ctx)
		cancel()
		req := request(h, h.Quick)
		if err := h.Runner.Start(c, req); err == nil {
			t.Fatal("Start with a cancelled context returned nil")
		}
		if _, err := h.Runner.Status(ctx, req.Ref); !errors.Is(err, agentrun.ErrNotFound) {
			t.Fatalf("a refused start left a run behind: %v", err)
		}
	})
}

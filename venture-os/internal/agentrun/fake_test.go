package agentrun_test

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/mrbiz/venture-os/internal/agentrun"
	"github.com/mrbiz/venture-os/internal/agentrun/agentruntest"
	"github.com/mrbiz/venture-os/internal/ports"
)

func TestTheFakePassesTheRunnerContract(t *testing.T) {
	agentruntest.Run(t, func(t *testing.T) agentruntest.Harness {
		f := agentrun.NewFake(nil)
		return agentruntest.Harness{
			Runner: f, Quick: agentrun.GoalQuick, Block: agentrun.GoalBlock, Chatty: agentrun.GoalChatty,
			LongSummary: agentrun.GoalLongSummary, Launches: f.Launches, CallCount: f.CallCount, Close: f.Close,
		}
	})
}

func req(goal string) agentrun.StartRequest {
	return agentrun.StartRequest{
		Ref: agentrun.RefFor(uuid.New(), 1), Tenant: ports.TenantInfo{ID: uuid.New(), Slug: "acme", Name: "Acme"},
		AgentKey: "sales", Profile: "venture-acme", Goal: goal, Token: "tok",
		Limits: agentrun.Limits{MaxSteps: 5, Deadline: time.Now().Add(5 * time.Second)},
	}
}

func settle(t *testing.T, f *agentrun.Fake, ref agentrun.Ref) agentrun.Status {
	t.Helper()
	for i := 0; i < 600; i++ {
		st, err := f.Status(context.Background(), ref)
		if err != nil {
			t.Fatal(err)
		}
		if st.State.Done() {
			return st
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the run did not finish")
	return agentrun.Status{}
}

func TestRefsAreTaskAndAttempt(t *testing.T) {
	id := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	if got := agentrun.RefFor(id, 2); got != "11111111-2222-3333-4444-555555555555.2" {
		t.Fatalf("RefFor = %q", got)
	}
}

func TestAWriteIsRecordedAsAWriteAndAReadAsARead(t *testing.T) {
	f := agentrun.NewFake(nil)
	defer f.Close()
	w, c := req(agentrun.GoalWrite), req(agentrun.GoalChatty)
	c.Limits.MaxSteps = 2
	for _, r := range []agentrun.StartRequest{w, c} {
		if err := f.Start(context.Background(), r); err != nil {
			t.Fatal(err)
		}
		settle(t, f, r.Ref)
	}
	if calls := f.Calls(w.Ref); len(calls) != 1 || calls[0].Tool != "crm_create_deal" || calls[0].ReadOnly {
		t.Fatalf("write run calls = %+v", calls)
	}
	if calls := f.Calls(c.Ref); len(calls) != 2 || !calls[0].ReadOnly {
		t.Fatalf("chatty run calls = %+v: exactly MaxSteps calls get through, then the limit applies", calls)
	}
}

func TestTheBusCanRefuseACallAndTheRunFailsWithThatReason(t *testing.T) {
	f := agentrun.NewFake(nil)
	defer f.Close()
	f.OnCall = func(context.Context, agentrun.StartRequest, string, bool) error {
		return errors.New("task is not running")
	}
	r := req(agentrun.GoalWrite)
	if err := f.Start(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	st := settle(t, f, r.Ref)
	if st.State != agentrun.Failed || st.Err == nil || st.Err.Code != agentrun.CodeStepDenied || !strings.Contains(st.Err.Detail, "task is not running") {
		t.Fatalf("status = %+v", st)
	}
}

func TestAnAgentErrorIsReportedBoundedAndWithoutCrashingTheRunner(t *testing.T) {
	f := agentrun.NewFake(func(context.Context, agentrun.StartRequest, agentrun.StepFunc) (agentrun.Result, error) {
		return agentrun.Result{}, errors.New(strings.Repeat("é", 5000))
	})
	defer f.Close()
	r := req("anything")
	_ = f.Start(context.Background(), r)
	st := settle(t, f, r.Ref)
	if st.State != agentrun.Failed || st.Err.Code != agentrun.CodeAgentFailed || utf8.RuneCountInString(st.Err.Detail) != agentrun.MaxDetail {
		t.Fatalf("status = %+v (detail %d characters)", st, utf8.RuneCountInString(st.Err.Detail))
	}
}

func TestUsageIsOnlyClaimedWhenTheScriptReportsIt(t *testing.T) {
	f := agentrun.NewFake(nil)
	defer f.Close()
	q, w := req(agentrun.GoalQuick), req(agentrun.GoalWrite)
	for _, r := range []agentrun.StartRequest{q, w} {
		_ = f.Start(context.Background(), r)
	}
	if st := settle(t, f, q.Ref); !st.Usage.Reported || st.Usage.InputTokens == 0 {
		t.Fatalf("quick run usage = %+v", st.Usage)
	}
	if st := settle(t, f, w.Ref); st.Usage.Reported {
		t.Fatalf("a run that gave no figures must not claim usage: %+v", st.Usage)
	}
}

func TestADeadlineIsEnforcedEvenWhenTheScriptIgnoresItsContext(t *testing.T) {
	release := make(chan struct{})
	f := agentrun.NewFake(func(context.Context, agentrun.StartRequest, agentrun.StepFunc) (agentrun.Result, error) {
		<-release // a stubborn agent: never looks at ctx
		return agentrun.Result{Summary: "too late"}, nil
	})
	r := req("stubborn")
	r.Limits.Deadline = time.Now().Add(100 * time.Millisecond)
	if err := f.Start(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	st := settle(t, f, r.Ref)
	if st.State != agentrun.Failed || st.Err.Code != agentrun.CodeDeadline {
		t.Fatalf("status = %+v: the runner, not the script, has to enforce the deadline", st)
	}
	close(release)
	f.Close()
	if after, _ := f.Status(context.Background(), r.Ref); after.State != agentrun.Failed || after.Summary != "" {
		t.Fatalf("a script that finishes after the deadline must not rewrite the outcome: %+v", after)
	}
}

func TestFinishingAndCancellingAtTheSameTimeLeavesExactlyOneOutcome(t *testing.T) {
	for i := 0; i < 50; i++ {
		f := agentrun.NewFake(nil)
		r := req(agentrun.GoalQuick)
		_ = f.Start(context.Background(), r)
		_ = f.Cancel(context.Background(), r.Ref)
		st := settle(t, f, r.Ref)
		if st.State != agentrun.Succeeded && st.State != agentrun.Cancelled {
			t.Fatalf("round %d: state %v", i, st.State)
		}
		for j := 0; j < 3; j++ {
			again, _ := f.Status(context.Background(), r.Ref)
			if again.State != st.State {
				t.Fatalf("round %d: outcome flipped from %v to %v", i, st.State, again.State)
			}
		}
		f.Close()
	}
}

func TestStartReturnsAtOnceAndTheWorkContinuesAfterTheCallersContextEnds(t *testing.T) {
	f := agentrun.NewFake(nil)
	defer f.Close()
	ctx, cancel := context.WithCancel(context.Background())
	r := req(agentrun.GoalBlock)
	if err := f.Start(ctx, r); err != nil {
		t.Fatal(err)
	}
	cancel() // the HTTP request that started the run is over
	time.Sleep(30 * time.Millisecond)
	if st, _ := f.Status(context.Background(), r.Ref); st.State != agentrun.Running {
		t.Fatalf("state = %v: ending the caller's context must not end the run", st.State)
	}
}

func TestRunsLeaveNoGoroutinesBehind(t *testing.T) {
	before := runtime.NumGoroutine()
	for i := 0; i < 10; i++ {
		f := agentrun.NewFake(nil)
		for _, g := range []string{agentrun.GoalQuick, agentrun.GoalBlock, agentrun.GoalChatty, agentrun.GoalFail} {
			_ = f.Start(context.Background(), req(g))
		}
		time.Sleep(5 * time.Millisecond)
		f.Close()
	}
	time.Sleep(100 * time.Millisecond)
	if after := runtime.NumGoroutine(); after > before+2 {
		t.Fatalf("goroutines grew from %d to %d", before, after)
	}
}

func TestBoundCutsByCharactersAndMarksTheCut(t *testing.T) {
	if got := agentrun.Bound("short", 10); got != "short" {
		t.Fatalf("Bound(short) = %q", got)
	}
	got := agentrun.Bound(strings.Repeat("हि", 100), 10)
	if utf8.RuneCountInString(got) != 10 || !utf8.ValidString(got) || !strings.HasSuffix(got, "…") {
		t.Fatalf("Bound = %q (%d characters)", got, utf8.RuneCountInString(got))
	}
}

func TestAnAgentThatFinishesAfterBeingCancelledDoesNotUndoTheCancel(t *testing.T) {
	for i := 0; i < 40; i++ {
		f := agentrun.NewFake(func(ctx context.Context, _ agentrun.StartRequest, _ agentrun.StepFunc) (agentrun.Result, error) {
			<-ctx.Done()
			return agentrun.Result{Summary: "finished anyway"}, nil // ignores the cancel and reports success
		})
		r := req("stubborn")
		if err := f.Start(context.Background(), r); err != nil {
			t.Fatal(err)
		}
		_ = f.Cancel(context.Background(), r.Ref)
		f.Close() // waits for the script and the watcher to finish
		st, _ := f.Status(context.Background(), r.Ref)
		if st.State != agentrun.Cancelled || st.Summary != "" {
			t.Fatalf("round %d: status = %+v; a cancel decided by a person must stand", i, st)
		}
	}
}

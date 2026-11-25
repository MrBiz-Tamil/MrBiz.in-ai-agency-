package agentrun

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mrbiz/venture-os/internal/ports"
)

// The one window an external test cannot reach reliably: the script has returned success, and before that outcome is
// recorded a person cancels the run. The cancel is the person's decision and must stand.
func TestACancelInTheWindowBeforeASuccessIsRecordedStands(t *testing.T) {
	f := NewFake(nil)
	req := StartRequest{
		Ref: RefFor(uuid.New(), 1), Tenant: ports.TenantInfo{ID: uuid.New(), Slug: "a", Name: "A"},
		AgentKey: "sales", Profile: "p", Goal: GoalQuick, Token: "t",
		Limits: Limits{MaxSteps: 3, Deadline: time.Now().Add(5 * time.Second)},
	}
	hooked := make(chan struct{})
	f.beforeFinish = func() { _ = f.Cancel(context.Background(), req.Ref); close(hooked) }
	if err := f.Start(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	select {
	case <-hooked:
	case <-time.After(2 * time.Second):
		t.Fatal("the script's success was never recorded: the window this test needs did not open")
	}
	f.Close()
	st, _ := f.Status(context.Background(), req.Ref)
	if st.State != Cancelled {
		t.Fatalf("state = %v: a late success overwrote a cancel", st.State)
	}
}

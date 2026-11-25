package mcpbus_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	"github.com/mrbiz/venture-os/gen/venture/agent/v1/agentv1connect"
	approvalv1 "github.com/mrbiz/venture-os/gen/venture/approval/v1"
	connectionv1 "github.com/mrbiz/venture-os/gen/venture/connection/v1"
	"github.com/mrbiz/venture-os/gen/venture/connection/v1/connectionv1connect"
	controlsv1 "github.com/mrbiz/venture-os/gen/venture/controls/v1"
	"github.com/mrbiz/venture-os/gen/venture/controls/v1/controlsv1connect"
	"github.com/mrbiz/venture-os/internal/mcpbus"
	"github.com/mrbiz/venture-os/internal/tenancy"
	"github.com/mrbiz/venture-os/internal/testdb"
	"github.com/mrbiz/venture-os/internal/testenv"
)

func bearerFor(token string) connect.Interceptor {
	return connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(c context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			req.Header().Set("Authorization", "Bearer "+token)
			return next(c, req)
		}
	})
}

type collabEnv struct {
	*e2e
	connections connectionv1connect.ConnectionServiceClient
	controls    controlsv1connect.ControlsServiceClient
}

func newCollabE2E(t *testing.T, tools bool) collabEnv {
	t.Helper()
	stack := testenv.New(t, testenv.WithCollab())
	agent := agentv1connect.NewAgentServiceClient(http.DefaultClient, stack.URL)
	bus := httptest.NewServer(mcpbus.New(mcpbus.Server{Agent: agent, Signer: stack.Signer, EnableCollab: tools}))
	t.Cleanup(bus.Close)
	opt := connect.WithInterceptors(bearerFor("tok-founder"))
	return collabEnv{
		e2e:         &e2e{stack: stack, bus: bus},
		connections: connectionv1connect.NewConnectionServiceClient(http.DefaultClient, stack.URL, opt),
		controls:    controlsv1connect.NewControlsServiceClient(http.DefaultClient, stack.URL, opt),
	}
}

func (e collabEnv) connect(t *testing.T, tenant, provider string, config, creds map[string]string) error {
	t.Helper()
	_, err := e.connections.Connect(context.Background(), connect.NewRequest(&connectionv1.ConnectRequest{TenantId: tenant, Provider: provider, Config: config, Credentials: creds}))
	return err
}

func TestCollabToolsAreOfferedOnlyWhenEnabled(t *testing.T) {
	off := newCollabE2E(t, false)
	b := off.stack.LiveBusiness(t, "tok-founder", "kerala-spice-exports")
	if names := off.toolNames(t, b.AgentToken); len(names) != 9 {
		t.Fatalf("collab tools must be absent unless enabled: %v", names)
	}
	on := newCollabE2E(t, true)
	b2 := on.stack.LiveBusiness(t, "tok-founder", "kerala-spice-exports")
	names := on.toolNames(t, b2.AgentToken)
	if len(names) != 15 || countPrefix(names, "meeting_") != 2 || countPrefix(names, "notes_") != 3 || countPrefix(names, "calendar_") != 1 {
		t.Fatalf("expected the 9 standard + 6 collab tools, got %d: %v", len(names), names)
	}
}

func TestConnectingAccountsVerifiesCredentialsAndNeverReturnsThem(t *testing.T) {
	e := newCollabE2E(t, true)
	a := e.stack.LiveBusiness(t, "tok-founder", "kerala-spice-exports")
	ctx := context.Background()

	// Before anything is connected an agent is told exactly what to do.
	b := a.AgentToken
	if text, isErr := e.call(t, b, "notes_search", map[string]any{}); !isErr || !strings.Contains(text, "ask the founder to connect") {
		t.Fatalf("unconnected: isErr=%v %q", isErr, text)
	}

	// Wrong credentials are refused, with a message that does not echo what was typed, and store nothing.
	bad := map[string]string{"client_id": "gid", "client_secret": "WRONG-SECRET-123", "refresh_token": "grefresh"}
	err := e.connect(t, a.ID, "google", nil, bad)
	if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "rejected") || strings.Contains(err.Error(), "WRONG-SECRET-123") {
		t.Fatalf("bad credentials: %v", err)
	}
	list, err := e.connections.List(ctx, connect.NewRequest(&connectionv1.ListRequest{TenantId: a.ID}))
	if err != nil || len(list.Msg.Connections) != 0 || len(list.Msg.Providers) != 3 {
		t.Fatalf("nothing may be stored after a failed check: %+v %v", list, err)
	}

	for _, c := range []struct {
		p        string
		cfg, sec map[string]string
	}{
		{"google", map[string]string{"calendar_id": "primary"}, testenv.GoogleCreds},
		{"zoom", nil, testenv.ZoomCreds},
		{"notion", map[string]string{"database_id": testenv.NotionDB}, testenv.NotionCreds},
	} {
		if err := e.connect(t, a.ID, c.p, c.cfg, c.sec); err != nil {
			t.Fatalf("%s: %v", c.p, err)
		}
	}
	list, _ = e.connections.List(ctx, connect.NewRequest(&connectionv1.ListRequest{TenantId: a.ID}))
	if len(list.Msg.Connections) != 3 {
		t.Fatalf("connections = %+v", list.Msg.Connections)
	}
	for _, c := range list.Msg.Connections {
		for k, v := range c.Config {
			if strings.Contains(v, "grefresh") || strings.Contains(v, "zsecret") || strings.Contains(v, "ntok") {
				t.Fatalf("a credential came back in %s config %s", c.Provider, k)
			}
		}
	}
	// The database never holds a credential in the clear.
	var leaked int
	err = tenancy.WithScopeTx(ctx, e.stack.Pool, tenancy.Scope{TenantID: a.UUID}, tenancy.Options{AssumeRole: testdb.Role}, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM tenant_secrets WHERE position('grefresh'::bytea in ciphertext) > 0 OR position('zsecret'::bytea in ciphertext) > 0 OR position('ntok'::bytea in ciphertext) > 0`).Scan(&leaked)
	})
	if err != nil || leaked != 0 {
		t.Fatalf("plaintext credentials in the database: %d %v", leaked, err)
	}

	// Another business's founder cannot see or disconnect these.
	other := e.stack.LiveBusiness(t, "tok-other", "globex-exim")
	otherClient := connectionv1connect.NewConnectionServiceClient(http.DefaultClient, e.stack.URL, connect.WithInterceptors(bearerFor("tok-other")))
	if _, err := otherClient.List(ctx, connect.NewRequest(&connectionv1.ListRequest{TenantId: a.ID})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("an outsider listing another business's connections: %v", err)
	}
	if _, err := otherClient.Disconnect(ctx, connect.NewRequest(&connectionv1.DisconnectRequest{TenantId: a.ID, Provider: "zoom"})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("an outsider disconnecting: %v", err)
	}
	if text, isErr := e.call(t, other.AgentToken, "notes_search", map[string]any{}); !isErr || !strings.Contains(text, "ask the founder to connect") {
		t.Fatalf("the other business's agent must not reach A's Notion: isErr=%v %q", isErr, text)
	}
}

func TestAgentsScheduleMeetingsInviteesNeedApprovalAndNotesAreIdempotent(t *testing.T) {
	e := newCollabE2E(t, true)
	a := e.stack.LiveBusiness(t, "tok-founder", "kerala-spice-exports")
	for p, c := range map[string]struct{ cfg, sec map[string]string }{
		"google": {nil, testenv.GoogleCreds}, "zoom": {nil, testenv.ZoomCreds}, "notion": {map[string]string{"database_id": testenv.NotionDB}, testenv.NotionCreds},
	} {
		if err := e.connect(t, a.ID, p, c.cfg, c.sec); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
	}
	ok := func(tool string, args map[string]any) map[string]any {
		t.Helper()
		text, isErr := e.call(t, a.AgentToken, tool, args)
		if isErr {
			t.Fatalf("%s: %s", tool, text)
		}
		return decode(t, text)
	}
	start := time.Date(2026, 3, 10, 9, 30, 0, 0, time.FixedZone("IST", 5*3600+1800)).Format(time.RFC3339)
	sched := func(over map[string]any) map[string]any {
		m := map[string]any{"provider": "google", "title": "Berlin buyer call", "start": start, "duration_minutes": 30, "timezone": "Asia/Kolkata", "idempotency_key": "m1"}
		for k, v := range over {
			m[k] = v
		}
		return m
	}

	t.Run("a meeting for ourselves is created right away with a link", func(t *testing.T) {
		r := ok("meeting_schedule", sched(nil))
		m := r["meeting"].(map[string]any)
		if !strings.HasPrefix(m["join_url"].(string), "https://meet.google.com/") || r["created"] != true {
			t.Fatalf("%v", r)
		}
	})

	t.Run("emailing people is queued, not done, and a human approval makes it happen", func(t *testing.T) {
		r := ok("meeting_schedule", sched(map[string]any{"idempotency_key": "m2", "attendees": []string{"ada@acme.com"}, "notify": true}))
		id := r["approval_id"].(string)
		if r["status"] != "PENDING_APPROVAL" || !strings.Contains(r["message"].(string), "NOTHING has been sent") {
			t.Fatalf("%v", r)
		}
		for _, ev := range e.stack.Google.Events() {
			if atts, _ := ev["attendees"].([]any); ev["summary"] == "Berlin buyer call" && len(atts) > 0 {
				t.Fatal("the event with attendees exists before approval")
			}
		}
		// Retrying the same request does not queue a second approval.
		if again := ok("meeting_schedule", sched(map[string]any{"idempotency_key": "m2", "attendees": []string{"ada@acme.com"}, "notify": true})); again["approval_id"] != id || again["replayed"] != true {
			t.Fatalf("retry = %v", again)
		}
		if st := ok("get_approval_status", map[string]any{"approval_id": id}); st["status"] != "PENDING_APPROVAL" {
			t.Fatalf("status = %v", st)
		}
		res := e.stack.Decide(t, "tok-founder", a.ID, id, true)
		if !res.Executed || res.Status != "EXECUTED" {
			t.Fatalf("approval did not execute: %+v", res)
		}
		var invited string
		for gid, ev := range e.stack.Google.Events() {
			if atts, _ := ev["attendees"].([]any); len(atts) > 0 {
				invited = e.stack.Google.SendUpdates(gid)
			}
		}
		if invited != "all" {
			t.Fatalf("the approved invitation must be emailed (sendUpdates=%q)", invited)
		}
		if st := ok("get_approval_status", map[string]any{"approval_id": id}); st["status"] != "EXECUTED" {
			t.Fatalf("status = %v", st)
		}
	})

	t.Run("a rejected invitation never happens", func(t *testing.T) {
		before := len(e.stack.Google.Events())
		r := ok("meeting_schedule", sched(map[string]any{"idempotency_key": "m3", "title": "Rejected call", "attendees": []string{"x@y.co"}, "notify": true}))
		e.stack.Decide(t, "tok-founder", a.ID, r["approval_id"].(string), false)
		if len(e.stack.Google.Events()) != before {
			t.Fatal("a rejected request created an event")
		}
	})

	t.Run("notes: a retried create is replayed and makes one page", func(t *testing.T) {
		args := map[string]any{"title": "Call with Berlin", "body": "Wants CIF Hamburg.", "idempotency_key": "note-1"}
		first := ok("notes_create", args)
		again := ok("notes_create", args)
		if again["replayed"] != true || again["page"].(map[string]any)["id"] != first["page"].(map[string]any)["id"] || len(e.stack.Notion.Pages()) != 1 {
			t.Fatalf("replay = %v, pages = %d", again, len(e.stack.Notion.Pages()))
		}
		different := map[string]any{"title": "Something else", "idempotency_key": "note-1"}
		if text, isErr := e.call(t, a.AgentToken, "notes_create", different); !isErr || !strings.Contains(text, "different request") {
			t.Fatalf("same key, different request: isErr=%v %q", isErr, text)
		}
	})

	t.Run("a rejected request frees its key so the corrected one can use it", func(t *testing.T) {
		if text, isErr := e.call(t, a.AgentToken, "meeting_schedule", sched(map[string]any{"idempotency_key": "fix-me", "start": "tomorrow"})); !isErr {
			t.Fatalf("a bad start must be refused: %q", text)
		}
		if r := ok("meeting_schedule", sched(map[string]any{"idempotency_key": "fix-me"})); r["created"] != true {
			t.Fatalf("the corrected request must be allowed to reuse the key: %v", r)
		}
	})

	t.Run("an outcome the platform cannot know is never silently repeated", func(t *testing.T) {
		z := func(key string) map[string]any {
			return sched(map[string]any{"provider": "zoom", "idempotency_key": key, "title": "Supplier sync"})
		}
		ok("meeting_schedule", z("z1")) // warm the Zoom token so the injected failure lands on the create itself
		before := e.stack.Zoom.Meetings()
		e.stack.Zoom.FailNext(503)
		text, isErr := e.call(t, a.AgentToken, "meeting_schedule", z("z2"))
		if !isErr || !strings.Contains(text, "may or may not have happened") || !strings.Contains(text, "SAME idempotency_key") {
			t.Fatalf("an unconfirmed create must say so honestly: isErr=%v %q", isErr, text)
		}
		text, isErr = e.call(t, a.AgentToken, "meeting_schedule", z("z2"))
		if !isErr || !strings.Contains(text, "outcome was not recorded") {
			t.Fatalf("a repeat after an unknown outcome must stop and say why: isErr=%v %q", isErr, text)
		}
		if e.stack.Zoom.Meetings() != before {
			t.Fatalf("a meeting was created by the failed or blocked attempts: %d -> %d", before, e.stack.Zoom.Meetings())
		}
		if r := ok("meeting_schedule", z("z3")); r["created"] != true || e.stack.Zoom.Meetings() != before+1 {
			t.Fatalf("a genuinely new request (new key) goes through: %v", r)
		}
	})

	t.Run("writes are attributed to the agent and audited, reads are not", func(t *testing.T) {
		var actions []string
		err := tenancy.WithScopeTx(context.Background(), e.stack.Pool, tenancy.Scope{TenantID: a.UUID}, tenancy.Options{AssumeRole: testdb.Role}, func(tx pgx.Tx) error {
			rows, err := tx.Query(context.Background(), `SELECT action || '/' || actor_id FROM audit_logs WHERE action LIKE 'collab.%' AND actor_type='AGENT' ORDER BY id`)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var s string
				if err := rows.Scan(&s); err != nil {
					return err
				}
				actions = append(actions, s)
			}
			return rows.Err()
		})
		if err != nil || len(actions) < 4 {
			t.Fatalf("audit = %v %v", actions, err)
		}
		for _, s := range actions {
			if !strings.HasSuffix(s, "/agent:sales") {
				t.Errorf("unattributed audit row %q", s)
			}
		}
	})

	t.Run("the audit chain stays intact through all of it", func(t *testing.T) {
		v, err := e.controls.VerifyAudit(context.Background(), connect.NewRequest(&controlsv1.VerifyAuditRequest{TenantId: a.ID}))
		if err != nil || !v.Msg.Intact || v.Msg.RowsChecked < 8 || len(v.Msg.HeadHex) != 64 {
			t.Fatalf("verify = %+v %v", v, err)
		}
	})
}

func TestSpendCapsBlockPaymentApprovalsEvenFromTheFounder(t *testing.T) {
	e := newCollabE2E(t, true)
	a := e.stack.LiveBusiness(t, "tok-founder", "kerala-spice-exports")
	ctx := context.Background()
	min := func(v int64) *int64 { return &v }

	pay := func(key string, amount int64) string {
		t.Helper()
		text, isErr := e.call(t, a.AgentToken, "request_human_approval", map[string]any{"kind": "PAYMENT", "title": "Pay supplier " + key, "summary": "x",
			"amount_minor": amount, "currency": "INR", "payload": map[string]any{"payee": "Farm co-op"}, "idempotency_key": key})
		if isErr {
			t.Fatal(text)
		}
		return decode(t, text)["approval_id"].(string)
	}
	tryApprove := func(id string) error {
		t.Helper()
		appr := e.stack.Approvals("tok-founder")
		g, err := appr.GetApproval(ctx, connect.NewRequest(&approvalv1.GetApprovalRequest{TenantId: a.ID, ApprovalId: id}))
		if err != nil {
			t.Fatal(err)
		}
		_, err = appr.DecideApproval(ctx, connect.NewRequest(&approvalv1.DecideApprovalRequest{TenantId: a.ID, ApprovalId: id, Approve: true, PayloadHash: g.Msg.PayloadHash}))
		return err
	}

	if _, err := e.controls.SetSpendCap(ctx, connect.NewRequest(&controlsv1.SetSpendCapRequest{TenantId: a.ID,
		Cap: &controlsv1.SpendCap{Currency: "INR", PerTransactionMinor: min(5_000_000), PerHourMinor: min(8_000_000)}})); err != nil {
		t.Fatal(err)
	}
	big := pay("big", 6_000_000)
	err := tryApprove(big)
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "Blocked by the spend cap") || !strings.Contains(err.Error(), "per-transaction cap of INR 50,000.00") {
		t.Fatalf("the founder must be stopped by their own cap, with the amounts spelled out: %v", err)
	}
	if st := e.call2(t, a, "get_approval_status", map[string]any{"approval_id": big}); st["status"] != "PENDING_APPROVAL" {
		t.Fatalf("a blocked approval stays pending: %v", st)
	}
	if err := tryApprove(pay("p1", 4_000_000)); err != nil {
		t.Fatalf("within the cap: %v", err)
	}
	if err := tryApprove(pay("p2", 4_000_000)); err != nil { // 4,000,000 + 4,000,000 = 8,000,000: exactly the hourly cap
		t.Fatalf("exactly at the hourly cap: %v", err)
	}
	if err := tryApprove(pay("p3", 1)); connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "last hour") {
		t.Fatalf("one unit over the hourly cap: %v", err)
	}

	// Raising the cap is the founder's deliberate act, and is itself audited.
	if _, err := e.controls.SetSpendCap(ctx, connect.NewRequest(&controlsv1.SetSpendCapRequest{TenantId: a.ID,
		Cap: &controlsv1.SpendCap{Currency: "INR", PerTransactionMinor: min(10_000_000), PerHourMinor: min(20_000_000)}})); err != nil {
		t.Fatal(err)
	}
	if err := tryApprove(big); err != nil {
		t.Fatalf("after raising the cap: %v", err)
	}
	var actions []string
	_ = tenancy.WithScopeTx(ctx, e.stack.Pool, tenancy.Scope{TenantID: a.UUID}, tenancy.Options{AssumeRole: testdb.Role}, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT action FROM audit_logs WHERE action IN ('spend_cap.set','spend_cap.blocked') ORDER BY id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var s string
			_ = rows.Scan(&s)
			actions = append(actions, s)
		}
		return nil
	})
	if strings.Join(actions, ",") != "spend_cap.set,spend_cap.blocked,spend_cap.blocked,spend_cap.set" {
		t.Fatalf("audit of cap changes and blocks = %v", actions)
	}

	// Bad caps are refused; reading is allowed; an outsider sees nothing.
	if _, err := e.controls.SetSpendCap(ctx, connect.NewRequest(&controlsv1.SetSpendCapRequest{TenantId: a.ID, Cap: &controlsv1.SpendCap{Currency: "INR", PerHourMinor: min(10), PerDayMinor: min(5)}})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("hour above day: %v", err)
	}
	if _, err := e.controls.SetSpendCap(ctx, connect.NewRequest(&controlsv1.SetSpendCapRequest{TenantId: a.ID, Cap: &controlsv1.SpendCap{Currency: "rupees"}})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("bad currency: %v", err)
	}
	got, err := e.controls.GetSpendCaps(ctx, connect.NewRequest(&controlsv1.GetSpendCapsRequest{TenantId: a.ID}))
	if err != nil || len(got.Msg.Caps) != 1 || got.Msg.Caps[0].GetPerHourMinor() != 20_000_000 || got.Msg.Caps[0].PerDayMinor != nil {
		t.Fatalf("caps = %+v %v", got, err)
	}
	outsider := controlsv1connect.NewControlsServiceClient(http.DefaultClient, e.stack.URL, connect.WithInterceptors(bearerFor("tok-other")))
	if _, err := outsider.GetSpendCaps(ctx, connect.NewRequest(&controlsv1.GetSpendCapsRequest{TenantId: a.ID})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("an outsider reading another business's caps: %v", err)
	}
}

// call2 is a successful tool call decoded.
func (e collabEnv) call2(t *testing.T, b testenv.Business, tool string, args map[string]any) map[string]any {
	t.Helper()
	text, isErr := e.call(t, b.AgentToken, tool, args)
	if isErr {
		t.Fatalf("%s: %s", tool, text)
	}
	return decode(t, text)
}

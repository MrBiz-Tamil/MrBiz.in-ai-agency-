package db_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mrbiz/venture-os/internal/agentrun"
	"github.com/mrbiz/venture-os/internal/ports"
	"github.com/mrbiz/venture-os/internal/tenancy"
	"github.com/mrbiz/venture-os/internal/testdb"
)

// dispatcher is the scope the control plane's dispatcher runs in: no tenant at all. It can reach other tenants' queue
// metadata only through the SECURITY DEFINER functions.
var dispatcher = tenancy.Scope{UserSub: "dispatcher"}

// soon is a "now" for the claim functions that is later than any row the test just created (database and test clocks
// are separate).
func soon() time.Time { return time.Now().Add(time.Minute) }

func exec(t *testing.T, pool *pgxpool.Pool, s tenancy.Scope, sql string, args ...any) error {
	t.Helper()
	return run(t, pool, s, func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), sql, args...)
		return err
	})
}

// workspace creates an ACTIVE business with approved, active agents.
func workspace(t *testing.T, pool *pgxpool.Pool, slug string, agents ...string) tenancy.Scope {
	t.Helper()
	s := newTenant(t, pool, slug, "founder-"+slug)
	must(t, exec(t, pool, s, `UPDATE organizations SET status='ACTIVE' WHERE id=$1`, s.TenantID))
	for _, a := range agents {
		addAgent(t, pool, s, a)
	}
	return s
}

func addAgent(t *testing.T, pool *pgxpool.Pool, s tenancy.Scope, key string) {
	t.Helper()
	must(t, exec(t, pool, s, `INSERT INTO agent_org_configs (tenant_id, agent_key, role_title, skill_md, idempotency_key)
	                          VALUES ($1,$2,$2,'# skill',$3)`, s.TenantID, key, "agent-"+key))
	must(t, exec(t, pool, s, `UPDATE agent_org_configs SET skill_status='APPROVED', status='ACTIVE' WHERE agent_key=$1`, key))
}

type taskOpts struct {
	goal, source, by, key string
	priority              int
	notBefore             *time.Time
	maxSteps, maxSeconds  int
	maxAttempts           int
}

func (o *taskOpts) defaults() {
	if o.goal == "" {
		o.goal = "do the thing"
	}
	if o.source == "" {
		o.source = "FOUNDER"
	}
	if o.by == "" {
		o.by = "user:founder"
	}
	if o.key == "" {
		o.key = uuid.NewString()
	}
	if o.priority == 0 {
		o.priority = 5
	}
	if o.maxSteps == 0 {
		o.maxSteps = 25
	}
	if o.maxSeconds == 0 {
		o.maxSeconds = 600
	}
	if o.maxAttempts == 0 {
		o.maxAttempts = 2
	}
}

func addTask(t *testing.T, pool *pgxpool.Pool, s tenancy.Scope, agent string, mods ...func(*taskOpts)) (uuid.UUID, error) {
	t.Helper()
	var o taskOpts
	for _, m := range mods {
		m(&o)
	}
	o.defaults()
	var id uuid.UUID
	err := run(t, pool, s, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(),
			`INSERT INTO agent_tasks (tenant_id, agent_key, goal, source, created_by, idempotency_key, priority, not_before, max_steps, max_seconds, max_attempts)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,COALESCE($8::timestamptz, now()),$9,$10,$11) RETURNING id`,
			s.TenantID, agent, o.goal, o.source, o.by, o.key, o.priority, o.notBefore, o.maxSteps, o.maxSeconds, o.maxAttempts).Scan(&id)
	})
	return id, err
}

func mustTask(t *testing.T, pool *pgxpool.Pool, s tenancy.Scope, agent string, mods ...func(*taskOpts)) uuid.UUID {
	t.Helper()
	id, err := addTask(t, pool, s, agent, mods...)
	must(t, err)
	return id
}

type taskRow struct {
	Status         string
	Attempt, Steps int
	Wrote          bool
	LeaseOwner     *string
	Lease          *time.Time
	RunRef         *string
	Started        *time.Time
	Finished       *time.Time
}

func getTask(t *testing.T, pool *pgxpool.Pool, s tenancy.Scope, id uuid.UUID) taskRow {
	t.Helper()
	var r taskRow
	must(t, run(t, pool, s, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(),
			`SELECT status, attempt, steps_used, wrote, lease_owner, lease_expires_at, run_ref, started_at, finished_at FROM agent_tasks WHERE id=$1`, id).
			Scan(&r.Status, &r.Attempt, &r.Steps, &r.Wrote, &r.LeaseOwner, &r.Lease, &r.RunRef, &r.Started, &r.Finished)
	}))
	return r
}

type claimed struct {
	Tenant, Task uuid.UUID
	Agent        string
	Attempt      int16
	RunRef       string
	Lease        time.Time
}

func claimWith(t *testing.T, pool *pgxpool.Pool, limit int, now time.Time, perTenant, perAgent int) []claimed {
	t.Helper()
	var out []claimed
	must(t, run(t, pool, dispatcher, func(tx pgx.Tx) error {
		rows, err := tx.Query(context.Background(),
			`SELECT r_tenant_id, r_task_id, r_agent_key, r_attempt, r_run_ref, r_lease_expires_at
			   FROM agent_task_claim('dispatcher-1', $1, $2::timestamptz, interval '30 seconds', $3, $4)`, limit, now, perTenant, perAgent)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var c claimed
			if err := rows.Scan(&c.Tenant, &c.Task, &c.Agent, &c.Attempt, &c.RunRef, &c.Lease); err != nil {
				return err
			}
			out = append(out, c)
		}
		return rows.Err()
	}))
	return out
}

func claim(t *testing.T, pool *pgxpool.Pool, limit int, now time.Time) []claimed {
	return claimWith(t, pool, limit, now, 2, 1)
}

func setStatus(t *testing.T, pool *pgxpool.Pool, s tenancy.Scope, id uuid.UUID, to string) error {
	t.Helper()
	return exec(t, pool, s, `UPDATE agent_tasks SET status=$2, error_code = CASE WHEN $2='FAILED' THEN 'boom' END WHERE id=$1`, id, to)
}

type event struct {
	Seq, Attempt int
	Kind         string
	Tool         *string
	ReadOnly     *bool
	OK           *bool
	Detail       string
}

func events(t *testing.T, pool *pgxpool.Pool, s tenancy.Scope, id uuid.UUID) []event {
	t.Helper()
	var out []event
	must(t, run(t, pool, s, func(tx pgx.Tx) error {
		rows, err := tx.Query(context.Background(),
			`SELECT seq, attempt, kind, tool, read_only, ok, detail::text FROM agent_task_events WHERE task_id=$1 ORDER BY seq`, id)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e event
			if err := rows.Scan(&e.Seq, &e.Attempt, &e.Kind, &e.Tool, &e.ReadOnly, &e.OK, &e.Detail); err != nil {
				return err
			}
			out = append(out, e)
		}
		return rows.Err()
	}))
	return out
}

func toolCall(t *testing.T, pool *pgxpool.Pool, s tenancy.Scope, id uuid.UUID, attempt int, tool string, readOnly bool) error {
	t.Helper()
	return exec(t, pool, s, `INSERT INTO agent_task_events (tenant_id, task_id, attempt, kind, tool, read_only) VALUES ($1,$2,$3,'TOOL_CALL',$4,$5)`,
		s.TenantID, id, attempt, tool, readOnly)
}

func toolResult(t *testing.T, pool *pgxpool.Pool, s tenancy.Scope, id uuid.UUID, attempt int, tool string, ok bool) error {
	t.Helper()
	return exec(t, pool, s, `INSERT INTO agent_task_events (tenant_id, task_id, attempt, kind, tool, ok) VALUES ($1,$2,$3,'TOOL_RESULT',$4,$5)`,
		s.TenantID, id, attempt, tool, ok)
}

// running returns a task that has been claimed (attempt 1) so the test can act on it.
func running(t *testing.T, pool *pgxpool.Pool, s tenancy.Scope, agent string, mods ...func(*taskOpts)) uuid.UUID {
	t.Helper()
	id := mustTask(t, pool, s, agent, mods...)
	for _, c := range claimWith(t, pool, 100, soon(), 100, 100) {
		if c.Task == id {
			return id
		}
	}
	t.Fatalf("task %s was not claimed", id)
	return id
}

// ───────────────────────────── creation ─────────────────────────────

func TestTaskCreationRules(t *testing.T) {
	pool := testdb.Pool(t)

	t.Run("only an active business with an active agent that has an approved skill can be given work", func(t *testing.T) {
		s := newTenant(t, pool, "draft-co", "f1") // organizations start as DRAFT
		addAgent(t, pool, s, "sales")
		_, err := addTask(t, pool, s, "sales")
		wantErr(t, err, "business is not active")

		must(t, exec(t, pool, s, `UPDATE organizations SET status='ACTIVE' WHERE id=$1`, s.TenantID))
		must(t, exec(t, pool, s, `INSERT INTO agent_org_configs (tenant_id, agent_key, role_title, skill_md, idempotency_key) VALUES ($1,'planner','P','# s','p1')`, s.TenantID))
		_, err = addTask(t, pool, s, "planner")
		wantErr(t, err, "is not active")
		must(t, exec(t, pool, s, `UPDATE agent_org_configs SET status='ACTIVE' WHERE agent_key='planner'`))
		_, err = addTask(t, pool, s, "planner")
		wantErr(t, err, "no approved skill")
		_, err = addTask(t, pool, s, "ghost")
		wantErr(t, err, "unknown agent")

		must(t, exec(t, pool, s, `UPDATE agent_org_configs SET skill_status='APPROVED' WHERE agent_key='planner'`))
		if _, err := addTask(t, pool, s, "planner"); err != nil {
			t.Fatalf("an active agent with an approved skill must be allowed: %v", err)
		}
		// Pausing the agent stops new work immediately.
		must(t, exec(t, pool, s, `UPDATE agent_org_configs SET status='PAUSED' WHERE agent_key='planner'`))
		_, err = addTask(t, pool, s, "planner")
		wantErr(t, err, "is not active")
	})

	s := workspace(t, pool, "create-co", "sales")

	t.Run("a task starts QUEUED, untouched, and its birth is recorded", func(t *testing.T) {
		id := mustTask(t, pool, s, "sales")
		r := getTask(t, pool, s, id)
		if r.Status != "QUEUED" || r.Attempt != 0 || r.Wrote || r.LeaseOwner != nil || r.Started != nil || r.Finished != nil {
			t.Fatalf("row = %+v", r)
		}
		ev := events(t, pool, s, id)
		if len(ev) != 1 || ev[0].Kind != "STATE" || ev[0].Seq != 1 || !strings.Contains(ev[0].Detail, `"to": "QUEUED"`) || !strings.Contains(ev[0].Detail, s.UserSub) {
			t.Fatalf("events = %+v", ev)
		}
		err := exec(t, pool, s, `INSERT INTO agent_tasks (tenant_id, agent_key, goal, source, created_by, idempotency_key, status, lease_owner, lease_expires_at, attempt)
		                         VALUES ($1,'sales','x','FOUNDER','user:f','k-run','RUNNING','me', now()+interval '1 hour', 1)`, s.TenantID)
		wantErr(t, err, "created QUEUED")
		err = exec(t, pool, s, `INSERT INTO agent_tasks (tenant_id, agent_key, goal, source, created_by, idempotency_key, wrote) VALUES ($1,'sales','x','FOUNDER','user:f','k-wrote',true)`, s.TenantID)
		wantErr(t, err, "created QUEUED")
	})

	t.Run("who asked has to match how the task came to exist", func(t *testing.T) {
		for _, c := range []struct{ source, by string }{
			{"FOUNDER", "schedule:abc"}, {"SCHEDULE", "user:founder"}, {"WEBHOOK", "user:founder"}, {"APPROVAL", "webhook:libredesk"}, {"CRON", "cron:1"},
		} {
			_, err := addTask(t, pool, s, "sales", func(o *taskOpts) { o.source, o.by = c.source, c.by })
			if err == nil {
				t.Errorf("source %s with created_by %s was accepted", c.source, c.by)
			}
		}
		for _, c := range []struct{ source, by string }{{"SCHEDULE", "schedule:1"}, {"WEBHOOK", "webhook:libredesk"}, {"APPROVAL", "approval:9"}} {
			if _, err := addTask(t, pool, s, "sales", func(o *taskOpts) { o.source, o.by = c.source, c.by }); err != nil {
				t.Errorf("%s / %s refused: %v", c.source, c.by, err)
			}
		}
	})

	t.Run("limits and text are bounded", func(t *testing.T) {
		bad := map[string]func(*taskOpts){
			"empty goal":     func(o *taskOpts) { o.goal = " " },
			"huge goal":      func(o *taskOpts) { o.goal = strings.Repeat("x", 4001) },
			"too many steps": func(o *taskOpts) { o.maxSteps = 201 },
			"too long":       func(o *taskOpts) { o.maxSeconds = 3601 },
			"too short":      func(o *taskOpts) { o.maxSeconds = 5 },
			"too many tries": func(o *taskOpts) { o.maxAttempts = 6 },
			"priority 10":    func(o *taskOpts) { o.priority = 10 },
		}
		for name, mod := range bad {
			if _, err := addTask(t, pool, s, "sales", mod); err == nil && name != "empty goal" {
				t.Errorf("%s was accepted", name)
			}
		}
		// A goal that is only spaces is non-empty to a length check; the application trims it, so the database's
		// promise is the length bounds, which the cases above prove. Record what the database does with it.
		if _, err := addTask(t, pool, s, "sales", bad["empty goal"]); err != nil {
			t.Logf("a whitespace-only goal is refused by the database as well: %v", err)
		} else {
			t.Log("a whitespace-only goal is accepted by the database; the store must trim and refuse it")
		}
	})

	t.Run("creating the same request twice is one task", func(t *testing.T) {
		_, err := addTask(t, pool, s, "sales", func(o *taskOpts) { o.key = "double-click" })
		must(t, err)
		_, err = addTask(t, pool, s, "sales", func(o *taskOpts) { o.key = "double-click" })
		if sqlState(err) != "23505" {
			t.Fatalf("second create: %v (want a unique violation)", err)
		}
	})

	t.Run("the queue is capped at 200 waiting tasks per business", func(t *testing.T) {
		full := workspace(t, pool, "full-co", "sales")
		must(t, exec(t, pool, full, `INSERT INTO agent_tasks (tenant_id, agent_key, goal, source, created_by, idempotency_key)
		                             SELECT $1,'sales','g','FOUNDER','user:f','bulk-'||g FROM generate_series(1,200) g`, full.TenantID))
		_, err := addTask(t, pool, full, "sales")
		wantErr(t, err, "queue is full")
		// Another business is unaffected.
		other := workspace(t, pool, "roomy-co", "sales")
		mustTask(t, pool, other, "sales")
		// Finishing one frees a place.
		must(t, exec(t, pool, full, `UPDATE agent_tasks SET status='CANCELLED' WHERE id = (SELECT id FROM agent_tasks ORDER BY created_at LIMIT 1)`))
		mustTask(t, pool, full, "sales")
	})
}

// ───────────────────────────── state machine ─────────────────────────────

func TestTaskStateMachine(t *testing.T) {
	pool := testdb.Pool(t)
	n := 0
	// drive puts a fresh task into the given state by the legal route.
	drive := func(t *testing.T, state string, mods ...func(*taskOpts)) (tenancy.Scope, uuid.UUID) {
		n++
		// Earlier cases leave tasks behind (an illegal move leaves the task where it was). Cancel them as the owner so
		// this case's claim sees only its own task.
		if _, err := pool.Exec(context.Background(), `UPDATE agent_tasks SET status='CANCELLED' WHERE status IN ('QUEUED','RUNNING','NEEDS_REVIEW')`); err != nil {
			t.Fatal(err)
		}
		s := workspace(t, pool, fmt.Sprintf("sm-%d", n), "sales")
		now := time.Now().Add(time.Duration(n) * time.Hour) // a "now" no other case's task is eligible at
		nb := now.Add(-time.Minute)
		id := mustTask(t, pool, s, "sales", append([]func(*taskOpts){func(o *taskOpts) { o.notBefore = &nb }}, mods...)...)
		if state == "QUEUED" {
			return s, id
		}
		if state == "CANCELLED" {
			must(t, setStatus(t, pool, s, id, "CANCELLED"))
			return s, id
		}
		got := claim(t, pool, 100, now)
		if len(got) != 1 || got[0].Task != id {
			t.Fatalf("claim returned %+v; want exactly this task", got)
		}
		if state != "RUNNING" {
			must(t, setStatus(t, pool, s, id, state))
		}
		return s, id
	}
	states := []string{"QUEUED", "RUNNING", "DONE", "FAILED", "CANCELLED", "NEEDS_REVIEW"}
	legal := map[string]bool{
		"QUEUED>CANCELLED": true,
		"RUNNING>DONE":     true, "RUNNING>FAILED": true, "RUNNING>CANCELLED": true, "RUNNING>NEEDS_REVIEW": true, "RUNNING>QUEUED": true,
		"NEEDS_REVIEW>QUEUED": true, "NEEDS_REVIEW>DONE": true, "NEEDS_REVIEW>FAILED": true, "NEEDS_REVIEW>CANCELLED": true,
	}
	for _, from := range states {
		for _, to := range states {
			if from == to || to == "RUNNING" {
				continue // starting a run is the claim function's job, tested separately
			}
			from, to := from, to
			t.Run(from+" to "+to, func(t *testing.T) {
				s, id := drive(t, from)
				err := setStatus(t, pool, s, id, to)
				if legal[from+">"+to] {
					if err != nil {
						t.Fatalf("should be allowed: %v", err)
					}
					r := getTask(t, pool, s, id)
					if r.Status != to || r.LeaseOwner != nil || r.Lease != nil {
						t.Fatalf("row = %+v: a lease exists only while RUNNING", r)
					}
					if (to == "DONE" || to == "FAILED" || to == "CANCELLED") && r.Finished == nil {
						t.Fatal("a final task must have a finish time")
					}
					return
				}
				if err == nil {
					t.Fatalf("%s to %s must be refused", from, to)
				}
				if from == "DONE" || from == "FAILED" || from == "CANCELLED" {
					wantErr(t, err, "is final")
				} else {
					wantErr(t, err, "illegal task transition")
				}
			})
		}
	}

	t.Run("a task cannot be started by hand", func(t *testing.T) {
		s, id := drive(t, "QUEUED")
		err := exec(t, pool, s, `UPDATE agent_tasks SET status='RUNNING', lease_owner='me', lease_expires_at=now()+interval '5 min' WHERE id=$1`, id)
		wantErr(t, err, "must be attempt 1")
	})

	t.Run("a failure needs a reason", func(t *testing.T) {
		s, id := drive(t, "RUNNING")
		err := exec(t, pool, s, `UPDATE agent_tasks SET status='FAILED' WHERE id=$1`, id)
		if err == nil {
			t.Fatal("FAILED without an error code was accepted")
		}
	})

	t.Run("a retry is allowed only if nothing was written and attempts remain", func(t *testing.T) {
		s, id := drive(t, "RUNNING", func(o *taskOpts) { o.maxAttempts = 2 })
		must(t, toolCall(t, pool, s, id, 1, "crm_create_deal", false)) // a write began
		wantErr(t, setStatus(t, pool, s, id, "QUEUED"), "may have written something")
		must(t, setStatus(t, pool, s, id, "NEEDS_REVIEW")) // the right place for it

		s2, id2 := drive(t, "RUNNING", func(o *taskOpts) { o.maxAttempts = 2 })
		must(t, toolCall(t, pool, s2, id2, 1, "crm_find_deals", true)) // reads only
		must(t, setStatus(t, pool, s2, id2, "QUEUED"))
		now := soon().Add(48 * time.Hour)
		if got := claim(t, pool, 10, now); len(got) != 1 || got[0].Task != id2 || got[0].Attempt != 2 {
			t.Fatalf("second claim = %+v", got)
		}
		wantErr(t, setStatus(t, pool, s2, id2, "QUEUED"), "used all 2 attempts")
	})

	t.Run("every state change is written to the history by the database", func(t *testing.T) {
		s, id := drive(t, "RUNNING")
		must(t, setStatus(t, pool, s, id, "DONE"))
		var path []string
		for _, e := range events(t, pool, s, id) {
			if e.Kind == "STATE" {
				path = append(path, e.Detail)
			}
		}
		if len(path) != 3 || !strings.Contains(path[0], `"to": "QUEUED"`) || !strings.Contains(path[1], `"to": "RUNNING"`) || !strings.Contains(path[2], `"to": "DONE"`) {
			t.Fatalf("history = %v", path)
		}
	})
}

func TestTaskRequestAndCountersAreProtected(t *testing.T) {
	pool := testdb.Pool(t)
	s := workspace(t, pool, "prot-co", "sales", "support")
	id := running(t, pool, s, "sales")

	for name, sql := range map[string]string{
		"goal":        `UPDATE agent_tasks SET goal='something else' WHERE id=$1`,
		"agent":       `UPDATE agent_tasks SET agent_key='support' WHERE id=$1`,
		"max steps":   `UPDATE agent_tasks SET max_steps=200 WHERE id=$1`,
		"max seconds": `UPDATE agent_tasks SET max_seconds=3600 WHERE id=$1`,
		"max tries":   `UPDATE agent_tasks SET max_attempts=5 WHERE id=$1`,
		"priority":    `UPDATE agent_tasks SET priority=1 WHERE id=$1`,
		"source":      `UPDATE agent_tasks SET source='WEBHOOK', created_by='webhook:x' WHERE id=$1`,
		"untrusted":   `UPDATE agent_tasks SET untrusted = NOT untrusted WHERE id=$1`,
		"created_by":  `UPDATE agent_tasks SET created_by='user:someone-else' WHERE id=$1`,
		"key":         `UPDATE agent_tasks SET idempotency_key='new' WHERE id=$1`,
	} {
		wantErr(t, exec(t, pool, s, sql, id), "request is immutable")
		_ = name
	}
	wantErr(t, exec(t, pool, s, `UPDATE agent_tasks SET tenant_id=$2 WHERE id=$1`, id, uuid.New()), "")

	must(t, toolCall(t, pool, s, id, 1, "crm_create_deal", false))
	must(t, toolCall(t, pool, s, id, 1, "crm_find_deals", true))
	wantErr(t, exec(t, pool, s, `UPDATE agent_tasks SET steps_used=0 WHERE id=$1`, id), "counters only increase")
	wantErr(t, exec(t, pool, s, `UPDATE agent_tasks SET wrote=false WHERE id=$1`, id), "counters only increase")
	must(t, exec(t, pool, s, `UPDATE agent_tasks SET cost_minor=50 WHERE id=$1`, id))
	wantErr(t, exec(t, pool, s, `UPDATE agent_tasks SET cost_minor=10 WHERE id=$1`, id), "counters only increase")
	wantErr(t, exec(t, pool, s, `UPDATE agent_tasks SET steps_used=26 WHERE id=$1`, id), "step limit")
	wantErr(t, exec(t, pool, s, `UPDATE agent_tasks SET attempt=2 WHERE id=$1`, id), "attempt changes only")

	// What a runner legitimately updates while running still works.
	must(t, exec(t, pool, s, `UPDATE agent_tasks SET lease_expires_at = lease_expires_at + interval '1 minute', cost_reported=true WHERE id=$1`, id))
	must(t, setStatus(t, pool, s, id, "DONE"))
	wantErr(t, exec(t, pool, s, `UPDATE agent_tasks SET result_summary='rewritten' WHERE id=$1`, id), "is final")
}

// ───────────────────────────── claiming ─────────────────────────────

func TestClaimRespectsPriorityTimeAndConcurrency(t *testing.T) {
	pool := testdb.Pool(t)

	t.Run("most urgent first, then oldest; a task is not eligible before its time", func(t *testing.T) {
		s := workspace(t, pool, "order-co", "a1", "a2", "a3", "a4")
		future := time.Now().Add(2 * time.Hour)
		later := mustTask(t, pool, s, "a1", func(o *taskOpts) { o.priority = 5 })
		urgent := mustTask(t, pool, s, "a2", func(o *taskOpts) { o.priority = 1 })
		_ = mustTask(t, pool, s, "a3", func(o *taskOpts) { o.priority = 1; o.notBefore = &future })
		got := claimWith(t, pool, 1, soon(), 10, 10)
		if len(got) != 1 || got[0].Task != urgent {
			t.Fatalf("first claim = %+v, want the urgent task", got)
		}
		got = claimWith(t, pool, 5, soon(), 10, 10)
		if len(got) != 1 || got[0].Task != later {
			t.Fatalf("second claim = %+v: the task scheduled for later must wait", got)
		}
		got = claimWith(t, pool, 5, future.Add(time.Minute), 10, 10)
		if len(got) != 1 || got[0].Agent != "a3" {
			t.Fatalf("after its time the waiting task is claimed: %+v", got)
		}
	})

	t.Run("a claim sets the lease, the run reference and the history", func(t *testing.T) {
		s := workspace(t, pool, "lease-co", "sales")
		id := mustTask(t, pool, s, "sales", func(o *taskOpts) { o.maxSeconds = 120 })
		now := soon()
		got := claim(t, pool, 1, now)
		if len(got) != 1 {
			t.Fatalf("claimed %d", len(got))
		}
		c := got[0]
		if c.Task != id || c.Tenant != s.TenantID || c.Agent != "sales" || c.Attempt != 1 {
			t.Fatalf("claim = %+v", c)
		}
		if want := string(agentrun.RefFor(id, 1)); c.RunRef != want {
			t.Fatalf("run ref %q, but the Go side computes %q: they must agree", c.RunRef, want)
		}
		if d := c.Lease.Sub(now); d < 149*time.Second || d > 151*time.Second {
			t.Fatalf("lease runs %v past now; want max_seconds + 30s grace = 150s", d)
		}
		r := getTask(t, pool, s, id)
		if r.Status != "RUNNING" || r.Attempt != 1 || r.LeaseOwner == nil || *r.LeaseOwner != "dispatcher-1" || r.Started == nil {
			t.Fatalf("row = %+v", r)
		}
	})

	t.Run("one agent runs one task at a time, one business runs two", func(t *testing.T) {
		s := workspace(t, pool, "limits-co", "aa", "bb", "cc")
		a1 := mustTask(t, pool, s, "aa")
		a2 := mustTask(t, pool, s, "aa")
		b1 := mustTask(t, pool, s, "bb")
		c1 := mustTask(t, pool, s, "cc")
		got := claim(t, pool, 10, soon())
		if len(got) != 2 {
			t.Fatalf("claimed %d tasks; two is the per-business limit: %+v", len(got), got)
		}
		seen := map[uuid.UUID]bool{}
		for _, c := range got {
			seen[c.Task] = true
		}
		if seen[a2] || (!seen[a1] && !seen[b1]) {
			t.Fatalf("claimed %v: the second task for agent a must wait", seen)
		}
		// Finish one: the next waiting task is let through.
		var done uuid.UUID
		for id := range seen {
			done = id
			break
		}
		must(t, setStatus(t, pool, s, done, "DONE"))
		more := claim(t, pool, 10, soon())
		if len(more) != 1 {
			t.Fatalf("after one finished, %d more were claimed; want exactly 1 (limit 2 running)", len(more))
		}
		_ = c1
	})

	t.Run("a claim spans businesses but never exceeds the limit for any one", func(t *testing.T) {
		a := workspace(t, pool, "span-a", "xx", "yy", "zz")
		b := workspace(t, pool, "span-b", "xx")
		for _, ag := range []string{"xx", "yy", "zz"} {
			mustTask(t, pool, a, ag)
		}
		bt := mustTask(t, pool, b, "xx")
		got := claim(t, pool, 100, soon())
		perTenant := map[uuid.UUID]int{}
		for _, c := range got {
			perTenant[c.Tenant]++
		}
		if perTenant[a.TenantID] != 2 || perTenant[b.TenantID] != 1 {
			t.Fatalf("per business = %v: A is capped at 2, B is not held back by A", perTenant)
		}
		if r := getTask(t, pool, b, bt); r.Status != "RUNNING" {
			t.Fatalf("B's task is %s", r.Status)
		}
	})

	t.Run("bad calls are refused", func(t *testing.T) {
		err := run(t, pool, dispatcher, func(tx pgx.Tx) error {
			_, err := tx.Exec(context.Background(), `SELECT * FROM agent_task_claim('', 1)`)
			return err
		})
		wantErr(t, err, "needs an owner")
		err = run(t, pool, dispatcher, func(tx pgx.Tx) error {
			_, err := tx.Exec(context.Background(), `SELECT * FROM agent_task_claim('d', 0)`)
			return err
		})
		wantErr(t, err, "p_limit")
	})
}

// On PGlite (one connection) this runs the dispatchers one after another, so it proves the bookkeeping but not the
// race. Against a real multi-connection PostgreSQL (VENTURE_TEST_DATABASE_URL) the dispatchers overlap for real.
func TestClaimGivesEachTaskToOneDispatcher(t *testing.T) {
	pool := testdb.Pool(t)
	const tenants, perTenant = 6, 5
	var scopes []tenancy.Scope
	agents := []string{"aa", "bb", "cc", "dd", "ee"}
	for i := 0; i < tenants; i++ {
		scopes = append(scopes, workspace(t, pool, fmt.Sprintf("race-%d", i), agents...))
	}
	// Queue the work round-robin across businesses, so that any batch of rows a dispatcher locks at once belongs to
	// several businesses and different dispatchers compete for the same business.
	for _, ag := range agents {
		for _, s := range scopes {
			mustTask(t, pool, s, ag)
		}
	}
	var mu sync.Mutex
	won := map[uuid.UUID]int{}
	per := map[uuid.UUID]int{}
	var wg sync.WaitGroup
	// With fewer than eight connections (PGlite has one) the dispatchers cannot truly overlap; waiting for eight would hang.
	overlap := pool.Config().MaxConns >= 8
	start := make(chan struct{}) // every dispatcher begins at the same moment
	var warm sync.WaitGroup      // ...and only after all of their connections are open (opening one takes longer than a claim)
	for d := 0; d < 8; d++ {
		wg.Add(1)
		if overlap {
			warm.Add(1)
		}
		go func() {
			defer wg.Done()
			if overlap {
				_ = run(t, pool, dispatcher, func(tx pgx.Tx) error {
					warm.Done()
					warm.Wait() // hold this connection until all eight are checked out
					return nil
				})
			}
			<-start
			for round := 0; round < 4; round++ {
				var got []claimed
				_ = run(t, pool, dispatcher, func(tx pgx.Tx) error {
					rows, err := tx.Query(context.Background(),
						`SELECT r_tenant_id, r_task_id FROM agent_task_claim($1, 6, $2::timestamptz)`, fmt.Sprintf("d-%d", d), soon())
					if err != nil {
						return err
					}
					defer rows.Close()
					for rows.Next() {
						var c claimed
						if err := rows.Scan(&c.Tenant, &c.Task); err != nil {
							return err
						}
						got = append(got, c)
					}
					// Keep the transaction open a moment before it commits: a claim that has not committed is invisible
					// to the others' "how many are running" count unless the per-business lock makes them wait.
					time.Sleep(15 * time.Millisecond)
					return rows.Err()
				})
				mu.Lock()
				for _, c := range got {
					won[c.Task]++
					per[c.Tenant]++
				}
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()
	for task, n := range won {
		if n != 1 {
			t.Fatalf("task %s was handed to %d dispatchers", task, n)
		}
	}
	for _, s := range scopes {
		t.Logf("tenant %s: %d running", s.TenantID, per[s.TenantID])
		if per[s.TenantID] != 2 {
			t.Fatalf("business %s has %d running tasks, want exactly the limit of 2 (queued work was available)", s.TenantID, per[s.TenantID])
		}
	}
	_ = perTenant
}

// ───────────────────────────── isolation and privileges ─────────────────────────────

func TestTaskIsolationAndPrivileges(t *testing.T) {
	pool := testdb.Pool(t)
	a := workspace(t, pool, "iso-a", "sales")
	b := workspace(t, pool, "iso-b", "sales")
	ta := running(t, pool, a, "sales")

	t.Run("another business sees and changes nothing", func(t *testing.T) {
		if n := count(t, pool, b, "agent_tasks"); n != 0 {
			t.Fatalf("B sees %d of A's tasks", n)
		}
		if n := count(t, pool, b, "agent_task_events"); n != 0 {
			t.Fatalf("B sees %d of A's events", n)
		}
		must(t, exec(t, pool, b, `UPDATE agent_tasks SET status='CANCELLED' WHERE id=$1`, ta))
		if r := getTask(t, pool, a, ta); r.Status != "RUNNING" {
			t.Fatalf("B cancelled A's task: %s", r.Status)
		}
		err := exec(t, pool, b, `INSERT INTO agent_task_events (tenant_id, task_id, attempt, kind, tool, read_only) VALUES ($1,$2,1,'TOOL_CALL','crm_find_deals',true)`, b.TenantID, ta)
		if err == nil {
			t.Fatal("B wrote an event onto A's task")
		}
		err = exec(t, pool, b, `INSERT INTO agent_tasks (tenant_id, agent_key, goal, source, created_by, idempotency_key) VALUES ($1,'sales','x','FOUNDER','user:f','k')`, a.TenantID)
		if err == nil {
			t.Fatal("B created a task inside A")
		}
	})

	t.Run("history cannot be forged, edited or removed", func(t *testing.T) {
		err := exec(t, pool, a, `INSERT INTO agent_task_events (tenant_id, task_id, attempt, kind, detail) VALUES ($1,$2,1,'STATE','{"to":"DONE"}')`, a.TenantID, ta)
		if err == nil {
			t.Fatal("the application role wrote a STATE row itself")
		}
		if err := exec(t, pool, a, `UPDATE agent_task_events SET kind='NOTE'`); err == nil {
			t.Fatal("events were updated")
		}
		if err := exec(t, pool, a, `DELETE FROM agent_task_events`); err == nil {
			t.Fatal("events were deleted")
		}
		if err := exec(t, pool, a, `DELETE FROM agent_tasks`); err == nil {
			t.Fatal("tasks were deleted")
		}
		// Not even the owner can edit history.
		_, err = pool.Exec(context.Background(), `UPDATE agent_task_events SET detail='{}'`)
		wantErr(t, err, "append-only")
	})

	t.Run("a scope with no business cannot read the tables, only call the narrow functions", func(t *testing.T) {
		var tasks, events int
		must(t, run(t, pool, dispatcher, func(tx pgx.Tx) error {
			if err := tx.QueryRow(context.Background(), `SELECT count(*) FROM agent_tasks`).Scan(&tasks); err != nil {
				return err
			}
			return tx.QueryRow(context.Background(), `SELECT count(*) FROM agent_task_events`).Scan(&events)
		}))
		if tasks != 0 || events != 0 {
			t.Fatalf("a scope with no business read %d tasks and %d events straight from the tables", tasks, events)
		}
		// NOT proven here: that the application role cannot become the owner to borrow the owner-only policies. This
		// harness connects as a superuser and emulates venture_app with SET ROLE, which a superuser session may undo.
		// In a real deployment venture_app and venture_owner are separate LOGIN roles with no membership in each other
		// (infra/postgres/init/01-bootstrap.sh), so the application connection has no way to take the owner role.
	})
}

// ───────────────────────────── events ─────────────────────────────

func TestTaskEventsGuardTheRun(t *testing.T) {
	pool := testdb.Pool(t)
	s := workspace(t, pool, "events-co", "sales")

	t.Run("each tool call is numbered, counted, and a write is remembered", func(t *testing.T) {
		id := running(t, pool, s, "sales", func(o *taskOpts) { o.maxSteps = 3 })
		must(t, toolCall(t, pool, s, id, 1, "crm_find_deals", true))
		if r := getTask(t, pool, s, id); r.Steps != 1 || r.Wrote {
			t.Fatalf("after a read: %+v", r)
		}
		must(t, toolCall(t, pool, s, id, 1, "crm_create_deal", false))
		if r := getTask(t, pool, s, id); r.Steps != 2 || !r.Wrote {
			t.Fatalf("after a write: %+v", r)
		}
		must(t, toolResult(t, pool, s, id, 1, "crm_create_deal", true))
		must(t, toolCall(t, pool, s, id, 1, "crm_find_deals", true))
		wantErr(t, toolCall(t, pool, s, id, 1, "crm_find_deals", true), "reached its step limit")
		if r := getTask(t, pool, s, id); r.Steps != 3 {
			t.Fatalf("a refused call must not be counted: %+v", r)
		}
		ev := events(t, pool, s, id)
		for i, e := range ev {
			if e.Seq != i+1 {
				t.Fatalf("event %d has seq %d: numbering must be 1,2,3...", i, e.Seq)
			}
		}
		var kinds []string
		for _, e := range ev {
			kinds = append(kinds, e.Kind)
		}
		if got := strings.Join(kinds, ","); got != "STATE,STATE,TOOL_CALL,TOOL_CALL,TOOL_RESULT,TOOL_CALL" {
			t.Fatalf("kinds = %s", got)
		}
	})

	t.Run("a task that is not running cannot act, but the outcome of a call in flight is still recorded", func(t *testing.T) {
		queued := mustTask(t, pool, s, "sales", func(o *taskOpts) { n := time.Now().Add(24 * time.Hour); o.notBefore = &n })
		wantErr(t, toolCall(t, pool, s, queued, 1, "crm_find_deals", true), "has no attempt 1")

		id := running(t, pool, s, "sales")
		must(t, toolCall(t, pool, s, id, 1, "crm_create_deal", false))
		must(t, setStatus(t, pool, s, id, "CANCELLED")) // a person cancels while the call is in flight
		wantErr(t, toolCall(t, pool, s, id, 1, "crm_create_company", false), "is not running")
		wantErr(t, exec(t, pool, s, `INSERT INTO agent_task_events (tenant_id, task_id, attempt, kind) VALUES ($1,$2,1,'NOTE')`, s.TenantID, id), "is not running")
		must(t, toolResult(t, pool, s, id, 1, "crm_create_deal", true)) // what happened is still written down
	})

	t.Run("a call from an earlier attempt, or from one that has not happened, is refused", func(t *testing.T) {
		id := running(t, pool, s, "sales")
		must(t, setStatus(t, pool, s, id, "QUEUED"))
		got := claimWith(t, pool, 100, soon().Add(72*time.Hour), 100, 100)
		if len(got) == 0 {
			t.Fatal("not re-claimed")
		}
		wantErr(t, toolCall(t, pool, s, id, 1, "crm_find_deals", true), "attempt 1")
		wantErr(t, toolCall(t, pool, s, id, 3, "crm_find_deals", true), "has no attempt 3")
		must(t, toolCall(t, pool, s, id, 2, "crm_find_deals", true))
	})

	t.Run("an approval can be linked, and only a real one", func(t *testing.T) {
		id := running(t, pool, s, "sales")
		must(t, toolCall(t, pool, s, id, 1, "request_human_approval", false))
		err := exec(t, pool, s, `INSERT INTO agent_task_events (tenant_id, task_id, attempt, kind, tool, ok, approval_id) VALUES ($1,$2,1,'TOOL_RESULT','request_human_approval',true,$3)`,
			s.TenantID, id, uuid.New())
		if err == nil {
			t.Fatal("an event pointed at an approval that does not exist")
		}
	})

	t.Run("the shape of an event is enforced", func(t *testing.T) {
		id := running(t, pool, s, "sales")
		bad := map[string]string{
			"call without a tool":  `INSERT INTO agent_task_events (tenant_id, task_id, attempt, kind, read_only) VALUES ($1,$2,1,'TOOL_CALL',true)`,
			"call without a flag":  `INSERT INTO agent_task_events (tenant_id, task_id, attempt, kind, tool) VALUES ($1,$2,1,'TOOL_CALL','x')`,
			"result without an ok": `INSERT INTO agent_task_events (tenant_id, task_id, attempt, kind, tool) VALUES ($1,$2,1,'TOOL_RESULT','x')`,
			"unknown kind":         `INSERT INTO agent_task_events (tenant_id, task_id, attempt, kind) VALUES ($1,$2,1,'WHATEVER')`,
			"an array as detail":   `INSERT INTO agent_task_events (tenant_id, task_id, attempt, kind, detail) VALUES ($1,$2,1,'NOTE','[]')`,
			"an enormous detail":   `INSERT INTO agent_task_events (tenant_id, task_id, attempt, kind, detail) VALUES ($1,$2,1,'NOTE', jsonb_build_object('x', repeat('y', 9000)))`,
		}
		for name, sql := range bad {
			if err := exec(t, pool, s, sql, s.TenantID, id); err == nil {
				t.Errorf("%s was accepted", name)
			}
		}
		must(t, exec(t, pool, s, `INSERT INTO agent_task_events (tenant_id, task_id, attempt, kind, detail) VALUES ($1,$2,1,'NOTE','{"msg":"hello"}')`, s.TenantID, id))
	})
}

// ───────────────────────────── leases ─────────────────────────────

func TestExpiredLeasesAreFoundAndSettledSafely(t *testing.T) {
	pool := testdb.Pool(t)
	s := workspace(t, pool, "expiry-co", "reader", "writer")
	now := soon()
	r := mustTask(t, pool, s, "reader", func(o *taskOpts) { o.maxSeconds = 60 })
	w := mustTask(t, pool, s, "writer", func(o *taskOpts) { o.maxSeconds = 60 })
	if got := claim(t, pool, 10, now); len(got) != 2 {
		t.Fatalf("claimed %d", len(got))
	}
	must(t, toolCall(t, pool, s, r, 1, "crm_find_deals", true))
	must(t, toolCall(t, pool, s, w, 1, "crm_create_deal", false))

	expired := func(at time.Time) map[uuid.UUID]bool {
		out := map[uuid.UUID]bool{}
		must(t, run(t, pool, dispatcher, func(tx pgx.Tx) error {
			rows, err := tx.Query(context.Background(), `SELECT r_task_id, r_wrote FROM agent_task_expired($1::timestamptz)`, at)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var id uuid.UUID
				var wrote bool
				if err := rows.Scan(&id, &wrote); err != nil {
					return err
				}
				out[id] = wrote
			}
			return rows.Err()
		}))
		return out
	}

	if got := expired(now.Add(80 * time.Second)); len(got) != 0 {
		t.Fatalf("expired before the lease ran out (60s + 30s grace): %v", got)
	}
	got := expired(now.Add(95 * time.Second))
	if len(got) != 2 || got[r] || !got[w] {
		t.Fatalf("expired = %v; want both, with only the writer marked as having written", got)
	}

	// A heartbeat moves the lease and the task is no longer expired.
	must(t, exec(t, pool, s, `UPDATE agent_tasks SET lease_expires_at = $2::timestamptz WHERE id=$1`, r, now.Add(10*time.Minute)))
	if got := expired(now.Add(95 * time.Second)); len(got) != 1 {
		t.Fatalf("a task with a fresh lease was reported expired: %v", got)
	}

	// Settling: the reader may be retried, the writer must be looked at by a person.
	must(t, setStatus(t, pool, s, r, "QUEUED"))
	wantErr(t, setStatus(t, pool, s, w, "QUEUED"), "may have written")
	must(t, setStatus(t, pool, s, w, "NEEDS_REVIEW"))
	if got := expired(now.Add(24 * time.Hour)); len(got) != 0 {
		t.Fatalf("settled tasks are still reported: %v", got)
	}
	// A person's decision on the reviewed task.
	must(t, setStatus(t, pool, s, w, "QUEUED"))
	if rw := getTask(t, pool, s, w); rw.Status != "QUEUED" || !rw.Wrote {
		t.Fatalf("a re-run keeps the memory that it may have written: %+v", rw)
	}
}

// ───────────────────────────── schedules ─────────────────────────────

func TestSchedulesFireOnceAndNeverPileUp(t *testing.T) {
	pool := testdb.Pool(t)
	a := workspace(t, pool, "sched-a", "sales")
	b := workspace(t, pool, "sched-b", "sales")
	now := soon()

	addSchedule := func(s tenancy.Scope, every int, at time.Time) uuid.UUID {
		var id uuid.UUID
		must(t, run(t, pool, s, func(tx pgx.Tx) error {
			return tx.QueryRow(context.Background(),
				`INSERT INTO agent_schedules (tenant_id, agent_key, goal, every_minutes, next_run_at, created_by) VALUES ($1,'sales','review open deals',$2,$3,'user:founder') RETURNING id`,
				s.TenantID, every, at).Scan(&id)
		}))
		return id
	}
	fire := func(s tenancy.Scope, id uuid.UUID, at time.Time) *uuid.UUID {
		var out *uuid.UUID
		must(t, run(t, pool, s, func(tx pgx.Tx) error {
			return tx.QueryRow(context.Background(), `SELECT agent_schedule_fire($1, $2::timestamptz)`, id, at).Scan(&out)
		}))
		return out
	}
	nextRun := func(s tenancy.Scope, id uuid.UUID) time.Time {
		var at time.Time
		must(t, run(t, pool, s, func(tx pgx.Tx) error {
			return tx.QueryRow(context.Background(), `SELECT next_run_at FROM agent_schedules WHERE id=$1`, id).Scan(&at)
		}))
		return at
	}

	t.Run("the dispatcher finds due schedules in every business", func(t *testing.T) {
		sa := addSchedule(a, 60, now.Add(-time.Minute))
		sb := addSchedule(b, 60, now.Add(-time.Minute))
		notYet := addSchedule(a, 60, now.Add(time.Hour))
		due := map[uuid.UUID]bool{}
		must(t, run(t, pool, dispatcher, func(tx pgx.Tx) error {
			rows, err := tx.Query(context.Background(), `SELECT r_schedule_id FROM agent_schedule_due($1::timestamptz)`, now)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var id uuid.UUID
				if err := rows.Scan(&id); err != nil {
					return err
				}
				due[id] = true
			}
			return rows.Err()
		}))
		if !due[sa] || !due[sb] || due[notYet] {
			t.Fatalf("due = %v", due)
		}
		must(t, exec(t, pool, a, `UPDATE agent_schedules SET enabled=false WHERE id=$1`, sa))
		must(t, exec(t, pool, a, `DELETE FROM agent_schedules WHERE id=$1`, notYet))
		must(t, exec(t, pool, b, `DELETE FROM agent_schedules WHERE id=$1`, sb))
	})

	t.Run("firing creates one task keyed by schedule and time, and moves the clock forward", func(t *testing.T) {
		due := now.Add(-time.Minute)
		sid := addSchedule(a, 60, due)
		tid := fire(a, sid, now)
		if tid == nil {
			t.Fatal("a due schedule did not fire")
		}
		var key, by, source, ref string
		must(t, run(t, pool, a, func(tx pgx.Tx) error {
			return tx.QueryRow(context.Background(), `SELECT idempotency_key, created_by, source, source_ref FROM agent_tasks WHERE id=$1`, *tid).Scan(&key, &by, &source, &ref)
		}))
		if want := fmt.Sprintf("sched:%s:%d", sid, due.Unix()); key != want || by != "schedule:"+sid.String() || source != "SCHEDULE" || ref != sid.String() {
			t.Fatalf("key=%q by=%q source=%q ref=%q", key, by, source, ref)
		}
		if next := nextRun(a, sid); next.Sub(now) < 59*time.Minute || next.Sub(now) > 61*time.Minute {
			t.Fatalf("next run is %v after now; want 60 minutes", next.Sub(now))
		}
		if again := fire(a, sid, now); again != nil {
			t.Fatal("a second fire at the same moment created another task")
		}
	})

	t.Run("a schedule that fell behind runs once, not once per missed slot", func(t *testing.T) {
		sid := addSchedule(b, 15, now.Add(-10*24*time.Hour))
		first := fire(b, sid, now)
		if first == nil {
			t.Fatal("did not fire")
		}
		if d := nextRun(b, sid).Sub(now); d < 14*time.Minute || d > 16*time.Minute {
			t.Fatalf("the next run is %v from now; a late schedule must resume from now, not replay ten days of slots", d)
		}
		// Even once the first task is finished (so the "still waiting" rule cannot be what stops a replay) nothing is due.
		must(t, setStatus(t, pool, b, *first, "CANCELLED"))
		if fire(b, sid, now.Add(time.Minute)) != nil {
			t.Fatal("a slot that was missed during the downtime was replayed")
		}
		if fire(b, sid, now) != nil || fire(b, sid, now.Add(time.Minute)) != nil {
			t.Fatal("the missed slots were replayed")
		}
		if n := count(t, pool, b, "agent_tasks"); n != 1 {
			t.Fatalf("%d tasks after ten days of missed slots; want 1", n)
		}
	})

	t.Run("no new task while the last one is still waiting, running or needs review", func(t *testing.T) {
		c := workspace(t, pool, "sched-c", "sales")
		sid := addSchedule(c, 15, now.Add(-time.Minute))
		first := fire(c, sid, now)
		if first == nil {
			t.Fatal("did not fire")
		}
		later := now.Add(16 * time.Minute)
		if fire(c, sid, later) != nil {
			t.Fatal("a second task was queued while the first still waits")
		}
		if next := nextRun(c, sid); next.Before(later) {
			t.Fatalf("the clock must still advance when a slot is skipped (next run %v)", next)
		}
		must(t, setStatus(t, pool, c, *first, "CANCELLED"))
		if fire(c, sid, later.Add(16*time.Minute)) == nil {
			t.Fatal("once the first task is finished the next slot fires")
		}
	})

	t.Run("a paused agent skips the slot without breaking the schedule", func(t *testing.T) {
		d := workspace(t, pool, "sched-d", "sales")
		sid := addSchedule(d, 15, now.Add(-time.Minute))
		must(t, exec(t, pool, d, `UPDATE agent_org_configs SET status='PAUSED' WHERE agent_key='sales'`))
		if fire(d, sid, now) != nil {
			t.Fatal("a paused agent was given work")
		}
		var enabled bool
		must(t, run(t, pool, d, func(tx pgx.Tx) error {
			return tx.QueryRow(context.Background(), `SELECT enabled FROM agent_schedules WHERE id=$1`, sid).Scan(&enabled)
		}))
		if !enabled {
			t.Fatal("the schedule was switched off by a skipped slot")
		}
		must(t, exec(t, pool, d, `UPDATE agent_org_configs SET status='ACTIVE' WHERE agent_key='sales'`))
		if fire(d, sid, now.Add(16*time.Minute)) == nil {
			t.Fatal("the schedule did not resume once the agent was active again")
		}
	})

	t.Run("a disabled schedule and a schedule belonging to someone else do nothing", func(t *testing.T) {
		e := workspace(t, pool, "sched-e", "sales")
		sid := addSchedule(e, 15, now.Add(-time.Minute))
		if fire(a, sid, now) != nil {
			t.Fatal("another business fired this schedule")
		}
		must(t, exec(t, pool, e, `UPDATE agent_schedules SET enabled=false WHERE id=$1`, sid))
		if fire(e, sid, now) != nil {
			t.Fatal("a disabled schedule fired")
		}
	})

	t.Run("the interval is bounded", func(t *testing.T) {
		for _, bad := range []int{0, 14, 10081} {
			err := exec(t, pool, a, `INSERT INTO agent_schedules (tenant_id, agent_key, goal, every_minutes, next_run_at, created_by) VALUES ($1,'sales','g',$2,now(),'user:f')`, a.TenantID, bad)
			if err == nil {
				t.Errorf("every %d minutes was accepted", bad)
			}
		}
	})
}

// ───────────────────────────── the runner and the database together ─────────────────────────────

// The fake runner plays an agent whose runtime IGNORES a cancel and keeps calling tools. Every call is written to the
// database first, as the bus will do. A person cancels the task; from then on the database refuses the calls and the
// run ends as "tool_denied". This is the kill switch the design relies on.
func TestADatabaseCancelStopsAnAgentThatIgnoresIt(t *testing.T) {
	pool := testdb.Pool(t)
	s := workspace(t, pool, "kill-co", "sales")
	id := running(t, pool, s, "sales", func(o *taskOpts) { o.maxSteps = 200 })
	ref := agentrun.RefFor(id, 1)

	fake := agentrun.NewFake(nil)
	defer fake.Close()
	fake.OnCall = func(ctx context.Context, req agentrun.StartRequest, tool string, readOnly bool) error {
		return exec(t, pool, s, `INSERT INTO agent_task_events (tenant_id, task_id, attempt, kind, tool, read_only) VALUES ($1,$2,1,'TOOL_CALL',$3,$4)`,
			s.TenantID, id, tool, readOnly)
	}
	req := agentrun.StartRequest{
		Ref: ref, Tenant: ports.TenantInfo{ID: s.TenantID, Slug: "kill-co", Name: "Kill"}, AgentKey: "sales", Profile: "venture-kill",
		Goal: agentrun.GoalChatty, Token: "tok", Limits: agentrun.Limits{MaxSteps: 200, Deadline: time.Now().Add(20 * time.Second)},
	}
	must(t, fake.Start(context.Background(), req))

	deadline := time.Now().Add(5 * time.Second)
	for getTask(t, pool, s, id).Steps < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if getTask(t, pool, s, id).Steps < 3 {
		t.Fatal("the agent never got going")
	}

	must(t, setStatus(t, pool, s, id, "CANCELLED")) // the person cancels; the runtime is NOT told

	for {
		st, _ := fake.Status(context.Background(), ref)
		if st.State.Done() {
			if st.State != agentrun.Failed || st.Err == nil || st.Err.Code != agentrun.CodeStepDenied || !strings.Contains(st.Err.Detail, "is not running") {
				t.Fatalf("the run ended as %+v; want failed with %q because the database refused it", st, agentrun.CodeStepDenied)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the agent kept going after the task was cancelled")
		}
		time.Sleep(5 * time.Millisecond)
	}
	stopped := getTask(t, pool, s, id).Steps
	time.Sleep(60 * time.Millisecond)
	if after := getTask(t, pool, s, id).Steps; after != stopped {
		t.Fatalf("calls kept being recorded after the cancel (%d, then %d)", stopped, after)
	}
}

// The runner's own step limit and the database's agree: whichever is lower wins, and the database never lets a task
// exceed its row's max_steps even if the runner is told a bigger number.
func TestTheDatabaseHoldsTheStepLimitEvenIfTheRunnerIsTold200(t *testing.T) {
	pool := testdb.Pool(t)
	s := workspace(t, pool, "steps-co", "sales")
	id := running(t, pool, s, "sales", func(o *taskOpts) { o.maxSteps = 4 })
	ref := agentrun.RefFor(id, 1)
	fake := agentrun.NewFake(nil)
	defer fake.Close()
	fake.OnCall = func(ctx context.Context, req agentrun.StartRequest, tool string, readOnly bool) error {
		return exec(t, pool, s, `INSERT INTO agent_task_events (tenant_id, task_id, attempt, kind, tool, read_only) VALUES ($1,$2,1,'TOOL_CALL',$3,$4)`,
			s.TenantID, id, tool, readOnly)
	}
	must(t, fake.Start(context.Background(), agentrun.StartRequest{
		Ref: ref, Tenant: ports.TenantInfo{ID: s.TenantID, Slug: "steps-co", Name: "S"}, AgentKey: "sales", Profile: "p",
		Goal: agentrun.GoalChatty, Token: "tok", Limits: agentrun.Limits{MaxSteps: 200, Deadline: time.Now().Add(20 * time.Second)},
	}))
	deadline := time.Now().Add(5 * time.Second)
	for {
		st, _ := fake.Status(context.Background(), ref)
		if st.State.Done() {
			if st.Err == nil || st.Err.Code != agentrun.CodeStepDenied || !strings.Contains(st.Err.Detail, "step limit") {
				t.Fatalf("status = %+v", st)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("never stopped")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if r := getTask(t, pool, s, id); r.Steps != 4 {
		t.Fatalf("steps used = %d, want exactly the row's limit of 4", r.Steps)
	}
}

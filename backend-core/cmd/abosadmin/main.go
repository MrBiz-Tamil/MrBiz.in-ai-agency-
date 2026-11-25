// Command abosadmin runs one-off administration tasks: migrations, workspaces, users and
// development seed data. Passwords are read from the environment, never from arguments
// (arguments end up in shell history and process listings).
//
//	abosadmin migrate up
//	abosadmin migrate down [steps]
//	abosadmin create-workspace <name>
//	ABOS_ADMIN_PASSWORD=... abosadmin create-user <workspace-id> <email> <owner|admin|operator|auditor>
//	ABOS_ENV=dev ABOS_ADMIN_PASSWORD=... abosadmin seed-dev
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"github.com/mrbiz/abos/backend-core/pkg/auth"
	"github.com/mrbiz/abos/backend-core/pkg/db"
	"github.com/mrbiz/abos/backend-core/pkg/db/repository"
	"github.com/mrbiz/abos/backend-core/pkg/events"
	"github.com/mrbiz/abos/backend-core/pkg/kernel"
	"github.com/mrbiz/abos/backend-core/pkg/tickets"
)

func fail(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", a...)
	os.Exit(1)
}

func main() {
	if len(os.Args) < 2 {
		fail("usage: abosadmin <migrate|create-workspace|create-user|seed-dev> ...")
	}
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		fail("DATABASE_URL is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	pool, err := db.NewTenantPool(ctx, url, os.Getenv("ABOS_DB_ROLE"))
	if err != nil {
		fail("connect: %v", err)
	}
	defer pool.Close()
	repo := repository.NewRepository(pool)

	switch os.Args[1] {
	case "migrate":
		migrate(ctx, pool, os.Args[2:])
	case "create-workspace":
		if len(os.Args) != 3 {
			fail("usage: abosadmin create-workspace <name>")
		}
		ws, err := repo.CreateWorkspace(ctx, os.Args[2], "INR", "GST_INDIA", decimal.NewFromInt(100))
		if err != nil {
			fail("%v", err)
		}
		fmt.Println(ws.ID)
	case "create-user":
		if len(os.Args) != 5 {
			fail("usage: ABOS_ADMIN_PASSWORD=... abosadmin create-user <workspace-id> <email> <role>")
		}
		u, err := createUser(ctx, repo, os.Args[2], os.Args[3], os.Args[4], os.Getenv("ABOS_ADMIN_PASSWORD"))
		if err != nil {
			fail("%v", err)
		}
		fmt.Printf("created %s (%s) in workspace %s\n", u.Email, u.Role, u.WorkspaceID)
	case "seed-dev":
		if os.Getenv("ABOS_ENV") != "dev" {
			fail("seed-dev only runs with ABOS_ENV=dev")
		}
		if err := seedDev(ctx, pool, repo, os.Getenv("ABOS_ADMIN_PASSWORD")); err != nil {
			fail("%v", err)
		}
	default:
		fail("unknown command %q", os.Args[1])
	}
}

func migrate(ctx context.Context, pool db.Pool, args []string) {
	if len(args) == 0 {
		fail("usage: abosadmin migrate up|down [steps]")
	}
	switch args[0] {
	case "up":
		ran, err := db.Up(ctx, pool)
		if err != nil {
			fail("%v", err)
		}
		fmt.Printf("applied: %v\n", ran)
	case "down":
		steps := 1
		if len(args) > 1 {
			n, err := strconv.Atoi(args[1])
			if err != nil || n < 1 {
				fail("steps must be a positive integer")
			}
			steps = n
		}
		ran, err := db.Down(ctx, pool, steps)
		if err != nil {
			fail("%v", err)
		}
		fmt.Printf("reverted: %v\n", ran)
	default:
		fail("usage: abosadmin migrate up|down [steps]")
	}
}

func createUser(ctx context.Context, repo *repository.Repository, workspaceID, email, role, password string) (*repository.User, error) {
	switch auth.Role(role) {
	case auth.RoleOwner, auth.RoleAdmin, auth.RoleOperator, auth.RoleAuditor:
	default:
		return nil, errors.New("role must be owner, admin, operator or auditor")
	}
	if !strings.Contains(email, "@") {
		return nil, errors.New("email looks invalid")
	}
	if password == "" {
		return nil, errors.New("set ABOS_ADMIN_PASSWORD (12-72 characters)")
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return nil, err
	}
	return repo.CreateUser(ctx, repository.User{WorkspaceID: workspaceID, Email: email, DisplayName: strings.Split(email, "@")[0], Role: role, PasswordHash: hash})
}

type simExec struct{}

func (simExec) Execute(_ context.Context, _, tool, _ string) (string, error) {
	b, _ := json.Marshal(map[string]any{"status": "success", "simulated": true, "action": tool})
	return string(b), nil
}

// seedDev creates a demo workspace with the 8 specialist agents, users and a few tickets.
func seedDev(ctx context.Context, pool db.Pool, repo *repository.Repository, password string) error {
	if password == "" {
		return errors.New("set ABOS_ADMIN_PASSWORD (12-72 characters) to choose the demo users' password")
	}
	if _, err := db.Up(ctx, pool); err != nil {
		return err
	}
	if existing, _ := repo.FindUsersByEmail(ctx, "owner@demo.test"); len(existing) > 0 {
		fmt.Println("demo data already present; nothing to do")
		return nil
	}
	ws, err := repo.CreateWorkspace(ctx, "Mrbiz Demo", "INR", "GST_INDIA", decimal.NewFromInt(100))
	if err != nil {
		return err
	}
	for email, role := range map[string]string{"owner@demo.test": "owner", "operator@demo.test": "operator", "auditor@demo.test": "auditor"} {
		if _, err := createUser(ctx, repo, ws.ID, email, role, password); err != nil {
			return err
		}
	}

	type agent struct {
		role, tier, cron string
		budget           int64
	}
	roster := []agent{
		{"Executive Business Planner", "FRONTIER", "0 8 * * *", 10},
		{"Customer Support Voice Agent", "FLASH", "", 10},
		{"Sales & Cart Recovery Voice Agent", "FLASH", "", 8},
		{"B2B & Distributor Desk Agent", "FRONTIER", "", 12},
		{"Procurement & Vendor Agent", "FRONTIER", "*/30 * * * *", 8},
		{"Finance & Reconciliation Auditor", "FRONTIER", "0 23 * * *", 15},
		{"Multi-Channel Catalog & Inventory Router", "FLASH", "*/15 * * * *", 5},
		{"SOP & Knowledge Sync Agent", "FLASH", "", 5},
	}
	for _, a := range roster {
		def := repository.AgentDefinition{WorkspaceID: ws.ID, RoleName: a.role, ModelTier: a.tier, DailyBudget: decimal.NewFromInt(a.budget)}
		if a.cron != "" {
			c := a.cron
			def.HeartbeatCron = &c
		}
		if _, err := repo.CreateAgent(ctx, def); err != nil {
			return err
		}
	}

	svc := tickets.NewService(repo, kernel.NewPolicyEvaluator(), simExec{}, events.NewBroker())
	for _, t := range []tickets.SubmitInput{
		{WorkspaceID: ws.ID, IdempotencyKey: "seed-invoice-1042", ActionType: "books.draft_invoice", Priority: 2, Payload: json.RawMessage(`{"order_id":"ord_1042","amount_inr":"4200.00"}`)},
		{WorkspaceID: ws.ID, IdempotencyKey: "seed-refund-9921", ActionType: "order.refund_requested", Priority: 1, Payload: json.RawMessage(`{"order_id":"ord_9921","refund_amount":"1500.00","reason":"Defective zipper"}`)},
		{WorkspaceID: ws.ID, IdempotencyKey: "seed-discount-77", ActionType: "order.discount_requested", Priority: 3, Payload: json.RawMessage(`{"order_id":"ord_77","discount_pct":"18"}`)},
		{WorkspaceID: ws.ID, IdempotencyKey: "seed-sync-88", ActionType: "inventory.sync_buffer", Priority: 4, Payload: json.RawMessage(`{"sku":"SKU-POLO-BLUE-M","odoo_qty":12,"shopify_qty":4}`)},
	} {
		if _, err := svc.Submit(ctx, t); err != nil {
			return err
		}
	}
	fmt.Printf("seeded workspace %s\nusers: owner@demo.test, operator@demo.test, auditor@demo.test (password from ABOS_ADMIN_PASSWORD)\n", ws.ID)
	return nil
}

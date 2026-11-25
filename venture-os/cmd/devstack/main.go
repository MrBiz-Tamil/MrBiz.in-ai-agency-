// Command devstack runs the whole Venture OS control plane on a local development database, with no Docker,
// no Keycloak and no API key, so the Command Center can be developed and demonstrated.
//
//   - Database: any Postgres reachable via DEVSTACK_DATABASE_URL (a superuser URL), typically PGlite from
//     tools/pgtest. Roles and schema are created on first run.
//   - Auth: static dev tokens (dev-founder, dev-viewer). NEVER expose this to a network.
//   - CRM: the real built-in CRM (same code as production). Other external systems: in-memory fakes, so their
//     provisioning is simulated.
//   - LLM: the real Anthropic client when ANTHROPIC_API_KEY is set, otherwise a scripted demo interviewer.
//   - DEVSTACK_SEED=1 creates two demo businesses with pending approvals.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	agentv1 "github.com/mrbiz/venture-os/gen/venture/agent/v1"
	"github.com/mrbiz/venture-os/gen/venture/agent/v1/agentv1connect"
	"github.com/mrbiz/venture-os/internal/auth"
	"github.com/mrbiz/venture-os/internal/connectors/anthropic"
	"github.com/mrbiz/venture-os/internal/controlplane"
	"github.com/mrbiz/venture-os/internal/crm"
	"github.com/mrbiz/venture-os/internal/crm/builtin"
	"github.com/mrbiz/venture-os/internal/devdb"
	"github.com/mrbiz/venture-os/internal/fakes"
	"github.com/mrbiz/venture-os/internal/fixtures"
	"github.com/mrbiz/venture-os/internal/genesis"
	"github.com/mrbiz/venture-os/internal/mcptoken"
	"github.com/mrbiz/venture-os/internal/ports"
	"github.com/mrbiz/venture-os/internal/provisioner"
	"github.com/mrbiz/venture-os/internal/store"
	"github.com/mrbiz/venture-os/internal/tenancy"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)
	if err := run(log); err != nil {
		log.Error("devstack stopped", "err", err)
		os.Exit(1)
	}
}

func env(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

func run(log *slog.Logger) error {
	dbURL := env("DEVSTACK_DATABASE_URL", "")
	if dbURL == "" {
		return errors.New("DEVSTACK_DATABASE_URL is required (e.g. postgres://postgres@127.0.0.1:25432/postgres?sslmode=disable&pool_max_conns=1&default_query_exec_mode=describe_exec)")
	}
	addr := env("DEVSTACK_ADDR", "127.0.0.1:8080")
	if host, _, err := net.SplitHostPort(addr); err != nil || (host != "127.0.0.1" && host != "localhost" && host != "::1") {
		return fmt.Errorf("devstack has no real authentication and must bind to loopback, got %q", addr)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	applied, err := devdb.Ensure(ctx, pool, env("DEVSTACK_SCHEMA", "db/schema.sql"))
	if err != nil {
		return err
	}
	if applied {
		log.Info("schema applied")
	}
	st := store.New(pool, devdb.Role)

	signer, err := mcptoken.NewSigner([]byte("devstack-signing-key-0123456789abcdef"))
	if err != nil {
		return err
	}
	hub := fakes.NewHub()
	crmProvider := builtin.New(pool, tenancy.Options{AssumeRole: devdb.Role})
	collabReg, err := devCollab(log, st)
	if err != nil {
		return err
	}
	prov := &provisioner.Provisioner{
		Identity: fakes.NewIdentity(hub), ERP: fakes.NewERP(hub), CRM: builtin.NewWorkspace(crmProvider), Support: fakes.NewSupport(hub),
		Runtime: fakes.NewRuntime(hub), Store: st, Tokens: signer, Log: log,
	}

	var llm ports.LLM = &demoLLM{}
	if key := env("ANTHROPIC_API_KEY", ""); key != "" {
		model := env("GENESIS_MODEL", "claude-sonnet-5-5")
		llm = &anthropic.Client{APIKey: key, DefaultModel: model}
		log.Info("interviewer: Anthropic", "model", model)
	} else {
		log.Info("interviewer: scripted demo (set ANTHROPIC_API_KEY for the real one)")
	}

	srv := &controlplane.Server{
		Repo: st, Interviewer: &genesis.Engine{LLM: llm, Model: env("GENESIS_MODEL", "claude-sonnet-5-5")},
		Provisioner: prov, AgentSigner: signer, CRM: crmProvider, Collab: collabReg, Log: log,
	}
	verifier := auth.StaticVerifier{"dev-founder": "sub-dev-founder", "dev-approver": "sub-dev-approver", "dev-viewer": "sub-dev-viewer"}
	httpSrv := &http.Server{
		Addr: addr, Handler: h2c.NewHandler(srv.Handler(verifier), &http2.Server{}),
		ReadHeaderTimeout: 10 * time.Second, WriteTimeout: 3 * time.Minute,
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	errCh := make(chan error, 1)
	go func() { errCh <- httpSrv.Serve(ln) }()
	log.Info("devstack listening", "addr", addr, "tokens", "dev-founder, dev-approver, dev-viewer")

	if env("DEVSTACK_SEED", "") == "1" {
		if err := seed(ctx, log, st, prov, signer, crmProvider, "http://"+addr); err != nil {
			return fmt.Errorf("seed: %w", err)
		}
	}
	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return httpSrv.Shutdown(shutdown)
	}
}

// ───────────────────────────── demo interviewer ─────────────────────────────

var demoSeq atomic.Int64

// demoLLM is a deterministic stand-in for the Venture Architect: it asks one question, then proposes the sample
// export business. It exists so the wizard can be exercised without an API key.
type demoLLM struct{}

func (d *demoLLM) Complete(_ context.Context, req ports.LLMRequest) (ports.LLMResponse, error) {
	founderTurns := 0
	for _, m := range req.Messages {
		if m.Role == "user" && !strings.HasPrefix(m.Content, "SYSTEM VALIDATION FAILED") {
			founderTurns++
		}
	}
	var env map[string]any
	if founderTurns <= 1 {
		env = map[string]any{"message": "Great idea. Which country will you operate from, and who are your first customers?", "blueprint": nil, "ready": false}
	} else {
		bp := fixtures.EXIMBlueprint()
		suffix := fmt.Sprintf("%04d", demoSeq.Add(1)+time.Now().UnixNano()%9000)
		bp.Business.Slug = "kerala-spice-" + suffix
		bp.Business.Name = "Kerala Spice Exports " + suffix
		env = map[string]any{"message": "Here is the plan I'd run: a landed-cost pricing rule with a 12% margin floor, an export-deals pipeline, " +
			"a five-agent team, and approval for every quote over USD 10,000. Review the preview on the right, and approve when you are happy.", "blueprint": bp, "ready": true}
	}
	raw, err := json.Marshal(env)
	return ports.LLMResponse{Content: string(raw)}, err
}

// ───────────────────────────── seed data ─────────────────────────────

func seed(ctx context.Context, log *slog.Logger, st *store.PG, prov *provisioner.Provisioner, signer *mcptoken.Signer, crmProvider crm.Provider, base string) error {
	const founder = "sub-dev-founder"
	if ms, err := st.Memberships(ctx, founder); err != nil {
		return err
	} else if len(ms) > 0 {
		log.Info("seed skipped: the dev founder already has businesses")
		return nil
	}
	mk := func(slug, name, industry, country, currency string) (uuid.UUID, error) {
		id, _, err := st.CreateTenant(ctx, founder, "seed-"+slug)
		if err != nil {
			return uuid.Nil, err
		}
		bp := fixtures.EXIMBlueprint()
		bp.Business.Slug, bp.Business.Name, bp.Business.Industry, bp.Business.Country, bp.Business.BaseCurrency = slug, name, industry, country, currency
		sb, err := st.SaveBlueprint(ctx, id, nil, bp, founder, "bp-"+slug)
		if err != nil {
			return uuid.Nil, err
		}
		if _, err := st.ApproveBlueprint(ctx, id, sb.ID, founder); err != nil {
			return uuid.Nil, err
		}
		tn, err := st.Tenant(ctx, id)
		if err != nil {
			return uuid.Nil, err
		}
		if _, err := prov.Provision(ctx, provisioner.Request{Tenant: tn.Info(), FounderSub: founder, BlueprintID: sb.ID, Blueprint: sb.Blueprint, IdempotencyKey: "prov-" + slug}); err != nil {
			return uuid.Nil, err
		}
		_, _, err = st.PostJournal(ctx, id, store.Journal{Memo: "opening capital", CreatedBy: founder, IdempotencyKey: "capital", Entries: []store.LedgerEntry{
			{AccountCode: "1010", Debit: 500000000, Currency: currency}, {AccountCode: "3000", Credit: 500000000, Currency: currency}}})
		return id, err
	}
	a, err := mk("kerala-spice-exports", "Kerala Spice Exports", "export-import", "IN", "INR")
	if err != nil {
		return err
	}
	// Teammates with narrower roles, so each can be tried in the browser (DEV_USER=approver|viewer).
	for sub, role := range map[string]string{"sub-dev-approver": "approver", "sub-dev-viewer": "viewer"} {
		if err := st.AddMember(ctx, a, sub, role); err != nil {
			return err
		}
	}
	if err := seedCRM(ctx, crmProvider, a); err != nil {
		return fmt.Errorf("seed crm: %w", err)
	}
	b, err := mk("madurai-textiles", "Madurai Textiles", "retail", "IN", "INR")
	if err != nil {
		return err
	}
	if _, _, err := st.CreateTenant(ctx, founder, "seed-draft"); err != nil {
		return err
	}

	for tenant, key := range map[uuid.UUID]string{a: "a", b: "b"} {
		tok, err := signer.Mint(tenant, "sales", 24*time.Hour)
		if err != nil {
			return err
		}
		c := agentv1connect.NewAgentServiceClient(http.DefaultClient, base)
		call := func(err error) {
			if err != nil {
				log.Error("seed call failed", "err", err)
			}
		}
		withTok := func(r *connect.Request[agentv1.CreateQuoteRequest]) *connect.Request[agentv1.CreateQuoteRequest] {
			r.Header().Set("Authorization", "Bearer "+tok)
			return r
		}
		_, e := c.CreateQuote(ctx, withTok(connect.NewRequest(&agentv1.CreateQuoteRequest{
			RuleName: "eu-landed-cost", Customer: "Berlin Gewürze GmbH", IdempotencyKey: "seed-q-" + key,
			InputJson: []byte(`{"exim":{"incoterm":"FOB","quantity":1000,"unit_cost_minor":800,"packing_minor":50000,"inland_freight_minor":30000}}`)})))
		call(e)
		if key == "a" {
			_, e = c.CreateQuote(ctx, withTok(connect.NewRequest(&agentv1.CreateQuoteRequest{
				RuleName: "eu-landed-cost", Customer: "Hamburg Importers AG (discounted)", IdempotencyKey: "seed-q-disc",
				InputJson: []byte(`{"price_minor":950000,"exim":{"incoterm":"FOB","quantity":1000,"unit_cost_minor":800,"packing_minor":50000,"inland_freight_minor":30000}}`)})))
			call(e)
			inv := connect.NewRequest(&agentv1.DraftInvoiceRequest{Draft: &agentv1.DocumentDraft{
				Counterparty: "Berlin Gewürze GmbH", TaxCode: "GST5", DueInDays: 30, Reference: "PO-4471", IdempotencyKey: "seed-inv",
				Lines: []*agentv1.DocumentLine{{Description: "Black pepper, 1000 kg", Quantity: 1000, UnitPriceMinor: 1112}, {Description: "Freight", Quantity: 1, UnitPriceMinor: 12345}}}})
			inv.Header().Set("Authorization", "Bearer "+tok)
			_, e = c.DraftInvoice(ctx, inv)
			call(e)
			pay := connect.NewRequest(&agentv1.RequestApprovalRequest{Kind: "PAYMENT", Title: "Pay Maersk ocean freight", Summary: "Invoice 4411 for the Kochi–Hamburg container.",
				AmountMinor: 12000000, Currency: "INR", IdempotencyKey: "seed-pay", PayloadJson: []byte(`{"payee":"Maersk Line","invoice":"4411","due":"2026-12-05"}`)})
			pay.Header().Set("Authorization", "Bearer "+tok)
			_, e = c.RequestApproval(ctx, pay)
			call(e)
			blk := connect.NewRequest(&agentv1.EscalateBlockerRequest{Title: "No cardamom quote", Details: "The Idukki co-op has not replied to three requests in five days. I cannot price the cardamom line without a supplier quote.",
				Severity: "HIGH", IdempotencyKey: "seed-blk"})
			blk.Header().Set("Authorization", "Bearer "+tok)
			_, e = c.EscalateBlocker(ctx, blk)
			call(e)
		}
	}
	log.Info("seeded demo businesses", "kerala", a, "madurai", b)
	return nil
}

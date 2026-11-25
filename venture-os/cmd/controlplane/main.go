// Command controlplane serves the Venture OS control plane: tenants, Genesis and the Provisioner over ConnectRPC.
//
//	controlplane              run the server
//	controlplane healthcheck  exit 0 if the local server answers /healthz (used by the container health check)
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	"github.com/mrbiz/venture-os/internal/auth"
	"github.com/mrbiz/venture-os/internal/collabsvc"
	"github.com/mrbiz/venture-os/internal/connectors"
	"github.com/mrbiz/venture-os/internal/connectors/anthropic"
	"github.com/mrbiz/venture-os/internal/controlplane"
	"github.com/mrbiz/venture-os/internal/genesis"
	"github.com/mrbiz/venture-os/internal/mcptoken"
	"github.com/mrbiz/venture-os/internal/provisioner"
	"github.com/mrbiz/venture-os/internal/store"
	"github.com/mrbiz/venture-os/internal/vault"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck())
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(log)
	if err := run(log); err != nil {
		log.Error("controlplane stopped", "err", err)
		os.Exit(1)
	}
}

type config struct {
	env          string
	addr         string
	databaseURL  string
	oidcIssuer   string
	oidcAudience string
	tokenKey     []byte
	anthropicKey string
	genesisModel string
	connectors   string
}

func loadConfig() (config, error) {
	get := func(k, def string) string {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
		return def
	}
	c := config{
		env:          get("VENTURE_ENV", "production"),
		addr:         get("VENTURE_HTTP_ADDR", ":8080"),
		databaseURL:  get("DATABASE_URL", ""),
		oidcIssuer:   get("OIDC_ISSUER_URL", ""),
		oidcAudience: get("OIDC_AUDIENCE", "venture-api"),
		tokenKey:     []byte(get("MCP_TOKEN_SIGNING_KEY", "")),
		anthropicKey: get("ANTHROPIC_API_KEY", ""),
		genesisModel: get("GENESIS_MODEL", "claude-sonnet-5-5"),
		connectors:   get("VENTURE_CONNECTORS", ""),
	}
	var missing []string
	for k, v := range map[string]string{
		"DATABASE_URL": c.databaseURL, "OIDC_ISSUER_URL": c.oidcIssuer,
		"MCP_TOKEN_SIGNING_KEY": string(c.tokenKey), "ANTHROPIC_API_KEY": c.anthropicKey,
		"VENTURE_CONNECTORS": c.connectors,
	} {
		if v == "" {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return c, fmt.Errorf("missing required environment: %s", strings.Join(missing, ", "))
	}
	return c, nil
}

func run(log *slog.Logger) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := pgxpool.New(ctx, cfg.databaseURL)
	if err != nil {
		return fmt.Errorf("database: %w", err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("database ping: %w", err)
	}
	if cfg.env != "dev" {
		if err := store.EnsureRestrictedRole(ctx, pool); err != nil {
			return err
		}
	}
	st := store.New(pool, "")

	signer, err := mcptoken.NewSigner(cfg.tokenKey)
	if err != nil {
		return err
	}

	set, err := connectors.BuildWith(os.Getenv, log, connectors.Deps{DB: pool})
	if err != nil {
		return err
	}
	prov := &provisioner.Provisioner{
		Store: st, Tokens: signer, Log: log,
		Identity: set.Identity, ERP: set.ERP, CRM: set.CRM, Support: set.Support, Runtime: set.Runtime,
	}

	discoverCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	verifier, err := auth.NewOIDCVerifierWithRetry(discoverCtx, cfg.oidcIssuer, cfg.oidcAudience, 3*time.Second)
	cancel()
	if err != nil {
		return err
	}

	// Third-party integrations need a secrets key. Without one they are switched off, never run insecurely.
	var collabReg *collabsvc.Registry
	if v, verr := vault.FromEnv(os.Getenv); verr == nil {
		collabReg = &collabsvc.Registry{Store: st, Vault: v}
		log.Info("integrations enabled (Google Calendar/Meet, Zoom, Notion)")
	} else {
		log.Warn("integrations are DISABLED: set VENTURE_SECRETS_KEYS to enable them", "reason", verr.Error())
	}

	srv := &controlplane.Server{
		Collab:      collabReg,
		Repo:        st,
		Interviewer: &genesis.Engine{LLM: &anthropic.Client{APIKey: cfg.anthropicKey, DefaultModel: cfg.genesisModel}, Model: cfg.genesisModel},
		Provisioner: prov,
		CRM:         set.CRMData,
		AgentSigner: signer, // verifies the per-tenant MCP tokens that authenticate AgentService calls
		Log:         log,
	}
	httpSrv := &http.Server{
		Addr:              cfg.addr,
		Handler:           h2c.NewHandler(srv.Handler(verifier), &http2.Server{}),
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      3 * time.Minute, // longer than the 60s provisioning budget plus rollback
		IdleTimeout:       2 * time.Minute,
	}
	ln, err := net.Listen("tcp", cfg.addr)
	if err != nil {
		return err
	}
	errCh := make(chan error, 1)
	go func() { errCh <- httpSrv.Serve(ln) }()
	log.Info("controlplane listening", "addr", cfg.addr, "simulated_systems", set.Simulated)

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return httpSrv.Shutdown(shutdown)
	}
}

func healthcheck() int {
	addr := os.Getenv("VENTURE_HTTP_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return 1
	}
	client := http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://127.0.0.1:" + port + "/healthz")
	if err != nil {
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}

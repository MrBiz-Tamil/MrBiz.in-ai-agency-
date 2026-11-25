package main

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/99designs/gqlgen/graphql/playground"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/shopspring/decimal"
	"google.golang.org/grpc"

	"github.com/mrbiz/abos/backend-core/pkg/auth"
	"github.com/mrbiz/abos/backend-core/pkg/db"
	"github.com/mrbiz/abos/backend-core/pkg/db/repository"
	"github.com/mrbiz/abos/backend-core/pkg/events"
	kernelv1 "github.com/mrbiz/abos/backend-core/pkg/gen/abos/kernel/v1"
	gql "github.com/mrbiz/abos/backend-core/pkg/graphql"
	coregrpc "github.com/mrbiz/abos/backend-core/pkg/grpc"
	"github.com/mrbiz/abos/backend-core/pkg/ingest"
	"github.com/mrbiz/abos/backend-core/pkg/kernel"
	"github.com/mrbiz/abos/backend-core/pkg/mcp"
	"github.com/mrbiz/abos/backend-core/pkg/tickets"
	"github.com/mrbiz/abos/backend-core/pkg/vault"
)

func main() {
	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("configuration error: %v", err)
	}
	log.Printf("Starting ABOS Core Backend (env=%s)...", cfg.Env)

	// 1. Infrastructure: Redis + Postgres. Required in production; optional in dev.
	var rdb *redis.Client
	if cfg.RedisAddr != "" {
		rdb = redis.NewClient(&redis.Options{Addr: cfg.RedisAddr, Password: cfg.RedisPass})
		pctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		if err := rdb.Ping(pctx).Err(); err != nil {
			cancel()
			log.Fatalf("cannot reach Redis at %s: %v", cfg.RedisAddr, err)
		}
		cancel()
	} else {
		log.Println("WARNING: REDIS_ADDR not set; using in-process idempotency/budget state and disabling webhook ingest (dev only)")
	}

	var repo *repository.Repository
	if cfg.DatabaseURL != "" {
		// Migrations run on their own connection WITHOUT the restricted application role
		// (which cannot change the schema), then that connection is closed.
		if cfg.AutoMigrate {
			admin, err := pgxpool.New(context.Background(), cfg.DatabaseURL)
			if err != nil {
				log.Fatalf("cannot create Postgres pool for migrations: %v", err)
			}
			mctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			ran, err := db.Up(mctx, admin)
			cancel()
			admin.Close()
			if err != nil {
				log.Fatalf("migration failed: %v", err)
			}
			if len(ran) > 0 {
				log.Printf("applied migrations: %s", strings.Join(ran, ", "))
			}
		}

		pool, err := db.NewTenantPool(context.Background(), cfg.DatabaseURL, cfg.DBRole)
		if err != nil {
			log.Fatalf("cannot create Postgres pool: %v", err)
		}
		defer pool.Close()
		pctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := pool.Ping(pctx); err != nil {
			cancel()
			log.Fatalf("cannot reach Postgres: %v", err)
		}
		cancel()
		repo = repository.NewRepository(pool)
	} else {
		log.Println("WARNING: DATABASE_URL not set; GraphQL and login are disabled and audit records are kept in memory only (dev only)")
	}

	// 2. Credential vault
	var vlt *vault.Vault
	if cfg.VaultKey != "" {
		vlt, err = vault.New(cfg.VaultKey)
		if err != nil {
			log.Fatalf("ABOS_VAULT_KEY: %v", err)
		}
	} else {
		vlt, _ = vault.NewRandom()
		log.Println("WARNING: ABOS_VAULT_KEY not set; using an ephemeral key, so stored MCP credentials will not survive a restart (dev only)")
	}

	// 3. Deterministic kernel + ticket service
	idempMgr := kernel.NewIdempotencyManager(rdb)
	budgetBreaker := kernel.NewBudgetCircuitBreaker(rdb)
	policyEval := kernel.NewPolicyEvaluator()
	broker := events.NewBroker()

	var executor tickets.Executor
	var gateway *mcp.Gateway
	if repo != nil {
		gateway = mcp.NewGateway(repo, vlt, &mcp.Client{HTTP: mcp.NewHTTPClient(cfg.IsDev())})
		executor = gateway
		if cfg.IsDev() {
			executor = devFallbackExecutor{primary: gateway} // tools with no registered MCP server are simulated
		}
		loadPolicies(context.Background(), repo, policyEval)
	}

	var ticketSvc *tickets.Service
	if repo != nil {
		ticketSvc = tickets.NewService(repo, policyEval, executor, broker)
		go purgeSessions(repo)
	}

	kopts := coregrpc.KernelOptions{DefaultDailyLimit: cfg.DefaultAgentDailyLimit}
	if repo != nil {
		kopts.Executor = executor
		kopts.Audit = &coregrpc.PostgresAuditSink{Repo: repo}
		kopts.Tickets = ticketSvc
		kopts.LimitFn = func(ctx context.Context, workspaceID, agentID string) (decimal.Decimal, error) {
			budget, status, err := repo.GetAgentDailyBudget(ctx, workspaceID, agentID)
			if err != nil {
				return decimal.Zero, err
			}
			if status != "ACTIVE" {
				return decimal.Zero, nil // paused / budget-capped agents get no budget
			}
			return budget, nil
		}
		kopts.OnBudget = budgetAlerts(repo, broker)
	} else if cfg.IsDev() {
		kopts.Executor = coregrpc.SimulatedExecutor{}
	}
	kernelServer := coregrpc.NewKernelServer(idempMgr, budgetBreaker, policyEval, kopts)

	// 4. gRPC kernel server (loopback by default, HMAC-authenticated, optional mTLS)
	tlsCfg, err := coregrpc.LoadTLS(cfg.TLSCert, cfg.TLSKey, cfg.TLSCA, true)
	if err != nil {
		log.Fatalf("TLS configuration error: %v", err)
	}
	if cfg.InternalSecret == "" {
		log.Println("WARNING: ABOS_INTERNAL_SECRET not set; gRPC authentication disabled (dev only)")
	}
	grpcServer := grpc.NewServer(coregrpc.ServerOptions(cfg.InternalSecret, tlsCfg)...)
	kernelv1.RegisterKernelExecutionServiceServer(grpcServer, kernelServer)

	lis, err := net.Listen("tcp", cfg.GRPCAddr)
	if err != nil {
		log.Fatalf("Failed to listen for gRPC on %s: %v", cfg.GRPCAddr, err)
	}
	go func() {
		log.Printf("Kernel gRPC server listening on %s", cfg.GRPCAddr)
		if err := grpcServer.Serve(lis); err != nil {
			log.Fatalf("gRPC server failed: %v", err)
		}
	}()

	// 5. HTTP: auth + GraphQL + webhooks
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	if repo != nil {
		authSvc := auth.NewService(repo)
		authSvc.Handlers(mux)

		aiCfg, err := coregrpc.ClientConfigFromEnv()
		if err != nil {
			log.Fatalf("AI client TLS configuration error: %v", err)
		}
		aiClient, err := coregrpc.NewAIClientWithConfig(aiCfg)
		if err != nil {
			log.Fatalf("AI client: %v", err)
		}
		defer aiClient.Close()

		resolver := &gql.Resolver{
			Repo: repo, Tickets: ticketSvc, Broker: broker, Policy: policyEval, Gateway: gateway, Vault: vlt,
			AI: aiClient, AllowPrivateMCP: cfg.IsDev(),
		}
		gqlSrv := gql.NewServer(resolver, gql.ServerOptions{
			Introspection: cfg.IsDev(),
			Auth:          authSvc,
			CheckOrigin:   func(r *http.Request) bool { return originAllowed(cfg, r.Header.Get("Origin")) },
		})
		mux.Handle("/graphql", authSvc.Middleware(gqlSrv))
	}
	if cfg.IsDev() {
		mux.Handle("/", playground.Handler("ABOS GraphQL Playground (dev)", "/graphql"))
	}

	if cfg.ShopifySecret != "" {
		var store ingest.EventStore
		if rdb != nil {
			store = ingest.NewRedisEventStore(rdb, "stream:webhooks:incoming")
		}
		h, err := ingest.NewIngestService(store).Handler(ingest.SourceConfig{
			Source:      "shopify",
			Secret:      cfg.ShopifySecret,
			WorkspaceID: cfg.ShopifyWorkspaceID,
			Base64Sig:   true,
		}, "X-Shopify-Hmac-Sha256")
		if err != nil {
			log.Fatalf("webhook configuration error: %v", err)
		}
		mux.Handle("/webhooks/shopify", h)
	} else {
		log.Println("SHOPIFY_WEBHOOK_SECRET not set; /webhooks/shopify is disabled")
	}

	httpServer := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           withCORS(cfg, mux),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      60 * time.Second, // AI-backed mutations can take a while
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 16,
	}
	go func() {
		log.Printf("API server listening on http://%s", cfg.HTTPAddr)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("HTTP server failed: %v", err)
		}
	}()

	// 6. Graceful shutdown
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Println("Shutting down servers gracefully...")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(ctx)
	grpcServer.GracefulStop()
	if rdb != nil {
		_ = rdb.Close()
	}
	log.Println("ABOS Core Backend stopped cleanly.")
}

// devFallbackExecutor runs tools through the MCP gateway but, in development only, simulates
// tools whose MCP server is not registered so the whole flow can be exercised without ERPs.
type devFallbackExecutor struct{ primary *mcp.Gateway }

func (d devFallbackExecutor) Execute(ctx context.Context, workspaceID, tool, payload string) (string, error) {
	out, err := d.primary.Execute(ctx, workspaceID, tool, payload)
	if errors.Is(err, mcp.ErrNoConnection) || errors.Is(err, mcp.ErrInvalidToolID) {
		return coregrpc.SimulatedExecutor{}.Execute(ctx, workspaceID, tool, payload)
	}
	return out, err
}

// loadPolicies installs every persisted workspace policy into the kernel at startup.
func loadPolicies(ctx context.Context, repo *repository.Repository, eval *kernel.PolicyEvaluator) {
	rules, err := repo.ListEnabledPolicyRules(ctx)
	if err != nil {
		log.Printf("WARNING: could not load policies: %v", err)
		return
	}
	loaded := 0
	for _, r := range rules {
		if err := eval.AddCompiledPolicyForWorkspace(r.WorkspaceID, string(r.CompiledAST)); err != nil {
			log.Printf("WARNING: skipping invalid stored policy %s (workspace %s): %v", r.RuleKey, r.WorkspaceID, err)
			continue
		}
		loaded++
	}
	log.Printf("loaded %d workspace policies", loaded)
}

func purgeSessions(repo *repository.Repository) {
	for range time.Tick(time.Hour) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_ = repo.DeleteExpiredSessions(ctx)
		cancel()
	}
}

// budgetAlerts turns kernel budget events into GraphQL subscription events.
func budgetAlerts(repo *repository.Repository, broker *events.Broker) func(coregrpc.BudgetEvent) {
	return func(ev coregrpc.BudgetEvent) {
		if _, err := uuid.Parse(ev.AgentID); err != nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		agent, err := repo.GetAgent(ctx, ev.WorkspaceID, ev.AgentID)
		if err != nil {
			return
		}
		_ = repo.SetAgentSpent(ctx, ev.WorkspaceID, ev.AgentID, ev.Evaluation.SpentTodayUSD)
		if !ev.Evaluation.IsWarning80 && !ev.Evaluation.IsCircuitBroken {
			return
		}
		pct, _ := ev.Evaluation.SpentTodayUSD.Div(nonZero(ev.Evaluation.DailyLimitUSD)).Mul(decimal.NewFromInt(100)).Float64()
		broker.Publish(events.BudgetTopic(ev.WorkspaceID), gql.BudgetAlert{
			AgentID: agent.ID, AgentRole: agent.RoleName, SpentTodayUsd: ev.Evaluation.SpentTodayUSD.String(),
			DailyBudgetUsd: ev.Evaluation.DailyLimitUSD.String(), PercentageUsed: pct, IsHardCutoff: ev.Evaluation.IsCircuitBroken,
		})
	}
}

func nonZero(d decimal.Decimal) decimal.Decimal {
	if d.IsZero() {
		return decimal.NewFromInt(1)
	}
	return d
}

func originAllowed(cfg Config, origin string) bool {
	if origin == "" {
		return true // non-browser clients; authenticated separately
	}
	for _, o := range cfg.AllowedOrigins {
		if strings.EqualFold(o, origin) {
			return true
		}
	}
	if cfg.IsDev() && len(cfg.AllowedOrigins) == 0 {
		return strings.HasPrefix(origin, "http://localhost:") || strings.HasPrefix(origin, "http://127.0.0.1:")
	}
	return false
}

// withCORS allows browser clients from configured origins to call the API.
func withCORS(cfg Config, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if origin := r.Header.Get("Origin"); origin != "" && originAllowed(cfg, origin) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// Command mcpbus serves the tenant-scoped MCP server agents connect to.
//
//	mcpbus              run the server (POST /mcp, GET /healthz)
//	mcpbus healthcheck  exit 0 if the local server answers /healthz (container health check)
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/mrbiz/venture-os/gen/venture/agent/v1/agentv1connect"
	"github.com/mrbiz/venture-os/internal/mcpbus"
	"github.com/mrbiz/venture-os/internal/mcptoken"
	"github.com/mrbiz/venture-os/internal/ratelimit"
)

var version = "dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck())
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(log)
	if err := run(log); err != nil {
		log.Error("mcpbus stopped", "err", err)
		os.Exit(1)
	}
}

// crmIsReal mirrors the control plane's connector selection: the CRM is real when VENTURE_CONNECTOR_CRM says so, or,
// if that is unset, when the VENTURE_CONNECTORS default does. (The control plane refuses to start on invalid values, so
// anything else here simply means "not real".)
func crmIsReal() bool {
	mode := strings.ToLower(env("VENTURE_CONNECTOR_CRM", env("VENTURE_CONNECTORS", "")))
	return mode == "real"
}

func env(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

func run(log *slog.Logger) error {
	addr := env("MCPBUS_HTTP_ADDR", ":8090")
	controlplane := env("CONTROLPLANE_URL", "")
	redisURL := env("REDIS_URL", "")
	key := env("MCP_TOKEN_SIGNING_KEY", "")
	var missing []string
	for k, v := range map[string]string{"CONTROLPLANE_URL": controlplane, "REDIS_URL": redisURL, "MCP_TOKEN_SIGNING_KEY": key} {
		if v == "" {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing required environment: %s", strings.Join(missing, ", "))
	}
	perMinute, err := strconv.ParseInt(env("MCPBUS_RATE_LIMIT_PER_MIN", "120"), 10, 64)
	if err != nil || perMinute < 1 {
		return fmt.Errorf("MCPBUS_RATE_LIMIT_PER_MIN must be a positive integer")
	}
	signer, err := mcptoken.NewSigner([]byte(key))
	if err != nil {
		return err
	}
	opts, err := redis.ParseURL(redisURL)
	if err != nil {
		return fmt.Errorf("REDIS_URL: %w", err)
	}
	rdb := redis.NewClient(opts)
	defer rdb.Close()

	var origins []string
	for _, o := range strings.Split(env("MCPBUS_ALLOWED_ORIGINS", ""), ",") {
		if o = strings.TrimSpace(o); o != "" {
			origins = append(origins, o)
		}
	}

	bus := mcpbus.New(mcpbus.Server{
		Agent:          agentv1connect.NewAgentServiceClient(&http.Client{Timeout: 35 * time.Second}, controlplane),
		Signer:         signer,
		Limiter:        &ratelimit.Redis{Client: rdb, Limit: perMinute, Window: time.Minute},
		Log:            log,
		Version:        version,
		AllowedOrigins: origins,
		EnableCRM:      crmIsReal(),
		EnableCollab:   strings.EqualFold(env("VENTURE_COLLAB_TOOLS", ""), "true"),
	})
	log.Info("crm_ops tools", "enabled", crmIsReal())
	mux := http.NewServeMux()
	mux.Handle("/mcp", bus)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })

	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second, WriteTimeout: 60 * time.Second, IdleTimeout: 2 * time.Minute}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()
	log.Info("mcpbus listening", "addr", addr, "rate_limit_per_min", perMinute)
	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		return srv.Shutdown(shutdown)
	}
}

func healthcheck() int {
	_, port, err := net.SplitHostPort(env("MCPBUS_HTTP_ADDR", ":8090"))
	if err != nil {
		return 1
	}
	resp, err := (&http.Client{Timeout: 2 * time.Second}).Get("http://127.0.0.1:" + port + "/healthz")
	if err != nil {
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}

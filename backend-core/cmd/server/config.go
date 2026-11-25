package main

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// Config is read from environment variables (see deploy/env.example).
type Config struct {
	Env string // "dev" or "production" (default: production; fail-safe)

	HTTPAddr string
	GRPCAddr string

	RedisAddr   string
	RedisPass   string
	DatabaseURL string
	DBRole      string // role to SET on each connection (abos_app) so row-level security applies
	AutoMigrate bool   // apply pending migrations at startup (default on in dev, off in production)

	InternalSecret string // HMAC secret for gRPC auth
	TLSCert        string
	TLSKey         string
	TLSCA          string

	VaultKey string // base64 32-byte key encrypting MCP credentials at rest
	AIAddr   string // Python AI engine address

	AllowedOrigins []string // CORS / websocket origins

	ShopifySecret      string
	ShopifyWorkspaceID string

	DefaultAgentDailyLimit decimal.Decimal
}

func (c Config) IsDev() bool { return c.Env == "dev" }

func loadConfig() (Config, error) {
	c := Config{
		Env:            strings.ToLower(getenv("ABOS_ENV", "production")),
		HTTPAddr:       getenv("ABOS_HTTP_ADDR", "127.0.0.1:8080"),
		GRPCAddr:       getenv("ABOS_GRPC_ADDR", "127.0.0.1:50052"),
		RedisAddr:      os.Getenv("REDIS_ADDR"),
		RedisPass:      os.Getenv("REDIS_PASSWORD"),
		DatabaseURL:    os.Getenv("DATABASE_URL"),
		DBRole:         os.Getenv("ABOS_DB_ROLE"),
		InternalSecret: os.Getenv("ABOS_INTERNAL_SECRET"),
		TLSCert:        os.Getenv("ABOS_TLS_CERT"),
		TLSKey:         os.Getenv("ABOS_TLS_KEY"),
		TLSCA:          os.Getenv("ABOS_TLS_CA"),
		VaultKey:       os.Getenv("ABOS_VAULT_KEY"),
		AIAddr:         os.Getenv("ABOS_AI_ADDR"),

		ShopifySecret:      os.Getenv("SHOPIFY_WEBHOOK_SECRET"),
		ShopifyWorkspaceID: os.Getenv("SHOPIFY_WORKSPACE_ID"),
	}
	c.AutoMigrate = getenv("ABOS_AUTO_MIGRATE", map[bool]string{true: "1", false: "0"}[c.Env == "dev"]) == "1"

	for _, o := range strings.Split(os.Getenv("ABOS_ALLOWED_ORIGINS"), ",") {
		if o = strings.TrimSpace(o); o != "" {
			c.AllowedOrigins = append(c.AllowedOrigins, o)
		}
	}

	limit := getenv("ABOS_AGENT_DAILY_LIMIT_USD", "10")
	d, err := decimal.NewFromString(limit)
	if err != nil || d.IsNegative() {
		return c, fmt.Errorf("ABOS_AGENT_DAILY_LIMIT_USD is invalid: %q", limit)
	}
	c.DefaultAgentDailyLimit = d

	return c, c.validate()
}

func (c Config) validate() error {
	if c.Env != "dev" && c.Env != "production" {
		return fmt.Errorf("ABOS_ENV must be \"dev\" or \"production\", got %q", c.Env)
	}
	for _, o := range c.AllowedOrigins {
		u, err := url.Parse(o)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return fmt.Errorf("ABOS_ALLOWED_ORIGINS contains an invalid origin: %q", o)
		}
	}
	if c.ShopifySecret != "" {
		if _, err := uuid.Parse(c.ShopifyWorkspaceID); err != nil {
			return errors.New("SHOPIFY_WORKSPACE_ID must be a UUID when SHOPIFY_WEBHOOK_SECRET is set")
		}
	}
	if c.IsDev() {
		return nil
	}

	// Production requirements: nothing may silently fall back to an insecure default.
	var missing []string
	if c.RedisAddr == "" {
		missing = append(missing, "REDIS_ADDR")
	}
	if c.DatabaseURL == "" {
		missing = append(missing, "DATABASE_URL")
	}
	if c.InternalSecret == "" {
		missing = append(missing, "ABOS_INTERNAL_SECRET")
	}
	if c.VaultKey == "" {
		missing = append(missing, "ABOS_VAULT_KEY")
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing required environment variables for production: %s", strings.Join(missing, ", "))
	}
	if len(c.InternalSecret) < 32 {
		return errors.New("ABOS_INTERNAL_SECRET must be at least 32 characters")
	}
	return nil
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

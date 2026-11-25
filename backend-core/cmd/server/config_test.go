package main

import (
	"strings"
	"testing"
)

func clearEnv(t *testing.T) {
	for _, k := range []string{"ABOS_ENV", "REDIS_ADDR", "DATABASE_URL", "ABOS_INTERNAL_SECRET", "ABOS_VAULT_KEY", "ABOS_AUTO_MIGRATE",
		"SHOPIFY_WEBHOOK_SECRET", "SHOPIFY_WORKSPACE_ID", "ABOS_ALLOWED_ORIGINS", "ABOS_AGENT_DAILY_LIMIT_USD"} {
		t.Setenv(k, "")
	}
}

func TestConfig_DefaultsToProductionAndRequiresSecrets(t *testing.T) {
	clearEnv(t)
	_, err := loadConfig()
	if err == nil {
		t.Fatal("production config without required variables must fail")
	}
	for _, want := range []string{"REDIS_ADDR", "DATABASE_URL", "ABOS_INTERNAL_SECRET", "ABOS_VAULT_KEY"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %s: %v", want, err)
		}
	}
}

func TestConfig_DevNeedsNothingAndAutoMigrates(t *testing.T) {
	clearEnv(t)
	t.Setenv("ABOS_ENV", "dev")
	c, err := loadConfig()
	if err != nil {
		t.Fatalf("dev config should load without secrets: %v", err)
	}
	if !c.AutoMigrate {
		t.Error("dev auto-migrates by default")
	}
	t.Setenv("ABOS_ENV", "production")
	t.Setenv("ABOS_AUTO_MIGRATE", "")
	c, _ = loadConfig()
	if c.AutoMigrate {
		t.Error("production must not migrate unless asked")
	}
}

func TestConfig_ProductionRejectsShortSecretsAndBadWebhookWorkspace(t *testing.T) {
	clearEnv(t)
	t.Setenv("REDIS_ADDR", "127.0.0.1:6379")
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("ABOS_VAULT_KEY", "k")
	t.Setenv("ABOS_INTERNAL_SECRET", "short")
	if _, err := loadConfig(); err == nil {
		t.Error("short internal secret must be rejected in production")
	}

	t.Setenv("ABOS_INTERNAL_SECRET", strings.Repeat("b", 40))
	if _, err := loadConfig(); err != nil {
		t.Errorf("valid production config rejected: %v", err)
	}

	t.Setenv("SHOPIFY_WEBHOOK_SECRET", "x")
	t.Setenv("SHOPIFY_WORKSPACE_ID", "ws_default")
	if _, err := loadConfig(); err == nil {
		t.Error("non-UUID webhook workspace must be rejected")
	}
}

func TestOriginAllowed(t *testing.T) {
	prod := Config{Env: "production", AllowedOrigins: []string{"https://app.example.com"}}
	if !originAllowed(prod, "https://app.example.com") || originAllowed(prod, "https://evil.example") {
		t.Error("production must only allow configured origins")
	}
	if !originAllowed(prod, "") {
		t.Error("non-browser clients (no Origin) are authenticated separately")
	}
	dev := Config{Env: "dev"}
	if !originAllowed(dev, "http://localhost:3000") || originAllowed(dev, "http://evil.example") {
		t.Error("dev should allow only localhost origins by default")
	}
}

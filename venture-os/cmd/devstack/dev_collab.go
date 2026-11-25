package main

import (
	"bytes"
	"log/slog"
	"os"

	"github.com/mrbiz/venture-os/internal/collabsvc"
	"github.com/mrbiz/venture-os/internal/connectors/fakeapi"
	"github.com/mrbiz/venture-os/internal/store"
	"github.com/mrbiz/venture-os/internal/vault"
)

// Demo credentials accepted by the fake providers started with DEVSTACK_FAKE_PROVIDERS=1.
const (
	demoNotionDB = "11111111-2222-4333-8444-555555555555"
)

// devCollab enables integrations in the dev stack. The vault key is a fixed, PUBLIC dev key: the devstack must never
// be exposed to a network or used with real credentials. With DEVSTACK_FAKE_PROVIDERS=1 it also starts in-process fakes
// of Google Calendar, Zoom and Notion and points the connections at them, so the Connect screens can be tried end to
// end without real accounts. Without it, connecting goes to the real providers.
func devCollab(log *slog.Logger, st *store.PG) (*collabsvc.Registry, error) {
	v, err := vault.New(map[int][]byte{1: bytes.Repeat([]byte("devstack-key!"), 3)[:32]}, 1)
	if err != nil {
		return nil, err
	}
	reg := &collabsvc.Registry{Store: st, Vault: v}
	if os.Getenv("DEVSTACK_FAKE_PROVIDERS") != "1" {
		return reg, nil
	}
	g := fakeapi.NewGoogle("dev-client", "dev-secret", "dev-refresh")
	z := fakeapi.NewZoom("dev-account", "dev-zoom-client", "dev-zoom-secret")
	n := fakeapi.NewNotion("dev-notion-token", demoNotionDB, "Name", "Status")
	reg.Endpoints = collabsvc.Endpoints{GoogleAPI: g.URL(), GoogleOAuth: g.URL(), ZoomAPI: z.URL(), ZoomOAuth: z.URL(), NotionAPI: n.URL() + "/v1"}
	log.Warn("DEVSTACK_FAKE_PROVIDERS=1: Google, Zoom and Notion are FAKES. Demo credentials follow.")
	log.Info("fake google", "client_id", "dev-client", "client_secret", "dev-secret", "refresh_token", "dev-refresh")
	log.Info("fake zoom", "account_id", "dev-account", "client_id", "dev-zoom-client", "client_secret", "dev-zoom-secret")
	log.Info("fake notion", "token", "dev-notion-token", "database_id", demoNotionDB)
	return reg, nil
}

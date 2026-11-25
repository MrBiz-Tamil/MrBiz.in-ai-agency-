// Package connectors decides, per external system, whether the control plane talks to the real thing or to an
// in-memory fake, and builds the adapters accordingly.
//
// Selection: VENTURE_CONNECTORS is the default for every system ("real" or "fake"); VENTURE_CONNECTOR_<SYSTEM>
// overrides it for one system. Systems: IDENTITY (Keycloak), ERP (ERPNext), CRM (Venture OS's built-in CRM),
// SUPPORT (LibreDesk), RUNTIME (Hermes).
//
// Guard rails, so a partly simulated deployment can never be mistaken for a real one:
//   - Asking for "real" on a system with no real adapter is a startup error that names the system.
//   - Outside VENTURE_ENV=dev, any fake requires VENTURE_ALLOW_FAKE_CONNECTORS=true.
//   - Every simulated system is logged at WARN at startup, and Set.Simulated lists them for the UI.
package connectors

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/mrbiz/venture-os/internal/connectors/erpnext"
	"github.com/mrbiz/venture-os/internal/connectors/hermes"
	"github.com/mrbiz/venture-os/internal/connectors/keycloak"
	"github.com/mrbiz/venture-os/internal/crm"
	"github.com/mrbiz/venture-os/internal/crm/builtin"
	"github.com/mrbiz/venture-os/internal/fakes"
	"github.com/mrbiz/venture-os/internal/ports"
	"github.com/mrbiz/venture-os/internal/tenancy"
)

// System names a connector slot.
type System string

const (
	Identity System = "IDENTITY"
	ERP      System = "ERP"
	CRM      System = "CRM"
	Support  System = "SUPPORT"
	Runtime  System = "RUNTIME"
)

// All lists every system in provisioning order.
var All = []System{Identity, ERP, CRM, Support, Runtime}

// implemented says which systems have a real adapter, with the reason when one does not exist.
var notImplemented = map[System]string{
	Support: "LibreDesk is single-tenant (one set of users, teams and inboxes per instance) and inbox creation needs real channel credentials; tenant isolation needs an instance per tenant",
}

// Set is the built adapters.
type Set struct {
	Identity ports.IdentityProvider
	ERP      ports.ERP
	CRM      ports.CRM
	// CRMData is the CRM clients' entry point (what the quote flow and agent tools call). It is set only when CRM
	// is real; a simulated CRM has no data behind it.
	CRMData crm.Provider
	Support ports.Support
	Runtime ports.AgentRuntime
	// Simulated lists the systems backed by in-memory fakes.
	Simulated []System
	// Modes records the effective mode of every system.
	Modes map[System]string
}

// Deps are the shared resources real adapters need beyond environment variables.
type Deps struct {
	// DB is the application database pool. Required when CRM is real: the built-in CRM lives in it.
	DB tenancy.Beginner
	// DBOptions says how to scope transactions: empty in production (the pool connects as venture_app), AssumeRole
	// in development, where the pool connects as a superuser.
	DBOptions tenancy.Options
}

// Build constructs the adapters from environment-style configuration, without a database. A real CRM needs one, so
// use BuildWith when the control plane has a pool.
func Build(get func(string) string, log *slog.Logger) (Set, error) {
	return BuildWith(get, log, Deps{})
}

// BuildWith constructs the adapters from environment-style configuration. get returns "" for unset variables.
func BuildWith(get func(string) string, log *slog.Logger, deps Deps) (Set, error) {
	if log == nil {
		log = slog.Default()
	}
	base := strings.ToLower(strings.TrimSpace(get("VENTURE_CONNECTORS")))
	if base != "real" && base != "fake" {
		return Set{}, fmt.Errorf("VENTURE_CONNECTORS must be \"fake\" or \"real\", got %q", base)
	}
	modes := map[System]string{}
	var problems []string
	for _, s := range All {
		m := strings.ToLower(strings.TrimSpace(get("VENTURE_CONNECTOR_" + string(s))))
		if m == "" {
			m = base
		}
		if m != "real" && m != "fake" {
			problems = append(problems, fmt.Sprintf("VENTURE_CONNECTOR_%s must be \"fake\" or \"real\", got %q", s, m))
			continue
		}
		if why, missing := notImplemented[s]; m == "real" && missing {
			problems = append(problems, fmt.Sprintf("%s has no real adapter yet (%s): set VENTURE_CONNECTOR_%s=fake to simulate it explicitly", s, why, s))
		}
		modes[s] = m
	}
	if len(problems) > 0 {
		return Set{}, errors.New(strings.Join(problems, "\n"))
	}

	set := Set{Modes: modes}
	for _, s := range All {
		if modes[s] == "fake" {
			set.Simulated = append(set.Simulated, s)
		}
	}
	if len(set.Simulated) > 0 && get("VENTURE_ENV") != "dev" && strings.ToLower(get("VENTURE_ALLOW_FAKE_CONNECTORS")) != "true" {
		return Set{}, fmt.Errorf("systems %v are set to fake, which simulates them without creating anything real; "+
			"set VENTURE_ALLOW_FAKE_CONNECTORS=true to run that way deliberately", set.Simulated)
	}

	hub := fakes.NewHub()
	var missing []string
	need := func(keys ...string) map[string]string {
		out := map[string]string{}
		for _, k := range keys {
			out[k] = strings.TrimSpace(get(k))
			if out[k] == "" {
				missing = append(missing, k)
			}
		}
		return out
	}

	if modes[Identity] == "real" {
		e := need("KEYCLOAK_ADMIN_URL", "KEYCLOAK_SERVICE_CLIENT_ID", "KEYCLOAK_SERVICE_CLIENT_SECRET")
		realm := get("KEYCLOAK_REALM")
		if realm == "" {
			realm = "venture"
		}
		set.Identity = &keycloak.Client{BaseURL: e["KEYCLOAK_ADMIN_URL"], Realm: realm, ClientID: e["KEYCLOAK_SERVICE_CLIENT_ID"], ClientSecret: e["KEYCLOAK_SERVICE_CLIENT_SECRET"]}
	} else {
		set.Identity = fakes.NewIdentity(hub)
	}
	if modes[ERP] == "real" {
		e := need("ERP_URL", "ERP_SITE_NAME", "ERP_API_KEY", "ERP_API_SECRET")
		set.ERP = &erpnext.Client{BaseURL: e["ERP_URL"], Site: e["ERP_SITE_NAME"], APIKey: e["ERP_API_KEY"], APISecret: e["ERP_API_SECRET"]}
	} else {
		set.ERP = fakes.NewERP(hub)
	}
	if modes[Runtime] == "real" {
		e := need("HERMES_DATA_DIR", "MCPBUS_PUBLIC_URL")
		set.Runtime = &hermes.Runtime{Root: e["HERMES_DATA_DIR"], MCPURL: e["MCPBUS_PUBLIC_URL"]}
	} else {
		set.Runtime = fakes.NewRuntime(hub)
	}
	if modes[CRM] == "real" {
		if deps.DB == nil {
			return Set{}, errors.New("CRM is real (the built-in CRM) but no database was given to the connector builder")
		}
		p := builtin.New(deps.DB, deps.DBOptions)
		set.CRM, set.CRMData = builtin.NewWorkspace(p), p
	} else {
		set.CRM = fakes.NewCRM(hub)
	}
	// No real adapter exists for this yet; Build has already refused "real".
	set.Support = fakes.NewSupport(hub)

	if len(missing) > 0 {
		return Set{}, fmt.Errorf("real connectors need these environment variables: %s", strings.Join(missing, ", "))
	}
	for _, s := range set.Simulated {
		log.Warn("connector is SIMULATED: nothing is created in the real system", "system", string(s))
	}
	for _, s := range All {
		if modes[s] == "real" {
			log.Info("connector is REAL", "system", string(s))
		}
	}
	return set, nil
}

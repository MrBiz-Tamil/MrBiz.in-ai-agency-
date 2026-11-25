package connectors_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/mrbiz/venture-os/internal/connectors"
	"github.com/mrbiz/venture-os/internal/connectors/erpnext"
	"github.com/mrbiz/venture-os/internal/connectors/hermes"
	"github.com/mrbiz/venture-os/internal/connectors/keycloak"
	"github.com/mrbiz/venture-os/internal/crm/builtin"
	"github.com/mrbiz/venture-os/internal/fakes"
	"github.com/mrbiz/venture-os/internal/tenancy"
)

// noDB satisfies tenancy.Beginner; the builder only stores it, so it is never asked to begin anything here.
type noDB struct{}

func (noDB) BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error) {
	return nil, errors.New("no database in this test")
}

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func env(kv ...string) func(string) string {
	m := map[string]string{}
	for i := 0; i < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return func(k string) string { return m[k] }
}

var realCreds = []string{
	"KEYCLOAK_ADMIN_URL", "http://keycloak:8080", "KEYCLOAK_SERVICE_CLIENT_ID", "venture-service", "KEYCLOAK_SERVICE_CLIENT_SECRET", "s",
	"ERP_URL", "http://erp:8080", "ERP_SITE_NAME", "erp.localhost", "ERP_API_KEY", "k", "ERP_API_SECRET", "s",
	"HERMES_DATA_DIR", "/hermes-data", "MCPBUS_PUBLIC_URL", "http://mcpbus:8090/mcp",
}

func TestAllFakeInDevWorks(t *testing.T) {
	set, err := connectors.Build(env("VENTURE_CONNECTORS", "fake", "VENTURE_ENV", "dev"), quiet)
	if err != nil {
		t.Fatal(err)
	}
	if len(set.Simulated) != 5 {
		t.Fatalf("simulated = %v", set.Simulated)
	}
	if _, ok := set.ERP.(*fakes.ERP); !ok {
		t.Fatalf("ERP is %T", set.ERP)
	}
}

func TestFakesNeedExplicitPermissionOutsideDev(t *testing.T) {
	_, err := connectors.Build(env("VENTURE_CONNECTORS", "fake"), quiet)
	if err == nil || !strings.Contains(err.Error(), "VENTURE_ALLOW_FAKE_CONNECTORS") {
		t.Fatalf("production with fakes and no consent: %v", err)
	}
	if _, err := connectors.Build(env("VENTURE_CONNECTORS", "fake", "VENTURE_ALLOW_FAKE_CONNECTORS", "true"), quiet); err != nil {
		t.Fatalf("explicit consent: %v", err)
	}
}

func TestRealIsRefusedForSystemsWithoutAnAdapterAndNamesThem(t *testing.T) {
	_, err := connectors.Build(env(append([]string{"VENTURE_CONNECTORS", "real"}, realCreds...)...), quiet)
	if err == nil {
		t.Fatal("VENTURE_CONNECTORS=real must not silently simulate SUPPORT")
	}
	for _, sys := range []string{"SUPPORT"} {
		if !strings.Contains(err.Error(), sys+" has no real adapter") {
			t.Errorf("error does not name %s:\n%v", sys, err)
		}
	}
	for _, sys := range []string{"IDENTITY", "ERP", "CRM", "RUNTIME"} {
		if strings.Contains(err.Error(), sys+" has no real adapter") {
			t.Errorf("%s has a real adapter but was refused", sys)
		}
	}
}

func TestMixedRealAndFakeBuildsRealAdaptersWhereAvailable(t *testing.T) {
	kv := append([]string{
		"VENTURE_CONNECTORS", "real", "VENTURE_ALLOW_FAKE_CONNECTORS", "true",
		"VENTURE_CONNECTOR_SUPPORT", "fake",
	}, realCreds...)
	set, err := connectors.BuildWith(env(kv...), quiet, connectors.Deps{DB: noDB{}})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := set.CRM.(*builtin.Workspace); !ok || set.CRMData == nil {
		t.Errorf("a real CRM is the built-in one, with a data provider: %T / %v", set.CRM, set.CRMData)
	}
	if _, ok := set.Identity.(*keycloak.Client); !ok {
		t.Errorf("identity is %T", set.Identity)
	}
	if _, ok := set.ERP.(*erpnext.Client); !ok {
		t.Errorf("erp is %T", set.ERP)
	}
	if _, ok := set.Runtime.(*hermes.Runtime); !ok {
		t.Errorf("runtime is %T", set.Runtime)
	}
	want := map[connectors.System]bool{connectors.Support: true}
	if len(set.Simulated) != 1 {
		t.Fatalf("simulated = %v", set.Simulated)
	}
	for _, s := range set.Simulated {
		if !want[s] {
			t.Errorf("unexpected simulated system %s", s)
		}
	}
	if set.Modes[connectors.ERP] != "real" || set.Modes[connectors.CRM] != "real" || set.Modes[connectors.Support] != "fake" {
		t.Errorf("modes = %v", set.Modes)
	}
}

func TestPerSystemOverrideBeatsTheDefault(t *testing.T) {
	set, err := connectors.Build(env("VENTURE_CONNECTORS", "fake", "VENTURE_ENV", "dev", "VENTURE_CONNECTOR_RUNTIME", "real",
		"HERMES_DATA_DIR", "/hermes-data", "MCPBUS_PUBLIC_URL", "http://mcpbus:8090/mcp"), quiet)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := set.Runtime.(*hermes.Runtime); !ok {
		t.Fatalf("runtime is %T", set.Runtime)
	}
	if len(set.Simulated) != 4 {
		t.Fatalf("simulated = %v", set.Simulated)
	}
}

func TestMissingRealConfigurationIsListed(t *testing.T) {
	_, err := connectors.Build(env("VENTURE_CONNECTORS", "fake", "VENTURE_ENV", "dev",
		"VENTURE_CONNECTOR_IDENTITY", "real", "VENTURE_CONNECTOR_ERP", "real"), quiet)
	if err == nil {
		t.Fatal("real connectors without configuration accepted")
	}
	for _, k := range []string{"KEYCLOAK_ADMIN_URL", "KEYCLOAK_SERVICE_CLIENT_SECRET", "ERP_API_KEY", "ERP_API_SECRET", "ERP_SITE_NAME"} {
		if !strings.Contains(err.Error(), k) {
			t.Errorf("error does not list %s: %v", k, err)
		}
	}
}

func TestInvalidModesAreRejected(t *testing.T) {
	for _, base := range []string{"", "maybe", "REAL!"} {
		if _, err := connectors.Build(env("VENTURE_CONNECTORS", base, "VENTURE_ENV", "dev"), quiet); err == nil {
			t.Errorf("base %q accepted", base)
		}
	}
	if _, err := connectors.Build(env("VENTURE_CONNECTORS", "fake", "VENTURE_ENV", "dev", "VENTURE_CONNECTOR_ERP", "yes"), quiet); err == nil {
		t.Error("bad override accepted")
	}
}

func TestRealCRMNeedsADatabase(t *testing.T) {
	_, err := connectors.Build(env("VENTURE_CONNECTORS", "fake", "VENTURE_ENV", "dev", "VENTURE_CONNECTOR_CRM", "real"), quiet)
	if err == nil || !strings.Contains(err.Error(), "CRM") || !strings.Contains(err.Error(), "database") {
		t.Fatalf("a real CRM without a database must say so: %v", err)
	}
	set, err := connectors.BuildWith(env("VENTURE_CONNECTORS", "fake", "VENTURE_ENV", "dev", "VENTURE_CONNECTOR_CRM", "real"), quiet,
		connectors.Deps{DB: noDB{}, DBOptions: tenancy.Options{AssumeRole: "venture_app"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(set.Simulated) != 4 || set.Modes[connectors.CRM] != "real" {
		t.Fatalf("simulated = %v, modes = %v", set.Simulated, set.Modes)
	}
}

func TestSimulatedCRMHasNoDataProvider(t *testing.T) {
	set, err := connectors.Build(env("VENTURE_CONNECTORS", "fake", "VENTURE_ENV", "dev"), quiet)
	if err != nil {
		t.Fatal(err)
	}
	if set.CRMData != nil {
		t.Fatal("a simulated CRM must not pretend to have data behind it")
	}
}

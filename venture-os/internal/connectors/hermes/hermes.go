// Package hermes implements ports.AgentRuntime by writing Hermes profiles to the shared Hermes data volume.
//
// Hermes keeps each profile in its own home directory, $HERMES_HOME/profiles/<name>/, holding config.yaml, .env,
// SOUL.md and skills/<category>/<skill>/SKILL.md (see Hermes' profiles and skills documentation). A running Hermes
// gateway discovers new profiles and reads skills from disk, so "provisioning" is deterministic file placement:
//
//	profiles/venture-<slug>/.venture-managed   tenant id: the only files this adapter will ever modify or delete
//	profiles/venture-<slug>/config.yaml        MCP client pointing at the Venture OS bus; token by ${VENTURE_MCP_TOKEN}
//	profiles/venture-<slug>/.env               VENTURE_MCP_TOKEN=<tenant token>  (mode 0600)
//	profiles/venture-<slug>/SOUL.md            persona
//	profiles/venture-<slug>/skills/venture/<skill>/SKILL.md
//
// Every operation goes through os.Root, so a malicious slug or skill name can never escape the data directory.
// Files are written atomically (temp file + rename). The directory must be writable by the control plane and
// readable by the Hermes user: run both with the same uid (HERMES_UID in docker-compose.yml).
package hermes

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/google/uuid"

	"github.com/mrbiz/venture-os/internal/ports"
)

const (
	profilePrefix = "venture-"
	skillCategory = "venture"
	markerFile    = ".venture-managed"
)

var (
	slugRe      = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,38}[a-z0-9]$`)
	skillNameRe = regexp.MustCompile(`^[a-z][a-z0-9-]{1,62}$`)
)

// Runtime writes Hermes profiles under Root (the Hermes data directory, i.e. HERMES_HOME).
type Runtime struct {
	// Root is HERMES_HOME as seen by this process (for example /hermes-data).
	Root string
	// MCPURL is the MCP bus URL Hermes should connect to, as reachable from the Hermes container.
	MCPURL string
}

var _ ports.AgentRuntime = (*Runtime)(nil)

func (r *Runtime) validate() error {
	if r.Root == "" {
		return errors.New("hermes: Root (HERMES_DATA_DIR) is required")
	}
	if r.MCPURL == "" {
		return errors.New("hermes: MCPURL is required")
	}
	return nil
}

func profileName(t ports.TenantInfo) (string, error) {
	if !slugRe.MatchString(t.Slug) {
		return "", fmt.Errorf("hermes: tenant slug %q is not a valid profile name", t.Slug)
	}
	return profilePrefix + t.Slug, nil
}

// EnsureProfile creates (or refreshes) the tenant's profile and binds it to mcpToken. It is idempotent; calling it
// with a new token rotates the credential in place. A profile directory owned by a different tenant is refused.
func (r *Runtime) EnsureProfile(ctx context.Context, t ports.TenantInfo, mcpToken string) (ports.Ref, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := r.validate(); err != nil {
		return "", err
	}
	if t.ID == uuid.Nil {
		return "", errors.New("hermes: tenant id is required")
	}
	if strings.TrimSpace(mcpToken) == "" || strings.ContainsAny(mcpToken, "\r\n\x00") {
		return "", errors.New("hermes: a single-line MCP token is required")
	}
	name, err := profileName(t)
	if err != nil {
		return "", err
	}
	root, err := r.openRoot()
	if err != nil {
		return "", err
	}
	defer root.Close()

	dir := path.Join("profiles", name)
	if owner, err := readMarker(root, dir); err == nil && owner != t.ID.String() {
		return "", fmt.Errorf("hermes: profile %q belongs to another tenant", name)
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("hermes: read profile marker: %w", err)
	}
	if err := root.MkdirAll(path.Join(dir, "skills", skillCategory), 0o755); err != nil {
		return "", fmt.Errorf("hermes: create profile: %w", err)
	}
	files := []struct {
		name string
		body []byte
		mode fs.FileMode
	}{
		{markerFile, []byte(t.ID.String() + "\n"), 0o644},
		{"config.yaml", r.configYAML(), 0o644},
		{".env", []byte("VENTURE_MCP_TOKEN=" + mcpToken + "\n"), 0o600},
		{"SOUL.md", soul(t), 0o644},
	}
	for _, f := range files {
		if err := writeAtomic(root, path.Join(dir, f.name), f.body, f.mode); err != nil {
			return "", fmt.Errorf("hermes: write %s: %w", f.name, err)
		}
	}
	return ports.Ref(name), nil
}

// InstallSkills makes the profile's venture skills exactly the given set: new and changed skills are written,
// skills no longer in the set are removed. Each skill is verified against its SHA-256 first, so only content the
// founder approved can be installed.
func (r *Runtime) InstallSkills(ctx context.Context, t ports.TenantInfo, profile ports.Ref, skills []ports.Skill) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := r.validate(); err != nil {
		return err
	}
	name, err := r.ownedProfile(t, profile)
	if err != nil {
		return err
	}
	root, err := r.openRoot()
	if err != nil {
		return err
	}
	defer root.Close()
	if err := r.checkOwner(root, name, t); err != nil {
		return err
	}

	want := map[string]bool{}
	for _, s := range skills {
		skill := skillDir(s.AgentKey)
		if !skillNameRe.MatchString(skill) {
			return fmt.Errorf("hermes: %q is not a valid skill name", skill)
		}
		if sha256.Sum256([]byte(s.Markdown)) != s.SHA256 {
			return fmt.Errorf("hermes: skill %q does not match its approved hash", s.AgentKey)
		}
		want[skill] = true
	}
	base := path.Join("profiles", name, "skills", skillCategory)
	if err := root.MkdirAll(base, 0o755); err != nil {
		return err
	}
	for _, s := range skills {
		dir := path.Join(base, skillDir(s.AgentKey))
		if err := root.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		if err := writeAtomic(root, path.Join(dir, "SKILL.md"), []byte(s.Markdown), 0o644); err != nil {
			return fmt.Errorf("hermes: install skill %q: %w", s.AgentKey, err)
		}
	}
	// Reconcile: remove skills the blueprint no longer defines.
	sub, err := root.OpenRoot(base)
	if err != nil {
		return err
	}
	defer sub.Close()
	entries, err := fs.ReadDir(sub.FS(), ".")
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() && !want[e.Name()] {
			if err := sub.RemoveAll(e.Name()); err != nil {
				return fmt.Errorf("hermes: remove stale skill %q: %w", e.Name(), err)
			}
		}
	}
	return nil
}

// RemoveProfile deletes the tenant's profile. It refuses to delete a directory without this tenant's marker, and
// succeeds if the profile is already gone.
func (r *Runtime) RemoveProfile(ctx context.Context, t ports.TenantInfo, ref ports.Ref) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := r.validate(); err != nil {
		return err
	}
	name, err := r.ownedProfile(t, ref)
	if err != nil {
		return err
	}
	root, err := r.openRoot()
	if err != nil {
		return err
	}
	defer root.Close()
	dir := path.Join("profiles", name)
	owner, err := readMarker(root, dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if _, serr := root.Lstat(dir); serr == nil {
			return fmt.Errorf("hermes: refusing to delete %q: it was not created by Venture OS", name)
		}
		return nil // already gone
	case err != nil:
		return err
	case owner != t.ID.String():
		return fmt.Errorf("hermes: refusing to delete %q: it belongs to another tenant", name)
	}
	return root.RemoveAll(dir)
}

// ownedProfile resolves the profile name for t and checks a supplied ref (if any) agrees with it.
func (r *Runtime) ownedProfile(t ports.TenantInfo, ref ports.Ref) (string, error) {
	name, err := profileName(t)
	if err != nil {
		return "", err
	}
	if ref != "" && string(ref) != name {
		return "", fmt.Errorf("hermes: profile ref %q does not belong to tenant %s", ref, t.ID)
	}
	return name, nil
}

func (r *Runtime) checkOwner(root *os.Root, name string, t ports.TenantInfo) error {
	owner, err := readMarker(root, path.Join("profiles", name))
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("hermes: profile %q does not exist: ensure it first", name)
	}
	if err != nil {
		return err
	}
	if owner != t.ID.String() {
		return fmt.Errorf("hermes: profile %q belongs to another tenant", name)
	}
	return nil
}

func (r *Runtime) openRoot() (*os.Root, error) {
	if err := os.MkdirAll(r.Root, 0o755); err != nil {
		return nil, fmt.Errorf("hermes: data directory: %w", err)
	}
	root, err := os.OpenRoot(r.Root)
	if err != nil {
		return nil, fmt.Errorf("hermes: open data directory: %w", err)
	}
	return root, nil
}

func readMarker(root *os.Root, dir string) (string, error) {
	b, err := root.ReadFile(path.Join(dir, markerFile))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

func skillDir(agentKey string) string { return strings.ReplaceAll(agentKey, "_", "-") }

// writeAtomic writes via a temp file in the same directory and renames it into place, so a reader (the Hermes
// gateway) never sees a half-written file.
func writeAtomic(root *os.Root, name string, body []byte, mode fs.FileMode) error {
	var rnd [6]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return err
	}
	tmp := name + ".tmp-" + hex.EncodeToString(rnd[:])
	if err := root.WriteFile(tmp, body, mode); err != nil {
		return err
	}
	if err := root.Chmod(tmp, mode); err != nil { // WriteFile honours umask; the mode is a security property here
		_ = root.Remove(tmp)
		return err
	}
	if err := root.Rename(tmp, name); err != nil {
		_ = root.Remove(tmp)
		return err
	}
	return nil
}

func (r *Runtime) configYAML() []byte {
	url, _ := json.Marshal(r.MCPURL) // a JSON string is a valid YAML double-quoted scalar
	var b strings.Builder
	b.WriteString("# Managed by Venture OS. Changes are overwritten when the business is re-provisioned.\n")
	b.WriteString("# The MCP bus derives the tenant from the token in .env; no tool takes a tenant argument.\n")
	b.WriteString("mcp_servers:\n  venture:\n")
	b.WriteString("    url: " + string(url) + "\n")
	b.WriteString("    headers:\n      Authorization: \"Bearer ${VENTURE_MCP_TOKEN}\"\n")
	return []byte(b.String())
}

func soul(t ports.TenantInfo) []byte {
	return []byte(fmt.Sprintf(`# %s

You are part of the AI team of %s, run through Venture OS.

- You act only through the Venture OS tools you have been given. Their results are the only source of prices, margins,
  taxes, balances and forecasts: never calculate or estimate money yourself.
- Anything outward-facing or irreversible (quotes, invoices, purchase orders, payments, campaigns) is queued for the
  founder's approval. A pending approval is not a result.
- When blocked or unsure, escalate instead of guessing.
`, t.Name, t.Name))
}

// Installed lists the skill directories currently present for a tenant (for diagnostics and tests).
func (r *Runtime) Installed(t ports.TenantInfo) ([]string, error) {
	name, err := profileName(t)
	if err != nil {
		return nil, err
	}
	root, err := r.openRoot()
	if err != nil {
		return nil, err
	}
	defer root.Close()
	sub, err := root.OpenRoot(path.Join("profiles", name, "skills", skillCategory))
	if err != nil {
		return nil, err
	}
	defer sub.Close()
	entries, err := fs.ReadDir(sub.FS(), ".")
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}

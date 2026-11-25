# Design: the Twenty (CRM) and LibreDesk (support) connectors

> **Update:** the CRM decision changed. Venture OS now ships its own CRM behind a `crm.CRM` interface
> ([ARCHITECTURE.md section 10](ARCHITECTURE.md)), so **Twenty is optional**: an adapter to write if a customer wants it as
> their CRM, not something the default provisioning path needs. Section 1 below still describes how that adapter would
> work (and the spikes in section 4 would still come first); section 2 (LibreDesk) is unaffected.

Status: design only. Nothing here is implemented. It records what the upstream source shows, what that forces, a
recommended design for each, the decisions only you can make, and an ordered list of spikes that turn unverified
assumptions into facts before any adapter code is written.

"Verified" below means I read it in the upstream repository's source or docs. "Unverified" means it needs a spike
against a running instance. Versions pinned in this repo: Twenty `v2.62.0`, LibreDesk `v2.9.2`.

---

## 0. The shared problem

`ports.CRM` and `ports.Support` were written assuming "a tenant is a thing you create inside the product". That is
true for ERPNext (Company), Keycloak (Organization) and Hermes (profile). It is **not** true for these two:

| | Tenant model in the product | Consequence |
| --- | --- | --- |
| Twenty | Real multi-tenancy: a *workspace* per tenant, data in a per-workspace Postgres schema. But workspaces are created by an authenticated **user**, through the sign-up flow, on a **subdomain**. | Possible, but needs multi-workspace mode, wildcard hostnames, a service user, and token plumbing. |
| LibreDesk | **None.** One set of users, teams, contacts, inboxes and help centers per instance. | Sound isolation means **one instance per tenant**, which is an infrastructure decision, not an API call. |

Cross-cutting needs both designs share (and which don't exist yet):

1. **A secret store.** Twenty gives us an API key per workspace; LibreDesk an API key per instance. Today
   `tenant_integrations.secret_ref` points at nothing. Proposed: a `tenant_secrets` table with the value sealed
   AES-256-GCM under a key from the environment (the same approach as the web session cookie), readable only by the
   control plane's runtime role, with `Put/Get/Delete` on a small `ports.SecretStore`. Rotation = Put a new version.
2. **Port changes** (small, but they touch fakes and the provisioner):
   * `Support.EnsureInboxes` must be able to say "created", "needs your credentials" or "unsupported" **per channel**
     (see §2.4), so it returns `(Ref, []ChannelState)` instead of a single `Ref`.
   * Both adapters need to hand back non-secret connection details (URL, subdomain). Proposed: `Ref` stays opaque, and
     `RecordIntegrations` also stores `config jsonb` (it already has the column).
3. **A contract test suite**, `internal/portstest`, run against the fake *and* each real adapter: idempotent ensure,
   idempotent remove, tenant isolation, ref misuse refused. Today the fakes are tested separately, so a real adapter
   could pass its own tests and still violate the contract. This is the cheapest way to keep the fakes honest.

---

## 1. Twenty (CRM)

### 1.1 What the source shows

Verified (`packages/twenty-server`, `main`):

* `signUpInNewWorkspace(input: {displayName, subdomain})` exists in `auth.resolver.ts`. It requires an **authenticated
  existing user** (`@AuthUser()`), creates the workspace, and returns `{loginToken, workspace{id, workspaceUrls}}`.
* `getAuthTokensFromLoginToken(loginToken, origin)` turns that login token into tokens for the new workspace. The
  `origin` must be the workspace's own URL (it is validated against the workspace's domain).
* `createApiKey(input: {name, expiresAt, roleId})` is a **metadata** GraphQL mutation. It requires an authenticated
  workspace session with permission to assign roles, and `expiresAt` is **required**: keys expire, so there must be
  a rotation job.
* `createOneObject(input: {nameSingular, namePlural, labelSingular, labelPlural, description, icon, ...})` exists on
  the metadata API.
* Config flags exist: `IS_MULTIWORKSPACE_ENABLED`, `IS_WORKSPACE_CREATION_LIMITED_TO_SERVER_ADMINS`. The shipped
  compose file does not set the first, so we are currently in **single-workspace mode**.
* `signUpWithoutWorkspace` (email + password) creates a user with no workspace.

Unverified (spike needed): the exact `createOneField` input (SELECT options), how a **Kanban view** is created over
a custom object, the workspace **deletion** mutation and whether deletion is soft, how workspace **SSO** with
Keycloak is configured, and whether `getAuthTokensFromLoginToken` accepts server-to-server calls without a browser
origin.

### 1.2 Recommended design

**Deployment changes**
* `IS_MULTIWORKSPACE_ENABLED=true`, `DEFAULT_SUBDOMAIN`, `FRONTEND_URL`/`SERVER_URL` on a base domain, and
  `IS_WORKSPACE_CREATION_LIMITED_TO_SERVER_ADMINS=true` so only the service user can create workspaces.
* Caddy serves a wildcard: `*.crm.<BASE_DOMAIN>` to `twenty-server:3000`. Locally `*.crm.localhost` works over HTTP.
  **In production a wildcard certificate needs a DNS-01 challenge** (a Caddy DNS plugin and API credentials for your DNS
  provider). This is the one infrastructure cost the other connectors do not have.

**Service identity (one-time bootstrap, `scripts/twenty-bootstrap`)**: create a dedicated user
`venture-provisioner@<domain>` via `signUpWithoutWorkspace`; store its credentials in the environment.

**`EnsureWorkspace`**
1. `signIn` as the service user, giving a workspace-agnostic access token.
2. If `tenant_integrations` already holds a workspace for this tenant, verify it still exists and return it.
3. `signUpInNewWorkspace(displayName=Business.Name, subdomain=tenant slug)`; on a subdomain clash, append a short
   tenant-id suffix (same disambiguation rule as ERPNext).
4. `getAuthTokensFromLoginToken(loginToken, origin=https://<slug>.crm.<domain>)` for workspace-scoped tokens.
5. `createApiKey(name="venture-os", expiresAt=now+90d, roleId=<least-privilege role>)`; seal it in the secret store.
6. Ref = workspace id; `config` = `{subdomain, url}`; `secret_ref` = the sealed key.

**`ApplyPipelines`** (blueprint pipeline to Twenty): create **one custom object per pipeline**
(`nameSingular` = camelCase of the blueprint key, checked against reserved names) with a `stage` SELECT field whose
options are the stages (in order), plus one field per blueprint custom field (`TEXT/NUMBER/DATE/SELECT/BOOLEAN` map
1:1 to Twenty field types). Re-applying lists existing objects and fields and only creates or updates what differs;
stage options are replaced as a set. Using Twenty's built-in `opportunity` object was considered and rejected: it
supports a single pipeline and would couple us to its default schema.

**`RemoveWorkspace`**: revoke the API key, delete the workspace (mutation to be confirmed), delete the secret.
Because deletion semantics are unconfirmed, the first implementation should **refuse to delete a workspace that has
any records** and return an error the provisioner reports, as the ERPNext adapter does.

**Founder access**: the service user owns the workspace, so the founder must be added. Two options, to be chosen
(see §3): invite by email (needs SMTP configured on Twenty) or configure workspace **OIDC SSO** against the existing
Keycloak realm so the founder signs in with the same identity everywhere (better, but its API is unverified).

**`crm_ops` tools** (later): the control plane calls Twenty with the workspace API key; agents never see it.

### 1.3 Risks
* Tokens/keys expire: needs a rotation job and an alert when rotation fails.
* Subdomain routing couples CRM availability to wildcard DNS/TLS.
* A single Twenty Postgres schema-per-workspace design means one noisy tenant shares the database with others (the
  isolation is logical, which is acceptable here but worth stating).

---

## 2. LibreDesk (support)

### 2.1 What the source shows

Verified (`cmd/handlers.go`, `internal/inbox`, repo `main`):

* There is **no tenant concept**. Users ("agents"), teams, contacts, inboxes and help centers are global to an
  instance. Access is controlled by role permission strings (`conversations:read_team_inbox`, `inboxes:manage`,
  `help_center:manage`, ...) and team membership, not by tenant.
* API keys exist (`authMethodAPIKey`; `POST /api/v1/agents/{id}/api-key`).
* Inboxes (`/api/v1/inboxes`) have a `channel` of **email**, **livechat** or **whatsapp**, each with its own config.
  Email needs IMAP/SMTP (or OAuth) credentials; WhatsApp needs Meta credentials; livechat needs none.
* Help centers, collections and articles have a full admin API plus public per-slug endpoints
  (`/api/v1/public/help-centers/{slug}/...`). Multiple help centers per instance are supported.
* The upstream compose runs `--install --idempotent-install --yes`, so unattended bootstrap works.

Unverified: the exact `Authorization` header format for API keys, the inbox-create payloads (especially livechat
config), and the install behaviour when many instances share one Postgres server.

### 2.2 Options considered

| Option | Isolation | Cost | Verdict |
| --- | --- | --- | --- |
| **A. One shared instance**, separate by teams, roles and per-tenant inboxes | Weak: contacts are shared, admin endpoints are global, search can cross inboxes, one compromised agent key is a cross-tenant breach. | Cheapest. | **Rejected** for anything but a single-founder demo. |
| **B. One instance per tenant** (own database, own container, hostname `support-<slug>.<domain>`) | Strong: a tenant's data and credentials live in a database no other tenant's container can reach. | Needs infrastructure to start containers (see 2.3). ~100-200 MB RAM per idle instance. | **Recommended.** |
| **C. Swap LibreDesk for a natively multi-tenant helpdesk** (Chatwoot has "accounts" and a platform API for creating them) | Strong, built in. | A product decision; heavier stack (Rails, Sidekiq); I have not evaluated its API in this repo. | Worth a half-day evaluation **if B's infrastructure cost is unacceptable.** |

### 2.3 Recommended design (option B)

**How instances come to exist** is the real decision. Candidates, in order of how I would take them:

1. **A pool.** Compose starts `N` LibreDesk instances (`libredesk-1..N`), each with its own database, and the
   provisioner *assigns* one to a tenant (a row in a `support_pool` table; ref = instance id). No container control
   needed, no Docker socket, trivially testable. Limit: capacity is whatever you pre-started.
2. **Docker API through a socket proxy** (e.g. a proxy allowing only create/start/stop of containers carrying a
   `venture.tenant` label). Unbounded capacity, but the control plane gains a powerful capability; needs careful
   scoping.
3. **Kubernetes/Nomad later.** Out of scope for the compose deployment.

Start with (1); the adapter interface does not change when (2) replaces it.

**Per-instance bootstrap** (`EnsureInboxes`):
1. Assign an instance (pool) and record its URL and an admin API key (sealed, secret store).
2. Create a **team** named after each distinct `route_to_team` in the blueprint.
3. Create inboxes per blueprint channel:
   * `CHAT` -> a livechat inbox (no external credentials): **real**.
   * `EMAIL`, `WHATSAPP` -> **cannot be created without the founder's credentials**. They are returned as
     `NEEDS_CREDENTIALS` in `[]ChannelState`; the Command Center shows "Connect your email" / "Connect WhatsApp"
     tasks on the business page, and a later flow (`ConnectChannel`) completes them. This is honest, where the
     current fake pretends all three exist.
4. `SeedKnowledgeBase`: create one help center (slug = tenant slug), a "General" collection, and one published
   article per blueprint KB document. Idempotent by article slug.
5. `RemoveInboxes`: return the instance to the pool **after wiping it** (drop and re-install its database), or
   refuse if conversations exist (mirrors the ERPNext rule).

**Events (later):** LibreDesk webhooks to the control plane feed `support_ops` (inbound RFQs).

### 2.4 Port change this forces
`EnsureInboxes(ctx, t, channels) (Ref, []ChannelState, error)` where `ChannelState{Name, Kind, Status:
READY|NEEDS_CREDENTIALS|UNSUPPORTED, Detail}`. The provisioner persists the states; a tenant is ACTIVE with
`NEEDS_CREDENTIALS` channels (they are follow-ups, not failures).

### 2.5 Risks
* Pool exhaustion: needs a visible capacity metric and a clear "no capacity" provisioning error.
* Wiping and reusing an instance must be provably complete before it is handed to another tenant (a data-leak risk
  that does not exist in the other connectors).

---

## 3. Decisions I need from you

1. **LibreDesk isolation** (the big one): B with a pool *(recommended)*, B with Docker API access, switch to a
   natively multi-tenant helpdesk (C), or accept weak isolation (A) for now?
2. **Wildcard DNS/TLS for Twenty:** are you willing to run a wildcard hostname (`*.crm.<domain>`) with DNS-01
   certificates in production? If not, Twenty must stay single-workspace and the CRM tenancy story changes.
3. **Founder access to Twenty:** SSO against Keycloak *(recommended, if the spike confirms it)* or email invites?
4. **Email/WhatsApp channels:** accept "needs your credentials" follow-up tasks instead of auto-creating them?
5. **Secret store:** sealed Postgres column *(recommended, simplest)* or an external vault?

---

## 4. Spikes (each against a running instance; do these before writing adapters)

| # | Spike | Done when | Rough effort |
| --- | --- | --- | --- |
| 1 | Twenty multi-workspace boot | Two workspaces on two subdomains, each isolated, reachable through Caddy locally | 0.5 day |
| 2 | Twenty service-user flow | Scripted: signIn, signUpInNewWorkspace, loginToken to tokens, createApiKey, and an authenticated record write using only the key | 1 day |
| 3 | Twenty metadata | A custom pipeline object with SELECT stage and custom fields created via API, plus a Kanban view; re-run is a no-op | 1-2 days |
| 4 | Twenty teardown + SSO | Workspace deletion semantics; Keycloak OIDC SSO configured through the API | 1 day |
| 5 | LibreDesk API | Confirm API-key header, create livechat inbox, team, help center, collection, article | 0.5-1 day |
| 6 | LibreDesk pool | Three instances sharing one Postgres server, each installed unattended; assignment and wipe-and-reuse proven | 1 day |
| 7 | `portstest` contract suite | Fake passes it; ready for the real adapters | 1 day |

Order: 7 first (it also tightens today's fakes), then 5 and 6, then 1-4. Adapter code follows each spike, not before.

Overall: roughly **6-9 days** of focused work for both adapters, dominated by the spikes, and **most sensitive to
decision 1**. If you pick option C (a different helpdesk), the LibreDesk half is replaced by an evaluation plus a
new adapter of similar size.

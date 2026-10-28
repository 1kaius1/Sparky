# Entra ID support - design

Status, 2026-10-27: section 3 (sign-in), the fake tenant's sign-in half and version 1
of the probe (section 8) are built, as `internal/auth/entra`, its `fakeentra`
subpackage and `cmd/sparky-entra-probe`, verified against a mock only. Everything
else here is design. This document is
replaced by `ARCHITECTURE.md` and `SCHEMA.md` content as each part lands, and is
then deleted.

The requirement and the decisions already made are in `PLANNING.md` (the v1.0.0
item "Microsoft Entra ID sign-in and group membership" and the three 2026-10-26
Decisions Log entries on it). In short: Entra ID sign-in and group membership are
required for 1.0.0; an Admin chooses **one** directory provider, Active Directory
or Entra ID (or neither), never both; changing the choice needs an explicit
confirmation. This document designs how, and records the decisions the maintainer
made on it (last section).

---

## 1. How this will be tested, and what that allows us to claim

Two facts shape the whole design:

- There is no Entra tenant for development, so everything is built against a
  **fake tenant** written from Microsoft's documentation.
- Testing against a real tenant happens elsewhere: someone who has a tenant builds
  a probe from this repository's public source, runs it, and shares sanitized
  results, as with any open-source project. Nothing here can run against a real
  tenant from the development machine.

So verification against a real tenant happens by a **probe program** run by
whoever has a tenant, whose output is safe to share by construction, with no
tenant ID, token or other identifier in it (section 8). The wording
used in `PLANNING.md`, PR descriptions and release notes follows from that:

| Evidence | What we may say |
|---|---|
| Unit and contract tests against the fake tenant | "verified against a mock only" |
| A probe report from a real tenant, analysed | "verified against a real tenant on DATE, report REF", for exactly the checks the report covers |
| Neither | "not verified" |

The fake tenant encodes one reader's understanding of Microsoft's documentation, so
it can share a misreading with the code it tests. That is why the probe comes early
(PR 2, section 9): a real run corrects the fake before more is built on it.

---

## 2. What exists today and where Entra touches it

| Concern | Today (AD only) | Where |
|---|---|---|
| Sign-in | Username and password bound against LDAP | `auth.IdentityProvider.Authenticate`, `httpapi.LoginService.Login` |
| Login gate | Membership of one AD group, `LDAP_ACCESS_GROUP_DN` | `AuthenticatedUser.InAccessGroup` |
| Hourly recheck | Re-checks the gate by the person's cached DN; a definite "no" logs them out, an unreachable directory fails open | `httpapi.RequireSession`, `recheckAccessGroup`, `auth.IdentityProvider.IsInAccessGroup` |
| Identity | `Users.ad_sid` (unique), `Users.ldap_dn`; `Users.entra_object_id` exists, unused | `SCHEMA.md` Users |
| Address | AD `mail`, cached at sign-in | `email.Service.RecordDirectoryEmail` |
| Application groups | Mapped to AD group DNs; searched, verified, expanded and checked through `auth.GroupDirectory`; membership cached per person | `internal/groups`, tables `app_group_ad_groups`, `user_ad_group_membership` |
| Configuration | `LDAP_*`, all or none; presence means AD is on | `internal/config`, `cmd/sparky-server/main.go` |

Two things in that table do not carry over to Entra unchanged:

- Sign-in is a redirect, not a password call, so `IdentityProvider.Authenticate(user,
  password)` cannot be implemented by an Entra provider.
- Groups and people are addressed by LDAP DN everywhere. Entra identifies both by
  object ID (a GUID).

---

## 3. Entra sign-in

**Flow.** OpenID Connect authorization code flow with PKCE (S256), as a
confidential client (a client secret, from the environment), against the v2.0
endpoints of one tenant.

1. `GET /login/entra` builds the authorize URL and redirects to Entra. It is a
   plain link or redirect, never a form POST, so no CSP `form-action` change is
   needed.
2. Entra redirects to `GET /login/entra/callback?code=...&state=...`, built from
   `SPARKY_PUBLIC_URL`. The redirect address must be registered on the app
   registration; the login page shows it to the Admin as configuration help.
3. The callback exchanges the code (with the PKCE verifier) for tokens and
   validates the ID token. On success it does exactly what a successful LDAP login
   does today: find or create the user (Read-only on first sight), update last
   login, refresh group memberships, record the address, set the signed session
   cookie.

**State, nonce and verifier** live in a short-lived (10 minute) cookie, HttpOnly,
SameSite=Lax, Secure when the site is HTTPS, holding `state`, `nonce`, the PKCE
verifier and the return path. It is authenticated and encrypted with the standard
library (AES-GCM, key derived from `SESSION_SECRET`), not only signed, because it
carries the verifier. It is deleted at the callback whatever the outcome. A
callback with a missing, expired or mismatching state is refused before anything
else happens.

**ID token validation**, all required, none skipped:

- signature verified against the tenant's published signing keys (JWKS from the
  discovery document), algorithm pinned to RS256, key chosen by `kid`, keys
  refreshed when an unknown `kid` appears (rotation) but not on every request;
- `iss` equal to `https://login.microsoftonline.com/<tenant>/v2.0` for the
  configured tenant, `tid` equal to the configured tenant;
- `aud` equal to the app's client ID; `exp` and `nbf` honoured with a small clock
  skew; `nonce` equal to the cookie's.

**Identity key.** The person is identified by the `oid` claim (their object ID in
the tenant), stored in the existing `Users.entra_object_id`. Never `sub`, which is
different for the same person in each application, and never the email or user
name, which can change and are not proof of identity. Single tenant only: a token
from any other tenant is refused (section 11, D3).

**Name and address.** Display name from the token. The address is not trusted from
the token's `email` claim (optional and not verified); it comes from Graph's `mail`
property for that user (section 4) and is validated like AD's, then stored through
the existing `RecordDirectoryEmail` path. Whether to also accept the claim is
decision D5.

**Dependency.** Doing token validation by hand is security-sensitive. The proposal
is to add `github.com/coreos/go-oidc/v3` for discovery, key handling and ID-token
verification, and to do everything else (code exchange, PKCE, Graph) with the
standard library. It brings its own dependencies (`go-jose` and
`golang.org/x/oauth2`, as I understand it; to be confirmed with `go mod` when it is
added). None of these are in the module today. This needs the maintainer's approval
(decision D1) and is documented in the PR that adds it.

---

## 4. Microsoft Graph

Everything beyond sign-in uses Microsoft Graph with the app's own identity (client
credentials, application permissions, scope `https://graph.microsoft.com/.default`).
The access token is cached until shortly before it expires and is never logged.

Group-and-person operations the existing code needs, and the Graph call each maps
to (from my understanding of Microsoft's documentation; each is to be checked
against its page when the fake is written, and this table corrected):

| Need (`auth.GroupDirectory` today) | Graph |
|---|---|
| Search groups by name, nothing unprompted, at most N | `GET /groups` with `$search="displayName:term"`, `ConsistencyLevel: eventual`, `$select=id,displayName`, `$top` (a name-contains search needs the advanced query form) |
| Verify one group exists | `GET /groups/{id}?$select=id,displayName` |
| List a group's people, nested included | `GET /groups/{id}/transitiveMembers/microsoft.graph.user?$select=id`, following pages |
| Which of these groups is this person in, nested included | `POST /users/{oid}/checkMemberGroups` with a list of group IDs (a small maximum per call, so batched) |
| Login gate and recheck | `checkMemberGroups` for the gate group, as above |
| Is the account still usable | `GET /users/{oid}?$select=accountEnabled,mail,displayName` |

**Never the token's groups claim.** Entra stops listing groups in a token when a
person is in too many and sends a pointer to Graph instead. Using Graph always
means the people in the most groups behave like everyone else, and the claim is
ignored whether or not the app registration emits it.

**Paging and redirects.** Pages are followed through `@odata.nextLink`, but only if
it points at the configured Graph host over HTTPS; any other URL is an error, so a
response cannot steer the server to fetch an address of its choosing.

**Throttling and failure.** A 429 or 503 is retried a bounded number of times
honouring `Retry-After` (capped), with a timeout on every call. As today, a
directory that cannot be reached is not a "no": sign-in fails with a clear message,
and the hourly recheck fails open, exactly as the AD path does. A definite answer
(not a member; user not found; account disabled) logs the person out at the next
recheck.

**Permissions.** Which application permissions are the least that work for the calls
above (I believe `GroupMember.Read.All` plus `User.Read.All`, possibly
`Group.Read.All` for search) is read from Microsoft's per-endpoint permission tables
when the fake is written, listed in `docs/SERVER.md` for the person doing the app
registration, and is a tenant-admin-consent step. Decision D2 on the gate and D4 on
which groups affect what the app registration needs.

---

## 5. Configuration and the provider choice

**Environment (secrets and connection details), all or none, like `LDAP_*`:**

| Variable | Meaning |
|---|---|
| `ENTRA_TENANT_ID` | The one tenant |
| `ENTRA_CLIENT_ID` | The app registration |
| `ENTRA_CLIENT_SECRET` | Its secret (certificate auth is a later option) |
| `ENTRA_ACCESS_GROUP_ID` | The login-gate group's object ID |

Optional `ENTRA_AUTHORITY_URL` and `ENTRA_GRAPH_URL`, defaulting to the public
cloud, so the fake tenant can be pointed at in tests; plain HTTP is accepted only
for a loopback host, the same rule the mailer uses. Other clouds (government,
China) are not supported in version 1.0.0 and are not tested.

All are added to `.env.example`. A partial set is a startup error, as for LDAP.

**Database (what the Admin chooses).** A singleton settings row, seeded at
migration time like the other settings rows:

| Field | Notes |
|---|---|
| `provider` | `none` / `ad` / `entra`. A migration cannot see the environment, so it seeds `none`; on its first start after the upgrade the server sets `ad` once if the `LDAP_*` variables are set, so an upgrade changes nothing for an existing deployment |
| `updated_by` / `updated_at` | As the other settings rows |

A provider can be chosen only if it is configured in the environment. The login
page offers exactly the chosen provider plus the local-account link, nothing else.
`none` is today's "no directory configured" behaviour.

---

## 6. Identity and schema

**People.** The identity `CHECK` on Users says exactly one of "AD" or "local-only".
It becomes: local-only, or at least one directory identity. After linking (section
7) one row can carry both an `ad_sid` and an `entra_object_id`, with only the active
provider's usable for sign-in. `entra_object_id` gets a unique index. The
email-source `CHECK` (`directory` only with an AD SID) accepts either identity.
`ldap_dn` stays AD-only.

**Groups.** The tables are named for AD and keyed by DN. They become provider-neutral
in one migration, in the groups PR:

- `app_group_ad_groups` becomes `app_group_directory_groups`, with a `provider`
  column and `group_ref` in place of `ad_group_dn` (a DN for AD, an object ID for
  Entra); existing rows get provider `ad`.
- `user_ad_group_membership` likewise gets `provider` and `group_ref`.
- `auth.GroupDirectory` takes and returns opaque references instead of DNs.

Only mappings and cached memberships of the **active** provider count for
membership and alert audiences. The other provider's rows stay, dormant and shown as
such on the Groups page, so a switch deletes nothing.

A renaming migration touches a lot of code and tests; the alternative (a `provider`
column on the existing tables, with a GUID in `ad_group_dn`) is smaller but leaves a
misleading name. The rename is recommended.

---

## 7. Switching the provider

The Admin picks the new provider on the Settings page. The change is a two-step
flow, because it changes who everyone is to Sparky.

**Step 1, preview** (a page, no change made). From current data and, for the target
provider, from Graph or LDAP:

- how many people currently sign in through the active provider, and how many are
  Admin or PowerDev;
- how many of them could be **linked** to the target provider (below) and how many
  could not;
- how many application-group mappings and cached memberships go dormant;
- whether any Admin could still sign in afterwards (a linked Admin, or an enabled
  local Admin account);
- whether the target provider is reachable and its configuration works (a check,
  not assumed).

**Step 2, confirm.** The Admin types the target provider's name and ticks an
acknowledgement that people not linked will not be able to sign in as themselves.
The confirmation carries a fingerprint of the previewed numbers; if the data
changed between preview and confirmation the change is refused and the preview
repeated. A CSRF-protected POST, Admin only, audited as `switched_directory_provider`
with the old provider, the new one and the counts.

**Refusal.** A switch that would leave no Admin able to sign in (no linked Admin and
no enabled local Admin) is refused, not merely warned about. The break-glass
credential always exists and is a recovery route, not a reason to allow it (D7).

**Linking.** Linking sets the target identity on the person's existing row so they
keep their tier, audit attribution, profile and instance ownership, application-group
membership and permission overrides. It is **never automatic by email address or
user name**, which are not proof of identity. For AD to Entra, a person synced from AD
may expose their on-premises SID in Entra (Graph's `onPremisesSecurityIdentifier`,
from general knowledge, not tried), which allows an exact match against `ad_sid`.
Anyone left over is either linked by an Admin from a proposed list (suggested
matches shown, each confirmed by a person) or left unlinked: they get a new Read-only
row the first time they sign in. The preview shows the numbers so the Admin can
decide before switching, not after.

**After the switch.** Nothing is deleted. People of the old provider cannot sign in
(their rows remain, so switching back restores them). Their existing sessions end at
their next recheck, within the recheck interval, or immediately if decision D6 is
taken. Application groups of the old provider are dormant until re-mapped to
groups of the new one; the Groups page says so.

---

## 8. The test strategy in detail

### The fake tenant

A Go package (`internal/auth/entra/fakeentra`, a normal package so other packages'
tests can use it) serving, from `httptest`:

- the OIDC discovery document, the signing keys, the authorize endpoint (which
  immediately redirects back with a code for a chosen test person) and the token
  endpoint (authorization code with PKCE, and client credentials);
- the Graph endpoints in section 4.

It is **strict**, so that it does not flatter the code: it rejects a wrong redirect
address, a missing or wrong PKCE verifier, a reused or unknown code, a wrong client
secret, a wrong audience or issuer, an expired token, a token signed by an unknown
key, and Graph calls without a valid token. And it reproduces the awkward cases on
demand: key rotation, clock skew, a person in more than 20 groups (batched checks)
and in nested groups, a very large group (paging), a throttled call with
`Retry-After`, a disabled account, a deleted person, a person with no `mail`, a
second tenant's token, Graph being down. Every endpoint cites, in its comment, the
Microsoft documentation page it was written from.

### The contract suite

Tests for the Entra provider are written once against a small `Target` (where the
authority and Graph are, and the credentials) and run:

- against the fake tenant, always (`go test ./...`);
- against a real tenant, only when `SPARKY_ENTRA_REAL_*` variables are set, which
  nobody does on the development machine. The probe (below) is the same checks run
  that way, with safe output.

### The probe

`cmd/sparky-entra-probe`, its own program with no database, no server and no session
secret, built from source by whoever runs it (no binary is shipped for them to
trust). It reads the same `ENTRA_*` variables (and, from the version that adds
step 4, a test person's object ID and a test group's ID), and runs, in order, each
reporting pass or fail with a reason. Version 1 (built in PR 2) runs steps 1, 2, 3 and
5, and shows step 4 as skipped:

1. discovery document fetched and its issuer matches the tenant;
2. signing keys fetched, algorithms and key count recorded;
3. client-credentials token obtained for Graph; the roles it carries recorded by name;
4. each Graph call in section 4, against the test person and group, recording status,
   Graph's error code if any, response shape and page counts;
5. an interactive sign-in: the probe listens on a loopback address, prints a URL for
   the maintainer's browser, completes the code exchange and validates the ID token
   with the same code the server uses, recording which claims were present. The
   redirect address has to be registered on the app registration. From Microsoft's
   redirect URI page: plain http is allowed for localhost addresses and their port is
   ignored when matching; Microsoft recommends 127.0.0.1, but the portal rejects
   `http://127.0.0.1` (it needs a manifest edit) and `[::1]` is not supported. So the
   probe listens on 127.0.0.1, redirects to `http://localhost:<port>/sparky-probe/callback`,
   and the person registers `http://localhost/sparky-probe/callback` under the Web
   platform; the probe's help says so.

**What the report contains, and what it never contains.** The report is plain text
the maintainer reads before sharing. It has step names, pass or fail, HTTP status,
Graph error codes, durations, counts, claim **names** and the **type** of each value,
JSON **key structure** of each response (so the fake can be corrected to the real
shape), and the Graph permissions seen. It never contains a token, the secret, the
tenant ID, any object ID, name, user name or address. Identifiers that must be
related to each other (this person is in that group) are replaced by stable
pseudonyms (`user-1`, `group-2`, `tenant-A`), numbered in order of first sight and kept
only in memory, so they cannot be reversed or matched between runs. (An earlier draft
said a random per-run salt; numbering in order needs no secret at all.) The default
writes nothing sensitive anywhere. An opt-in local-only file holding the real values
for the maintainer's own use was considered and is not built: nothing needs it yet.

**This property is tested, not assumed.** A test runs the probe against the fake
tenant (whose tenant ID, object IDs, names, addresses, tokens and secret are known
canary strings) and fails if any canary appears anywhere in the report, in logs, or in
an error message. Any new field added to the report goes through the same test.

The report is built in layers so that one mistake does not reach the output. A step can
add only typed facts (a result, a count, a yes or no, a short keyword, a set of
keywords), never free text or an error message; a failure is described by a fixed
vocabulary (`entra.ErrorKind`), never by what the network said. Claim and document
member names are shown only if they are standard ones, and anything else is shown as
`other-N`, because a custom name can embed an application ID. And when the report is
rendered it is refused outright, writing nothing, if it contains any value the probe
registered as sensitive (the tenant ID, the secret, every token and identifier it
handled) or anything shaped like a GUID, a token, an email address or a URL. The one
place that does carry identifiers is the sign-in address the person opens in their
browser, printed on the console with a warning and kept out of the report.

The maintainer reviews the report before sharing, and decides what to share.

---

## 9. Pull requests, in order

Each is one PR, merged by the maintainer, and each states what it verified and how.

| # | Contents | Verified by |
|---|---|---|
| 1 | This document and the `PLANNING.md` updates | Review |
| 2 | `internal/auth/entra` sign-in (discovery, PKCE, code exchange, ID-token validation), the fake tenant's authority half, the contract suite for those, and **probe v1** (steps 1, 2, 3 and 5) with its leak test | Mock only; then a **probe run by the maintainer**, analysed, before PR 3 starts |
| 3 | Graph client and the group operations, the fake's Graph half, contract tests, **probe v2** (step 4) | Mock only; then a second probe run |
| 4 | Product wiring: the settings row and `none`/`ad`/`entra` choice (without the switch flow yet), identity migration, login page, callback, gate, recheck, address | Mock only |
| 5 | Application groups made provider-neutral (migration, `GroupDirectory`, mapping pages, dormant mappings) | Mock only |
| 6 | The switch: preview, confirmation, linking, audit, refusal, session handling | Mock only |

Sending PR 2's probe first means the real tenant corrects the fake at the start,
not after five PRs are built on a reading of the documentation.

---

## 10. Out of scope for 1.0.0

Several tenants at once; Entra cloud variants other than the public cloud;
certificate credentials for the app registration; provisioning or removing people in
Entra; reading anything from Entra beyond what section 4 lists; replacing the local
or break-glass accounts.

---

## 11. Decisions

All eight were accepted as recommended on 2026-10-27 and are recorded in the
`PLANNING.md` Decisions Log. The table keeps the question and the reasoning.

| | Question | Decision (as recommended) |
|---|---|---|
| D1 | Add `go-oidc/v3` (and its dependencies) for ID-token verification, or hand-write verification with the standard library? | Add it. Hand-written token validation is where mistakes cost most; everything else stays standard library. Needs explicit approval under the project's dependency rule |
| D2 | The login gate: membership of a group (as with AD), or Entra's own "assignment required" on the enterprise application with an app role? | A group: the same mechanism as application groups and the same recheck. "Assignment required" can still be switched on in Entra as an extra layer |
| D3 | One tenant only, or several? | One. A token from any other tenant is refused. Several is a different product |
| D4 | Which Entra groups can be mapped and used as the gate: security groups only, or also Microsoft 365 groups? | Security groups only at first; widen on request |
| D5 | Address: Graph's `mail` only, or the token's `email` claim as a fallback? | Graph `mail` only; the claim is optional and unverified |
| D6 | After a switch, end old-provider sessions at the next recheck (up to the recheck interval), or immediately? | Immediately, with a cheap cached check on each request. It matters if the switch is a response to a compromise. It changes the session format once, so everyone signs in again after the upgrade |
| D7 | A switch that would lock out every Admin: refuse, or warn? | Refuse |
| D8 | Probe delivery: source only, built by whoever runs it? | Yes. The probe is small enough to read in full before building it |

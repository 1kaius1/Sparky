# Running the Entra probe

For anyone with a Microsoft Entra ID tenant who wants to help check Sparky's
sign-in code against a real tenant. Sparky's Entra support is written and tested
against a fake tenant built from Microsoft's documentation, and a fake can share a
misreading with the code it tests. The probe is a small program that does what
Sparky will do, against your tenant, and writes a report you can read and then
choose to share. Until a report has been looked at, Sparky describes its Entra
support as "verified against a mock only".

Design background is in `ENTRA_DESIGN.md` section 8. This page is the how-to.

## What it does to your tenant

It only reads and signs in. It creates and changes nothing in your tenant. It makes:

- one request for an app-only token for Microsoft Graph, using a client secret;
- one interactive sign-in through your own browser, as an account you choose.

Both show up in the tenant's sign-in logs like any other sign-in to that app. It
does not call Microsoft Graph itself in this version; it only asks for a token for
it and looks at the token's shape.

The probe is small enough to read before you build it: `cmd/sparky-entra-probe`
and `internal/auth/entra`. No binary is distributed, on purpose.

## What you need

- Go 1.26 or newer, and a copy of this repository.
- An app registration in your tenant (below), and a test account to sign in with.
  A test account that is not a real person's is the better choice.

### The app registration

In the Microsoft Entra admin center, under App registrations:

1. New registration. Any name, for example "Sparky probe". Supported account types:
   "Accounts in this organizational directory only" (a single tenant).
2. Redirect URI: platform **Web**, address `http://localhost/sparky-probe/callback`.
   Microsoft allows plain http for localhost addresses and ignores the port when
   matching them, so the probe may use any free port.
3. Certificates and secrets, New client secret. Copy the **Value** (not the Secret
   ID) at once; it is shown only once.
4. From the Overview page, note the Application (client) ID and the Directory
   (tenant) ID.

No API permissions need to be added for this version of the probe. The app token
step records whatever application permissions the app has been granted, and if Entra
refuses the request because it has none, the report says so, which is also useful to
know. Adding the application permissions Sparky will need (they are listed in
`ENTRA_DESIGN.md` section 4 once the Graph step exists) gives a more complete picture,
but is not required to run this version.

## Build and run

```bash
go build -o sparky-entra-probe ./cmd/sparky-entra-probe
```

The probe reads its settings from the environment only, because a secret given on
a command line shows up in process lists and shell history. `read` keeps the values
out of your history too:

```bash
read -r  ENTRA_TENANT_ID   && export ENTRA_TENANT_ID
read -r  ENTRA_CLIENT_ID   && export ENTRA_CLIENT_ID
read -rs ENTRA_CLIENT_SECRET && export ENTRA_CLIENT_SECRET   # no echo
./sparky-entra-probe --report probe-report.txt
```

The probe prints each step's result as it finishes. For the sign-in step it prints
an address: open it in a browser and sign in with your test account. **That address
contains your tenant ID and application ID, so it is not part of the report and
must not be shared.** The probe waits five minutes by default (`--signin-timeout`).

Other options: `--skip-signin` skips the interactive step, `--listen-port` fixes the
loopback port, and `-h` prints the help. `--report` writes to a new file and refuses
to overwrite one that exists; without it the report goes to standard output.

When you are done, delete the client secret, or the whole app registration.

## The report

The report is plain text. It contains, for each step: whether it passed, the HTTP
status, counts, yes or no answers (for example, "is the issuer what Sparky
expects"), the names and the JSON type of the claims in a token and the members of
the documents Entra returns, and the application permissions on the app token. That
is what is needed to correct the fake tenant where it differs from the real one.

It never contains a token, the client secret, the tenant ID, the application ID, an
object ID, a name, a user name, an email address, or a URL. The signed-in person
appears only as a label such as `user-1`, and the tenant as `tenant-A`. Claim names
that are not standard ones are shown as `other-1`, `other-2`, because a custom name
can contain an identifier.

This is enforced, not just intended. The report is built from typed facts and
cannot hold free text. When it is written, the probe refuses to write it at all, and
says so, if it contains anything it handled that is sensitive or anything shaped
like a GUID, a token, an address or a URL. The test suite plants known values in a
fake tenant and fails if any of them reaches the report.

You are still the last check. Before sharing, read it, and:

```bash
grep -c -i -e "$ENTRA_TENANT_ID" -e "$ENTRA_CLIENT_ID" probe-report.txt   # should print 0
```

then search it for your own name and your domain. Share what you are comfortable
sharing; a report with a failed step is as useful as one that passes.

## If something fails

The report names the kind of failure, never the network's text. Exit codes: 0 all
steps passed or were skipped, 1 a step failed, 2 a setup problem (a missing or
invalid setting, or the report file could not be created), 3 the report was
refused because it looked unsafe.

Common ones: `AADSTS error numbers: 7000215` means the secret is wrong (a common
cause is copying the Secret ID instead of the Value); `50011` means the redirect
address does not match what is registered; `700016` means the application ID is not
in that tenant. Microsoft says these numbers may change, so they are recorded for
reading, not relied on.

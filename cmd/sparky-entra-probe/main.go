// SPDX-License-Identifier: AGPL-3.0-or-later

// Command sparky-entra-probe checks Sparky's Microsoft Entra ID sign-in code
// against a real tenant and writes a report that is safe to share by
// construction: it contains no token, secret, tenant ID, application ID, object
// ID, name, address or URL. See docs/ENTRA_PROBE.md and docs/ENTRA_DESIGN.md
// section 8.
//
// It exists because the fake tenant the code is tested against is one reader's
// understanding of Microsoft's documentation. A run against a real tenant,
// reported safely, is the only check against that reading.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"time"

	"github.com/1kaius1/Sparky/internal/auth/entra"
)

// probeVersion is shown at the top of the report so an analysis knows which
// steps a report can contain.
const probeVersion = "1"

const usageText = `sparky-entra-probe - checks Sparky's Entra ID sign-in code against a real tenant

It makes real requests to your tenant, and they appear in its sign-in logs: one
app-only token request and one interactive sign-in. It sends nothing anywhere
else and writes nothing but the report.

Configuration is read from the environment only (a secret on a command line shows
in process lists and shell history):

  ENTRA_TENANT_ID       the tenant's ID (a GUID)
  ENTRA_CLIENT_ID       the app registration's application (client) ID
  ENTRA_CLIENT_SECRET   one of its client secrets
  ENTRA_AUTHORITY_URL   optional; defaults to the public cloud sign-in host

On the app registration, add a redirect address under the "Web" platform:

  http://localhost/sparky-probe/callback

(Entra ignores the port of a localhost address, so the probe may use any.) No API
permissions are required to run this version; the app-token step reports whatever
permissions the app has been granted.

Options:
`

func main() {
	os.Exit(run(os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

// run is main without the process-level parts, so a test can drive it.
func run(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("sparky-entra-probe", flag.ContinueOnError)
	fs.SetOutput(stderr)
	skipSignIn := fs.Bool("skip-signin", false, "skip the interactive sign-in (step 5)")
	port := fs.Int("listen-port", 0, "loopback port for the sign-in callback (0 picks a free one)")
	timeout := fs.Duration("signin-timeout", 5*time.Minute, "how long to wait for the sign-in")
	reportPath := fs.String("report", "", "write the report to this new file instead of standard output")
	fs.Usage = func() {
		fmt.Fprint(stderr, usageText)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *port < 0 || *port > 65535 {
		fmt.Fprintln(stderr, "the listen port must be between 0 and 65535")
		return 2
	}

	cfg, err := entra.Config{
		TenantID:     getenv("ENTRA_TENANT_ID"),
		ClientID:     getenv("ENTRA_CLIENT_ID"),
		ClientSecret: entra.Secret(getenv("ENTRA_CLIENT_SECRET")),
		AuthorityURL: getenv("ENTRA_AUTHORITY_URL"),
		// Replaced with the real loopback address for the sign-in step.
		RedirectURL: "http://localhost" + callbackPath,
	}.Normalized()
	if err != nil {
		// These messages name the setting, never its value.
		fmt.Fprintf(stderr, "configuration problem: %v\n(run with -h for the settings)\n", err)
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	p := newProbe(probeConfig{Entra: cfg, SignIn: !*skipSignIn, ListenPort: *port, SignInTimeout: *timeout}, stderr)
	p.Run(ctx)

	var out bytes.Buffer
	err = p.report.Render(&out,
		"Sparky Entra ID probe report",
		"Probe version: "+probeVersion,
		"Run date (UTC): "+time.Now().UTC().Format("2006-01-02"),
		"Covers: discovery document, signing keys, app token, interactive sign-in. Microsoft Graph calls are not in this version.",
		"This report contains no token, secret, tenant ID, application ID, object ID, name, address or URL.",
		"Read it before you share it.",
	)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 3
	}

	if *reportPath != "" {
		// O_EXCL: never overwrite a file the person did not mean to replace.
		f, err := os.OpenFile(*reportPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			fmt.Fprintln(stderr, "could not create the report file (it must not already exist)")
			return 2
		}
		_, werr := f.Write(out.Bytes())
		cerr := f.Close()
		if werr != nil || cerr != nil {
			fmt.Fprintln(stderr, "could not write the report file")
			return 2
		}
		fmt.Fprintln(stderr, "report written to the file you named")
	} else if _, err := stdout.Write(out.Bytes()); err != nil {
		return 2
	}

	if !p.report.Overall() {
		return 1
	}
	return 0
}

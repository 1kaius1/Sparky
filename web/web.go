// SPDX-License-Identifier: AGPL-3.0-or-later

// Package web embeds the dashboard's templates and static assets into the
// server binary - see CLAUDE.md Tech Stack ("single-binary deployment via
// embed.FS") and ARCHITECTURE.md Deployment Topology ("Static assets:
// embedded in the binary via embed.FS, served directly - no CDN"). This
// package holds only the //go:embed directive: it must live at or above
// templates/ and static/ in the tree, which internal/httpapi (where these
// assets are actually parsed and served) is not - see CLAUDE.md's
// repository layout, where web/ and internal/ are siblings.
package web

import "embed"

// FS holds every template and static asset. Editing a template or static
// file requires a server restart to pick up - CLAUDE.md Build and Run:
// "Nothing to build... requires a server restart," no dev-mode hot-reload
// flag exists yet.
//
//go:embed templates static
var FS embed.FS

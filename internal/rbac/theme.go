// SPDX-License-Identifier: AGPL-3.0-or-later

package rbac

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"

	"github.com/1kaius1/Sparky/internal/db"
)

// ErrInvalidTheme is returned by UpdateOwnTheme (this package) and
// internal/settings.Service's UpdateDefaultTheme when a preset, status
// palette, custom color key, or hex value fails validation.
var ErrInvalidTheme = errors.New("invalid theme")

// ThemeColorKey pairs one customizable CSS custom-property name with a
// short, human-typeable slug (no "--color-" prefix, underscores instead
// of hyphens) - one canonical, ordered scheme shared by every call site
// that needs a name for the same 15 keys: internal/httpapi/account.go's
// <input> field names and display labels, internal/httpapi/settings.go's
// YAML upload "colors:" map keys, and this file's own whitelist checks.
type ThemeColorKey struct {
	CSSVar string // e.g. "--color-primary"
	Slug   string // e.g. "primary"
}

// ThemeColorKeys lists every customizable color, in a fixed display
// order - the single source of truth other packages build picker UIs
// from, so there is exactly one place this 15-entry list is maintained.
// Deliberately excludes every --color-status-* variable (SCHEMA.md Users:
// status colors are locked, never part of arbitrary overrides), the two
// preset-only --color-chart-accent-* variables (chart series colors stay
// preset-only, not user-overridable), and the non-color tokens
// (--sidebar-width, --radius).
var ThemeColorKeys = []ThemeColorKey{
	{"--color-bg", "bg"},
	{"--color-surface", "surface"},
	{"--color-border", "border"},
	{"--color-text", "text"},
	{"--color-text-muted", "text_muted"},
	{"--color-primary", "primary"},
	{"--color-primary-contrast", "primary_contrast"},
	{"--color-sidebar-bg", "sidebar_bg"},
	{"--color-sidebar-text", "sidebar_text"},
	{"--color-sidebar-text-active", "sidebar_text_active"},
	{"--color-sidebar-brand", "sidebar_brand"},
	{"--color-sidebar-hover-bg", "sidebar_hover_bg"},
	{"--color-sidebar-active-bg", "sidebar_active_bg"},
	{"--color-form-error-bg", "form_error_bg"},
	{"--color-form-success-bg", "form_success_bg"},
}

var themeColorKeySet = func() map[string]bool {
	m := make(map[string]bool, len(ThemeColorKeys))
	for _, k := range ThemeColorKeys {
		m[k.CSSVar] = true
	}
	return m
}()

var slugToThemeColorKey = func() map[string]string {
	m := make(map[string]string, len(ThemeColorKeys))
	for _, k := range ThemeColorKeys {
		m[k.Slug] = k.CSSVar
	}
	return m
}()

// AllowedThemeColorKey reports whether key (a CSS custom-property name
// like "--color-primary") is one of the whitelisted customizable
// variables - injected into a server-rendered <style> block in
// web/templates/layouts/base.html, so this whitelist is security-
// sensitive, not just cosmetic.
func AllowedThemeColorKey(key string) bool {
	return themeColorKeySet[key]
}

// CSSVarForThemeColorSlug resolves a short slug (as used in an account
// page form field or a YAML upload's "colors:" map) back to its real CSS
// variable name, e.g. "sidebar_bg" -> "--color-sidebar-bg". Returns false
// for an unrecognized slug.
func CSSVarForThemeColorSlug(slug string) (string, bool) {
	cssVar, ok := slugToThemeColorKey[slug]
	return cssVar, ok
}

// hexColorPattern requires a strict, 6-digit #RRGGBB value only - no
// 3-digit shorthand, no alpha channel, no named CSS colors, no
// whitespace - so every accepted value is a fixed-length, fixed-
// character-class token that cannot contain '<', '"', ';', a newline, or
// a '}' that could break out of the <style> block's declaration.
var hexColorPattern = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

// ValidHexColor reports whether v is a strict #RRGGBB hex color.
func ValidHexColor(v string) bool {
	return hexColorPattern.MatchString(v)
}

// ValidThemePreset reports whether p is one of the 8 built-in presets.
func ValidThemePreset(p db.ThemePreset) bool {
	for _, valid := range db.ThemePresets {
		if p == valid {
			return true
		}
	}
	return false
}

// ValidThemeStatusPalette reports whether p is a recognized status
// palette.
func ValidThemeStatusPalette(p db.ThemeStatusPalette) bool {
	return p == db.ThemeStatusPaletteLight || p == db.ThemeStatusPaletteDark
}

// ValidateThemeCustomColors checks every key/value in raw against the
// whitelist and hex-format rules above and returns the result as
// json.RawMessage ready to persist. An empty/nil raw is valid (clears
// customization) and marshals to "{}". Exported because both
// rbac.Service.UpdateOwnTheme and internal/settings.Service.
// UpdateDefaultTheme (the plain preset path and the YAML-upload path)
// need the identical check - one validation function, multiple callers.
func ValidateThemeCustomColors(raw map[string]string) (json.RawMessage, error) {
	if raw == nil {
		raw = map[string]string{}
	}
	for key, value := range raw {
		if !AllowedThemeColorKey(key) {
			return nil, fmt.Errorf("%w: %q is not a customizable color", ErrInvalidTheme, key)
		}
		if !ValidHexColor(value) {
			return nil, fmt.Errorf("%w: %q must be a #RRGGBB hex color", ErrInvalidTheme, key)
		}
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("encode custom colors: %w", err)
	}
	return encoded, nil
}

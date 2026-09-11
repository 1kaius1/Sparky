// SPDX-License-Identifier: AGPL-3.0-or-later

package httpapi

import (
	"github.com/1kaius1/Sparky/internal/db"
	"github.com/1kaius1/Sparky/internal/rbac"
)

// themePresetOption is one entry in a theme preset picker - shared view
// model between the account page (a user's own preference) and the
// Settings page (the Admin-configurable system default).
type themePresetOption struct {
	Value, Label, Family string
}

var themePresetLabels = map[db.ThemePreset]string{
	db.ThemePresetSlateLight:   "Slate",
	db.ThemePresetLinenLight:   "Linen",
	db.ThemePresetArcticLight:  "Arctic",
	db.ThemePresetSandLight:    "Sand",
	db.ThemePresetCarbonDark:   "Carbon",
	db.ThemePresetMatrixDark:   "Matrix",
	db.ThemePresetTronDark:     "Tron",
	db.ThemePresetAmethystDark: "Amethyst",
}

// themePresetOptions lists every built-in preset as a picker option, in
// db.ThemePresets' own display order (light family first, then dark).
func themePresetOptions() []themePresetOption {
	options := make([]themePresetOption, 0, len(db.ThemePresets))
	for _, p := range db.ThemePresets {
		options = append(options, themePresetOption{
			Value:  string(p),
			Label:  themePresetLabels[p],
			Family: string(p.Family()),
		})
	}
	return options
}

// themeColorField is one customizable color's picker row. Value is only
// set when the viewer has an existing explicit override for this key -
// left empty otherwise, so the template's inline script (see
// account.html) knows to fill the <input type="color"> from the live
// computed CSS value instead of the browser's own black default. Without
// this distinction, a color <input> always submits a concrete hex value
// (never "unset"), so simply checking "customize" without touching any
// picker would submit #000000 for every key.
type themeColorField struct {
	Slug, Label, CSSVar, Value string
}

var themeColorLabels = map[string]string{
	"bg":                  "Background",
	"surface":             "Surface",
	"border":              "Border",
	"text":                "Text",
	"text_muted":          "Muted text",
	"primary":             "Primary accent",
	"primary_contrast":    "Primary accent contrast (button text)",
	"sidebar_bg":          "Sidebar background",
	"sidebar_text":        "Sidebar text",
	"sidebar_text_active": "Sidebar active text",
	"sidebar_brand":       "Sidebar brand",
	"sidebar_hover_bg":    "Sidebar hover background",
	"sidebar_active_bg":   "Sidebar active background",
	"form_error_bg":       "Error message background",
	"form_success_bg":     "Success message background",
}

// themeColorFields builds one row per customizable color, in
// rbac.ThemeColorKeys' fixed order, filling Value from current (a slug ->
// "#RRGGBB" map, already translated from CSS variable names) where an
// override exists.
func themeColorFields(current map[string]string) []themeColorField {
	fields := make([]themeColorField, 0, len(rbac.ThemeColorKeys))
	for _, k := range rbac.ThemeColorKeys {
		fields = append(fields, themeColorField{
			Slug:   k.Slug,
			Label:  themeColorLabels[k.Slug],
			CSSVar: k.CSSVar,
			Value:  current[k.Slug],
		})
	}
	return fields
}

// cssVarMapToSlugMap translates a map keyed by CSS variable name (as
// stored in ThemeCustomColors/DefaultCustomColors) into one keyed by
// short slug, for populating form fields.
func cssVarMapToSlugMap(cssVars map[string]string) map[string]string {
	slugs := make(map[string]string, len(cssVars))
	for _, k := range rbac.ThemeColorKeys {
		if v, ok := cssVars[k.CSSVar]; ok {
			slugs[k.Slug] = v
		}
	}
	return slugs
}

// SPDX-License-Identifier: AGPL-3.0-or-later

package db

// ThemePreset mirrors the theme_preset Postgres enum - see
// migrations/000026_create_theme_settings.up.sql and SCHEMA.md Users /
// Theme settings. Shared by users.theme_preset (nullable - a user's own
// choice) and theme_settings.default_theme (never null - the base preset
// underlying the Admin-configured system-wide default, even when that
// default is a fully custom uploaded theme), which is why it lives in its
// own file rather than users.go or theme_settings.go alone.
type ThemePreset string

const (
	ThemePresetSlateLight   ThemePreset = "slate-light"
	ThemePresetLinenLight   ThemePreset = "linen-light"
	ThemePresetArcticLight  ThemePreset = "arctic-light"
	ThemePresetSandLight    ThemePreset = "sand-light"
	ThemePresetCarbonDark   ThemePreset = "carbon-dark"
	ThemePresetMatrixDark   ThemePreset = "matrix-dark"
	ThemePresetTronDark     ThemePreset = "tron-dark"
	ThemePresetAmethystDark ThemePreset = "amethyst-dark"
)

// ThemeFamily is which of the two status-color palettes a preset uses by
// default - see ThemeStatusPalette and SCHEMA.md Users.
type ThemeFamily string

const (
	ThemeFamilyLight ThemeFamily = "light"
	ThemeFamilyDark  ThemeFamily = "dark"
)

var themePresetFamilies = map[ThemePreset]ThemeFamily{
	ThemePresetSlateLight:   ThemeFamilyLight,
	ThemePresetLinenLight:   ThemeFamilyLight,
	ThemePresetArcticLight:  ThemeFamilyLight,
	ThemePresetSandLight:    ThemeFamilyLight,
	ThemePresetCarbonDark:   ThemeFamilyDark,
	ThemePresetMatrixDark:   ThemeFamilyDark,
	ThemePresetTronDark:     ThemeFamilyDark,
	ThemePresetAmethystDark: ThemeFamilyDark,
}

// Family reports which status-color palette p uses when nothing has
// explicitly overridden it - see ThemeStatusPalette. An unrecognized
// preset (should never happen past validation in internal/rbac) defaults
// to ThemeFamilyDark, matching the system-wide shipped default's own
// family.
func (p ThemePreset) Family() ThemeFamily {
	if f, ok := themePresetFamilies[p]; ok {
		return f
	}
	return ThemeFamilyDark
}

// ThemePresets lists every valid preset, in display order (light family
// first, then dark) - the single source of truth for the account and
// settings page pickers.
var ThemePresets = []ThemePreset{
	ThemePresetSlateLight, ThemePresetLinenLight, ThemePresetArcticLight, ThemePresetSandLight,
	ThemePresetCarbonDark, ThemePresetMatrixDark, ThemePresetTronDark, ThemePresetAmethystDark,
}

// ThemeStatusPalette mirrors the theme_status_palette Postgres enum - the
// explicit, Slack-style toggle a customizing user (or an Admin's uploaded
// default) can set independent of the base preset's own family. See
// SCHEMA.md Users.
type ThemeStatusPalette string

const (
	ThemeStatusPaletteLight ThemeStatusPalette = "light"
	ThemeStatusPaletteDark  ThemeStatusPalette = "dark"
)

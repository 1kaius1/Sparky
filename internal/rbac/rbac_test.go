// SPDX-License-Identifier: AGPL-3.0-or-later

package rbac

import (
	"fmt"
	"testing"

	"github.com/1kaius1/Sparky/internal/db"
)

var allTiers = []db.Tier{db.TierReadOnly, db.TierDeveloper, db.TierPowerDev, db.TierAdmin}

func TestCanElevate_SuperAdmin_AlwaysPermitted(t *testing.T) {
	actor := Actor{IsSuperAdmin: true}

	for _, from := range allTiers {
		for _, to := range allTiers {
			if !CanElevate(actor, from, to) {
				t.Errorf("CanElevate(SuperAdmin, %q, %q) = false, want true", from, to)
			}
		}
	}
}

func TestCanElevate_Admin_ExhaustiveMatrix(t *testing.T) {
	actor := Actor{Tier: db.TierAdmin}

	// want[from][to] - every (from, to) pair across all four tiers.
	// Admin's authority is exactly one step, either direction, strictly
	// within {read_only, developer, power_dev} - see SCHEMA.md Users,
	// Elevation rules. Admin tier as either endpoint is always refused.
	want := map[db.Tier]map[db.Tier]bool{
		db.TierReadOnly: {
			db.TierReadOnly:  false, // no-op
			db.TierDeveloper: true,  // single-step promotion
			db.TierPowerDev:  false, // skips a step
			db.TierAdmin:     false, // Admin tier is out of range
		},
		db.TierDeveloper: {
			db.TierReadOnly:  true, // single-step demotion
			db.TierDeveloper: false,
			db.TierPowerDev:  true, // single-step promotion
			db.TierAdmin:     false,
		},
		db.TierPowerDev: {
			db.TierReadOnly:  false, // skips a step
			db.TierDeveloper: true,  // single-step demotion
			db.TierPowerDev:  false,
			db.TierAdmin:     false,
		},
		db.TierAdmin: {
			db.TierReadOnly:  false, // Admin can't touch an existing Admin at all
			db.TierDeveloper: false,
			db.TierPowerDev:  false,
			db.TierAdmin:     false,
		},
	}

	for _, from := range allTiers {
		for _, to := range allTiers {
			got := CanElevate(actor, from, to)
			if got != want[from][to] {
				t.Errorf("CanElevate(Admin, %q, %q) = %v, want %v", from, to, got, want[from][to])
			}
		}
	}
}

func TestCanElevate_NonAdminActors_NeverPermitted(t *testing.T) {
	for _, actorTier := range []db.Tier{db.TierReadOnly, db.TierDeveloper, db.TierPowerDev} {
		actor := Actor{Tier: actorTier}
		t.Run(string(actorTier), func(t *testing.T) {
			for _, from := range allTiers {
				for _, to := range allTiers {
					if CanElevate(actor, from, to) {
						t.Errorf("CanElevate(%q, %q, %q) = true, want false", actorTier, from, to)
					}
				}
			}
		})
	}
}

func TestCanManageModelStore(t *testing.T) {
	tests := []struct {
		actor       Actor
		hasOverride bool
		want        bool
	}{
		{Actor{IsSuperAdmin: true}, false, true},
		{Actor{IsSuperAdmin: true}, true, true},
		{Actor{Tier: db.TierAdmin}, false, true},
		{Actor{Tier: db.TierAdmin}, true, true},
		{Actor{Tier: db.TierPowerDev}, true, true},
		{Actor{Tier: db.TierPowerDev}, false, false},
		{Actor{Tier: db.TierDeveloper}, true, false},
		{Actor{Tier: db.TierDeveloper}, false, false},
		{Actor{Tier: db.TierReadOnly}, true, false},
		{Actor{Tier: db.TierReadOnly}, false, false},
	}

	for _, tt := range tests {
		name := fmt.Sprintf("tier=%s,superadmin=%v,override=%v", tt.actor.Tier, tt.actor.IsSuperAdmin, tt.hasOverride)
		t.Run(name, func(t *testing.T) {
			got := CanManageModelStore(tt.actor, tt.hasOverride)
			if got != tt.want {
				t.Errorf("CanManageModelStore() = %v, want %v", got, tt.want)
			}
		})
	}
}

// SPDX-License-Identifier: AGPL-3.0-or-later

package runtime

import (
	"reflect"
	"testing"
)

func TestGPURequest(t *testing.T) {
	cases := []struct {
		backend string
		mech    GPUDeviceMechanism
		cdi     []string
		env     []string
	}{
		{"docker", GPUDeviceMechanismNvidia, nil, []string{"NVIDIA_VISIBLE_DEVICES=all"}},
		{"podman", GPUDeviceMechanismCDI, []string{"nvidia.com/gpu=all"}, nil},
		{"bare-metal", "", nil, nil},
		{"", "", nil, nil},
		{"something-else", "", nil, nil},
	}
	for _, c := range cases {
		mech, cdi, env := GPURequest(c.backend)
		if mech != c.mech || !reflect.DeepEqual(cdi, c.cdi) || !reflect.DeepEqual(env, c.env) {
			t.Errorf("GPURequest(%q) = %q %v %v, want %q %v %v", c.backend, mech, cdi, env, c.mech, c.cdi, c.env)
		}
	}
}

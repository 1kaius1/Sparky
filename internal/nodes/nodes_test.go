// SPDX-License-Identifier: AGPL-3.0-or-later

package nodes

import (
	"errors"
	"testing"

	"github.com/1kaius1/Sparky/internal/db"
)

func validBareMetalParams() RegisterNodeParams {
	return RegisterNodeParams{
		Name:           "spark-1",
		Hostname:       "spark-1.local",
		IPAddress:      "10.0.0.5",
		RuntimeBackend: db.RuntimeBackendBareMetal,
		GPUMemoryGB:    128,
		CPUMemoryGB:    128,
	}
}

func validDockerGPUParams() RegisterNodeParams {
	return RegisterNodeParams{
		Name:           "gpu-host-1",
		Hostname:       "gpu-host-1.local",
		IPAddress:      "10.0.0.6",
		RuntimeBackend: db.RuntimeBackendPodman,
		GPUMemoryGB:    24,
		CPUMemoryGB:    64,
	}
}

func TestRegisterNodeParams_Validate_Valid(t *testing.T) {
	for name, params := range map[string]RegisterNodeParams{
		"bare-metal": validBareMetalParams(),
		"docker-gpu": validDockerGPUParams(),
	} {
		t.Run(name, func(t *testing.T) {
			if err := params.validate(); err != nil {
				t.Errorf("validate() error = %v, want nil", err)
			}
		})
	}
}

func TestRegisterNodeParams_Validate_Invalid(t *testing.T) {
	tests := map[string]RegisterNodeParams{
		"empty name": func() RegisterNodeParams {
			p := validBareMetalParams()
			p.Name = ""
			return p
		}(),
		"empty hostname": func() RegisterNodeParams {
			p := validBareMetalParams()
			p.Hostname = ""
			return p
		}(),
		"empty ip_address": func() RegisterNodeParams {
			p := validBareMetalParams()
			p.IPAddress = ""
			return p
		}(),
		"unknown runtime_backend": func() RegisterNodeParams {
			p := validBareMetalParams()
			p.RuntimeBackend = db.RuntimeBackend("unknown")
			return p
		}(),
		"zero gpu_memory_gb": func() RegisterNodeParams {
			p := validBareMetalParams()
			p.GPUMemoryGB = 0
			return p
		}(),
		"negative cpu_memory_gb": func() RegisterNodeParams {
			p := validBareMetalParams()
			p.CPUMemoryGB = -1
			return p
		}(),
	}

	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			err := params.validate()
			if !errors.Is(err, ErrInvalidNode) {
				t.Errorf("validate() error = %v, want ErrInvalidNode", err)
			}
		})
	}
}

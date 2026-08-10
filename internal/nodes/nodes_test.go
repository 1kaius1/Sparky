// SPDX-License-Identifier: AGPL-3.0-or-later

package nodes

import (
	"errors"
	"testing"

	"github.com/1kaius1/Sparky/internal/db"
)

func validSparkParams() RegisterNodeParams {
	return RegisterNodeParams{
		Name:        "spark-1",
		Hostname:    "spark-1.local",
		IPAddress:   "10.0.0.5",
		NodeType:    db.NodeTypeSpark,
		GPUMemoryGB: 128,
		CPUMemoryGB: 128,
	}
}

func validDockerGPUParams() RegisterNodeParams {
	runtime := db.ContainerRuntimePodman
	return RegisterNodeParams{
		Name:             "gpu-host-1",
		Hostname:         "gpu-host-1.local",
		IPAddress:        "10.0.0.6",
		NodeType:         db.NodeTypeDockerGPU,
		ContainerRuntime: &runtime,
		GPUMemoryGB:      24,
		CPUMemoryGB:      64,
	}
}

func TestRegisterNodeParams_Validate_Valid(t *testing.T) {
	for name, params := range map[string]RegisterNodeParams{
		"spark":      validSparkParams(),
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
	runtime := db.ContainerRuntimeDocker

	tests := map[string]RegisterNodeParams{
		"empty name": func() RegisterNodeParams {
			p := validSparkParams()
			p.Name = ""
			return p
		}(),
		"empty hostname": func() RegisterNodeParams {
			p := validSparkParams()
			p.Hostname = ""
			return p
		}(),
		"empty ip_address": func() RegisterNodeParams {
			p := validSparkParams()
			p.IPAddress = ""
			return p
		}(),
		"spark with container_runtime set": func() RegisterNodeParams {
			p := validSparkParams()
			p.ContainerRuntime = &runtime
			return p
		}(),
		"docker-gpu with no container_runtime": func() RegisterNodeParams {
			p := validDockerGPUParams()
			p.ContainerRuntime = nil
			return p
		}(),
		"unknown node_type": func() RegisterNodeParams {
			p := validSparkParams()
			p.NodeType = db.NodeType("unknown")
			return p
		}(),
		"zero gpu_memory_gb": func() RegisterNodeParams {
			p := validSparkParams()
			p.GPUMemoryGB = 0
			return p
		}(),
		"negative cpu_memory_gb": func() RegisterNodeParams {
			p := validSparkParams()
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

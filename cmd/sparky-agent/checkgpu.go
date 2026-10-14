// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/1kaius1/Sparky/agent/gpucheck"
	"github.com/1kaius1/Sparky/agent/provision"
	"github.com/1kaius1/Sparky/agent/runtime"
	"github.com/1kaius1/Sparky/agent/runtime/containers"
)

// runCheckGPU implements `sparky-agent check-gpu`: start a short container the
// way Sparky starts engine containers and report whether it sees a GPU. See
// agent/gpucheck for why this is worth a command of its own.
//
// Dispatched before config.Load(), like setup and scan-models: it needs only
// the node's runtime backend and container socket, not the central URL and
// token that config.Load insists on, so an operator can run it right after
// install, before the node is configured. Both come from the environment if
// set, else from secrets.env, so it works from a plain shell as the service
// account:
//
//	sudo -u serviceloop sparky-agent check-gpu
//
// Exit status: 0 for ok or not applicable, 1 when the GPU check failed, 2 for
// a usage or configuration problem.
func runCheckGPU(args []string) int {
	return checkGPU(args, os.Getenv, newContainerBackend, os.Stdout, os.Stderr)
}

// newContainerBackend builds the real backend for a container runtime. The
// socket address comes from DOCKER_HOST, which the Docker client reads from
// the environment, so a value found in secrets.env has to be exported first.
func newContainerBackend(runtimeBackend, dockerHost string) (runtime.Backend, error) {
	if runtimeBackend != "docker" && runtimeBackend != "podman" {
		return nil, nil
	}
	if dockerHost != "" {
		if err := os.Setenv("DOCKER_HOST", dockerHost); err != nil {
			return nil, fmt.Errorf("set DOCKER_HOST: %w", err)
		}
	}
	return containers.New()
}

func checkGPU(args []string, getenv func(string) string, newBackend func(runtimeBackend, dockerHost string) (runtime.Backend, error), stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("check-gpu", flag.ContinueOnError)
	fs.SetOutput(stderr)
	envFile := fs.String("env-file", provision.DefaultSecretsEnvPath, "secrets.env to read SPARKY_RUNTIME_BACKEND and DOCKER_HOST from when they are not in the environment")
	image := fs.String("image", "", "probe image (default: $SPARKY_GPU_CHECK_IMAGE, else "+gpucheck.DefaultImage+")")
	timeout := fs.Duration("timeout", gpucheck.DefaultTimeout, "how long to wait for the probe, including an image pull")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	// The environment wins over the file, as for the service itself. An
	// unreadable file is only a problem if it leaves the backend unknown.
	var fileErr error
	lookup := func(key string) string {
		if v := strings.TrimSpace(getenv(key)); v != "" {
			return v
		}
		v, err := provision.ReadEnvFileValue(*envFile, key)
		if err != nil {
			fileErr = err
			return ""
		}
		return v
	}

	runtimeBackend := lookup("SPARKY_RUNTIME_BACKEND")
	if runtimeBackend == "" {
		if fileErr != nil {
			fmt.Fprintf(stderr, "check-gpu: cannot read %s: %v\n  Run it as the service account (sudo -u serviceloop sparky-agent check-gpu), or export SPARKY_RUNTIME_BACKEND (and DOCKER_HOST for a non-default socket).\n", *envFile, fileErr)
		} else {
			fmt.Fprintf(stderr, "check-gpu: SPARKY_RUNTIME_BACKEND is not set in the environment or in %s\n", *envFile)
		}
		return 2
	}
	if runtimeBackend != "docker" && runtimeBackend != "podman" && runtimeBackend != "bare-metal" {
		fmt.Fprintf(stderr, "check-gpu: unknown SPARKY_RUNTIME_BACKEND %q (want docker, podman or bare-metal)\n", runtimeBackend)
		return 2
	}
	dockerHost := lookup("DOCKER_HOST")
	probeImage := *image
	if probeImage == "" {
		probeImage = lookup("SPARKY_GPU_CHECK_IMAGE")
	}

	be, err := newBackend(runtimeBackend, dockerHost)
	if err != nil {
		fmt.Fprintf(stderr, "check-gpu: cannot create the %s backend: %v\n", runtimeBackend, err)
		return 2
	}
	if closer, ok := be.(io.Closer); ok {
		defer closer.Close()
	}

	daemon := dockerHost
	if daemon == "" {
		daemon = "(default)"
	}
	fmt.Fprintf(stdout, "sparky-agent check-gpu\n  runtime backend:  %s\n", runtimeBackend)
	if runtimeBackend != "bare-metal" {
		fmt.Fprintf(stdout, "  container daemon: %s\n  probe image:      %s\n", daemon, orDefault(probeImage, gpucheck.DefaultImage))
		fmt.Fprintf(stdout, "  starting a probe container (this pulls the image if it is not present)...\n")
	}

	res := gpucheck.Run(context.Background(), be, runtimeBackend, gpucheck.Options{Image: probeImage, Timeout: *timeout})

	switch res.Status {
	case gpucheck.StatusOK:
		fmt.Fprintf(stdout, "\nOK: %s\n", res.Detail)
		if res.Output != "" {
			fmt.Fprintf(stdout, "%s\n", indent(res.Output))
		}
		return 0
	case gpucheck.StatusNotApplicable:
		fmt.Fprintf(stdout, "\nNot applicable: %s\n", res.Detail)
		return 0
	default:
		fmt.Fprintf(stdout, "\nFAILED: %s\n", res.Detail)
		if res.Output != "" {
			fmt.Fprintf(stdout, "  probe output:\n%s\n", indent(res.Output))
		}
		if res.Hint != "" {
			fmt.Fprintf(stdout, "  hint: %s\n", res.Hint)
		}
		return 1
	}
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func indent(s string) string {
	return "    " + strings.ReplaceAll(s, "\n", "\n    ")
}

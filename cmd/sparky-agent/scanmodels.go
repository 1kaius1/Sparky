// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"text/tabwriter"

	"github.com/1kaius1/Sparky/agent/modelscan"
)

// scanModelsDefaultPath duplicates agent/config's bare-metal default for
// SPARKY_MODEL_STORAGE_PATH rather than importing it (unexported there, and
// this subcommand deliberately avoids config.Load - see runScanModels).
const scanModelsDefaultPath = "/opt/sparky/serviceloop/models"

// runScanModels implements `sparky-agent scan-models [--path DIR] [--json]`:
// list the model copies found under the node's model storage directory. It
// is read-only and talks to nothing - it cannot say which of these the
// central app already knows about (the Inventory page's scan does that).
//
// Dispatched before config.Load(), like setup: an operator running this by
// hand as the serviceloop account does not have secrets.env's variables in
// their shell, and a storage-directory listing needs none of them.
func runScanModels(args []string) int {
	return scanModels(args, os.Getenv, os.Stdout, os.Stderr)
}

func scanModels(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("scan-models", flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := fs.String("path", "", "model storage directory (default: $SPARKY_MODEL_STORAGE_PATH, else "+scanModelsDefaultPath+")")
	asJSON := fs.Bool("json", false, "print machine-readable JSON instead of a table")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	root := *path
	if root == "" {
		root = getenv("SPARKY_MODEL_STORAGE_PATH")
	}
	if root == "" {
		root = scanModelsDefaultPath
	}

	cands, truncated, err := modelscan.Scan(root)
	if err != nil {
		fmt.Fprintf(stderr, "scan-models: %v\n", err)
		return 1
	}

	if *asJSON {
		type out struct {
			StoragePath string                `json:"storage_path"`
			Truncated   bool                  `json:"truncated"`
			Models      []modelscan.Candidate `json:"models"`
		}
		if cands == nil {
			cands = []modelscan.Candidate{}
		}
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(out{StoragePath: root, Truncated: truncated, Models: cands}); err != nil {
			fmt.Fprintf(stderr, "scan-models: %v\n", err)
			return 1
		}
		return 0
	}

	if len(cands) == 0 {
		fmt.Fprintf(stdout, "No models found under %s\n", root)
		return 0
	}
	tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "MODEL\tFORMAT\tQUANTIZATION\tSIZE\tNOTE")
	for _, c := range cands {
		quant := c.Quantization
		if quant == "" {
			quant = "(whole repo)"
		}
		note := ""
		if c.PossiblyIncomplete {
			note = "possibly incomplete (rsync temp file present)"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", c.ModelRef, c.Format, quant, humanBytes(c.SizeBytes), note)
	}
	if err := tw.Flush(); err != nil {
		fmt.Fprintf(stderr, "scan-models: %v\n", err)
		return 1
	}
	if truncated {
		fmt.Fprintf(stderr, "scan-models: listing truncated at %d models\n", modelscan.MaxCandidates)
	}
	return 0
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

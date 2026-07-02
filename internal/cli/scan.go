package cli

import (
	"context"
	"fmt"
	"io"
	"net/http"

	"github.com/RoninForge/akashi/internal/probe"
	"github.com/RoninForge/akashi/internal/registry"
	"github.com/RoninForge/akashi/internal/scan"
	"github.com/spf13/cobra"
)

func newScanCmd(stdout, stderr io.Writer) *cobra.Command {
	var opts scan.Options
	cmd := &cobra.Command{
		Use:   "scan",
		Short: "Drain the MCP registry and census every server's health",
		Long: `akashi scan runs the same keyless check set as "akashi check" against every
server in the official MCP registry and writes a dated dataset.

It writes two files into --out:

  records.jsonl   one probe result per server, one JSON line each (the same
                   fields "akashi check <server> --json" prints)
  summary.json    verdict counts and rates, a remote-bearing segment, the
                   reproducibility parameters for this run, and a non-fatal
                   nameIssues report on registry names that would not survive
                   per-server page routing

A scan resumes automatically: if --out already has a records.jsonl from a
previous run, servers already recorded in it are not re-probed. Interrupt
and rerun with the same --out to pick up where it left off.`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			ctx := context.Background()
			opts.Progress = stderr

			client := registry.NewClient()

			eng := probe.NewEngine()
			eng.GitHubToken = githubToken(ctx)
			eng.HTTP = &http.Client{Transport: scan.NewGitHubBackoffTransport(nil)}

			summary, err := scan.Run(ctx, client, eng, opts)
			if err != nil {
				return err
			}
			fmt.Fprintf(stdout, "wrote %s: %d servers (%d healthy, %d degraded, %d dead, %d unknown)\n",
				opts.Out, summary.Overall.Total,
				summary.Overall.Counts[probe.Healthy], summary.Overall.Counts[probe.Degraded],
				summary.Overall.Counts[probe.Dead], summary.Overall.Counts[probe.Unknown])
			return nil
		},
	}

	cmd.Flags().StringVar(&opts.Out, "out", "", "output directory for records.jsonl and summary.json (required)")
	cmd.Flags().IntVar(&opts.Limit, "limit", 0, "cap the number of servers drained from the registry (0 = the whole registry)")
	cmd.Flags().IntVar(&opts.Concurrency, "concurrency", scan.DefaultConcurrency, "number of servers probed in parallel")
	cmd.Flags().DurationVar(&opts.Timeout, "timeout", scan.DefaultTimeout, "time budget for one server's full probe set")
	_ = cmd.MarkFlagRequired("out")
	return cmd
}

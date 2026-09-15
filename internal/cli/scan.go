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

It writes into --out:

  records/        the census, sharded into 64 JSONL files, one probe result
                   per server per line (the same fields "akashi check
                   <server> --json" prints). A namespace is never split, so
                   one operator's servers are always in one shard.
  records/manifest.json
                  the shard listing: per-shard counts, the total, and a
                   namespace-to-shard map, so a consumer can enumerate the
                   census or fetch one operator without a directory listing
  summary.json    verdict counts and rates, a remote-bearing segment, the
                   reproducibility parameters for this run, and a non-fatal
                   nameIssues report on registry names that would not survive
                   per-server page routing

Sharding exists because a single records.jsonl crossed GitHub's 50MiB warning
at 31,538 servers and the 100MiB hard push limit lands near 47,700 at the same
bytes per server. Censuses published before sharding keep their single
records.jsonl and are still read.

With --compare pointing at a previous census, a second pass re-probes every
namespace whose aggregate signals moved against that edition and writes a
third file:

  reprobe.jsonl   the second reading for each re-probed server, in the same
                   shape as a records shard

A long scan is not a snapshot: over a window of hours one operator's outage
lands in the data as if it were a property of its servers. The second pass is
what tells the two apart. It NEVER edits the census shards, which stay exactly
as first observed; the second reading is evidence recorded beside them, and
summary.json carries what it found.

A scan resumes automatically: if --out already holds records from a previous
run, in either layout, servers already recorded are not re-probed. Interrupt
and rerun with the same --out to pick up where it left off.`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			ctx := context.Background()
			opts.Progress = stderr

			client := registry.NewClient()

			eng := probe.NewEngine()
			eng.GitHubToken = githubToken(ctx)
			// Keep NewEngine's whole-request backstop when swapping in the
			// GitHub backoff transport; replacing the client wholesale is how
			// the census lost it and hung on a context-free SDK teardown.
			eng.HTTP = &http.Client{
				Transport: scan.NewGitHubBackoffTransport(nil),
				Timeout:   probe.HTTPClientTimeout,
			}

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

	cmd.Flags().StringVar(&opts.Out, "out", "", "output directory for records/ and summary.json (required)")
	cmd.Flags().IntVar(&opts.Limit, "limit", 0, "cap the number of servers drained from the registry (0 = the whole registry)")
	cmd.Flags().IntVar(&opts.Concurrency, "concurrency", scan.DefaultConcurrency, "number of servers probed in parallel")
	cmd.Flags().DurationVar(&opts.Timeout, "timeout", scan.DefaultTimeout, "time budget for one server's full probe set")
	cmd.Flags().StringVar(&opts.Compare, "compare", "", "previous census dir (or its records/ or records.jsonl) to re-probe drifting namespaces against")
	cmd.Flags().Float64Var(&opts.ReprobeThreshold, "reprobe-threshold", scan.DefaultReprobeThreshold, "percentage points a namespace's signal rate must move to earn a second reading")
	cmd.Flags().IntVar(&opts.ReprobeMinServers, "reprobe-min-servers", scan.DefaultReprobeMinServers, "ignore namespaces smaller than this when looking for drift")
	cmd.Flags().IntVar(&opts.ReprobeMaxServers, "reprobe-max-servers", scan.DefaultReprobeMaxServers, "cap how many servers the second pass re-probes")
	_ = cmd.MarkFlagRequired("out")
	return cmd
}

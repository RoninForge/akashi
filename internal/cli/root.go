// Package cli builds the cobra command tree. Keeping the tree in a dedicated
// package (rather than inside cmd/akashi) makes it testable without spawning a
// binary.
package cli

import (
	"errors"
	"fmt"
	"io"

	"github.com/RoninForge/akashi/internal/version"
	"github.com/spf13/cobra"
)

// ErrUnhealthy is returned by check when a probed server is not healthy. It is
// distinct from cobra usage errors so Execute can map it to exit code 1 rather
// than 2.
var ErrUnhealthy = errors.New("server not healthy")

// Execute is the single entry point cmd/akashi/main.go calls. It returns the
// process exit code rather than calling os.Exit, so main stays testable.
//
// Exit codes:
//
//	0 - healthy
//	1 - degraded, dead, or unknown (a real health finding)
//	2 - invocation error (bad flags, or the registry lookup itself failed).
//	    Probe-time network failures fold into the verdict (0/1), not this code.
func Execute(stdout, stderr io.Writer, args []string) int {
	root := newRoot(stdout, stderr)
	root.SetArgs(args)
	if err := root.Execute(); err != nil {
		if errors.Is(err, ErrUnhealthy) {
			// The pretty report already explained the verdict; adding a
			// cobra-style error line would be redundant noise in CI logs.
			return 1
		}
		fmt.Fprintln(stderr, "Error:", err)
		return 2
	}
	return 0
}

func newRoot(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "akashi",
		Short: "Verify an MCP server is alive and conformant, keyless",
		Long: `akashi probes an MCP server with only public, unauthenticated signals -
repository liveness, package publication, and a capability-only MCP
initialize handshake - and reports whether it is healthy, degraded, or
dead. It emits an embeddable "verified on DATE" badge for the healthy ones.

It never authenticates to a probed server and never runs a tool, so the
zero-key pledge holds by construction.

The name means "proof" or "certificate" (証) in Japanese.`,
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       formatVersion(),
	}

	cmd.SetOut(stdout)
	cmd.SetErr(stderr)

	cmd.AddCommand(newCheckCmd(stdout, stderr))
	cmd.AddCommand(newVersionCmd(stdout))

	return cmd
}

func formatVersion() string {
	v := version.Get()
	return fmt.Sprintf("%s (commit %s, built %s)", v.Version, v.Commit, v.BuildDate)
}

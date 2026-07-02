package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/RoninForge/akashi/internal/probe"
	"github.com/RoninForge/akashi/internal/registry"
	"github.com/RoninForge/akashi/internal/report"
	"github.com/spf13/cobra"
)

type checkFlags struct {
	json    bool
	badge   bool
	color   bool
	timeout time.Duration
}

func newCheckCmd(stdout, stderr io.Writer) *cobra.Command {
	var flags checkFlags
	cmd := &cobra.Command{
		Use:   "check <server>",
		Short: "Probe one MCP server and report its health",
		Long: `akashi check resolves <server> and runs the keyless check set against it.

<server> may be:
  - an official-registry server name   (e.g. io.github.owner/name)
  - a GitHub repository URL            (e.g. https://github.com/owner/repo)
  - a remote endpoint URL              (e.g. https://mcp.example.com/sse)

It prints a per-check pass/warn/fail table and an overall verdict. Use
--json for the machine-readable result row, or --badge for a shields.io
endpoint badge.

Exit code: 0 healthy, 1 degraded/dead/unknown, 2 invocation error (bad
target, or the registry lookup itself failed).`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if flags.json && flags.badge {
				return fmt.Errorf("choose one of --json or --badge, not both")
			}

			ctx, cancel := context.WithTimeout(context.Background(), flags.timeout)
			defer cancel()

			client := registry.NewClient()
			server, err := client.Resolve(ctx, args[0])
			if err != nil {
				return err
			}

			eng := probe.NewEngine()
			eng.GitHubToken = githubToken(ctx)
			result := eng.ProbeServer(ctx, *server)

			if err := emit(stdout, result, flags); err != nil {
				return err
			}
			if !result.OK() {
				return ErrUnhealthy
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&flags.json, "json", false, "emit the full result as JSON instead of the pretty table")
	cmd.Flags().BoolVar(&flags.badge, "badge", false, "emit a shields.io endpoint badge JSON")
	cmd.Flags().BoolVar(&flags.color, "color", ttyColor(), "colorize the pretty output (auto-detected from TTY)")
	cmd.Flags().DurationVar(&flags.timeout, "timeout", 60*time.Second, "overall time budget for all probes")
	return cmd
}

func emit(stdout io.Writer, r probe.Result, flags checkFlags) error {
	switch {
	case flags.json:
		return report.WriteJSON(stdout, r)
	case flags.badge:
		return report.WriteBadge(stdout, r)
	default:
		report.WritePretty(stdout, r, flags.color)
		return nil
	}
}

// githubToken discovers a token to raise the GitHub API rate limit. It is used
// ONLY against the public GitHub API as a measurement instrument, never sent to
// a probed server. Order: GITHUB_TOKEN, GH_TOKEN, then the local gh CLI. An
// empty result means unauthenticated GitHub API (60 requests/hour).
func githubToken(ctx context.Context) string {
	for _, env := range []string{"GITHUB_TOKEN", "GH_TOKEN"} {
		if v := strings.TrimSpace(os.Getenv(env)); v != "" {
			return v
		}
	}
	if _, err := exec.LookPath("gh"); err != nil {
		return ""
	}
	c, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(c, "gh", "auth", "token").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// ttyColor reports whether stdout is a terminal, so color defaults sensibly
// without a dependency.
func ttyColor() bool {
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}

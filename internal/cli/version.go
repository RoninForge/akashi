package cli

import (
	"fmt"
	"io"

	"github.com/RoninForge/akashi/internal/version"
	"github.com/spf13/cobra"
)

func newVersionCmd(stdout io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version, commit, and build date",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			v := version.Get()
			fmt.Fprintf(stdout, "akashi %s\ncommit %s\nbuilt  %s\n", v.Version, v.Commit, v.BuildDate)
			return nil
		},
	}
}

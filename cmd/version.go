package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

// Set via -ldflags at release time; see .github/workflows/release-plugin.yml.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

// Version is the plugin's version string. main reports it to cub in the
// install-hook manifest, and cub passes it back as CUB_PLUGIN_PREVIOUS_VERSION
// on the next upgrade.
func Version() string { return version }

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the plugin version",
		RunE: func(c *cobra.Command, _ []string) error {
			fmt.Fprintf(c.OutOrStdout(), "eksinf %s\n  commit: %s\n  built:  %s\n", version, commit, date)
			return nil
		},
	}
}

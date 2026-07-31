package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// Flags shared by every subcommand that talks to AWS or a cluster.
var (
	flagVariant string
	flagProfile string
)

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "eksinf",
		Short: "Administer the eks-inference stack",
		Long: `Administer the eks-inference stack: an EKS cluster for inference workloads,
provisioned from a local kind cluster through AWS Controllers for Kubernetes and
managed as data in ConfigHub.

The stack spans two apply planes. The mgmt plane runs on the kind cluster and
creates AWS infrastructure; the workload plane runs on the EKS cluster once it is
enrolled. Nothing moves between them.

Build-time operations (render, guard, bundle) are deliberately absent: they need
the source tree, helm, and GNU tar, and only ever run in the eks-inference repo
or its CI. This tool covers what a consumer of the stack does.`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	root.PersistentFlags().StringVar(&flagVariant, "variant", "dev",
		"ConfigHub variant to operate on")
	root.PersistentFlags().StringVar(&flagProfile, "profile", "",
		"AWS profile to use (default: ambient credentials)")

	root.AddCommand(
		newStatusCmd(),
		newComponentsCmd(),
		newVersionCmd(),
	)
	return root
}

// Execute runs the command tree. main calls this after handing over the
// embedded component manifest.
func Execute(componentsData []byte) {
	if err := LoadComponents(componentsData); err != nil {
		fmt.Fprintf(os.Stderr, "eksinf: %v\n", err)
		os.Exit(1)
	}
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "eksinf: %v\n", err)
		os.Exit(1)
	}
}

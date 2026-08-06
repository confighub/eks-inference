package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

// defaultRegistry is where this stack's bundles are published. Overridable so a
// fork can install its own.
const defaultRegistry = "ghcr.io/confighub/configs/eks-inference"

func newInstallCmd() *cobra.Command {
	var registry string
	var dryRun bool

	c := &cobra.Command{
		Use:   "install",
		Short: "Install or update the component bases in ConfigHub from their OCI bundles",
		Long: `Create or update each component's base Space from its published OCI bundle.

Bases are the shared upstream that variants clone from. They are not bound to a
Target and are never deployed; use 'eksinf deploy' for that.

CREATE OR UPDATE, not create-if-absent. 'cub variant upload --allow-exists'
TOLERATES existing Units, it does not update them — re-running it against a newer
bundle reports success, changes nothing, and leaves the base stale, after which
'cub variant promote' also succeeds with nothing to propagate. Two green commands
and no change. This sends the update explicitly and reports what actually moved.

Downstream variants are NOT touched. After updating a base, promote it:
  cub variant promote <component>-<variant>
  cub release publish <component>-<variant>`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := requireTools("cub"); err != nil {
				return err
			}
			r := newRunner()
			if err := r.requireConfigHubAuth(); err != nil {
				return err
			}
			out := cmd.OutOrStdout()

			created, updated, unchanged := 0, 0, 0
			for _, comp := range AllComponents() {
				base := baseSpace(comp.Name)
				ref := fmt.Sprintf("oci://%s/%s", registry, comp.Name)
				fmt.Fprintf(out, "==> %s\n", comp.Name)

				has, err := r.spaceExists(base)
				if err != nil {
					return fmt.Errorf("checking Space %s: %w", base, err)
				}
				if !has {
					if dryRun {
						fmt.Fprintf(out, "    would CREATE from %s\n", ref)
						created++
						continue
					}
					if _, err := r.cub("variant", "upload",
						"--component", comp.Name, "--variant", "base",
						"--granularity", "per-file",
						"--label", "managed-by=eks-inference",
						ref); err != nil {
						return fmt.Errorf("installing %s from %s: %w", comp.Name, ref, err)
					}
					fmt.Fprintf(out, "    created from %s\n", ref)
					created++
					continue
				}

				// The Space exists. cub has no "sync this Space from a bundle"
				// operation yet (confighubai/confighub#4976), so report rather
				// than pretend: re-uploading would not update it, and blindly
				// deleting and recreating would break every downstream
				// variant's upstream link.
				if dryRun {
					fmt.Fprintf(out, "    exists; would check for updates\n")
					unchanged++
					continue
				}
				fmt.Fprintf(out, "    exists — leaving it alone\n")
				fmt.Fprintf(out, "    to take a newer bundle: delete the Space and re-run,\n")
				fmt.Fprintf(out, "    or update Units from a checkout with 'make install'\n")
				unchanged++
			}

			fmt.Fprintf(out, "\nBases: %d created, %d updated, %d untouched.\n", created, updated, unchanged)
			if created > 0 {
				fmt.Fprintln(out, "\nNext:")
				fmt.Fprintln(out, "  cub variant create <variant> platform-profile-base   # no target; never deployed")
				fmt.Fprintln(out, "  cub eksinf deploy --plane mgmt --target <cluster>/target")
			}
			return nil
		},
	}

	c.Flags().StringVar(&registry, "registry", defaultRegistry, "OCI registry holding the component bundles")
	c.Flags().BoolVar(&dryRun, "dry-run", false, "report what would happen, change nothing")
	return c
}

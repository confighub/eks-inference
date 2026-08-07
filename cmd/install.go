package cmd

import (
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

// defaultRegistry is where this stack's bundles are published. Overridable so a
// fork can install its own.
const defaultRegistry = "ghcr.io/confighub/configs/eks-inference"

func newInstallCmd() *cobra.Command {
	var registry string
	var dryRun, recreate bool

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
				// operation yet (confighubai/confighub#4976). Until it does,
				// the only honest way to take a newer bundle is to delete the
				// base and upload it again — which is what --recreate does.
				if recreate {
					// Refuse while any downstream still points at this base.
					// Deleting it would break their upstream links, and a
					// variant whose upstream is gone can never be promoted
					// again — a worse state than being out of date.
					downstreams, err := r.variantsOf(comp.Name)
					if err != nil {
						return err
					}
					if len(downstreams) > 0 {
						return fmt.Errorf(
							"%s still has downstream variant(s): %s\n"+
								"Deleting the base would orphan them permanently. Remove them first:\n"+
								"  cub eksinf teardown --delete-config",
							base, strings.Join(downstreams, ", "))
					}
					if dryRun {
						fmt.Fprintf(out, "    would DELETE and re-upload from %s\n", ref)
						updated++
						continue
					}
					if _, err := r.cub("space", "delete", base, "--recursive"); err != nil {
						return fmt.Errorf("deleting base Space %s: %w", base, err)
					}
					if _, err := r.cub("variant", "upload",
						"--component", comp.Name, "--variant", "base",
						"--granularity", "per-file",
						"--label", "managed-by=eks-inference",
						ref); err != nil {
						return fmt.Errorf("re-uploading %s from %s: %w", comp.Name, ref, err)
					}
					fmt.Fprintf(out, "    recreated from %s\n", ref)
					updated++
					continue
				}
				if dryRun {
					fmt.Fprintf(out, "    exists; would leave it alone (--recreate to refresh)\n")
					unchanged++
					continue
				}
				fmt.Fprintf(out, "    exists — leaving it alone\n")
				fmt.Fprintf(out, "    to take a newer bundle: cub eksinf install --recreate\n")
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
	c.Flags().BoolVar(&recreate, "recreate", false,
		"delete and re-upload bases that already exist, to take a newer bundle")
	return c
}

// variantsOf lists the downstream variant Spaces of a component's base.
//
// Spaces are named <component>-<variant>, so the base is one of the matches and
// has to be excluded. This drives a refusal rather than a warning: a variant
// whose upstream Space has been deleted cannot be promoted again, and that is
// not recoverable by re-uploading the base — the new Units have new IDs.
func (r *runner) variantsOf(component string) ([]string, error) {
	out, err := r.cub("space", "list", "--no-headers")
	if err != nil {
		return nil, fmt.Errorf("listing Spaces: %w", err)
	}
	base := baseSpace(component)
	var found []string
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		name := fields[0]
		if name != base && strings.HasPrefix(name, component+"-") {
			found = append(found, name)
		}
	}
	sort.Strings(found)
	return found, nil
}

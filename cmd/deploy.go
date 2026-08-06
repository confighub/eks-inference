package cmd

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
)

// noCredsCheck opts out of the pre-deploy credentials check, for deploying the
// config deliberately ahead of having an AWS identity to hand.
var noCredsCheck bool

// requireCredsSecret refuses to deploy the mgmt plane into a cluster that has
// no AWS credentials.
//
// Deploying first is not harmful, just consistently worse: the controllers
// CrashLoop, or come up and reconcile against an API refusing them, and because
// the SDK reads the file once at startup a later `creds` write needs a restart
// to take effect. Every one of those failure modes reads like the stack is
// broken. This turns it into one sentence, before anything is created.
func (r *runner) requireCredsSecret(target string, w io.Writer) error {
	// Derive the cluster from --target rather than from discovery. Discovery
	// identifies the management cluster by its ack-system namespace, which does
	// not exist before the first deploy — precisely the moment this check is for
	// — and on a machine with several cub-managed clusters there is nothing to
	// single one out. --target names it outright: `cub cluster up --name X`
	// creates Space X holding target X/target, so the Space IS the cluster.
	mgmt, _, ok := strings.Cut(target, "/")
	if !ok || mgmt == "" {
		return nil
	}
	kc := cubClusterKubeconfig(mgmt)
	if !r.reachable(kc) {
		// Not a cub-cluster target, or its kubeconfig is gone. Either way this
		// check cannot speak to it, and refusing on that basis would block a
		// deploy for a reason that has nothing to do with credentials.
		return nil
	}
	if _, err := r.kubectl(kc, "get", "secret", credsSecret, "-n", ackNamespace); err != nil {
		return fmt.Errorf(
			"no %s/%s Secret in cluster %q.\n"+
				"The ACK controllers read credentials once at startup, so write them first:\n"+
				"  cub eksinf creds use-existing      (reuse your AWS identity)\n"+
				"  cub eksinf creds create-user       (a dedicated, revocable IAM user)\n"+
				"Pass --no-creds-check to deploy the config anyway",
			ackNamespace, credsSecret, mgmt)
	}
	fmt.Fprintf(w, "  credentials present in %s\n\n", mgmt)
	return nil
}

func newDeployCmd() *cobra.Command {
	var planeFlag, target string
	var dryRun bool

	c := &cobra.Command{
		Use:   "deploy",
		Short: "Create downstream variants of a plane's components and publish their releases",
		Long: `Deploy one plane: create a downstream variant of each of its components,
bound to a Target, and publish each variant's release.

ORDERING BETWEEN PLANES IS YOURS TO ENFORCE. Deploy mgmt, let it converge, then
deploy workload. Nothing here waits, and nothing can: karpenter-aws creates an
IAM role in the mgmt plane that the Karpenter controller assumes in the workload
plane, and no Argo sync wave can span two clusters.

Deploying does not scale any workload up. Everything in inference-workloads
ships at replicas: 0, so a deploy costs nothing until you ask for capacity.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			plane, err := ParsePlane(planeFlag)
			if err != nil {
				return err
			}
			if plane == PlaneHub {
				return fmt.Errorf(
					"the hub plane is never deployed — its Space has no Target and is never released.\n" +
						"Create its variant directly:  cub variant create <variant> platform-profile-base")
			}
			if target == "" {
				return fmt.Errorf("--target is required (as <space>/<target>)")
			}
			if err := requireTools("cub"); err != nil {
				return err
			}

			r := newRunner()
			if err := r.requireConfigHubAuth(); err != nil {
				return err
			}
			if plane == PlaneMgmt && !noCredsCheck && !dryRun {
				if err := r.requireCredsSecret(target, cmd.OutOrStdout()); err != nil {
					return err
				}
			}

			list := ComponentsInPlane(plane)
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "plane %q -> target %s, variant %q\n", plane, target, flagVariant)
			for _, comp := range list {
				fmt.Fprintf(out, "  %s\n", comp.Name)
			}
			fmt.Fprintln(out)

			if dryRun {
				fmt.Fprintln(out, "(dry run; nothing changed)")
				return nil
			}

			for _, comp := range list {
				base := baseSpace(comp.Name)
				down := variantSpace(comp.Name, flagVariant)
				fmt.Fprintf(out, "==> %s\n", comp.Name)

				hasBase, err := r.spaceExists(base)
				if err != nil {
					return fmt.Errorf("checking base Space %s: %w", base, err)
				}
				if !hasBase {
					fmt.Fprintf(out, "    SKIP: no base Space %q — run 'cub eksinf install' first\n", base)
					continue
				}

				hasDown, err := r.spaceExists(down)
				if err != nil {
					return fmt.Errorf("checking variant Space %s: %w", down, err)
				}
				if hasDown {
					fmt.Fprintf(out, "    variant %s already exists\n", down)
				} else {
					// --target also creates the Argo CD Application for a
					// cub-cluster target and republishes the apps Space, so
					// there is no separate wiring step.
					if _, err := r.cub("variant", "create", flagVariant, base, "--target", target); err != nil {
						return fmt.Errorf("creating variant %s: %w", down, err)
					}
					fmt.Fprintf(out, "    created variant %s\n", down)
				}

				changed, err := r.publishRelease(down)
				if err != nil {
					return fmt.Errorf("publishing release for %s: %w", down, err)
				}
				if changed {
					fmt.Fprintln(out, "    published release")
				} else {
					fmt.Fprintln(out, "    release already current")
				}
			}

			fmt.Fprintf(out, "\nDeployed plane %q. Argo pulls on its next reconcile.\n", plane)
			fmt.Fprintln(out, "Check with: cub eksinf status")
			return nil
		},
	}

	c.Flags().StringVar(&planeFlag, "plane", "", "plane to deploy (mgmt or workload)")
	c.Flags().StringVar(&target, "target", "", "target for the cloned Units, as <space>/<target>")
	c.Flags().BoolVar(&dryRun, "dry-run", false, "print what would be done, change nothing")
	c.Flags().BoolVar(&noCredsCheck, "no-creds-check", false,
		"deploy the mgmt plane without AWS credentials in the cluster")
	_ = c.MarkFlagRequired("plane")
	return c
}

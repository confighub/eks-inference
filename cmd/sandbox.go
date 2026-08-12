// Copyright (C) ConfigHub, Inc.
// SPDX-License-Identifier: MIT

package cmd

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"
)

// Config-only mode: build the entire configuration tree for this stack and
// create no infrastructure at all.
//
// WHY THIS IS POSSIBLE, and worth understanding before reading the code: almost
// nothing in this tool needs a cluster. `install` is pure ConfigHub. `deploy` is
// pure ConfigHub — see deployPlane. Even the credentials check degrades to a
// no-op when there is no reachable cluster, deliberately. The one thing `deploy`
// needs from the outside world is a Target to bind variants to, and a Target is
// itself just a ConfigHub entity: a Space, a server-hosted worker, and an OCI
// target, which enrollConfigHubSide already creates without touching a cluster.
//
// So this command is mostly assembly, not new machinery. That is the point it
// demonstrates: the stack is defined by its data, and the data can exist with
// nothing running.
//
// WHAT IS REAL HERE. Everything except a consumer. The Units are real, the links
// resolve for real, the placeholder gate really blocks a bad publish, and
// `cub release publish` really produces an OCI bundle in ConfigHub's registry
// that can be pulled and inspected. There is simply no Argo CD pulling it and no
// ACK controller acting on it. This is not a simulation of the config layer; it
// is the config layer, minus the cluster.
const (
	sandboxDefaultName    = "sandbox"
	sandboxDefaultVariant = "sandbox"
)

// sandboxMgmt and sandboxWorkload name the two Spaces that stand in for the two
// clusters. They hold a Target each and nothing else — the same shape
// `cub cluster up` and `eksinf enroll` produce, minus the cluster.
func sandboxMgmt(name string) string     { return name + "-mgmt" }
func sandboxWorkload(name string) string { return name + "-workload" }

func newSandboxCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "sandbox",
		Short: "Build the whole configuration with no infrastructure, for free",
		Long: `Create every Space, Unit, link and release this stack uses, and no cloud
resources whatsoever.

Nothing here costs money, needs an AWS account, or needs Docker. It takes about
half a minute. What you get is the real configuration tree: eight components,
their downstream variants across two planes, the platform-profile links that
carry shared values between them, the vet-placeholders gate, and a published
release per variant.

What you do NOT get is anything applying it. The Targets exist and the releases
publish to ConfigHub's OCI registry, but no Argo CD is subscribed and no ACK
controller is listening. Use it to read, edit, promote and break the config
without provisioning an EKS cluster to do it.

It defaults to variant "sandbox" rather than "dev", so it can live alongside a
real stack in the same organization without colliding.`,
	}
	c.AddCommand(newSandboxUpCmd(), newSandboxDownCmd())
	return c
}

func newSandboxUpCmd() *cobra.Command {
	var name string

	c := &cobra.Command{
		Use:   "up",
		Short: "Create the full configuration tree with no infrastructure",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := requireTools("cub"); err != nil {
				return err
			}
			// Default the variant to "sandbox" unless the caller asked for a
			// specific one. Sharing "dev" with a real stack would have the two
			// fighting over the same variant Spaces.
			if !cmd.Flags().Changed("variant") {
				flagVariant = sandboxDefaultVariant
			}
			r := newRunner()
			if err := r.requireConfigHubAuth(); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			return r.sandboxUp(out, name)
		},
	}
	c.Flags().StringVar(&name, "name", sandboxDefaultName, "prefix for the two Target Spaces")
	return c
}

// sandboxUp assembles the configuration. Each step prints the equivalent command
// before running it: the sequence IS the lesson, so narrating it is more useful
// than hiding it behind one opaque command.
func (r *runner) sandboxUp(out io.Writer, name string) error {
	mgmt, workload := sandboxMgmt(name), sandboxWorkload(name)

	// The bases are the one prerequisite. `install` is already free and creates
	// no infrastructure, so it stays a separate step rather than being folded in
	// — it is a real concept worth seeing on its own.
	missing := []string{}
	for _, comp := range AllComponents() {
		has, err := r.spaceExists(baseSpace(comp.Name))
		if err != nil {
			return fmt.Errorf("checking base Space for %s: %w", comp.Name, err)
		}
		if !has {
			missing = append(missing, comp.Name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf(
			"%d component base(s) missing (%v).\n"+
				"Install them first — it is also free and creates nothing:\n"+
				"  cub eksinf install",
			len(missing), missing)
	}

	// 1. The parameter surface. Its Space has no Target and is never released;
	// it exists to be linked FROM.
	profileSpace := variantSpace(profileComponent, flagVariant)
	hasProfile, err := r.spaceExists(profileSpace)
	if err != nil {
		return fmt.Errorf("checking Space %s: %w", profileSpace, err)
	}
	fmt.Fprintf(out, "==> the parameter surface\n")
	if hasProfile {
		fmt.Fprintf(out, "    %s already exists\n", profileSpace)
	} else {
		fmt.Fprintf(out, "    cub variant create %s %s\n", flagVariant, baseSpace(profileComponent))
		if _, err := r.cub("variant", "create", flagVariant, baseSpace(profileComponent)); err != nil {
			return fmt.Errorf("creating %s: %w", profileSpace, err)
		}
	}

	// 2. Two Spaces holding a Target each, standing in for the two clusters.
	// enrollConfigHubSide creates exactly this and touches no cluster: a Space,
	// a server-hosted OCI worker, an OCI target carrying the argo-apps-space
	// annotation, and the vet-placeholders Trigger on that target.
	for _, s := range []string{mgmt, workload} {
		fmt.Fprintf(out, "\n==> target Space %s (no cluster behind it)\n", s)
		o := &enrollOpts{name: s}
		if err := r.enrollConfigHubSide(o, out); err != nil {
			return err
		}
		// Mark it, so a human reading `cub space list` can tell at a glance that
		// nothing is going to apply this.
		if _, err := r.cub("space", "update", s, "--patch", "--label", "Mode=sandbox"); err != nil {
			return fmt.Errorf("labelling %s: %w", s, err)
		}
	}

	// 3. Both planes. Identical to what `eksinf deploy` runs against a real
	// cluster — same function, same links, same gate, same releases.
	for _, p := range []struct {
		plane  Plane
		target string
	}{
		{PlaneMgmt, mgmt + "/target"},
		{PlaneWorkload, workload + "/target"},
	} {
		fmt.Fprintf(out, "\n==> cub eksinf deploy --plane %s --target %s\n\n", p.plane, p.target)
		if err := r.deployPlane(out, p.plane, p.target); err != nil {
			return err
		}
	}

	fmt.Fprintf(out, "\nSandbox %q is up. No infrastructure exists and nothing is billing.\n\n", name)
	fmt.Fprintln(out, "The config is real. Things worth trying:")
	fmt.Fprintf(out, "  cub unit list --space %s\n", variantSpace("karpenter", flagVariant))
	fmt.Fprintf(out, "  cub eksinf link-profile --list                      # what the profile drives\n")
	fmt.Fprintf(out, "  cub unit edit --space %s nodepools   # then: cub release publish\n",
		variantSpace("karpenter", flagVariant))
	fmt.Fprintf(out, "  cub eksinf sandbox down --name %s\n", name)
	return nil
}

func newSandboxDownCmd() *cobra.Command {
	var name string
	var yes bool

	c := &cobra.Command{
		Use:   "down",
		Short: "Delete everything the sandbox created",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := requireTools("cub"); err != nil {
				return err
			}
			if !cmd.Flags().Changed("variant") {
				flagVariant = sandboxDefaultVariant
			}
			r := newRunner()
			if err := r.requireConfigHubAuth(); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if !yes {
				fmt.Fprintf(out, "This deletes the %q sandbox: variant %q plus Spaces %s and %s.\n",
					name, flagVariant, sandboxMgmt(name), sandboxWorkload(name))
				fmt.Fprintln(out, "No infrastructure is involved. Re-run with --yes.")
				return nil
			}
			return r.sandboxDown(out, name)
		},
	}
	c.Flags().StringVar(&name, "name", sandboxDefaultName, "prefix used at 'sandbox up' time")
	c.Flags().BoolVar(&yes, "yes", false, "proceed without confirmation")
	return c
}

// sandboxDeleteOrder returns the Spaces to delete, in an order that satisfies
// the reference constraints. Split out to be testable: getting this wrong does
// not fail loudly, it fails with an HTTP 400 naming a BridgeWorker rather than
// the Space actually holding the reference, and that has cost real time twice.
//
// The rules it encodes:
//   - a Space holding a Target cannot go while anything still references that
//     Target, so both Target Spaces are last;
//   - the apps Spaces release TO those Targets, so they precede them;
//   - the component variants are bound to those Targets, so they precede them too;
//   - the profile is linked FROM the component variants, so it follows them.
func sandboxDeleteOrder(name, variant string) []string {
	mgmt, workload := sandboxMgmt(name), sandboxWorkload(name)
	var order []string
	for _, comp := range AllComponents() {
		if comp.Name == profileComponent {
			continue
		}
		order = append(order, variantSpace(comp.Name, variant))
	}
	order = append(order, mgmt+"-argo-apps", workload+"-argo-apps")
	return append(order, variantSpace(profileComponent, variant), mgmt, workload)
}

// sandboxDown removes the sandbox in dependency order.
//
// The order is the same one that bites every hand-rolled teardown of this stack:
// a Space holding a Target cannot be deleted while anything still references
// that Target. So the variant Spaces and the apps Spaces go first, the profile
// after the variants that link to it, and the two Target Spaces last.
func (r *runner) sandboxDown(out io.Writer, name string) error {
	failed := false
	for _, s := range sandboxDeleteOrder(name, flagVariant) {
		has, err := r.spaceExists(s)
		if err != nil {
			return fmt.Errorf("checking Space %s: %w", s, err)
		}
		if !has {
			continue
		}
		// --recursive, not --recursive-force: a delete gate is somebody marking
		// config as protected on purpose, and overriding that should be a human
		// decision rather than something this command hard-codes.
		if _, err := r.cub("space", "delete", s, "--recursive"); err != nil {
			failed = true
			fmt.Fprintf(out, "  could not delete %s: %v\n", s, err)
			continue
		}
		fmt.Fprintf(out, "  deleted %s\n", s)
	}
	if failed {
		return fmt.Errorf("some Spaces remain (above); they must be gone before creating a sandbox of the same name")
	}

	fmt.Fprintln(out, "\nSandbox removed. The component bases are untouched — rebuild with 'cub eksinf sandbox up'.")
	return nil
}

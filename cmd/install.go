package cmd

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

// defaultRegistry is where this stack's bundles are published. Overridable so a
// fork can install its own.
const defaultRegistry = "ghcr.io/confighub/configs/eks-inference"

// stackOwner is the value of the well-known "Owner" Space label, which groups
// this stack's Spaces together in the component view.
//
// It is stamped on the component BASES and nowhere else, because that is
// sufficient: `cub variant create` inherits Component, Layer and Owner from the
// upstream Space. Every downstream variant therefore picks it up without this
// plugin setting it again, including variants somebody creates by hand with
// plain `cub variant create` and never tells the plugin about.
const stackOwner = "EKS Inference"

func newInstallCmd() *cobra.Command {
	var registry string
	var dryRun, recreate, prune bool

	c := &cobra.Command{
		Use:   "install",
		Short: "Install or update the component bases in ConfigHub from their OCI bundles",
		Long: `Create or update each component's base Space from its published OCI bundle.

Bases are the shared upstream that variants clone from. They are not bound to a
Target and are never deployed; use 'eksinf deploy' for that.

Run it again whenever the bundles are republished. A re-upload 3-way merges the
new bundle against the last one, so Unit IDs, target bindings and upstream links
survive, and anything changed in ConfigHub after the upload survives with them.
A bundle that has not moved is a no-op. Needs cub v0.2.14 or newer; older
versions have no re-upload and fail here with "already exists".

--prune additionally EMPTIES Units the bundle no longer produces. Off by default,
because emptying a base Unit propagates to every downstream that promotes from it
— which is right when a resource was genuinely dropped upstream, and wrong when
you are installing a partial bundle by mistake. Nothing is ever deleted either
way: the Unit record, its ID and its bindings survive being emptied.

--recreate DELETES each base and uploads it again. It is only for changing
granularity, which changes which Units exist and so cannot preserve links no
matter how it is done. For every other case a plain re-upload is correct and
non-destructive.

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
			if !recreate {
				// --recreate deletes first, so it uploads into an empty Space
				// and works on any cub. Only the re-upload path has a floor.
				if err := r.requireCubVersion(minCubVersion); err != nil {
					return err
				}
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

				// --recreate is for a granularity change and nothing else. It
				// destroys and rebuilds, which is the honest expression of that
				// operation: the Unit set itself changes, so links to the old
				// Units cannot survive by any means. Everything else goes
				// through the plain re-upload below.
				if recreate && has {
					downstreams, err := r.variantsOf(comp.Name)
					if err != nil {
						return err
					}
					if len(downstreams) > 0 {
						return fmt.Errorf(
							"%s still has downstream variant(s): %s\n"+
								"Deleting the base would orphan them permanently — rebuilding gives new\n"+
								"Unit IDs their links cannot follow. Remove them first:\n"+
								"  cub eksinf teardown --delete-config\n"+
								"If you only want a newer bundle, drop --recreate: a plain re-upload\n"+
								"updates in place and keeps the links.",
							base, strings.Join(downstreams, ", "))
					}
					if dryRun {
						fmt.Fprintf(out, "    would DELETE and rebuild from %s\n", ref)
						updated++
						continue
					}
					if _, err := r.cub("space", "delete", base, "--recursive"); err != nil {
						return fmt.Errorf("deleting base Space %s: %w", base, err)
					}
					has = false
				}

				// Create and re-upload are the SAME command: cub decides which
				// it is from the Space's state. There is nothing to branch on
				// here, and no create-if-absent race to lose.
				args := []string{"variant", "upload",
					"--component", comp.Name, "--variant", "base",
					"--granularity", baseGranularity,
					"--owner", stackOwner,
					"--label", "managed-by=eks-inference"}
				if dryRun {
					args = append(args, "--dry-run")
				}
				if prune {
					args = append(args, "--prune")
				}
				uploadOut, err := r.cub(append(args, ref)...)
				if err != nil {
					return fmt.Errorf("uploading %s from %s: %w", comp.Name, ref, err)
				}

				summary := uploadSummary(uploadOut)
				switch {
				case !has:
					fmt.Fprintf(out, "    created from %s\n", ref)
					created++
				case summary == unchangedSummary:
					fmt.Fprintln(out, "    already up to date")
					unchanged++
				default:
					if summary == "" {
						summary = "re-uploaded"
					}
					fmt.Fprintf(out, "    %s\n", summary)
					updated++
				}
				// cub records each re-upload in a ChangeSet and prints the
				// command that rolls it back. That is the most useful line in
				// its output and the easiest to lose in the noise, so lift it.
				if revert := revertCommand(uploadOut); revert != "" {
					fmt.Fprintf(out, "    revert: %s\n", revert)
				}
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
	c.Flags().BoolVar(&prune, "prune", false,
		"also empty Units the bundle no longer produces")
	c.Flags().BoolVar(&recreate, "recreate", false,
		"delete and rebuild each base; only needed to change granularity")
	return c
}

// variantsOf lists the downstream variant Spaces of a component's base.
//
// Selected by the Component LABEL, not by name prefix. Component names overlap
// — "karpenter" is a prefix of "karpenter-aws" — so matching on the name makes
// karpenter-aws-base look like a downstream variant of karpenter-base, and
// --recreate then refuses to touch a base that has no downstreams at all. The
// label is what actually identifies a component; the naming is a convention on
// top of it.
//
// This drives a refusal rather than a warning: a variant whose upstream Space
// has been deleted cannot be promoted again, and re-uploading the base does not
// repair it because the new Units have new IDs.
func (r *runner) variantsOf(component string) ([]string, error) {
	out, err := r.cub("space", "list", "--no-headers",
		"--where", fmt.Sprintf("Labels.Component = '%s'", component))
	if err != nil {
		return nil, fmt.Errorf("listing Spaces for component %s: %w", component, err)
	}
	base := baseSpace(component)
	var found []string
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if name := fields[0]; name != base {
			found = append(found, name)
		}
	}
	sort.Strings(found)
	return found, nil
}

// baseGranularity is the Unit model for every base Space.
//
// per-file makes each file in the bundle one Unit, so file names in configs/
// are an interface. cub now refuses a re-upload at a different granularity —
// which is the right answer, since changing it changes which Units exist and
// therefore breaks every link and target binding pointing at the old ones.
const baseGranularity = "per-file"

// unchangedSummary is what uploadSummary returns for a bundle that has not moved.
const unchangedSummary = "already up to date"

// uploadSummary reduces cub's upload output to one line.
//
// This parses, so it is written to degrade rather than break: an unrecognised
// output yields "", the caller falls back to a generic message, and nothing
// depends on the wording being stable. The alternative — asking ConfigHub what
// changed — costs an extra round trip per component to phrase a status line.
func uploadSummary(out string) string {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "Already up to date"):
			return unchangedSummary
		case strings.HasPrefix(line, "Re-uploaded "):
			// "Re-uploaded <space>: 1 created, 1 updated, 0 emptied"
			if _, counts, ok := strings.Cut(line, ": "); ok {
				return counts
			}
		}
	}
	return ""
}

// revertCommand extracts the rollback command cub prints for a re-upload.
//
// Worth surfacing rather than leaving in the noise: it is the only way back
// from a bad bundle, and it is scoped to the exact ChangeSet the upload made.
func revertCommand(out string) string {
	const prefix = "Revert with: "
	for _, line := range strings.Split(out, "\n") {
		if line = strings.TrimSpace(line); strings.HasPrefix(line, prefix) {
			return strings.TrimPrefix(line, prefix)
		}
	}
	return ""
}

// minCubVersion is a HARD FLOOR for install, for the same reason the Argo CD
// version is one for enroll: below it the command cannot work, and the way it
// fails does not say so. cub gained re-upload in v0.2.14; older builds cannot
// upload into a populated Space and fail with "already exists", which reads
// like a bug in this plugin rather than a version skew.
//
// The plugin installs from GitHub releases and upgrades independently of cub,
// so the skew is ordinary rather than exotic.
const minCubVersion = "v0.2.14"

// requireCubVersion fails when the cub on PATH is older than min.
//
// An unparseable version — a dev build reports "dev" — is allowed through
// deliberately. Refusing to run against a locally built cub would block the
// people most likely to be testing an unreleased one, and the failure it
// guards against is loud and immediate anyway.
func (r *runner) requireCubVersion(min string) error {
	out, err := r.cub("version")
	if err != nil {
		return fmt.Errorf("reading cub version: %w", err)
	}
	got := parseCubVersion(out)
	if got == nil {
		return nil
	}
	if compareVersions(got, mustParseVersion(min)) < 0 {
		return fmt.Errorf(
			"this needs cub %s or newer for re-upload; found %v.%v.%v.\n"+
				"Older versions cannot upload into a Space that already has Units and fail\n"+
				"with \"already exists\". Upgrade cub, or use --recreate to rebuild the bases.",
			min, got[0], got[1], got[2])
	}
	return nil
}

// parseCubVersion pulls the client semver out of `cub version` output, or nil
// when there isn't one to find.
func parseCubVersion(out string) []int {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		rest, ok := strings.CutPrefix(line, "Version:")
		if !ok {
			continue
		}
		if v := mustParseVersionOrNil(strings.TrimSpace(rest)); v != nil {
			return v
		}
	}
	return nil
}

func mustParseVersionOrNil(s string) []int {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	parts := strings.SplitN(s, ".", 3)
	if len(parts) != 3 {
		return nil
	}
	out := make([]int, 3)
	for i, p := range parts {
		// Tolerate a pre-release or build suffix on the patch component.
		p = strings.FieldsFunc(p, func(r rune) bool { return r == '-' || r == '+' })[0]
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil
		}
		out[i] = n
	}
	return out
}

func mustParseVersion(s string) []int {
	v := mustParseVersionOrNil(s)
	if v == nil {
		panic("unparseable version constant: " + s)
	}
	return v
}

func compareVersions(a, b []int) int {
	for i := range a {
		switch {
		case a[i] < b[i]:
			return -1
		case a[i] > b[i]:
			return 1
		}
	}
	return 0
}

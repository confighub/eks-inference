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
A bundle that has not moved is a no-op. Needs cub v0.4.17 or newer: from that
release an upload goes through the server and makes one Unit per resource,
which is what this plugin's links rely on.

--prune lets an upload EMPTY Units whose resources the bundle no longer
contains. Without it such an upload stops and says so, because emptying a base
Unit propagates to every downstream that promotes from it, which is right when
a resource was genuinely dropped upstream and wrong when you are installing a
partial bundle by mistake. Nothing is ever deleted either way: the Unit record,
its ID and its bindings survive being emptied.

--recreate DELETES each base and uploads it again. It is for bases installed by
a plugin release before 0.13, which made one Unit per file: their Units are not
the ones the links now look for, and that cannot be changed in place. For every other case a plain re-upload is correct and
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

				// --recreate is for bases uploaded one Unit per file, before
				// 0.13, and nothing else. It destroys and rebuilds because the
				// Unit set itself changes, so links to the old Units cannot
				// survive by any means. Everything else goes through the plain
				// re-upload below.
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
					"--owner", stackOwner,
					"--space-label", "managed-by=eks-inference"}
				if dryRun {
					args = append(args, "--dry-run")
				}
				if prune {
					// cub asks before emptying a Unit; --prune is the answer.
					args = append(args, "--yes")
				}
				uploadOut, err := r.cub(append(args, ref)...)
				if err != nil {
					if strings.Contains(err.Error(), "confirmation") || strings.Contains(err.Error(), "refusing to empty") {
						return fmt.Errorf("uploading %s from %s: the bundle no longer contains some resources this base has; to empty their Units, run again with --prune: %w", comp.Name, ref, err)
					}
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
		"let the upload empty Units whose resources the bundle no longer contains")
	c.Flags().BoolVar(&recreate, "recreate", false,
		"delete and rebuild each base; needed once for bases installed before 0.13")
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

// unchangedSummary is what uploadSummary returns for a bundle that has not moved.
const unchangedSummary = "already up to date"

// uploadSummary reduces cub's upload output to one line.
//
// This parses, so it is written to degrade rather than break: an unrecognised
// output yields "", the caller falls back to a generic message, and nothing
// depends on the wording being stable. The alternative — asking ConfigHub what
// changed — costs an extra round trip per component to phrase a status line.
func uploadSummary(out string) string {
	created, updated, emptied, listed := 0, 0, 0, false
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		// cub v0.4.17 and newer print one line per Unit under a "Space X
		// (Create|Update)" heading, and "Unchanged N Unit(s)" for the rest.
		switch first, _, _ := strings.Cut(line, " "); first {
		case "Create":
			created, listed = created+1, true
		case "Update":
			updated, listed = updated+1, true
		case "Empty":
			emptied, listed = emptied+1, true
		case "Unchanged":
			listed = true
		}
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
	switch {
	case !listed:
		return ""
	case created+updated+emptied == 0:
		return unchangedSummary
	}
	return fmt.Sprintf("%d created, %d updated, %d emptied", created, updated, emptied)
}

// revertCommand extracts the rollback command cub prints for a re-upload.
//
// Worth surfacing rather than leaving in the noise: it is the only way back
// from a bad bundle, and it is scoped to the exact ChangeSet the upload made.
func revertCommand(out string) string {
	const prefix = "Revert with: "
	lines := strings.Split(out, "\n")
	for i, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, prefix) {
			return strings.TrimPrefix(line, prefix)
		}
		// cub v0.4.17 and newer: "Revert this upload of <space> with:" and the
		// command on the next line.
		if strings.HasPrefix(line, "Revert this upload of ") && strings.HasSuffix(line, " with:") && i+1 < len(lines) {
			return strings.TrimSpace(lines[i+1])
		}
	}
	return ""
}

// minCubVersion is a HARD FLOOR for install, for the same reason the Argo CD
// version is one for enroll: below it the command cannot work, and the way it
// fails does not say so.
//
// Pre-1.0 the second version number is the API version, so the floor names an
// API rather than a feature: every command below is spelled the way v0.4 spells
// it, and the responses it parses carry the field names v0.4 settled on.
//
// cub refuses outright to run against a server on a different API version, so
// the server is the real floor and this check only turns a wall of per-command
// failures into one message naming the fix.
//
// The plugin installs from GitHub releases and upgrades independently of cub,
// so the skew is ordinary rather than exotic.
const minCubVersion = "v0.4.17"

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
			"this needs cub %s or newer; found %v.%v.%v.\n"+
				"From %s an upload makes one Unit per resource, which the links this plugin\n"+
				"creates rely on. Upgrade cub: cub upgrade",
			min, got[0], got[1], got[2], min)
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

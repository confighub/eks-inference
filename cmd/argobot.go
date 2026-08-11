// Copyright (C) ConfigHub, Inc.
// SPDX-License-Identifier: MIT

package cmd

import (
	"fmt"
	"io"
	"strings"
)

// argobot install for `cub eksinf enroll cluster`, mirroring what `cub cluster
// up` does for the kind clusters it creates (see cluster_argobot.go in the cub
// source). argobot watches the ConfigHub event log and force-syncs the matching
// Argo CD Application the moment a deploy happens, closing Argo's
// reconcile-interval gap.
//
// WHY THIS EXISTS: without it the two planes behave differently under the same
// command. The mgmt plane, on a kind cluster from `cub cluster up`, force-syncs
// within seconds of `eksinf deploy`; the workload plane waited for Argo's poll.
// Measured on one stack, on the same operation (scaling smoke-gpu): 74s to react
// before argobot, under 12s after. The asymmetry is invisible in the config —
// both planes look identically wired — so it reads as "EKS is slow" rather than
// as a missing component.
//
// Nothing here is EKS-specific; it is the kind path with two differences, both
// of which fall out rather than needing special handling:
//
//   - CONFIGHUB_URL is the context's server URL verbatim. `cub cluster up`
//     rewrites loopback to host.docker.internal because a kind container cannot
//     reach the host's 127.0.0.1; an EKS pod reaching a hosted ConfigHub needs no
//     such rewrite, and a non-loopback URL is already left alone.
//   - The base Space is shared per hub, so `variant upload --allow-exists` is a
//     no-op on every cluster after the first.
const (
	// argobotComponent is the "Component" label value; the base Space is
	// <component>-base, shared across every cluster on this hub.
	argobotComponent = "argobot"
	// argobotBaseVariant is the "Variant" label of that shared base Space.
	argobotBaseVariant = "base"
	// argobotOCIRef is the published config bundle holding argobot's manifests.
	argobotOCIRef = "oci://ghcr.io/confighub/configs/argobot"
	// argobotNamespace is where argobot's manifests deploy.
	argobotNamespace = "argobot"
	// argobotUnitSlug is the Unit for argobot's single manifest file, a
	// consequence of --granularity per-file.
	argobotUnitSlug = "argobot"
	// argobotContainer is the container name in argobot's Deployment.
	argobotContainer = "argobot"
	// argobotSecretName is the out-of-band Secret holding argobot's worker
	// credentials. Never shipped in the bundle: cub refuses to upload rendered
	// Secrets and these bundles are public.
	argobotSecretName = "argobot-secrets"
)

// argobotSpace is the per-cluster variant Space for an enrolled cluster.
func argobotSpace(clusterName string) string {
	return variantSpace(argobotComponent, clusterName)
}

// installArgobot wires argobot into an enrolled cluster. It reuses the cluster's
// server-hosted OCI worker as argobot's identity: that worker owns the cluster's
// OCI target, which is all argobot needs to auto-scope its event subscription —
// there is no target to configure.
//
// The child Argo Application is NOT created here. `cub variant create --target`
// creates it, deriving the apps Space from the confighub.com/argo-apps-space
// annotation that enrollConfigHubSide puts on the OCI target. argobot rides the
// same path as any deployment variant rather than hand-rolling an Application.
func (r *runner) installArgobot(kc string, o *enrollOpts, w io.Writer) error {
	if o.noArgobot {
		fmt.Fprintln(w, "  argobot: SKIPPED (--no-argobot)")
		return nil
	}

	// A cluster named "base" would derive the variant Space "argobot-base",
	// which IS the shared base every other cluster variants from — the install
	// would re-upload over it and bind hub-wide state to one cluster. Refuse
	// rather than corrupt; the name is the caller's to change.
	if o.name == argobotBaseVariant {
		return fmt.Errorf(
			"cannot install argobot for a cluster named %q: its variant Space would collide "+
				"with the shared %s Space. Enroll under a different --name, or pass --no-argobot",
			o.name, baseSpace(argobotComponent))
	}

	space := argobotSpace(o.name)

	// 1. The shared base, from argobot's published bundle. --granularity
	// per-file keeps the single manifest file as one Unit; --allow-exists makes
	// this a no-op once any cluster on this hub has installed it.
	if _, err := r.cub("variant", "upload",
		"--component", argobotComponent,
		"--variant", argobotBaseVariant,
		"--granularity", "per-file",
		"--allow-exists",
		argobotOCIRef); err != nil {
		return fmt.Errorf("installing the argobot base component: %w", err)
	}

	// 2. The per-cluster variant, bound to this cluster's OCI target (which also
	// becomes the variant Space's release target). No --namespace: argobot's
	// manifests already place resources across the argobot and argocd
	// namespaces, and set-namespace is Space-wide — it would move the
	// argocd-namespace RBAC too.
	if _, err := r.cub("variant", "create",
		"--target", o.name+"/target",
		o.name, baseSpace(argobotComponent)); err != nil {
		return fmt.Errorf("creating the argobot variant %s: %w", space, err)
	}

	// 3. Point argobot at this ConfigHub. Deliberately the context's URL rather
	// than a constant: enrolling against staging with a hardcoded prod URL yields
	// a bot that authenticates nowhere and simply never syncs anything, with the
	// Application still reporting Healthy.
	c, err := r.context()
	if err != nil {
		return fmt.Errorf("reading cub context for argobot's CONFIGHUB_URL: %w", err)
	}
	if c.Coordinate.ServerURL == "" {
		return fmt.Errorf("cub context has no server URL; cannot configure argobot")
	}
	if _, err := r.cub("function", "do", "--quiet",
		"--space", space, "--unit", argobotUnitSlug,
		"set-env", argobotContainer,
		"CONFIGHUB_URL="+c.Coordinate.ServerURL); err != nil {
		return fmt.Errorf("configuring argobot's CONFIGHUB_URL: %w", err)
	}

	// 4. The worker-credential Secret, applied straight to the cluster BEFORE
	// publishing. `cub cluster up` publishes first and applies the Secret after,
	// which leaves argobot's pod crash-looping on a missing Secret until the
	// apply lands; the ordering here is the only intentional divergence.
	if err := r.applyArgobotSecret(kc, o, w); err != nil {
		return err
	}

	// 5. Publish argobot's own Release. Argo pulls its workload from here.
	if _, err := r.publishRelease(space); err != nil {
		return fmt.Errorf("publishing the argobot Release: %w", err)
	}

	fmt.Fprintf(w, "  argobot: variant %s, Argo app %s (kubernetes sync mode)\n", space, space)
	return nil
}

// applyArgobotSecret creates argobot's namespace and its worker-credential
// Secret. argobot reuses the cluster's worker, so this is the same identity
// ensureOCICredentials hands to Argo, in a different shape.
//
// The Secret is applied out of band and never becomes a Unit, which also puts it
// outside Argo's view: never pruned, never reported as drift.
func (r *runner) applyArgobotSecret(kc string, o *enrollOpts, w io.Writer) error {
	idOut, err := r.cub("worker", "get", "worker", "--space", o.name, "-o", "json")
	if err != nil {
		return fmt.Errorf("reading worker for argobot: %w", err)
	}
	workerID := extractJSONAt(idOut, "BridgeWorker", "BridgeWorkerID")
	if workerID == "" {
		return fmt.Errorf("could not read BridgeWorkerID for %s/worker", o.name)
	}
	secretOut, err := r.cub("worker", "get-secret", "worker", "--space", o.name)
	if err != nil {
		return fmt.Errorf("reading worker secret for argobot: %w", err)
	}
	workerSecret := strings.TrimSpace(secretOut)
	if workerSecret == "" {
		return fmt.Errorf("empty worker secret for %s/worker", o.name)
	}

	// The Namespace has to exist before the Secret lands in it, and before Argo
	// renders argobot's Deployment.
	nsYAML, err := r.kubectl(kc, "create", "namespace", argobotNamespace,
		"--dry-run=client", "-o", "yaml")
	if err != nil {
		return fmt.Errorf("rendering the %s namespace: %w", argobotNamespace, err)
	}
	if err := r.kubectlApply(kc, nsYAML); err != nil {
		return fmt.Errorf("creating the %s namespace: %w", argobotNamespace, err)
	}

	secretYAML, err := r.kubectl(kc, "create", "secret", "generic", argobotSecretName,
		"--namespace", argobotNamespace,
		"--from-literal=CONFIGHUB_WORKER_ID="+workerID,
		"--from-literal=CONFIGHUB_WORKER_SECRET="+workerSecret,
		"--dry-run=client", "-o", "yaml")
	if err != nil {
		return fmt.Errorf("rendering the argobot Secret: %w", err)
	}
	if err := r.kubectlApply(kc, secretYAML); err != nil {
		return fmt.Errorf("applying the argobot Secret: %w", err)
	}
	fmt.Fprintf(w, "  secret %s/%s (worker %s)\n", argobotNamespace, argobotSecretName, workerID)
	return nil
}

// removeArgobotSpace deletes the per-cluster argobot variant Space, for
// `enroll remove --delete-spaces`.
//
// It MUST run before the cluster Space is deleted. The argobot Space's release
// target points at <cluster>/target, and a Target with an inbound reference
// refuses to delete — so leaving this Space behind makes the cluster Space
// undeletable, with an error naming a BridgeWorker rather than the Space
// actually holding the reference.
//
// The release target is cleared first for the same reason `cub cluster up` clears
// it when rolling back a partial install: dropping the reference before the
// delete keeps the failure modes independent of ordering elsewhere.
//
// Absence is not an error. A cluster enrolled with --no-argobot, or before this
// existed, simply has no such Space.
func (r *runner) removeArgobotSpace(clusterName string, w io.Writer) error {
	space := argobotSpace(clusterName)
	has, err := r.spaceExists(space)
	if err != nil {
		return fmt.Errorf("checking Space %s: %w", space, err)
	}
	if !has {
		return nil
	}
	if _, err := r.cub("space", "update", space, "--patch", "--release-target", "-"); err != nil {
		return fmt.Errorf("clearing the release target on %s: %w", space, err)
	}
	if _, err := r.cub("space", "delete", space, "--recursive"); err != nil {
		return fmt.Errorf("deleting Space %s: %w", space, err)
	}
	fmt.Fprintf(w, "  deleted Space %s\n", space)
	return nil
}

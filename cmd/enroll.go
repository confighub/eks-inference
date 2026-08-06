package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// Argo CD v3.1 is a HARD FLOOR: earlier versions cannot resolve an oci:// source
// for plain manifests and fail with `unsupported scheme "oci"` — while reporting
// the Application Healthy, so the failure is quiet. v3.4.5 is what `cub cluster
// up` installs; matching it avoids surprises between clusters.
const defaultArgoCDVersion = "v3.4.5"

func newEnrollCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "enroll",
		Short: "Wire an existing cluster into ConfigHub (everything `cub cluster up` does except create it)",
		Long: `Enroll an existing cluster into ConfigHub: install Argo CD, create the cluster
and apps Spaces, register a server-hosted OCI worker and target, and bootstrap
the root app-of-apps.

Argo pulls from ConfigHub's OCI registry, so no worker pod runs in the cluster
and nothing needs inbound access to it. Cluster credentials are used exactly
once, to install Argo CD and apply the root Application.

ENROLL NEVER CREATES OR DESTROYS A CLUSTER. 'unenroll' removes ConfigHub wiring
and leaves the cluster running — that asymmetry with 'cub cluster down' is
deliberate.`,
	}
	c.AddCommand(newEnrollRunCmd(), newUnenrollCmd())
	return c
}

type enrollOpts struct {
	name        string
	eksCluster  string
	region      string
	kubeconfig  string
	argoVersion string
	argoNS      string
	ociRegistry string
	noArgoCD    bool

	noPlaceholderGate bool
	grantAccess       bool
}

func newEnrollRunCmd() *cobra.Command {
	var o enrollOpts

	c := &cobra.Command{
		Use:   "cluster",
		Short: "Enroll an EKS cluster",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if o.name == "" {
				return fmt.Errorf("--name is required (the ConfigHub Space prefix)")
			}
			if err := requireTools("cub", "kubectl", "aws"); err != nil {
				return err
			}
			r := newRunner()
			if err := r.requireConfigHubAuth(); err != nil {
				return err
			}
			w := cmd.OutOrStdout()

			registry, err := r.ociRegistry(o.ociRegistry)
			if err != nil {
				return err
			}
			fmt.Fprintf(w, "  OCI registry: %s\n", registry)

			kc, err := r.resolveEnrollKubeconfig(&o, w)
			if err != nil {
				return err
			}
			if err := r.enrollPreflight(kc, &o, w); err != nil {
				return err
			}
			if err := r.enrollConfigHubSide(&o, w); err != nil {
				return err
			}
			if err := r.installArgoCD(kc, &o, w); err != nil {
				return err
			}
			if err := r.ensureOCICredentials(kc, &o, registry, w); err != nil {
				return err
			}
			if err := r.ensureRootApp(kc, &o, registry, w); err != nil {
				return err
			}

			appsSpace := o.name + "-argo-apps"
			fmt.Fprintf(w, "\nEnrolled %q.\n\n", o.name)
			fmt.Fprintf(w, "  cluster space : %s   (worker + OCI target)\n", o.name)
			fmt.Fprintf(w, "  apps space    : %s\n", appsSpace)
			fmt.Fprintf(w, "  kubeconfig    : %s\n\n", kc)
			fmt.Fprintf(w, "Deploy the workload plane with:\n")
			fmt.Fprintf(w, "  cub eksinf deploy --plane workload --target %s/target\n", o.name)
			return nil
		},
	}

	c.Flags().StringVar(&o.name, "name", "", "ConfigHub Space prefix; apps Space is <name>-argo-apps")
	c.Flags().StringVar(&o.eksCluster, "eks-cluster", "", "EKS cluster to enroll (resolved via aws eks update-kubeconfig)")
	c.Flags().StringVar(&o.region, "region", "", "AWS region, with --eks-cluster")
	c.Flags().StringVar(&o.kubeconfig, "kubeconfig", "", "explicit kubeconfig instead of --eks-cluster")
	c.Flags().StringVar(&o.argoVersion, "argocd-version", defaultArgoCDVersion, "Argo CD version (v3.1+ required for oci://)")
	c.Flags().StringVar(&o.argoNS, "argocd-namespace", "argocd", "namespace to install Argo CD into")
	c.Flags().StringVar(&o.ociRegistry, "oci-registry", "", "override the OCI registry (derived from the cub context by default)")
	c.Flags().BoolVar(&o.noArgoCD, "no-argocd", false, "do not install Argo CD; fail if it is absent")
	c.Flags().BoolVar(&o.grantAccess, "grant-access", false,
		"grant your AWS identity cluster-admin on the EKS cluster if it has no access entry")
	c.Flags().BoolVar(&o.noPlaceholderGate, "no-placeholder-gate", false,
		"skip the default vet-placeholders Trigger, allowing Releases with unfilled placeholders")
	return c
}

// resolveEnrollKubeconfig writes the cluster's kubeconfig to the same path
// `cub cluster up` uses, so every other command that hunts for a cub cluster
// kubeconfig finds this one too.
func (r *runner) resolveEnrollKubeconfig(o *enrollOpts, w interface{ Write([]byte) (int, error) }) (string, error) {
	if o.kubeconfig != "" {
		if _, err := os.Stat(o.kubeconfig); err != nil {
			return "", fmt.Errorf("no kubeconfig at %s", o.kubeconfig)
		}
		return o.kubeconfig, nil
	}
	if o.eksCluster == "" {
		return "", fmt.Errorf("pass --eks-cluster (with --region) or --kubeconfig")
	}
	if o.region == "" {
		return "", fmt.Errorf("--region is required with --eks-cluster")
	}
	path := cubClusterKubeconfig(o.name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if _, err := r.aws("eks", "update-kubeconfig",
		"--name", o.eksCluster, "--region", o.region, "--kubeconfig", path); err != nil {
		return "", fmt.Errorf("resolving EKS cluster %q in %s: %w", o.eksCluster, o.region, err)
	}
	fmt.Fprintf(w, "  kubeconfig:   %s\n", path)
	return path, nil
}

// accessEntryPrincipal converts the caller's STS identity into the ARN an EKS
// access entry actually takes.
//
// sts:GetCallerIdentity returns the SESSION for anything assumed — an SSO login
// reports arn:aws:sts::123:assumed-role/AWSReservedSSO_Admin_abc/user@example.com
// — and EKS wants the underlying role, arn:aws:iam::123:role/AWSReservedSSO_Admin_abc.
// Passing the session ARN is accepted by nothing and the error does not explain
// why. An IAM user ARN needs no conversion.
func accessEntryPrincipal(callerARN string) string {
	parts := strings.Split(callerARN, ":")
	if len(parts) < 6 || parts[2] != "sts" {
		return callerARN
	}
	res := strings.SplitN(parts[5], "/", 3)
	if len(res) < 2 || res[0] != "assumed-role" {
		return callerARN
	}
	return fmt.Sprintf("arn:%s:iam::%s:role/%s", parts[1], parts[4], res[1])
}

// diagnoseUnreachable explains a cluster that authenticates nobody.
//
// This is a cliff the stack creates for itself: ACK builds the cluster as the
// dedicated eks-inference-ack identity, so THAT principal is the one EKS grants
// bootstrap admin to. The human running enroll is a different identity with no
// access entry, and gets a bare 401 that says nothing about why.
func (r *runner) diagnoseUnreachable(kc string, o *enrollOpts, w io.Writer) error {
	generic := fmt.Errorf("cannot reach the cluster with that kubeconfig")
	if o.eksCluster == "" || o.region == "" {
		return generic
	}
	id, err := r.callerIdentity()
	if err != nil {
		return generic
	}
	principal := accessEntryPrincipal(id.Arn)

	out, err := r.aws("eks", "list-access-entries", "--cluster-name", o.eksCluster,
		"--region", o.region, "--output", "json")
	if err != nil {
		return generic
	}
	var entries struct {
		AccessEntries []string `json:"accessEntries"`
	}
	if err := json.Unmarshal([]byte(out), &entries); err != nil {
		return generic
	}
	for _, e := range entries.AccessEntries {
		if e == principal {
			// It has access and still cannot connect, so this is not the
			// access-entry problem and saying so would send them the wrong way.
			return generic
		}
	}

	if !o.grantAccess {
		return fmt.Errorf(
			"the cluster rejects your credentials: %s has no EKS access entry.\n"+
				"ACK created this cluster as its own identity, so that principal holds the\n"+
				"bootstrap admin access and yours was never granted any.\n"+
				"Re-run with --grant-access to add it, or do it by hand:\n"+
				"  aws eks create-access-entry --cluster-name %s --region %s \\\n"+
				"    --principal-arn %s --type STANDARD\n"+
				"  aws eks associate-access-policy --cluster-name %s --region %s \\\n"+
				"    --principal-arn %s --access-scope type=cluster \\\n"+
				"    --policy-arn arn:aws:eks::aws:cluster-access-policy/AmazonEKSClusterAdminPolicy",
			principal, o.eksCluster, o.region, principal, o.eksCluster, o.region, principal)
	}

	fmt.Fprintf(w, "  granting %s cluster-admin access\n", principal)
	if _, err := r.aws("eks", "create-access-entry", "--cluster-name", o.eksCluster,
		"--region", o.region, "--principal-arn", principal, "--type", "STANDARD"); err != nil {
		return fmt.Errorf("creating the access entry for %s: %w", principal, err)
	}
	if _, err := r.aws("eks", "associate-access-policy", "--cluster-name", o.eksCluster,
		"--region", o.region, "--principal-arn", principal, "--access-scope", "type=cluster",
		"--policy-arn", "arn:aws:eks::aws:cluster-access-policy/AmazonEKSClusterAdminPolicy"); err != nil {
		return fmt.Errorf("associating the admin policy for %s: %w", principal, err)
	}

	// The entry is not effective immediately; it took about 30s in practice.
	deadline := time.Now().Add(3 * time.Minute)
	for !r.reachable(kc) {
		if time.Now().After(deadline) {
			return fmt.Errorf("granted access to %s but the cluster still rejects it after 3m", principal)
		}
		time.Sleep(10 * time.Second)
	}
	fmt.Fprintln(w, "  access granted")
	return nil
}

func (r *runner) enrollPreflight(kc string, o *enrollOpts, w interface{ Write([]byte) (int, error) }) error {
	if !r.reachable(kc) {
		if err := r.diagnoseUnreachable(kc, o, w); err != nil {
			return err
		}
	}
	// Installing Argo CD needs cluster-admin. Fail here rather than halfway
	// through applying a 20k-line manifest.
	out, err := r.kubectl(kc, "auth", "can-i", "create", "clusterrole", "--all-namespaces")
	if err != nil || strings.TrimSpace(out) != "yes" {
		return fmt.Errorf("need cluster-admin on this cluster (cannot create ClusterRoles)")
	}
	fmt.Fprintln(w, "  cluster-admin: yes")
	return nil
}

func (r *runner) enrollConfigHubSide(o *enrollOpts, w interface{ Write([]byte) (int, error) }) error {
	appsSpace := o.name + "-argo-apps"

	if _, err := r.cub("space", "create", o.name, "--allow-exists"); err != nil {
		return fmt.Errorf("creating Space %s: %w", o.name, err)
	}
	// A server-hosted worker: ConfigHub runs it, nothing is deployed to the
	// cluster for it. This is what lets Argo pull without a worker pod.
	if _, err := r.cub("worker", "create", "worker", "--space", o.name,
		"--is-server-worker", "--allow-exists"); err != nil {
		return fmt.Errorf("creating worker in %s: %w", o.name, err)
	}
	// -p/-t are REQUIRED: target create defaults to Kubernetes/Kubernetes-YAML,
	// which a server-hosted worker does not support, and the resulting error
	// names the provider type rather than the flag you forgot.
	if _, err := r.cub("target", "create", "target", "{}", "worker",
		"--space", o.name, "-p", "OCI", "-t", "Any",
		"--annotation", "confighub.com/argo-apps-space="+appsSpace,
		"--allow-exists"); err != nil {
		return fmt.Errorf("creating OCI target in %s: %w", o.name, err)
	}
	fmt.Fprintf(w, "  space %s: worker + OCI target\n", o.name)

	if _, err := r.cub("space", "create", appsSpace, "--allow-exists"); err != nil {
		return fmt.Errorf("creating Space %s: %w", appsSpace, err)
	}
	// Cross-space release target: the apps Space's bundle is served by the OCI
	// target in the cluster Space, so neither Space releases into itself.
	if _, err := r.cub("space", "update", appsSpace, "--patch",
		"--release-target", o.name+"/target"); err != nil {
		return fmt.Errorf("setting release target on %s: %w", appsSpace, err)
	}
	fmt.Fprintf(w, "  space %s: release target %s/target\n", appsSpace, o.name)

	if err := r.ensurePlaceholderGate(o, w); err != nil {
		return err
	}
	return nil
}

// ensurePlaceholderGate makes the cluster refuse Releases that still contain an
// unfilled confighubplaceholder value, matching what `cub cluster up` does for
// the clusters it creates (cub >= v0.2.10).
//
// This stack depends on it. The AWS region and the subnet availability zones are
// rendered as placeholders and filled per environment by links from the
// platform-profile Unit. Without the gate, forgetting `eksinf link-profile`
// publishes cleanly and the ACK controllers come up with a literal region of
// "confighubplaceholder", reconciling into nowhere while reporting healthy.
//
// The Target selects every Trigger in the cluster Space, so additional gates are
// just more Triggers there plus `cub target update --refresh-triggers`.
func (r *runner) ensurePlaceholderGate(o *enrollOpts, w interface{ Write([]byte) (int, error) }) error {
	if o.noPlaceholderGate {
		fmt.Fprintln(w, "  placeholder gate: SKIPPED (--no-placeholder-gate)")
		return nil
	}
	if _, err := r.cub("trigger", "create", "no-placeholders",
		"Mutation", "Kubernetes/YAML", "vet-placeholders",
		"--space", o.name, "--allow-exists",
		"--description", "Blocks publishing a Release that still contains an unfilled confighubplaceholder value."); err != nil {
		return fmt.Errorf("creating the placeholder Trigger: %w", err)
	}
	out, err := r.cub("space", "get", o.name, "-o", "json")
	if err != nil {
		return fmt.Errorf("reading Space %s: %w", o.name, err)
	}
	spaceID := extractJSONAt(out, "Space", "SpaceID")
	if spaceID == "" {
		return fmt.Errorf("could not read SpaceID for %s", o.name)
	}
	if _, err := r.cub("target", "update", "--space", o.name, "target",
		"--where-trigger", "SpaceID = '"+spaceID+"'"); err != nil {
		return fmt.Errorf("attaching the gate to the target: %w", err)
	}
	fmt.Fprintln(w, "  placeholder gate: on (Releases with unfilled placeholders are blocked)")
	return nil
}

func (r *runner) installArgoCD(kc string, o *enrollOpts, w interface{ Write([]byte) (int, error) }) error {
	if _, err := r.kubectl(kc, "get", "deployment", "argocd-server", "-n", o.argoNS); err == nil {
		fmt.Fprintf(w, "  Argo CD already present in %s; leaving it alone\n", o.argoNS)
		return nil
	}
	if o.noArgoCD {
		return fmt.Errorf("Argo CD not found in %s and --no-argocd was given", o.argoNS)
	}
	fmt.Fprintf(w, "  installing Argo CD %s into %s\n", o.argoVersion, o.argoNS)
	nsYAML, err := r.kubectl(kc, "create", "namespace", o.argoNS, "--dry-run=client", "-o", "yaml")
	if err != nil {
		return err
	}
	if err := r.kubectlApply(kc, nsYAML); err != nil {
		return err
	}
	url := fmt.Sprintf("https://raw.githubusercontent.com/argoproj/argo-cd/%s/manifests/install.yaml", o.argoVersion)
	if _, err := r.kubectl(kc, "apply", "-n", o.argoNS, "--server-side", "--force-conflicts", "-f", url); err != nil {
		return fmt.Errorf("installing Argo CD: %w", err)
	}
	if _, err := r.kubectl(kc, "rollout", "status", "deployment/argocd-server",
		"-n", o.argoNS, "--timeout=300s"); err != nil {
		return fmt.Errorf("argocd-server did not become ready: %w", err)
	}
	fmt.Fprintln(w, "  Argo CD ready")
	return nil
}

// ensureOCICredentials gives Argo the credentials to pull from ConfigHub's OCI
// registry: the worker ID as username, the worker secret as password, in an Argo
// repo-creds Secret.
//
// Without this the Application resolves the oci:// scheme and then fails with a
// bare 401 — and because Argo reports an Application whose manifests it cannot
// load as Healthy, the failure is quiet. This is the least discoverable step in
// the whole flow.
func (r *runner) ensureOCICredentials(kc string, o *enrollOpts, registry string, w interface{ Write([]byte) (int, error) }) error {
	idOut, err := r.cub("worker", "get", "worker", "--space", o.name, "-o", "json")
	if err != nil {
		return fmt.Errorf("reading worker: %w", err)
	}
	workerID := extractJSONAt(idOut, "BridgeWorker", "BridgeWorkerID")
	if workerID == "" {
		return fmt.Errorf("could not read BridgeWorkerID for %s/worker", o.name)
	}
	secretOut, err := r.cub("worker", "get-secret", "worker", "--space", o.name)
	if err != nil {
		return fmt.Errorf("reading worker secret: %w", err)
	}
	workerSecret := strings.TrimSpace(secretOut)
	if workerSecret == "" {
		return fmt.Errorf("empty worker secret for %s/worker", o.name)
	}

	yaml, err := r.kubectl(kc, "create", "secret", "generic", "confighub-oci-creds",
		"--namespace", o.argoNS,
		"--from-literal=type=oci",
		"--from-literal=enableOCI=true",
		"--from-literal=url=oci://"+registry,
		"--from-literal=username="+workerID,
		"--from-literal=password="+workerSecret,
		"--dry-run=client", "-o", "yaml")
	if err != nil {
		return err
	}
	if err := r.kubectlApply(kc, yaml); err != nil {
		return err
	}
	if _, err := r.kubectl(kc, "label", "secret", "confighub-oci-creds", "-n", o.argoNS,
		"argocd.argoproj.io/secret-type=repo-creds", "--overwrite"); err != nil {
		return err
	}
	fmt.Fprintf(w, "  secret %s/confighub-oci-creds (repo-creds, worker %s)\n", o.argoNS, workerID)
	return nil
}

func rootApplicationYAML(appsSpace, argoNS, registry string) string {
	return fmt.Sprintf(`apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: %[1]s
  namespace: %[2]s
  finalizers:
  - resources-finalizer.argocd.argoproj.io
spec:
  project: default
  source:
    repoURL: oci://%[3]s/space/%[1]s
    targetRevision: latest
    path: .
  destination:
    server: https://kubernetes.default.svc
    namespace: %[2]s
  syncPolicy:
    automated:
      selfHeal: true
    syncOptions:
    - ServerSideApply=true
    - ServerSideApply.ForceConflicts=true
    - RespectIgnoreDifferences=true
    - CreateNamespace=false
`, appsSpace, argoNS, registry)
}

func (r *runner) ensureRootApp(kc string, o *enrollOpts, registry string, w interface{ Write([]byte) (int, error) }) error {
	appsSpace := o.name + "-argo-apps"
	body := rootApplicationYAML(appsSpace, o.argoNS, registry)

	hasRoot, err := r.unitExists(appsSpace, "root")
	if err != nil {
		return fmt.Errorf("checking root Unit: %w", err)
	}
	if hasRoot {
		if _, err := r.cubStdin([]byte(body), "unit", "update", "--space", appsSpace, "root", "-"); err != nil {
			return fmt.Errorf("updating root Unit: %w", err)
		}
	} else {
		if _, err := r.cubStdin([]byte(body), "unit", "create", "--space", appsSpace, "root", "-"); err != nil {
			return fmt.Errorf("creating root Unit: %w", err)
		}
	}
	if _, err := r.publishRelease(appsSpace); err != nil {
		return fmt.Errorf("publishing %s: %w", appsSpace, err)
	}
	// The one and only time cluster credentials are needed for config. From here
	// Argo pulls the apps-Space bundle from ConfigHub's OCI registry itself.
	if err := r.kubectlApply(kc, body); err != nil {
		return fmt.Errorf("applying the root Application: %w", err)
	}
	fmt.Fprintf(w, "  applied Application/%s in %s\n", appsSpace, o.argoNS)
	return nil
}

func newUnenrollCmd() *cobra.Command {
	var name, argoNS, kubeconfig string
	var deleteSpaces, yes bool

	c := &cobra.Command{
		Use:   "remove",
		Short: "Remove ConfigHub wiring from a cluster, leaving the cluster running",
		Long: `Remove the ConfigHub wiring for an enrolled cluster.

THIS DOES NOT DELETE THE CLUSTER, and does not delete the workloads Argo
deployed. The asymmetry with 'cub cluster down' is deliberate.

The root Application carries a resources-finalizer, so a plain delete CASCADES:
Argo would delete every child Application and everything they deployed. The
finalizer is stripped first so the delete orphans instead. That is the single
most dangerous step here.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if name == "" {
				return fmt.Errorf("--name is required")
			}
			if err := requireTools("cub", "kubectl"); err != nil {
				return err
			}
			r := newRunner()
			w := cmd.OutOrStdout()
			appsSpace := name + "-argo-apps"

			kc := kubeconfig
			if kc == "" {
				kc = cubClusterKubeconfig(name)
			}
			if !yes {
				fmt.Fprintf(w, "This removes ConfigHub wiring for %q.\n", name)
				fmt.Fprintln(w, "It will NOT delete the cluster or the workloads Argo deployed.")
				if deleteSpaces {
					fmt.Fprintf(w, "It WILL delete Spaces %s and %s.\n", name, appsSpace)
				}
				return fmt.Errorf("re-run with --yes to proceed")
			}

			if r.reachable(kc) {
				if _, err := r.kubectl(kc, "get", "application", appsSpace, "-n", argoNS); err == nil {
					// Strip the finalizer BEFORE deleting, then orphan.
					if _, err := r.kubectl(kc, "patch", "application", appsSpace, "-n", argoNS,
						"--type=merge", "-p", `{"metadata":{"finalizers":[]}}`); err != nil {
						return fmt.Errorf("removing finalizer: %w", err)
					}
					fmt.Fprintln(w, "  removed resources-finalizer (children orphaned, not deleted)")
					if _, err := r.kubectl(kc, "delete", "application", appsSpace,
						"-n", argoNS, "--cascade=orphan"); err != nil {
						return fmt.Errorf("deleting root Application: %w", err)
					}
					fmt.Fprintf(w, "  deleted Application/%s\n", appsSpace)
				} else {
					fmt.Fprintln(w, "  no root Application found")
				}
			} else {
				fmt.Fprintln(w, "  cluster unreachable — skipping in-cluster cleanup")
			}
			fmt.Fprintln(w, "  Argo CD left installed (it may predate enrollment)")

			if deleteSpaces {
				for _, s := range []string{appsSpace, name} {
					if _, err := r.cub("space", "delete", s); err != nil {
						fmt.Fprintf(w, "  could not delete Space %s: %v\n", s, err)
					} else {
						fmt.Fprintf(w, "  deleted Space %s\n", s)
					}
				}
			} else {
				fmt.Fprintln(w, "  ConfigHub Spaces left in place (pass --delete-spaces to remove)")
			}

			fmt.Fprintf(w, "\nUnenrolled %q. The cluster is untouched.\n", name)
			return nil
		},
	}

	c.Flags().StringVar(&name, "name", "", "ConfigHub Space prefix used at enroll time")
	c.Flags().StringVar(&argoNS, "argocd-namespace", "argocd", "namespace Argo CD is installed in")
	c.Flags().StringVar(&kubeconfig, "kubeconfig", "", "explicit kubeconfig")
	c.Flags().BoolVar(&deleteSpaces, "delete-spaces", false, "also delete the ConfigHub Spaces")
	c.Flags().BoolVar(&yes, "yes", false, "proceed without confirmation")
	return c
}

// extractJSONString pulls a string field out of cub's nested JSON without
// modelling the whole entity, which varies by command.
//
// This parses rather than scans. The previous version searched for the literal
// `"key":"`, which silently found nothing the moment cub pretty-printed its
// output with a space after the colon — reporting "could not read SpaceID" for
// a Space that existed and whose JSON contained the field.
func extractJSONString(blob, key string) string {
	var doc any
	if err := json.Unmarshal([]byte(blob), &doc); err != nil {
		return ""
	}
	return findJSONString(doc, key)
}

// extractJSONAt pulls a string from an explicit path, which is what callers
// should use whenever they know the shape.
//
// Searching by key alone is not safe on cub's output: `cub unit get` returns
// six sibling entities and TWO of them carry a UnitID — Unit.UnitID and
// UpstreamUnit.UnitID. A search returns whichever the traversal reaches first,
// so it happened to be right only because "Unit" sorts before "UpstreamUnit".
// Naming the path removes the coincidence.
func extractJSONAt(blob string, path ...string) string {
	var doc any
	if err := json.Unmarshal([]byte(blob), &doc); err != nil {
		return ""
	}
	for _, key := range path {
		m, ok := doc.(map[string]any)
		if !ok {
			return ""
		}
		doc, ok = m[key]
		if !ok {
			return ""
		}
	}
	s, _ := doc.(string)
	return s
}

// findJSONString walks the decoded document for the first string value under
// key, at any depth. Prefer extractJSONAt where the shape is known; this
// remains for the cases where it genuinely is not.
func findJSONString(node any, key string) string {
	switch v := node.(type) {
	case map[string]any:
		if s, ok := v[key].(string); ok {
			return s
		}
		// Deterministic order: map iteration is randomised, and a document with
		// the key at two depths would otherwise return a different answer per
		// run.
		names := make([]string, 0, len(v))
		for k := range v {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, k := range names {
			if s := findJSONString(v[k], key); s != "" {
				return s
			}
		}
	case []any:
		for _, item := range v {
			if s := findJSONString(item, key); s != "" {
				return s
			}
		}
	}
	return ""
}

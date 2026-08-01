package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

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
			if err := r.enrollPreflight(kc, w); err != nil {
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

func (r *runner) enrollPreflight(kc string, w interface{ Write([]byte) (int, error) }) error {
	if !r.reachable(kc) {
		return fmt.Errorf("cannot reach the cluster with that kubeconfig")
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
	workerID := extractJSONString(idOut, "BridgeWorkerID")
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
func extractJSONString(blob, key string) string {
	needle := `"` + key + `":"`
	i := strings.Index(blob, needle)
	if i < 0 {
		return ""
	}
	rest := blob[i+len(needle):]
	j := strings.IndexByte(rest, '"')
	if j < 0 {
		return ""
	}
	return rest[:j]
}

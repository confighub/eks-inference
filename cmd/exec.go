package cmd

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// This tool shells out to cub, kubectl, and aws rather than importing their SDKs.
//
// That is a deliberate v1 choice. The AWS SDK alone would dominate the binary
// and the dependency surface, and the operations here are exactly the ones those
// CLIs already expose. What the plugin adds over the shell scripts it replaces is
// typed flags, structured output, real error handling, and a single place where
// the two-plane model is encoded — not a reimplementation of three API clients.
//
// The one rule it does not relax: a command that cannot reach a cluster or the
// AWS API must FAIL, never report an empty result. Every helper below
// distinguishes "no results" from "could not ask", because conflating them is
// how a lost session reads as a healthy, reassuring zero.

type runner struct {
	env []string
}

func newRunner() *runner { return &runner{env: os.Environ()} }

func (r *runner) withEnv(kv ...string) *runner {
	out := &runner{env: append([]string(nil), r.env...)}
	out.env = append(out.env, kv...)
	return out
}

// run executes a command and returns stdout. Stderr is folded into the error so
// the caller can surface the tool's own message rather than a generic exit code.
func (r *runner) run(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Env = r.env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		if msg == "" {
			return "", fmt.Errorf("%s: %w", name, err)
		}
		return "", fmt.Errorf("%s: %s", name, firstLine(msg))
	}
	return stdout.String(), nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// requireTools fails early with one clear message rather than letting the first
// missing binary surface as a confusing exec error mid-operation.
func requireTools(names ...string) error {
	var missing []string
	for _, n := range names {
		if _, err := exec.LookPath(n); err != nil {
			missing = append(missing, n)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("required tool(s) not found in PATH: %s", strings.Join(missing, ", "))
	}
	return nil
}

// cub runs the cub CLI. CONFIGHUB_AGENT is set so cub emits machine-oriented
// output and hints.
func (r *runner) cub(args ...string) (string, error) {
	return r.withEnv("CONFIGHUB_AGENT=1").run("cub", args...)
}

// requireConfigHubAuth fails with the actionable message rather than letting
// every subsequent call fail with a generic authentication error. Re-auth is an
// interactive browser sign-in, so the only useful response is to tell the user.
func (r *runner) requireConfigHubAuth() error {
	if _, err := r.cub("auth", "status"); err != nil {
		return fmt.Errorf("not authenticated to ConfigHub — run 'cub auth login'")
	}
	return nil
}

// cubStdin runs cub with a body on stdin. Used for `link create --from-stdin`,
// which requires JSON — see the note in link.go.
func (r *runner) cubStdin(body []byte, args ...string) (string, error) {
	cmd := exec.Command("cub", args...)
	cmd.Env = append(append([]string(nil), r.env...), "CONFIGHUB_AGENT=1")
	cmd.Stdin = bytes.NewReader(body)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		if msg == "" {
			return "", fmt.Errorf("cub: %w", err)
		}
		return "", fmt.Errorf("cub: %s", firstLine(msg))
	}
	return stdout.String(), nil
}

// aws runs the AWS CLI, honouring --profile.
func (r *runner) aws(args ...string) (string, error) {
	if flagProfile != "" {
		args = append([]string{"--profile", flagProfile}, args...)
	}
	return r.run("aws", args...)
}

// kubectl runs kubectl against an explicit kubeconfig.
func (r *runner) kubectl(kubeconfig string, args ...string) (string, error) {
	return r.withEnv("KUBECONFIG="+kubeconfig).run("kubectl", args...)
}

// kubectlApply pipes a manifest to `kubectl apply -f -`.
func (r *runner) kubectlApply(kubeconfig, manifest string) error {
	cmd := exec.Command("kubectl", "apply", "-f", "-")
	cmd.Env = append(append([]string(nil), r.env...), "KUBECONFIG="+kubeconfig)
	cmd.Stdin = strings.NewReader(manifest)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return fmt.Errorf("kubectl apply: %s", firstLine(msg))
		}
		return fmt.Errorf("kubectl apply: %w", err)
	}
	return nil
}

// reachable reports whether a cluster genuinely answers. Callers must gate on
// this rather than letting individual queries degrade to empty output — an
// expired token and a healthy empty cluster otherwise look identical.
func (r *runner) reachable(kubeconfig string) bool {
	_, err := r.kubectl(kubeconfig, "version", "-o", "json")
	return err == nil
}

// cubClusterKubeconfig returns the path `cub cluster up` writes for a cluster.
// cub does not add a context to the default kubeconfig, so `kubectl --context
// kind-<name>` does not work and every caller needs this path.
func cubClusterKubeconfig(name string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".confighub", "clusters", name+".kubeconfig")
}

// reachableClusters lists the cub-managed clusters that answer, without
// regard to what is deployed to them.
func (r *runner) reachableClusters() ([]string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	paths, _ := filepath.Glob(filepath.Join(home, ".confighub", "clusters", "*.kubeconfig"))
	var names []string
	for _, p := range paths {
		if r.reachable(p) {
			names = append(names, strings.TrimSuffix(filepath.Base(p), ".kubeconfig"))
		}
	}
	return names, nil
}

// discoverClusters finds the mgmt and workload clusters among the cub-managed
// kubeconfigs, identifying each by what is deployed to it. Only reachable
// clusters are considered, so an unreachable one is reported as such rather
// than silently omitted.
func (r *runner) discoverClusters() (mgmt, workload string) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", ""
	}
	paths, _ := filepath.Glob(filepath.Join(home, ".confighub", "clusters", "*.kubeconfig"))
	for _, p := range paths {
		if !r.reachable(p) {
			continue
		}
		name := strings.TrimSuffix(filepath.Base(p), ".kubeconfig")
		if mgmt == "" {
			if _, err := r.kubectl(p, "get", "ns", "ack-system"); err == nil {
				mgmt = name
			}
		}
		if workload == "" {
			if _, err := r.kubectl(p, "get", "crd", "nodepools.karpenter.sh"); err == nil {
				workload = name
			}
		}
	}
	return mgmt, workload
}

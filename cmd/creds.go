package cmd

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

const (
	ackNamespace = "ack-system"
	credsSecret  = "aws-creds"
	expiresAtAnn = "eks-inference.confighub.com/expires-at"
)

func newCredsCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "creds",
		Short: "Manage the AWS credentials the ACK controllers use",
		Long: `Put AWS credentials where the ACK controllers can read them.

The ACK controllers run in the local kind cluster and call AWS on your behalf. In
EKS they would use IRSA or Pod Identity and hold no long-lived credentials; in
kind neither is available, so they need a credentials file.

The Secret is written straight to the cluster and is never a ConfigHub Unit:
cub refuses to upload rendered Secrets, and these bundles are public. It is
therefore also invisible to Argo — never pruned, never reported as drift.`,
	}
	c.AddCommand(
		newCredsStatusCmd(),
		newCredsUseExistingCmd(),
		newCredsRefreshCmd(),
		newCredsCreateUserCmd(),
		newCredsDeleteUserCmd(),
		newCredsShowPolicyCmd(),
	)
	return c
}

// exportedCreds is the shape of `aws configure export-credentials --format process`.
type exportedCreds struct {
	AccessKeyID     string `json:"AccessKeyId"`
	SecretAccessKey string `json:"SecretAccessKey"`
	SessionToken    string `json:"SessionToken"`
	Expiration      string `json:"Expiration"`
}

func (r *runner) exportCreds() (*exportedCreds, error) {
	out, err := r.aws("configure", "export-credentials", "--format", "process")
	if err != nil {
		return nil, fmt.Errorf(
			"could not export AWS credentials. Is a profile configured (or --profile passed)?\n  %w", err)
	}
	var c exportedCreds
	if err := json.Unmarshal([]byte(out), &c); err != nil {
		return nil, fmt.Errorf("parsing exported credentials: %w", err)
	}
	return &c, nil
}

type callerIdentity struct {
	Account string `json:"Account"`
	Arn     string `json:"Arn"`
}

func (r *runner) callerIdentity() (*callerIdentity, error) {
	out, err := r.aws("sts", "get-caller-identity", "--output", "json")
	if err != nil {
		return nil, err
	}
	var id callerIdentity
	if err := json.Unmarshal([]byte(out), &id); err != nil {
		return nil, err
	}
	return &id, nil
}

// mgmtKubeconfig resolves the kind cluster running the ACK controllers, failing
// loudly rather than falling back to whatever kubectl happens to point at.
func (r *runner) mgmtKubeconfig() (string, error) {
	mgmt, _ := r.discoverClusters()
	if mgmt == "" {
		return "", fmt.Errorf(
			"no reachable cluster with an %s namespace.\n"+
				"It may not be up, or its credentials may have expired — those are not the same thing",
			ackNamespace)
	}
	return cubClusterKubeconfig(mgmt), nil
}

// writeSecret creates or rotates the credentials Secret and restarts the
// controllers. The restart is not optional: the AWS SDK reads the credentials
// file once at startup, so updating the Secret alone changes the mounted file
// but not the running process.
func (r *runner) writeSecret(kubeconfig string, c *exportedCreds, w io.Writer) error {
	body := fmt.Sprintf("[default]\naws_access_key_id = %s\naws_secret_access_key = %s\n",
		c.AccessKeyID, c.SecretAccessKey)
	if c.SessionToken != "" {
		body += fmt.Sprintf("aws_session_token = %s\n", c.SessionToken)
	}

	nsYAML, err := r.kubectl(kubeconfig, "create", "namespace", ackNamespace,
		"--dry-run=client", "-o", "yaml")
	if err != nil {
		return err
	}
	if err := r.kubectlApply(kubeconfig, nsYAML); err != nil {
		return err
	}

	secYAML, err := r.kubectl(kubeconfig, "create", "secret", "generic", credsSecret,
		"--namespace", ackNamespace, "--from-literal=credentials="+body,
		"--dry-run=client", "-o", "yaml")
	if err != nil {
		return err
	}
	if err := r.kubectlApply(kubeconfig, secYAML); err != nil {
		return err
	}

	// Record the expiry as an annotation (not sensitive) so status can answer
	// "will these outlast what I am about to provision?" without re-deriving it
	// from an environment that may have moved on.
	if c.Expiration != "" {
		_, _ = r.kubectl(kubeconfig, "annotate", "secret", credsSecret, "-n", ackNamespace,
			"--overwrite", expiresAtAnn+"="+c.Expiration)
	} else {
		_, _ = r.kubectl(kubeconfig, "annotate", "secret", credsSecret, "-n", ackNamespace,
			expiresAtAnn+"-")
	}
	fmt.Fprintf(w, "  wrote Secret %s/%s\n", ackNamespace, credsSecret)

	if _, err := r.kubectl(kubeconfig, "rollout", "restart", "deployment", "-n", ackNamespace); err == nil {
		fmt.Fprintf(w, "  restarted controllers in %s\n", ackNamespace)
	}
	return nil
}

func newCredsUseExistingCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "use-existing",
		Short: "Reuse your current AWS identity (a permanent key or an SSO session)",
		Long: `Reuse whatever AWS identity you already have: a permanent key in
~/.aws/credentials, or an SSO / assumed-role session. Both are supported.

An expiring session is not dangerous here. When the credentials lapse the ACK
controllers get auth errors, mark their resources ACK.Recoverable, and pause.
Nothing is deleted and nothing is half-destroyed; 'creds refresh' resumes it.
So rather than refuse, this reports how long the session has left.

Root credentials ARE refused: they are permanent, so they would otherwise pass,
but they cannot be scoped and cannot be revoked independently.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := requireTools("aws", "kubectl"); err != nil {
				return err
			}
			r := newRunner()
			w := cmd.OutOrStdout()

			c, err := r.exportCreds()
			if err != nil {
				return err
			}
			id, err := r.callerIdentity()
			if err != nil {
				return fmt.Errorf("credentials are not valid: %w", err)
			}
			fmt.Fprintf(w, "  account:    %s\n  arn:        %s\n  access key: %s\n",
				id.Account, id.Arn, c.AccessKeyID)

			if strings.HasSuffix(id.Arn, ":root") {
				return fmt.Errorf(
					"these are ROOT account credentials.\n" +
						"Root keys cannot be scoped and cannot be revoked without disrupting the\n" +
						"whole account. Create a scoped IAM user instead")
			}

			if c.SessionToken != "" || !strings.Contains(id.Arn, ":user/") {
				fmt.Fprintf(w, "  type:       temporary session\n")
				if left, ok := remaining(c.Expiration); ok {
					fmt.Fprintf(w, "  expires in: %s\n", left)
					// The full stack takes ~25 minutes. Below that the session
					// will likely lapse partway — recoverable, but it needs a
					// refresh, so say so before it happens rather than after.
					if left < 30*time.Minute {
						fmt.Fprintf(w, "\n  This session is likely to lapse before the stack finishes\n"+
							"  provisioning (~25 min). That is recoverable — reconciliation pauses\n"+
							"  and resumes — but you will need:  aws sso login && cub eksinf creds refresh\n\n")
					}
				}
			} else {
				fmt.Fprintf(w, "  type:       permanent IAM user key\n")
			}

			kc, err := r.mgmtKubeconfig()
			if err != nil {
				return err
			}
			if err := r.writeSecret(kc, c, w); err != nil {
				return err
			}
			fmt.Fprintln(w, "\nDone. Verify with: cub eksinf creds status")
			return nil
		},
	}
}

func newCredsRefreshCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "refresh",
		Short: "Re-issue credentials after `aws sso login` and restart the controllers",
		Long: `Re-issue credentials from your current identity and restart the controllers.

This is the SSO loop: 'aws sso login' refreshes your local session, this pushes
it into the cluster.

It is a separate verb because the restart is not optional. The AWS SDK reads the
credentials file once at startup, so updating the Secret alone changes the
mounted file but not the running controller — which looks like nothing
happening.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := requireTools("aws", "kubectl"); err != nil {
				return err
			}
			r := newRunner()
			w := cmd.OutOrStdout()

			c, err := r.exportCreds()
			if err != nil {
				return fmt.Errorf("%w\n  Has your SSO session expired? Try: aws sso login", err)
			}
			if _, err := r.callerIdentity(); err != nil {
				return fmt.Errorf("credentials are not valid. Try: aws sso login\n  %w", err)
			}
			fmt.Fprintf(w, "  access key: %s\n", c.AccessKeyID)
			if left, ok := remaining(c.Expiration); ok {
				fmt.Fprintf(w, "  expires in: %s\n", left)
			}
			kc, err := r.mgmtKubeconfig()
			if err != nil {
				return err
			}
			if err := r.writeSecret(kc, c, w); err != nil {
				return err
			}
			fmt.Fprintln(w, "\nDone. Reconciliation resumes as the controllers come back up.")
			return nil
		},
	}
}

func newCredsStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Report the credentials in the cluster and how long they last",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := requireTools("kubectl"); err != nil {
				return err
			}
			r := newRunner()
			w := cmd.OutOrStdout()

			kc, err := r.mgmtKubeconfig()
			if err != nil {
				return err
			}
			fmt.Fprintf(w, "  kubeconfig: %s\n", kc)

			raw, err := r.kubectl(kc, "get", "secret", credsSecret, "-n", ackNamespace,
				"-o", "jsonpath={.data.credentials}")
			if err != nil {
				fmt.Fprintf(w, "  Secret %s/%s: MISSING\n\n", ackNamespace, credsSecret)
				fmt.Fprintln(w, "  Run:  cub eksinf creds use-existing")
				return nil
			}
			decoded, err := base64Decode(raw)
			if err != nil {
				return err
			}
			fmt.Fprintf(w, "  Secret %s/%s: present\n", ackNamespace, credsSecret)
			for _, line := range strings.Split(decoded, "\n") {
				if strings.HasPrefix(line, "aws_access_key_id") {
					fmt.Fprintf(w, "  access key: %s\n", strings.TrimSpace(strings.SplitN(line, "=", 2)[1]))
				}
			}
			if strings.Contains(decoded, "aws_session_token") {
				fmt.Fprintln(w, "  type:       temporary session")
				exp, _ := r.kubectl(kc, "get", "secret", credsSecret, "-n", ackNamespace,
					"-o", "jsonpath={.metadata.annotations.eks-inference\\.confighub\\.com/expires-at}")
				if left, ok := remaining(strings.TrimSpace(exp)); ok {
					if left <= 0 {
						fmt.Fprintln(w, "  expires in: EXPIRED")
						fmt.Fprintln(w, "\n  The controllers cannot reach AWS; ACK resources show ACK.Recoverable")
						fmt.Fprintln(w, "  and reconciliation is paused. Nothing has been lost. To resume:")
						fmt.Fprintln(w, "    aws sso login && cub eksinf creds refresh")
					} else {
						fmt.Fprintf(w, "  expires in: %s\n", left.Round(time.Minute))
					}
				}
			} else {
				fmt.Fprintln(w, "  type:       long-lived access key")
			}

			fmt.Fprintln(w, "\n  controllers:")
			pods, err := r.kubectl(kc, "get", "pods", "-n", ackNamespace, "--no-headers")
			if err != nil {
				return err
			}
			for _, line := range strings.Split(strings.TrimSpace(pods), "\n") {
				if line != "" {
					fmt.Fprintf(w, "    %s\n", line)
				}
			}
			return nil
		},
	}
}

func remaining(expiration string) (time.Duration, bool) {
	if expiration == "" {
		return 0, false
	}
	t, err := time.Parse(time.RFC3339, expiration)
	if err != nil {
		return 0, false
	}
	return time.Until(t).Round(time.Minute), true
}

// base64Decode decodes the raw Secret data that kubectl jsonpath returns.
func base64Decode(s string) (string, error) {
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return "", fmt.Errorf("decoding secret data: %w", err)
	}
	return string(b), nil
}

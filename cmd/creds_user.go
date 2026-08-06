package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

const (
	defaultIAMUser = "eks-inference-ack"
	iamPolicyName  = "ack-controllers"
)

func newCredsCreateUserCmd() *cobra.Command {
	var userName string
	var yes bool

	c := &cobra.Command{
		Use:   "create-user",
		Short: "Create a dedicated IAM user for the ACK controllers and install its key",
		Long: `Create a scoped IAM user, attach the embedded policy, issue a long-lived access
key, and write it into the cluster.

Use this when you do not have a suitable key, or want this stack to hold one
that can be revoked on its own without affecting anything else.

An SSO session is perfectly good for RUNNING this — it only needs to last the few
seconds the IAM calls take, and the key it produces does not expire. So "I am on
SSO" and "I want a long-lived scoped key" are not in tension.

The policy is embedded in this binary from iam/ack-controllers-policy.json.
Inspect it with --show-policy before running.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := requireTools("aws", "kubectl"); err != nil {
				return err
			}
			r := newRunner()
			w := cmd.OutOrStdout()

			id, err := r.callerIdentity()
			if err != nil {
				return fmt.Errorf("no valid AWS credentials to create the user with: %w", err)
			}
			fmt.Fprintf(w, "  account:  %s\n  as:       %s\n", id.Account, id.Arn)
			fmt.Fprintf(w, "  IAM user: %s\n  policy:   %s (embedded)\n", userName, iamPolicyName)
			if !yes {
				return fmt.Errorf("re-run with --yes to create the user and its access key")
			}

			if _, err := r.aws("iam", "get-user", "--user-name", userName); err == nil {
				fmt.Fprintf(w, "  user %s already exists; reusing it\n", userName)
			} else {
				if _, err := r.aws("iam", "create-user", "--user-name", userName,
					"--tags", "Key=eks-inference.confighub.com/stack,Value=inference-demo"); err != nil {
					return fmt.Errorf("creating user: %w", err)
				}
				fmt.Fprintf(w, "  created user %s\n", userName)
			}

			// The AWS CLI wants a file:// URL for the policy document, and the
			// policy lives inside this binary — so write it to a temp file for
			// the duration of the call.
			tmp, err := os.CreateTemp("", "ack-policy-*.json")
			if err != nil {
				return err
			}
			defer os.Remove(tmp.Name())
			if _, err := tmp.Write(ackPolicy); err != nil {
				return err
			}
			tmp.Close()

			if _, err := r.aws("iam", "put-user-policy", "--user-name", userName,
				"--policy-name", iamPolicyName,
				"--policy-document", "file://"+tmp.Name()); err != nil {
				return fmt.Errorf("attaching policy: %w", err)
			}
			fmt.Fprintf(w, "  attached policy %s\n", iamPolicyName)

			// More than two access keys per user is an API error, and stale keys
			// from a previous run are the usual cause.
			if out, err := r.aws("iam", "list-access-keys", "--user-name", userName,
				"--query", "AccessKeyMetadata[].AccessKeyId", "--output", "text"); err == nil {
				for _, k := range strings.Fields(out) {
					if _, err := r.aws("iam", "delete-access-key",
						"--user-name", userName, "--access-key-id", k); err == nil {
						fmt.Fprintf(w, "  removed superseded key %s\n", k)
					}
				}
			}

			out, err := r.aws("iam", "create-access-key", "--user-name", userName, "--output", "json")
			if err != nil {
				return fmt.Errorf("creating access key: %w", err)
			}
			var res struct {
				AccessKey struct {
					AccessKeyID     string `json:"AccessKeyId"`
					SecretAccessKey string `json:"SecretAccessKey"`
				} `json:"AccessKey"`
			}
			if err := json.Unmarshal([]byte(out), &res); err != nil {
				return err
			}
			key := res.AccessKey.AccessKeyID
			fmt.Fprintf(w, "  created access key %s\n", key)

			// IAM is eventually consistent: a brand new key is routinely
			// rejected for the first several seconds. Fail only if it never
			// becomes usable.
			fmt.Fprintln(w, "  waiting for the key to become usable")
			live := false
			probe := newRunner().withEnv(
				"AWS_ACCESS_KEY_ID="+key,
				"AWS_SECRET_ACCESS_KEY="+res.AccessKey.SecretAccessKey,
				"AWS_SESSION_TOKEN=",
			)
			for range 12 {
				if _, err := probe.run("aws", "sts", "get-caller-identity"); err == nil {
					live = true
					break
				}
				time.Sleep(5 * time.Second)
			}
			if !live {
				return fmt.Errorf("key %s never became usable (IAM propagation)", key)
			}
			fmt.Fprintln(w, "  key is live")

			kc, err := r.mgmtKubeconfigForWrite(w)
			if err != nil {
				return err
			}
			if err := r.writeSecret(kc, &exportedCreds{
				AccessKeyID:     key,
				SecretAccessKey: res.AccessKey.SecretAccessKey,
			}, w); err != nil {
				return err
			}
			fmt.Fprintln(w, "\nDone. Verify with: cub eksinf creds status")
			fmt.Fprintln(w, "Remove it later with: cub eksinf creds delete-user --yes")
			return nil
		},
	}
	c.Flags().StringVar(&userName, "user-name", defaultIAMUser, "IAM user to create")
	c.Flags().BoolVar(&yes, "yes", false, "proceed without confirmation")
	return c
}

func newCredsDeleteUserCmd() *cobra.Command {
	var userName string
	var yes bool

	c := &cobra.Command{
		Use:   "delete-user",
		Short: "Delete the dedicated IAM user, its policy, and its access keys",
		Long: `Delete the IAM user this stack created, its inline policy, and all its keys.

This does NOT delete any AWS resources the stack provisioned — the ACK
controllers run with deletionPolicy: retain, so teardown is a separate,
deliberate operation.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := requireTools("aws"); err != nil {
				return err
			}
			r := newRunner()
			w := cmd.OutOrStdout()

			if _, err := r.aws("iam", "get-user", "--user-name", userName); err != nil {
				return fmt.Errorf("no IAM user named %s", userName)
			}
			if !yes {
				fmt.Fprintf(w, "This deletes IAM user %s, its policy, and all its access keys.\n", userName)
				fmt.Fprintln(w, "It does NOT delete AWS resources the stack created.")
				return fmt.Errorf("re-run with --yes to proceed")
			}

			if out, err := r.aws("iam", "list-access-keys", "--user-name", userName,
				"--query", "AccessKeyMetadata[].AccessKeyId", "--output", "text"); err == nil {
				for _, k := range strings.Fields(out) {
					if _, err := r.aws("iam", "delete-access-key",
						"--user-name", userName, "--access-key-id", k); err == nil {
						fmt.Fprintf(w, "  deleted key %s\n", k)
					}
				}
			}
			_, _ = r.aws("iam", "delete-user-policy", "--user-name", userName, "--policy-name", iamPolicyName)
			if _, err := r.aws("iam", "delete-user", "--user-name", userName); err != nil {
				return fmt.Errorf("deleting user: %w", err)
			}
			fmt.Fprintf(w, "  deleted user %s\n", userName)

			if kc, err := r.mgmtKubeconfig(); err == nil {
				if _, err := r.kubectl(kc, "delete", "secret", credsSecret, "-n", ackNamespace); err == nil {
					fmt.Fprintf(w, "  deleted Secret %s/%s\n", ackNamespace, credsSecret)
				}
			}
			return nil
		},
	}
	c.Flags().StringVar(&userName, "user-name", defaultIAMUser, "IAM user to delete")
	c.Flags().BoolVar(&yes, "yes", false, "proceed without confirmation")
	return c
}

func newCredsShowPolicyCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show-policy",
		Short: "Print the IAM policy that create-user attaches",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := cmd.OutOrStdout().Write(ackPolicy)
			return err
		},
	}
}

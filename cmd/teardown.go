package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// Teardown order, outermost dependency first. AWS will refuse to delete a VPC
// while anything still holds an ENI in it, so this is not cosmetic: getting it
// wrong strands a VPC that then cannot be removed until you find the resource
// still pinning it.
//
// Each stage is a set of ACK kinds deleted together, then waited on. Kinds
// within a stage have no ordering constraint between them.
var teardownStages = []struct {
	Name  string
	Kinds []string
	// Wait is how long to allow this stage before giving up. EKS control planes
	// and NAT gateways are slow; IAM is immediate.
	Wait time.Duration
}{
	{"nodegroups", []string{"nodegroup.eks.services.k8s.aws"}, 15 * time.Minute},
	{"addons and pod identity", []string{
		"addon.eks.services.k8s.aws",
		"podidentityassociation.eks.services.k8s.aws",
	}, 5 * time.Minute},
	{"EKS control plane", []string{"cluster.eks.services.k8s.aws"}, 20 * time.Minute},
	// The NAT gateway holds an ENI in a public subnet, so it must go before the
	// subnets. Its Elastic IP must go after it — releasing an EIP still attached
	// to a live NAT gateway fails, and an EIP left behind bills forever.
	{"NAT gateway", []string{"natgateway.ec2.services.k8s.aws"}, 10 * time.Minute},
	{"elastic IP", []string{"elasticipaddress.ec2.services.k8s.aws"}, 5 * time.Minute},
	{"subnets and route tables", []string{
		"subnet.ec2.services.k8s.aws",
		"routetable.ec2.services.k8s.aws",
	}, 10 * time.Minute},
	{"internet gateway and security group", []string{
		"internetgateway.ec2.services.k8s.aws",
		"securitygroup.ec2.services.k8s.aws",
	}, 10 * time.Minute},
	{"VPC", []string{"vpc.ec2.services.k8s.aws"}, 10 * time.Minute},
	{"IAM roles", []string{"role.iam.services.k8s.aws"}, 5 * time.Minute},
}

func newTeardownCmd() *cobra.Command {
	var yes, keepIAM bool
	var region string

	c := &cobra.Command{
		Use:   "teardown",
		Short: "Destroy the AWS resources this stack created",
		Long: `Destroy the AWS infrastructure, in dependency order, and verify afterwards
against EC2.

THIS IS NOT THE OPPOSITE OF deploy. The ACK controllers run with
deletionPolicy: retain, so deleting Units — or letting Argo prune them — removes
the Kubernetes objects and leaves the AWS resources running and billing. That is
deliberate: it makes an accidental prune inert. The cost is that teardown has to
be explicit, and this is it.

Each resource is annotated with services.k8s.aws/deletion-policy=delete before
being deleted, which is what actually authorises the controller to destroy the
AWS side.

Order matters and is not cosmetic. AWS refuses to delete a VPC while anything
holds an ENI in it, so a wrong order strands a VPC you then have to hunt down by
hand. The Elastic IP is released AFTER the NAT gateway for the same reason, and
because an EIP left behind bills quietly forever — the most common leftover of a
hand-rolled teardown.

What this does NOT delete: the kind cluster, the EKS cluster's ConfigHub Spaces,
the IAM user created by 'creds create-user', or anything Karpenter provisioned
that is not part of the declared config. Scale workloads to 0 first so Karpenter
releases its nodes; a node it still owns will block subnet deletion.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := requireTools("kubectl", "aws"); err != nil {
				return err
			}
			r := newRunner()
			w := cmd.OutOrStdout()

			kc, err := r.mgmtKubeconfig()
			if err != nil {
				return err
			}

			present, err := r.teardownInventory(kc)
			if err != nil {
				return err
			}
			if len(present) == 0 {
				fmt.Fprintln(w, "Nothing to tear down: no ACK resources in aws-inference.")
				return r.verifyNothingLeft(region, w)
			}

			fmt.Fprintf(w, "This will DESTROY %d AWS resource(s) created by this stack:\n\n", len(present))
			for _, p := range present {
				fmt.Fprintf(w, "  %s\n", p)
			}
			fmt.Fprintln(w, "\nThe EKS cluster, its nodes, the VPC, and the NAT gateway are included.")
			if keepIAM {
				fmt.Fprintln(w, "IAM roles will be KEPT (--keep-iam).")
			}
			if !yes {
				fmt.Fprintln(w, "\nRe-run with --yes to proceed.")
				return nil
			}

			for _, stage := range teardownStages {
				if keepIAM && stage.Name == "IAM roles" {
					fmt.Fprintln(w, "==> IAM roles: skipped (--keep-iam)")
					continue
				}
				if err := r.teardownStage(kc, stage.Name, stage.Kinds, stage.Wait, w); err != nil {
					return err
				}
			}

			fmt.Fprintln(w, "\n==> verifying against EC2")
			return r.verifyNothingLeft(region, w)
		},
	}

	c.Flags().BoolVar(&yes, "yes", false, "actually destroy (without this, only report what would go)")
	c.Flags().BoolVar(&keepIAM, "keep-iam", false, "keep the IAM roles")
	c.Flags().StringVar(&region, "region", "us-west-2", "AWS region to verify against")
	return c
}

// teardownInventory lists what actually exists, so the confirmation prompt names
// real resources rather than a generic warning.
func (r *runner) teardownInventory(kc string) ([]string, error) {
	var kinds []string
	for _, s := range teardownStages {
		kinds = append(kinds, s.Kinds...)
	}
	out, err := r.kubectl(kc, "get", strings.Join(kinds, ","), "-n", "aws-inference",
		"-o", "json")
	if err != nil {
		// A missing CRD means that controller was never installed, which is not
		// an error here. Anything else is.
		if strings.Contains(err.Error(), "the server doesn't have a resource type") {
			return nil, nil
		}
		return nil, fmt.Errorf("listing ACK resources: %w", err)
	}
	var parsed struct {
		Items []struct {
			Kind     string `json:"kind"`
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		return nil, err
	}
	var names []string
	for _, it := range parsed.Items {
		names = append(names, it.Kind+"/"+it.Metadata.Name)
	}
	return names, nil
}

func (r *runner) teardownStage(kc, name string, kinds []string, wait time.Duration, w io.Writer) error {
	list, err := r.kubectl(kc, "get", strings.Join(kinds, ","), "-n", "aws-inference",
		"-o", "name")
	if err != nil || strings.TrimSpace(list) == "" {
		fmt.Fprintf(w, "==> %s: none\n", name)
		return nil
	}
	resources := strings.Fields(strings.TrimSpace(list))
	fmt.Fprintf(w, "==> %s (%d)\n", name, len(resources))

	// Authorise destruction BEFORE deleting. Without this the controller honours
	// its retain default: the Kubernetes object disappears and the AWS resource
	// stays, which is the failure this command exists to prevent.
	for _, res := range resources {
		if _, err := r.kubectl(kc, "annotate", res, "-n", "aws-inference", "--overwrite",
			"services.k8s.aws/deletion-policy=delete"); err != nil {
			return fmt.Errorf("annotating %s for deletion: %w", res, err)
		}
	}
	for _, res := range resources {
		if _, err := r.kubectl(kc, "delete", res, "-n", "aws-inference", "--wait=false"); err != nil {
			return fmt.Errorf("deleting %s: %w", res, err)
		}
		fmt.Fprintf(w, "    deleting %s\n", res)
	}

	deadline := time.Now().Add(wait)
	for {
		remaining, err := r.kubectl(kc, "get", strings.Join(kinds, ","), "-n", "aws-inference", "-o", "name")
		if err != nil {
			return fmt.Errorf("checking %s: %w", name, err)
		}
		n := len(strings.Fields(strings.TrimSpace(remaining)))
		if n == 0 {
			fmt.Fprintf(w, "    gone\n")
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf(
				"%s: %d resource(s) still present after %s.\n"+
					"Check for an ACK.Terminal condition:\n"+
					"  kubectl describe %s -n aws-inference",
				name, n, wait, resources[0])
		}
		fmt.Fprintf(w, "    %d remaining…\n", n)
		time.Sleep(20 * time.Second)
	}
}

// verifyNothingLeft checks AWS directly. The Kubernetes view is not evidence:
// an object can outlive its resource, and an unreachable cluster reports zero.
func (r *runner) verifyNothingLeft(region string, w io.Writer) error {
	type check struct {
		label string
		args  []string
	}
	checks := []check{
		{"EKS clusters", []string{"eks", "list-clusters", "--query", "clusters", "--output", "text"}},
		{"stack VPCs", []string{"ec2", "describe-vpcs",
			"--filters", "Name=tag:eks-inference.confighub.com/stack,Values=inference-demo",
			"--query", "Vpcs[].VpcId", "--output", "text"}},
		{"NAT gateways", []string{"ec2", "describe-nat-gateways",
			"--filter", "Name=state,Values=available,pending",
			"--query", "NatGateways[].NatGatewayId", "--output", "text"}},
		{"Karpenter instances", []string{"ec2", "describe-instances",
			"--filters", "Name=instance-state-name,Values=running,pending",
			"Name=tag-key,Values=karpenter.sh/nodepool",
			"--query", "Reservations[].Instances[].InstanceId", "--output", "text"}},
	}

	clean := true
	for _, c := range checks {
		out, err := r.aws(append(c.args, "--region", region)...)
		if err != nil {
			fmt.Fprintf(w, "  !! could not check %s — status UNKNOWN, not clean:\n     %v\n", c.label, err)
			clean = false
			continue
		}
		v := strings.TrimSpace(out)
		if v == "" || v == "None" {
			fmt.Fprintf(w, "  %-20s none\n", c.label)
		} else {
			fmt.Fprintf(w, "  %-20s STILL PRESENT: %s\n", c.label, v)
			clean = false
		}
	}

	// Unassociated Elastic IPs bill and are the classic leftover: releasing a NAT
	// gateway does not release its address. Reported separately because an EIP
	// can survive every other check passing.
	out, err := r.aws("ec2", "describe-addresses", "--region", region,
		"--query", "Addresses[?AssociationId==null].[AllocationId,Tags[?Key=='Name']|[0].Value]",
		"--output", "text")
	if err != nil {
		fmt.Fprintf(w, "  !! could not check Elastic IPs — status UNKNOWN\n")
		clean = false
	} else if v := strings.TrimSpace(out); v != "" {
		fmt.Fprintf(w, "  idle Elastic IPs     %s\n", strings.ReplaceAll(v, "\n", "; "))
		fmt.Fprintln(w, "     (these bill while unassociated; release any that belong to this stack)")
	} else {
		fmt.Fprintf(w, "  %-20s none\n", "idle Elastic IPs")
	}

	fmt.Fprintln(w)
	if clean {
		fmt.Fprintln(w, "Nothing from this stack is running in AWS.")
		fmt.Fprintln(w, "The kind cluster and the ConfigHub Spaces are untouched:")
		fmt.Fprintln(w, "  cub cluster down <name>            remove the management cluster")
		fmt.Fprintln(w, "  cub eksinf enroll remove --name …  unenroll the EKS cluster")
		fmt.Fprintln(w, "  cub eksinf creds delete-user --yes remove the IAM user")
		return nil
	}
	return fmt.Errorf("teardown incomplete — see above")
}

package cmd

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

const ackDeletionPolicyAnnotation = "services.k8s.aws/deletion-policy"

func newTeardownCmd() *cobra.Command {
	var planeFlag, region string
	var yes, deleteConfig bool

	c := &cobra.Command{
		Use:   "teardown",
		Short: "Destroy the AWS resources this stack created, in the correct order",
		Long: `Destroy the stack's AWS infrastructure and verify against EC2.

With no --plane, both planes run in the only safe order: workload, then mgmt.
The mgmt plane owns the EKS cluster the workload plane runs on, and the ACK
controllers that destroy AWS resources live in the kind cluster — so once the
mgmt plane is gone, nothing can clean up through ACK any more.

THIS IS NOT THE OPPOSITE OF deploy, and it cannot be done by deleting Kubernetes
objects. Three separate mechanisms prevent that:

  - Argo owns these objects and syncs with selfHeal, so a delete is drift and is
    restored within about a minute. It even looks like it worked in between.
  - The generated Applications do not prune, so removing Units from a bundle
    does not remove the objects either.
  - The ACK controllers run with deletionPolicy: retain, so even a delete that
    survives leaves the AWS resource running.

So teardown goes THROUGH config, not around it. Authorising deletion is itself a
config change — an annotation set on the resources, published, and applied by
Argo — which makes it reviewable like everything else here. Only then is Argo
detached and the objects removed.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := requireTools("cub", "kubectl", "aws"); err != nil {
				return err
			}
			r := newRunner()
			if err := r.requireConfigHubAuth(); err != nil {
				return err
			}
			w := cmd.OutOrStdout()

			planes := []Plane{PlaneWorkload, PlaneMgmt}
			if planeFlag != "" {
				p, err := ParsePlane(planeFlag)
				if err != nil {
					return err
				}
				if p == PlaneHub {
					return fmt.Errorf("the hub plane creates nothing to tear down; use --delete-config to remove its Spaces")
				}
				planes = []Plane{p}
			}

			fmt.Fprintf(w, "Teardown plan: %v\n", planes)
			if deleteConfig {
				fmt.Fprintln(w, "ConfigHub Spaces will be deleted afterwards (--delete-config).")
			}
			if !yes {
				fmt.Fprintln(w, "\nThis destroys real AWS infrastructure. Re-run with --yes to proceed.")
				return nil
			}

			for _, p := range planes {
				switch p {
				case PlaneWorkload:
					if err := r.teardownWorkload(region, w); err != nil {
						return err
					}
				case PlaneMgmt:
					if err := r.teardownMgmt(region, w); err != nil {
						return err
					}
				}
			}

			fmt.Fprintln(w, "\n==> verifying against EC2")
			if err := r.verifyNothingLeft(region, w); err != nil {
				return err
			}

			if deleteConfig {
				return r.deleteConfig(planes, w)
			}
			fmt.Fprintln(w, "\nConfigHub Spaces kept. Remove them with --delete-config,")
			fmt.Fprintln(w, "or redeploy from the bases with: cub eksinf deploy --plane mgmt --target …")
			return nil
		},
	}

	c.Flags().StringVar(&planeFlag, "plane", "", "only this plane (workload or mgmt); default is both, in order")
	c.Flags().BoolVar(&yes, "yes", false, "actually destroy")
	c.Flags().BoolVar(&deleteConfig, "delete-config", false, "also delete the ConfigHub Spaces afterwards")
	c.Flags().StringVar(&region, "region", "us-west-2", "AWS region to verify against")
	return c
}

// teardownWorkload drains Karpenter. This plane creates no AWS resources of its
// own — Karpenter does, on its behalf.
//
// This step is not optional and not merely tidy. Karpenter's instances are plain
// EC2: no eks:nodegroup-name tag, no association with the cluster's lifecycle.
// Deleting the EKS control plane does NOT terminate them. Skipping this leaves
// GPU instances running against a control plane that no longer exists, billing,
// with nothing left that knows they belong to anything.
func (r *runner) teardownWorkload(region string, w io.Writer) error {
	fmt.Fprintln(w, "\n══ workload plane: draining Karpenter")

	space := variantSpace("inference-workloads", flagVariant)
	has, err := r.spaceExists(space)
	if err != nil {
		return fmt.Errorf("checking %s: %w", space, err)
	}
	if !has {
		fmt.Fprintf(w, "  %s not deployed; nothing to drain\n", space)
	} else {
		// Through config, because Argo would revert a kubectl scale.
		if _, err := r.cub("function", "do", "--space", space, "set-replicas", "0"); err != nil {
			return fmt.Errorf("scaling workloads to 0: %w", err)
		}
		if _, err := r.publishRelease(space); err != nil {
			return fmt.Errorf("publishing %s: %w", space, err)
		}
		fmt.Fprintln(w, "  scaled all workloads to 0 and published")
	}

	fmt.Fprintln(w, "  waiting for Karpenter to release its nodes (checking EC2, not Kubernetes)")
	deadline := time.Now().Add(15 * time.Minute)
	for {
		out, err := r.aws("ec2", "describe-instances", "--region", region,
			"--filters", "Name=instance-state-name,Values=running,pending",
			"Name=tag-key,Values=karpenter.sh/nodepool",
			"--query", "Reservations[].Instances[].InstanceId", "--output", "text")
		if err != nil {
			return fmt.Errorf("checking for Karpenter instances: %w", err)
		}
		ids := strings.Fields(strings.TrimSpace(out))
		if len(ids) == 0 {
			fmt.Fprintln(w, "  no Karpenter instances remain")
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf(
				"%d Karpenter instance(s) still running after 15m: %s\n"+
					"Deleting the EKS cluster would orphan these. Investigate before continuing",
				len(ids), strings.Join(ids, " "))
		}
		fmt.Fprintf(w, "    %d still running…\n", len(ids))
		time.Sleep(30 * time.Second)
	}
}

// teardownMgmt destroys the AWS resources: authorise through config, detach Argo,
// then delete.
func (r *runner) teardownMgmt(region string, w io.Writer) error {
	fmt.Fprintln(w, "\n══ mgmt plane: destroying AWS resources")

	mgmt, _ := r.discoverClusters()
	if mgmt == "" {
		return fmt.Errorf(
			"no reachable cluster with an ack-system namespace.\n" +
				"The ACK controllers are the only thing that can destroy these resources through\n" +
				"config; without them this has to be done in the AWS console")
	}
	kc := cubClusterKubeconfig(mgmt)

	// 1. Authorise, through config. The controllers default to retain, so without
	// this a delete removes the Kubernetes object and leaves the AWS resource.
	// Setting it as config rather than a manual annotation matters: Argo would
	// revert an annotation applied behind its back.
	fmt.Fprintln(w, "  authorising deletion (config change, published, applied by Argo)")
	for _, comp := range ComponentsInPlane(PlaneMgmt) {
		if comp.Name == "ack-controllers" {
			continue // the controllers themselves are not AWS resources
		}
		space := variantSpace(comp.Name, flagVariant)
		has, err := r.spaceExists(space)
		if err != nil {
			return fmt.Errorf("checking %s: %w", space, err)
		}
		if !has {
			continue
		}
		if _, err := r.cub("function", "do", "--space", space,
			"set-annotation", ackDeletionPolicyAnnotation, "delete"); err != nil {
			return fmt.Errorf("authorising deletion in %s: %w", space, err)
		}
		if _, err := r.publishRelease(space); err != nil {
			return fmt.Errorf("publishing %s: %w", space, err)
		}
		fmt.Fprintf(w, "    %s\n", space)
	}

	// Wait for Argo to actually apply it. Deleting before the annotation lands
	// would silently fall back to retain — the exact failure this guards.
	fmt.Fprintln(w, "  waiting for the annotation to reach the cluster")
	if err := r.waitForDeletionPolicy(kc, 5*time.Minute, w); err != nil {
		return err
	}

	// 2. Detach Argo. Until this happens, selfHeal restores anything deleted —
	// which makes a teardown look like it worked for about one minute.
	fmt.Fprintln(w, "  detaching Argo (otherwise selfHeal restores everything)")
	for _, comp := range ComponentsInPlane(PlaneMgmt) {
		app := variantSpace(comp.Name, flagVariant)
		if _, err := r.kubectl(kc, "get", "application", app, "-n", "argocd"); err != nil {
			continue
		}
		// Strip the finalizer first: a plain delete cascades and would delete the
		// managed resources through Argo rather than through ACK, bypassing the
		// deletion policy entirely.
		if _, err := r.kubectl(kc, "patch", "application", app, "-n", "argocd",
			"--type=merge", "-p", `{"metadata":{"finalizers":[]}}`); err != nil {
			return fmt.Errorf("removing finalizer from %s: %w", app, err)
		}
		if _, err := r.kubectl(kc, "delete", "application", app, "-n", "argocd",
			"--cascade=orphan"); err != nil {
			return fmt.Errorf("deleting Application %s: %w", app, err)
		}
		fmt.Fprintf(w, "    detached %s\n", app)
	}

	// 3. Delete. ACK retries a deletion whose dependencies are not yet gone, the
	// same way it requeues a reference it cannot resolve during creation, so
	// exact ordering is advisory — it converges, noisily.
	fmt.Fprintln(w, "  deleting ACK resources")
	kinds := "nodegroup.eks.services.k8s.aws,addon.eks.services.k8s.aws," +
		"podidentityassociation.eks.services.k8s.aws,cluster.eks.services.k8s.aws," +
		"natgateway.ec2.services.k8s.aws,elasticipaddress.ec2.services.k8s.aws," +
		"subnet.ec2.services.k8s.aws,routetable.ec2.services.k8s.aws," +
		"internetgateway.ec2.services.k8s.aws,securitygroup.ec2.services.k8s.aws," +
		"vpc.ec2.services.k8s.aws,role.iam.services.k8s.aws"
	if _, err := r.kubectl(kc, "delete", kinds, "-n", "aws-inference",
		"--all", "--wait=false"); err != nil {
		return fmt.Errorf("deleting ACK resources: %w", err)
	}

	fmt.Fprintln(w, "  waiting for AWS to converge (this takes ~20 minutes)")
	deadline := time.Now().Add(35 * time.Minute)
	for {
		out, err := r.kubectl(kc, "get", kinds, "-n", "aws-inference", "-o", "name")
		if err != nil {
			return fmt.Errorf("checking remaining resources: %w", err)
		}
		n := len(strings.Fields(strings.TrimSpace(out)))
		if n == 0 {
			fmt.Fprintln(w, "  all ACK resources gone")
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%d ACK resource(s) still present after 35m.\n"+
				"Check for a terminal condition:  kubectl get %s -n aws-inference", n, kinds)
		}
		fmt.Fprintf(w, "    %d remaining…\n", n)
		time.Sleep(30 * time.Second)
	}
}

// waitForDeletionPolicy blocks until Argo has applied the authorising annotation
// to the VPC, which is the last resource to be destroyed and therefore a
// reasonable proxy for the whole set.
func (r *runner) waitForDeletionPolicy(kc string, wait time.Duration, w io.Writer) error {
	deadline := time.Now().Add(wait)
	for {
		out, err := r.kubectl(kc, "get", "vpc", "-n", "aws-inference",
			"-o", `jsonpath={.items[*].metadata.annotations.services\.k8s\.aws/deletion-policy}`)
		if err == nil && strings.Contains(out, "delete") {
			fmt.Fprintln(w, "    annotation applied")
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf(
				"the deletion-policy annotation has not reached the cluster after %s.\n"+
					"Deleting now would silently fall back to retain and leave the AWS resources running.\n"+
					"Check that Argo is syncing:  kubectl get applications -n argocd", wait)
		}
		time.Sleep(15 * time.Second)
	}
}

// deleteConfig removes the ConfigHub Spaces. Links first, then Units, then the
// Space — a Space with either still attached refuses to delete.
func (r *runner) deleteConfig(planes []Plane, w io.Writer) error {
	fmt.Fprintln(w, "\n══ deleting ConfigHub configuration")

	var spaces []string
	for _, p := range planes {
		for _, comp := range ComponentsInPlane(p) {
			spaces = append(spaces, variantSpace(comp.Name, flagVariant))
		}
	}
	// The profile goes last: other Spaces' links point at it, and a Space with
	// inbound links cannot be deleted.
	spaces = append(spaces, variantSpace(profileComponent, flagVariant))

	for _, space := range spaces {
		has, err := r.spaceExists(space)
		if err != nil {
			return fmt.Errorf("checking %s: %w", space, err)
		}
		if !has {
			continue
		}
		for _, l := range r.listNames(space, "link") {
			_, _ = r.cub("link", "delete", "--space", space, l)
		}
		for _, u := range r.listNames(space, "unit") {
			_, _ = r.cub("unit", "delete", "--space", space, u)
		}
		if _, err := r.cub("space", "delete", space); err != nil {
			fmt.Fprintf(w, "  could not delete %s: %v\n", space, err)
			continue
		}
		fmt.Fprintf(w, "  deleted %s\n", space)
	}

	fmt.Fprintln(w, "\nBases are untouched — redeploy with 'cub eksinf deploy'.")
	fmt.Fprintln(w, "Still yours to remove if you want them gone:")
	fmt.Fprintln(w, "  cub cluster down <name>              the kind cluster")
	fmt.Fprintln(w, "  cub eksinf creds delete-user --yes   the IAM user")
	return nil
}

func (r *runner) listNames(space, entity string) []string {
	out, err := r.cub(entity, "list", "--space", space, "--no-headers")
	if err != nil {
		return nil
	}
	var names []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if f := strings.Fields(line); len(f) > 0 {
			names = append(names, f[0])
		}
	}
	return names
}

// verifyNothingLeft checks AWS directly. The Kubernetes view is not evidence: an
// object can outlive its resource, and an unreachable cluster reports zero.
func (r *runner) verifyNothingLeft(region string, w io.Writer) error {
	checks := []struct {
		label string
		args  []string
	}{
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
			fmt.Fprintf(w, "  !! could not check %s — UNKNOWN, not clean: %v\n", c.label, err)
			clean = false
			continue
		}
		if v := strings.TrimSpace(out); v == "" || v == "None" {
			fmt.Fprintf(w, "  %-20s none\n", c.label)
		} else {
			fmt.Fprintf(w, "  %-20s STILL PRESENT: %s\n", c.label, v)
			clean = false
		}
	}

	// Idle Elastic IPs bill and are the classic leftover: releasing a NAT gateway
	// does not release its address. Reported separately because one can survive
	// every other check passing.
	out, err := r.aws("ec2", "describe-addresses", "--region", region,
		"--query", "Addresses[?AssociationId==null].[AllocationId,Tags[?Key=='Name']|[0].Value]",
		"--output", "text")
	switch {
	case err != nil:
		fmt.Fprintln(w, "  !! could not check Elastic IPs — UNKNOWN")
		clean = false
	case strings.TrimSpace(out) != "":
		fmt.Fprintf(w, "  idle Elastic IPs     %s\n", strings.ReplaceAll(strings.TrimSpace(out), "\n", "; "))
		fmt.Fprintln(w, "     (these bill while unassociated; release any belonging to this stack)")
	default:
		fmt.Fprintf(w, "  %-20s none\n", "idle Elastic IPs")
	}

	if !clean {
		return fmt.Errorf("teardown incomplete — see above")
	}
	fmt.Fprintln(w, "\nNothing from this stack is running in AWS.")
	return nil
}

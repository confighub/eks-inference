package cmd

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

const ackDeletionPolicyAnnotation = "services.k8s.aws/deletion-policy"

// stackTagFilter selects the resources aws-network tags as belonging to this
// stack. Both the "is anything left" check and the orphan sweeps scope on it.
const stackTagFilter = "Name=tag:eks-inference.confighub.com/stack,Values=inference-demo"

// eksClusterSGPrefix is how EKS names the security group it creates per cluster.
const eksClusterSGPrefix = "eks-cluster-sg-inference-demo"

// stallSweepEvery is how many consecutive no-progress polls (30s each) pass
// before sweeping for orphans, and between repeat sweeps. Six is three minutes:
// long enough not to fire on ACK's ordinary backoff, short enough to leave most
// of the 35-minute budget for the delete to finish afterwards.
const stallSweepEvery = 6

// Every ACK kind this stack creates. The annotation wait and the delete MUST
// use the same list: a kind checked but not deleted leaks, and a kind deleted
// but not checked is deleted while possibly still set to retain.
const ackKinds = "nodegroup.eks.services.k8s.aws,addon.eks.services.k8s.aws," +
	"podidentityassociation.eks.services.k8s.aws,cluster.eks.services.k8s.aws," +
	"natgateway.ec2.services.k8s.aws,elasticipaddress.ec2.services.k8s.aws," +
	"subnet.ec2.services.k8s.aws,routetable.ec2.services.k8s.aws," +
	"internetgateway.ec2.services.k8s.aws,securitygroup.ec2.services.k8s.aws," +
	"vpc.ec2.services.k8s.aws,role.iam.services.k8s.aws"

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

			// Verify the CONTROLLERS can reach AWS before deleting anything.
			//
			// This is the check whose absence orphaned eight resources: the ACK
			// credentials had expired, so deleting an object removed it from
			// Kubernetes while the AWS resource survived — and once the object is
			// gone, ACK can no longer destroy what it created. Recovery is by hand
			// in the console. Checking the local CLI is not enough; it is the
			// Secret mounted into the controllers that has to be live.
			if err := r.requireControllerCredentials(w); err != nil {
				return err
			}

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
		// No management cluster. That is either "already torn down" or "the only
		// thing that could clean this up is gone" — and those are very different.
		// Ask AWS rather than assuming either.
		if err := r.verifyNothingLeft(region, w); err == nil {
			fmt.Fprintln(w, "  no management cluster, and nothing left in AWS — already torn down")
			return nil
		}
		return fmt.Errorf(
			"no reachable cluster with an ack-system namespace, but AWS resources still exist (above).\n" +
				"The ACK controllers are the only thing that can destroy these through config.\n" +
				"Either bring the management cluster back, or remove them in the AWS console")
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

	// 2. Detach Argo — ALL of it, root included.
	//
	// Deleting the child Applications is not enough. They are managed by the root
	// app-of-apps, which recreates them, which recreates the ACK objects, which
	// recreates the AWS resources. That is not theoretical: it rebuilt a VPC and
	// a NAT gateway during a teardown that had already deleted them.
	//
	// Stopping the application controller is the one action that reliably halts
	// every reconciliation loop at once, including the root's. Argo is going away
	// with the cluster anyway, so there is nothing to preserve.
	fmt.Fprintln(w, "  halting Argo (selfHeal and the root app-of-apps both restore deletions)")
	if _, err := r.kubectl(kc, "scale", "statefulset", "argocd-application-controller",
		"-n", "argocd", "--replicas=0"); err != nil {
		return fmt.Errorf("stopping the Argo application controller: %w", err)
	}
	if err := r.waitForArgoStopped(kc, 3*time.Minute, w); err != nil {
		return err
	}

	// 3. Delete. ACK retries a deletion whose dependencies are not yet gone, the
	// same way it requeues a reference it cannot resolve during creation, so
	// exact ordering is advisory — it converges, noisily.
	fmt.Fprintln(w, "  deleting ACK resources")
	if _, err := r.kubectl(kc, "delete", ackKinds, "-n", "aws-inference",
		"--all", "--wait=false"); err != nil {
		return fmt.Errorf("deleting ACK resources: %w", err)
	}

	// Karpenter creates an instance profile per EC2NodeClass when spec.role is
	// used, and nothing in the config owns it. It holds the node role, so the
	// role's deletion fails with DeleteConflict until the profile is gone.
	if err := r.releaseKarpenterInstanceProfiles(w); err != nil {
		return err
	}

	fmt.Fprintln(w, "  waiting for AWS to converge (this takes ~20 minutes)")
	deadline := time.Now().Add(35 * time.Minute)
	prev, stalled := -1, 0
	for {
		out, err := r.kubectl(kc, "get", ackKinds, "-n", "aws-inference", "-o", "name")
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
				"Check for a terminal condition:  kubectl get %s -n aws-inference", n, ackKinds)
		}

		// Progress is the signal. ACK converges noisily but it does converge, so
		// a count that stops moving means something is blocking a delete that
		// ACK cannot clear on its own — always a DependencyViolation, always
		// something no Unit describes. Sweep for those, rather than sitting out
		// the full 35 minutes waiting for a retry that cannot succeed.
		if n == prev {
			stalled++
		} else {
			prev, stalled = n, 0
		}
		if stalled > 0 && stalled%stallSweepEvery == 0 {
			fmt.Fprintf(w, "    no progress for %s; sweeping for orphans that block deletion\n",
				time.Duration(stalled)*30*time.Second)
			r.releaseOrphanedENIs(region, w)
			r.releaseClusterSecurityGroups(region, w)
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
	var missing []string
	for {
		// EVERY resource, not just the VPC. Argo patches these objects one at a
		// time, so seeing the annotation on one says nothing about the rest —
		// and the next step halts Argo, freezing whatever has not been patched
		// yet at the default of retain. That is not hypothetical: checking only
		// the VPC left three private subnets, a route table and a security
		// group behind, because ACK dropped their finalizers without deleting
		// the AWS resources, which puts them beyond ACK's reach entirely.
		out, err := r.kubectl(kc, "get", ackKinds, "-n", "aws-inference", "-o",
			`jsonpath={range .items[*]}{.kind}/{.metadata.name}={.metadata.annotations.services\.k8s\.aws/deletion-policy}{"\n"}{end}`)
		if err == nil {
			missing = nil
			total := 0
			for _, line := range strings.Split(out, "\n") {
				line = strings.TrimSpace(line)
				if line == "" {
					continue
				}
				total++
				if name, policy, _ := strings.Cut(line, "="); policy != "delete" {
					missing = append(missing, name)
				}
			}
			if total > 0 && len(missing) == 0 {
				fmt.Fprintf(w, "    annotation applied to all %d resource(s)\n", total)
				return nil
			}
			if total == 0 {
				fmt.Fprintln(w, "    no ACK resources present")
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf(
				"the deletion-policy annotation has not reached %d resource(s) after %s: %s\n"+
					"Deleting now would silently fall back to retain, and ACK would drop their\n"+
					"finalizers without deleting the AWS resources — which cannot be undone through ACK.\n"+
					"Check that Argo is syncing:  kubectl get applications -n argocd",
				len(missing), wait, strings.Join(missing, ", "))
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

	// --recursive removes the Space's Releases, Tags, Links, and Units for us.
	// Doing it by hand means discovering that dependency chain one 400 at a time,
	// and getting the order wrong.
	//
	// NOT --recursive-force: that ignores delete gates, and a delete gate is
	// someone deliberately marking config as protected (variant create sets them
	// with --unit-delete-gate). Refusing is the correct response; overriding
	// should be a decision a human makes, not a flag this command hard-codes.
	for _, space := range spaces {
		has, err := r.spaceExists(space)
		if err != nil {
			return fmt.Errorf("checking %s: %w", space, err)
		}
		if !has {
			continue
		}
		if _, err := r.cub("space", "delete", space, "--recursive"); err != nil {
			fmt.Fprintf(w, "  could not delete %s: %v\n", space, err)
			fmt.Fprintln(w, "     (a delete gate will block this; clear it or remove the Space by hand)")
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

// verifyNothingLeft checks AWS directly. The Kubernetes view is not evidence: an
// object can outlive its resource, and an unreachable cluster reports zero.
func (r *runner) verifyNothingLeft(region string, w io.Writer) error {
	checks := []struct {
		label string
		args  []string
	}{
		{"EKS clusters", []string{"eks", "list-clusters", "--query", "clusters", "--output", "text"}},
		{"stack VPCs", []string{"ec2", "describe-vpcs",
			"--filters", stackTagFilter,
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

// requireControllerCredentials verifies the ACK controllers can actually reach
// AWS, using the Secret mounted into them rather than the caller's own session.
//
// Deleting an ACK object while its controller cannot authenticate destroys
// nothing: the object goes away, the AWS resource stays, and with the object
// gone ACK can never clean it up. That is unrecoverable through config — the
// leftovers have to be found and deleted in the console.
func (r *runner) requireControllerCredentials(w io.Writer) error {
	mgmt, _ := r.discoverClusters()
	if mgmt == "" {
		return nil // teardownMgmt reports this properly; nothing to check yet
	}
	kc := cubClusterKubeconfig(mgmt)

	if _, err := r.kubectl(kc, "get", "secret", credsSecret, "-n", ackNamespace); err != nil {
		return fmt.Errorf(
			"no %s/%s Secret — the ACK controllers have no AWS credentials.\n"+
				"Deleting now would remove the Kubernetes objects and strand every AWS\n"+
				"resource. Run: cub eksinf creds use-existing", ackNamespace, credsSecret)
	}

	// The controllers log an ExpiredTokenException rather than reporting
	// unhealthy, so ask them: a recent auth failure in the logs means the mounted
	// credentials are stale even though every pod looks Running.
	logs, err := r.kubectl(kc, "logs", "-n", ackNamespace,
		"-l", "app.kubernetes.io/name=ec2-chart", "--tail=40")
	if err == nil {
		for _, marker := range []string{"ExpiredToken", "InvalidClientTokenId", "security token included in the request is expired"} {
			if strings.Contains(logs, marker) {
				return fmt.Errorf(
					"the ACK controllers' AWS credentials look expired (%s in recent logs).\n"+
						"Refresh before tearing down, or deletions will strand AWS resources:\n"+
						"  aws sso login && cub eksinf creds refresh", marker)
			}
		}
	}
	fmt.Fprintln(w, "  ACK controller credentials: present, no recent auth errors")
	return nil
}

// waitForArgoStopped blocks until no application-controller pod is running.
// Proceeding while one is still alive means racing the reconciler.
func (r *runner) waitForArgoStopped(kc string, wait time.Duration, w io.Writer) error {
	deadline := time.Now().Add(wait)
	for {
		out, err := r.kubectl(kc, "get", "pods", "-n", "argocd",
			"-l", "app.kubernetes.io/name=argocd-application-controller", "-o", "name")
		if err == nil && strings.TrimSpace(out) == "" {
			fmt.Fprintln(w, "    Argo stopped")
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the Argo application controller is still running after %s; "+
				"deleting now would race it", wait)
		}
		time.Sleep(10 * time.Second)
	}
}

// stackVPC returns this stack's VPC id, or "" if it is gone or unreadable.
// Every orphan sweep scopes itself to this: nothing outside the VPC aws-network
// created is ours to delete.
func (r *runner) stackVPC(region string) string {
	out, err := r.aws("ec2", "describe-vpcs", "--region", region,
		"--filters", stackTagFilter,
		"--query", "Vpcs[].VpcId", "--output", "text")
	if err != nil {
		return ""
	}
	if ids := strings.Fields(strings.TrimSpace(out)); len(ids) > 0 {
		return ids[0]
	}
	return ""
}

// releaseOrphanedENIs deletes detached network interfaces left in the stack VPC.
//
// The VPC CNI gives each node secondary ENIs for pod IPs. When a node goes away
// abruptly — which is how Karpenter reclaims a consolidated node — one can be
// left behind in "available": attached to nothing, running nothing, billing
// nothing, and therefore invisible to every check this command makes. It still
// holds its subnet and its security group, which hold the route table, which
// holds the VPC. One leaked ENI stalls the entire delete on DependencyViolation.
//
// Measured: a single ENI from a released g6.xlarge blocked four resources for the
// full 35-minute deadline, and teardown then failed with all four "still
// present" and no indication of why.
//
// Only "available" interfaces are touched. An in-use one belongs to something
// still running and is not ours to delete.
//
// This deliberately does NOT run at drain time, where it would be the obvious
// place. Two reasons: the ENI is often still detaching when the instance
// disappears, so a sweep there misses it; and the system nodegroup is still live
// then, so deleting interfaces out from under a running CNI risks breaking a
// cluster that has not been destroyed yet. Stalled convergence is the honest
// signal — by then the control plane is going or gone and an available ENI in
// this VPC is unambiguously garbage.
func (r *runner) releaseOrphanedENIs(region string, w io.Writer) {
	vpc := r.stackVPC(region)
	if vpc == "" {
		return
	}
	out, err := r.aws("ec2", "describe-network-interfaces", "--region", region,
		"--filters", "Name=vpc-id,Values="+vpc, "Name=status,Values=available",
		"--query", "NetworkInterfaces[].NetworkInterfaceId", "--output", "text")
	if err != nil {
		fmt.Fprintf(w, "      could not list network interfaces (%v); the VPC may hang on DependencyViolation\n", err)
		return
	}
	for _, eni := range strings.Fields(out) {
		if _, err := r.aws("ec2", "delete-network-interface", "--region", region,
			"--network-interface-id", eni); err != nil {
			fmt.Fprintf(w, "      could not delete orphaned ENI %s: %v\n", eni, err)
			continue
		}
		fmt.Fprintf(w, "      released orphaned ENI %s\n", eni)
	}
}

// releaseClusterSecurityGroups deletes the security group EKS creates for the
// cluster.
//
// EKS creates this one, not ACK, so no Unit describes it and nothing in the
// config owns it. EKS deletes it along with the cluster — unless something still
// references it, which a leaked ENI does. Once the control plane is gone nothing
// will ever retry, and a VPC cannot be deleted while a non-default security group
// remains inside it. The result is a VPC that is permanently undeletable by any
// path this command has.
//
// Matched by EKS's own naming (eks-cluster-sg-<cluster>-<id>) rather than by
// "everything that is not default", so a security group someone added by hand is
// left alone. The default group is never touched: AWS deletes it with the VPC and
// refuses to delete it on its own.
func (r *runner) releaseClusterSecurityGroups(region string, w io.Writer) {
	vpc := r.stackVPC(region)
	if vpc == "" {
		return
	}
	out, err := r.aws("ec2", "describe-security-groups", "--region", region,
		"--filters", "Name=vpc-id,Values="+vpc,
		"--query", "SecurityGroups[].[GroupId,GroupName]", "--output", "text")
	if err != nil {
		fmt.Fprintf(w, "      could not list security groups (%v); the VPC may hang on DependencyViolation\n", err)
		return
	}
	for _, sg := range eksClusterSGs(out) {
		if _, err := r.aws("ec2", "delete-security-group", "--region", region, "--group-id", sg.id); err != nil {
			// Expected while an ENI still references it; the next sweep retries
			// after releaseOrphanedENIs has had a turn.
			fmt.Fprintf(w, "      could not delete %s (%s) yet: %v\n", sg.name, sg.id, err)
			continue
		}
		fmt.Fprintf(w, "      released EKS cluster security group %s\n", sg.name)
	}
}

type securityGroup struct{ id, name string }

// eksClusterSGs picks the EKS-created cluster security groups out of the text
// output of `describe-security-groups`, one "<GroupId>\t<GroupName>" per line.
//
// Split out to be testable: this is text-shaped AWS output being matched by
// prefix, which is exactly the kind of parsing that fails silently and reports
// "nothing to do" for a group that is sitting right there — the same trap
// extractJSONString fell into.
func eksClusterSGs(awsOutput string) []securityGroup {
	var found []securityGroup
	for _, line := range strings.Split(strings.TrimSpace(awsOutput), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 || !strings.HasPrefix(f[1], eksClusterSGPrefix) {
			continue
		}
		found = append(found, securityGroup{id: f[0], name: f[1]})
	}
	return found
}

// releaseKarpenterInstanceProfiles detaches and deletes the instance profiles
// Karpenter created.
//
// When an EC2NodeClass sets spec.role, Karpenter creates an instance profile to
// hold it. Nothing in this repo's config declares that profile, so nothing
// deletes it — and while it exists, deleting the node role fails with
// DeleteConflict: "must remove roles from instance profile first". The IAM role
// then hangs as ACK.Recoverable indefinitely.
func (r *runner) releaseKarpenterInstanceProfiles(w io.Writer) error {
	out, err := r.aws("iam", "list-instance-profiles",
		"--query", "InstanceProfiles[].InstanceProfileName", "--output", "text")
	if err != nil {
		fmt.Fprintf(w, "    could not list instance profiles (%v); the node role may hang on DeleteConflict\n", err)
		return nil
	}
	for _, profile := range strings.Fields(out) {
		// Karpenter names them <clusterName>_<hash>.
		if !strings.HasPrefix(profile, "inference-demo_") {
			continue
		}
		roles, err := r.aws("iam", "get-instance-profile", "--instance-profile-name", profile,
			"--query", "InstanceProfile.Roles[].RoleName", "--output", "text")
		if err == nil {
			for _, role := range strings.Fields(roles) {
				_, _ = r.aws("iam", "remove-role-from-instance-profile",
					"--instance-profile-name", profile, "--role-name", role)
			}
		}
		if _, err := r.aws("iam", "delete-instance-profile", "--instance-profile-name", profile); err != nil {
			fmt.Fprintf(w, "    could not delete instance profile %s: %v\n", profile, err)
			continue
		}
		fmt.Fprintf(w, "    released Karpenter instance profile %s\n", profile)
	}
	return nil
}

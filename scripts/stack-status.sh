#!/usr/bin/env bash
# What is actually running across both planes — and what it costs.
#
#   scripts/stack-status.sh [--profile P] [--mgmt-cluster N] [--workload-cluster N]
#
# TWO RULES THIS SCRIPT FOLLOWS, BOTH LEARNED THE HARD WAY:
#
# 1. COST COMES FROM EC2, NOT FROM KUBERNETES. A Node or NodeClaim object can
#    outlive its instance by minutes, and an unreachable cluster reports zero
#    nodes — so `kubectl get nodes` is wrong in both directions. Only
#    describe-instances answers "am I being billed".
#
# 2. UNREACHABLE IS NOT EMPTY. Every query here runs behind an explicit
#    connectivity check that fails loudly. Piping kubectl through `2>/dev/null`
#    renders an expired token identically to a healthy zero, which is how a lost
#    session looked like a quiet, successful "nothing running" for 13 minutes.

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

MGMT_CLUSTER=""
WORKLOAD_CLUSTER=""
PROFILE=""
STACK_TAG="eks-inference.confighub.com/stack"
STACK_VALUE="inference-demo"

note() { echo "  $*"; }
step() { echo "==> $*"; }
head2() {
	echo
	echo "── $*"
}
warn() { echo "  !! $*"; }

while [[ $# -gt 0 ]]; do
	case "$1" in
	--profile) PROFILE="${2:?}"; shift 2 ;;
	--mgmt-cluster) MGMT_CLUSTER="${2:?}"; shift 2 ;;
	--workload-cluster) WORKLOAD_CLUSTER="${2:?}"; shift 2 ;;
	-h | --help)
		sed -n '2,4p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
		exit 0
		;;
	*)
		echo "unknown option: $1" >&2
		exit 1
		;;
	esac
done

aws_cli() {
	if [[ -n "$PROFILE" ]]; then aws --profile "$PROFILE" "$@"; else aws "$@"; fi
}
REGION="$(yq -r '.aws.region' src/ack-controllers/values/ec2.yaml)"

kubeconfig_for() { echo "${HOME}/.confighub/clusters/${1}.kubeconfig"; }

# Returns 0 only if the cluster genuinely answers. Callers must gate on this
# rather than letting individual queries degrade to empty output.
reachable() {
	KUBECONFIG="$1" kubectl version -o json >/dev/null 2>&1
}

discover_clusters() {
	local kc f
	for f in "${HOME}"/.confighub/clusters/*.kubeconfig; do
		[[ -f "$f" ]] || continue
		reachable "$f" || continue
		kc="$(basename "$f" .kubeconfig)"
		[[ -z "$MGMT_CLUSTER" ]] &&
			KUBECONFIG="$f" kubectl get ns ack-system >/dev/null 2>&1 && MGMT_CLUSTER="$kc"
		[[ -z "$WORKLOAD_CLUSTER" ]] &&
			KUBECONFIG="$f" kubectl get crd nodepools.karpenter.sh >/dev/null 2>&1 && WORKLOAD_CLUSTER="$kc"
	done
}

discover_clusters

# ------------------------------------------------------------------ billing

# Approximate on-demand hourly rates for the instance types this stack actually
# provisions. Deliberately small and clearly labelled rather than a full pricing
# integration — the purpose is "roughly how fast is this burning", not an invoice.
rate_for() {
	case "$1" in
	t4g.medium) echo "0.034" ;;
	c6g.large | c7g.large) echo "0.072" ;;
	c6g.xlarge | c7g.xlarge) echo "0.145" ;;
	c6g.2xlarge | c7g.2xlarge) echo "0.290" ;;
	m6g.large | m7g.large) echo "0.081" ;;
	g5.xlarge) echo "1.006" ;;
	g5.2xlarge) echo "1.212" ;;
	g6.xlarge) echo "0.805" ;;
	g6.2xlarge) echo "0.978" ;;
	g6e.xlarge) echo "1.861" ;;
	p5e.48xlarge | p5en.48xlarge) echo "35.000" ;;
	*) echo "" ;;
	esac
}

head2 "billing (from EC2, the only reliable source)"
if ! instances="$(aws_cli ec2 describe-instances --region "$REGION" \
	--filters "Name=instance-state-name,Values=running,pending" \
	--query 'Reservations[].Instances[].[InstanceId,InstanceType,LaunchTime,Tags[?Key==`karpenter.sh/nodepool`].Value|[0]]' \
	--output text 2>&1)"; then
	warn "cannot query EC2 — cost is UNKNOWN, not zero:"
	echo "$instances" | head -2 | sed 's/^/     /'
	warn "if your SSO session expired, run: aws sso login${PROFILE:+ --profile ${PROFILE}}"
else
	if [[ -z "$instances" ]]; then
		note "no running instances in ${REGION}"
	else
		total=0
		gpu=0
		while IFS=$'\t' read -r id itype launched pool; do
			[[ -n "$id" ]] || continue
			r="$(rate_for "$itype")"
			[[ "$itype" == g* || "$itype" == p* ]] && gpu=$((gpu + 1))
			printf "  %-20s %-16s %-10s %s\n" "$id" "$itype" "${pool:-system}" "${r:+\$${r}/hr}${r:+ }${launched}"
			[[ -n "$r" ]] && total="$(echo "$total + $r" | bc -l)"
		done <<<"$instances"
		echo
		printf "  approx total: \$%.2f/hr  (~\$%.0f/month if left running)\n" "$total" "$(echo "$total * 730" | bc -l)"
		[[ "$gpu" -gt 0 ]] && warn "${gpu} GPU instance(s) running — scale workloads to 0 to release them"
	fi
fi

# ---------------------------------------------------------------- mgmt plane

head2 "mgmt plane (creates AWS infrastructure)"
if [[ -z "$MGMT_CLUSTER" ]]; then
	warn "no reachable cluster with an ack-system namespace"
	warn "either it is not up, or its credentials expired — this is NOT the same as 'nothing deployed'"
else
	export KUBECONFIG="$(kubeconfig_for "$MGMT_CLUSTER")"
	note "cluster: ${MGMT_CLUSTER}"

	step "ACK resources"
	kubectl get vpc,subnet,natgateway,securitygroup,roles.iam.services.k8s.aws,cluster,nodegroup,addon,podidentityassociations.eks.services.k8s.aws \
		-n aws-inference -o json |
		jq -r '.items[] | "  \(.kind)/\(.metadata.name): \(.status.status // .status.state // (([(.status.conditions//[])[]|select(.type=="ACK.ResourceSynced")|.status]|first) // "?"))"'

	step "problems"
	kubectl get vpc,subnet,natgateway,securitygroup,roles.iam.services.k8s.aws,cluster,nodegroup,addon,podidentityassociations.eks.services.k8s.aws \
		-n aws-inference -o json |
		jq -r '[.items[] | . as $r | ($r.status.conditions//[])[]
            | select((.type=="ACK.Terminal" and .status=="True") or (.type=="ACK.Recoverable" and .status=="True"))
            | "  \($r.kind)/\($r.metadata.name) [\(.type)]: \(.message)"]
           | if length==0 then "  none" else .[] end'
fi

# ------------------------------------------------------------ workload plane

head2 "workload plane (runs the inference stack)"
if [[ -z "$WORKLOAD_CLUSTER" ]]; then
	warn "no reachable cluster with Karpenter CRDs"
	warn "not deployed, or unreachable — check before concluding anything"
	exit 0
fi

export KUBECONFIG="$(kubeconfig_for "$WORKLOAD_CLUSTER")"
note "cluster: ${WORKLOAD_CLUSTER}"

step "Argo applications"
kubectl get applications -n argocd --no-headers | awk '{printf "  %-30s %-12s %s\n", $1, $2, $3}'

step "EC2NodeClass resolution (validates the cross-plane contract against AWS)"
kubectl get ec2nodeclasses.karpenter.k8s.aws -o json | jq -r '
  .items[] |
  "  \(.metadata.name): ready=\(([(.status.conditions//[])[]|select(.type=="Ready")|.status]|first) // "?")  subnets=\((.status.subnets//[])|length)  sgs=\((.status.securityGroups//[])|length)  amis=\((.status.amis//[])|length)  reservations=\((.status.capacityReservations//[])|length)"'
kubectl get ec2nodeclasses.karpenter.k8s.aws -o json | jq -r '
  [.items[] | select(((.status.subnets//[])|length)==0 or ((.status.securityGroups//[])|length)==0 or ((.status.amis//[])|length)==0)
   | "  !! \(.metadata.name) resolved 0 subnets/SGs/AMIs — the karpenter.sh/discovery tag no longer agrees with aws-network"]
  | .[]'

step "NodePools"
kubectl get nodepools.karpenter.sh -o json | jq -r '
  .items[] | "  \(.metadata.name): ready=\(([(.status.conditions//[])[]|select(.type=="Ready")|.status]|first) // "?")  nodes=\(.status.resources.nodes // "0")  gpu=\(.status.resources["nvidia.com/gpu"] // "0")"'

step "GPU device plugin"
if kubectl get ns gpu-operator >/dev/null 2>&1; then
	kubectl get ds -n gpu-operator -o json |
		jq -r '.items[] | "  \(.metadata.name): \(.status.numberReady)/\(.status.desiredNumberScheduled) ready"'
	note "(0/0 is correct when no GPU node exists — the DaemonSet requires the accelerator label)"
else
	note "gpu-operator namespace absent — gpu-runtime not deployed"
fi

step "workload replicas"
if kubectl get ns inference >/dev/null 2>&1; then
	kubectl get deploy -n inference -o json |
		jq -r '.items[] | "  \(.metadata.name): \(.spec.replicas) desired, \(.status.readyReplicas // 0) ready"'
	note "change these through config, not kubectl — Argo selfHeal reverts a manual scale:"
	note "  cub function do --space inference-workloads-<variant> --where \"Slug = '<unit>'\" set-replicas N"
	note "  cub release publish inference-workloads-<variant>"
else
	note "inference namespace absent — inference-workloads not deployed"
fi

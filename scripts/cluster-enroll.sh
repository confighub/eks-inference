#!/usr/bin/env bash
# Enroll an EXISTING cluster into ConfigHub — everything `cub cluster up` does
# except creating the cluster.
#
#   scripts/cluster-enroll.sh enroll   --name N --eks-cluster C --region R
#   scripts/cluster-enroll.sh status   --name N
#   scripts/cluster-enroll.sh unenroll --name N
#
# This is a prototype for a `cub cluster enroll` CLI command. It is written
# against the public cub CLI only — no server-side support is needed — so that
# what it does is legible and can be lifted into the CLI directly.
#
# The shape it produces is copied from a real `cub cluster up` cluster:
#
#   Space <name>                worker (server-hosted) + OCI target owned by it.
#                               A pure namespace; holds no config bundle.
#   Space <name>-argo-apps      root "app of apps" Application Unit and every
#                               child Application Unit. Its release target is the
#                               OCI target in <name> (a cross-Space reference),
#                               so this Space IS the bundle Argo pulls.
#
# Argo pulls from ConfigHub's OCI registry, so no worker pod runs in the cluster
# and nothing needs inbound access to it. Cluster credentials are used exactly
# once, to install Argo CD and apply the root Application.
#
# ENROLL NEVER CREATES OR DESTROYS A CLUSTER. `unenroll` removes ConfigHub
# wiring and leaves the cluster running — that asymmetry with `cub cluster down`
# is deliberate and load-bearing.

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

NAME=""
EKS_CLUSTER=""
REGION=""
KUBECONFIG_PATH=""
KUBE_CONTEXT=""
PROFILE=""
# Argo CD v3.1 is a HARD FLOOR: earlier versions cannot resolve an oci:// source
# for plain manifests and fail with `unsupported scheme "oci"` — the Application
# reports Healthy while syncing nothing, so the failure is quiet. v3.4.5 is what
# `cub cluster up` installs; keeping them the same avoids surprises between the
# management cluster and enrolled ones.
ARGOCD_VERSION="v3.4.5"
ARGOCD_NAMESPACE="argocd"
# The OCI registry Argo pulls bundles from. Belongs to the ConfigHub instance, so
# it differs for staging or a local server. The real CLI should resolve this from
# the active context rather than take it as a flag.
OCI_REGISTRY="oci.hub.confighub.com:443"
INSTALL_ARGOCD=1
KEEP_ARGOCD=1
DELETE_SPACES=0
ASSUME_YES=0

die() {
	echo "ERROR: $*" >&2
	exit 1
}
note() { echo "  $*"; }
step() { echo "==> $*"; }

usage() {
	sed -n '2,8p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
	cat <<'EOF'

Cluster selection (one of):
  --eks-cluster NAME --region R   resolve an EKS cluster via `aws eks update-kubeconfig`
  --kubeconfig PATH               use an existing kubeconfig
  --context NAME                  use a context in the default kubeconfig

Options:
  --name NAME          ConfigHub space prefix (required). Apps space is NAME-argo-apps.
  --profile NAME       AWS profile, for --eks-cluster
  --argocd-version V   Argo CD version to install (default v3.4.5; v3.1+ required for oci://)
  --oci-registry H     ConfigHub OCI registry (default oci.hub.confighub.com:443)
  --no-argocd          do not install Argo CD (it is already there)
  --delete-spaces      unenroll: also delete the ConfigHub spaces
  --yes                do not prompt
EOF
}

cmd="${1:-}"
[[ -n "$cmd" ]] || {
	usage
	exit 1
}
shift || true

while [[ $# -gt 0 ]]; do
	case "$1" in
	--name) NAME="${2:?}"; shift 2 ;;
	--eks-cluster) EKS_CLUSTER="${2:?}"; shift 2 ;;
	--region) REGION="${2:?}"; shift 2 ;;
	--kubeconfig) KUBECONFIG_PATH="${2:?}"; shift 2 ;;
	--context) KUBE_CONTEXT="${2:?}"; shift 2 ;;
	--profile) PROFILE="${2:?}"; shift 2 ;;
	--argocd-version) ARGOCD_VERSION="${2:?}"; shift 2 ;;
	--argocd-namespace) ARGOCD_NAMESPACE="${2:?}"; shift 2 ;;
	--oci-registry) OCI_REGISTRY="${2:?}"; shift 2 ;;
	--no-argocd) INSTALL_ARGOCD=0; shift ;;
	--delete-spaces) DELETE_SPACES=1; shift ;;
	--yes | -y) ASSUME_YES=1; shift ;;
	-h | --help) usage; exit 0 ;;
	*) die "unknown option: $1" ;;
	esac
done

require_name() {
	[[ -n "$NAME" ]] || die "--name is required"
	APPS_SPACE="${NAME}-argo-apps"
	CLUSTER_KUBECONFIG="${HOME}/.confighub/clusters/${NAME}.kubeconfig"
}
APPS_SPACE="${NAME}-argo-apps"
# Same convention as `cub cluster up`, so scripts/aws-creds.sh and anything else
# that looks for a cub cluster kubeconfig finds this one too.
CLUSTER_KUBECONFIG="${HOME}/.confighub/clusters/${NAME}.kubeconfig"

aws_cli() {
	if [[ -n "$PROFILE" ]]; then aws --profile "$PROFILE" "$@"; else aws "$@"; fi
}
cubx() { CONFIGHUB_AGENT=1 cub "$@"; }

confirm() {
	[[ "$ASSUME_YES" == "1" ]] && return 0
	local reply
	read -r -p "$1 [y/N] " reply
	[[ "$reply" == "y" || "$reply" == "Y" ]]
}

# ---------------------------------------------------------------- kube access

resolve_kube() {
	if [[ -n "$EKS_CLUSTER" ]]; then
		[[ -n "$REGION" ]] || die "--region is required with --eks-cluster"
		mkdir -p "$(dirname "$CLUSTER_KUBECONFIG")"
		aws_cli eks update-kubeconfig \
			--name "$EKS_CLUSTER" --region "$REGION" \
			--kubeconfig "$CLUSTER_KUBECONFIG" >/dev/null ||
			die "could not resolve EKS cluster '${EKS_CLUSTER}' in ${REGION}"
		note "kubeconfig: ${CLUSTER_KUBECONFIG}"
	elif [[ -n "$KUBECONFIG_PATH" ]]; then
		[[ -f "$KUBECONFIG_PATH" ]] || die "no kubeconfig at ${KUBECONFIG_PATH}"
		CLUSTER_KUBECONFIG="$KUBECONFIG_PATH"
		note "kubeconfig: ${CLUSTER_KUBECONFIG}"
	elif [[ -n "$KUBE_CONTEXT" ]]; then
		CLUSTER_KUBECONFIG="${KUBECONFIG:-${HOME}/.kube/config}"
		note "kubeconfig: ${CLUSTER_KUBECONFIG} (context ${KUBE_CONTEXT})"
	else
		die "select a cluster with --eks-cluster/--region, --kubeconfig, or --context"
	fi
}

kc() {
	if [[ -n "$KUBE_CONTEXT" ]]; then
		KUBECONFIG="$CLUSTER_KUBECONFIG" kubectl --context "$KUBE_CONTEXT" "$@"
	else
		KUBECONFIG="$CLUSTER_KUBECONFIG" kubectl "$@"
	fi
}

preflight() {
	step "checking cluster access"
	local ver
	ver="$(kc version -o json 2>/dev/null | jq -r '.serverVersion.gitVersion // empty')" ||
		die "cannot reach the cluster"
	[[ -n "$ver" ]] || die "cannot reach the cluster"
	note "server: ${ver}"

	# Installing Argo CD needs cluster-admin. Fail here with a clear message
	# rather than halfway through applying a 20k-line manifest.
	local can
	can="$(kc auth can-i create clusterrole --all-namespaces 2>/dev/null || echo no)"
	[[ "$can" == "yes" ]] || die "need cluster-admin on this cluster (cannot create ClusterRoles)"
	note "cluster-admin: yes"
}

# ---------------------------------------------------------------- confighub

ensure_confighub_side() {
	step "ConfigHub: cluster space '${NAME}'"
	cubx space create "$NAME" --allow-exists >/dev/null || die "could not create space ${NAME}"
	note "space ${NAME}"

	# A server-hosted worker: ConfigHub runs it, nothing is deployed to the
	# cluster for it. This is what lets Argo pull without a worker pod.
	cubx worker create worker --space "$NAME" --is-server-worker --allow-exists >/dev/null ||
		die "could not create worker in ${NAME}"
	note "worker ${NAME}/worker (server-hosted)"

	# The OCI target the apps space releases to. The argo-apps-space annotation
	# is how an API client resolves the Application-holding Space from a target.
	#
	# -p/-t are REQUIRED: target create defaults to Kubernetes/Kubernetes-YAML,
	# which a server-hosted worker does not support, and the resulting error
	# ("BridgeWorker does not support ConfigType...") names the provider type
	# rather than the flag you forgot. The worker advertises OCI/Any.
	cubx target create target '{}' worker \
		--space "$NAME" \
		-p OCI -t Any \
		--annotation "confighub.com/argo-apps-space=${APPS_SPACE}" \
		--allow-exists >/dev/null ||
		die "could not create OCI target in ${NAME}"
	note "target ${NAME}/target (OCI/Any)"

	step "ConfigHub: apps space '${APPS_SPACE}'"
	cubx space create "$APPS_SPACE" --allow-exists >/dev/null || die "could not create space ${APPS_SPACE}"
	# Cross-space release target: the apps space's bundle is served by the OCI
	# target living in the cluster space, so neither space releases into itself.
	cubx space update "$APPS_SPACE" --patch --release-target "${NAME}/target" >/dev/null ||
		die "could not set release target on ${APPS_SPACE}"
	note "space ${APPS_SPACE} (release target ${NAME}/target)"
}

root_application_yaml() {
	cat <<EOF
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: ${APPS_SPACE}
  namespace: ${ARGOCD_NAMESPACE}
  finalizers:
  - resources-finalizer.argocd.argoproj.io
spec:
  project: default
  source:
    repoURL: oci://${OCI_REGISTRY}/space/${APPS_SPACE}
    targetRevision: latest
    path: .
  destination:
    server: https://kubernetes.default.svc
    namespace: ${ARGOCD_NAMESPACE}
  syncPolicy:
    automated:
      selfHeal: true
    syncOptions:
    - ServerSideApply=true
    - ServerSideApply.ForceConflicts=true
    - RespectIgnoreDifferences=true
    - CreateNamespace=false
EOF
}

ensure_root_unit() {
	step "ConfigHub: root app-of-apps unit"
	if cubx unit get --space "$APPS_SPACE" root >/dev/null 2>&1; then
		root_application_yaml | cubx unit update --space "$APPS_SPACE" root - >/dev/null
		note "updated unit ${APPS_SPACE}/root"
	else
		root_application_yaml | cubx unit create --space "$APPS_SPACE" root - >/dev/null
		note "created unit ${APPS_SPACE}/root"
	fi

	# Publishing an unchanged space is an error ("no changes were made since
	# :latest bundle"), not a no-op — so a re-run of an already-enrolled cluster
	# would fail here. Treat that specific case as success; enroll is meant to be
	# safe to repeat.
	local out
	if out="$(cubx release publish "$APPS_SPACE" 2>&1)"; then
		note "published release for ${APPS_SPACE}"
	elif grep -q "no changes were made" <<<"$out"; then
		note "release for ${APPS_SPACE} already current"
	else
		echo "$out" >&2
		die "could not publish ${APPS_SPACE} release"
	fi
}

# ---------------------------------------------------------------- argo cd

install_argocd() {
	if kc get deployment argocd-server -n "$ARGOCD_NAMESPACE" >/dev/null 2>&1; then
		note "Argo CD already present in ${ARGOCD_NAMESPACE}; leaving it alone"
		return 0
	fi
	if [[ "$INSTALL_ARGOCD" != "1" ]]; then
		die "Argo CD not found in ${ARGOCD_NAMESPACE} and --no-argocd was given"
	fi

	step "installing Argo CD ${ARGOCD_VERSION} into ${ARGOCD_NAMESPACE}"
	kc create namespace "$ARGOCD_NAMESPACE" --dry-run=client -o yaml | kc apply -f - >/dev/null
	kc apply -n "$ARGOCD_NAMESPACE" --server-side --force-conflicts \
		-f "https://raw.githubusercontent.com/argoproj/argo-cd/${ARGOCD_VERSION}/manifests/install.yaml" >/dev/null ||
		die "Argo CD install failed"
	note "waiting for argocd-server"
	kc rollout status deployment/argocd-server -n "$ARGOCD_NAMESPACE" --timeout=300s >/dev/null ||
		die "argocd-server did not become ready"
	note "Argo CD ready"
}

# Argo authenticates to ConfigHub's OCI registry as the cluster's worker: the
# worker ID is the username and the worker secret is the password, in an Argo
# `repo-creds` Secret.
#
# Without this the Application resolves the oci:// scheme and then fails with a
# bare 401 from the registry — and because Argo reports an Application whose
# manifests it cannot load as Healthy, the failure is quiet.
ensure_oci_credentials() {
	step "Argo CD: ConfigHub OCI registry credentials"

	local worker_id worker_secret
	worker_id="$(cubx worker get worker --space "$NAME" -o json 2>/dev/null |
		jq -r '.BridgeWorker.BridgeWorkerID // empty')"
	[[ -n "$worker_id" ]] || die "could not read worker ID for ${NAME}/worker"

	worker_secret="$(cubx worker get-secret worker --space "$NAME" 2>/dev/null | tr -d '[:space:]')"
	[[ -n "$worker_secret" ]] || die "could not read worker secret for ${NAME}/worker"

	kc create secret generic confighub-oci-creds \
		--namespace "$ARGOCD_NAMESPACE" \
		--from-literal=type=oci \
		--from-literal=enableOCI=true \
		--from-literal=url="oci://${OCI_REGISTRY}" \
		--from-literal=username="$worker_id" \
		--from-literal=password="$worker_secret" \
		--dry-run=client -o yaml |
		kc apply -f - >/dev/null
	kc label secret confighub-oci-creds -n "$ARGOCD_NAMESPACE" \
		argocd.argoproj.io/secret-type=repo-creds --overwrite >/dev/null

	note "secret ${ARGOCD_NAMESPACE}/confighub-oci-creds (repo-creds, worker ${worker_id})"
}

bootstrap_root_app() {
	step "bootstrapping the root Application"
	# The one and only time cluster credentials are needed for config. From here
	# Argo pulls the apps-space bundle from ConfigHub's OCI registry itself.
	root_application_yaml | kc apply -f - >/dev/null || die "could not apply the root Application"
	note "applied Application/${APPS_SPACE} in ${ARGOCD_NAMESPACE}"
}

# ---------------------------------------------------------------- commands

cmd_enroll() {
	require_name
	for t in kubectl jq cub aws; do command -v "$t" >/dev/null || die "$t not found"; done
	cubx auth status >/dev/null 2>&1 || die "not authenticated to ConfigHub — run 'cub auth login'"

	resolve_kube
	preflight
	ensure_confighub_side
	install_argocd
	ensure_oci_credentials
	ensure_root_unit
	bootstrap_root_app

	echo
	echo "Enrolled '${NAME}'."
	echo
	echo "  cluster space : ${NAME}          (worker + OCI target)"
	echo "  apps space    : ${APPS_SPACE}    (root + child Applications)"
	echo "  kubeconfig    : ${CLUSTER_KUBECONFIG}"
	echo
	echo "Add a component with:"
	echo "  cub variant create <variant> <component>-base --target ${NAME}/target"
	echo "  cub release publish <component>-<variant>"
	echo
	echo "Argo CD UI (no ingress is created for you):"
	echo "  KUBECONFIG=${CLUSTER_KUBECONFIG} kubectl port-forward -n ${ARGOCD_NAMESPACE} svc/argocd-server 8080:443"
}

cmd_status() {
	require_name
	resolve_kube 2>/dev/null || true

	step "ConfigHub"
	for s in "$NAME" "$APPS_SPACE"; do
		if cubx space get "$s" >/dev/null 2>&1; then
			note "space ${s}: present"
		else
			note "space ${s}: MISSING"
		fi
	done
	cubx target get --space "$NAME" target -o json 2>/dev/null |
		jq -r '.Target | "  target: \(.Slug) (\(.ProviderType))  apps-space=\(.Annotations["confighub.com/argo-apps-space"] // "<unset>")"' ||
		note "target: MISSING"

	echo
	step "apps space units"
	cubx unit list --space "$APPS_SPACE" --no-headers 2>/dev/null | awk '{print "  "$1}' || note "none"

	echo
	step "cluster"
	if kc get ns "$ARGOCD_NAMESPACE" >/dev/null 2>&1; then
		kc get applications -n "$ARGOCD_NAMESPACE" --no-headers 2>/dev/null |
			awk '{printf "  %-28s %-12s %s\n", $1, $2, $3}' || note "no Applications"
	else
		note "no ${ARGOCD_NAMESPACE} namespace — cluster not reachable or Argo CD not installed"
	fi
}

cmd_unenroll() {
	require_name
	resolve_kube

	echo "This will remove ConfigHub wiring for '${NAME}'."
	echo "It will NOT delete the cluster, and NOT delete workloads Argo deployed."
	[[ "$DELETE_SPACES" == "1" ]] && echo "It WILL delete spaces ${NAME} and ${APPS_SPACE}."
	echo
	confirm "Proceed?" || exit 1

	step "detaching the root Application"
	if kc get application "$APPS_SPACE" -n "$ARGOCD_NAMESPACE" >/dev/null 2>&1; then
		# The root Application carries resources-finalizer, so a plain delete
		# CASCADES: Argo would delete every child Application and everything they
		# deployed. Strip the finalizer first so the delete orphans instead.
		# This is the single most dangerous step in the whole script.
		kc patch application "$APPS_SPACE" -n "$ARGOCD_NAMESPACE" \
			--type=merge -p '{"metadata":{"finalizers":[]}}' >/dev/null
		note "removed resources-finalizer (children will be orphaned, not deleted)"
		kc delete application "$APPS_SPACE" -n "$ARGOCD_NAMESPACE" --cascade=orphan >/dev/null
		note "deleted Application/${APPS_SPACE}"
	else
		note "no root Application found"
	fi

	note "Argo CD left installed (it may predate enrollment)"

	if [[ "$DELETE_SPACES" == "1" ]]; then
		step "deleting ConfigHub spaces"
		for s in "$APPS_SPACE" "$NAME"; do
			for u in $(cubx unit list --space "$s" --no-headers 2>/dev/null | awk '{print $1}'); do
				cubx unit delete --space "$s" "$u" >/dev/null 2>&1 || true
			done
			cubx space delete "$s" >/dev/null 2>&1 && note "deleted space ${s}" || note "could not delete space ${s}"
		done
	else
		note "ConfigHub spaces left in place (pass --delete-spaces to remove)"
	fi

	echo
	echo "Unenrolled '${NAME}'. The cluster is untouched."
}

case "$cmd" in
enroll) cmd_enroll ;;
status) cmd_status ;;
unenroll) cmd_unenroll ;;
-h | --help | help) usage ;;
*) die "unknown command: ${cmd} (try --help)" ;;
esac

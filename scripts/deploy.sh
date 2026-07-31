#!/usr/bin/env bash
# Create downstream variants of a plane's components and publish their releases.
#
#   scripts/deploy.sh --plane mgmt     --target inf/target
#   scripts/deploy.sh --plane workload --target inference-demo/target
#
# Components, their plane, and their order come from components.yaml.
#
# ORDERING BETWEEN PLANES IS YOURS TO ENFORCE. Deploy mgmt, let it converge, then
# deploy workload. Nothing here waits: the karpenter-aws IAM role lives in the
# mgmt plane and the controller that assumes it lives in the workload plane, and
# no Argo sync wave can span two clusters. See docs/dependencies.md.
#
# This does NOT scale any workload up. Everything in inference-workloads ships at
# replicas: 0 precisely so that deploying it costs nothing.

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

PLANE=""
TARGET=""
VARIANT="dev"
DRY_RUN=0

die() {
	echo "ERROR: $*" >&2
	exit 1
}
note() { echo "  $*"; }

usage() {
	cat <<'EOF'
Usage: scripts/deploy.sh --plane <mgmt|workload> --target <space>/<target> [options]

Options:
  --variant NAME   downstream variant name (default: dev)
  --dry-run        print what would be done, change nothing
EOF
}

while [[ $# -gt 0 ]]; do
	case "$1" in
	--plane) PLANE="${2:?}"; shift 2 ;;
	--target) TARGET="${2:?}"; shift 2 ;;
	--variant) VARIANT="${2:?}"; shift 2 ;;
	--dry-run) DRY_RUN=1; shift ;;
	-h | --help) usage; exit 0 ;;
	*) die "unknown option: $1" ;;
	esac
done

[[ -n "$PLANE" ]] || { usage; die "--plane is required"; }
[[ -n "$TARGET" ]] || { usage; die "--target is required"; }
[[ "$PLANE" == "mgmt" || "$PLANE" == "workload" ]] || die "--plane must be mgmt or workload"

cubx() { CONFIGHUB_AGENT=1 cub "$@"; }

cubx auth status >/dev/null 2>&1 || die "not authenticated to ConfigHub — run 'cub auth login'"

# Ordered component list for this plane.
COMPONENTS=()
while IFS= read -r _c; do
	[[ -n "$_c" ]] && COMPONENTS+=("$_c")
done < <(yq -r ".components[] | select(.plane == \"${PLANE}\") | [.order, .name] | @tsv" components.yaml |
	sort -n | cut -f2)

[[ "${#COMPONENTS[@]}" -gt 0 ]] || die "no components with plane '${PLANE}' in components.yaml"

echo "==> plane '${PLANE}' -> target ${TARGET}, variant '${VARIANT}'"
for c in "${COMPONENTS[@]}"; do note "$c"; done
echo

if [[ "$DRY_RUN" == "1" ]]; then
	echo "(dry run; nothing changed)"
	exit 0
fi

for component in "${COMPONENTS[@]}"; do
	base="${component}-base"
	downstream="${component}-${VARIANT}"

	echo "==> ${component}"

	if ! cubx space get "$base" >/dev/null 2>&1; then
		note "SKIP: no base space '${base}' — run 'make install' first"
		continue
	fi

	if cubx space get "$downstream" >/dev/null 2>&1; then
		note "variant ${downstream} already exists"
	else
		# --target also creates the Argo CD Application for a cub-cluster target
		# and republishes the apps space, so there is no separate wiring step.
		cubx variant create "$VARIANT" "$base" --target "$TARGET" >/dev/null ||
			die "could not create variant ${downstream}"
		note "created variant ${downstream}"
	fi

	# Publishing an unchanged space is currently an error rather than a no-op,
	# so a re-run of an already-deployed plane would fail here.
	# See confighubai/confighub#4870.
	pub_out=""
	if pub_out="$(cubx release publish "$downstream" 2>&1)"; then
		note "published release"
	elif grep -q "no changes were made" <<<"$pub_out"; then
		note "release already current"
	else
		echo "$pub_out" >&2
		die "could not publish release for ${downstream}"
	fi
done

echo
echo "Deployed plane '${PLANE}'. Argo pulls on its next reconcile."
echo "Check with: scripts/stack-status.sh"

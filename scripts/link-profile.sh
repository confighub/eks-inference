#!/usr/bin/env bash
# Wire component Units to the platform-profile, so values that must agree across
# components come from one place.
#
#   scripts/link-profile.sh [--variant dev] [--list] [--unlink]
#
# THE PATH ESCAPE, WHICH IS THE WHOLE TRICK:
#
# ConfigHub path expressions use "." as the segment separator, and "~1" escapes a
# literal "." inside a segment. Kubernetes and AWS tag keys are full of dots, so
# nearly every tag binding needs it:
#
#   tags.karpenter~1sh/discovery      ->  tags["karpenter.sh/discovery"]
#
# Getting this wrong is SILENT AND DESTRUCTIVE. An unescaped
# "tags.karpenter.sh/discovery" is parsed as three segments and happily creates
#
#   tags:
#     karpenter.sh/discovery: <the real, now-stale value>
#     karpenter:
#       sh/discovery: <the value you meant to set>
#
# — a successful write to a key nothing reads, leaving the real key untouched.
# Quoting ("...") and backslash escaping are both rejected outright, which is at
# least loud; the unescaped form is the dangerous one.
#
# ALSO: --from-stdin requires JSON. The YAML form shown in the ConfigHub docs is
# accepted without complaint and every field in it is DISCARDED — the Link is
# created with Bindings/UpstreamPaths null and simply never propagates anything.
#
# WHY THESE LINKS AND NOT MORE: only paths with no dotted map keys, or with keys
# that can be escaped as above, are bindable. Values consumed by a Helm chart's
# values.yaml (karpenter's settings.clusterName) cannot be linked at all — those
# are resolved at render time, before ConfigHub sees them.

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

VARIANT="dev"
ACTION="link"
PROFILE_KIND="eks-inference.confighub.com/v1/PlatformProfile"
PROFILE_RES="/inference-demo"

die() {
	echo "ERROR: $*" >&2
	exit 1
}
note() { echo "  $*"; }

while [[ $# -gt 0 ]]; do
	case "$1" in
	--variant) VARIANT="${2:?}"; shift 2 ;;
	--list) ACTION="list"; shift ;;
	--unlink) ACTION="unlink"; shift ;;
	-h | --help)
		sed -n '2,6p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
		exit 0
		;;
	*) die "unknown option: $1" ;;
	esac
done

cubx() { CONFIGHUB_AGENT=1 cub "$@"; }
cubx auth status >/dev/null 2>&1 || die "not authenticated to ConfigHub — run 'cub auth login'"

PROFILE_SPACE="platform-profile-${VARIANT}"
cubx space get "$PROFILE_SPACE" >/dev/null 2>&1 ||
	die "no ${PROFILE_SPACE} — run 'make install' then 'cub variant create ${VARIANT} platform-profile-base'"

# space | unit | profile field | resource type | resource name | downstream path
BINDINGS=(
	"karpenter|nodeclasses|networkName|karpenter.k8s.aws/v1/EC2NodeClass|/general|spec.subnetSelectorTerms.0.tags.karpenter~1sh/discovery"
	"karpenter|nodeclasses|networkName|karpenter.k8s.aws/v1/EC2NodeClass|/general|spec.securityGroupSelectorTerms.0.tags.karpenter~1sh/discovery"
	"karpenter|nodeclasses|networkName|karpenter.k8s.aws/v1/EC2NodeClass|/gpu|spec.subnetSelectorTerms.0.tags.karpenter~1sh/discovery"
	"karpenter|nodeclasses|networkName|karpenter.k8s.aws/v1/EC2NodeClass|/gpu|spec.securityGroupSelectorTerms.0.tags.karpenter~1sh/discovery"
	# Dot-free paths: the node role name and the pinned GPU AMI alias. The alias is
	# the duplication flagged in versions.env — now it has one owner.
	"karpenter|nodeclasses|nodeRoleName|karpenter.k8s.aws/v1/EC2NodeClass|/general|spec.role"
	"karpenter|nodeclasses|nodeRoleName|karpenter.k8s.aws/v1/EC2NodeClass|/gpu|spec.role"
	"karpenter|nodeclasses|gpuAMIAlias|karpenter.k8s.aws/v1/EC2NodeClass|/gpu|spec.amiSelectorTerms.0.alias"
	# Cross-plane: the EKS cluster's own name, and the role names derived from it.
	"eks-cluster|cluster|clusterName|eks.services.k8s.aws/v1alpha1/Cluster|aws-inference/inference-demo|spec.name"
	"eks-cluster|nodegroup|nodeRoleName|eks.services.k8s.aws/v1alpha1/Nodegroup|aws-inference/inference-demo-system|spec.nodeRoleRef.from.name"
)

# ConfigHub permits ONE Link per (from-unit, to-unit) pair — the auto-generated
# slug is derived from both names, so a second attempt collides. That is the right
# model rather than a limitation: a single Link carries many UpstreamPaths and
# DownstreamPaths, so all of a Unit's bindings to the profile belong together.
# Entries are therefore grouped by (space, unit) and emitted as one Link.
link_json() {
	# $1: newline-separated "field|rtype|rname|dpath" entries.
	#
	# Passed as an ARGUMENT, not on stdin: `python3 - <<'PY'` makes the heredoc the
	# program itself, so sys.stdin is already consumed and reading it yields
	# nothing — which produced a Link with empty paths and an opaque 400.
	python3 - "$PROFILE_KIND" "$PROFILE_RES" "$1" <<'PY'
import json, sys
pkind, pres, entries = sys.argv[1:4]
ups, downs = {}, []
for line in entries.splitlines():
    line = line.strip()
    if not line:
        continue
    field, rtype, rname, dpath = line.split("|")
    # Deduplicate upstream reads: several downstream paths often want the same
    # profile field, and it only needs to be read once.
    ups[field] = {"Name": field, "Path": f"spec.{field}",
                  "Resource": {"ResourceType": pkind, "ResourceName": pres}}
    downs.append({"Path": dpath,
                  "Resource": {"ResourceType": rtype, "ResourceName": rname},
                  "Expression": "{{.Params.%s}}" % field,
                  "Evaluator": "template", "Parameters": [field],
                  "DataType": "string"})
print(json.dumps({"UpstreamPaths": list(ups.values()), "DownstreamPaths": downs}))
PY
}

case "$ACTION" in
list)
	for entry in "${BINDINGS[@]}"; do
		IFS='|' read -r comp unit field _ rname dpath <<<"$entry"
		printf "  %-14s %-12s %-14s %-34s %s\n" "${comp}-${VARIANT}" "$unit" "$field" "$rname" "$dpath"
	done
	echo
	echo "Existing links to ${PROFILE_SPACE}:"
	for comp in $(yq -r '.components[] | select(.plane != "hub") | .name' components.yaml); do
		cubx link list --space "${comp}-${VARIANT}" --no-headers 2>/dev/null |
			awk -v c="${comp}-${VARIANT}" '$5 ~ /platform-profile/ {print "  "c"/"$1}'
	done
	exit 0
	;;
unlink)
	for comp in $(yq -r '.components[] | select(.plane != "hub") | .name' components.yaml); do
		space="${comp}-${VARIANT}"
		cubx space get "$space" >/dev/null 2>&1 || continue
		while IFS= read -r l; do
			[[ -n "$l" ]] || continue
			cubx link delete --space "$space" "$l" >/dev/null 2>&1 && note "removed ${space}/${l}"
		done < <(cubx link list --space "$space" --no-headers 2>/dev/null | awk '$5 ~ /platform-profile/ {print $1}')
	done
	exit 0
	;;
esac

created=0
paths=0
# One Link per (space, unit) pair, carrying every path that pair needs.
for group in $(printf '%s\n' "${BINDINGS[@]}" | cut -d'|' -f1,2 | sort -u); do
	comp="${group%%|*}"
	unit="${group##*|}"
	space="${comp}-${VARIANT}"

	if ! cubx space get "$space" >/dev/null 2>&1; then
		note "SKIP ${space} (not deployed)"
		continue
	fi

	entries="$(printf '%s\n' "${BINDINGS[@]}" |
		awk -F'|' -v c="$comp" -v u="$unit" '$1==c && $2==u {print $3"|"$4"|"$5"|"$6}')"
	n="$(echo "$entries" | grep -c . || true)"

	if ! link_json "$entries" |
		cubx link create --space "$space" - "$unit" profile "$PROFILE_SPACE" \
			--update-type TransformPaths --auto-update --from-stdin >/dev/null 2>&1; then
		die "could not create link ${space}/${unit} (${n} paths)"
	fi
	note "${space}/${unit}: ${n} path(s)"
	echo "$entries" | awk -F'|' '{printf "    %-14s -> %s\n", $1, $4}'
	created=$((created + 1))
	paths=$((paths + n))
done

echo
echo "Created ${created} link(s) carrying ${paths} path(s) from ${PROFILE_SPACE}."
echo
echo "Resolve and publish:"
for group in $(printf '%s\n' "${BINDINGS[@]}" | cut -d'|' -f1,2 | sort -u); do
	echo "  cub unit update --space ${group%%|*}-${VARIANT} --patch --resolve 'Link:*' ${group##*|}"
done
exit 0

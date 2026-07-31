#!/usr/bin/env bash
# Install or update each component's base Space in ConfigHub.
#
#   scripts/install.sh                 create or update from ./configs/
#   scripts/install.sh --from-oci      first install from the published bundles
#   scripts/install.sh --dry-run       report what would change
#
# WHY THIS IS NOT JUST `cub variant upload`:
#
# `cub variant upload --allow-exists` TOLERATES existing Units, it does not
# update them. Re-running it against a changed bundle reports success, creates
# nothing, and silently leaves the base at its old content — after which
# `cub variant promote` has nothing to propagate and also reports success. Two
# green commands and no change. This was not obvious and cost real debugging
# time, so install has explicit create-or-update semantics instead.
#
# Downstream variants are NOT touched. After updating a base, promote it:
#   cub variant promote <component>-<variant>
#   cub release publish <component>-<variant>

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

# shellcheck disable=SC1091
source versions.env

FROM_OCI=0
DRY_RUN=0

die() {
	echo "ERROR: $*" >&2
	exit 1
}
note() { echo "  $*"; }

while [[ $# -gt 0 ]]; do
	case "$1" in
	--from-oci) FROM_OCI=1; shift ;;
	--registry) REGISTRY="${2:?}"; shift 2 ;;
	--dry-run) DRY_RUN=1; shift ;;
	-h | --help)
		sed -n '2,6p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
		exit 0
		;;
	*) die "unknown option: $1" ;;
	esac
done

cubx() { CONFIGHUB_AGENT=1 cub "$@"; }

# HeadRevisionNum is the only reliable "did anything change" signal (see the
# comment in the update path below).
rev_of() {
	cubx unit get --space "$1" "$2" -o json 2>/dev/null |
		jq -r '.Unit.HeadRevisionNum // .HeadRevisionNum // "?"'
}
cubx auth status >/dev/null 2>&1 || die "not authenticated to ConfigHub — run 'cub auth login'"

COMPONENTS=()
while IFS= read -r _c; do
	[[ -n "$_c" && -d "configs/${_c}" ]] && COMPONENTS+=("$_c")
done < <(yq -r '.components[].name' components.yaml)
[[ "${#COMPONENTS[@]}" -gt 0 ]] || die "nothing rendered — run 'make render' first"

created=0
updated=0
unchanged=0

for component in "${COMPONENTS[@]}"; do
	space="${component}-base"
	echo "==> ${component}"

	if ! cubx space get "$space" >/dev/null 2>&1; then
		# First install: variant upload creates the Space, the Units, the
		# well-known labels, and the inferred links in one step.
		local_src="./configs/${component}"
		[[ "$FROM_OCI" == "1" ]] && local_src="oci://${REGISTRY}/${component}"
		if [[ "$DRY_RUN" == "1" ]]; then
			note "would CREATE base from ${local_src}"
			continue
		fi
		cubx variant upload \
			--component "$component" --variant base \
			--granularity per-file \
			--label managed-by=eks-inference \
			"$local_src" >/dev/null || die "upload failed for ${component}"
		note "created base from ${local_src}"
		created=$((created + 1))
		continue
	fi

	# Space exists: reconcile Unit by Unit. File stem == Unit slug, which is what
	# --granularity per-file guarantees.
	for f in "configs/${component}"/*.yaml; do
		[[ -f "$f" ]] || continue
		slug="$(basename "$f" .yaml)"

		if cubx unit get --space "$space" "$slug" >/dev/null 2>&1; then
			# Always send the update and let ConfigHub decide whether it is a
			# change: it no-ops on identical data, leaving the revision number
			# alone. Comparing client-side does not work — ConfigHub prepends a
			# "# Source:" header and normalises YAML list indentation, so the
			# stored bytes never equal the file's even when nothing changed.
			# The revision number is the only honest signal.
			if [[ "$DRY_RUN" == "1" ]]; then
				note "would sync ${slug}"
				updated=$((updated + 1))
				continue
			fi
			before="$(rev_of "$space" "$slug")"
			cubx unit update --space "$space" "$slug" "$f" >/dev/null ||
				die "could not update ${space}/${slug}"
			after="$(rev_of "$space" "$slug")"
			if [[ "$before" == "$after" ]]; then
				unchanged=$((unchanged + 1))
			else
				note "updated ${slug} (rev ${before} -> ${after})"
				updated=$((updated + 1))
			fi
		else
			if [[ "$DRY_RUN" == "1" ]]; then
				note "would CREATE ${slug}"
			else
				cubx unit create --space "$space" "$slug" "$f" >/dev/null ||
					die "could not create ${space}/${slug}"
				note "created ${slug}"
			fi
			created=$((created + 1))
		fi
	done

	# A Unit with no corresponding file is a source that was renamed or deleted.
	# Report it rather than delete it — removing a Unit that a downstream variant
	# still tracks is not something to do implicitly.
	for slug in $(cubx unit list --space "$space" --no-headers 2>/dev/null | awk '{print $1}'); do
		[[ -f "configs/${component}/${slug}.yaml" ]] ||
			note "ORPHAN: unit '${slug}' has no source file — delete it by hand if intended"
	done
done

echo
if [[ "$DRY_RUN" == "1" ]]; then
	echo "Dry run: ${created} to create, ${updated} to sync (ConfigHub decides which actually change)."
else
	echo "Bases: ${created} created, ${updated} updated, ${unchanged} unchanged."
	# An `if` block, not `[[ ... ]] && cat`: as the script's last command, a false
	# test would become the script's exit status and fail `make install` on a
	# perfectly successful no-op run.
	if [[ "$updated" -gt 0 || "$created" -gt 0 ]]; then
		cat <<'EOF'

Bases changed. Downstream variants are unaffected until you promote:
  cub variant promote <component>-<variant>
  cub release publish <component>-<variant>
EOF
	fi
fi

exit 0

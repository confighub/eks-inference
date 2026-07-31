#!/usr/bin/env bash
# Render every component into literal Kubernetes YAML.
#
# Usage: scripts/render.sh <output-dir>
#
# One file per Unit: the bundles are installed with
# `cub variant upload --granularity per-file`, so the file layout here IS the
# Unit layout in ConfigHub. Renaming a file renames a Unit — treat these names
# as an interface, not an implementation detail.
#
# NOTE ON HOOKS: we deliberately do NOT pass --no-hooks to `helm template`.
# Hook resources render into the output where scripts/guard.sh can see and
# reject them. Suppressing them here would silently drop install-time behaviour
# and we would find out on a live cluster instead of in CI. See docs/flattening.md.

set -euo pipefail

die() {
	echo "ERROR: $*" >&2
	exit 1
}

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

# shellcheck disable=SC1091
source versions.env

out="${1:?usage: render.sh <output-dir>}"
mkdir -p "$out"

# Annotate a rendered stream with an Argo CD sync wave. ConfigHub does not
# sequence applies; the GitOps operator does. Waves are how ordering is
# expressed, so they are part of the rendered artifact.
set_wave() {
	yq -i "(select(.kind != null) | .metadata.annotations.\"argocd.argoproj.io/sync-wave\") = \"$1\"" "$2"
}

# Render one chart into <name>-crds.yaml and <name>-controller.yaml, applying the
# same CRD split and document-conservation check as the ACK controllers.
render_chart() {
	local name="$1" chart="$2" version="$3" ns="$4" values="$5" repo="$6" dest="$7"
	local tmp
	tmp="$(mktemp)"

	local -a args=(template "$name" "$chart" --version "$version" --namespace "$ns"
		--kube-version "${RENDER_KUBE_VERSION}" --include-crds)
	[[ -n "$repo" ]] && args+=(--repo "$repo")
	[[ -n "$values" ]] && args+=(--values "$values")

	helm "${args[@]}" >"$tmp" || die "helm template failed for ${name}"

	yq 'select(.kind == "CustomResourceDefinition")' "$tmp" >"${dest}/${name}-crds.yaml"
	yq 'select(.kind != "CustomResourceDefinition" and .kind != null)' "$tmp" >"${dest}/${name}-controller.yaml"

	# A chart with no CRDs leaves an empty file, which would become an empty Unit.
	[[ -s "${dest}/${name}-crds.yaml" ]] || rm -f "${dest}/${name}-crds.yaml"

	[[ -f "${dest}/${name}-crds.yaml" ]] && set_wave "-20" "${dest}/${name}-crds.yaml"
	set_wave "-10" "${dest}/${name}-controller.yaml"

	local in_count out_count crd_count ctl_count
	in_count="$(yq -N 'select(.kind != null) | .kind' "$tmp" | wc -l | tr -d ' ')"
	crd_count=0
	[[ -f "${dest}/${name}-crds.yaml" ]] &&
		crd_count="$(yq -N 'select(.kind != null) | .kind' "${dest}/${name}-crds.yaml" | wc -l | tr -d ' ')"
	ctl_count="$(yq -N 'select(.kind != null) | .kind' "${dest}/${name}-controller.yaml" | wc -l | tr -d ' ')"
	out_count=$((crd_count + ctl_count))
	if [[ "$in_count" != "$out_count" ]]; then
		die "${name} split lost documents: helm emitted ${in_count}, wrote ${out_count}."
	fi
	echo "  ${name}: ${out_count} resources"

	rm -f "$tmp"
}

render_ack_controller() {
	local svc="$1" version="$2" dest="$3"
	local tmp
	tmp="$(mktemp)"

	helm template "ack-${svc}" \
		"oci://public.ecr.aws/aws-controllers-k8s/${svc}-chart" \
		--version "${version}" \
		--namespace "${ACK_NAMESPACE}" \
		--kube-version "${RENDER_KUBE_VERSION}" \
		--include-crds \
		--values "src/ack-controllers/values/${svc}.yaml" \
		>"$tmp"

	# CRDs land in their own Unit. They have a different lifecycle from the
	# controller (never garbage collected, upgraded out of band) and ConfigHub's
	# own `minimal` granularity splits them out for the same reason.
	# No -N here: it suppresses the "---" separators, which silently concatenates
	# every document into one unparseable blob. The split must preserve them.
	yq 'select(.kind == "CustomResourceDefinition")' "$tmp" >"${dest}/${svc}-crds.yaml"
	yq 'select(.kind != "CustomResourceDefinition" and .kind != null)' "$tmp" >"${dest}/${svc}-controller.yaml"

	set_wave "-20" "${dest}/${svc}-crds.yaml"
	set_wave "-10" "${dest}/${svc}-controller.yaml"

	# Conservation check: every document helm emitted must land in exactly one of
	# the two output files. Splitting a multi-document stream is easy to get
	# subtly wrong (a yq flag that drops "---" separators will silently collapse
	# 22 CRDs into 1), and the result still looks like valid YAML. Counting is
	# the cheapest way to make that failure loud.
	# Count each file on its own and sum. Do NOT cat them together first: without
	# a "---" between them the last document of one file merges into the first of
	# the next, which undercounts by one per boundary.
	local in_count out_count crd_count ctl_count
	in_count="$(yq -N 'select(.kind != null) | .kind' "$tmp" | wc -l | tr -d ' ')"
	crd_count="$(yq -N 'select(.kind != null) | .kind' "${dest}/${svc}-crds.yaml" | wc -l | tr -d ' ')"
	ctl_count="$(yq -N 'select(.kind != null) | .kind' "${dest}/${svc}-controller.yaml" | wc -l | tr -d ' ')"
	out_count=$((crd_count + ctl_count))
	if [[ "$in_count" != "$out_count" ]]; then
		echo "ERROR: ${svc} split lost documents: helm emitted ${in_count}, wrote ${out_count}." >&2
		exit 1
	fi
	echo "  ${svc}: ${out_count} resources"

	rm -f "$tmp"
}

# The component set comes from components.yaml, not from a list kept here. A
# component added to one script's array and forgotten in another is how stale
# files survive in configs/ and how a component silently never gets deployed.
# Read with a while-loop rather than mapfile: mapfile is bash 4+, and macOS still
# ships bash 3.2 as /bin/bash, which `env bash` picks up without Homebrew bash.
COMPONENTS=()
while IFS= read -r _c; do
	[[ -n "$_c" ]] && COMPONENTS+=("$_c")
done < <(yq -r '.components[].name' components.yaml)
[[ "${#COMPONENTS[@]}" -gt 0 ]] || die "no components found in components.yaml"

render_of() { yq -r ".components[] | select(.name == \"$1\") | .render" components.yaml; }

# Each component directory is rebuilt from scratch. Without this, deleting or
# renaming a source file leaves the old rendered output behind — and a stale file
# in configs/ ships in the OCI bundle and becomes a Unit that no source produces.
# Only the component directories we own are removed, never $out itself.
for component in "${COMPONENTS[@]}"; do
	rm -rf "${out:?}/${component}"
done

for component in "${COMPONENTS[@]}"; do
	case "$(render_of "$component")" in
	ack-charts)
		echo "==> ${component}"
		mkdir -p "${out}/${component}"
		cp src/ack-controllers/namespace.yaml "${out}/${component}/"
		render_ack_controller iam "${ACK_IAM_CHART_VERSION}" "${out}/${component}"
		render_ack_controller ec2 "${ACK_EC2_CHART_VERSION}" "${out}/${component}"
		render_ack_controller eks "${ACK_EKS_CHART_VERSION}" "${out}/${component}"
		;;
	copy)
		# Handwritten ACK custom resources and plain manifests — no chart to
		# flatten. Copied verbatim so what you review in src/ is byte-identical
		# to what ships in the bundle.
		if compgen -G "src/${component}/*.yaml" >/dev/null; then
			echo "==> ${component}"
			mkdir -p "${out}/${component}"
			cp "src/${component}"/*.yaml "${out}/${component}/"
		else
			echo "==> ${component} (no sources yet, skipping)"
		fi
		;;
	helm)
		echo "==> ${component}"
		mkdir -p "${out}/${component}"
		case "$component" in
		karpenter)
			render_chart karpenter \
				"oci://public.ecr.aws/karpenter/karpenter" \
				"${KARPENTER_CHART_VERSION}" kube-system \
				"src/karpenter/values/karpenter.yaml" \
				"" "${out}/${component}"
			;;
		gpu-runtime)
			# An https chart repo rather than OCI. --repo avoids depending on
			# `helm repo add` having been run, so the render works from a clean
			# checkout and in CI without extra state.
			render_chart nvidia-device-plugin \
				"nvidia-device-plugin" \
				"${NVIDIA_DEVICE_PLUGIN_CHART_VERSION}" gpu-operator \
				"src/gpu-runtime/values/nvidia-device-plugin.yaml" \
				"https://nvidia.github.io/k8s-device-plugin" \
				"${out}/${component}"
			;;
		*)
			die "component '${component}' is render:helm but has no chart defined in render.sh"
			;;
		esac
		# Handwritten resources that ship alongside the chart (NodePools,
		# EC2NodeClasses). Copied verbatim, same as a `copy` component.
		if compgen -G "src/${component}/*.yaml" >/dev/null; then
			cp "src/${component}"/*.yaml "${out}/${component}/"
		fi
		;;
	*)
		die "component '${component}' has an unknown render type in components.yaml"
		;;
	esac
done

echo
echo "Rendered into ${out}/"
find "$out" -name '*.yaml' | sort | sed 's/^/  /'

scripts/guard.sh "$out"

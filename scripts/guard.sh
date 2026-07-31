#!/usr/bin/env bash
# Reject Helm constructs that do not survive being flattened into literal YAML.
#
# Usage: scripts/guard.sh <dir>
#
# `helm template` renders a chart's *templates*. It does not render Helm's
# *runtime*: hooks do not fire, resource policies are not honoured, lookup()
# returns empty, and there is no release to roll back. For most charts that is
# fine — but when it is not fine, the failure is silent and shows up on a live
# cluster. This turns that silent class of bug into a build failure.
#
# When a hazard is legitimate, record it in src/<component>/HAZARDS.allow with a
# line explaining how it is handled. Acknowledging is cheap; discovering a
# dropped pre-install Job during a demo is not.

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
dir="${1:?usage: guard.sh <dir>}"

fail=0

# hazard <label> <grep-pattern> <explanation>
hazard() {
	local label="$1" pattern="$2" explanation="$3"
	local hits component allow

	hits="$(grep -rlE "$pattern" "$dir" 2>/dev/null || true)"
	[[ -z "$hits" ]] && return 0

	while IFS= read -r file; do
		component="$(basename "$(dirname "$file")")"
		allow="${repo_root}/src/${component}/HAZARDS.allow"
		if [[ -f "$allow" ]] && grep -qxF "$label" "$allow"; then
			echo "  allowed: ${label} in ${file#"$dir"/} (per src/${component}/HAZARDS.allow)"
			continue
		fi
		echo "  HAZARD:  ${label} in ${file#"$dir"/}"
		echo "           ${explanation}"
		fail=1
	done <<<"$hits"
}

echo
echo "==> flattening guard"

hazard "helm-hook" \
	'helm\.sh/hook' \
	"Hook resources never fire without Helm. Translate to an Argo sync hook (argocd.argoproj.io/hook) or drop the chart feature."

hazard "helm-resource-policy" \
	'helm\.sh/resource-policy' \
	"Marks a resource Helm must not delete. Argo will happily prune it. Add argocd.argoproj.io/sync-options: Prune=false."

hazard "bare-job" \
	'^kind: (Job|CronJob)' \
	"Jobs are immutable; re-applying an unchanged Job fails. Confirm it is idempotent under repeated sync or make it an Argo hook."

hazard "admission-webhook" \
	'^kind: (Mutating|Validating)WebhookConfiguration' \
	"Webhook CA bundles are usually injected by a hook Job or cert-manager. Flattened, the caBundle may be empty and every admission call fails."

hazard "empty-ca-bundle" \
	'caBundle: ""' \
	"An empty caBundle confirms the CA injection step was lost in flattening."

if [[ "$fail" -ne 0 ]]; then
	echo
	echo "Flattening guard failed. See docs/flattening.md."
	exit 1
fi

echo "  clean — no unhandled Helm constructs."

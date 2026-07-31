#!/usr/bin/env bash
# Package configs/ into OCI config bundles and push them.
#
# Usage: scripts/bundle.sh package <component>...
#        scripts/bundle.sh push    <component>...
#
# The tarballs are byte-reproducible: fixed entry order, zeroed ownership, epoch
# mtime, and gzip -n (no embedded filename/timestamp). Combined with the pinned
# org.opencontainers.image.created annotation, identical content yields an
# identical manifest digest — so re-running the build on an unchanged tree
# re-points :latest at the digest that is already there instead of churning it.
# That is what makes the digest recorded on the ConfigHub Space meaningful.

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

# shellcheck disable=SC1091
source versions.env

# Reproducible tarballs need GNU tar. macOS ships bsdtar, which cannot set entry
# mtime at all — the resulting digest would change on every run.
tar_bin=""
for candidate in gtar tar; do
	if command -v "$candidate" >/dev/null 2>&1 && "$candidate" --version 2>/dev/null | grep -q 'GNU tar'; then
		tar_bin="$candidate"
		break
	fi
done
if [[ -z "$tar_bin" ]]; then
	echo "ERROR: GNU tar is required for reproducible bundles (bsdtar cannot set mtime)." >&2
	echo "       macOS: brew install gnu-tar" >&2
	exit 1
fi

cmd="${1:?usage: bundle.sh <package|push> <component>...}"
shift

case "$cmd" in
package)
	mkdir -p dist
	for component in "$@"; do
		[[ -d "configs/${component}" ]] || {
			echo "ERROR: configs/${component} does not exist — run 'make render' first." >&2
			exit 1
		}
		"$tar_bin" --sort=name --owner=0 --group=0 --numeric-owner \
			--mtime='UTC 1970-01-01' \
			-cf - -C "configs/${component}" . | gzip -n >"dist/${component}.tar.gz"
		echo "  dist/${component}.tar.gz  ($(wc -c <"dist/${component}.tar.gz" | tr -d ' ') bytes)"
	done
	;;

push)
	for component in "$@"; do
		[[ -f "dist/${component}.tar.gz" ]] || {
			echo "ERROR: dist/${component}.tar.gz missing — run 'make bundles' first." >&2
			exit 1
		}
		# Pushed from inside dist/ so the layer's org.opencontainers.image.title is
		# a bare "<component>.tar.gz". oras uses the path exactly as given, so
		# pushing "dist/<component>.tar.gz" from the repo root records that whole
		# path as the title — and `oras pull` then recreates a dist/ directory on
		# the consumer's disk. Harmless for `cub variant upload`, which reads the
		# layer either way, but it makes these bundles inconsistent with the other
		# confighub config bundles (argobot, cubbychat) that ship bare filenames.
		(
			cd dist && oras push \
				"${REGISTRY}/${component}:latest" \
				--artifact-type application/vnd.confighub.config.bundle.v1 \
				--annotation "org.opencontainers.image.created=1970-01-01T00:00:00Z" \
				"${component}.tar.gz:application/vnd.oci.image.layer.v1.tar+gzip"
		)
		# Assert the layout rather than trust it. This is the only place the
		# bundle's on-disk shape is decided, and getting it wrong is invisible
		# until someone runs `oras pull` and finds an unexpected directory.
		title="$(oras manifest fetch "${REGISTRY}/${component}:latest" 2>/dev/null |
			jq -r '.layers[0].annotations["org.opencontainers.image.title"] // ""')"
		if [[ "$title" != "${component}.tar.gz" ]]; then
			echo "ERROR: layer title is '${title}', expected '${component}.tar.gz'." >&2
			echo "       The bundle would extract into an unexpected directory." >&2
			exit 1
		fi

		echo "  pushed ${REGISTRY}/${component}:latest  (layer: ${title})"
	done
	;;

*)
	echo "ERROR: unknown subcommand '${cmd}' (expected package or push)" >&2
	exit 1
	;;
esac

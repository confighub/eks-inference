# Flattening Helm charts into literal config

This repo turns Helm charts into literal Kubernetes YAML and ships that. Nothing
in the delivery path runs Helm: ConfigHub stores the rendered resources as Units,
and Argo CD applies them. `cub variant upload` is explicit that it "does not
render anything; it ingests what you give it."

That is the point — literal config is reviewable, diffable, and independently
addressable as Units. But flattening silently discards part of what a chart does,
and the discarded parts fail on a live cluster rather than in the build. This
page is the record of what we lose, what we check for, and what to do when a
chart needs something we cannot flatten.

## What `helm template` does not give you

`helm template` renders a chart's *templates*. It does not render Helm's
*runtime*. Specifically:

| Construct | What breaks when flattened |
|---|---|
| `helm.sh/hook` | Hook Jobs never fire. Pre-install migrations, CRD installers, and cert-generation jobs simply do not happen. |
| `helm.sh/resource-policy: keep` | Marks a resource Helm must not delete. Argo has never heard of it and will prune. |
| `lookup()` | Returns empty at template time. Charts that self-discover cluster state render *valid but wrong* output. |
| Webhook CA injection | A chart that generates its own serving cert in a hook Job renders with an empty `caBundle`. Every admission call then fails closed. |
| `.Capabilities` | `helm template` guesses the cluster version unless told. A chart branching on API availability can emit the wrong `apiVersion`. |
| Release state | No `helm list`, no `helm rollback`. |

The last one is not a loss — it is a substitution. ConfigHub owns revisions and
rollback for these Units. That is the trade being made deliberately.

## What we do about it

**1. Hooks are rendered, not suppressed.** `scripts/render.sh` deliberately does
*not* pass `--no-hooks`. Hook resources render into the output where the guard
can see them. Suppressing them at render time would hide the problem rather than
surface it.

**2. `scripts/guard.sh` fails the build** on `helm.sh/hook`,
`helm.sh/resource-policy`, bare `Job`/`CronJob`, admission webhook
configurations, and empty `caBundle` values. It runs on every render and in CI
via `make bundles`.

**3. Hazards are acknowledged in writing, not in someone's memory.** When a
construct is legitimate, add its label to `src/<component>/HAZARDS.allow` with a
comment explaining how it is handled. The guard then permits that one label in
that one component and reports it as allowed. There is no global override.

**4. `--kube-version` is pinned** in `versions.env`, so `.Capabilities` does not
depend on whatever kubectl the operator happens to have installed.

**5. Document conservation is checked.** The render splits each chart's output
into a CRD Unit and a controller Unit, and then asserts that the number of
documents written equals the number Helm emitted. This is not paranoia: the first
version of this repo silently collapsed 22 EC2 CRDs into 1, because `yq -N`
suppresses `---` separators. The output was still valid YAML. It still applied
without error. It was just missing twenty-one CRDs. Counting makes that loud.

## Current status

All three ACK controller charts (`ec2`, `iam`, `eks`) are clean: zero hooks, zero
Jobs, zero webhooks, zero resource policies. No `HAZARDS.allow` file exists yet,
and that is the honest state of the repo today, not an oversight.

## Where this will bite

The EKS-side components are the ones to watch:

- **Karpenter** ships CRDs in a *separate* `karpenter-crd` chart precisely because
  CRD lifecycle differs from the controller's. Older versions used hooks to patch
  conversion webhooks.
- **NVIDIA GPU Operator** is the hard case: hook-driven cleanup, a Node Feature
  Discovery subchart, and driver container orchestration.
- **cert-manager**, if it ever appears, has both a `startupapicheck` hook Job and
  admission webhooks.

For those, the flattened path is not automatically the right one. Two options:

1. **Translate the hook to an Argo sync hook.** Since delivery is Argo CD,
   `helm.sh/hook: pre-install` maps reasonably onto
   `argocd.argoproj.io/hook: PreSync` with
   `argocd.argoproj.io/hook-delete-policy: HookSucceeded`. This is a real
   transform the render step can perform — it just has to be written deliberately
   per chart, not applied blindly.
2. **Do not flatten that chart.** Some charts are better delivered as a Helm
   release that Argo manages, with ConfigHub owning the values rather than the
   rendered output. That is a legitimate answer and the guard failing is the
   signal to consider it.

The guard's job is not to make flattening always work. It is to make sure we
choose, rather than find out during a demo.

## Why `configs/` is committed

The rendered output is committed to git, and `make verify` fails CI if it drifts
from what the sources produce.

This is what makes a version bump reviewable. Change `ACK_EC2_CHART_VERSION` in
`versions.env`, run `make render`, and the diff in `configs/` is the actual
change — new CRD fields, changed RBAC, a new container arg. Without the committed
output, upgrading a chart is a one-line diff whose real effect is invisible until
it reaches a cluster.

It also means `cub variant upload ./configs/aws-network` works from a checkout
with no Helm installed.

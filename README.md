# eks-inference

An EKS cluster for inference workloads, provisioned from a local kind cluster
through [AWS Controllers for Kubernetes](https://aws-controllers-k8s.github.io/community/)
(ACK) and managed as data in [ConfigHub](https://confighub.com).

Karpenter provisions GPU nodes on demand: a quantized-LLM pool on L4/A10G spot,
and an H200 pool for when you have the capacity reservation to use it.

Everything is driven by config. Scaling a model up is a change to a Unit and a
release, not a `kubectl` command.

## Install

```bash
cub plugin install confighub/eks-inference
cub eksinf --help
```

The plugin is the admin tool for this stack. You do not need this repo checked
out to use it.

## Quick start

```bash
# 1. A local management cluster, wired to ConfigHub via Argo CD.
cub cluster up --name inference-mgmt

# 2. Install the component bases from their published OCI bundles.
cub eksinf install

# 3. The parameter surface. Its Space has no Target and is never deployed.
cub variant create dev platform-profile-base

# 4. Give the ACK controllers AWS credentials. The one out-of-band step —
#    the Secret is never a ConfigHub Unit. Do this BEFORE deploying: the
#    controllers read credentials once at startup, and a bad identity fails
#    here in a second rather than as a condition on a VPC later.
cub eksinf creds create-user --yes        # or: creds use-existing

# 5. Deploy the management plane. This creates AWS infrastructure.
cub eksinf deploy --plane mgmt --target inference-mgmt/target

# 6. Watch it converge. ~5 min for the network, ~15 for the EKS control plane.
cub eksinf status
```

When the EKS cluster is `ACTIVE`, bring it under management and deploy the
workload plane onto it:

```bash
# 7. Enroll EKS: install Argo CD, register a worker and OCI target, bootstrap
#    the root app-of-apps. Never creates or destroys a cluster.
cub eksinf enroll cluster --name inference-demo \
  --eks-cluster inference-demo --region us-west-2

# 8. Deploy Karpenter, the GPU runtime, and the workloads.
cub eksinf deploy --plane workload --target inference-demo/target
```

`deploy` links each plane to the `platform-profile` itself, between creating the
variants and publishing them — it has to, since publishing is gated on there
being no unfilled placeholders left. `cub eksinf link-profile` is still there to
inspect or rework the links after the fact:

```bash
cub eksinf link-profile --list      # show the bindings and existing links
cub eksinf link-profile --unlink    # remove them
```

Nothing is running yet beyond the system nodegroup: every workload ships at
`replicas: 0`, so installing costs nothing. To actually provision a GPU:

```bash
cub function do --space inference-workloads-dev --where "Slug = 'smoke-gpu'" set-replicas 1
cub release publish inference-workloads-dev
```

Karpenter launches a `g6.xlarge` in about 90 seconds. Scale back to `0` and it is
released. **Do not use `kubectl scale`** — the Argo Application syncs with
`selfHeal: true`, so a manual scale is reverted within a minute, having reported
success.

## The two apply planes

The single fact that shapes this repo: **kind and EKS are different apply
targets.** Components are separated by which cluster applies them before they are
separated by anything else.

```
  kind (cub cluster up)              AWS                  EKS (cub eksinf enroll)
  ─────────────────────              ───                  ──────────────────────
  ack-controllers  ──────────────▶   VPC, subnets, NAT
  aws-network      ──────────────▶   IAM roles            karpenter
  eks-cluster      ──────────────▶   EKS control plane    gpu-runtime
  karpenter-aws    ──────────────▶   Karpenter IAM        inference-workloads
```

Ownership is split, never migrated. kind keeps the provisioning plane
permanently; the workload plane belongs to EKS from the day it is written. Since
nothing moves between planes there is no adoption step, and no way for the
cluster to delete itself.

Karpenter is the case that proves the point: its IAM role and Pod Identity
association are ACK resources only kind can create, while its controller and
NodePools run on EKS. One component in each plane.

`cub eksinf components` lists them.

## How values cross component boundaries

Three mechanisms, three different times. Choosing the wrong one is the main way
this goes wrong.

| Mechanism | Carries | When |
|---|---|---|
| ConfigHub links | names, CIDRs, tags, AMI aliases | config time, in the hub |
| ACK `*Ref` fields | actual AWS IDs (`vpc-0a1b…`) | runtime, in-cluster |
| Argo sync waves | apply ordering | apply time |

AWS IDs do not exist at config time, so ConfigHub cannot propagate them — ACK
resolves object names to IDs itself. What ConfigHub is for is the values that
must *agree* across components, of which `karpenter.sh/discovery` is the sharpest
example: it spans two planes, and a mismatch produces no error at all, just a
Karpenter that never launches a node.

**Links span the plane boundary; sync waves cannot.** A single edit to the
`platform-profile` Unit reaches components applied by two different clusters.
Ordering between planes is not expressible in config — deploy mgmt, let it
converge, then deploy workload.

See [docs/dependencies.md](./docs/dependencies.md).

## What this costs

Idle, in `us-west-2`, with nothing scheduled:

| Resource | Approx. monthly |
|---|---|
| EKS control plane | $73 |
| NAT gateway (1) | $33 + data processing |
| 2 × t4g.medium | $24 |
| **Total** | **~$130/month** |

GPU nodes are on top of that and only exist while a workload asks for one:
roughly $0.80/hr for a `g6.xlarge`, and tens of dollars an hour for H200.

`cub eksinf status` reports what is running **from EC2, not from Kubernetes** — a
Node object can outlive its instance, and an unreachable cluster reports zero
nodes, so kubectl is wrong in both directions.

The ACK controllers run with `deletionPolicy: retain`, so deleting Units or
letting Argo prune them does **not** delete AWS resources. Teardown is deliberate
— see [docs/teardown.md](./docs/teardown.md).

## Developing this repo

Only needed to change the config itself or cut a release.

```bash
make render    # helm charts + handwritten CRs -> configs/
make verify    # fail if configs/ drifts from sources (CI gate)
make bundles   # configs/ -> dist/<component>.tar.gz
make push      # -> ghcr.io/confighub/configs/eks-inference/<component>:latest
make plugin    # build ./eksinf locally
make check     # go vet + go test + gofmt, as CI runs them
```

CI runs exactly these targets; there is no build logic in the workflow files.

The rendered output in `configs/` is committed on purpose: it makes a chart
version bump reviewable as a diff, and it is what the OCI bundles contain. See
[docs/flattening.md](./docs/flattening.md) for what is lost when a Helm chart is
flattened into literal YAML, and how the build guards against it.

Two release cadences, deliberately independent:

- **config bundles** float at `:latest`, republished on every push to `main`
- **the plugin** is cut from a `v*` tag as a GitHub release

### Layout

```
components.yaml           the component set: name, plane, order. Embedded in the plugin.
versions.env              pinned chart versions and render inputs
Makefile                  build entry points
main.go, embed.go, cmd/   the eksinf plugin
scripts/render.sh         helm template + copy -> configs/
scripts/guard.sh          rejects Helm constructs that do not survive flattening
scripts/bundle.sh         reproducible tarballs + oras push
src/                      sources: chart values and handwritten ACK resources
configs/                  rendered output (committed; one file per ConfigHub Unit)
iam/                      the IAM policy `creds create-user` attaches
```

File names in `configs/` are an interface: bundles install with
`--granularity per-file`, so each file becomes one Unit and renaming a file
renames a Unit.

### Taking a newer bundle

```bash
cub eksinf install
```

Re-running install takes the current bundles. A re-upload 3-way merges the new
bundle against the last one, so Unit IDs, target bindings and links survive, and
so do changes made in ConfigHub afterwards — which matters here, because
`link-profile` and the `set-env-var` setters mutate Units after upload. A bundle
that has not moved is a no-op.

Bases are upstreams, so taking a newer one does not move anything that is
deployed. Promote per variant when you want it:

```bash
cub variant promote <component>-dev
cub release publish <component>-dev
```

`--prune` additionally empties Units the bundle no longer produces; `--recreate`
deletes and rebuilds, which is only needed to change granularity. See
[docs/install.md](./docs/install.md#taking-a-newer-bundle).

### Versioning the plugin

`0.MINOR.PATCH`, and the distinction is not decorative — the number is the only
thing a reader has to go on when deciding whether an upgrade needs attention.

- **PATCH** — fixes. Something did not work and now does. This is the default,
  and a release of nothing but fixes is a patch release however many commits it
  contains.
- **MINOR** — new commands or flags, or a change that can make a previously
  working invocation fail: a new gate, a renamed flag, a different default.

Pre-1.0 lets us change anything at any time; it is not a licence to make the
version meaningless. Bumping MINOR by reflex is how that happens, and v0.7.0 is
an example — eight commits, all fixes, tagged as though it added something.

Releases are cut from a tag:

```bash
git tag v0.7.1 && git push origin v0.7.1
```

If GitHub is not processing push events (it happens), dispatch the workflow
against the TAG rather than main — the version is derived from the ref, so
dispatching against main builds a plugin that calls itself `main`:

```bash
gh workflow run release-plugin.yml --ref v0.7.1
```

## Docs

- [dependencies.md](./docs/dependencies.md) — how values cross component boundaries, and the path-escaping trap
- [flattening.md](./docs/flattening.md) — why Helm is rendered to literal YAML, what breaks, how the guard works
- [aws-credentials.md](./docs/aws-credentials.md) — credential modes, the IAM policy, SSO sessions
- [karpenter.md](./docs/karpenter.md) — the node pools, why the GPU AMI is pinned, the interruption queue
- [install.md](./docs/install.md) — what the install and deploy commands actually do
- [teardown.md](./docs/teardown.md) — deleting AWS resources, given `deletionPolicy: retain`

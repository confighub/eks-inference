# eks-inference

Config bundles that provision an EKS cluster for inference workloads, driven from
a local kind cluster via [AWS Controllers for Kubernetes](https://aws-controllers-k8s.github.io/community/)
(ACK) and managed as data in [ConfigHub](https://confighub.com).

You run `cub cluster up` to get a kind cluster wired to ConfigHub, install these
components into it, hand it AWS credentials, and a VPC and an EKS cluster appear.

## Status: step one

This repo currently ships the **provisioning plane** — the components that run in
kind and create AWS infrastructure:

| Component | Bundle | What it creates |
|---|---|---|
| `ack-controllers` | `ghcr.io/confighub/configs/eks-inference/ack-controllers` | ACK EC2, IAM, and EKS controllers + their CRDs |
| `aws-network` | `ghcr.io/confighub/configs/eks-inference/aws-network` | VPC, 3 public + 3 private subnets, IGW, NAT, route tables, security group |
| `eks-cluster` | `ghcr.io/confighub/configs/eks-inference/eks-cluster` | Cluster and node IAM roles, EKS control plane, pod identity addon, t4g.medium system nodegroup |

Not yet built: the **workload plane** — Karpenter, the inference nodepools
(quantized-GPU and H200), and the inference runtime. Those target the EKS cluster
rather than kind, and are gated on being able to adopt EKS as a ConfigHub target.

## Two apply planes

The single fact that shapes this repo: **kind and EKS are different apply
targets**, and components are separated by which one applies them before they are
separated by anything else.

```
  kind cluster (cub cluster up)          AWS                    EKS cluster
  ─────────────────────────────          ───                    ───────────
  ack-controllers  ──────────────────▶   VPC, subnets, NAT
  aws-network      ──────────────────▶   IAM roles
  eks-cluster      ──────────────────▶   EKS control plane  ──▶  (workload plane
                                         system nodegroup         lands here)
```

Ownership is split rather than migrated. kind keeps the provisioning plane
permanently — those resources change rarely and deleting them is catastrophic.
The workload plane will belong to EKS from the day it is written. Nothing ever
moves between planes, so there is no adoption step and no risk of the cluster
deleting itself.

## Dependencies between components

Three mechanisms, three different times:

| Mechanism | Carries | When |
|---|---|---|
| ConfigHub links | names, CIDRs, region, tags, cluster name | config time, in the hub |
| ACK `*Ref` fields | actual AWS IDs (`vpc-0a1b…`) | runtime, in-cluster |
| Argo sync waves | apply ordering | apply time |

AWS IDs do not exist at config time, so ConfigHub cannot propagate them — ACK
resolves object names to IDs itself. What ConfigHub is for here is the values
that must *agree* across components, of which `karpenter.sh/discovery` is the
sharpest example. See [docs/dependencies.md](./docs/dependencies.md).

## Prerequisites

- `cub`, `helm`, `yq`, `kubectl`, `kind`, `docker`, `oras`
- GNU tar (`brew install gnu-tar` on macOS) — only needed to build bundles
- An AWS account you are willing to create a VPC and an EKS cluster in
- Docker running locally

## Quick start

```bash
# 1. A kind cluster wired to ConfigHub via Argo CD.
cub cluster up --name inference-mgmt

# 2. Install the three components as ConfigHub bases from their OCI bundles.
make install

# 3. Create a downstream variant of each component, bound to the cluster's OCI
#    target. This also auto-creates the Argo CD Application for each one.
for c in ack-controllers aws-network eks-cluster; do
  cub variant create dev "${c}-base" --target inference-mgmt/target
done

# 4. Publish each variant's release. Until you do, the Applications point at a
#    bundle that does not exist yet.
for c in ack-controllers aws-network eks-cluster; do
  cub release publish "${c}-dev"
done

# 5. Give the controllers AWS credentials. This is the one out-of-band step;
#    the Secret is never a ConfigHub Unit.
scripts/aws-creds.sh create-user      # or use-existing, for a quick test

# 6. Watch AWS resources converge.
make creds-status
```

Expect roughly 15 minutes for the EKS control plane and another 3–5 for the
nodegroup.

## What this costs

Running continuously, in `us-west-2`, with nothing scheduled on it:

| Resource | Approx. monthly |
|---|---|
| EKS control plane | $73 |
| NAT gateway (1) | $33 + data processing |
| 2 × t4g.medium | $24 |
| **Total** | **~$130/month** |

This is not a stack to leave running. The GPU nodepools in the workload plane
will cost considerably more when they scale up.

Note that the ACK controllers run with `deletionPolicy: retain`, so deleting
Units or letting Argo prune them does **not** delete the AWS resources. Teardown
is an explicit operation — see [docs/teardown.md](./docs/teardown.md).

## Building

```bash
make render    # helm charts + handwritten CRs -> configs/
make verify    # fail if configs/ drifts from sources (CI gate)
make bundles   # configs/ -> dist/<component>.tar.gz
make push      # dist/ -> ghcr.io/confighub/configs/eks-inference/<component>:latest
```

CI runs exactly these targets — there is no build logic in the workflow file.

The rendered output in `configs/` is committed on purpose: it makes a chart
version bump reviewable as a diff, and it is what the OCI bundles contain.
See [docs/flattening.md](./docs/flattening.md) for what is lost when a Helm chart
is flattened into literal YAML, and how the build guards against it.

## Layout

```
versions.env              pinned chart versions and render inputs
Makefile                  every build entry point
scripts/render.sh         helm template + copy -> configs/
scripts/guard.sh          rejects Helm constructs that do not survive flattening
scripts/bundle.sh         reproducible tarballs + oras push
scripts/aws-creds.sh      AWS credentials into the cluster (existing, or a new IAM user)
iam/                      the IAM policy granted to the dedicated user
src/                      sources: chart values and handwritten ACK resources
configs/                  rendered output (committed; one file per ConfigHub Unit)
```

File names in `configs/` are an interface: bundles are installed with
`cub variant upload --granularity per-file`, so each file becomes one Unit and
renaming a file renames a Unit.

## Docs

- [install.md](./docs/install.md) — installing the bundles, variants, and releases into ConfigHub
- [flattening.md](./docs/flattening.md) — why we render Helm to literal YAML, what breaks, how the guard works
- [aws-credentials.md](./docs/aws-credentials.md) — the credential script, the IAM policy, and the out-of-band Secret
- [dependencies.md](./docs/dependencies.md) — how values cross component boundaries
- [teardown.md](./docs/teardown.md) — deleting the AWS resources, given `deletionPolicy: retain`

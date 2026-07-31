# Installing the components into ConfigHub

## 1. A management cluster

```bash
cub cluster up --name inference-mgmt
```

This creates a kind cluster with Argo CD installed, plus two ConfigHub spaces:
`inference-mgmt` (holding a server-hosted OCI worker and an OCI target) and
`inference-mgmt-argo-apps` (the root "app of apps" Application and every child
Application Unit).

Argo pulls config from ConfigHub's OCI registry — there is no worker pod in the
cluster.

## 2. Install the bundles as component bases

```bash
make install
```

which is, per component:

```bash
cub variant upload \
  --component aws-network --variant base \
  --granularity per-file \
  oci://ghcr.io/confighub/configs/aws-network
```

This creates a Space named `<component>-base` (from the default space pattern
`{{.Labels.Component}}-{{.Labels.Variant}}`), with one Unit per file in the
bundle. The bundle's resolved digest is recorded on the Space as a
`confighub.com/external-source` annotation, so the exact bytes installed are
auditable and the upload can be reproduced.

Bases are not bound to a target and are not deployed. They are the shared
upstream that variants clone from.

## 3. Create deployable variants

```bash
cub variant create dev aws-network-base --target inference-mgmt/<oci-target>
```

This clones the base Space and every Unit into `aws-network-dev`, links each
clone to its upstream so it can later be upgraded, and sets the Space's release
target.

Find the OCI target slug with:

```bash
cub target list --space inference-mgmt
```

## 4. Publish a release and let Argo pull it

```bash
cub release publish aws-network-dev
```

> **This step is not yet verified end to end.** Specifically: how each component
> Space's release is surfaced to Argo as a child Application in
> `inference-mgmt-argo-apps`, and whether the sync-wave annotations carried on
> the resources are sufficient to order the three components relative to one
> another, or whether waves are also needed on the Application Units themselves.
>
> Sync waves order resources *within* an Argo Application. Ordering *between*
> Applications is a separate concern. Until this is confirmed against a live
> `cub cluster up`, treat the ordering between `ack-controllers`, `aws-network`,
> and `eks-cluster` as unenforced.
>
> In practice ACK converges anyway — a resource whose reference cannot resolve
> requeues rather than fails permanently — so the stack should reach the right
> state regardless. It will just be noisy on the way there.

## 5. Credentials

The controllers will crash-loop until they can read AWS credentials. Follow
[aws-credentials.md](./aws-credentials.md).

## 6. Watch it converge

```bash
kubectl -n ack-system get pods
kubectl -n aws-inference get vpc,subnet,natgateway,routetable,securitygroup
kubectl -n aws-inference get cluster,nodegroup,addon
```

ACK reports status through conditions. The two that matter:

```bash
kubectl -n aws-inference describe vpc inference-demo-vpc
```

- `ACK.ResourceSynced=True` — the AWS resource matches the spec.
- `ACK.Terminal=True` — the controller has given up; the message says why.
  Usually a missing IAM permission or an invalid field combination.

## A note on Argo health

Argo CD has no health assessment for ACK custom resources, so a sync wave will
advance as soon as the resources are *created*, not when the underlying AWS
resources are ready. To make waves actually gate on AWS convergence, add a
resource customization keyed on `ACK.ResourceSynced`. Without it, ordering
between waves is advisory.

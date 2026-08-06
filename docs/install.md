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
cub eksinf install
```

which is, per component:

```bash
cub variant upload \
  --component aws-network --variant base \
  --granularity per-file \
  oci://ghcr.io/confighub/configs/eks-inference/aws-network
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

## 4. Credentials — before the controllers, not after

```bash
cub eksinf creds use-existing     # or: creds create-user
```

Do this **before** publishing. The controllers read their credentials once at
startup, so a controller that starts without them CrashLoops, and one that starts
with credentials it cannot use burns reconcile attempts against an API that is
refusing it. Neither is harmful — ACK recovers — but both are noise that looks
like a failure, and the fix in each case is a restart the plugin would have to
issue anyway.

Writing the Secret first also means a missing or invalid identity fails here,
against `sts:GetCallerIdentity`, rather than fifteen minutes later as an
`ACK.Recoverable` condition on a VPC.

The command works on a cluster where nothing is deployed yet: it creates the
`ack-system` namespace itself. Since it normally identifies the management
cluster *by* that namespace, on a fresh cluster it falls back to "the only
reachable cub-managed cluster" — and if there is more than one candidate it asks
for `--cluster` rather than guessing.

See [aws-credentials.md](./aws-credentials.md) for choosing a mode, SSO
sessions, and rotation.

## 5. Publish a release and let Argo pull it

```bash
cub release publish aws-network-dev
```

> **Verified.** `cub variant create --target` auto-creates the child Argo CD
> Application in the apps Space and republishes it, so there is no separate
> wiring step. Publishing each component's own release is still required — until
> you do, the Application points at a bundle that does not exist.
>
> What is NOT solved: sync waves order resources *within* one Application. The
> components are separate Applications syncing independently, so there is no
> ordering between them.
>
> In practice ACK converges anyway — a resource whose reference cannot resolve
> requeues rather than fails permanently — so the stack reaches the right state
> regardless. It is just noisy on the way there.

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

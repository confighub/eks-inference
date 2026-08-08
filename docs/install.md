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

`cub eksinf deploy` does this, and the order inside it is not cosmetic: it
creates every variant in the plane, links them to the `platform-profile` and
resolves them, and only then publishes.

That order is forced by the gate. Publishing is refused while any
`confighubplaceholder` remains, and the links are what fill them — so a
create-then-publish loop over one component at a time cannot work. The first
publish would happen while the rest of the plane is still placeholders:

```
HTTP 422: outstanding ApplyGates; triggers re-queued for evaluation
```

Creating a link is also not sufficient on its own. A Link records the
relationship but does not rewrite the downstream Unit — the Unit keeps its
placeholders until it is resolved, which is why `deploy` resolves as part of
linking rather than leaving it to the operator.

```bash
cub release publish aws-network-dev   # what deploy does per component
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

### Enrolling the EKS cluster

`cub eksinf enroll cluster --name … --eks-cluster … --region … --grant-access`

`--grant-access` matters whenever the ACK controllers hold a different AWS
identity from yours, which is exactly what `creds create-user` arranges. EKS
grants bootstrap admin to the principal that CREATED the cluster — the ACK user —
so your own identity has no access entry and the API server rejects you with a
bare 401 that says nothing about access entries. The flag adds a STANDARD entry
with `AmazonEKSClusterAdminPolicy` and waits for it to take effect, which is not
instant. Without it, `enroll` now names the missing principal and prints the two
`aws` commands rather than failing opaquely.

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

## Taking a newer bundle

```bash
cub eksinf install
```

Needs **cub v0.2.14 or newer** — older versions cannot upload into a populated
Space at all and fail with "already exists".

Re-running install is a re-upload, not a second create. Each Unit is 3-way merged
against the last upload, which is what makes it safe to run against a populated
Space:

- **Unit IDs, target bindings and upstream links survive.** Downstream variants
  keep pointing at the same Units, so `cub variant promote` still works.
- **Post-upload changes survive too.** This stack depends on that: `link-profile`
  writes values into Units after upload, and the `set-env-var` setters rewrite
  `AWS_REGION`. Replace-semantics would silently undo exactly the wiring that
  makes `platform-profile` work.
- **An unchanged bundle is a no-op**, so it is safe to run on a schedule.

Bases are upstreams and are never deployed, so this moves nothing that is
running. Promote per variant when you want the change:

```bash
cub variant promote karpenter-dev
cub release publish karpenter-dev
```

### --prune

Off by default. It EMPTIES Units the bundle no longer produces — it never deletes
them, so the Unit record, its ID and its bindings survive with no content, and
the resources they contributed are removed on the next apply.

Default-off because emptying a base Unit propagates to every downstream that
promotes from it. That is right when a resource was genuinely dropped upstream,
and wrong when you are pointing install at a partial bundle by mistake, so it is
a decision rather than a default.

### --recreate

Deletes each base Space and uploads it again, and refuses while any downstream
variant still points at it.

It exists for exactly one case: **changing granularity.** Granularity determines
which Units exist, so changing it cannot preserve links by any mechanism — the
Units the links point at stop existing. Deleting and rebuilding is the honest
expression of that, not a workaround for a missing feature. cub itself refuses a
re-upload at a different granularity than the Space was created with:

```
Failed: Space "karpenter-base" was uploaded with --granularity per-file,
but this upload specifies minimal.
```

For every other case — a chart bump, a bug fix, a new resource — a plain
re-upload is correct and non-destructive.

## Starting completely over

Rarely needed now that a re-upload tracks the bundles. When you do want the org
empty — abandoning a stack rather than updating one:

```bash
# 1. AWS, then this stack's variant Spaces (8 components + the profile).
cub eksinf teardown --yes --delete-config

# 2. The enrolled EKS cluster's wiring. Its Spaces are NOT covered by
#    teardown, which only knows about component variants.
cub eksinf enroll remove --name inference-demo --delete-spaces --yes

# 3. The management cluster, its Spaces, and argobot's.
cub cluster down --name inference-mgmt --delete-config

# 4. Bases, if you want those gone too.
cub space delete <component>-base --recursive
```

Bases can usually stay: they are current as of the last `install`, carry no
Target, and cost nothing.

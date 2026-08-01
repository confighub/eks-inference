# How values cross component boundaries

Three mechanisms operate at three different times. Choosing the wrong one is the
main way this stack goes wrong.

## 0. Where the Unit boundaries go

Before any of that: with `--granularity per-file`, **file boundaries become Unit
boundaries, and Unit boundaries become nodes in ConfigHub's inferred link
graph.** Grouping is therefore a modelling decision, not a filing convenience —
splitting resources across files can invent dependencies that do not exist
between the resources themselves.

`aws-network` was originally seven files. Upload reported:

```
broke reference link gateways -> subnets-public to resolve cycle:
  gateways -> subnets-public -> route-tables
```

There is no cycle among the actual resources — `NATGateway → public-a →
public-rt → igw → vpc` is a clean chain. The cycle existed only because the two
route tables shared a file and the two gateways shared a file. ConfigHub broke
the weakest edge, as documented, leaving the graph quietly wrong.

The rule that came out of it: **a Unit should be the smallest thing you would
independently version, review, or roll back.** Nobody rolls back "just the route
tables", so the VPC and everything in it is one Unit. By contrast
`ack-controllers` keeps CRDs separate from controllers (genuinely different
upgrade lifecycles) and `eks-cluster` keeps `nodegroup` separate from `cluster`
(scaling changes far more often than the control plane version).

Merging costs nothing operationally: `argocd.argoproj.io/sync-wave` is a
per-resource annotation, so ordering survives regardless of grouping, and
bindings address resources by `ResourceType` + `ResourceName` *within* a Unit,
so fine-grained links still work.

## 1. ACK refs — runtime, for AWS IDs

`vpc-0a1b2c3d`, `subnet-…`, and role ARNs do not exist until AWS creates them.
No config-time mechanism can carry them.

ACK handles this itself. A resource references another by **Kubernetes object
name**, and the controller resolves that to the AWS ID once the referenced
resource reports it:

```yaml
spec:
  vpcRef:
    from:
      name: inference-demo-vpc
```

If the referenced resource is not ready, the controller requeues. This is why the
stack converges even with imperfect apply ordering.

Use refs for anything that is an AWS identifier. Never try to route those through
ConfigHub.

## 2. ConfigHub links — config time, for values that must agree

What ConfigHub is for here is the opposite category: values that exist before
anything is applied, and that must be **identical** in several places.

The clearest example is `karpenter.sh/discovery`. It appears in:

- `src/aws-network/network.yaml` — the three private subnets and the cluster
  security group
- and, in the workload plane, the `subnetSelectorTerms` and
  `securityGroupSelectorTerms` of the EC2NodeClass

Two components, one string, no compiler. Get it wrong and Karpenter finds no
subnets and silently never launches a node — there is no error message that
points at the tag.

### Its value is a network identifier, not the cluster name

The usual convention is `karpenter.sh/discovery: <cluster-name>`, which reads
naturally when one cluster owns one VPC. It is wrong here, and the reason is the
same one that keeps `aws-network` and `eks-cluster` as separate components: a
blue/green EKS version upgrade stands up a second cluster in the *same* VPC.
Those subnets belong to the network, and both clusters' Karpenters should
legitimately discover them. Tagged with a cluster name, the second cluster would
match by accident rather than by design.

So the value is `inference-demo-net`, and `eks-cluster` carries no discovery tag
at all.

An earlier version also tagged the IAM node role for discovery. That did nothing:
`EC2NodeClass` takes `spec.role` as a plain role *name* and has no role selector,
so no tag on it is ever read.

This is now owned by the `platform-profile` Unit and propagated by a ConfigHub
link — see below.

### The shape

A `platform-profile` Space holding a single Unit, never applied to any cluster:

```yaml
accountID: "…"
region: us-west-2
availabilityZones: [us-west-2a, us-west-2b, us-west-2c]
clusterName: inference-demo
vpcCIDR: 10.42.0.0/16
```

Every component links from it. Two different things are easy to conflate here:

- **Link inference** *does* work on ACK resources. `cub variant upload` walks
  reference fields generically, so it reads `vpcRef.from.name` and builds the
  edge itself. The `eks-cluster` component came out of upload with a correct
  dependency graph (`nodegroup → cluster → iam-roles`) that nobody wrote down.
- **Automatic value binding** does *not*. ConfigHub knows the built-in case — a
  `Namespace` provides `metadata.name` — but nothing tells it that a `Subnet`
  tag at `spec.tags[2].value` needs a cluster name.

So the graph comes free; the values do not. Cross-component propagation needs
**explicit bindings**:

```bash
cat <<'EOF' | cub link create --space aws-network-dev - \
  aws-network platform-profile \
  --update-type NeedsProvides --auto-update --from-stdin
Bindings:
  - AttributeName: cluster-name
    DataType: string
    ProvidedResource:
      ResourceType: confighub.com/v1/PlatformProfile
      ResourceName: /inference-demo
    ProvidedPath: clusterName
    NeededResource:
      ResourceType: ec2.services.k8s.aws/v1alpha1/Subnet
      ResourceName: aws-inference/inference-demo-private-a
    NeededPath: spec.tags.2.value
    AutoUpdate: true
EOF
```

### What actually works (built, and verified against the live stack)

`cub eksinf link-profile` wires this up. Three links carrying nine paths, and they
propagate **across the plane boundary** — `eks-cluster` is applied by the kind
cluster and `karpenter` by EKS, and a single edit to the profile reaches both.
That is worth stating plainly: Argo sync waves cannot express ordering between the
planes, but a ConfigHub link moves a *value* across it without either cluster
knowing the other exists. Ordering and value propagation are separate problems.

`TransformPaths` turned out to be the right update type, not `NeedsProvides` —
these are explicit "read this path, write that path" bindings rather than
placeholder discovery.

Four things that cost real time to work out:

**`--from-stdin` requires JSON.** The YAML form in the ConfigHub dependency docs
is accepted without complaint and every field in it is silently discarded. The
Link is created with `UpstreamPaths: null`, propagates nothing, and looks fine.

**`~1` escapes a literal `.` in a path segment.** Paths split on `.`, so a
Kubernetes or AWS tag key needs escaping:

```
spec.subnetSelectorTerms.0.tags.karpenter~1sh/discovery   ->  tags["karpenter.sh/discovery"]
```

This is the dangerous one. Quoting and backslash escaping are both *rejected*,
which is fine — but the unescaped form is **accepted and writes to the wrong
place**, creating a nested key beside the real one:

```yaml
tags:
  karpenter.sh/discovery: inference-demo-net   # the real key, now stale
  karpenter:
    sh/discovery: probe-value-123              # where the value actually went
```

A successful write that no one reads, leaving the value you meant to change
untouched. Since almost every Kubernetes label, annotation, and AWS tag key
contains a dot, this is easy to hit.

**One Link per (from-unit, to-unit) pair.** The auto-generated slug derives from
both names, so a second link between the same pair collides. This is the right
model rather than a limitation — a single Link carries many paths, so all of a
Unit's bindings to the profile belong in one place.

**Helm values are reachable after all — through their rendered output.** It is
tempting to conclude that anything a chart templates is beyond ConfigHub, since
Helm resolves values before ConfigHub sees anything. But the *result* lands in
the rendered manifest, and that is what ConfigHub stores.

So the region, which the ACK charts consume as `aws.region`, renders into the
controller Deployment as `AWS_REGION` and is linkable there. The chart values
carry `confighubplaceholder` and a link fills the rendered env var.

**Prefer a setter to a positional path.** `AWS_REGION` is the sixth entry in the
container's `env` list, so a path binding would be
`spec.template.spec.containers.0.env.5.value` — and a chart bump that adds or
reorders an env var would silently redirect the write into a neighbouring
variable. The `set-env-var` function addresses it by NAME instead, via the Link's
`DownstreamSetters`:

```json
"DownstreamSetters": [{
  "Parameters": ["region"],
  "FunctionInvocation": {
    "FunctionName": "set-env-var",
    "WhereResource": "ConfigHub.ResourceType = 'apps/v1/Deployment'",
    "Arguments": [
      {"Value": "controller"},
      {"Value": "AWS_REGION"},
      {"Value": "{{.Params.region}}", "Evaluator": "template"}
    ]
  }
}]
```

Any value whose location is positional wants a setter rather than a path.

What genuinely remains unlinkable is a value a chart consumes without emitting —
one that changes *which* resources are rendered rather than what is in them.

### Derived values

Values that must be *computed* rather than copied — an ARN built from account ID
and region, or a tag value that is the cluster name with a prefix — use the same
`TransformPaths` mechanism with a richer Go template in `Expression`, or
`DownstreamSetters` to invoke a mutating function.

The single parameter surface is also what makes variants cheap: a second stack in
another region is a new `platform-profile` Unit, not an edit in six files.

## 3. Argo sync waves — apply time, for ordering

ConfigHub does not sequence applies. Per the ConfigHub docs: a Release bundles
the Units and the GitOps operator applies them, so ordering is the operator's
concern.

Waves are annotated on the resources at render time (see `set_wave` in
`scripts/render.sh`) and appear in the committed output:

| Wave | Resources |
|---|---|
| -30 | `ack-system` namespace |
| -20 | ACK CRDs |
| -10 | ACK controllers |
| 0–6 | VPC, gateways, route tables, subnets, security groups |
| 8–12 | IAM roles, EKS cluster, nodegroup, addons |

Waves order resources within one Argo Application. Ordering *between*
Applications is a separate, currently unverified question — see
[install.md](./install.md).

Waves are also only as good as Argo's health assessment, and Argo does not know
how to assess an ACK resource. Without a resource customization keyed on
`ACK.ResourceSynced`, a wave advances when resources are *created*, not when the
AWS resources are ready. The refs in mechanism 1 are what actually make this
safe; the waves just reduce noise.

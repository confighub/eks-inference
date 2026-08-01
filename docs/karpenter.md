# Karpenter and the node pools

Karpenter provisions nodes on demand. This stack ships three pools, in increasing
order of cost and decreasing order of availability.

| Pool | Hardware | Capacity types | Consolidation | Weight |
|---|---|---|---|---|
| `general` | c/m/r/t, gen > 4, arm64 + amd64 | spot, on-demand | `WhenEmptyOrUnderutilized`, 1m | 50 |
| `quantized-gpu` | g6 / g6e / g5, one GPU (L4 or A10G, 24GB) | spot, on-demand | `WhenEmptyOrUnderutilized`, 5m | 20 |
| `h200` | p5e / p5en (8 × H200, 141GB) | **reserved**, on-demand | `WhenEmpty`, 10m | 1 |

`general` has the highest weight, so a pod that does not explicitly tolerate the
GPU taint can never land on GPU hardware.

Both GPU pools are tainted `nvidia.com/gpu=true:NoSchedule`. The H200 pool
carries **a second, pool-specific taint** as well — tolerating "any GPU" must not
be enough to land on a machine that scarce; it has to be a deliberate choice in
the pod spec.

## Karpenter spans both planes

This is the clearest example of why components are separated by apply plane.

| Piece | Component | Applied by |
|---|---|---|
| Controller IAM role, Pod Identity association | `karpenter-aws` | kind (via ACK) |
| CRDs, controller, NodePools, EC2NodeClasses | `karpenter` | EKS |

The IAM role must exist **before** the controller starts, or the controller
crash-loops with no credentials. No Argo sync wave can express that: it spans two
clusters and two Argo instances. Deploy the mgmt plane, let it converge, then
deploy the workload plane.

It uses **EKS Pod Identity, not IRSA** — the principal is `pods.eks.amazonaws.com`,
so there is no OIDC provider to create and no cluster-specific issuer to template
into a trust policy. The trust policy needs `sts:TagSession` alongside
`sts:AssumeRole`; omitting it is a well-known and confusing failure.

The association is created against a namespace and ServiceAccount that do not
exist yet. That is the point: the identity is waiting when the controller first
starts on EKS.

## Why the GPU AMI is pinned

The `general` NodeClass uses `alias: al2023@latest`. The `gpu` NodeClass is
pinned to a specific release.

Karpenter's own CRD documentation says `latest` is not recommended for
production: a new EKS AMI release makes existing nodes drift, and Karpenter
recycles them to pick it up. On a CPU pool that is a cheap rolling replacement.
On a GPU pool it swaps the NVIDIA driver under a running model and can break CUDA
compatibility — and on the H200 pool it would destroy a node you may have waited
on a capacity reservation to obtain.

Having both in one file makes the contrast visible.

The pinned value is owned by the `platform-profile` Unit and propagated by a
ConfigHub link, so there is one place to change it per environment. To find a
current value:

```bash
aws ssm get-parameter --region <region> \
  --name /aws/service/eks/optimized-ami/<k8s-version>/amazon-linux-2023/x86_64/nvidia/recommended/image_id
aws ec2 describe-images --image-ids <id> --query 'Images[0].Name'
```

The trailing `-vYYYYMMDD` of the AMI name is the alias version.

## The H200 pool will usually not provision

That is expected, and it is modelled honestly rather than aspirationally.

Default P5 on-demand quota is frequently zero and the capacity is genuinely
scarce, so `capacity-type: reserved` is used — Karpenter's distinct type for
On-Demand Capacity Reservations, matched through the GPU NodeClass's
`capacityReservationSelectorTerms`.

Tag an ODCR or Capacity Block with
`eks-inference.confighub.com/stack=inference-demo` and the pool will use it.
Without one it falls back to on-demand and will most likely fail on quota.

The configuration is correct either way — you can review and promote it without
being able to run it. `cub eksinf status` reports `reservations=N` on each
NodeClass, so you can tell whether it *would* work without launching anything.

## The device plugin is not optional

EKS-optimized AL2023 accelerated AMIs ship the NVIDIA driver and container
toolkit but **not** the Kubernetes device plugin. Without it no node ever
advertises `nvidia.com/gpu`.

The failure mode is misleading: a GPU pod stays `Pending`, and Karpenter — seeing
an unschedulable pod it cannot satisfy by adding a node — correctly declines to
launch one. It looks exactly like "Karpenter is broken" and is nothing of the
sort.

The plugin is its own component (`gpu-runtime`) rather than part of `karpenter`,
because the two are independently useful: Karpenter without it for CPU-only
pools, it without Karpenter on managed nodegroups. It is separate from
`inference-workloads` because it is platform — deleting your models must not stop
GPUs being schedulable.

Its tolerations and node affinity are load-bearing, not boilerplate. The
DaemonSet must run **on** the tainted GPU nodes, including tolerating the
H200-specific taint; get that wrong and the node never advertises its GPUs and
sits idle and expensive. The affinity on the accelerator label keeps it off the
arm64 system nodegroup, where it would crash-loop with no GPU to manage.

`0/0 ready` is the correct state when no GPU node exists.

## No interruption queue

`settings.interruptionQueue` is deliberately empty.

Karpenter's interruption queue is optional. Without it you lose graceful handling
of spot reclaims and scheduled maintenance — nodes go away with less warning —
but provisioning and consolidation work normally.

Enabling it means an SQS queue and EventBridge rules, and therefore ACK
controllers for **both** services added to the mgmt plane. That doubles the
controller count for a demo nicety.

If you turn it on, expect to add `sqs` and `eventbridge` to the ACK controllers
component and the corresponding resources to `karpenter-aws`, since they are AWS
resources and only the mgmt plane can create them.

## The security group trap

`securityGroupSelectorTerms` selects **two** groups, and both are required.

Our own security group carries the discovery tag and the intra-VPC allow. But EKS
also creates a managed *cluster security group* and attaches it to managed
nodegroup nodes — where CoreDNS runs. Its only ingress rule permits traffic from
itself, so a Karpenter node carrying just our group cannot reach any pod on a
managed nodegroup node.

Passing a custom SG in the cluster's `resourcesVPCConfig` **adds** to the managed
cluster SG; it does not replace it.

The symptom is not an obvious network error. The node registers fine — the API
server ENIs do carry our SG. Images pull fine — that is the kubelet on the host.
Then the first pod that needs DNS dies with `Temporary failure in name
resolution`.

## Provisioning times, observed

| Step | Duration |
|---|---|
| NodeClaim created after release published | ~20s |
| Instance launched and node registered | ~60–90s |
| GPU pod running (`nvidia-smi`) | ~2 min total |
| vLLM serving a 7B AWQ model | ~10 min (image pull + model download) |
| Node released after scaling to 0 | 5–7 min (consolidation window plus drain) |

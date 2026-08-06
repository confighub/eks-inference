# Making AWS credentials available to the ACK controllers

The ACK controllers run in your local kind cluster and call the AWS API on your
behalf. In EKS they would use IRSA or EKS Pod Identity and hold no long-lived
credentials. In kind neither is available, so they need a credentials file.

This is the least pleasant part of the stack, and deliberately the only step that
is not config.

## When

**Before deploying `ack-controllers`, not after.** The AWS SDK reads the
credentials file once at process start, so a controller that starts without a
usable Secret CrashLoops until one appears, and one that starts with expired
credentials keeps its stale copy even after the Secret is corrected. Writing the
Secret first makes both cases impossible, and makes a bad identity fail against
`sts:GetCallerIdentity` in a second rather than as an `ACK.Recoverable`
condition on a VPC some minutes later.

The write-modes work on a cluster where nothing is deployed yet — they create
the `ack-system` namespace themselves. The one wrinkle: these commands normally
find the management cluster *by* that namespace, so on a fresh cluster they fall
back to "the only reachable cub-managed cluster". With more than one candidate
that is a genuine ambiguity, and they ask for `--cluster <name>` rather than
pick one.

## Use the plugin

```bash
cub eksinf creds status          # what is in the cluster, and how long it lasts
cub eksinf creds use-existing    # reuse your current AWS identity (SSO or key)
cub eksinf creds refresh         # re-issue after `aws sso login`
cub eksinf creds create-user     # create a dedicated IAM user for this stack
cub eksinf creds delete-user     # remove that user, its policy, and its keys
cub eksinf creds show-policy     # print the policy create-user attaches
```

It finds the management cluster on its own by looking for the one with an
`ack-system` namespace. `cub cluster up` writes to
`~/.confighub/clusters/<name>.kubeconfig` rather than adding a context to your
default kubeconfig, so plain `kubectl --context kind-<name>` does not work.

Both write-modes validate the credentials, write the Secret, and restart the
controllers — credentials are read at startup, so a running controller will not
pick up a rotated key by itself.

## Which mode?

| | `use-existing` | `create-user` |
|---|---|---|
| Assumes | you already have a permanent IAM key | you have permission to create one |
| Speed | immediate | ~15s (IAM propagation) |
| Blast radius | everything that key can do | only what `iam/ack-controllers-policy.json` allows |
| Cleanup | nothing to clean up | `delete-user` |

**`use-existing` takes whatever you already have** — a permanent key in
`~/.aws/credentials`, or an SSO / assumed-role session. Pass `--profile NAME` to
choose one.

**`create-user` is for when you do not have a suitable identity**, or want this
stack to hold one that can be revoked on its own. It creates `eks-inference-ack`,
attaches the policy below inline, and issues a long-lived access key. Note that
an SSO session is perfectly good for *running* this — it only needs to last the
few seconds the IAM calls take, and the key it produces does not expire.

Root account credentials are refused. They are permanent, so they would otherwise
pass, but they cannot be scoped and cannot be revoked without disrupting the
whole account.

## Using an SSO session

Supported, and not the second-class option it might look like.

When an SSO session lapses, the controllers get auth errors, ACK marks the
affected resources `ACK.Recoverable`, and reconciliation pauses. **Nothing is
deleted and nothing is left half-destroyed** — the AWS resources that exist keep
existing, and ACK picks up where it left off once credentials return:

```bash
aws sso login
cub eksinf creds refresh
```

`refresh` re-exports your credentials, updates the Secret, and restarts the
controllers. The restart is the part that is easy to miss and not optional: the
AWS SDK reads the credentials file once at startup, so updating the Secret alone
changes the mounted file but not the running process.

`use-existing` reports how long a session has left, and prompts for confirmation
if that is under 30 minutes — the full stack takes roughly 25 (about 5 for the
network, 15 for the EKS control plane, 5 for the nodegroup). `status` reports the
same thing later, reading an expiry annotation recorded on the Secret, so you can
check whether your session will outlast what is still provisioning.

## The policy

`create-user` attaches [`iam/ack-controllers-policy.json`](../iam/ack-controllers-policy.json),
embedded in the plugin binary at build time so it works without a checkout, and
kept as a reviewable JSON file rather than a string literal. Print it with
`cub eksinf creds show-policy`. It grants:

- `ec2:*` and `eks:*` — the network and the cluster
- a specific list of `iam:*` role actions — the cluster and node roles
- `iam:PassRole`, **conditioned on `iam:PassedToService` being `eks.amazonaws.com`
  or `ec2.amazonaws.com`**

`PassRole` is the one people miss. Creating an EKS cluster means handing the
control plane role to the EKS service, which is a `PassRole` call; without it the
Cluster resource fails with an authorization error naming the *role*, not the
missing permission. The condition matters too — unconditioned `iam:PassRole`
alongside `iam:CreateRole` lets this user grant itself anything.

`ec2:*` and `eks:*` are still blunt. Adequate for a demo account; not appropriate
for an account with anything else in it. AWS publishes tighter per-service
policies at
<https://github.com/aws-controllers-k8s/community/tree/main/config/iam>.

## The Secret is never in ConfigHub

`cub variant upload` refuses to upload rendered Secrets, and these bundles are
published to a public registry. The Secret is applied straight to the cluster and
is never a Unit.

It is therefore also invisible to Argo — it will not be pruned and will not be
reported as drift. It is genuinely outside the managed set.

## Confirming it worked

```bash
cub eksinf creds status
```

reports the Secret, whether it holds temporary or long-lived credentials, the
controller pods, and any ACK resources that exist.

Failure modes, in the order you will meet them:

- **`secret "aws-creds" not found`** on the pod — the Secret is missing or in the
  wrong namespace. This is the expected state before you run the command.
- **Controllers `Running` but nothing appears in AWS** — credentials are valid
  but under-permissioned. Look at the resource, not the pod:
  ```bash
  kubectl -n aws-inference describe vpc inference-demo-vpc
  ```
  `ACK.Recoverable` means it will retry; `ACK.Terminal` means it has given up and
  the message says why.
- **Everything reconciles, but into the wrong region** — the region comes from
  the `platform-profile` Unit, not from your AWS profile. If they disagree,
  resources appear in a region your CLI is not looking at.

## Region

The region has exactly one owner: `spec.region` on the `platform-profile` Unit,
with `spec.availabilityZones` alongside it.

The chart values render `AWS_REGION: confighubplaceholder` and the subnets render
`availabilityZone: confighubplaceholder`; `cub eksinf link-profile` fills both.
Change the region in one place and everything follows.

`vet-placeholders` fails while any placeholder remains, so a stack deployed
without running `link-profile` is caught rather than silently reconciling into
nowhere. See [dependencies.md](./dependencies.md).

## Rotating

Re-run either write-mode. The Secret is applied with `--dry-run=client | apply`,
so it rotates in place, and the controllers are restarted for you.

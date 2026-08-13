# Teardown

**Deleting ConfigHub Units DOES delete AWS resources.** Argo prunes, and all
three ACK controllers run with `deletionPolicy: delete`, so removing a Unit and
publishing removes the VPC, NAT gateway, or EKS cluster it describes. Config
decides whether a resource exists.

That is deliberate, and it is the opposite of what this stack shipped before.
Retain used to make an accidental deletion inert — at the cost of forcing every
teardown to annotate each resource, publish, and wait for Argo before deleting
anything. A partial rollout of that annotation left resources ACK could no longer
reach, recoverable only by hand in the AWS console. That happened.

Deleting the kind cluster still leaves everything running, under either policy:
without the controllers, nothing acts on the deletion.

If you want destruction to be hard, see "Hardening this for production" in
README.md — it is a handful of `cub` commands, and needs no plugin flag.

## Ordered teardown

AWS dependencies force the order. Each step must complete before the next.

```bash
# 1. Nodegroup first — nodes hold ENIs in the subnets.
kubectl -n aws-inference delete nodegroup inference-demo-system
aws eks wait nodegroup-deleted --cluster-name inference-demo --nodegroup-name system

# 2. Control plane.
kubectl -n aws-inference delete cluster inference-demo
aws eks wait cluster-deleted --name inference-demo

# 3. NAT gateway, then release its elastic IP.
kubectl -n aws-inference delete natgateway inference-demo-natgw
kubectl -n aws-inference delete elasticipaddress inference-demo-nat-eip

# 4. Subnets, route tables, gateway, security group.
kubectl -n aws-inference delete subnet --all
kubectl -n aws-inference delete routetable --all
kubectl -n aws-inference delete internetgateway inference-demo-igw
kubectl -n aws-inference delete securitygroup inference-demo-cluster-sg

# 5. VPC.
kubectl -n aws-inference delete vpc inference-demo-vpc

# 6. IAM roles.
kubectl -n aws-inference delete role inference-demo-node-role inference-demo-cluster-role
```

Because of `retain`, these `kubectl delete` calls remove the Kubernetes objects
but **leave the AWS resources**. To actually delete the AWS resource, override
the policy on the object first:

```bash
kubectl -n aws-inference annotate vpc inference-demo-vpc \
  services.k8s.aws/deletion-policy=delete --overwrite
kubectl -n aws-inference delete vpc inference-demo-vpc
```

Annotate each resource you genuinely want destroyed, in the order above.

## Verify nothing is left billing

Do not trust the Kubernetes view — check AWS directly:

```bash
aws eks list-clusters
aws ec2 describe-nat-gateways \
  --filter 'Name=tag:eks-inference.confighub.com/stack,Values=inference-demo' \
  --query 'NatGateways[?State!=`deleted`].[NatGatewayId,State]'
aws ec2 describe-addresses \
  --query 'Addresses[?Tags[?Key==`Name` && contains(Value,`inference-demo`)]].[PublicIp,AllocationId]'
aws ec2 describe-vpcs \
  --filters 'Name=tag:eks-inference.confighub.com/stack,Values=inference-demo' \
  --query 'Vpcs[].VpcId'
```

An unattached elastic IP still bills. It is the most common leftover, because
releasing the NAT gateway does not release the address.

## Then the management cluster

```bash
cub cluster down inference-mgmt
```

Do this **last**. Once the kind cluster is gone, the ACK controllers are gone,
and the only remaining way to delete the AWS resources is the AWS console or CLI.

## A blunter option

For a demo account with nothing else in it, deleting by tag is faster and less
error-prone than walking the dependency graph. Every resource this stack creates
carries `eks-inference.confighub.com/stack=inference-demo`. Tools like
`aws-nuke` filtered to that tag will do the whole thing in one pass — but they
are unforgiving, so only point one at an account you are certain is disposable.

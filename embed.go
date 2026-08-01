package main

import _ "embed"

// components.yaml is embedded so the plugin is self-contained: a user who runs
// `cub plugin install confighub/eks-inference` has a binary and nothing else —
// no repo, no configs/ directory. Embedding also pins the component set to the
// plugin version, which is the correct coupling: a plugin release and the OCI
// bundles it installs describe the same stack.
//
// The embed lives HERE, in package main at the repo root, rather than in cmd/.
// go:embed can only reference files at or below its own package directory, and
// components.yaml has to stay at the root because the Makefile and
// scripts/render.sh read it as the source of truth. Rather than duplicate it or
// generate a copy at build time, main reads it and hands it to cmd.
//
//go:embed components.yaml
var componentsYAML []byte

// The IAM policy granted to the dedicated ACK user, embedded for the same reason
// components.yaml is: `creds create-user` must work from an installed binary
// with no repo checkout. Keeping it as a reviewable JSON file at iam/ rather
// than a string literal means it can be inspected and edited like config.
//
//go:embed iam/ack-controllers-policy.json
var ackPolicyJSON []byte

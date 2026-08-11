// Copyright (C) ConfigHub, Inc.
// SPDX-License-Identifier: MIT

package cmd

import "testing"

func TestEksClusterSGs(t *testing.T) {
	// Real `aws ec2 describe-security-groups --output text` shape: tab-separated
	// GroupId and GroupName, one per line. The two groups left in the VPC after
	// a torn-down cluster are the EKS-created one and "default".
	out := "sg-05c347a4067fa75f6\teks-cluster-sg-inference-demo-37233897\n" +
		"sg-0ab1b22526c9bd08d\tdefault\n"

	got := eksClusterSGs(out)
	if len(got) != 1 {
		t.Fatalf("matched %d group(s), want 1: %+v", len(got), got)
	}
	if got[0].id != "sg-05c347a4067fa75f6" {
		t.Errorf("id = %q, want the EKS group's id", got[0].id)
	}

	// "default" must never be selected. AWS refuses to delete it and removes it
	// with the VPC; trying would turn a clean sweep into a recurring error.
	for _, sg := range got {
		if sg.name == "default" {
			t.Error("selected the default security group")
		}
	}

	// A group someone added by hand is not ours to delete, even though it also
	// blocks the VPC. Naming is what distinguishes "EKS made this" from
	// "somebody made this on purpose".
	withCustom := out + "sg-0deadbeef\tmy-bastion-sg\n"
	if n := len(eksClusterSGs(withCustom)); n != 1 {
		t.Errorf("matched %d with a hand-made group present, want 1", n)
	}

	// Empty output is the common case — the VPC is already gone.
	if n := len(eksClusterSGs("")); n != 0 {
		t.Errorf("empty output matched %d, want 0", n)
	}
	if n := len(eksClusterSGs("   \n")); n != 0 {
		t.Errorf("whitespace-only output matched %d, want 0", n)
	}

	// A cluster SG for a DIFFERENT stack in the same account must not match.
	// The sweep is already scoped to this stack's VPC, so this is belt and
	// braces — but the prefix carries the cluster name for exactly this reason.
	other := "sg-0other\teks-cluster-sg-some-other-cluster-123\n"
	if n := len(eksClusterSGs(other)); n != 0 {
		t.Errorf("matched another cluster's SG (%d), want 0", n)
	}
}

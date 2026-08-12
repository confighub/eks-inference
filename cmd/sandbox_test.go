// Copyright (C) ConfigHub, Inc.
// SPDX-License-Identifier: MIT

package cmd

import (
	"slices"
	"strings"
	"testing"
)

func TestSandboxDeleteOrder(t *testing.T) {
	// Against the real components.yaml the plugin ships, not a fixture: the
	// order has to cover whatever components actually exist.
	loadRealComponents(t)

	order := sandboxDeleteOrder("sandbox", "sandbox")
	pos := func(s string) int {
		i := slices.Index(order, s)
		if i < 0 {
			t.Fatalf("%q missing from the delete order: %v", s, order)
		}
		return i
	}

	// Every Space `sandbox up` creates must be accounted for, or `down` reports
	// success while leaving Spaces behind — and the next `up` under the same
	// name then trips over them.
	want := len(AllComponents()) + 4 // components (profile included) + 2 apps + 2 target Spaces
	if len(order) != want {
		t.Errorf("delete order has %d Spaces, want %d: %v", len(order), want, order)
	}
	for _, comp := range AllComponents() {
		pos(variantSpace(comp.Name, "sandbox"))
	}

	// The Target Spaces go last: a Target cannot be deleted while anything
	// references it, and everything else here does.
	mgmt, workload := pos("sandbox-mgmt"), pos("sandbox-workload")
	for i, s := range order {
		if s == "sandbox-mgmt" || s == "sandbox-workload" {
			continue
		}
		if i > mgmt || i > workload {
			t.Errorf("%q is deleted after a Target Space; it holds a reference to one", s)
		}
	}

	// The apps Spaces release TO those Targets, so they must precede them.
	if pos("sandbox-mgmt-argo-apps") > mgmt {
		t.Error("mgmt apps Space deleted after the Target Space it releases to")
	}
	if pos("sandbox-workload-argo-apps") > workload {
		t.Error("workload apps Space deleted after the Target Space it releases to")
	}

	// The profile is linked FROM the component variants, so it must follow them.
	profile := pos(variantSpace(profileComponent, "sandbox"))
	for _, comp := range AllComponents() {
		if comp.Name == profileComponent {
			continue
		}
		if pos(variantSpace(comp.Name, "sandbox")) > profile {
			t.Errorf("%s is deleted after the profile it links to", comp.Name)
		}
	}

	// The variant is a parameter, not baked in: a sandbox named with a custom
	// --variant must not address the default one and delete a real stack's Spaces.
	custom := sandboxDeleteOrder("demo2", "scratch")
	for _, s := range custom {
		if strings.HasSuffix(s, "-sandbox") || strings.HasSuffix(s, "-dev") {
			t.Errorf("custom variant produced %q, which addresses another variant's Space", s)
		}
	}
}

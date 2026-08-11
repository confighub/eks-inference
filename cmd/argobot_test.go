// Copyright (C) ConfigHub, Inc.
// SPDX-License-Identifier: MIT

package cmd

import (
	"io"
	"strings"
	"testing"
)

func TestArgobotSpace(t *testing.T) {
	// The Space name is not cosmetic: `cub variant create <variant> <base>`
	// derives the Space from the base's Component label plus the variant name,
	// so this has to agree with what the CLI actually produces or every
	// follow-up call (set-env, publish, delete) addresses a Space that is not
	// there. The variant name is the cluster name, matching `cub cluster up`.
	if got, want := argobotSpace("inference-demo"), "argobot-inference-demo"; got != want {
		t.Errorf("argobotSpace = %q, want %q", got, want)
	}
	// The base is shared per hub and must NOT be per-cluster.
	if got, want := baseSpace(argobotComponent), "argobot-base"; got != want {
		t.Errorf("baseSpace = %q, want %q", got, want)
	}
	// A cluster named "base" derives the shared base Space itself. That is the
	// collision installArgobot refuses, so assert the premise still holds — if
	// the naming ever changes so they no longer collide, the guard is dead code.
	if argobotSpace(argobotBaseVariant) != baseSpace(argobotComponent) {
		t.Errorf("premise broken: argobotSpace(%q) = %q no longer collides with %q; "+
			"the guard in installArgobot is now unreachable",
			argobotBaseVariant, argobotSpace(argobotBaseVariant), baseSpace(argobotComponent))
	}
}

func TestInstallArgobotRefusesBaseName(t *testing.T) {
	// The guard must fire before any cub call: reaching `variant upload` with
	// this name re-uploads the shared base and binds it to one cluster. A nil
	// runner is deliberate — if the guard regresses, this panics rather than
	// quietly passing on a mocked call that never happened.
	var r *runner
	err := r.installArgobot("", &enrollOpts{name: argobotBaseVariant}, io.Discard)
	if err == nil {
		t.Fatal("installArgobot accepted a cluster named \"base\"")
	}
	if !strings.Contains(err.Error(), baseSpace(argobotComponent)) {
		t.Errorf("error %q does not name the Space it would collide with", err)
	}

	// --no-argobot short-circuits first, so the guard must not block an opt-out.
	if err := r.installArgobot("", &enrollOpts{name: argobotBaseVariant, noArgobot: true}, io.Discard); err != nil {
		t.Errorf("--no-argobot should skip before the guard, got %v", err)
	}
}

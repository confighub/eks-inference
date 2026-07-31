package cmd

import (
	"os"
	"testing"
)

// The plugin embeds components.yaml at build time, so a malformed or renamed
// file breaks the binary at startup rather than at compile time. This test reads
// the real file from the repo root — the same bytes main embeds — so a change
// that would break the shipped plugin fails in CI instead.
func loadRealComponents(t *testing.T) {
	t.Helper()
	data, err := os.ReadFile("../components.yaml")
	if err != nil {
		t.Fatalf("reading components.yaml: %v", err)
	}
	if err := LoadComponents(data); err != nil {
		t.Fatalf("LoadComponents on the real components.yaml: %v", err)
	}
}

func TestRealComponentsParse(t *testing.T) {
	loadRealComponents(t)
	if len(AllComponents()) == 0 {
		t.Fatal("no components parsed")
	}
}

// Every component must declare a plane this tool knows how to act on. An
// unrecognised plane would otherwise silently drop a component from deploy.
func TestEveryComponentHasAKnownPlane(t *testing.T) {
	loadRealComponents(t)
	for _, c := range AllComponents() {
		if _, err := ParsePlane(string(c.Plane)); err != nil {
			t.Errorf("component %q: %v", c.Name, err)
		}
		if c.Name == "" {
			t.Error("component with empty name")
		}
	}
}

// Order must be unique within a plane, otherwise deployment order is decided by
// map iteration rather than by the manifest.
func TestOrderIsUnambiguousWithinAPlane(t *testing.T) {
	loadRealComponents(t)
	for _, p := range []Plane{PlaneHub, PlaneMgmt, PlaneWorkload} {
		seen := map[int]string{}
		for _, c := range ComponentsInPlane(p) {
			if prev, dup := seen[c.Order]; dup {
				t.Errorf("plane %s: %q and %q share order %d", p, prev, c.Name, c.Order)
			}
			seen[c.Order] = c.Name
		}
	}
}

// The two planes that get deployed must both be non-empty; a typo in the plane
// name would otherwise produce a deploy that silently does nothing.
func TestDeployablePlanesAreNonEmpty(t *testing.T) {
	loadRealComponents(t)
	for _, p := range []Plane{PlaneMgmt, PlaneWorkload} {
		if len(ComponentsInPlane(p)) == 0 {
			t.Errorf("plane %s has no components", p)
		}
	}
}

func TestParsePlaneRejectsUnknown(t *testing.T) {
	if _, err := ParsePlane("nonsense"); err == nil {
		t.Fatal("expected an error for an unknown plane")
	}
}

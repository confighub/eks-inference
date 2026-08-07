package cmd

import "testing"

func TestUploadSummary(t *testing.T) {
	cases := map[string]string{
		"Pulled oci://x (sha256:abc)\nAlready up to date: karpenter-base matches the input\n": unchangedSummary,
		"== Space upl2-base ==\nRe-uploaded upl2-base: 1 created, 1 updated, 0 emptied\n":     "1 created, 1 updated, 0 emptied",
		"Successfully created unit nodepools (abc)\n":                                         "",
		"": "",
		// A wording change upstream must degrade to "", not to a wrong summary.
		"Reuploaded upl2-base -- 1 changed\n": "",
	}
	for in, want := range cases {
		if got := uploadSummary(in); got != want {
			t.Errorf("uploadSummary(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRevertCommand(t *testing.T) {
	out := `Re-uploaded upl2-base: 0 created, 1 updated, 0 emptied
Revert with: cub unit update --patch --space upl2-base --restore Before:ChangeSet:reconcile-1 --where "Slug IN ('nodepools')"
`
	want := `cub unit update --patch --space upl2-base --restore Before:ChangeSet:reconcile-1 --where "Slug IN ('nodepools')"`
	if got := revertCommand(out); got != want {
		t.Errorf("revertCommand = %q, want %q", got, want)
	}
	if got := revertCommand("nothing here"); got != "" {
		t.Errorf("revertCommand with no revert line = %q, want empty", got)
	}
}

func TestParseCubVersion(t *testing.T) {
	real := "Client Version:\n  Version:    v0.2.14\n  Commit:     4784110\n  Build Date: 2026-08-07T22:45:25Z\n"
	if got := parseCubVersion(real); len(got) != 3 || got[0] != 0 || got[1] != 2 || got[2] != 14 {
		t.Errorf("parseCubVersion(real) = %v, want [0 2 14]", got)
	}
	// A dev build must parse as nil so the check lets it through rather than
	// blocking the people testing an unreleased cub.
	dev := "Client Version:\n  Version:    dev\n  Commit:     unknown\n"
	if got := parseCubVersion(dev); got != nil {
		t.Errorf("parseCubVersion(dev) = %v, want nil", got)
	}
	if got := parseCubVersion("no version line here"); got != nil {
		t.Errorf("parseCubVersion(junk) = %v, want nil", got)
	}
	// Pre-release suffixes must not defeat the comparison.
	if got := parseCubVersion("  Version: v1.2.3-rc1\n"); len(got) != 3 || got[2] != 3 {
		t.Errorf("parseCubVersion(rc) = %v, want [1 2 3]", got)
	}
}

func TestCompareVersions(t *testing.T) {
	min := mustParseVersion(minCubVersion)
	older := [][]int{{0, 2, 13}, {0, 1, 99}, {0, 0, 0}}
	newer := [][]int{{0, 2, 14}, {0, 2, 15}, {0, 3, 0}, {1, 0, 0}}
	for _, v := range older {
		if compareVersions(v, min) >= 0 {
			t.Errorf("%v should be older than %s", v, minCubVersion)
		}
	}
	for _, v := range newer {
		if compareVersions(v, min) < 0 {
			t.Errorf("%v should be at least %s", v, minCubVersion)
		}
	}
}

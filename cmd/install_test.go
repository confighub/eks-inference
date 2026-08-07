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

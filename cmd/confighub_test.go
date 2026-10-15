package cmd

import "testing"

func TestExtractJSONString(t *testing.T) {
	cases := []struct {
		name string
		blob string
		key  string
		want string
	}{
		{"compact", `{"Space":{"SpaceID":"abc"}}`, "SpaceID", "abc"},
		{"pretty-printed", "{\n  \"Space\": {\n    \"SpaceID\": \"abc\"\n  }\n}", "SpaceID", "abc"},
		{"missing", `{"Space":{"Slug":"x"}}`, "SpaceID", ""},
		{"not json", "nope", "SpaceID", ""},
		{"nested in array", `{"Items":[{"A":1},{"SpaceID":"deep"}]}`, "SpaceID", "deep"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractJSONString(tc.blob, tc.key); got != tc.want {
				t.Errorf("extractJSONString(%q) = %q, want %q", tc.key, got, tc.want)
			}
		})
	}
}

func TestExtractJSONAt(t *testing.T) {
	blob := `{"Unit":{"UnitID":"right"},"UpstreamUnit":{"UnitID":"wrong"}}`
	if got := extractJSONAt(blob, "Unit", "UnitID"); got != "right" {
		t.Errorf("extractJSONAt(Unit.UnitID) = %q, want right", got)
	}
	if got := extractJSONAt(blob, "Unit", "Missing"); got != "" {
		t.Errorf("missing path = %q, want \"\"", got)
	}
	if got := extractJSONAt(blob, "Unit"); got != "" {
		t.Errorf("non-string leaf = %q, want \"\"", got)
	}
}

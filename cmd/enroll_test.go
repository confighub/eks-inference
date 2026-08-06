package cmd

import "testing"

func TestExtractJSONString(t *testing.T) {
	// The pretty-printed case is the one that broke: the original scanner
	// looked for the literal `"SpaceID":"` and found nothing when cub emitted
	// a space after the colon, so a Space that existed reported as missing.
	pretty := `{
  "Space": {
    "DisplayName": "inference-demo",
    "SpaceID": "d91bda6b-86f0-4296-9602-80231090edd9",
    "Slug": "inference-demo"
  },
  "TotalBridgeWorkerCount": 1
}`
	compact := `{"Space":{"SpaceID":"d91bda6b-86f0-4296-9602-80231090edd9"}}`

	for name, blob := range map[string]string{"pretty": pretty, "compact": compact} {
		if got := extractJSONString(blob, "SpaceID"); got != "d91bda6b-86f0-4296-9602-80231090edd9" {
			t.Errorf("%s: SpaceID = %q, want the uuid", name, got)
		}
	}

	if got := extractJSONString(pretty, "BridgeWorkerID"); got != "" {
		t.Errorf("absent key = %q, want empty", got)
	}
	if got := extractJSONString("not json at all", "SpaceID"); got != "" {
		t.Errorf("non-JSON = %q, want empty", got)
	}
	// A non-string value must not be coerced into one.
	if got := extractJSONString(`{"SpaceID": 42}`, "SpaceID"); got != "" {
		t.Errorf("numeric value = %q, want empty", got)
	}
	// Nested under an array, as list-shaped responses are.
	nested := `{"Items": [{"BridgeWorker": {"BridgeWorkerID": "w-123"}}]}`
	if got := extractJSONString(nested, "BridgeWorkerID"); got != "w-123" {
		t.Errorf("nested in array = %q, want w-123", got)
	}
}

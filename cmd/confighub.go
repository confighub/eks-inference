package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
)

// spaceExists reports whether a Space is present.
//
// ONLY a recognisable "not found" counts as absence. Any other error — expired
// token, unreachable server, permissions — is returned, because treating an
// outage as "this does not exist" is how a tool cheerfully reports that it will
// create eight things that already exist. An unrecognised error is deliberately
// treated as fatal rather than as absence: guessing wrong in that direction is
// the expensive one.
func (r *runner) spaceExists(slug string) (bool, error) {
	_, err := r.cub("space", "get", slug)
	if err == nil {
		return true, nil
	}
	if isNotFound(err) {
		return false, nil
	}
	return false, err
}

func (r *runner) unitExists(space, slug string) (bool, error) {
	_, err := r.cub("unit", "get", "--space", space, slug)
	if err == nil {
		return true, nil
	}
	if isNotFound(err) {
		return false, nil
	}
	return false, err
}

// isNotFound recognises cub's absence errors. Anything it does not recognise is
// reported as a real failure by the callers above.
func isNotFound(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "not found") ||
		strings.Contains(msg, "does not exist") ||
		strings.Contains(msg, "no such")
}

// publishRelease publishes a Space's release, tolerating the unchanged case.
//
// Publishing a Space whose content has not changed since :latest is currently an
// ERROR rather than a no-op, so a re-run of an already-deployed plane fails at
// this step. That forces callers to match on the error prose, which is what this
// does. See confighubai/confighub#4870 — when that is fixed this can collapse to
// a plain error check.
func (r *runner) publishRelease(space string) (changed bool, err error) {
	out, err := r.cub("release", "publish", space)
	if err == nil {
		return true, nil
	}
	if strings.Contains(err.Error(), "no changes were made") ||
		strings.Contains(out, "no changes were made") {
		return false, nil
	}
	return false, err
}

// resolveLinks re-resolves a Unit's links, pulling current upstream values into
// it. Needed after changing a platform-profile value: AutoUpdate on the Link
// does not appear to propagate on its own.
func (r *runner) resolveLinks(space, unit string) error {
	_, err := r.cub("unit", "update", "--space", space, "--patch", "--resolve", "Link:*", unit)
	return err
}

// unitData returns a Unit's config data.
func (r *runner) unitData(space, unit string) (string, error) {
	return r.cub("unit", "data", "--space", space, unit)
}

// baseSpace and variantSpace name the Spaces for a component, matching the
// default space pattern `{{.Labels.Component}}-{{.Labels.Variant}}` that
// `cub variant upload` and `cub variant create` use.
func baseSpace(component string) string { return component + "-base" }

func variantSpace(component, variant string) string { return component + "-" + variant }

// ensurePlaceholderGate makes a Target refuse Releases that still contain an
// unfilled confighubplaceholder value, matching what `cub cluster up` and `cub
// cluster attach` do for the clusters they wire.
//
// This stack depends on it. The AWS region and the subnet availability zones are
// rendered as placeholders and filled per environment by links from the
// platform-profile Unit. Without the gate, forgetting `eksinf link-profile`
// publishes cleanly and the ACK controllers come up with a literal region of
// "confighubplaceholder", reconciling into nowhere while reporting healthy.
//
// The Target selects every Trigger in its Space, so additional gates are just
// more Triggers there plus `cub target update --refresh-triggers`.
func (r *runner) ensurePlaceholderGate(space, target string, w io.Writer) error {
	if _, err := r.cub("trigger", "create", "no-placeholders",
		"Mutation", "Kubernetes/YAML", "vet-placeholders",
		"--space", space, "--allow-exists",
		"--description", "Blocks publishing a Release that still contains an unfilled confighubplaceholder value."); err != nil {
		return fmt.Errorf("creating the placeholder Trigger: %w", err)
	}
	out, err := r.cub("space", "get", space, "-o", "json")
	if err != nil {
		return fmt.Errorf("reading Space %s: %w", space, err)
	}
	spaceID := extractJSONAt(out, "Space", "SpaceID")
	if spaceID == "" {
		return fmt.Errorf("could not read SpaceID for %s", space)
	}
	if _, err := r.cub("target", "update", "--space", space, target,
		"--where-trigger", "SpaceID = '"+spaceID+"'"); err != nil {
		return fmt.Errorf("attaching the gate to the target: %w", err)
	}
	fmt.Fprintln(w, "  placeholder gate: on (Releases with unfilled placeholders are blocked)")
	return nil
}

// extractJSONString pulls a string field out of cub's nested JSON without
// modelling the whole entity, which varies by command.
//
// This parses rather than scans. The previous version searched for the literal
// `"key":"`, which silently found nothing the moment cub pretty-printed its
// output with a space after the colon — reporting "could not read SpaceID" for
// a Space that existed and whose JSON contained the field.
func extractJSONString(blob, key string) string {
	var doc any
	if err := json.Unmarshal([]byte(blob), &doc); err != nil {
		return ""
	}
	return findJSONString(doc, key)
}

// extractJSONAt pulls a string from an explicit path, which is what callers
// should use whenever they know the shape.
//
// Searching by key alone is not safe on cub's output: `cub unit get` returns
// six sibling entities and TWO of them carry a UnitID — Unit.UnitID and
// UpstreamUnit.UnitID. A search returns whichever the traversal reaches first,
// so it happened to be right only because "Unit" sorts before "UpstreamUnit".
// Naming the path removes the coincidence.
func extractJSONAt(blob string, path ...string) string {
	var doc any
	if err := json.Unmarshal([]byte(blob), &doc); err != nil {
		return ""
	}
	for _, key := range path {
		m, ok := doc.(map[string]any)
		if !ok {
			return ""
		}
		doc, ok = m[key]
		if !ok {
			return ""
		}
	}
	s, _ := doc.(string)
	return s
}

// findJSONString walks the decoded document for the first string value under
// key, at any depth. Prefer extractJSONAt where the shape is known; this
// remains for the cases where it genuinely is not.
func findJSONString(node any, key string) string {
	switch v := node.(type) {
	case map[string]any:
		if s, ok := v[key].(string); ok {
			return s
		}
		// Deterministic order: map iteration is randomised, and a document with
		// the key at two depths would otherwise return a different answer per
		// run.
		names := make([]string, 0, len(v))
		for k := range v {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, k := range names {
			if s := findJSONString(v[k], key); s != "" {
				return s
			}
		}
	case []any:
		for _, item := range v {
			if s := findJSONString(item, key); s != "" {
				return s
			}
		}
	}
	return ""
}

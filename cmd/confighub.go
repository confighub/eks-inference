package cmd

import (
	"encoding/json"
	"fmt"
	"net/url"
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

type cubContext struct {
	Coordinate struct {
		ServerURL      string `json:"serverURL"`
		OrganizationID string `json:"organizationID"`
		User           string `json:"user"`
	} `json:"coordinate"`
}

func (r *runner) context() (*cubContext, error) {
	out, err := r.cub("context", "get", "-o", "json")
	if err != nil {
		return nil, err
	}
	var c cubContext
	if err := json.Unmarshal([]byte(out), &c); err != nil {
		return nil, fmt.Errorf("parsing cub context: %w", err)
	}
	return &c, nil
}

// ociRegistry derives the ConfigHub OCI registry from the active context.
//
// The registry is a property of the ConfigHub instance, so hardcoding it means
// enrolling against staging silently produces an Argo Application pointing at
// the wrong registry — an Application that syncs nothing and reports Healthy.
//
// The hosted convention is <scheme>://<host> -> oci.<host>:443. That does not
// generalise to a local or self-hosted server, so anything other than https is
// refused rather than guessed at; pass --oci-registry for those.
func (r *runner) ociRegistry(override string) (string, error) {
	if override != "" {
		return override, nil
	}
	c, err := r.context()
	if err != nil {
		return "", fmt.Errorf("reading cub context: %w", err)
	}
	u, err := url.Parse(c.Coordinate.ServerURL)
	if err != nil {
		return "", fmt.Errorf("parsing server URL %q: %w", c.Coordinate.ServerURL, err)
	}
	if u.Scheme != "https" {
		return "", fmt.Errorf(
			"cannot derive the OCI registry from a non-https server (%s); pass --oci-registry",
			c.Coordinate.ServerURL)
	}
	return "oci." + u.Hostname() + ":443", nil
}

// baseSpace and variantSpace name the Spaces for a component, matching the
// default space pattern `{{.Labels.Component}}-{{.Labels.Variant}}` that
// `cub variant upload` and `cub variant create` use.
func baseSpace(component string) string { return component + "-base" }

func variantSpace(component, variant string) string { return component + "-" + variant }

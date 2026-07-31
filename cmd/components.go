package cmd

import (
	"fmt"
	"sort"

	"gopkg.in/yaml.v3"
)

// Plane is which cluster applies a component. See components.yaml.
type Plane string

const (
	// PlaneHub components live only in ConfigHub. Never bound to a Target,
	// never released, never applied. They hold values other components link to.
	PlaneHub Plane = "hub"
	// PlaneMgmt components are applied by the local kind cluster and create AWS
	// infrastructure through ACK.
	PlaneMgmt Plane = "mgmt"
	// PlaneWorkload components are applied by the EKS cluster once enrolled.
	PlaneWorkload Plane = "workload"
)

type Component struct {
	Name        string `yaml:"name"`
	Plane       Plane  `yaml:"plane"`
	Order       int    `yaml:"order"`
	Render      string `yaml:"render"`
	Description string `yaml:"description"`
}

type manifest struct {
	Components []Component `yaml:"components"`
}

var components []Component

// LoadComponents parses the embedded components.yaml. main owns the embed (see
// embed.go for why) and calls this before Execute.
func LoadComponents(data []byte) error {
	var m manifest
	if err := yaml.Unmarshal(data, &m); err != nil {
		return fmt.Errorf("parsing embedded components.yaml: %w", err)
	}
	if len(m.Components) == 0 {
		return fmt.Errorf("embedded components.yaml declares no components")
	}
	components = m.Components
	return nil
}

// AllComponents returns every component, in plane then order.
func AllComponents() []Component {
	out := append([]Component(nil), components...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Plane != out[j].Plane {
			return out[i].Plane < out[j].Plane
		}
		return out[i].Order < out[j].Order
	})
	return out
}

// ComponentsInPlane returns the components of one plane in deployment order.
//
// Ordering WITHIN a plane is meaningful and honoured here. Ordering BETWEEN
// planes is not expressible in config at all — the mgmt plane must converge
// before the workload plane is deployed, and no Argo sync wave can span two
// clusters. Callers deploy one plane at a time, deliberately.
func ComponentsInPlane(p Plane) []Component {
	var out []Component
	for _, c := range components {
		if c.Plane == p {
			out = append(out, c)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Order < out[j].Order })
	return out
}

// ParsePlane validates a user-supplied plane name.
func ParsePlane(s string) (Plane, error) {
	switch Plane(s) {
	case PlaneHub, PlaneMgmt, PlaneWorkload:
		return Plane(s), nil
	default:
		return "", fmt.Errorf("unknown plane %q (want hub, mgmt, or workload)", s)
	}
}

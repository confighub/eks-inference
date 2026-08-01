package cmd

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

// A binding: one value read from the platform-profile, written to one path in
// one component's Unit.
//
// THE ~1 ESCAPE IS THE WHOLE TRICK. ConfigHub path expressions split on ".", and
// "~1" escapes a literal "." inside a segment. Kubernetes and AWS tag keys are
// full of dots, so nearly every tag binding needs it:
//
//	tags.karpenter~1sh/discovery  ->  tags["karpenter.sh/discovery"]
//
// Getting it wrong is silent and destructive: an unescaped
// "tags.karpenter.sh/discovery" is parsed as three segments and creates a nested
// key beside the real one, so the write succeeds into a location nothing reads
// while the value you meant to change stays stale. Quoting and backslash
// escaping are both rejected outright, which is at least loud. See
// confighubai/confighub#4903.
type binding struct {
	Component string // component whose Unit receives the value
	Unit      string // Unit within that component
	Field     string // template parameter name; also the profile field unless Upstream is set
	Type      string // downstream resource type
	Resource  string // downstream resource name ("namespace/name", or "/name" if cluster-scoped)
	Path      string // downstream path, with ~1 escaping where needed
	Upstream  string // profile path to read; defaults to "spec.<Field>"
}

// upstreamPathFor resolves the profile path this binding reads. Most bindings
// read spec.<Field>; the availability zones read indexed elements of a list, so
// the parameter name and the path differ.
func (b binding) upstreamPathFor() string {
	if b.Upstream != "" {
		return b.Upstream
	}
	return "spec." + b.Field
}

// The bindings this stack needs. Only paths whose map keys can be escaped, or
// that have no dotted keys at all, are expressible. Values consumed by a Helm
// chart's values.yaml — karpenter's settings.clusterName — cannot be linked:
// they are resolved at render time, before ConfigHub ever sees them.
var profileBindings = []binding{
	{"karpenter", "nodeclasses", "networkName", "karpenter.k8s.aws/v1/EC2NodeClass", "/general",
		"spec.subnetSelectorTerms.0.tags.karpenter~1sh/discovery", ""},
	{"karpenter", "nodeclasses", "networkName", "karpenter.k8s.aws/v1/EC2NodeClass", "/general",
		"spec.securityGroupSelectorTerms.0.tags.karpenter~1sh/discovery", ""},
	{"karpenter", "nodeclasses", "networkName", "karpenter.k8s.aws/v1/EC2NodeClass", "/gpu",
		"spec.subnetSelectorTerms.0.tags.karpenter~1sh/discovery", ""},
	{"karpenter", "nodeclasses", "networkName", "karpenter.k8s.aws/v1/EC2NodeClass", "/gpu",
		"spec.securityGroupSelectorTerms.0.tags.karpenter~1sh/discovery", ""},
	{"karpenter", "nodeclasses", "nodeRoleName", "karpenter.k8s.aws/v1/EC2NodeClass", "/general", "spec.role", ""},
	{"karpenter", "nodeclasses", "nodeRoleName", "karpenter.k8s.aws/v1/EC2NodeClass", "/gpu", "spec.role", ""},
	{"karpenter", "nodeclasses", "gpuAMIAlias", "karpenter.k8s.aws/v1/EC2NodeClass", "/gpu",
		"spec.amiSelectorTerms.0.alias", ""},
	// Cross-plane: these Units are applied by the kind cluster while the ones
	// above are applied by EKS. A ConfigHub link spans that boundary without
	// either cluster knowing the other exists.
	{"eks-cluster", "cluster", "clusterName", "eks.services.k8s.aws/v1alpha1/Cluster",
		"aws-inference/inference-demo", "spec.name", ""},
	{"eks-cluster", "nodegroup", "nodeRoleName", "eks.services.k8s.aws/v1alpha1/Nodegroup",
		"aws-inference/inference-demo-system", "spec.nodeRoleRef.from.name", ""},

	// Availability zones. Six subnets draw from three profile values, so the
	// parameter name and the upstream path differ here — hence Upstream.
	//
	// These matter as much as the region: an AZ string encodes the region, so
	// leaving them literal while the region is linked gives a half-parameterised
	// stack where the controllers move and the subnets do not.
	{"aws-network", "network", "az0", subnetType, "aws-inference/inference-demo-private-a",
		"spec.availabilityZone", "spec.availabilityZones.0"},
	{"aws-network", "network", "az1", subnetType, "aws-inference/inference-demo-private-b",
		"spec.availabilityZone", "spec.availabilityZones.1"},
	{"aws-network", "network", "az2", subnetType, "aws-inference/inference-demo-private-c",
		"spec.availabilityZone", "spec.availabilityZones.2"},
	{"aws-network", "network", "az0", subnetType, "aws-inference/inference-demo-public-a",
		"spec.availabilityZone", "spec.availabilityZones.0"},
	{"aws-network", "network", "az1", subnetType, "aws-inference/inference-demo-public-b",
		"spec.availabilityZone", "spec.availabilityZones.1"},
	{"aws-network", "network", "az2", subnetType, "aws-inference/inference-demo-public-c",
		"spec.availabilityZone", "spec.availabilityZones.2"},
}

const subnetType = "ec2.services.k8s.aws/v1alpha1/Subnet"

const (
	profileComponent = "platform-profile"
	profileUnit      = "profile"
	profileKind      = "eks-inference.confighub.com/v1/PlatformProfile"
	profileResource  = "/inference-demo"
)

// linkPayload is the JSON body for `cub link create --from-stdin`.
//
// JSON, NOT YAML. --from-stdin accepts YAML without complaint and discards every
// field in it, producing a Link with null paths that propagates nothing while
// reporting success. See confighubai/confighub#4904.
type linkPayload struct {
	UpstreamPaths     []upstreamPath   `json:"UpstreamPaths"`
	DownstreamPaths   []downstreamPath `json:"DownstreamPaths,omitempty"`
	DownstreamSetters []setter         `json:"DownstreamSetters,omitempty"`
}

// A setter invokes a mutating function on the downstream Unit instead of writing
// a raw path. Use it wherever a path would be positional.
//
// AWS_REGION is the case in point: it is the sixth entry in the ACK controller's
// env list, so a path binding would be
// spec.template.spec.containers.0.env.5.value — and a chart bump that adds or
// reorders an env var would silently redirect the write into a neighbouring
// variable. set-env-var addresses it BY NAME, so the binding survives.
type setter struct {
	Parameters         []string           `json:"Parameters"`
	FunctionInvocation functionInvocation `json:"FunctionInvocation"`
}

type functionInvocation struct {
	FunctionName  string     `json:"FunctionName"`
	WhereResource string     `json:"WhereResource,omitempty"`
	Arguments     []argument `json:"Arguments"`
}

type argument struct {
	Value     string `json:"Value"`
	Evaluator string `json:"Evaluator,omitempty"`
}

// envBinding fills a container env var on a Unit from a profile field.
type envBinding struct {
	Component string
	Unit      string
	Field     string // profile spec field to read
	Container string // container name, or "*" for any
	EnvVar    string // env var to set
}

// The ACK controllers all render AWS_REGION as a literal placeholder; the region
// belongs to the environment, not the chart.
var profileEnvBindings = []envBinding{
	{"ack-controllers", "ec2-controller", "region", "controller", "AWS_REGION"},
	{"ack-controllers", "iam-controller", "region", "controller", "AWS_REGION"},
	{"ack-controllers", "eks-controller", "region", "controller", "AWS_REGION"},
}

type resourceRef struct {
	ResourceType string `json:"ResourceType"`
	ResourceName string `json:"ResourceName"`
}

type upstreamPath struct {
	Name     string      `json:"Name"`
	Path     string      `json:"Path"`
	Resource resourceRef `json:"Resource"`
}

type downstreamPath struct {
	Path       string      `json:"Path"`
	Resource   resourceRef `json:"Resource"`
	Expression string      `json:"Expression"`
	Evaluator  string      `json:"Evaluator"`
	Parameters []string    `json:"Parameters"`
	DataType   string      `json:"DataType"`
}

func newLinkProfileCmd() *cobra.Command {
	var list, unlink bool

	c := &cobra.Command{
		Use:   "link-profile",
		Short: "Link component Units to the platform-profile so shared values have one owner",
		Long: `Wire component Units to the platform-profile Unit, so values that must agree
across components come from one place instead of being duplicated literals.

The profile is never applied to a cluster: it has no CRD and no controller, and
its Space has no Target. It exists only as the upstream that these links read.

Links span the plane boundary. Argo sync waves cannot express ordering between
the mgmt and workload planes, but a link propagates a VALUE across it without
either cluster knowing the other exists. Ordering and value propagation are
separate problems and only one of them needs the planes to see each other.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := requireTools("cub"); err != nil {
				return err
			}
			r := newRunner()
			if err := r.requireConfigHubAuth(); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			profileSpace := variantSpace(profileComponent, flagVariant)

			switch {
			case list:
				return r.listProfileLinks(out, profileSpace)
			case unlink:
				return r.unlinkProfile(out, profileSpace)
			}

			hasProfile, err := r.spaceExists(profileSpace)
			if err != nil {
				return fmt.Errorf("checking Space %s: %w", profileSpace, err)
			}
			if !hasProfile {
				return fmt.Errorf(
					"no Space %q — create it with:  cub variant create %s %s-base",
					profileSpace, flagVariant, profileComponent)
			}
			return r.linkProfile(out, profileSpace)
		},
	}

	c.Flags().BoolVar(&list, "list", false, "show the bindings and any existing links")
	c.Flags().BoolVar(&unlink, "unlink", false, "remove links to the platform-profile")
	return c
}

// groupKey is component+unit. ConfigHub permits ONE Link per (from-unit,
// to-unit) pair — the auto-generated slug derives from both names, so a second
// link between the same pair collides. That is the right model rather than a
// limitation: one Link carries many paths, so all of a Unit's bindings belong
// together.
type groupKey struct{ component, unit string }

func groupBindings() ([]groupKey, map[groupKey][]binding) {
	groups := map[groupKey][]binding{}
	for _, b := range profileBindings {
		k := groupKey{b.Component, b.Unit}
		groups[k] = append(groups[k], b)
	}
	// Env bindings share the (component, unit) grouping — one Link per pair
	// carries both paths and setters.
	for _, e := range profileEnvBindings {
		k := groupKey{e.Component, e.Unit}
		if _, ok := groups[k]; !ok {
			groups[k] = nil
		}
	}
	keys := make([]groupKey, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].component != keys[j].component {
			return keys[i].component < keys[j].component
		}
		return keys[i].unit < keys[j].unit
	})
	return keys, groups
}

func envBindingsFor(k groupKey) []envBinding {
	var out []envBinding
	for _, e := range profileEnvBindings {
		if e.Component == k.component && e.Unit == k.unit {
			out = append(out, e)
		}
	}
	return out
}

func payloadFor(bs []binding, envs []envBinding) ([]byte, error) {
	var p linkPayload
	seen := map[string]bool{}
	for _, b := range bs {
		// Deduplicate upstream reads: several downstream paths often want the
		// same profile field, and it only needs to be read once.
		if !seen[b.Field] {
			seen[b.Field] = true
			p.UpstreamPaths = append(p.UpstreamPaths, upstreamPath{
				Name:     b.Field,
				Path:     b.upstreamPathFor(),
				Resource: resourceRef{profileKind, profileResource},
			})
		}
		p.DownstreamPaths = append(p.DownstreamPaths, downstreamPath{
			Path:       b.Path,
			Resource:   resourceRef{b.Type, b.Resource},
			Expression: "{{.Params." + b.Field + "}}",
			Evaluator:  "template",
			Parameters: []string{b.Field},
			DataType:   "string",
		})
	}

	for _, e := range envs {
		if !seen[e.Field] {
			seen[e.Field] = true
			p.UpstreamPaths = append(p.UpstreamPaths, upstreamPath{
				Name:     e.Field,
				Path:     "spec." + e.Field,
				Resource: resourceRef{profileKind, profileResource},
			})
		}
		p.DownstreamSetters = append(p.DownstreamSetters, setter{
			Parameters: []string{e.Field},
			FunctionInvocation: functionInvocation{
				FunctionName:  "set-env-var",
				WhereResource: "ConfigHub.ResourceType = 'apps/v1/Deployment'",
				Arguments: []argument{
					{Value: e.Container},
					{Value: e.EnvVar},
					{Value: "{{.Params." + e.Field + "}}", Evaluator: "template"},
				},
			},
		})
	}
	return json.Marshal(p)
}

func (r *runner) linkProfile(out interface{ Write([]byte) (int, error) }, profileSpace string) error {
	keys, groups := groupBindings()
	links, paths := 0, 0

	for _, k := range keys {
		space := variantSpace(k.component, flagVariant)
		has, err := r.spaceExists(space)
		if err != nil {
			return fmt.Errorf("checking Space %s: %w", space, err)
		}
		if !has {
			fmt.Fprintf(out, "  SKIP %s (not deployed)\n", space)
			continue
		}
		envs := envBindingsFor(k)
		body, err := payloadFor(groups[k], envs)
		if err != nil {
			return err
		}
		if _, err := r.cubStdin(body, "link", "create", "--space", space, "-", k.unit,
			profileUnit, profileSpace, "--update-type", "TransformPaths",
			"--auto-update", "--from-stdin"); err != nil {
			return fmt.Errorf("linking %s/%s: %w", space, k.unit, err)
		}
		fmt.Fprintf(out, "  %s/%s: %d path(s), %d setter(s)\n", space, k.unit, len(groups[k]), len(envs))
		for _, b := range groups[k] {
			fmt.Fprintf(out, "      %-14s -> %s\n", b.Field, b.Path)
		}
		for _, e := range envs {
			fmt.Fprintf(out, "      %-14s -> set-env-var %s/%s\n", e.Field, e.Container, e.EnvVar)
		}
		links++
		paths += len(groups[k]) + len(envs)
	}

	fmt.Fprintf(out, "\nCreated %d link(s) carrying %d path(s) from %s.\n", links, paths, profileSpace)
	fmt.Fprintln(out, "\nResolve and publish the affected Units:")
	for _, k := range keys {
		fmt.Fprintf(out, "  cub unit update --space %s --patch --resolve 'Link:*' %s\n",
			variantSpace(k.component, flagVariant), k.unit)
	}
	return nil
}

func (r *runner) listProfileLinks(out interface{ Write([]byte) (int, error) }, profileSpace string) error {
	keys, groups := groupBindings()
	for _, k := range keys {
		for _, b := range groups[k] {
			fmt.Fprintf(out, "  %-16s %-18s %-12s %-30s %s\n",
				variantSpace(k.component, flagVariant), k.unit, b.Field, b.Resource, b.Path)
		}
		for _, e := range envBindingsFor(k) {
			fmt.Fprintf(out, "  %-16s %-18s %-12s %-30s set-env-var %s\n",
				variantSpace(k.component, flagVariant), k.unit, e.Field, e.Container, e.EnvVar)
		}
	}
	fmt.Fprintf(out, "\nExisting links to %s:\n", profileSpace)
	found := false
	for _, c := range AllComponents() {
		if c.Plane == PlaneHub {
			continue
		}
		space := variantSpace(c.Name, flagVariant)
		outStr, err := r.cub("link", "list", "--space", space, "--no-headers")
		if err != nil {
			continue
		}
		for _, line := range strings.Split(strings.TrimSpace(outStr), "\n") {
			if strings.Contains(line, profileComponent) {
				fmt.Fprintf(out, "  %s/%s\n", space, strings.Fields(line)[0])
				found = true
			}
		}
	}
	if !found {
		fmt.Fprintln(out, "  none")
	}
	return nil
}

func (r *runner) unlinkProfile(out interface{ Write([]byte) (int, error) }, profileSpace string) error {
	removed := 0
	for _, c := range AllComponents() {
		if c.Plane == PlaneHub {
			continue
		}
		space := variantSpace(c.Name, flagVariant)
		has, err := r.spaceExists(space)
		if err != nil {
			return fmt.Errorf("checking Space %s: %w", space, err)
		}
		if !has {
			continue
		}
		outStr, err := r.cub("link", "list", "--space", space, "--no-headers")
		if err != nil {
			continue
		}
		for _, line := range strings.Split(strings.TrimSpace(outStr), "\n") {
			if !strings.Contains(line, profileComponent) {
				continue
			}
			slug := strings.Fields(line)[0]
			if _, err := r.cub("link", "delete", "--space", space, slug); err == nil {
				fmt.Fprintf(out, "  removed %s/%s\n", space, slug)
				removed++
			}
		}
	}
	if removed == 0 {
		fmt.Fprintln(out, "  no links to remove")
	}
	return nil
}

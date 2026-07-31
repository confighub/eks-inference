// Command eksinf is the admin tool for the eks-inference stack, published as a
// cub CLI plugin.
//
//	cub plugin install confighub/eks-inference
//	cub eksinf status
//
// It wraps the operations a *consumer* of this stack performs — installing the
// component bases from their OCI bundles, deploying a plane, enrolling the EKS
// cluster, managing AWS credentials, and reporting state.
//
// It deliberately does NOT wrap the build-time operations (render, guard,
// bundle). Those need the source tree, helm, and GNU tar, and only ever run in
// this repo or its CI. They stay as make targets.
package main

import (
	"fmt"
	"os"

	"github.com/confighub/sdk/core/plugin"

	"github.com/confighub/eks-inference/cmd"
)

func main() {
	// cub invokes this binary with the hook environment set when installing or
	// upgrading the plugin. HandleHook writes cub-plugin.yaml into the plugin
	// directory and we exit before the command tree runs — which is why there is
	// no cub-plugin.yaml committed anywhere: the binary is self-describing, so
	// the manifest can never drift from the commands actually implemented.
	manifest := plugin.Manifest{
		Name:    "eksinf",
		Version: cmd.Version(),
		Commands: []plugin.Command{{
			Name:    "eksinf",
			Summary: "Administer the eks-inference stack: EKS for inference workloads via ACK and Karpenter",
		}},
	}
	if handled, err := plugin.HandleHook(manifest); handled {
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}

	cmd.Execute(componentsYAML)
}

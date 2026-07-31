package cmd

import (
	"encoding/json"
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

func newComponentsCmd() *cobra.Command {
	var asJSON bool
	var planeFlag string

	c := &cobra.Command{
		Use:   "components",
		Short: "List the components of the stack and which plane applies each",
		Long: `List the components this plugin knows about, read from the components.yaml
embedded at build time.

The plane column is the important one. Components are separated by which cluster
applies them before they are separated by anything else:

  hub       lives only in ConfigHub; never bound to a Target, never applied
  mgmt      applied by the local kind cluster; creates AWS infrastructure via ACK
  workload  applied by the EKS cluster once enrolled

Order is meaningful within a plane. Between planes it is not expressible in
config at all — the mgmt plane must converge before the workload plane is
deployed, and no Argo sync wave can span two clusters.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			list := AllComponents()
			if planeFlag != "" {
				p, err := ParsePlane(planeFlag)
				if err != nil {
					return err
				}
				list = ComponentsInPlane(p)
			}

			out := cmd.OutOrStdout()
			if asJSON {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(list)
			}

			w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "PLANE\tORDER\tCOMPONENT\tDESCRIPTION")
			for _, c := range list {
				fmt.Fprintf(w, "%s\t%d\t%s\t%s\n", c.Plane, c.Order, c.Name, c.Description)
			}
			return w.Flush()
		},
	}

	c.Flags().BoolVar(&asJSON, "json", false, "emit JSON")
	c.Flags().StringVar(&planeFlag, "plane", "", "only components in this plane (hub, mgmt, workload)")
	return c
}

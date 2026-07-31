package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
)

// Approximate on-demand hourly rates for the instance types this stack
// provisions. Deliberately a small hand-maintained table rather than a pricing
// API call: the question being answered is "roughly how fast is this burning",
// not "what is the invoice".
var hourlyRate = map[string]float64{
	"t4g.medium":    0.034,
	"c6g.large":     0.072,
	"c7g.large":     0.072,
	"c6g.xlarge":    0.145,
	"c7g.xlarge":    0.145,
	"c6g.2xlarge":   0.290,
	"c7g.2xlarge":   0.290,
	"m6g.large":     0.081,
	"m7g.large":     0.081,
	"g5.xlarge":     1.006,
	"g5.2xlarge":    1.212,
	"g6.xlarge":     0.805,
	"g6.2xlarge":    0.978,
	"g6e.xlarge":    1.861,
	"p5e.48xlarge":  35.000,
	"p5en.48xlarge": 35.000,
}

type instance struct {
	ID       string  `json:"id"`
	Type     string  `json:"type"`
	Pool     string  `json:"pool"`
	Rate     float64 `json:"hourlyRate"`
	RateKnwn bool    `json:"hourlyRateKnown"`
}

func newStatusCmd() *cobra.Command {
	var asJSON bool
	var region string

	c := &cobra.Command{
		Use:   "status",
		Short: "Report what is running across both planes, and what it costs",
		Long: `Report the state of the stack.

Two rules this command follows, both learned by getting them wrong:

  1. COST COMES FROM EC2, NOT KUBERNETES. A Node or NodeClaim object can outlive
     its instance by minutes, and an unreachable cluster reports zero nodes, so
     kubectl is wrong in both directions. Only describe-instances answers
     "am I being billed".

  2. UNREACHABLE IS NOT EMPTY. Every cluster query is gated on an explicit
     connectivity check. A cluster that cannot be reached is reported as such,
     never as a healthy-looking zero.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := requireTools("aws", "kubectl"); err != nil {
				return err
			}
			r := newRunner()
			out := cmd.OutOrStdout()

			rep := statusReport{Region: region}
			rep.Billing = r.billing(region, &rep)
			mgmt, workload := r.discoverClusters()
			rep.MgmtCluster, rep.WorkloadCluster = mgmt, workload
			rep.Karpenter = r.karpenterState(workload, &rep)

			if asJSON {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(rep)
			}
			rep.write(out)
			return nil
		},
	}

	c.Flags().BoolVar(&asJSON, "json", false, "emit JSON")
	c.Flags().StringVar(&region, "region", "us-west-2", "AWS region to inspect")
	return c
}

type statusReport struct {
	Region string `json:"region"`

	Billing      []instance `json:"instances"`
	BillingError string     `json:"billingError,omitempty"`

	MgmtCluster     string `json:"mgmtCluster,omitempty"`
	WorkloadCluster string `json:"workloadCluster,omitempty"`

	Karpenter      []nodeClass `json:"nodeClasses,omitempty"`
	KarpenterError string      `json:"karpenterError,omitempty"`
}

type nodeClass struct {
	Name           string `json:"name"`
	Ready          string `json:"ready"`
	Subnets        int    `json:"subnets"`
	SecurityGroups int    `json:"securityGroups"`
	AMIs           int    `json:"amis"`
	Reservations   int    `json:"capacityReservations"`
}

// billing reads running instances from EC2. A failure here is recorded as an
// explicit error, never as an empty list — "cost unknown" and "cost zero" are
// very different answers and must not look alike.
func (r *runner) billing(region string, rep *statusReport) []instance {
	outStr, err := r.aws("ec2", "describe-instances",
		"--region", region,
		"--filters", "Name=instance-state-name,Values=running,pending",
		"--query", "Reservations[].Instances[].[InstanceId,InstanceType,Tags[?Key=='karpenter.sh/nodepool'].Value|[0]]",
		"--output", "text")
	if err != nil {
		rep.BillingError = err.Error()
		return nil
	}
	var list []instance
	for _, line := range strings.Split(strings.TrimSpace(outStr), "\n") {
		if line == "" {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		in := instance{ID: f[0], Type: f[1], Pool: "system"}
		if len(f) > 2 && f[2] != "None" {
			in.Pool = f[2]
		}
		in.Rate, in.RateKnwn = hourlyRate[in.Type]
		list = append(list, in)
	}
	return list
}

func (r *runner) karpenterState(workload string, rep *statusReport) []nodeClass {
	if workload == "" {
		return nil
	}
	kc := cubClusterKubeconfig(workload)
	outStr, err := r.kubectl(kc, "get", "ec2nodeclasses.karpenter.k8s.aws", "-o", "json")
	if err != nil {
		rep.KarpenterError = err.Error()
		return nil
	}
	var parsed struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Status struct {
				Conditions []struct {
					Type   string `json:"type"`
					Status string `json:"status"`
				} `json:"conditions"`
				Subnets              []any `json:"subnets"`
				SecurityGroups       []any `json:"securityGroups"`
				AMIs                 []any `json:"amis"`
				CapacityReservations []any `json:"capacityReservations"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(outStr), &parsed); err != nil {
		rep.KarpenterError = fmt.Sprintf("parsing EC2NodeClass list: %v", err)
		return nil
	}
	var list []nodeClass
	for _, it := range parsed.Items {
		n := nodeClass{
			Name:           it.Metadata.Name,
			Ready:          "?",
			Subnets:        len(it.Status.Subnets),
			SecurityGroups: len(it.Status.SecurityGroups),
			AMIs:           len(it.Status.AMIs),
			Reservations:   len(it.Status.CapacityReservations),
		}
		for _, c := range it.Status.Conditions {
			if c.Type == "Ready" {
				n.Ready = c.Status
			}
		}
		list = append(list, n)
	}
	return list
}

func (rep *statusReport) write(w io.Writer) {
	fmt.Fprintf(w, "\n── billing (from EC2, the only reliable source)\n")
	switch {
	case rep.BillingError != "":
		fmt.Fprintf(w, "  !! cannot query EC2 — cost is UNKNOWN, not zero:\n     %s\n", rep.BillingError)
		fmt.Fprintf(w, "  !! if your SSO session expired, run: aws sso login\n")
	case len(rep.Billing) == 0:
		fmt.Fprintf(w, "  no running instances in %s\n", rep.Region)
	default:
		var total float64
		gpu := 0
		for _, in := range rep.Billing {
			rate := "  rate unknown"
			if in.RateKnwn {
				rate = fmt.Sprintf("  $%.3f/hr", in.Rate)
				total += in.Rate
			}
			if strings.HasPrefix(in.Type, "g") || strings.HasPrefix(in.Type, "p") {
				gpu++
			}
			fmt.Fprintf(w, "  %-20s %-14s %-16s%s\n", in.ID, in.Type, in.Pool, rate)
		}
		fmt.Fprintf(w, "\n  approx total: $%.2f/hr  (~$%.0f/month if left running)\n", total, total*730)
		if gpu > 0 {
			fmt.Fprintf(w, "  !! %d GPU instance(s) running — scale workloads to 0 to release them\n", gpu)
		}
	}

	fmt.Fprintf(w, "\n── clusters\n")
	writeCluster(w, "mgmt    ", rep.MgmtCluster, "creates AWS infrastructure")
	writeCluster(w, "workload", rep.WorkloadCluster, "runs the inference stack")

	fmt.Fprintf(w, "\n── EC2NodeClass resolution (validates the cross-plane contract against AWS)\n")
	switch {
	case rep.KarpenterError != "":
		fmt.Fprintf(w, "  !! %s\n", rep.KarpenterError)
	case rep.WorkloadCluster == "":
		// Do NOT claim "not deployed" here. Reaching this branch means the
		// workload cluster was never identified, which happens both when it does
		// not exist and when it cannot be reached — and the cluster section above
		// has already said which. Asserting either one would be the exact
		// unreachable-reads-as-empty mistake this command exists to avoid.
		fmt.Fprintf(w, "  no workload cluster identified — see the cluster section above\n")
	case len(rep.Karpenter) == 0:
		fmt.Fprintf(w, "  workload cluster reachable, but no EC2NodeClasses exist —\n")
		fmt.Fprintf(w, "  the karpenter component is not deployed to it\n")
	default:
		for _, n := range rep.Karpenter {
			fmt.Fprintf(w, "  %-10s ready=%-6s subnets=%d sgs=%d amis=%d reservations=%d\n",
				n.Name, n.Ready, n.Subnets, n.SecurityGroups, n.AMIs, n.Reservations)
			if n.Subnets == 0 || n.SecurityGroups == 0 || n.AMIs == 0 {
				fmt.Fprintf(w, "     !! resolved 0 subnets/SGs/AMIs — the karpenter.sh/discovery tag\n")
				fmt.Fprintf(w, "        no longer agrees with what aws-network writes\n")
			}
		}
	}
	fmt.Fprintln(w)
}

func writeCluster(w io.Writer, label, name, what string) {
	if name == "" {
		fmt.Fprintf(w, "  %s !! no reachable cluster (%s)\n", label, what)
		fmt.Fprintf(w, "           not deployed, or unreachable — these are not the same thing\n")
		return
	}
	fmt.Fprintf(w, "  %s %s  (%s)\n", label, name, what)
}

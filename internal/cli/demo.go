package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/mwtrigg/awxctl/internal/demo"
)

var demoCmd = &cobra.Command{
	Use:   "demo",
	Short: "Serve a made-up controller on localhost to try awxctl without AWX or AAP",
	Long: `Serves a small made-up controller on this machine and prints the variables that
point awxctl at it. Everything in it is invented (hosts under example.com): a few
job templates, an inventory, and some jobs, one of which failed.

  awxctl demo &
  export AWXCTL_URL=http://127.0.0.1:8043 AWXCTL_API_TOKEN=fake-token AWXCTL_PERMISSIONS=operator
  awxctl get jobs --failed -o table
  awxctl diagnose job 1042 --compare-last-success -o table
  awxctl launch "Patch servers" --limit web1.example.com          # a dry run
  awxctl launch "Patch servers" --limit web1.example.com --apply

Changes are kept in memory until it stops (Ctrl-C). It answers the calls awxctl
makes, not the whole API, so awxctl doctor reports much of it as missing.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		addr, _ := cmd.Flags().GetString("addr")
		flavor, _ := cmd.Flags().GetString("flavor")
		if flavor != "awx" && flavor != "aap" {
			return &usageError{"--flavor is awx or aap"}
		}
		url, stop, err := demo.Start(flavor, addr)
		if err != nil {
			return &usageError{"cannot start the demo: " + err.Error()}
		}
		defer stop()
		out := cmd.ErrOrStderr() // stdout stays free: nothing here is a result
		fmt.Fprintf(out, "demo controller (%s layout) on %s; made-up data, kept in memory\n\n", flavor, url)
		fmt.Fprintf(out, "  export AWXCTL_URL=%s AWXCTL_API_TOKEN=%s AWXCTL_PERMISSIONS=operator\n\n", url, demo.Token)
		fmt.Fprintln(out, "then, in the same shell or another one:")
		fmt.Fprintln(out, "  awxctl get jobs --failed -o table")
		fmt.Fprintln(out, "  awxctl diagnose job 1042 --compare-last-success -o table")
		fmt.Fprintln(out, "  awxctl launch \"Patch servers\" --limit web1.example.com")
		fmt.Fprintln(out, "\nCtrl-C stops it.")
		<-cmd.Context().Done()
		return nil
	},
}

func init() {
	demoCmd.Flags().String("addr", "127.0.0.1:8043", "address to listen on (this machine only by default)")
	demoCmd.Flags().String("flavor", "awx", "awx or aap: which API layout to serve")
	rootCmd.AddCommand(demoCmd)
}

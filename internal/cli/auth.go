package cli

import (
	"github.com/spf13/cobra"

	"github.com/mwtrigg/awxctl/internal/output"
)

var authCmd = group(&cobra.Command{
	Use:   "auth",
	Short: "Check the connection and credentials",
})

var authStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show which controller and user the active context reaches",
	Long: `Connects to the controller of the active context, detects whether it is AWX
(/api/v2) or the AAP gateway (/api/controller/v2), and shows the controller
version and the user the credentials belong to. Changes nothing.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		ctx, name, client, err := connect()
		if err != nil {
			return err
		}
		info, err := client.Detect(cmd.Context())
		if err != nil {
			return err
		}
		me, err := client.Me(cmd.Context())
		if err != nil {
			return err
		}
		auth := "none"
		switch {
		case ctx.APIToken != "":
			auth = "token"
		case ctx.Username != "":
			auth = "password"
		}
		tier := ctx.Permissions
		if tier == "" {
			tier = "readonly"
		}
		mode := "dry-run"
		if ctx.ApplyByDefault() {
			mode = "apply"
		}
		cols := []output.Column{
			{Key: "context", Header: "CONTEXT"}, {Key: "url", Header: "URL"}, {Key: "flavor", Header: "FLAVOR"},
			{Key: "api_path", Header: "API PATH"}, {Key: "controller_version", Header: "VERSION"},
			{Key: "username", Header: "USER"}, {Key: "superuser", Header: "SUPERUSER"}, {Key: "auth", Header: "AUTH"},
			{Key: "permissions", Header: "PERMISSIONS"}, {Key: "mode", Header: "MODE"},
		}
		row := map[string]any{
			"context": name, "url": client.BaseURL(), "flavor": string(info.Flavor), "api_path": info.APIPath,
			"controller_version": info.Version, "username": me.Username, "superuser": me.IsSuperuser, "auth": auth,
			"permissions": tier, "mode": mode,
		}
		return output.Print(output.Format(flagOutput), cols, []map[string]any{row})
	},
}

func init() {
	authCmd.AddCommand(authStatusCmd)
}

package cli

import (
	"bufio"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/spf13/cobra"

	"github.com/mwtrigg/awxctl/internal/config"
)

var configCmd = group(&cobra.Command{Use: "config", Short: "The config file: where it is, and a starting point for it"})

var contextName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

// configTemplate is the file config init writes: one context, the rest as
// comments to switch on.
const configTemplate = `# awxctl configuration. Keep this file private (mode 0600): it holds an API token.
# Flags override AWXCTL_* environment variables, which override this file.

current_context: %[1]s

contexts:
  %[1]s:
    url: %[2]s
    # A personal access token (AWX: Users > your user > Tokens; AAP: Access
    # Management > Users > your user > Tokens). Read scope is enough for looking.
    api_token: %[3]s
    # What this context may do. readonly: look, and show changes as dry runs.
    # operator: also launch, relaunch, cancel, sync, approve. write: everything.
    # This guards against accidents; the token's own rights are the real limit:
    # see awxctl auth status.
    permissions: %[4]s

    # mode: dry-run            # dry-run (default): a change needs --apply. apply: it needs none.
    # allowed_templates:       # the only templates that may be launched (names or id:<n>);
    #   - "Patch servers"      # the MCP server launches nothing that is not listed here
    # flavor: auto             # auto | awx | aap (AAP 2.5+ behind the gateway)
    # default_limit: 100       # rows a list returns unless --limit says otherwise
    # output: json             # default output format: json | jsonl | table | csv
    # ca_file: /path/to/ca.pem # a private certificate authority
    # state_dir: ~/.local/state/awxctl     # audit records of applied changes
    # state_keep: 200          # how many of them to keep

  # A second controller is another block:
  # lab:
  #   url: https://awx.lab.example.com
  #   api_token: "<token>"
  #   permissions: write
`

var configInitCmd = &cobra.Command{
	Use:   "init",
	Short: "Write a starting config file with one context",
	Long: `Writes ~/.config/awxctl/config.yaml (YAML, readable only by you) with one context
and the other settings as comments. It never overwrites an existing file.

  awxctl config init --url https://awx.example.com
  awxctl config init --context prod --url https://aap.example.com --token-stdin < token.txt

The API token is not taken as a flag, so that it does not end up in the shell
history: pipe it in with --token-stdin, or leave it out and put it into the file
yourself (or keep it out of the file altogether and set AWXCTL_API_TOKEN).

Then: awxctl auth status`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		name, _ := cmd.Flags().GetString("context")
		rawURL, _ := cmd.Flags().GetString("url")
		tier, _ := cmd.Flags().GetString("permissions")
		fromStdin, _ := cmd.Flags().GetBool("token-stdin")
		if !contextName.MatchString(name) {
			return &usageError{"--context is a short name of letters, digits, - _ and ."}
		}
		if tier == "" || config.ValidateTier(tier) != nil {
			return &usageError{"--permissions is readonly, operator or write"}
		}
		if rawURL == "" {
			rawURL = "https://awx.example.com"
		} else if u, err := url.Parse(rawURL); err != nil || u.Scheme != "https" || u.Host == "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
			return &usageError{"--url is the controller's address, like https://awx.example.com (https; no user, query or fragment)"}
		}
		token := `"<paste the API token here>"`
		if fromStdin {
			line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
			t := strings.TrimSpace(line)
			if t == "" || strings.ContainsAny(t, " \t\"'\\") {
				return &usageError{"--token-stdin: no token on standard input (or it holds spaces or quotes)"}
			}
			token = `"` + t + `"`
		}
		path, err := config.Path()
		if err != nil {
			return err
		}
		if _, err := os.Stat(path); err == nil {
			return &usageError{path + " already exists: edit it, or move it away first (awxctl context list shows what it holds)"}
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return &usageError{err.Error()}
		}
		// O_EXCL: never write over a file that appeared in the meantime.
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return &usageError{err.Error()}
		}
		if _, err := fmt.Fprintf(f, configTemplate, name, strings.TrimRight(rawURL, "/"), token, tier); err != nil {
			f.Close()
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
		fmt.Fprintln(os.Stderr, "wrote "+path+" (context "+name+", permissions "+tier+")")
		if !fromStdin {
			fmt.Fprintln(os.Stderr, "next: put the API token into it (or set AWXCTL_API_TOKEN), then run: awxctl auth status")
		} else {
			fmt.Fprintln(os.Stderr, "next: awxctl auth status")
		}
		return nil
	},
}

var configPathCmd = &cobra.Command{
	Use:   "path",
	Short: "Print where the config file is looked for",
	Args:  cobra.NoArgs,
	RunE: func(*cobra.Command, []string) error {
		path, err := config.Path()
		if err != nil {
			return err
		}
		fmt.Println(path)
		return nil
	},
}

func init() {
	configInitCmd.Flags().String("context", "default", "name of the context to write")
	configInitCmd.Flags().String("url", "", "the controller's address, e.g. https://awx.example.com")
	configInitCmd.Flags().String("permissions", "readonly", "readonly, operator or write")
	configInitCmd.Flags().Bool("token-stdin", false, "read the API token from standard input")
	configCmd.AddCommand(configInitCmd, configPathCmd)
	rootCmd.AddCommand(configCmd)
}

// Package cli is the command wiring: it reads flags, calls core and prints. No API
// logic lives here.
package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/mwtrigg/awxctl/internal/config"
	"github.com/mwtrigg/awxctl/internal/core"
	"github.com/mwtrigg/awxctl/internal/output"
	"github.com/mwtrigg/awxctl/internal/version"
)

var (
	flagOutput   string
	flagContext  string
	flagField    string
	flagInsecure bool
	flagRaw      bool
	flagTimeout  time.Duration
	flagRetries  int
)

var warnedHTTP bool

// cliErr carries an exit code for an error that was already printed.
type cliErr struct{ code int }

func (e *cliErr) Error() string { return "" }

// usageError marks a bad flag or argument (exit code 5).
type usageError struct{ msg string }

func (e *usageError) Error() string { return e.msg }

var rootCmd = &cobra.Command{
	Use:   "awxctl",
	Short: "awxctl: AWX and Ansible Automation Platform CLI for agents and operators",
	Long: `awxctl is a CLI for the AWX / Ansible Automation Platform controller API.

Single static binary. Machine-readable output. Structured errors.
Settings: flags override AWXCTL_* environment variables, which override
~/.config/awxctl/config.yaml.`,
	SilenceUsage:  true,
	SilenceErrors: true,
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		if flagYes {
			flagApply = true
		}
		if err := enforceMCPPolicy(cmd, args); err != nil {
			return err
		}
		output.Raw = flagRaw
		output.Field = flagField
		output.Limit, output.Fetched, output.Total, output.TotalKnown = 0, 0, 0, false
		output.Command = strings.TrimPrefix(cmd.CommandPath(), "awxctl ")
		if flagYes {
			flagApply = true
		}
		if flagApply && flagDryRun {
			return &usageError{"--apply and --dry-run contradict each other: use one"}
		}

		switch output.Format(flagOutput) {
		case output.FormatJSON, output.FormatJSONL, output.FormatTable, output.FormatCSV:
		default:
			bad := flagOutput
			flagOutput = string(output.DefaultFormat()) // so the error itself is structured
			return &usageError{"unknown output format " + bad + ": use json, jsonl, table or csv"}
		}
		return nil
	},
}

// classify maps an error to an exit code.
func classify(err error) int {
	var ue *usageError
	var fe *output.FieldError
	var pe *config.PermissionError
	var uo *core.OutcomeUnknownError
	var amb *core.AmbiguousError
	switch {
	case errors.As(err, &uo):
		return output.ExitUnknown
	case errors.Is(err, context.Canceled):
		return output.ExitError // interrupted before anything (more) was sent
	case errors.As(err, &pe), errors.Is(err, core.ErrDenied):
		return output.ExitPermission
	case errors.Is(err, config.ErrNotConfigured), errors.As(err, &ue), errors.As(err, &fe), isCobraUsage(err),
		errors.Is(err, core.ErrUsage), errors.As(err, &amb):
		return output.ExitUsage
	case errors.Is(err, core.ErrNotFound):
		return output.ExitNotFound
	case errors.Is(err, core.ErrConnect):
		return output.ExitConnect
	case errors.Is(err, core.ErrAuth):
		return output.ExitAuthError
	}
	return output.ExitError
}

// isCobraUsage recognises cobra's own flag, argument and command errors.
func isCobraUsage(err error) bool {
	m := err.Error()
	for _, p := range []string{"unknown command", "unknown flag", "unknown shorthand flag", "accepts ", "requires at least", "required flag", "invalid argument", "flag needs an argument"} {
		if strings.HasPrefix(m, p) {
			return true
		}
	}
	return false
}

// Execute runs the root command and returns the exit code.
func Execute() int {
	// Ctrl-C and a kill from a supervisor (the MCP server's time limit) cancel the
	// context: the request in flight is recorded as outcome unknown, later steps
	// as skipped, and the audit record is written before the process ends.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := rootCmd.ExecuteContext(ctx); err != nil {
		var ce *cliErr
		if errors.As(err, &ce) {
			return ce.code
		}
		code := classify(err)
		output.PrintError(output.Format(flagOutput), err.Error(), code)
		return code
	}
	return output.ExitSuccess
}

// connect loads the active context and builds a client for it.
func connect() (*config.Context, string, *core.Client, error) {
	name, err := config.ActiveContextName(flagContext)
	if err != nil {
		return nil, "", nil, err
	}
	_, ctx, err := config.LoadConfig(flagContext)
	if err != nil {
		return nil, "", nil, err
	}
	if err := ctx.ValidateMode(); err != nil {
		return nil, "", nil, &usageError{err.Error()}
	}
	if strings.HasPrefix(strings.ToLower(ctx.URL), "http://") && !warnedHTTP {
		warnedHTTP = true
		fmt.Fprintln(os.Stderr, "warning: the url is http://, so the token and everything else travel unencrypted")
	}
	if flagInsecure {
		off := false
		ctx.TLSVerify = &off
	}
	client, err := core.NewClient(ctx, core.Options{Timeout: flagTimeout, Retries: flagRetries})
	if err != nil {
		return nil, "", nil, &usageError{err.Error()}
	}
	return ctx, name, client, nil
}

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print version information",
	Args:  cobra.NoArgs,
	RunE: func(*cobra.Command, []string) error {
		cols := []output.Column{{Key: "version", Header: "VERSION"}, {Key: "os", Header: "OS"}, {Key: "arch", Header: "ARCH"}}
		row := map[string]any{"version": version.Version, "os": runtime.GOOS, "arch": runtime.GOARCH}
		return output.Print(output.Format(flagOutput), cols, []map[string]any{row})
	},
}

func init() {
	pf := rootCmd.PersistentFlags()
	pf.StringVarP(&flagOutput, "output", "o", string(output.DefaultFormat()), "output format: json|jsonl|table|csv")
	pf.StringVarP(&flagContext, "context", "c", "", "override current context")
	pf.StringVar(&flagField, "field", "", "print only this field of each result, one value per line")
	pf.DurationVar(&flagTimeout, "timeout", 180*time.Second, "timeout of each API request")
	pf.IntVar(&flagRetries, "retries", 2, "retries of read-only calls after a connection error or 502/503/504 (writes are never retried)")
	pf.BoolVar(&flagRaw, "raw", false, "include raw API values (*_raw, *_unix) next to the readable ones")
	pf.BoolVar(&flagInsecure, "insecure", false, "disable TLS verification")

	rootCmd.AddCommand(authCmd)
	rootCmd.AddCommand(versionCmd)
}

// cliPrintErr prints a structured error and returns the exit code to end with.
func cliPrintErr(outFmt output.Format, msg string, code int) error {
	output.PrintError(outFmt, msg, code)
	return &cliErr{code: code}
}

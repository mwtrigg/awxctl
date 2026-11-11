package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/mwtrigg/awxctl/internal/config"
	"github.com/mwtrigg/awxctl/internal/core"
	"github.com/mwtrigg/awxctl/internal/output"
)

var (
	flagApply  bool
	flagYes    bool
	flagDryRun bool
	flagExpect int
)

// stateDir is where audit records go: the context's state_dir, or
// ~/.local/state/awxctl.
func stateDir(ctx *config.Context) (string, error) {
	if ctx != nil && ctx.StateDir != "" {
		return ctx.StateDir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state", "awxctl"), nil
}

var planColumns = []output.Column{
	{Key: "target", Header: "TARGET"}, {Key: "action", Header: "ACTION"}, {Key: "status", Header: "STATUS"},
	{Key: "result", Header: "RESULT"}, {Key: "impact", Header: "ALSO"}, {Key: "error", Header: "ERROR"},
}

func planRows(p *core.Plan) []map[string]any {
	rows := make([]map[string]any, len(p.Steps))
	for i, s := range p.Steps {
		row := map[string]any{"target": s.Target, "action": s.Action, "status": s.Status, "request": s.Method + " " + s.Path}
		if s.Send != nil {
			row["send"] = s.Send
		}
		if s.Before != nil {
			row["before"] = s.Before
		}
		if s.Impact != "" {
			row["impact"] = s.Impact
		}
		if s.Result != nil {
			row["result"] = s.Result
		}
		if s.Error != "" {
			row["error"] = s.Error
		}
		rows[i] = row
	}
	if output.Format(flagOutput) == output.FormatTable { // a map prints badly in a cell
		for _, row := range rows {
			if res, ok := row["result"].(map[string]any); ok {
				keys := make([]string, 0, len(res))
				for k := range res {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				parts := make([]string, len(keys))
				for i, k := range keys {
					parts[i] = fmt.Sprintf("%s=%v", k, res[k])
				}
				row["result"] = strings.Join(parts, " ")
			}
		}
	}
	return rows
}

// runPlanQuiet is runPlan for a caller that prints the applied result itself.
func runPlanQuiet(cmd *cobra.Command, ctx *config.Context, ctxName string, client *core.Client, p *core.Plan) (bool, error) {
	return runPlanMode(cmd, ctx, ctxName, client, p, true)
}

// runPlan shows a plan (the default) or applies it (--apply, or mode: apply). It
// reports whether it applied, so a caller can use the results.
func runPlan(cmd *cobra.Command, ctx *config.Context, ctxName string, client *core.Client, p *core.Plan) (applied bool, err error) {
	return runPlanMode(cmd, ctx, ctxName, client, p, false)
}

func runPlanMode(cmd *cobra.Command, ctx *config.Context, ctxName string, client *core.Client, p *core.Plan, quiet bool) (applied bool, err error) {
	outFmt := output.Format(flagOutput)
	if cmd.Flags().Changed("expect") && flagExpect != len(p.Steps) {
		return false, &usageError{fmt.Sprintf("--expect %d, but this would change %d object(s): nothing was changed", flagExpect, len(p.Steps))}
	}
	apply := flagApply || (ctx.ApplyByDefault() && !flagDryRun)
	if len(p.Steps) == 0 {
		fmt.Fprintln(os.Stderr, "nothing to change: everything is already as asked")
		return false, output.Print(outFmt, planColumns, []map[string]any{})
	}
	if !apply {
		if err := output.Print(outFmt, planColumns, planRows(p)); err != nil {
			return false, err
		}
		note := "dry run: nothing was changed; add --apply to do it"
		if perr := ctx.CheckPermission(p.Tier); perr != nil {
			note += " (and the context needs permissions: " + p.Tier + ")"
		}
		fmt.Fprintln(os.Stderr, note)
		return false, nil
	}
	if err := ctx.CheckPermission(p.Tier); err != nil {
		return false, err
	}
	var rec *core.FileRecorder
	var recorder core.Recorder
	if ctx.SaveBeforeState() {
		dir, err := stateDir(ctx)
		if err != nil {
			return false, err
		}
		rec = &core.FileRecorder{Dir: dir, Keep: ctx.KeepStates(), Meta: map[string]any{"context": ctxName, "url": client.BaseURL(), "args": maskArgs(os.Args[1:])}}
		recorder = rec
	}
	runErr := p.Apply(cmd.Context(), client, recorder)
	if !quiet || runErr != nil {
		if err := output.Print(outFmt, planColumns, planRows(p)); err != nil {
			return true, err
		}
	}
	if rec != nil && rec.Path() != "" {
		fmt.Fprintln(os.Stderr, "record:", rec.Path())
		rec.Prune()
	}
	if runErr != nil {
		code := classify(runErr)
		return true, cliPrintErr(outFmt, runErr.Error(), code)
	}
	return true, nil
}

// parseValue reads a NAME=VALUE value. true, false and null, and JSON lists and
// objects, are sent as such. A number is sent as a number only when that changes
// nothing: 5 and 2.5 are numbers, but 1.10, 007, 1e3 and an integer too long for
// 64 bits stay text exactly as typed (a version "1.10" must not arrive as 1.1).
// Everything else is text. NAME:=JSON forces JSON (see splitAssignment).
func parseValue(s string) any {
	switch s {
	case "true":
		return true
	case "false":
		return false
	case "null":
		return nil
	}
	if t := strings.TrimSpace(s); strings.HasPrefix(t, "[") || strings.HasPrefix(t, "{") {
		if v, err := parseJSON(s); err == nil {
			return v
		}
		return s
	}
	if i, err := strconv.ParseInt(s, 10, 64); err == nil && strconv.FormatInt(i, 10) == s {
		return i
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil && strconv.FormatFloat(f, 'f', -1, 64) == s && strings.Contains(s, ".") {
		return f
	}
	return s
}

// parseJSON decodes one JSON value, keeping numbers exactly as written.
func parseJSON(s string) (any, error) {
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, errors.New("more than one JSON value")
	}
	return v, nil
}

// splitAssignment reads NAME=VALUE (value typed by parseValue) or NAME:=JSON
// (value is JSON, for a number or text that parseValue would read differently).
func splitAssignment(flag, kv string) (string, any, error) {
	if k, raw, ok := strings.Cut(kv, ":="); ok && k != "" && !strings.Contains(k, "=") {
		v, err := parseJSON(raw)
		if err != nil {
			return "", nil, &usageError{fmt.Sprintf("%s %s: the value after := is not JSON: %v", flag, k, err)}
		}
		return k, v, nil
	}
	k, v, ok := strings.Cut(kv, "=")
	if !ok || k == "" {
		return "", nil, &usageError{fmt.Sprintf("%s wants NAME=VALUE (or NAME:=JSON), got %q", flag, kv)}
	}
	return k, parseValue(v), nil
}

// textFields are sent to the API as text even when given as a structure.
var textFields = map[string]bool{"variables": true, "extra_vars": true, "source_vars": true}

// readValues merges --file, --set and --set-file into the fields to write.
func readValues(cmd *cobra.Command) (map[string]any, error) {
	values := map[string]any{}
	file, _ := cmd.Flags().GetString("file")
	if file != "" {
		data, err := readFileOrStdin(file)
		if err != nil {
			return nil, err
		}
		if err := yaml.Unmarshal(data, &values); err != nil {
			return nil, &usageError{fmt.Sprintf("--file %s is not a YAML or JSON mapping: %v", file, err)}
		}
	}
	sets, _ := cmd.Flags().GetStringArray("set")
	for _, kv := range sets {
		k, v, err := splitAssignment("--set", kv)
		if err != nil {
			return nil, err
		}
		if secretLooking(k) {
			return nil, &usageError{fmt.Sprintf("--set %s would put a secret on the command line: use --set-file %s=PATH (or - for stdin) or --file", k, k)}
		}
		values[k] = v
	}
	setFiles, _ := cmd.Flags().GetStringArray("set-file")
	for _, kv := range setFiles {
		k, path, ok := strings.Cut(kv, "=")
		if !ok || k == "" || path == "" {
			return nil, &usageError{fmt.Sprintf("--set-file wants FIELD=PATH, got %q", kv)}
		}
		data, err := readFileOrStdin(path)
		if err != nil {
			return nil, err
		}
		var v any
		if yaml.Unmarshal(data, &v) == nil {
			if _, scalar := v.(string); !scalar && v != nil {
				values[k] = v
				continue
			}
		}
		values[k] = strings.TrimRight(string(data), "\n")
	}
	for k, v := range values {
		if _, isText := v.(string); textFields[k] && !isText && v != nil {
			b, err := json.Marshal(v)
			if err != nil {
				return nil, err
			}
			values[k] = string(b)
		}
	}
	return values, nil
}

func secretLooking(k string) bool {
	m := core.Mask(map[string]any{k: "x"}).(map[string]any)
	return m[k] != "x"
}

func readFileOrStdin(path string) ([]byte, error) {
	if path == "-" {
		return io.ReadAll(os.Stdin)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, &usageError{err.Error()}
	}
	return data, nil
}

// targets gathers the positional names plus --id-file (one name or ID per line).
func targets(cmd *cobra.Command, args []string) ([]string, error) {
	out := append([]string(nil), args...)
	if file, _ := cmd.Flags().GetString("id-file"); file != "" {
		data, err := readFileOrStdin(file)
		if err != nil {
			return nil, err
		}
		for _, line := range bytes.Split(data, []byte("\n")) {
			if s := strings.TrimSpace(string(line)); s != "" && !strings.HasPrefix(s, "#") {
				out = append(out, s)
			}
		}
	}
	if len(out) == 0 {
		return nil, &usageError{"name at least one object (or use --id-file)"}
	}
	return out, nil
}

func scopeFlag(cmd *cobra.Command, r *core.Resource) url.Values {
	q := url.Values{}
	if r.Scope == "" {
		return q
	}
	if fl := cmd.Flags().Lookup(r.Scope); fl != nil && fl.Changed {
		q.Set(r.Scope+"__name", fl.Value.String())
	}
	return q
}

func kindArg(name string) (*core.Resource, error) {
	r := core.FindResource(name)
	if r == nil {
		var kinds []string
		for _, k := range core.Resources() {
			kinds = append(kinds, k.Kind)
		}
		return nil, &usageError{fmt.Sprintf("unknown kind %q; the kinds are: %s", name, strings.Join(kinds, ", "))}
	}
	return r, nil
}

func addBulkFlags(c *cobra.Command) {
	c.Flags().IntVar(&flagExpect, "expect", 0, "refuse to run unless exactly this many objects would change")
	c.Flags().String("id-file", "", "file with one name or ID per line (- for stdin), added to the arguments")
	c.Flags().String("inventory", "", "for hosts, groups and inventory sources: the inventory the names are in")
}

// ---- launch, relaunch, cancel, sync, approve ----

var (
	lLimit, lTags, lSkip, lInventory, lBranch, lVarsFile string
	lVars                                                []string
	lCheck, lDiff, lWait, lFollow                        bool
	lVerbosity                                           int
)

// allowed checks the launch allowlist. An empty list allows everything for a
// person; over MCP only listed templates may be launched.
func allowed(ctx *config.Context, tmpl core.Object) error {
	name, _ := tmpl["name"].(string)
	id := "id:" + strconv.Itoa(core.ID(tmpl))
	for _, a := range ctx.AllowedTemplates {
		if a == name || a == id {
			return nil
		}
	}
	if len(ctx.AllowedTemplates) == 0 && os.Getenv("AWXCTL_FROM_MCP") == "" {
		return nil
	}
	return &config.PermissionError{Required: "an allowed_templates entry for " + strconv.Quote(name) + " (or " + id + ")", Have: "not on the context's allowlist"}
}

func extraVars() (map[string]any, error) {
	vars := map[string]any{}
	if lVarsFile != "" {
		data, err := readFileOrStdin(lVarsFile)
		if err != nil {
			return nil, err
		}
		if err := yaml.Unmarshal(data, &vars); err != nil {
			return nil, &usageError{fmt.Sprintf("--extra-vars-file is not a YAML or JSON mapping: %v", err)}
		}
	}
	for _, kv := range lVars {
		if strings.HasPrefix(strings.TrimSpace(kv), "{") {
			obj, err := parseJSON(kv)
			m, isMap := obj.(map[string]any)
			if err != nil || !isMap {
				return nil, &usageError{"--extra-vars: not a JSON object"}
			}
			for k, v := range m {
				vars[k] = v
			}
			continue
		}
		k, v, err := splitAssignment("--extra-vars", kv)
		if err != nil {
			return nil, err
		}
		vars[k] = v
	}
	return vars, nil
}

var launchCmd = &cobra.Command{
	Use:   "launch <template>",
	Short: "Launch a job template (or a workflow template with --workflow)",
	Long: `Launches a template. Without --apply it only shows what would be launched.

  awxctl launch "Patch servers" --limit web1 --extra-vars reboot=false
  awxctl launch "Patch servers" --limit web1 --apply --wait

--check launches the job in Ansible check mode; that is a real job. --dry-run (the
default) sends nothing at all.

An option the template does not prompt for is refused: the controller would
ignore it silently, and an ignored --limit means running against every host.
See what a template accepts with: awxctl get launch-info <template>`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, name, client, err := connect()
		if err != nil {
			return err
		}
		r, tmpl, err := findTemplate(cmd, client, args[0])
		if err != nil {
			return err
		}
		if err := allowed(ctx, tmpl); err != nil {
			return err
		}
		vars, err := extraVars()
		if err != nil {
			return err
		}
		opts := core.LaunchOptions{Limit: lLimit, Tags: lTags, SkipTags: lSkip, ExtraVars: vars, Inventory: lInventory, Check: lCheck, Diff: lDiff, Branch: lBranch, Verbosity: -1}
		if cmd.Flags().Changed("verbosity") {
			opts.Verbosity = lVerbosity
		}
		p, err := client.PlanLaunch(cmd.Context(), r, "id:"+strconv.Itoa(core.ID(tmpl)), opts)
		if err != nil {
			return err
		}
		return runAndMaybeWait(cmd, ctx, name, client, p)
	},
}

// runAndMaybeWait applies a plan that starts one job and, with --wait or
// --follow, waits for it.
func runAndMaybeWait(cmd *cobra.Command, ctx *config.Context, name string, client *core.Client, p *core.Plan) error {
	applied, err := runPlan(cmd, ctx, name, client, p)
	if err != nil || !(lWait || lFollow) {
		return err
	}
	if !applied {
		fmt.Fprintln(os.Stderr, "--wait and --follow do nothing in a dry run: no job was started")
		return nil
	}
	id := 0
	if res := p.Steps[0].Result; res != nil {
		id, _ = res["job_id"].(int)
	}
	if id == 0 {
		return errors.New("the launch answer named no job to wait for")
	}
	j, err := client.FindJob(cmd.Context(), id)
	if err != nil {
		return err
	}
	return waitAndReport(cmd.Context(), client, j, lFollow)
}

var relaunchCmd = &cobra.Command{
	Use:   "relaunch <job-id>",
	Short: "Run a job again with the same settings (--hosts failed: only on the hosts that failed)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id, err := jobID(args[0])
		if err != nil {
			return err
		}
		ctx, name, client, err := connect()
		if err != nil {
			return err
		}
		j, err := client.FindJob(cmd.Context(), id)
		if err != nil {
			return err
		}
		if tid := core.Lookup(j.Obj, "summary_fields.unified_job_template.id"); tid != nil || len(ctx.AllowedTemplates) > 0 || os.Getenv("AWXCTL_FROM_MCP") != "" {
			tmpl := core.Object{"id": tid, "name": core.Lookup(j.Obj, "summary_fields.unified_job_template.name")}
			if err := allowed(ctx, tmpl); err != nil {
				return err
			}
		}
		hosts, _ := cmd.Flags().GetString("hosts")
		p, err := client.PlanRelaunch(cmd.Context(), id, hosts)
		if err != nil {
			return err
		}
		return runAndMaybeWait(cmd, ctx, name, client, p)
	},
}

func idArgs(cmd *cobra.Command, args []string) ([]int, error) {
	names, err := targets(cmd, args)
	if err != nil {
		return nil, err
	}
	seen := map[int]bool{}
	var ids []int
	for _, n := range names {
		id, err := jobID(n)
		if err != nil {
			return nil, err
		}
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	return ids, nil
}

var cancelCmd = &cobra.Command{
	Use:   "cancel <job-id>...",
	Short: "Cancel running or pending jobs",
	RunE: func(cmd *cobra.Command, args []string) error {
		ids, err := idArgs(cmd, args)
		if err != nil {
			return err
		}
		ctx, name, client, err := connect()
		if err != nil {
			return err
		}
		p, err := client.PlanCancel(cmd.Context(), ids)
		if err != nil {
			return err
		}
		_, err = runPlan(cmd, ctx, name, client, p)
		return err
	},
}

var syncCmd = &cobra.Command{
	Use:   "sync <projects|inventory-sources> <name>...",
	Short: "Start a project sync (SCM update) or an inventory source sync",
	Args:  cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		r, err := kindArg(args[0])
		if err != nil {
			return err
		}
		if r.Kind != "projects" && r.Kind != "inventory-sources" {
			return &usageError{"sync takes projects or inventory-sources"}
		}
		refs, err := targets(cmd, args[1:])
		if err != nil {
			return err
		}
		ctx, name, client, err := connect()
		if err != nil {
			return err
		}
		p, err := client.PlanSync(cmd.Context(), r, refs, scopeFlag(cmd, r))
		if err != nil {
			return err
		}
		if len(p.Steps) == 1 {
			return runAndMaybeWait(cmd, ctx, name, client, p)
		}
		_, err = runPlan(cmd, ctx, name, client, p)
		return err
	},
}

func approvalCmd(verb string, approve bool) *cobra.Command {
	c := &cobra.Command{
		Use:   verb + " <approval-id>...",
		Short: strings.ToUpper(verb[:1]) + verb[1:] + " pending workflow approvals (see: awxctl get workflow-approvals --status pending)",
		RunE: func(cmd *cobra.Command, args []string) error {
			ids, err := idArgs(cmd, args)
			if err != nil {
				return err
			}
			ctx, name, client, err := connect()
			if err != nil {
				return err
			}
			p, err := client.PlanApproval(cmd.Context(), ids, approve)
			if err != nil {
				return err
			}
			_, err = runPlan(cmd, ctx, name, client, p)
			return err
		},
	}
	addBulkFlags(c)
	return c
}

// ---- create, update, delete, enable, disable, attach, detach ----

var createCmd = &cobra.Command{
	Use:   "create <kind> [name]",
	Short: "Create an object: hosts, inventories, groups, projects, job-templates, schedules, ...",
	Long: `Creates one object. Fields come from --set FIELD=VALUE (repeatable), --file (a
YAML or JSON mapping) and --set-file FIELD=PATH (the value is read from a file,
- for stdin; the way to pass secrets). A field that names another object takes its
name or ID:

  awxctl create hosts web1 --set inventory=lab --set 'variables={"ansible_host": "192.0.2.10"}'
  awxctl create job-templates "Patch" --set project=ops --set playbook=patch.yml --set inventory=lab
  awxctl create credentials deploy --set credential_type=Machine --set organization=Default --set-file inputs=inputs.yml`,
	Args: cobra.RangeArgs(1, 2),
	RunE: func(cmd *cobra.Command, args []string) error {
		r, err := kindArg(args[0])
		if err != nil {
			return err
		}
		values, err := readValues(cmd)
		if err != nil {
			return err
		}
		if len(args) == 2 {
			values[r.NameField] = args[1]
		}
		ctx, name, client, err := connect()
		if err != nil {
			return err
		}
		p, err := client.PlanCreate(cmd.Context(), r, values)
		if err != nil {
			return err
		}
		_, err = runPlan(cmd, ctx, name, client, p)
		return err
	},
}

var updateCmd = &cobra.Command{
	Use:   "update <kind> <name>... --set FIELD=VALUE",
	Short: "Change fields of existing objects; only fields that differ are sent",
	Args:  cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		r, err := kindArg(args[0])
		if err != nil {
			return err
		}
		refs, err := targets(cmd, args[1:])
		if err != nil {
			return err
		}
		values, err := readValues(cmd)
		if err != nil {
			return err
		}
		ctx, name, client, err := connect()
		if err != nil {
			return err
		}
		p, err := client.PlanUpdate(cmd.Context(), r, refs, scopeFlag(cmd, r), values)
		if err != nil {
			return err
		}
		_, err = runPlan(cmd, ctx, name, client, p)
		return err
	},
}

var deleteCmd = &cobra.Command{
	Use:   "delete <kind> <name>...",
	Short: "Delete objects; the dry run shows what else goes with them",
	Long: `Deletes objects. The dry run (the default) shows each object and what the delete
takes with it: the hosts and groups of an inventory, the schedules of a template.
With --apply the state before the delete is saved to the audit record first.
There is no undo: the record is what you would re-create it from.`,
	Args: cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		r, err := kindArg(args[0])
		if err != nil {
			return err
		}
		refs, err := targets(cmd, args[1:])
		if err != nil {
			return err
		}
		ctx, name, client, err := connect()
		if err != nil {
			return err
		}
		p, err := client.PlanDelete(cmd.Context(), r, refs, scopeFlag(cmd, r))
		if err != nil {
			return err
		}
		_, err = runPlan(cmd, ctx, name, client, p)
		return err
	},
}

func enableCmd(verb string, enabled bool) *cobra.Command {
	c := &cobra.Command{
		Use:   verb + " <hosts|schedules> <name>...",
		Short: strings.ToUpper(verb[:1]) + verb[1:] + " hosts (in or out of job runs) or schedules",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := kindArg(args[0])
			if err != nil {
				return err
			}
			if r.Kind != "hosts" && r.Kind != "schedules" {
				return &usageError{verb + " takes hosts or schedules"}
			}
			refs, err := targets(cmd, args[1:])
			if err != nil {
				return err
			}
			ctx, name, client, err := connect()
			if err != nil {
				return err
			}
			p, err := client.PlanSetEnabled(cmd.Context(), r, refs, scopeFlag(cmd, r), enabled)
			if err != nil {
				return err
			}
			_, err = runPlan(cmd, ctx, name, client, p)
			return err
		},
	}
	addBulkFlags(c)
	return c
}

func attachCmd(verb string, detach bool) *cobra.Command {
	c := &cobra.Command{
		Use:   verb + " <kind> <name>... --to KIND=NAME",
		Short: strings.ToUpper(verb[:1]) + verb[1:] + " objects: hosts and groups of a group, credentials and notifications of a job template",
		Long: "Possible pairs (parent <- what is attached): " + strings.Join(core.Relations(), ", ") + `.

  awxctl attach hosts web1 web2 --to groups=webservers --inventory lab
  awxctl detach credentials deploy --to "job-templates=Patch"
  awxctl attach notification-templates "Ops chat" --to "job-templates=Patch" --on error`,
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			child, err := kindArg(args[0])
			if err != nil {
				return err
			}
			toFlag, _ := cmd.Flags().GetString("to")
			toKind, toName, ok := strings.Cut(toFlag, "=")
			if !ok || toName == "" {
				return &usageError{"give the parent as --to KIND=NAME, e.g. --to groups=webservers"}
			}
			to := []string{toKind, toName}
			parent, err := kindArg(to[0])
			if err != nil {
				return err
			}
			refs, err := targets(cmd, args[1:])
			if err != nil {
				return err
			}
			ctx, name, client, err := connect()
			if err != nil {
				return err
			}
			var p *core.Plan
			if child.Kind == "notification-templates" {
				when, _ := cmd.Flags().GetString("on")
				if len(refs) != 1 {
					return &usageError{"attach or detach one notification template at a time"}
				}
				p, err = client.PlanNotify(cmd.Context(), parent, to[1], refs[0], when, scopeFlag(cmd, parent), detach)
			} else {
				p, err = client.PlanAttach(cmd.Context(), parent, to[1], child, refs, scopeFlag(cmd, parent), detach)
			}
			if err != nil {
				return err
			}
			_, err = runPlan(cmd, ctx, name, client, p)
			return err
		},
	}
	c.Flags().String("to", "", "the parent, as KIND=NAME: --to groups=webservers")
	c.Flags().String("on", "", "for notification-templates: started, success or error")
	addBulkFlags(c)
	return c
}

var runCmd = group(&cobra.Command{Use: "run", Short: "Run an ad hoc command on hosts"})

var adhoc core.AdHocOptions

var runCommandCmd = &cobra.Command{
	Use:   "command --inventory <inv> --limit <pattern> --credential <cred> --module <name> [--args <text>]",
	Short: "Run one module on hosts of an inventory (an ad hoc command); needs the write tier",
	Long: `Runs one Ansible module on the hosts a limit selects. This runs arbitrary
commands on real machines: it needs permissions: write, --limit is required, and it
is never offered over MCP.

  awxctl run command --inventory lab --limit web1 --credential deploy --module ping
  awxctl run command --inventory lab --limit web1 --credential deploy --module shell --args 'uptime' --apply --wait`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		for flag, v := range map[string]string{"inventory": adhoc.Inventory, "credential": adhoc.Credential, "module": adhoc.Module} {
			if v == "" {
				return &usageError{"--" + flag + " is required"}
			}
		}
		ctx, name, client, err := connect()
		if err != nil {
			return err
		}
		p, err := client.PlanAdHoc(cmd.Context(), adhoc)
		if err != nil {
			return err
		}
		return runAndMaybeWait(cmd, ctx, name, client, p)
	},
}

func addJobWaitFlags(c *cobra.Command) {
	c.Flags().BoolVar(&lWait, "wait", false, "with --apply: wait until the job ends; exit 0 only if it was successful")
	c.Flags().BoolVar(&lFollow, "follow", false, "with --apply: like --wait, and print the job output to stderr as it arrives")
	addWaitFlags(c)
}

func init() {
	pf := rootCmd.PersistentFlags()
	pf.BoolVar(&flagDryRun, "dry-run", false, "only show what a change would do, even if the context's mode is apply")
	pf.BoolVar(&flagApply, "apply", false, "execute state-changing commands (default is a dry run that changes nothing)")
	pf.BoolVarP(&flagYes, "yes", "y", false, "alias for --apply")

	launchCmd.Flags().Bool("workflow", false, "the template is a workflow template")
	launchCmd.Flags().StringVar(&lLimit, "limit", "", "host pattern to limit the run to")
	launchCmd.Flags().StringVar(&lTags, "tags", "", "job tags, comma-separated")
	launchCmd.Flags().StringVar(&lSkip, "skip-tags", "", "tags to skip, comma-separated")
	launchCmd.Flags().StringArrayVarP(&lVars, "extra-vars", "e", nil, "extra variable NAME=VALUE (repeatable), NAME:=JSON, or a JSON object; 1.10 stays the text 1.10")
	launchCmd.Flags().StringVar(&lVarsFile, "extra-vars-file", "", "YAML or JSON file with extra variables (- for stdin)")
	launchCmd.Flags().StringVar(&lInventory, "inventory", "", "inventory to run against, when the template prompts for it")
	launchCmd.Flags().BoolVar(&lCheck, "check", false, "launch in Ansible check mode (a real job that changes nothing on the hosts)")
	launchCmd.Flags().BoolVar(&lDiff, "diff", false, "launch with diff mode")
	launchCmd.Flags().StringVar(&lBranch, "scm-branch", "", "project branch to run from")
	launchCmd.Flags().IntVar(&lVerbosity, "verbosity", 0, "verbosity 0-5")
	addJobWaitFlags(launchCmd)
	relaunchCmd.Flags().String("hosts", "all", "all, or failed: only the hosts that failed")
	addJobWaitFlags(relaunchCmd)
	addBulkFlags(cancelCmd)
	addBulkFlags(syncCmd)
	addJobWaitFlags(syncCmd)
	for _, c := range []*cobra.Command{createCmd, updateCmd} {
		c.Flags().StringArray("set", nil, "FIELD=VALUE (repeatable): true/false/null, plain numbers, [..] and {..} are sent as such, the rest as text; FIELD:=JSON forces JSON")
		c.Flags().StringArray("set-file", nil, "FIELD=PATH: read the value from a file, - for stdin (repeatable)")
		c.Flags().String("file", "", "YAML or JSON mapping of fields (- for stdin)")
	}
	addBulkFlags(updateCmd)
	addBulkFlags(deleteCmd)
	runCommandCmd.Flags().StringVar(&adhoc.Inventory, "inventory", "", "inventory")
	runCommandCmd.Flags().StringVar(&adhoc.Limit, "limit", "", "host pattern (required; all = every host)")
	runCommandCmd.Flags().StringVar(&adhoc.Credential, "credential", "", "machine credential to log in with")
	runCommandCmd.Flags().StringVar(&adhoc.Module, "module", "", "module name, e.g. ping, command, shell, setup")
	runCommandCmd.Flags().StringVar(&adhoc.Args, "args", "", "module arguments")
	runCommandCmd.Flags().BoolVar(&adhoc.Become, "become", false, "run with privilege escalation")
	runCommandCmd.Flags().BoolVar(&adhoc.Check, "check", false, "run in check mode")
	runCommandCmd.Flags().IntVar(&adhoc.Forks, "forks", 0, "parallel hosts (0 = the default)")
	addJobWaitFlags(runCommandCmd)
	runCmd.AddCommand(runCommandCmd)

	rootCmd.AddCommand(launchCmd, relaunchCmd, cancelCmd, syncCmd, approvalCmd("approve", true), approvalCmd("deny", false),
		createCmd, updateCmd, deleteCmd, enableCmd("enable", true), enableCmd("disable", false), attachCmd("attach", false), attachCmd("detach", true), runCmd)
}

// maskArgs hides secret-looking values in the command line kept in an audit
// record: NAME=VALUE pairs and JSON objects given to --extra-vars or --set.
func maskArgs(args []string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = a
		body := a
		prefix := ""
		if strings.HasPrefix(a, "--") {
			if flag, val, ok := strings.Cut(a, "="); ok {
				prefix, body = flag+"=", val
			}
		}
		if strings.HasPrefix(strings.TrimSpace(body), "{") {
			var obj map[string]any
			if json.Unmarshal([]byte(body), &obj) == nil {
				if b, err := json.Marshal(core.Mask(obj)); err == nil {
					out[i] = prefix + string(b)
				}
			}
			continue
		}
		if k, v, ok := strings.Cut(body, "="); ok {
			if secretLooking(k) {
				out[i] = prefix + k + "=******"
			} else if m, isMap := core.Mask(map[string]any{k: v}).(map[string]any); isMap {
				out[i] = prefix + k + "=" + fmt.Sprint(m[k]) // variables={...}: masked inside
			}
		}
	}
	return out
}

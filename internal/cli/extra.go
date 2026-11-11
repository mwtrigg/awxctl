package cli

import (
	"fmt"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/mwtrigg/awxctl/internal/core"
	"github.com/mwtrigg/awxctl/internal/output"
)

// kindName splits a KIND=NAME flag value.
func kindName(flag, value string) (*core.Resource, string, error) {
	k, n, ok := strings.Cut(value, "=")
	if !ok || n == "" {
		return nil, "", &usageError{fmt.Sprintf("give --%s as KIND=NAME, e.g. --%s inventories=lab", flag, flag)}
	}
	r, err := kindArg(k)
	return r, n, err
}

func inventoryScope(cmd *cobra.Command) url.Values {
	q := url.Values{}
	if inv, _ := cmd.Flags().GetString("inventory"); inv != "" {
		q.Set("inventory__name", inv)
	}
	return q
}

// ---- tokens ----

var tokenCmd = group(&cobra.Command{Use: "token", Short: "Create API tokens (list and delete them with get tokens / delete tokens)"})

var tokenCreateCmd = &cobra.Command{
	Use:   "create --description <text> [--scope read|write] [--user <name>]",
	Short: "Create an API token for yourself, or as an administrator for another user",
	Long: `Creates an OAuth2 token. The token's value is shown once, in the result of
--apply, and nowhere else: not in the dry run, not in the audit record. With
--save it is written to a file (mode 0600) instead of being printed.

  awxctl token create --description "reporting agent" --scope read --apply --save token.txt
  awxctl get tokens --user ann
  awxctl delete tokens 12 --apply

On AAP 2.5 and newer tokens belong to the platform gateway; awxctl goes there.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		desc, _ := cmd.Flags().GetString("description")
		scope, _ := cmd.Flags().GetString("scope")
		user, _ := cmd.Flags().GetString("user")
		save, _ := cmd.Flags().GetString("save")
		ctx, name, client, err := connect()
		if err != nil {
			return err
		}
		p, err := client.PlanTokenCreate(cmd.Context(), user, desc, scope)
		if err != nil {
			return err
		}
		// The token must reach the user exactly once: print the plan result
		// ourselves instead of through runPlan's generic rows.
		applied, err := runPlanQuiet(cmd, ctx, name, client, p)
		if err != nil || !applied {
			return err
		}
		step := p.Steps[0]
		row := map[string]any{"id": step.Result["id"], "scope": step.Result["scope"], "expires": step.Result["expires"], "description": desc}
		secret, _ := step.Raw()["token"].(string)
		if save != "" {
			if err := writePrivate(save, []byte(secret+"\n")); err != nil {
				return fmt.Errorf("the token (id %v) was created but could not be saved: %w; delete it and try again", row["id"], err)
			}
			row["saved_to"] = save
		} else {
			row["token"] = secret
			fmt.Fprintln(os.Stderr, "this is the only time the token is shown: store it now")
		}
		return output.Print(output.Format(flagOutput), []output.Column{{Key: "id", Header: "ID"}, {Key: "scope", Header: "SCOPE"}, {Key: "expires", Header: "EXPIRES"}, {Key: "token", Header: "TOKEN"}, {Key: "saved_to", Header: "SAVED TO"}}, []map[string]any{row})
	},
}

// ---- role grants ----

func grantCmd(verb string, revoke bool) *cobra.Command {
	c := &cobra.Command{
		Use:   verb + " <role> (--user <name> | --team <name>) [--on KIND=NAME]",
		Short: map[bool]string{false: "Give a role to a user or a team, on one object or system-wide", true: "Take a role away from a user or a team"}[revoke],
		Long: `Roles are the role definitions (awxctl get role-definitions): for example
"JobTemplate Execute" on one job template, or "Organization Member" on an
organization. Roles can be granted on: ` + strings.Join(core.RoleKinds(), ", ") + `.

  awxctl ` + verb + ` "JobTemplate Execute" --user ann --on "job-templates=Patch servers"
  awxctl ` + verb + ` "Inventory Use" --team ops --on inventories=lab
  awxctl get access inventories lab

On AAP 2.5 and newer this is done on the platform gateway.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			user, _ := cmd.Flags().GetString("user")
			team, _ := cmd.Flags().GetString("team")
			var target *core.Resource
			ref := ""
			if on, _ := cmd.Flags().GetString("on"); on != "" {
				var err error
				if target, ref, err = kindName("on", on); err != nil {
					return err
				}
			}
			ctx, name, client, err := connect()
			if err != nil {
				return err
			}
			p, err := client.PlanGrant(cmd.Context(), args[0], user, team, target, ref, nil, revoke)
			if err != nil {
				return err
			}
			_, err = runPlan(cmd, ctx, name, client, p)
			return err
		},
	}
	c.Flags().String("user", "", "the user who gets (or loses) the role")
	c.Flags().String("team", "", "the team that gets (or loses) the role")
	c.Flags().String("on", "", "the object, as KIND=NAME; leave out for a system-wide role")
	return c
}

var accessCmd = &cobra.Command{
	Use:   "access <kind> <name>",
	Short: "List who has which role on an object (direct grants to users and teams)",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		r, err := kindArg(args[0])
		if err != nil {
			return err
		}
		_, _, client, err := connect()
		if err != nil {
			return err
		}
		rows, err := client.Access(cmd.Context(), r, args[1], nil)
		if err != nil {
			return err
		}
		return output.Print(output.Format(flagOutput), fieldColumns(core.AccessFields, true), rows)
	},
}

// ---- hosts, inventories, projects, schedules ----

var hostFactsCmd = &cobra.Command{
	Use:   "host-facts <host>",
	Short: "Show the facts last gathered for a host (--fact picks one, e.g. ansible_distribution)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		_, _, client, err := connect()
		if err != nil {
			return err
		}
		hosts := core.FindResource("hosts")
		h, err := client.Resolve(cmd.Context(), hosts, args[0], inventoryScope(cmd))
		if err != nil {
			return err
		}
		facts, err := client.Get(cmd.Context(), hosts.Path+strconv.Itoa(core.ID(h))+"/ansible_facts/")
		if err != nil {
			return err
		}
		if len(facts) == 0 {
			fmt.Fprintln(os.Stderr, "note: no facts are stored for this host (fact storage is a template setting, off by default)")
		}
		row := map[string]any{"host": h["name"], "host_id": core.ID(h), "facts_modified": h["ansible_facts_modified"], "facts": facts}
		if fact, _ := cmd.Flags().GetString("fact"); fact != "" {
			row = map[string]any{"host": h["name"], "fact": fact, "value": core.Lookup(facts, fact)}
		}
		return output.Print(output.Format(flagOutput), []output.Column{{Key: "host", Header: "HOST"}, {Key: "fact", Header: "FACT"}, {Key: "value", Header: "VALUE"}, {Key: "facts_modified", Header: "GATHERED"}}, []map[string]any{row})
	},
}

var variablesCmd = &cobra.Command{
	Use:   "variables <hosts|groups|inventories> <name>",
	Short: "Show an object's variables; --merged shows what a host ends up with and where each value comes from",
	Long: `Shows the variables set on a host, a group or an inventory.

  awxctl get variables hosts web1 --inventory lab
  awxctl get variables hosts web1 --inventory lab --merged

--merged (hosts only) computes what the host ends up with: inventory variables,
then its groups from the outermost in, then its own; sources says where each
final value came from. Values of secret-looking names are shown as ******
unless --show-secrets is given.`,
	Args: cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		r, err := kindArg(args[0])
		if err != nil {
			return err
		}
		if r.Kind != "hosts" && r.Kind != "groups" && r.Kind != "inventories" {
			return &usageError{"variables takes hosts, groups or inventories"}
		}
		merged, _ := cmd.Flags().GetBool("merged")
		show, _ := cmd.Flags().GetBool("show-secrets")
		if merged && r.Kind != "hosts" {
			return &usageError{"--merged applies to hosts"}
		}
		_, _, client, err := connect()
		if err != nil {
			return err
		}
		o, err := client.Resolve(cmd.Context(), r, args[1], scopeFlag(cmd, r))
		if err != nil {
			return err
		}
		row := map[string]any{"kind": r.Singular, "name": o["name"], "id": core.ID(o)}
		if merged {
			vars, sources, err := client.MergedVars(cmd.Context(), o, show)
			if err != nil {
				return err
			}
			row["variables"], row["sources"] = vars, sources
		} else {
			vars, err := client.Get(cmd.Context(), r.Path+strconv.Itoa(core.ID(o))+"/variable_data/")
			if err != nil {
				return err
			}
			if !show {
				vars = core.Mask(vars).(map[string]any)
			}
			row["variables"] = vars
		}
		return output.Print(output.Format(flagOutput), []output.Column{{Key: "kind", Header: "KIND"}, {Key: "name", Header: "NAME"}, {Key: "variables", Header: "VARIABLES"}}, []map[string]any{row})
	},
}

var projectFilesCmd = &cobra.Command{
	Use:   "project-files <project>",
	Short: "List the playbooks and inventory files of a project (as of its last sync)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		_, _, client, err := connect()
		if err != nil {
			return err
		}
		r := core.FindResource("projects")
		p, err := client.Resolve(cmd.Context(), r, args[0], nil)
		if err != nil {
			return err
		}
		rows := []map[string]any{}
		for _, kind := range []struct{ sub, typ string }{{"playbooks", "playbook"}, {"inventories", "inventory"}} {
			var files []string
			if err := client.GetAny(cmd.Context(), r.Path+strconv.Itoa(core.ID(p))+"/"+kind.sub+"/", &files); err != nil {
				return err
			}
			sort.Strings(files)
			for _, f := range files {
				rows = append(rows, map[string]any{"type": kind.typ, "path": f, "project": p["name"], "scm_revision": p["scm_revision"]})
			}
		}
		return output.Print(output.Format(flagOutput), []output.Column{{Key: "type", Header: "TYPE"}, {Key: "path", Header: "PATH"}}, rows)
	},
}

var scheduleRunsCmd = &cobra.Command{
	Use:   "schedule-runs <schedule> | --rrule <rule>",
	Short: "Show the next runs of a schedule, or of a recurrence rule before you save it",
	Long: `Asks the controller when a schedule runs next (up to ten times), in UTC and in
the schedule's own time zone.

  awxctl get schedule-runs nightly
  awxctl get schedule-runs --rrule 'DTSTART;TZID=America/New_York:20300101T020000 RRULE:FREQ=WEEKLY;INTERVAL=1;BYDAY=MO'`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		rule, _ := cmd.Flags().GetString("rrule")
		if (len(args) == 1) == (rule != "") {
			return &usageError{"give a schedule, or --rrule with a rule"}
		}
		_, _, client, err := connect()
		if err != nil {
			return err
		}
		name := any(nil)
		if len(args) == 1 {
			s, err := client.Resolve(cmd.Context(), core.FindResource("schedules"), args[0], nil)
			if err != nil {
				return err
			}
			rule, _ = s["rrule"].(string)
			name = s["name"]
			if s["enabled"] == false {
				fmt.Fprintln(os.Stderr, "note: this schedule is disabled: these runs will not happen until it is enabled")
			}
		}
		utc, local, err := client.SchedulePreview(cmd.Context(), rule)
		if err != nil {
			return err
		}
		rows := make([]map[string]any, len(utc))
		for i := range utc {
			rows[i] = map[string]any{"n": i + 1, "utc": utc[i], "schedule": name}
			if i < len(local) {
				rows[i]["local"] = local[i]
			}
		}
		return output.Print(output.Format(flagOutput), []output.Column{{Key: "n", Header: "N"}, {Key: "utc", Header: "UTC"}, {Key: "local", Header: "SCHEDULE'S ZONE"}}, rows)
	},
}

// ---- copy ----

var copyCmd = &cobra.Command{
	Use:   "copy <kind> <name> --name <new name>",
	Short: "Copy a job template, workflow template, inventory, project, credential, notification template or execution environment",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		r, err := kindArg(args[0])
		if err != nil {
			return err
		}
		newName, _ := cmd.Flags().GetString("name")
		ctx, name, client, err := connect()
		if err != nil {
			return err
		}
		p, err := client.PlanCopy(cmd.Context(), r, args[1], newName, nil)
		if err != nil {
			return err
		}
		_, err = runPlan(cmd, ctx, name, client, p)
		return err
	},
}

// ---- surveys ----

var surveyGetCmd = &cobra.Command{
	Use:   "survey <template>",
	Short: "Show a template's survey in the short form `set survey` takes (--save writes it to a file to edit)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		_, _, client, err := connect()
		if err != nil {
			return err
		}
		r, tmpl, err := findTemplate(cmd, client, args[0])
		if err != nil {
			return err
		}
		s, err := client.Survey(cmd.Context(), r, tmpl)
		if err != nil {
			return err
		}
		if save, _ := cmd.Flags().GetString("save"); save != "" {
			data, err := yaml.Marshal(map[string]any{"questions": s["questions"]})
			if err != nil {
				return err
			}
			if err := writePrivate(save, data); err != nil {
				return err
			}
			s["saved_to"] = save
		}
		return output.Print(output.Format(flagOutput), []output.Column{{Key: "template", Header: "TEMPLATE"}, {Key: "enabled", Header: "ENABLED"}, {Key: "questions", Header: "QUESTIONS"}}, []map[string]any{s})
	},
}

var setCmd = group(&cobra.Command{Use: "set", Short: "Replace a template's survey or a workflow's graph from a file"})

var setSurveyCmd = &cobra.Command{
	Use:   "survey <template> (--file <questions.yml> | --remove)",
	Short: "Replace a template's survey with the questions in a file and switch it on, or remove it",
	Long: `The file is a YAML or JSON mapping with a list of questions:

  questions:
    - variable: release          # required: the extra variable the answer becomes
      question: Which release?   # shown to the person launching (default: the variable)
      type: text                 # text, textarea, password, integer, float, multiplechoice, multiselect
      required: true             # default true
      default: "1.0"
    - variable: env
      type: multiplechoice
      choices: [dev, prod]

Start from the current survey: awxctl get survey <template> --save questions.yml
The whole survey is replaced; the dry run shows the old one next to the new.
A password question's default is never shown back: write it again when you
replace a survey that has one.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		remove, _ := cmd.Flags().GetBool("remove")
		file, _ := cmd.Flags().GetString("file")
		if remove == (file != "") {
			return &usageError{"give --file with the questions, or --remove"}
		}
		var doc struct {
			Questions []map[string]any `yaml:"questions"`
		}
		if file != "" {
			data, err := readFileOrStdin(file)
			if err != nil {
				return err
			}
			if err := yaml.Unmarshal(data, &doc); err != nil {
				return &usageError{"--file: " + err.Error()}
			}
		}
		ctx, name, client, err := connect()
		if err != nil {
			return err
		}
		kind := "job-templates"
		if wf, _ := cmd.Flags().GetBool("workflow"); wf {
			kind = "workflow-templates"
		}
		p, err := client.PlanSurveySet(cmd.Context(), core.FindResource(kind), args[0], doc.Questions, remove)
		if err != nil {
			return err
		}
		_, err = runPlan(cmd, ctx, name, client, p)
		return err
	},
}

// ---- workflow graph ----

var workflowGraphCmd = &cobra.Command{
	Use:   "workflow-graph <workflow template>",
	Short: "Show a workflow's nodes and links in the short form `set workflow-graph` takes (--save writes a file to edit)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		_, _, client, err := connect()
		if err != nil {
			return err
		}
		wf, err := client.Resolve(cmd.Context(), core.FindResource("workflow-templates"), args[0], nil)
		if err != nil {
			return err
		}
		nodes, err := client.ReadGraph(cmd.Context(), wf)
		if err != nil {
			return err
		}
		if save, _ := cmd.Flags().GetString("save"); save != "" {
			data, err := yaml.Marshal(map[string]any{"nodes": nodes})
			if err != nil {
				return err
			}
			if err := writePrivate(save, data); err != nil {
				return err
			}
			fmt.Fprintln(os.Stderr, "saved to", save)
		}
		outFmt := output.Format(flagOutput)
		if outFmt == output.FormatTable {
			rows := make([]map[string]any, len(nodes))
			for i, n := range nodes {
				what := n.Template
				if n.Approval != nil {
					what = "approval: " + n.Approval.Name
				}
				rows[i] = map[string]any{"id": n.ID, "runs": what, "type": n.Type, "on_success": strings.Join(n.OnSuccess, ","), "on_failure": strings.Join(n.OnFailure, ","), "always": strings.Join(n.Always, ",")}
			}
			return output.Print(outFmt, []output.Column{{Key: "id", Header: "ID"}, {Key: "runs", Header: "RUNS"}, {Key: "type", Header: "TYPE"}, {Key: "on_success", Header: "ON SUCCESS"}, {Key: "on_failure", Header: "ON FAILURE"}, {Key: "always", Header: "ALWAYS"}}, rows)
		}
		return output.Print(outFmt, nil, []map[string]any{{"workflow": wf["name"], "id": core.ID(wf), "nodes": nodes}})
	},
}

var setWorkflowGraphCmd = &cobra.Command{
	Use:   "workflow-graph <workflow template> --file <graph.yml>",
	Short: "Make a workflow's nodes and links match a file: awxctl works out what to create, change, link and delete",
	Long: `The file describes the whole graph. Each node has an id you choose, what it
runs, and which nodes follow it:

  nodes:
    - id: build
      template: Build image          # a job template, workflow, project or inventory source, by name
      on_success: [approve]
      on_failure: [notify]
    - id: approve
      approval:
        name: Release this build
        timeout: 3600              # seconds; 0 waits for ever
      on_success: [deploy]
    - id: deploy
      template: Deploy
      limit: web
    - id: notify
      template: Send alert

Optional per node: type (job, workflow, project, inventory-source: only needed when
two templates share a name), always: [...], all_parents_must_converge: true,
limit, extra_data: {...}.

Nodes are matched by id: a node that keeps its id is updated in place, a node
missing from the file is deleted, a new id is created. Start from the current
graph (awxctl get workflow-graph <template> --save graph.yml), edit, then run
this without --apply to see every step it would take.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		file, _ := cmd.Flags().GetString("file")
		if file == "" {
			return &usageError{"give the graph with --file"}
		}
		data, err := readFileOrStdin(file)
		if err != nil {
			return err
		}
		var doc struct {
			Nodes []core.GraphNode `yaml:"nodes"`
		}
		dec := yaml.NewDecoder(strings.NewReader(string(data)))
		dec.KnownFields(true)
		if err := dec.Decode(&doc); err != nil {
			return &usageError{"--file: " + err.Error()}
		}
		ctx, name, client, err := connect()
		if err != nil {
			return err
		}
		p, err := client.PlanGraph(cmd.Context(), args[0], doc.Nodes)
		if err != nil {
			return err
		}
		_, err = runPlan(cmd, ctx, name, client, p)
		return err
	},
}

func init() {
	tokenCreateCmd.Flags().String("description", "", "what the token is for (required)")
	tokenCreateCmd.Flags().String("scope", "write", "read or write")
	tokenCreateCmd.Flags().String("user", "", "create it for this user (needs an administrator)")
	tokenCreateCmd.Flags().String("save", "", "write the token to this file (mode 0600) instead of printing it")
	tokenCmd.AddCommand(tokenCreateCmd)

	hostFactsCmd.Flags().String("inventory", "", "the host's inventory, when the name exists in more than one")
	hostFactsCmd.Flags().String("fact", "", "only this fact, as a dotted path: ansible_default_ipv4.address")
	variablesCmd.Flags().String("inventory", "", "for hosts and groups: the inventory the name is in")
	variablesCmd.Flags().Bool("merged", false, "hosts: the variables the host ends up with, and where each comes from")
	variablesCmd.Flags().Bool("show-secrets", false, "do not mask secret-looking values")
	scheduleRunsCmd.Flags().String("rrule", "", "a recurrence rule to preview instead of a saved schedule")
	copyCmd.Flags().String("name", "", "the name of the copy (required)")
	surveyGetCmd.Flags().Bool("workflow", false, "the template is a workflow template")
	surveyGetCmd.Flags().String("save", "", "also write the questions to this YAML file, ready to edit")
	setSurveyCmd.Flags().Bool("workflow", false, "the template is a workflow template")
	setSurveyCmd.Flags().String("file", "", "YAML or JSON file with the questions (- for stdin)")
	setSurveyCmd.Flags().Bool("remove", false, "remove the survey and switch it off")
	workflowGraphCmd.Flags().String("save", "", "also write the graph to this YAML file, ready to edit")
	setWorkflowGraphCmd.Flags().String("file", "", "YAML or JSON file with the graph (- for stdin)")
	setCmd.AddCommand(setSurveyCmd, setWorkflowGraphCmd)

	getCmd.AddCommand(accessCmd, hostFactsCmd, variablesCmd, projectFilesCmd, scheduleRunsCmd, surveyGetCmd, workflowGraphCmd)
	rootCmd.AddCommand(tokenCmd, grantCmd("grant", false), grantCmd("revoke", true), copyCmd, setCmd)
}

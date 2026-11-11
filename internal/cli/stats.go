package cli

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/mwtrigg/awxctl/internal/core"
	"github.com/mwtrigg/awxctl/internal/output"
)

var statsCmd = group(&cobra.Command{Use: "stats", Short: "Figures over many runs: runtimes and failure rates per template, jobs per host"})

var statsJobsCmd = &cobra.Command{
	Use:   "jobs",
	Short: "Runs, failure rate and runtimes (min, average, median, p95, max) per template, day, status, ...",
	Long: `Reads the jobs of a time window (default: the last 7 days) and summarizes them
per group. Runtimes are in seconds and count finished runs only.

The controller has no summary of its own, so every job in the window is read:
on a busy controller that is roughly 50 jobs a second. Narrow it with --template
or a shorter --since when it is slow; --max-jobs (default 2000) bounds it, and a
note says when the window held more.

  awxctl stats jobs                                   # one row per template, last 7 days
  awxctl stats jobs --template "Patch servers" --by day
  awxctl stats jobs --since 7d --by launched_by

--by takes template, day, status, inventory, launched_by, project or limit. All runs
(workflows, syncs too) with --all-types.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		by, _ := cmd.Flags().GetString("by")
		since, _ := cmd.Flags().GetString("since")
		until, _ := cmd.Flags().GetString("until")
		template, _ := cmd.Flags().GetString("template")
		inventory, _ := cmd.Flags().GetString("inventory")
		allTypes, _ := cmd.Flags().GetBool("all-types")
		max, _ := cmd.Flags().GetInt("max-jobs")
		r := core.FindResource("jobs")
		tmplParam := "job_template__name"
		if allTypes {
			r = core.FindResource("unified-jobs")
			tmplParam = "unified_job_template__name"
			if by == "template" {
				by = "name"
			}
		}
		known := by == "day"
		for _, fd := range r.Fields {
			known = known || fd.Key == by
		}
		if !known {
			return &usageError{fmt.Sprintf("--by %s: use template, day, status, inventory, launched_by, project or limit", by)}
		}
		q := url.Values{"order_by": {"-id"}}
		from, err := parseWhen(since, time.Now())
		if err != nil {
			return err
		}
		q.Set("created__gte", from)
		if until != "" {
			to, err := parseWhen(until, time.Now())
			if err != nil {
				return err
			}
			q.Set("created__lte", to)
		}
		if template != "" {
			q.Set(tmplParam, template)
		}
		if inventory != "" {
			if allTypes {
				return &usageError{"--inventory does not combine with --all-types"}
			}
			q.Set("inventory__name", inventory)
		}
		_, _, client, err := connect()
		if err != nil {
			return err
		}
		objs, total, err := client.List(cmd.Context(), r.Path, q, max)
		if err != nil {
			return err
		}
		if total > len(objs) {
			fmt.Fprintf(os.Stderr, "note: the figures cover the newest %d of %d runs in the window; raise --max-jobs or shorten --since\n", len(objs), total)
		}
		rows := make([]map[string]any, len(objs))
		for i, o := range objs {
			rows[i] = r.Row(o)
		}
		groups := core.GroupRuns(rows, by)
		out := make([]map[string]any, len(groups))
		for i, g := range groups {
			out[i] = g.Row(by)
			out[i]["window_from"] = from
		}
		cols := []output.Column{{Key: by, Header: strings.ToUpper(by)}, {Key: "runs", Header: "RUNS"}, {Key: "failed", Header: "FAILED"}, {Key: "failure_rate", Header: "FAIL RATE"},
			{Key: "seconds_avg", Header: "AVG S"}, {Key: "seconds_median", Header: "MEDIAN S"}, {Key: "seconds_p95", Header: "P95 S"}, {Key: "seconds_max", Header: "MAX S"},
			{Key: "last_status", Header: "LAST"}, {Key: "last_id", Header: "LAST ID"}}
		return output.Print(output.Format(flagOutput), cols, out)
	},
}

var statsHostsCmd = &cobra.Command{
	Use:   "hosts --inventory <inventory>",
	Short: "Per host of an inventory: how many jobs ran against it, how many it failed in, the last one",
	Long: `Counts, for every host of an inventory, the jobs that ran against it in a time
window (default: the last 30 days): jobs, those the host failed in, those it was
unreachable in, those that changed it, and its last job.

  awxctl stats hosts --inventory lab --since 7d
  awxctl stats hosts --inventory lab --filter-failed --field host

It asks the controller once per host: --max-hosts (default 200) bounds it.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		inventory, _ := cmd.Flags().GetString("inventory")
		since, _ := cmd.Flags().GetString("since")
		max, _ := cmd.Flags().GetInt("max-hosts")
		onlyFailed, _ := cmd.Flags().GetBool("filter-failed")
		if inventory == "" {
			return &usageError{"--inventory is required"}
		}
		from, err := parseWhen(since, time.Now())
		if err != nil {
			return err
		}
		_, _, client, err := connect()
		if err != nil {
			return err
		}
		inv, err := client.Resolve(cmd.Context(), core.FindResource("inventories"), inventory, nil)
		if err != nil {
			return err
		}
		rows, truncated, err := client.HostStats(cmd.Context(), inv, from, max)
		if err != nil {
			return err
		}
		if truncated {
			fmt.Fprintf(os.Stderr, "note: only the first %d hosts (by name) were counted; raise --max-hosts\n", max)
		}
		if onlyFailed {
			kept := rows[:0]
			for _, r := range rows {
				if r["failed"].(int) > 0 {
					kept = append(kept, r)
				}
			}
			rows = kept
		}
		if rows == nil {
			rows = []map[string]any{}
		}
		cols := []output.Column{{Key: "host", Header: "HOST"}, {Key: "jobs", Header: "JOBS"}, {Key: "failed", Header: "FAILED"}, {Key: "unreachable", Header: "UNREACHABLE"},
			{Key: "changed", Header: "CHANGED"}, {Key: "templates", Header: "TEMPLATES"}, {Key: "last_job_id", Header: "LAST JOB"}, {Key: "last_job_status", Header: "LAST STATUS"}, {Key: "last_job", Header: "WHEN"}}
		return output.Print(output.Format(flagOutput), cols, rows)
	},
}

var hostOutputCmd = &cobra.Command{
	Use:   "host-output <host> [--job <id> | --template <name>]",
	Short: "Show what a job printed for one host only: its tasks and results, without the other hosts",
	Long: `Picks a job (the newest that ran against the host, the newest of one template,
or the one you name) and returns only this host's part of its output: each task
the host took part in, with the host's result.

  awxctl get host-output web1                       # the last job that touched web1
  awxctl get host-output web1 --template "Patch servers" --failed
  awxctl get host-output web1 --job 1234 --grep 'changed|fatal' --text

--failed keeps only failed results (and, without --job, picks the newest job the
host failed in). --grep, --tail and --head narrow the text further.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		jobFlag, _ := cmd.Flags().GetInt("job")
		template, _ := cmd.Flags().GetString("template")
		failed, _ := cmd.Flags().GetBool("failed")
		grep, _ := cmd.Flags().GetString("grep")
		tail, _ := cmd.Flags().GetInt("tail")
		head, _ := cmd.Flags().GetInt("head")
		context, _ := cmd.Flags().GetInt("context")
		text, _ := cmd.Flags().GetBool("text")
		if jobFlag != 0 && template != "" {
			return &usageError{"give --job or --template, not both"}
		}
		filter := core.LineFilter{Head: head, Tail: tail, Context: context}
		if grep != "" {
			var err error
			if filter.Grep, err = regexp.Compile(grep); err != nil {
				return &usageError{"--grep: " + err.Error()}
			}
		}
		_, _, client, err := connect()
		if err != nil {
			return err
		}
		// A job's output is found by the host's name. Only "the newest job" needs the
		// host itself, and a name shared by hosts of several inventories means the
		// newest job of any of them.
		name := strings.TrimPrefix(args[0], "name:")
		id := jobFlag
		if id == 0 || strings.HasPrefix(args[0], "id:") {
			found, err := client.ResolveAll(cmd.Context(), core.FindResource("hosts"), args[0], inventoryScope(cmd))
			if err != nil {
				return err
			}
			name, _ = found[0]["name"].(string)
			if id == 0 {
				if id, err = client.LatestHostJob(cmd.Context(), found, template, failed); err != nil {
					return err
				}
			}
		}
		j, err := client.FindJob(cmd.Context(), id)
		if err != nil {
			return err
		}
		if j.Type != "job" {
			return &usageError{fmt.Sprintf("job %d is a %s: per-host output exists for playbook jobs only", id, strings.ReplaceAll(j.Type, "_", " "))}
		}
		out, tasks, err := client.HostOutput(cmd.Context(), id, name, failed)
		if err != nil {
			return err
		}
		var b strings.Builder
		lines := filter.Apply(out)
		for _, l := range lines {
			b.WriteString(l.Text)
			b.WriteByte('\n')
		}
		if text {
			_, err := os.Stdout.WriteString(b.String())
			return err
		}
		row := map[string]any{"host": name, "job_id": id, "job_name": j.Obj["name"], "job_status": j.Status(), "job_seconds": j.Obj["elapsed"],
			"started": j.Obj["started"], "tasks": tasks, "lines": len(lines), "text": b.String()}
		if tasks == 0 {
			row["note"] = "the job has no task results for this host (it was not in the run, or was skipped by the limit)"
		}
		return output.Print(output.Format(flagOutput), []output.Column{{Key: "text", Header: "OUTPUT"}}, []map[string]any{row})
	},
}

// ---- doctor ----

// testedVersions are the controller versions awxctl has been run against.
var testedVersions = map[core.Flavor][]string{core.FlavorAWX: {"24.6"}, core.FlavorAAP: {"4.8"}}

var doctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Check that this controller serves everything awxctl uses, and report what is missing or different",
	Long: `Reads every API path awxctl depends on and reports each as ok, missing, denied
or error. It sends GET requests only, unless --with-preview adds the schedule
preview check, which is a POST that changes nothing. Run it against a new controller version before trusting
awxctl there, or when a command fails in a way that looks like an API change.

Exit 0 if every check is ok, 1 otherwise. "denied" usually means the token may not
see that kind, not that the API changed.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		_, _, client, err := connect()
		if err != nil {
			return err
		}
		ctx := cmd.Context()
		rows := []map[string]any{}
		bad := 0
		add := func(check, path string, err error, detail string) {
			status := "ok"
			switch {
			case err == nil:
			case errors.Is(err, core.ErrNotFound):
				status = "missing"
			case errors.Is(err, core.ErrDenied):
				status = "denied"
			default:
				status = "error"
			}
			if err != nil {
				detail = err.Error()
				bad++
			}
			rows = append(rows, map[string]any{"check": check, "path": path, "status": status, "detail": detail})
		}
		info, err := client.Detect(ctx)
		if err != nil {
			return err
		}
		tested := false
		for _, v := range testedVersions[info.Flavor] {
			tested = tested || strings.HasPrefix(info.Version, v+".") || info.Version == v
		}
		verdict := "a version awxctl is tested with"
		if !tested {
			verdict = "NOT a version awxctl is tested with (tested: " + strings.Join(testedVersions[info.Flavor], ", ") + "): read the results below"
		}
		gw := "none (AWX: the controller serves identity and access itself)"
		if info.GatewayPath != "" {
			gw = info.GatewayPath
		}
		rows = append(rows, map[string]any{"check": "flavor and version", "path": info.APIPath, "status": "ok",
			"detail": fmt.Sprintf("%s, controller %s, %s; gateway: %s", info.Flavor, info.Version, verdict, gw)})
		_, err = client.Me(ctx)
		add("who am I", "/me/", err, "")

		first := map[string]core.Object{} // one object per kind, for the sub-paths
		for _, r := range core.Resources() {
			objs, _, err := client.List(ctx, r.Path, nil, 1)
			add("list "+r.Kind, r.Path, err, "")
			if err == nil && len(objs) > 0 {
				first[r.Kind] = objs[0]
			}
		}
		sub := func(check, kind, suffix string) {
			o := first[kind]
			if o == nil {
				rows = append(rows, map[string]any{"check": check, "path": core.FindResource(kind).Path + "<id>" + suffix, "status": "skipped", "detail": "no " + core.FindResource(kind).Singular + " exists to try it on"})
				return
			}
			path := core.FindResource(kind).Path + fmt.Sprint(core.ID(o)) + suffix
			var body interface{}
			add(check, path, client.GetAny(ctx, path, &body), "")
		}
		sub("job output", "jobs", "/stdout/?format=json&content_format=txt&start_line=0&end_line=1")
		sub("job events", "jobs", "/job_events/?page_size=1")
		sub("job host results", "jobs", "/job_host_summaries/?page_size=1")
		sub("relaunch info", "jobs", "/relaunch/")
		sub("cancel info", "jobs", "/cancel/")
		sub("workflow job nodes", "workflow-jobs", "/workflow_nodes/?page_size=1")
		sub("launch info", "job-templates", "/launch/")
		sub("survey", "job-templates", "/survey_spec/")
		sub("template credentials", "job-templates", "/credentials/?page_size=1")
		sub("template notifications", "job-templates", "/notification_templates_error/?page_size=1")
		sub("copy info", "job-templates", "/copy/")
		sub("workflow launch info", "workflow-templates", "/launch/")
		sub("workflow template nodes", "workflow-templates", "/workflow_nodes/?page_size=1")
		sub("host job history", "hosts", "/job_host_summaries/?page_size=1")
		sub("host facts", "hosts", "/ansible_facts/")
		sub("host variables", "hosts", "/variable_data/")
		sub("inventory variables", "inventories", "/variable_data/")
		sub("inventory graph", "inventories", "/script/?hostvars=1&all=1")
		sub("inventory hosts", "inventories", "/hosts/?page_size=1")
		sub("project sync info", "projects", "/update/")
		sub("project playbooks", "projects", "/playbooks/")
		sub("project inventory files", "projects", "/inventories/")
		sub("group hosts", "groups", "/all_hosts/?page_size=1")
		var body interface{}
		add("all runs", "/unified_jobs/", client.GetAny(ctx, "/unified_jobs/?page_size=1", &body), "")
		add("all templates", "/unified_job_templates/", client.GetAny(ctx, "/unified_job_templates/?page_size=1", &body), "")
		if preview, _ := cmd.Flags().GetBool("with-preview"); preview {
			_, _, err = client.SchedulePreview(ctx, "DTSTART:20300101T000000Z RRULE:FREQ=DAILY;INTERVAL=1;COUNT=1")
			add("schedule preview", "/schedules/preview/", err, "")
		} else {
			rows = append(rows, map[string]any{"check": "schedule preview", "path": "/schedules/preview/", "status": "skipped", "detail": "it is a POST (that changes nothing): add --with-preview to include it"})
		}

		outFmt := output.Format(flagOutput)
		if err := output.Print(outFmt, []output.Column{{Key: "check", Header: "CHECK"}, {Key: "status", Header: "STATUS"}, {Key: "path", Header: "PATH"}, {Key: "detail", Header: "DETAIL"}}, rows); err != nil {
			return err
		}
		if bad > 0 {
			return cliPrintErr(outFmt, fmt.Sprintf("%d of %d checks are not ok", bad, len(rows)), output.ExitError)
		}
		if !tested {
			fmt.Fprintln(os.Stderr, "every check is ok, but this version is not one awxctl is tested with")
		}
		return nil
	},
}

func init() {
	statsJobsCmd.Flags().String("by", "template", "group by: template, day, status, inventory, launched_by, project, limit")
	statsJobsCmd.Flags().String("since", "7d", "window start: 24h, 7d, 30d, a date or RFC 3339")
	statsJobsCmd.Flags().String("until", "", "window end (same forms)")
	statsJobsCmd.Flags().String("template", "", "only runs of this template")
	statsJobsCmd.Flags().String("inventory", "", "only runs against this inventory")
	statsJobsCmd.Flags().Bool("all-types", false, "every kind of run (workflows, syncs, system jobs), grouped by name")
	statsJobsCmd.Flags().Int("max-jobs", 2000, "most runs to read (newest first); a busy controller serves about 50 job rows a second")
	statsHostsCmd.Flags().String("inventory", "", "the inventory (required)")
	statsHostsCmd.Flags().String("since", "30d", "window start: 24h, 7d, 30d, a date or RFC 3339")
	statsHostsCmd.Flags().Int("max-hosts", 200, "most hosts to count (one request per host)")
	statsHostsCmd.Flags().Bool("filter-failed", false, "only hosts that failed in at least one job")
	statsCmd.AddCommand(statsJobsCmd, statsHostsCmd)

	hostOutputCmd.Flags().Int("job", 0, "this job (default: the newest that ran against the host)")
	hostOutputCmd.Flags().String("template", "", "the newest job of this job template")
	hostOutputCmd.Flags().Bool("failed", false, "only failed results; without --job, the newest job the host failed in")
	hostOutputCmd.Flags().String("inventory", "", "the host's inventory, when the name exists in more than one")
	hostOutputCmd.Flags().String("grep", "", "only lines matching this regular expression")
	hostOutputCmd.Flags().Int("context", 0, "with --grep: this many lines around each match")
	hostOutputCmd.Flags().Int("tail", 0, "only the last N lines")
	hostOutputCmd.Flags().Int("head", 0, "only the first N lines")
	hostOutputCmd.Flags().Bool("text", false, "print only the text, not JSON")
	getCmd.AddCommand(hostOutputCmd)
	doctorCmd.Flags().Bool("with-preview", false, "also check the schedule preview (a POST that changes nothing; a read-scope token is refused)")
	rootCmd.AddCommand(statsCmd, doctorCmd)
}

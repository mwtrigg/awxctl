package cli

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/mwtrigg/awxctl/internal/core"
	"github.com/mwtrigg/awxctl/internal/output"
)

// group makes a command that only holds subcommands fail on a wrong one instead
// of printing help and exiting 0.
func group(c *cobra.Command) *cobra.Command {
	c.Args = cobra.ArbitraryArgs
	c.RunE = func(cmd *cobra.Command, args []string) error {
		if len(args) > 0 {
			return &usageError{fmt.Sprintf("unknown command %q for %q: see %s --help", args[0], cmd.CommandPath(), cmd.CommandPath())}
		}
		return cmd.Help()
	}
	return c
}

func jobID(s string) (int, error) {
	id, err := strconv.Atoi(strings.TrimPrefix(s, "id:"))
	if err != nil || id <= 0 {
		return 0, &usageError{fmt.Sprintf("%q is not a job ID (a positive number)", s)}
	}
	return id, nil
}

func fieldColumns(fields []core.Field, wide bool) []output.Column {
	return columns(&core.Resource{Fields: fields}, wide)
}

var (
	outTail, outHead, outContext, outStart int
	outGrep, outSave                       string
	outText, outFollow                     bool
	waitTimeout                            time.Duration
	waitEvery                              time.Duration
)

var jobOutputCmd = &cobra.Command{
	Use:   "job-output <job-id>",
	Short: "Show the output of a job, a sync or an ad hoc command, or part of it",
	Long: `Reads the output (stdout) of a run. Narrow it before it reaches you:

  awxctl get job-output 1234 --tail 40
  awxctl get job-output 1234 --grep 'fatal|FAILED' --context 3
  awxctl get job-output 1234 --save out.txt      # whole output to a file, not printed
  awxctl get job-output 1234 --follow --text     # print as it arrives, until the job ends

The JSON result has the text plus line numbers; --text prints only the text.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id, err := jobID(args[0])
		if err != nil {
			return err
		}
		_, _, client, err := connect()
		if err != nil {
			return err
		}
		filter := core.LineFilter{Head: outHead, Tail: outTail, Context: outContext}
		if outGrep != "" {
			if filter.Grep, err = regexp.Compile(outGrep); err != nil {
				return &usageError{"--grep: " + err.Error()}
			}
		}
		j, err := client.FindJob(cmd.Context(), id)
		if err != nil {
			return err
		}
		if outFollow {
			if !outText {
				return &usageError{"--follow prints text as it arrives: use it with --text"}
			}
			j, err = followOutput(cmd.Context(), client, j, os.Stdout, outStart)
			if err != nil {
				return err
			}
			if j.Status() != "successful" {
				return cliPrintErr(output.Format(flagOutput), fmt.Sprintf("job %d ended %s", j.ID, j.Status()), output.ExitError)
			}
			return nil
		}
		text, next, err := client.Output(cmd.Context(), j, outStart)
		if err != nil {
			return err
		}
		row := map[string]any{"job_id": j.ID, "type": j.Type, "status": j.Status(), "finished": core.Finished(j.Status()), "next_line": next}
		if outSave != "" {
			if err := writePrivate(outSave, []byte(text)); err != nil {
				return err
			}
			row["saved_to"], row["bytes"] = outSave, len(text)
			row["lines"] = strings.Count(text, "\n")
			return output.Print(output.Format(flagOutput), []output.Column{{Key: "job_id", Header: "JOB"}, {Key: "status", Header: "STATUS"}, {Key: "lines", Header: "LINES"}, {Key: "saved_to", Header: "SAVED TO"}}, []map[string]any{row})
		}
		lines := filter.Apply(text)
		var b strings.Builder
		for _, l := range lines {
			if filter.Grep != nil {
				fmt.Fprintf(&b, "%d: ", l.N+outStart)
			}
			b.WriteString(l.Text)
			b.WriteByte('\n')
		}
		if outText {
			_, err := os.Stdout.WriteString(b.String())
			return err
		}
		row["text"], row["lines"], row["total_lines"] = b.String(), len(lines), strings.Count(text, "\n")
		return output.Print(output.Format(flagOutput), []output.Column{{Key: "text", Header: "OUTPUT"}}, []map[string]any{row})
	},
}

// largeOutputPause is the extra wait between reads of an output that has to be
// downloaded whole each time (a var so a test can shorten it).
var largeOutputPause = 15 * time.Second

// followOutput prints new output lines until the run ends.
func followOutput(ctx context.Context, client *core.Client, j *core.Job, w *os.File, start int) (*core.Job, error) {
	if j.Type == "workflow_job" {
		return client.Wait(ctx, j, waitEvery, nil)
	}
	next := start
	emit := func(job *core.Job) error {
		text, n, err := client.Output(ctx, job, next)
		if err != nil {
			return err
		}
		next = n
		if _, err = w.WriteString(text); err != nil {
			return err
		}
		// Above the display limit every read downloads the whole output again:
		// ask less often, so following a chatty job does not hammer the controller.
		if client.OutputIsLarge(job.ID) && !core.Finished(job.Status()) {
			select {
			case <-ctx.Done():
			case <-time.After(largeOutputPause):
			}
		}
		return nil
	}
	wctx, cancel := context.WithTimeout(ctx, waitTimeout)
	defer cancel()
	return client.Wait(wctx, j, waitEvery, emit)
}

var (
	evHost, evTask, evEvent string
	evFailed, evChanged     bool
)

var jobEventsCmd = &cobra.Command{
	Use:   "job-events <job-id>",
	Short: "List the task results of a job: which task did what on which host",
	Long: `Lists a run's events. The usual question is what failed:

  awxctl get job-events 1234 --failed
  awxctl get job-events 1234 --host web1 --changed

duration is how many seconds the task took on that host. To find what held a
run up:

  awxctl get job-events 1234 --slowest 10            # the ten slowest task results
  awxctl get job-events 1234 --slowest 5 --task "Wait for"

--slowest reads every task result of the run (that the other flags select) and
returns the slowest first.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id, err := jobID(args[0])
		if err != nil {
			return err
		}
		ctx, _, client, err := connect()
		if err != nil {
			return err
		}
		j, err := client.FindJob(cmd.Context(), id)
		if err != nil {
			return err
		}
		path := j.EventsPath()
		if path == "" {
			return &usageError{fmt.Sprintf("a %s has no events: see `awxctl get workflow-nodes %d`", strings.ReplaceAll(j.Type, "_", " "), id)}
		}
		q := url.Values{"order_by": {"counter"}}
		if (evFailed || evChanged || evEvent != "") && !j.HasTaskEvents() {
			return &usageError{"a system job has only output lines, no task results: drop --failed, --changed and --event"}
		}
		if evFailed {
			q.Set("failed", "true")
			q.Set("event__startswith", "runner_")
		}
		if evChanged {
			q.Set("changed", "true")
			q.Set("event__startswith", "runner_")
		}
		if evHost != "" {
			q.Set("host_name", evHost)
		}
		if evTask != "" {
			q.Set("task__icontains", evTask)
		}
		if evEvent != "" {
			q.Set("event", evEvent)
		}
		for _, kv := range getFilters {
			k, v, ok := strings.Cut(kv, "=")
			if !ok {
				return &usageError{fmt.Sprintf("--filter wants KEY=VALUE, got %q", kv)}
			}
			q.Add(k, v)
		}
		limit := effectiveLimit(cmd, ctx.DefaultLimit)
		slowest, _ := cmd.Flags().GetInt("slowest")
		if cmd.Flags().Changed("slowest") {
			if !j.HasTaskEvents() {
				return &usageError{"a system job has only output lines, no task results: nothing to time"}
			}
			if slowest < 1 {
				return &usageError{"--slowest wants how many to show, e.g. --slowest 10"}
			}
			// The controller cannot order by duration, so every task result is
			// read and ordered here. Only results have a duration.
			if q.Get("event") == "" && q.Get("event__startswith") == "" {
				q.Set("event__in", "runner_on_ok,runner_on_failed,runner_on_unreachable,runner_on_skipped")
			}
			limit = 0
		}
		evs, total, err := client.List(cmd.Context(), path, q, limit)
		if err != nil {
			return err
		}
		rows := make([]map[string]any, len(evs))
		for i, e := range evs {
			rows[i] = core.EventRow(e)
		}
		if cmd.Flags().Changed("slowest") {
			sort.SliceStable(rows, func(a, b int) bool { return core.Seconds(rows[a]) > core.Seconds(rows[b]) })
			output.Note("the %d slowest of %d task results read", min(slowest, len(rows)), len(rows))
			if len(rows) > slowest {
				rows = rows[:slowest]
			}
			total = len(rows)
		}
		output.Limit = limit
		if total > len(rows) {
			output.Fetched = limit
		}
		outFmt := output.Format(flagOutput)
		return output.Print(outFmt, fieldColumns(core.EventFields, getWide || outFmt != output.FormatTable), rows)
	},
}

var jobTimingCmd = &cobra.Command{
	Use:   "job-timing <job-id>",
	Short: "Where a run's time went: per task, per play or per host",
	Long: `Reads a run's task results and shows where the time went:

  awxctl get job-timing 1234 -o table              # one row per task, in the order they ran
  awxctl get job-timing 1234 --sort wall --limit 10   # the ten tasks that took longest
  awxctl get job-timing 1234 --by play
  awxctl get job-timing 1234 --by role             # time per role, for "which role costs us"
  awxctl get job-timing 1234 --by host             # which hosts cost the most time
  awxctl get job-timing 1234 --changed             # only tasks that changed something

Per task: wall_seconds (first start to last end over all hosts), host_seconds
(the sum over hosts, more than the wall clock when hosts run side by side),
slowest_seconds and slowest_host, and how many hosts changed, were unchanged,
failed or were skipped. --by host turns it round: per host, the time it used and
its slowest task. A play that runs its hosts in serial batches runs every task
once per batch: each is a row of its own, and play_run says which batch.

meta.summary says what the run's time was spent on, each task counted once with
its wall clock: changed (a host changed something), failed (none changed, one
failed), skipped (every host skipped it) and unchanged (it ran and changed
nothing: a no-op for this run). job_seconds is the run's own runtime; what the
tasks do not account for is time between tasks.

The whole run is read (one request per 200 task results), so a very large run
takes a moment. Time between tasks (gathering, waiting for a free fork) is not
in any task result: the tasks' wall clocks add up to less than the run.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id, err := jobID(args[0])
		if err != nil {
			return err
		}
		by, _ := cmd.Flags().GetString("by")
		sortBy, _ := cmd.Flags().GetString("sort")
		onlyChanged, _ := cmd.Flags().GetBool("changed")
		if by != "task" && by != "play" && by != "host" && by != "role" {
			return &usageError{"--by is task, play, role or host"}
		}
		keys := map[string]string{"run": "", "wall": "wall_seconds", "host-time": "host_seconds", "slowest": "slowest_seconds"}
		sortKey, ok := keys[sortBy]
		if !ok {
			return &usageError{"--sort is run (the order they ran), wall, host-time or slowest"}
		}
		ctx, _, client, err := connect()
		if err != nil {
			return err
		}
		j, err := client.FindJob(cmd.Context(), id)
		if err != nil {
			return err
		}
		if j.Type != "job" {
			return &usageError{fmt.Sprintf("only a playbook run has tasks to time; job %d is a %s", id, strings.ReplaceAll(j.Type, "_", " "))}
		}
		q := url.Values{"order_by": {"counter"}, "event__in": {core.TimingEvents}}
		if evHost != "" {
			q.Set("host_name", evHost)
		}
		if evTask != "" {
			q.Set("task__icontains", evTask)
		}
		evs, _, err := client.List(cmd.Context(), j.EventsPath(), q, 0)
		if err != nil {
			return err
		}
		rows := core.Timing(evs, by)
		// The summary is about the run, whatever the rows are grouped by.
		tasks := rows
		if by != "task" {
			tasks = core.Timing(evs, "task")
		}
		summary := core.TimingSummary(tasks)
		if evHost == "" && evTask == "" {
			if secs, ok := j.Obj["elapsed"]; ok {
				summary["job_seconds"] = secs
			}
		} else {
			summary["partial"] = "only the results that --host and --task select"
		}
		output.Summary = summary
		if onlyChanged {
			kept := rows[:0]
			for _, r := range rows {
				if n, _ := r["changed"].(int); n > 0 {
					kept = append(kept, r)
				}
			}
			rows = kept
		}
		if sortKey != "" {
			num := func(r map[string]any) float64 { f, _ := r[sortKey].(float64); return f }
			sort.SliceStable(rows, func(a, b int) bool { return num(rows[a]) > num(rows[b]) })
		}
		output.Note("built from %d task results of job %d", len(evs), id)
		if !core.Finished(j.Status()) {
			output.Note("the job is still %s: this is what has happened so far", j.Status())
		}
		if len(rows) > 0 && rows[0]["wall_seconds"] == nil {
			output.Note("this controller's events carry no start and end times, so there is no wall clock per %s: use host_seconds and slowest_seconds", by)
		}
		output.Total, output.TotalKnown = len(rows), true
		limit := effectiveLimit(cmd, ctx.DefaultLimit)
		if limit > 0 && len(rows) > limit {
			rows = rows[:limit]
			output.Limit, output.Fetched = limit, limit
		}
		var cols []output.Column
		switch by {
		case "host":
			cols = []output.Column{{Key: "host", Header: "HOST"}, {Key: "host_seconds", Header: "SECONDS"}, {Key: "tasks", Header: "TASKS"}, {Key: "slowest_seconds", Header: "SLOWEST S"}, {Key: "slowest_task", Header: "SLOWEST TASK"}}
		case "role":
			cols = []output.Column{{Key: "role", Header: "ROLE"}, {Key: "wall_seconds", Header: "WALL S"}, {Key: "host_seconds", Header: "HOST S"}, {Key: "tasks", Header: "TASKS"}, {Key: "slowest_seconds", Header: "SLOWEST S"}, {Key: "slowest_task", Header: "SLOWEST TASK"}}
		case "play":
			cols = []output.Column{{Key: "play", Header: "PLAY"}, {Key: "play_run", Header: "RUN"}, {Key: "wall_seconds", Header: "WALL S"}, {Key: "host_seconds", Header: "HOST S"}, {Key: "tasks", Header: "TASKS"}, {Key: "hosts", Header: "HOSTS"}, {Key: "slowest_seconds", Header: "SLOWEST S"}, {Key: "slowest_host", Header: "SLOWEST HOST"}}
		default:
			cols = []output.Column{{Key: "play", Header: "PLAY"}, {Key: "play_run", Header: "RUN"}, {Key: "task", Header: "TASK"}, {Key: "wall_seconds", Header: "WALL S"}, {Key: "host_seconds", Header: "HOST S"}, {Key: "hosts", Header: "HOSTS"}, {Key: "slowest_seconds", Header: "SLOWEST S"}, {Key: "slowest_host", Header: "SLOWEST HOST"}}
		}
		cols = append(cols, output.Column{Key: "changed", Header: "CHANGED"}, output.Column{Key: "unchanged", Header: "UNCHANGED"}, output.Column{Key: "failed", Header: "FAILED"}, output.Column{Key: "skipped", Header: "SKIPPED"})
		if output.Format(flagOutput) != output.FormatJSON {
			printTimingSummary(summary)
		}
		return output.Print(output.Format(flagOutput), cols, rows)
	},
}

// printTimingSummary writes the summary for a person, to standard error: in
// JSON it is meta.summary.
func printTimingSummary(sum map[string]any) {
	secs, _ := sum["seconds_by_outcome"].(map[string]float64)
	count, _ := sum["tasks_by_outcome"].(map[string]int)
	total, _ := sum["task_seconds"].(float64)
	if secs == nil || total == 0 {
		return
	}
	line := fmt.Sprintf("summary: %d tasks, %.0f s in tasks:", sum["tasks"], total)
	for _, k := range []string{"changed", "unchanged", "skipped", "failed"} {
		line += fmt.Sprintf(" %s %.0f s (%.0f%%, %d tasks);", k, secs[k], 100*secs[k]/total, count[k])
	}
	fmt.Fprintln(os.Stderr, strings.TrimSuffix(line, ";"))
}

var hostJobsCmd = &cobra.Command{
	Use:   "host-jobs <host>",
	Short: "List the jobs that ran against a host, newest first, with the host's result in each",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, _, client, err := connect()
		if err != nil {
			return err
		}
		hosts := core.FindResource("hosts")
		scope := url.Values{}
		if inv, _ := cmd.Flags().GetString("inventory"); inv != "" {
			scope.Set("inventory__name", inv)
		}
		// The same name can be a host in several inventories: their jobs are one list.
		found, err := client.ResolveAll(cmd.Context(), hosts, args[0], scope)
		if err != nil {
			return err
		}
		q := url.Values{"order_by": {"-id"}}
		if getFailed {
			q.Set("failed", "true")
		}
		if tmpl, _ := cmd.Flags().GetString("template"); tmpl != "" {
			q.Set("job__job_template__name", tmpl)
		}
		if getSince != "" {
			t, err := parseWhen(getSince, time.Now())
			if err != nil {
				return err
			}
			q.Set("created__gte", t)
		}
		limit := effectiveLimit(cmd, ctx.DefaultLimit)
		res := &core.Resource{Fields: core.HostSummaryFields}
		var rows []map[string]any
		total := 0
		for _, h := range found {
			sums, n, err := client.List(cmd.Context(), hosts.Path+strconv.Itoa(core.ID(h))+"/job_host_summaries/", q, limit)
			if err != nil {
				return err
			}
			total += n
			for _, s := range sums {
				row := res.Row(s)
				row["inventory"] = core.Lookup(h, "summary_fields.inventory.name")
				rows = append(rows, row)
			}
		}
		if len(found) > 1 {
			sort.SliceStable(rows, func(i, j int) bool {
				return core.ID(core.Object{"id": rows[i]["job_id"]}) > core.ID(core.Object{"id": rows[j]["job_id"]})
			})
			if limit > 0 && len(rows) > limit {
				rows = rows[:limit]
			}
			output.Note("%d hosts in different inventories have this name: their jobs are listed together (see the inventory field; --inventory picks one)", len(found))
		}
		if rows == nil {
			rows = []map[string]any{}
		}
		output.Limit, output.Total = limit, total
		if total > len(rows) {
			output.Fetched = limit
		}
		cols := []output.Column{{Key: "job_id", Header: "JOB"}, {Key: "job_name", Header: "NAME"}, {Key: "job_template", Header: "TEMPLATE"}, {Key: "job_status", Header: "JOB STATUS"}, {Key: "job_seconds", Header: "JOB S"}, {Key: "failed", Header: "HOST FAILED"},
			{Key: "ok", Header: "OK"}, {Key: "changed", Header: "CHANGED"}, {Key: "failures", Header: "FAILURES"}, {Key: "unreachable", Header: "UNREACHABLE"}, {Key: "created", Header: "WHEN"}}
		if len(found) > 1 {
			cols = append(cols, output.Column{Key: "inventory", Header: "INVENTORY"})
		}
		return output.Print(output.Format(flagOutput), cols, rows)
	},
}

var jobHostsCmd = &cobra.Command{
	Use:   "job-hosts <job-id>",
	Short: "List the per-host result counts of a job (ok, changed, failures, unreachable)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id, err := jobID(args[0])
		if err != nil {
			return err
		}
		ctx, _, client, err := connect()
		if err != nil {
			return err
		}
		q := url.Values{"order_by": {"host_name"}}
		if getFailed {
			q.Set("failed", "true")
		}
		limit := effectiveLimit(cmd, ctx.DefaultLimit)
		sums, total, err := client.List(cmd.Context(), "/jobs/"+strconv.Itoa(id)+"/job_host_summaries/", q, limit)
		if err != nil {
			return err
		}
		res := &core.Resource{Fields: core.HostSummaryFields[:9]}
		rows := make([]map[string]any, len(sums))
		for i, s := range sums {
			rows[i] = res.Row(s)
		}
		output.Limit = limit
		if total > len(rows) {
			output.Fetched = limit
		}
		return output.Print(output.Format(flagOutput), fieldColumns(core.HostSummaryFields[:9], true), rows)
	},
}

var workflowNodesCmd = &cobra.Command{
	Use:   "workflow-nodes <workflow-job-id | --template name>",
	Short: "List the nodes of a workflow job (which jobs it started and how they ended) or of a workflow template",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		tmpl, _ := cmd.Flags().GetString("template")
		if (len(args) == 1) == (tmpl != "") {
			return &usageError{"give a workflow job ID, or --template with a workflow template"}
		}
		_, _, client, err := connect()
		if err != nil {
			return err
		}
		var path string
		if tmpl != "" {
			wt := core.FindResource("workflow-templates")
			o, err := client.Resolve(cmd.Context(), wt, tmpl, nil)
			if err != nil {
				return err
			}
			path = wt.Path + strconv.Itoa(core.ID(o)) + "/workflow_nodes/"
		} else {
			id, err := jobID(args[0])
			if err != nil {
				return err
			}
			path = "/workflow_jobs/" + strconv.Itoa(id) + "/workflow_nodes/"
		}
		nodes, _, err := client.List(cmd.Context(), path, url.Values{"order_by": {"id"}}, 0)
		if err != nil {
			return err
		}
		rows := make([]map[string]any, len(nodes))
		for i, n := range nodes {
			rows[i] = core.WorkflowNodeRow(n)
		}
		outFmt := output.Format(flagOutput)
		return output.Print(outFmt, fieldColumns(core.WorkflowNodeFields, outFmt != output.FormatTable), rows)
	},
}

var launchInfoCmd = &cobra.Command{
	Use:   "launch-info <template>",
	Short: "Show what a template asks for at launch: prompts, defaults and survey questions",
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
		info, err := client.LaunchInfo(cmd.Context(), r, tmpl)
		if err != nil {
			return err
		}
		cols := []output.Column{{Key: "id", Header: "ID"}, {Key: "name", Header: "NAME"}, {Key: "type", Header: "TYPE"}, {Key: "prompts", Header: "PROMPTS"},
			{Key: "variables_needed_to_start", Header: "REQUIRED VARS"}, {Key: "can_start_without_user_input", Header: "STARTS UNATTENDED"}}
		return output.Print(output.Format(flagOutput), cols, []map[string]any{info})
	},
}

// findTemplate resolves a job template, or a workflow template with --workflow.
func findTemplate(cmd *cobra.Command, client *core.Client, ref string) (*core.Resource, core.Object, error) {
	kind := "job-templates"
	if wf, _ := cmd.Flags().GetBool("workflow"); wf {
		kind = "workflow-templates"
	}
	r := core.FindResource(kind)
	o, err := client.Resolve(cmd.Context(), r, ref, nil)
	return r, o, err
}

var diagnoseCmd = group(&cobra.Command{
	Use:   "diagnose",
	Short: "Explain what went wrong, in one call",
})

var diagnoseJobCmd = &cobra.Command{
	Use:   "job <job-id>",
	Short: "Why did this job fail: verdict, failed tasks per host, failing hosts, workflow nodes",
	Long: `Collects what explains a run in one call:

  - the job's own facts (template, inventory, limit, who or what launched it);
  - the failed and unreachable task results, one entry per distinct failure with
    the play, role, task, file and line, message and the hosts it hit: hosts that
    failed the same task the same way share one entry;
  - hosts_failed: the hosts that failed, with their counts;
  - stalled: for a job that stopped without a failed task, the last task that
    ran and the hosts that started it and did not return, with the reason
    (timeout, canceled, or "play ended early": an abort or an error outside a task);
  - errors: the output lines that report such an error ([ERROR], ERROR!, fatal),
    each with the lines that explain it;
  - for a workflow: the nodes that did not succeed, with their job IDs;
  - the last output lines (timing-only lines left out) when nothing better is recorded;
  - with --compare-last-success: what differs from the newest successful run of
    the same template (revision, limit, inventory, execution environment, which
    extra variables, host count) and both runtimes.

This is best effort: it reports what the controller recorded. A task that hides
its result (no_log) or only says "failed, see above" tells diagnose no more than
it tells you; then read the output (awxctl get job-output, get host-output).`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id, err := jobID(args[0])
		if err != nil {
			return err
		}
		_, _, client, err := connect()
		if err != nil {
			return err
		}
		diagnose := client.Diagnose
		if deep, _ := cmd.Flags().GetBool("recursive"); deep {
			diagnose = client.DiagnoseDeep
		}
		d, err := diagnose(cmd.Context(), id)
		if err != nil {
			return err
		}
		if compare, _ := cmd.Flags().GetBool("compare-last-success"); compare {
			if err := client.CompareLastSuccess(cmd.Context(), d, id); err != nil {
				return err
			}
		}
		row := map[string]any{"verdict": d.Verdict, "job": d.Job, "failures": d.Failures, "hosts_failed": d.Hosts, "nodes": d.Nodes}
		if d.Stalled != nil {
			row["stalled"] = d.Stalled
		}
		if len(d.Errors) > 0 {
			row["errors"] = d.Errors
		}
		if d.Compare != nil {
			row["compare"] = d.Compare
		}
		if d.Leaves != nil {
			row["leaf_failures"] = d.Leaves
		}
		if len(d.Tail) > 0 {
			row["tail"] = d.Tail
		}
		outFmt := output.Format(flagOutput)
		if outFmt != output.FormatTable {
			return output.Print(outFmt, nil, []map[string]any{row})
		}
		printDiagnosis(id, d)
		return nil
	},
}

// printDiagnosis is the view for a person: short labelled lines that fit a
// terminal, in the order one reads them (what, where, why, what changed). The
// JSON form has everything; this leaves out what does not help at a glance.
func printDiagnosis(id int, d *core.Diagnosis) {
	p := func(s any) string { return output.Printable(fmt.Sprint(s)) }
	kind := strings.ReplaceAll(fmt.Sprint(d.Job["type"]), "_", " ")
	fmt.Printf("%s %d  %s  %s\n", strings.ToUpper(kind[:1])+kind[1:], id, p(d.Job["name"]), strings.ToUpper(fmt.Sprint(d.Job["status"])))
	// the verdict is one long sentence: one clause a line
	for _, clause := range strings.Split(d.Verdict, "; ") {
		fmt.Printf("  %s\n", p(clause))
	}
	for _, f := range d.Failures {
		hosts := f["hosts"].([]string)
		fmt.Printf("\n  FAILED on %d host(s): %s\n", len(hosts), p(strings.Join(hosts, ", ")))
		task := fmt.Sprintf("%v", f["task"])
		if role, _ := f["role"].(string); role != "" {
			task += "  (role " + role + ")"
		}
		fmt.Printf("      task   %s\n", p(task))
		if path, _ := f["task_path"].(string); path != "" {
			fmt.Printf("      file   %s\n", p(strings.TrimPrefix(path, "/runner/project/")))
		}
		fmt.Printf("      error  %s\n", p(strings.ReplaceAll(fmt.Sprint(f["message"]), "\n", "\n             ")))
	}
	if d.Stalled != nil {
		fmt.Printf("\n  STOPPED (%v) in task %s\n", d.Stalled["reason"], p(d.Stalled["last_task"]))
		fmt.Printf("      last result  %v\n", d.Stalled["last_result_at"])
		if missing, _ := d.Stalled["hosts_not_returned"].([]string); len(missing) > 0 {
			fmt.Printf("      no return    %d of %v host(s): %s\n", len(missing), d.Stalled["hosts_started"], p(strings.Join(missing, ", ")))
		}
	}
	for _, e := range d.Errors {
		fmt.Printf("\n  ERROR at output line %v\n      %s\n", e["line"], p(strings.ReplaceAll(fmt.Sprint(e["text"]), "\n", "\n      ")))
	}
	for _, n := range d.Nodes {
		fmt.Printf("\n  NODE %s  job %v  %v\n", p(n["template"]), n["job_id"], n["status"])
	}
	for _, l := range d.Leaves {
		fmt.Printf("\n  FAILED JOB %v  %s\n      %s\n", l["job_id"], p(l["path"]), p(strings.ReplaceAll(fmt.Sprint(l["verdict"]), "; ", "\n      ")))
	}
	if d.Compare != nil {
		last, _ := d.Compare["last_success"].(map[string]any)
		diff, _ := d.Compare["differences"].(map[string]any)
		switch {
		case d.Compare["found"] != true:
			fmt.Printf("\n  COMPARE  %s\n", p(d.Compare["note"]))
		case len(diff) == 0:
			fmt.Printf("\n  SAME settings as the last success (job %v)\n", last["id"])
		default:
			fmt.Printf("\n  DIFFERS from the last success (job %v)\n", last["id"])
			keys := make([]string, 0, len(diff))
			for k := range diff {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				m, _ := diff[k].(map[string]any)
				if names, ok := m["names_that_differ"]; ok {
					fmt.Printf("      %-14s %s\n", k, p(names))
					continue
				}
				this, was := fmt.Sprint(m["this"]), fmt.Sprint(m["last_success"])
				if k == "scm_revision" && len(this) > 12 && len(was) > 12 { // the short form is enough to tell them apart
					this, was = this[:12], was[:12]
				}
				fmt.Printf("      %-14s %s  (was %s)\n", k, p(this), p(was))
			}
		}
	}
	if len(d.Tail) > 0 {
		fmt.Println("\n  LAST OUTPUT")
		for _, l := range d.Tail {
			fmt.Println("      " + p(l))
		}
	}
}

var waitCmd = &cobra.Command{
	Use:   "wait <job-id>",
	Short: "Wait until a job has finished; exit 0 only if it was successful",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id, err := jobID(args[0])
		if err != nil {
			return err
		}
		_, _, client, err := connect()
		if err != nil {
			return err
		}
		j, err := client.FindJob(cmd.Context(), id)
		if err != nil {
			return err
		}
		return waitAndReport(cmd.Context(), client, j, false)
	},
}

// waitAndReport waits for the run, prints its final row and fails unless it was
// successful. With follow the output goes to stderr while waiting.
func waitAndReport(ctx context.Context, client *core.Client, j *core.Job, follow bool) error {
	var err error
	if follow {
		j, err = followOutput(ctx, client, j, os.Stderr, 0)
	} else {
		wctx, cancel := context.WithTimeout(ctx, waitTimeout)
		defer cancel()
		j, err = client.Wait(wctx, j, waitEvery, nil)
	}
	if err != nil {
		return err
	}
	r := core.FindResource("unified-jobs")
	outFmt := output.Format(flagOutput)
	if err := output.Print(outFmt, columns(r, false), []map[string]any{r.Row(j.Obj)}); err != nil {
		return err
	}
	if j.Status() != "successful" {
		return cliPrintErr(outFmt, fmt.Sprintf("job %d ended %s: run `awxctl diagnose job %d`", j.ID, j.Status(), j.ID), output.ExitError)
	}
	return nil
}

func addWaitFlags(c *cobra.Command) {
	c.Flags().DurationVar(&waitTimeout, "wait-timeout", time.Hour, "give up waiting after this long (the job keeps running)")
	c.Flags().DurationVar(&waitEvery, "poll", 3*time.Second, "how often to ask for the status")
}

func init() {
	jobOutputCmd.Flags().IntVar(&outTail, "tail", 0, "only the last N lines (after --grep)")
	jobOutputCmd.Flags().IntVar(&outHead, "head", 0, "only the first N lines (after --grep)")
	jobOutputCmd.Flags().StringVar(&outGrep, "grep", "", "only lines matching this regular expression, shown with their line numbers")
	jobOutputCmd.Flags().IntVar(&outContext, "context", 0, "with --grep: this many lines around each match")
	jobOutputCmd.Flags().IntVar(&outStart, "start-line", 0, "read from this line on (next_line of an earlier call)")
	jobOutputCmd.Flags().StringVar(&outSave, "save", "", "write the whole output to this file instead of printing it")
	jobOutputCmd.Flags().BoolVar(&outText, "text", false, "print only the text, not JSON")
	jobOutputCmd.Flags().BoolVar(&outFollow, "follow", false, "with --text: keep printing until the job ends; exit 0 only if it was successful")
	addWaitFlags(jobOutputCmd)
	addWaitFlags(waitCmd)

	jobEventsCmd.Flags().Int("slowest", 0, "the N slowest task results of the run (reads them all), slowest first")
	jobEventsCmd.Flags().BoolVar(&evFailed, "failed", false, "only failed task results")
	jobEventsCmd.Flags().BoolVar(&evChanged, "changed", false, "only task results that changed something")
	jobEventsCmd.Flags().StringVar(&evHost, "host", "", "only this host")
	jobEventsCmd.Flags().StringVar(&evTask, "task", "", "only tasks whose name contains this")
	jobEventsCmd.Flags().StringVar(&evEvent, "event", "", "only this event type, e.g. runner_on_unreachable or playbook_on_stats")
	jobEventsCmd.Flags().StringArrayVar(&getFilters, "filter", nil, "API filter KEY=VALUE, passed through (repeatable)")
	jobEventsCmd.Flags().IntVar(&getLimit, "limit", 0, "most rows to return (default: the context's default_limit); -1 = all")
	jobEventsCmd.Flags().BoolVar(&getWide, "wide", false, "show every field in table output")

	hostJobsCmd.Flags().String("inventory", "", "the host's inventory, when the name exists in more than one")
	hostJobsCmd.Flags().String("template", "", "only jobs of this job template")
	hostJobsCmd.Flags().BoolVar(&getFailed, "failed", false, "only jobs in which this host failed")
	hostJobsCmd.Flags().StringVar(&getSince, "since", "", "only jobs newer than this: 30m, 24h, 7d, a date or RFC 3339")
	hostJobsCmd.Flags().IntVar(&getLimit, "limit", 0, "most rows to return; -1 = all")
	jobHostsCmd.Flags().BoolVar(&getFailed, "failed", false, "only hosts that failed")
	jobHostsCmd.Flags().IntVar(&getLimit, "limit", 0, "most rows to return; -1 = all")
	launchInfoCmd.Flags().Bool("workflow", false, "the template is a workflow template")
	workflowNodesCmd.Flags().String("template", "", "show the nodes of this workflow template instead of a workflow job")

	jobTimingCmd.Flags().String("by", "task", "one row per task, play, role or host")
	jobTimingCmd.Flags().String("sort", "run", "run (the order they ran), wall, host-time or slowest; longest first")
	jobTimingCmd.Flags().Bool("changed", false, "only rows where something changed")
	jobTimingCmd.Flags().StringVar(&evHost, "host", "", "only this host's results")
	jobTimingCmd.Flags().StringVar(&evTask, "task", "", "only tasks whose name contains this")
	jobTimingCmd.Flags().IntVar(&getLimit, "limit", 0, "most rows to return; -1 = all")
	getCmd.AddCommand(jobOutputCmd, jobEventsCmd, jobTimingCmd, hostJobsCmd, jobHostsCmd, workflowNodesCmd, launchInfoCmd)
	diagnoseJobCmd.Flags().Bool("recursive", false, "for a workflow: follow the failed nodes, also through workflows inside it, and diagnose the jobs that failed")
	diagnoseJobCmd.Flags().Bool("compare-last-success", false, "also say what differs from the last successful run of the same template")
	diagnoseCmd.AddCommand(diagnoseJobCmd)
	rootCmd.AddCommand(diagnoseCmd, waitCmd)
}

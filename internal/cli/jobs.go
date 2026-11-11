package cli

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"regexp"
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
			if err := os.WriteFile(outSave, []byte(text), 0o600); err != nil {
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
		_, err = w.WriteString(text)
		return err
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
  awxctl get job-events 1234 --host web1 --changed`,
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
		evs, total, err := client.List(cmd.Context(), path, q, limit)
		if err != nil {
			return err
		}
		rows := make([]map[string]any, len(evs))
		for i, e := range evs {
			rows[i] = core.EventRow(e)
		}
		output.Limit = limit
		if total > len(rows) {
			output.Fetched = limit
		}
		outFmt := output.Format(flagOutput)
		return output.Print(outFmt, fieldColumns(core.EventFields, getWide || outFmt != output.FormatTable), rows)
	},
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
		h, err := client.Resolve(cmd.Context(), hosts, args[0], scope)
		if err != nil {
			return err
		}
		q := url.Values{"order_by": {"-id"}}
		if getFailed {
			q.Set("failed", "true")
		}
		if getSince != "" {
			t, err := parseWhen(getSince, time.Now())
			if err != nil {
				return err
			}
			q.Set("created__gte", t)
		}
		limit := effectiveLimit(cmd, ctx.DefaultLimit)
		sums, total, err := client.List(cmd.Context(), hosts.Path+strconv.Itoa(core.ID(h))+"/job_host_summaries/", q, limit)
		if err != nil {
			return err
		}
		res := &core.Resource{Fields: core.HostSummaryFields}
		rows := make([]map[string]any, len(sums))
		for i, s := range sums {
			rows[i] = res.Row(s)
		}
		output.Limit = limit
		if total > len(rows) {
			output.Fetched = limit
		}
		cols := []output.Column{{Key: "job_id", Header: "JOB"}, {Key: "job_name", Header: "NAME"}, {Key: "job_status", Header: "JOB STATUS"}, {Key: "failed", Header: "HOST FAILED"},
			{Key: "ok", Header: "OK"}, {Key: "changed", Header: "CHANGED"}, {Key: "failures", Header: "FAILURES"}, {Key: "unreachable", Header: "UNREACHABLE"}, {Key: "created", Header: "WHEN"}}
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
	Use:   "workflow-nodes <workflow-job-id>",
	Short: "List the nodes of a workflow job: which jobs it started and how they ended",
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
		nodes, _, err := client.List(cmd.Context(), "/workflow_jobs/"+strconv.Itoa(id)+"/workflow_nodes/", url.Values{"order_by": {"id"}}, 0)
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
	Long: `Collects what explains a run in one call: the job's own facts, every failed or
unreachable task result (task, host, message), the hosts that failed with their
counts, the nodes of a workflow that did not succeed, and the last output lines
when no task failure was recorded. Works for jobs, workflow jobs, project and
inventory syncs.`,
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
		d, err := client.Diagnose(cmd.Context(), id)
		if err != nil {
			return err
		}
		row := map[string]any{"verdict": d.Verdict, "job": d.Job, "failures": d.Failures, "hosts": d.Hosts, "nodes": d.Nodes}
		if len(d.Tail) > 0 {
			row["tail"] = d.Tail
		}
		outFmt := output.Format(flagOutput)
		if outFmt != output.FormatTable {
			return output.Print(outFmt, nil, []map[string]any{row})
		}
		fmt.Printf("Job %d (%v) %v: %s\n", id, d.Job["type"], d.Job["name"], d.Verdict)
		for _, f := range d.Failures {
			fmt.Printf("  FAILED  host=%v  task=%q  %v\n", f["host"], f["task"], output.Printable(fmt.Sprint(f["message"])))
		}
		for _, n := range d.Nodes {
			fmt.Printf("  NODE    %v  job=%v  status=%v\n", n["template"], n["job_id"], n["status"])
		}
		for _, l := range d.Tail {
			fmt.Println("  | " + output.Printable(l))
		}
		return nil
	},
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

	jobEventsCmd.Flags().BoolVar(&evFailed, "failed", false, "only failed task results")
	jobEventsCmd.Flags().BoolVar(&evChanged, "changed", false, "only task results that changed something")
	jobEventsCmd.Flags().StringVar(&evHost, "host", "", "only this host")
	jobEventsCmd.Flags().StringVar(&evTask, "task", "", "only tasks whose name contains this")
	jobEventsCmd.Flags().StringVar(&evEvent, "event", "", "only this event type, e.g. runner_on_unreachable or playbook_on_stats")
	jobEventsCmd.Flags().StringArrayVar(&getFilters, "filter", nil, "API filter KEY=VALUE, passed through (repeatable)")
	jobEventsCmd.Flags().IntVar(&getLimit, "limit", 0, "most rows to return (default: the context's default_limit); -1 = all")
	jobEventsCmd.Flags().BoolVar(&getWide, "wide", false, "show every field in table output")

	hostJobsCmd.Flags().String("inventory", "", "the host's inventory, when the name exists in more than one")
	hostJobsCmd.Flags().BoolVar(&getFailed, "failed", false, "only jobs in which this host failed")
	hostJobsCmd.Flags().StringVar(&getSince, "since", "", "only jobs newer than this: 30m, 24h, 7d, a date or RFC 3339")
	hostJobsCmd.Flags().IntVar(&getLimit, "limit", 0, "most rows to return; -1 = all")
	jobHostsCmd.Flags().BoolVar(&getFailed, "failed", false, "only hosts that failed")
	jobHostsCmd.Flags().IntVar(&getLimit, "limit", 0, "most rows to return; -1 = all")
	launchInfoCmd.Flags().Bool("workflow", false, "the template is a workflow template")

	getCmd.AddCommand(jobOutputCmd, jobEventsCmd, hostJobsCmd, jobHostsCmd, workflowNodesCmd, launchInfoCmd)
	diagnoseCmd.AddCommand(diagnoseJobCmd)
	rootCmd.AddCommand(diagnoseCmd, waitCmd)
}

package core

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Job is one run of anything: a playbook job, a workflow, a project or inventory
// sync, an ad hoc command or a system job. IDs are shared across all of them.
type Job struct {
	ID   int
	Type string // job, workflow_job, project_update, inventory_update, ad_hoc_command, system_job
	Path string // "/jobs/12/"
	Obj  Object
}

var jobPaths = map[string]string{
	"job": "/jobs/", "workflow_job": "/workflow_jobs/", "project_update": "/project_updates/",
	"inventory_update": "/inventory_updates/", "ad_hoc_command": "/ad_hoc_commands/", "system_job": "/system_jobs/",
}

// FindJob looks a run up by ID, whatever its type.
func (c *Client) FindJob(ctx context.Context, id int) (*Job, error) {
	objs, _, err := c.List(ctx, "/unified_jobs/", url.Values{"id": {strconv.Itoa(id)}}, 1)
	if err != nil {
		return nil, err
	}
	if len(objs) == 0 {
		return nil, &NotFoundError{"job", "id:" + strconv.Itoa(id)}
	}
	typ, _ := objs[0]["type"].(string)
	base, ok := jobPaths[typ]
	if !ok {
		return nil, fmt.Errorf("job %d has the unknown type %q", id, typ)
	}
	path := base + strconv.Itoa(id) + "/"
	full, err := c.Get(ctx, path)
	if err != nil {
		return nil, err
	}
	return &Job{ID: id, Type: typ, Path: path, Obj: full}, nil
}

// Status is the job's status field.
func (j *Job) Status() string { s, _ := j.Obj["status"].(string); return s }

// Finished reports whether the run has ended, whatever the result.
func Finished(status string) bool {
	switch status {
	case "successful", "failed", "error", "canceled":
		return true
	}
	return false
}

var ansiRE = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)

// StripANSI removes terminal colour codes from job output.
func StripANSI(s string) string { return ansiRE.ReplaceAllString(s, "") }

// Output reads the run's output from line start on (0 = the beginning). It
// returns the text without colour codes and the line number to continue from.
func (c *Client) Output(ctx context.Context, j *Job, start int) (text string, next int, err error) {
	if j.Type == "workflow_job" {
		return "", 0, usagef("a workflow job has no output of its own: see `awxctl get workflow-nodes %d` for the jobs it started", j.ID)
	}
	if err := c.ready(ctx); err != nil {
		return "", 0, err
	}
	var buf strings.Builder
	next = start
	for {
		var chunk struct {
			Range struct {
				End         int `json:"end"`
				AbsoluteEnd int `json:"absolute_end"`
			} `json:"range"`
			Content string `json:"content"`
		}
		// Asking for a bounded range keeps one answer small; the loop reads on.
		q := fmt.Sprintf("stdout/?format=json&content_format=txt&start_line=%d&end_line=%d", next, next+5000)
		if err := c.get(ctx, c.apiPath+j.Path+q, &chunk); err != nil {
			return "", 0, err
		}
		if next == start && strings.HasPrefix(chunk.Content, "Standard Output too large to display") {
			// Above the controller's display limit (1 MB by default) the ranged
			// form only answers with this notice: the whole text must be downloaded.
			return c.outputDownload(ctx, j, start)
		}
		buf.WriteString(chunk.Content)
		if chunk.Range.End <= next || chunk.Range.End >= chunk.Range.AbsoluteEnd {
			if chunk.Range.End > next {
				next = chunk.Range.End
			}
			break
		}
		next = chunk.Range.End
	}
	return StripANSI(buf.String()), next, nil
}

// outputDownload reads the whole output as one text and cuts it at the start line.
func (c *Client) outputDownload(ctx context.Context, j *Job, start int) (string, int, error) {
	if c.largeOutput == nil {
		c.largeOutput = map[int]bool{}
	}
	c.largeOutput[j.ID] = true
	text, err := c.GetText(ctx, j.Path+"stdout/?format=txt_download")
	if err != nil {
		return "", 0, err
	}
	text = strings.ReplaceAll(StripANSI(text), "\r\n", "\n")
	lines := strings.SplitAfter(text, "\n")
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	total := len(lines)
	if start >= total {
		return "", total, nil
	}
	return strings.Join(lines[start:], ""), total, nil
}

// OutputIsLarge reports whether the job's output has to be downloaded whole on
// every read (it is above the controller's display limit). Following such a job
// should poll slowly.
func (c *Client) OutputIsLarge(id int) bool { return c.largeOutput[id] }

// LineFilter narrows output: lines matching Grep (with Context lines around each
// match), then the first Head or last Tail of what is left.
type LineFilter struct {
	Grep    *regexp.Regexp
	Context int
	Head    int
	Tail    int
}

// Apply returns the selected lines, each with its 1-based line number.
func (f LineFilter) Apply(text string) []NumberedLine {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return nil
	}
	keep := make([]bool, len(lines))
	for i, l := range lines {
		if f.Grep == nil {
			keep[i] = true
		} else if f.Grep.MatchString(l) {
			for k := max(0, i-f.Context); k <= min(len(lines)-1, i+f.Context); k++ {
				keep[k] = true
			}
		}
	}
	var out []NumberedLine
	for i, l := range lines {
		if keep[i] {
			out = append(out, NumberedLine{N: i + 1, Text: l})
		}
	}
	if f.Head > 0 && len(out) > f.Head {
		out = out[:f.Head]
	}
	if f.Tail > 0 && len(out) > f.Tail {
		out = out[len(out)-f.Tail:]
	}
	return out
}

// NumberedLine is one line of output and where it is.
type NumberedLine struct {
	N    int
	Text string
}

// ErrWaitTimeout means the wait ended before the job did. The job is not failed:
// it is still running.
var ErrWaitTimeout = errors.New("stopped waiting")

// Wait polls until the run has finished or ctx ends. onPoll, when set, is called
// after every poll with the current job (for --follow).
func (c *Client) Wait(ctx context.Context, j *Job, every time.Duration, onPoll func(*Job) error) (*Job, error) {
	if every <= 0 {
		every = 3 * time.Second
	}
	for {
		obj, err := c.Get(ctx, j.Path)
		if err != nil {
			return j, err
		}
		j = &Job{ID: j.ID, Type: j.Type, Path: j.Path, Obj: obj}
		if onPoll != nil {
			if err := onPoll(j); err != nil {
				return j, err
			}
		}
		if Finished(j.Status()) {
			return j, nil
		}
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return j, fmt.Errorf("%w: job %d is still %s; it keeps running, look again with `awxctl wait %d` or `awxctl get jobs %d`", ErrWaitTimeout, j.ID, j.Status(), j.ID, j.ID)
			}
			return j, fmt.Errorf("gave up waiting for job %d (status %s): %w", j.ID, j.Status(), ctx.Err())
		case <-time.After(every):
		}
	}
}

// EventsPath is where the run's events are, or "" if the type has none.
func (j *Job) EventsPath() string {
	switch j.Type {
	case "job":
		return j.Path + "job_events/"
	case "project_update", "inventory_update", "ad_hoc_command", "system_job":
		return j.Path + "events/"
	}
	return ""
}

// HasTaskEvents reports whether the events are Ansible task results (a system
// job only has plain output lines).
func (j *Job) HasTaskEvents() bool { return j.Type != "system_job" && j.Type != "workflow_job" }

// EventFields are the flat fields of a job event.
var EventFields = []Field{
	f("counter", "N"), f("event", "EVENT"), ff("host", "HOST", "host_name"), f("task", "TASK"), f("play", "PLAY"),
	f("failed", "FAILED"), f("changed", "CHANGED"), ff("message", "MESSAGE", "event_data.res.msg"),
	w("role"), w("created"), w("start_line"), w("end_line"), w("stdout"), w("id"),
	wf("module", "event_data.task_action"), wf("stderr", "event_data.res.stderr"), wf("rc", "event_data.res.rc"),
	wf("task_path", "event_data.task_path"), wf("ignore_errors", "event_data.ignore_errors"),
}

// EventRow flattens one event; long texts are cut so a list stays readable.
func EventRow(o Object) map[string]any {
	r := (&Resource{Fields: EventFields}).Row(o)
	for _, k := range []string{"message", "stderr", "stdout"} {
		if s, ok := r[k].(string); ok {
			r[k] = clip(StripANSI(s), 2000)
		} else if r[k] != nil {
			r[k] = clip(fmt.Sprint(r[k]), 2000)
		}
	}
	return r
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + fmt.Sprintf(" ... (%d more bytes)", len(s)-n)
}

// HostSummaryFields are the per-host result counts of a job.
var HostSummaryFields = []Field{
	ff("host", "HOST", "host_name"), f("failed", "FAILED"), f("ok", "OK"), f("changed", "CHANGED"), f("failures", "FAILURES"),
	ff("unreachable", "UNREACHABLE", "dark"), f("skipped", "SKIPPED"), f("rescued", "RESCUED"), f("ignored", "IGNORED"),
	ff("job_id", "JOB", "summary_fields.job.id"), ff("job_name", "JOB NAME", "summary_fields.job.name"),
	ff("job_status", "JOB STATUS", "summary_fields.job.status"), ff("job_template", "TEMPLATE", "summary_fields.job.job_template_name"),
	ff("job_seconds", "JOB S", "summary_fields.job.elapsed"), w("created"), wf("inventory", "summary_fields.host.inventory_name|inventory_name"),
}

// Diagnosis is the answer to "why did this run fail".
type Diagnosis struct {
	Job     map[string]any `json:"job"`
	Verdict string         `json:"verdict"`
	// Failures are the failed and unreachable task results, one entry per distinct
	// failure: hosts that failed the same task with the same message share an
	// entry, which lists them.
	Failures []map[string]any `json:"failures"`
	Hosts    []map[string]any `json:"hosts_failed"` // per-host counts of the hosts that failed
	Nodes    []map[string]any `json:"nodes"`        // workflow jobs: the nodes that did not succeed
	// Stalled is set for a job that was cut off (timeout, cancel) or ended without
	// a failed task: the last task that ran and the hosts that never returned from it.
	Stalled map[string]any `json:"stalled,omitempty"`
	// Errors are the output lines that report an error outside a task result (a
	// play abort, a templating or syntax error), each with the lines that follow it.
	Errors []map[string]any `json:"errors,omitempty"`
	Tail   []string         `json:"tail,omitempty"` // last output lines, without timing-only lines, when there is no better evidence
	// Compare is set by CompareLastSuccess: what differs from the last good run.
	Compare map[string]any `json:"compare,omitempty"`

	failedHosts int // distinct hosts among the failure events
}

// maxFailureEvents bounds how many failed task results a diagnosis reads.
const maxFailureEvents = 1000

// groupFailures folds failure events into one entry per distinct failure. Two
// results are the same failure when the event, play, role, task and message
// agree once the host's own name is taken out of the message.
func groupFailures(evs []Object) (groups_ []map[string]any, ignored int, hostCount int) {
	var order []string
	groups := map[string]map[string]any{}
	// A failed loop task reports each failed item and then the task once more
	// ("One or more items failed"): with item results present, the summary is
	// the same failure a second time.
	hasItems := map[string]bool{}
	for _, e := range evs {
		if e["event"] == "runner_item_on_failed" {
			hasItems[fmt.Sprint(e["host_name"], "\x00", e["parent_uuid"], "\x00", e["task"])] = true
		}
	}
	allHosts := map[string]bool{}
	for _, e := range evs {
		// An error the play was told to ignore did not fail anything.
		if Lookup(e, "event_data.ignore_errors") == true {
			ignored++
			continue
		}
		if e["event"] == "runner_on_failed" && hasItems[fmt.Sprint(e["host_name"], "\x00", e["parent_uuid"], "\x00", e["task"])] {
			continue
		}
		row := EventRow(e)
		host, _ := row["host"].(string)
		msg := fmt.Sprint(row["message"])
		if row["message"] == nil {
			msg = ""
		}
		norm := replaceWord(msg, host, "<host>")
		// An unreachable host's message names its address, not its inventory name.
		for _, alt := range []string{"event_data.remote_addr", "event_data.res.ansible_host"} {
			if addr, _ := Lookup(e, alt).(string); addr != "" && addr != host {
				norm = replaceWord(norm, addr, "<host>")
			}
		}
		if host != "" {
			allHosts[host] = true
		}
		key := fmt.Sprint(row["event"], "\x00", row["play"], "\x00", row["role"], "\x00", row["task"], "\x00", norm)
		g := groups[key]
		if g == nil {
			g = map[string]any{"event": row["event"], "play": row["play"], "role": row["role"], "task": row["task"], "task_path": row["task_path"],
				"module": row["module"], "message": norm, "hosts": []string{}, "first_counter": row["counter"], "first_line": row["start_line"]}
			if norm == msg {
				g["message"] = row["message"]
			}
			for _, k := range []string{"stderr", "rc"} {
				if row[k] != nil && row[k] != "" {
					g[k] = row[k]
				}
			}
			groups[key] = g
			order = append(order, key)
		}
		if host != "" {
			g["hosts"] = append(g["hosts"].([]string), host)
		}
	}
	out := make([]map[string]any, len(order))
	for i, k := range order {
		g := groups[k]
		hosts := uniqueSorted(g["hosts"].([]string))
		g["hosts"], g["host_count"] = hosts, len(hosts)
		out[i] = g
	}
	return out, ignored, len(allHosts)
}

func uniqueSorted(in []string) []string {
	sort.Strings(in)
	out := in[:0]
	for i, s := range in {
		if i == 0 || s != in[i-1] {
			out = append(out, s)
		}
	}
	return out
}

// replaceWord replaces name where it stands as a whole host name or address, not
// where it is part of a longer word: host "a" must not rewrite every letter a.
func replaceWord(text, name, with string) string {
	if name == "" || !strings.Contains(text, name) {
		return text
	}
	isWord := func(r byte) bool {
		return r == '_' || r == '-' || r == '.' || (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
	}
	var b strings.Builder
	for i := 0; i < len(text); {
		j := strings.Index(text[i:], name)
		if j < 0 {
			b.WriteString(text[i:])
			break
		}
		start, end := i+j, i+j+len(name)
		before := start > 0 && isWord(text[start-1])
		after := end < len(text) && isWord(text[end]) && !(text[end] == '.' && (end+1 == len(text) || !isWord(text[end+1])))
		b.WriteString(text[i:start])
		if before || after {
			b.WriteString(name)
		} else {
			b.WriteString(with)
		}
		i = end
	}
	return b.String()
}

// stalled finds where a job stopped: the newest task result, the task it belongs
// to, and the hosts that started that task without returning from it.
func (c *Client) stalled(ctx context.Context, j *Job) (map[string]any, error) {
	ep := j.EventsPath()
	newest, _, err := c.List(ctx, ep, url.Values{"order_by": {"-counter"}, "event__startswith": {"runner_"}}, 1)
	if err != nil || len(newest) == 0 {
		return nil, err
	}
	last := newest[0]
	out := map[string]any{"last_task": last["task"], "play": last["play"], "role": last["role"], "last_result_at": last["created"]}
	parent, _ := last["parent_uuid"].(string)
	if parent == "" {
		return out, nil
	}
	evs, _, err := c.List(ctx, ep, url.Values{"parent_uuid": {parent}, "event__startswith": {"runner_"}}, 0)
	if err != nil {
		return nil, err
	}
	started, returned := map[string]bool{}, map[string]bool{}
	for _, e := range evs {
		host, _ := e["host_name"].(string)
		if host == "" {
			continue
		}
		switch e["event"] {
		case "runner_on_start":
			started[host] = true
		case "runner_on_ok", "runner_on_failed", "runner_on_unreachable", "runner_on_skipped":
			returned[host] = true
		}
	}
	missing := []string{}
	for h := range started {
		if !returned[h] {
			missing = append(missing, h)
		}
	}
	sort.Strings(missing)
	out["hosts_started"], out["hosts_returned"], out["hosts_not_returned"] = len(started), len(returned), missing
	return out, nil
}

// Diagnose explains a run in one call.
func (c *Client) Diagnose(ctx context.Context, id int) (*Diagnosis, error) {
	j, err := c.FindJob(ctx, id)
	if err != nil {
		return nil, err
	}
	d := &Diagnosis{Failures: []map[string]any{}, Hosts: []map[string]any{}, Nodes: []map[string]any{}}
	d.Job = map[string]any{"id": j.ID, "type": j.Type}
	for _, k := range []string{"name", "status", "failed", "started", "finished", "elapsed", "job_explanation", "limit", "launch_type", "playbook", "scm_revision", "job_type", "execution_node", "result_traceback"} {
		if v, ok := j.Obj[k]; ok && v != nil && v != "" {
			d.Job[k] = v
		}
	}
	for k, from := range map[string]string{"template": "summary_fields.unified_job_template.name", "inventory": "summary_fields.inventory.name",
		"project": "summary_fields.project.name", "launched_by": "launched_by.name",
		"execution_environment": "summary_fields.execution_environment.name", "schedule": "summary_fields.schedule.name"} {
		if v := Lookup(j.Obj, from); v != nil {
			d.Job[k] = v
		}
	}
	if s, ok := d.Job["result_traceback"].(string); ok {
		d.Job["result_traceback"] = clip(s, 4000)
	}

	if j.Type == "workflow_job" {
		nodes, _, err := c.List(ctx, j.Path+"workflow_nodes/", nil, 0)
		if err != nil {
			return nil, err
		}
		for _, n := range nodes {
			st, _ := Lookup(n, "summary_fields.job.status").(string)
			if st == "successful" || (st == "" && n["do_not_run"] == true) {
				continue
			}
			d.Nodes = append(d.Nodes, WorkflowNodeRow(n))
		}
	}
	if ep := j.EventsPath(); ep != "" && j.HasTaskEvents() {
		evs, total, err := c.List(ctx, ep, url.Values{"event__in": {"runner_on_failed,runner_on_unreachable,runner_item_on_failed"}, "order_by": {"counter"}}, maxFailureEvents)
		if err != nil {
			return nil, err
		}
		var ignored int
		d.Failures, ignored, d.failedHosts = groupFailures(evs)
		d.Job["failed_results"] = total - ignored
		if ignored > 0 {
			d.Job["ignored_errors"] = ignored
		}
		// Events are stored after the job ends: until that is done, failures can
		// be missing and a host can look as if it never returned.
		if j.Obj["event_processing_finished"] == false {
			d.Job["events_complete"] = false
		}
		if total > len(evs) {
			d.Job["failed_results_read"] = len(evs)
		}
	}
	if j.Type == "job" {
		sums, _, err := c.List(ctx, j.Path+"job_host_summaries/", url.Values{"failed": {"true"}}, 200)
		if err != nil {
			return nil, err
		}
		res := &Resource{Fields: HostSummaryFields[:9]}
		for _, s := range sums {
			d.Hosts = append(d.Hosts, res.Row(s))
		}
	}
	expl, _ := j.Obj["job_explanation"].(string)
	cutOff := j.Status() == "canceled" || strings.Contains(strings.ToLower(expl), "timeout") || strings.Contains(strings.ToLower(expl), "timed out")
	if j.HasTaskEvents() && Finished(j.Status()) && j.Status() != "successful" && (cutOff || len(d.Failures) == 0) {
		st, err := c.stalled(ctx, j)
		if err != nil {
			return nil, err
		}
		if st != nil {
			if missing, _ := st["hosts_not_returned"].([]string); len(missing) > 0 || cutOff {
				// Why the hosts did not return: the job was cut off, or the play
				// simply ended there (an abort, an error outside a task).
				switch {
				case j.Status() == "canceled":
					st["reason"] = "canceled"
				case cutOff:
					st["reason"] = "timeout"
				default:
					st["reason"] = "play ended early"
				}
				d.Stalled = st
			}
		}
	}
	if len(d.Failures) == 0 && len(d.Nodes) == 0 && j.Type != "workflow_job" && Finished(j.Status()) && j.Status() != "successful" {
		if text, _, err := c.Output(ctx, j, 0); err == nil {
			d.Errors = errorLines(text, 20)
			d.Tail = quietTail(text, 25)
		}
	}
	d.Verdict = verdict(j, d)
	return d, nil
}

func verdict(j *Job, d *Diagnosis) string {
	st := j.Status()
	expl, _ := j.Obj["job_explanation"].(string)
	stuck := ""
	if d.Stalled != nil {
		missing, _ := d.Stalled["hosts_not_returned"].([]string)
		shown := missing
		if len(shown) > 10 {
			shown = shown[:10]
		}
		list := strings.Join(shown, ", ")
		if len(missing) > len(shown) {
			list += ", ..."
		}
		if d.Stalled["reason"] == "play ended early" {
			// Not a hang: the play stopped while hosts were still in the task.
			stuck = fmt.Sprintf("; the play ended during task %q before %d of %v host(s) returned (not a timeout: a play abort or an error outside a task)", d.Stalled["last_task"], len(missing), d.Stalled["hosts_started"])
		} else {
			stuck = fmt.Sprintf("; the last task result is from %v in task %q", d.Stalled["last_result_at"], d.Stalled["last_task"])
			if len(missing) > 0 {
				stuck += fmt.Sprintf("; %d host(s) started it and never returned: %s", len(missing), list)
			}
		}
	}
	if len(d.Errors) > 0 {
		stuck += fmt.Sprintf("; the output has %d error line(s), first: %v", len(d.Errors), clip(fmt.Sprint(d.Errors[0]["text"]), 300))
	}
	why := ""
	if expl != "" {
		why = " (" + expl + ")"
	}
	if d.Job["events_complete"] == false {
		stuck += "; the controller is still storing this job's events, so this may be incomplete: ask again in a moment"
	}
	results := 0
	for _, f := range d.Failures {
		results += max(asInt(f["host_count"]), 1)
	}
	switch {
	case !Finished(st):
		return fmt.Sprintf("not finished yet: status %s", st)
	case st == "successful":
		if n := results + asInt(d.Job["ignored_errors"]); n > 0 {
			return fmt.Sprintf("successful, with %d task failure(s) that were ignored or rescued", n)
		}
		return "successful"
	case st == "canceled":
		return "canceled" + ifText(": ", expl) + stuck
	case len(d.Nodes) > 0:
		return fmt.Sprintf("%s: %d workflow node(s) did not succeed; diagnose their job IDs for the cause", st, len(d.Nodes))
	case len(d.Failures) > 0:
		first := d.Failures[0]
		hosts, _ := first["hosts"].([]string)
		where := fmt.Sprintf("on %d host(s)", len(hosts))
		if len(hosts) == 1 {
			where = "on host " + hosts[0]
		}
		return fmt.Sprintf("%s: %d failed task result(s) on %d host(s), %d distinct failure(s); first: task %q %s", st, results, max(d.failedHosts, 1), len(d.Failures), first["task"], where) + why + stuck
	case expl != "":
		return st + ": " + expl + stuck
	case j.Obj["result_traceback"] != nil && j.Obj["result_traceback"] != "":
		return st + ": the run crashed before any task ran, see job.result_traceback"
	}
	if len(d.Errors) > 0 || d.Stalled != nil {
		return st + ": no failed task was recorded" + stuck
	}
	return st + ": no failed task was recorded, see tail (the last output lines)"
}

func ifText(prefix, s string) string {
	if s == "" {
		return ""
	}
	return prefix + s
}

var (
	errorLineRE = regexp.MustCompile(`\[ERROR\]|ERROR!|^fatal: |FAILED! =>|UNREACHABLE! =>|^ERROR:`)
	// profile_tasks and timer callbacks: a timestamp line per task and a table of
	// task timings at the end. They say when, not what went wrong.
	timingLineRE = regexp.MustCompile(`^(\w+ \d{1,2} \w+ \d{4} +\d{2}:\d{2}:\d{2} [+-]\d{4} \(.*\) +[\d:.]+ \*+|.* -{3,} +\d+\.\d+s|={5,}|Playbook run took .*)$`)
)

// errorLines finds the lines of an output that report an error, each with the
// lines that follow it up to the next blank line (where Ansible says where and why).
func errorLines(text string, max int) []map[string]any {
	lines := strings.Split(text, "\n")
	var out []map[string]any
	for i := 0; i < len(lines) && len(out) < max; i++ {
		if !errorLineRE.MatchString(lines[i]) {
			continue
		}
		block := []string{strings.TrimRight(lines[i], "\r ")}
		j := i + 1
		for ; j < len(lines) && j < i+8 && strings.TrimSpace(lines[j]) != "" && !errorLineRE.MatchString(lines[j]); j++ {
			block = append(block, strings.TrimRight(lines[j], "\r "))
		}
		out = append(out, map[string]any{"line": i + 1, "text": clip(strings.Join(block, "\n"), 1500)})
		i = j - 1
	}
	return out
}

// quietTail returns the last n lines that say something: timing-only lines and
// blank lines are left out.
func quietTail(text string, n int) []string {
	var kept []string
	for _, l := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		l = strings.TrimRight(l, "\r ")
		if strings.TrimSpace(l) == "" || timingLineRE.MatchString(l) {
			continue
		}
		kept = append(kept, l)
	}
	if len(kept) > n {
		kept = kept[len(kept)-n:]
	}
	return kept
}

// compareFields are what "what changed since it last worked" looks at.
var compareFields = []string{"scm_revision", "scm_branch", "playbook", "limit", "job_type", "job_tags", "skip_tags", "forks", "verbosity", "launch_type", "diff_mode"}

// CompareLastSuccess finds the newest successful run of the same template before
// this job and reports what differs: revision, limit, inventory, execution
// environment, extra variables (by name), host counts and runtime.
func (c *Client) CompareLastSuccess(ctx context.Context, d *Diagnosis, id int) error {
	j, err := c.FindJob(ctx, id)
	if err != nil {
		return err
	}
	tmplField := map[string]string{"job": "job_template", "workflow_job": "workflow_job_template"}[j.Type]
	tmpl := asInt(j.Obj[tmplField])
	if tmplField == "" || tmpl == 0 {
		d.Compare = map[string]any{"found": false, "note": "only a job or workflow job that still has its template can be compared"}
		return nil
	}
	base := jobPaths[j.Type]
	prev, _, err := c.List(ctx, base, url.Values{tmplField: {strconv.Itoa(tmpl)}, "status": {"successful"}, "id__lt": {strconv.Itoa(id)}, "order_by": {"-id"}}, 1)
	if err != nil {
		return err
	}
	if len(prev) == 0 {
		d.Compare = map[string]any{"found": false, "note": "no earlier successful run of this template is on record"}
		return nil
	}
	p := prev[0]
	diff, unchanged := map[string]any{}, []string{}
	put := func(name string, a, b any) {
		if same(a, b) {
			unchanged = append(unchanged, name)
			return
		}
		diff[name] = map[string]any{"this": a, "last_success": b}
	}
	for _, f := range compareFields {
		if _, ok := j.Obj[f]; ok {
			put(f, j.Obj[f], p[f])
		}
	}
	for name, path := range map[string]string{"inventory": "summary_fields.inventory.name", "project": "summary_fields.project.name",
		"execution_environment": "summary_fields.execution_environment.name", "launched_by": "launched_by.name"} {
		put(name, Lookup(j.Obj, path), Lookup(p, path))
	}
	// extra variables: which names differ, never the values
	vars := func(o Object) map[string]any {
		var m map[string]any
		if s, _ := o["extra_vars"].(string); s != "" {
			_ = decodeJSON([]byte(s), &m)
		}
		return m
	}
	a, b := vars(j.Obj), vars(p)
	var changed []string
	for k, v := range a {
		if w, ok := b[k]; !ok || !same(v, w) {
			changed = append(changed, k)
		}
	}
	for k := range b {
		if _, ok := a[k]; !ok {
			changed = append(changed, k)
		}
	}
	if len(changed) > 0 {
		sort.Strings(changed)
		diff["extra_vars"] = map[string]any{"names_that_differ": changed}
	} else {
		unchanged = append(unchanged, "extra_vars")
	}
	if j.Type == "job" {
		count := func(jobID int) int {
			_, n, err := c.List(ctx, base+strconv.Itoa(jobID)+"/job_host_summaries/", nil, 1)
			if err != nil {
				return -1
			}
			return n
		}
		put("hosts", count(id), count(ID(p)))
	}
	sort.Strings(unchanged)
	d.Compare = map[string]any{"found": true, "differences": diff, "same": unchanged,
		"last_success": map[string]any{"id": ID(p), "finished": p["finished"], "elapsed": p["elapsed"], "scm_revision": p["scm_revision"]},
		"this":         map[string]any{"id": id, "elapsed": j.Obj["elapsed"], "status": j.Status()}}
	names := make([]string, 0, len(diff))
	for k := range diff {
		names = append(names, k)
	}
	sort.Strings(names)
	if len(names) == 0 {
		d.Verdict += fmt.Sprintf("; nothing in the job's settings differs from the last successful run (job %d)", ID(p))
	} else {
		d.Verdict += fmt.Sprintf("; differs from the last successful run (job %d) in: %s", ID(p), strings.Join(names, ", "))
	}
	return nil
}

// WorkflowNodeFields are the flat fields of a workflow job node.
var WorkflowNodeFields = []Field{
	f("id", "NODE"), ff("template", "TEMPLATE", "summary_fields.unified_job_template.name"),
	ff("type", "TYPE", "summary_fields.unified_job_template.unified_job_type"),
	ff("job_id", "JOB", "summary_fields.job.id"), ff("status", "STATUS", "summary_fields.job.status"),
	ff("failed", "FAILED", "summary_fields.job.failed"), ff("elapsed", "SECONDS", "summary_fields.job.elapsed"),
	f("do_not_run", "SKIPPED"), w("identifier"), w("success_nodes"), w("failure_nodes"), w("always_nodes"),
}

// WorkflowNodeRow flattens one workflow node.
func WorkflowNodeRow(o Object) map[string]any {
	return (&Resource{Fields: WorkflowNodeFields}).Row(o)
}

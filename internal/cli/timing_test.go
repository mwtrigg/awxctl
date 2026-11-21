package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/mwtrigg/awxctl/internal/fakeawx"
)

func decode(t *testing.T, out string) (rows []map[string]any, meta map[string]any) {
	t.Helper()
	var doc struct {
		Data []map[string]any `json:"data"`
		Meta map[string]any   `json:"meta"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("standard output is not one JSON document: %v\n%s", err, out)
	}
	return doc.Data, doc.Meta
}

// Which host held the play: every task result has its duration, and --slowest
// puts the long ones first.
func TestJobEventsHaveDurationsAndSlowest(t *testing.T) {
	srv, env := lab(t, "awx", "readonly")
	srv.Add("/unified_jobs/", map[string]any{"id": 700, "type": "job"})
	srv.Add("/jobs/", map[string]any{"id": 700, "name": "Patch", "status": "successful"})
	ev := func(n int, host, task string, data map[string]any) map[string]any {
		data["task_action"] = "command"
		return map[string]any{"id": n, "counter": n, "event": "runner_on_ok", "host_name": host, "task": task, "event_data": data}
	}
	var asked string
	srv.Handle("/jobs/700/job_events/", func(w http.ResponseWriter, r *http.Request) {
		asked = r.URL.RawQuery
		fakeawx.JSON(w, 200, map[string]any{"count": 4, "results": []map[string]any{
			ev(1, "web1", "Install", map[string]any{"duration": 2.25}),
			ev(2, "web2", "Wait for the update", map[string]any{"duration": 2220.0004}),
			// no duration recorded: the start and end are used
			ev(3, "web1", "Wait for the update", map[string]any{"start": "2026-03-01T10:00:00.000000", "end": "2026-03-01T10:01:30.500000"}),
			{"id": 4, "counter": 4, "event": "playbook_on_task_start", "task": "Install", "event_data": map[string]any{}},
		}})
	})
	out, errOut, code := runCLI(t, env, "get", "job-events", "700")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	rows, _ := decode(t, out)
	if rows[0]["duration"] != 2.25 || rows[1]["duration"] != 2220.0 || rows[2]["duration"] != 90.5 || rows[3]["duration"] != nil {
		t.Errorf("durations: %v %v %v %v", rows[0]["duration"], rows[1]["duration"], rows[2]["duration"], rows[3]["duration"])
	}
	out, errOut, code = runCLI(t, env, "get", "job-events", "700", "--slowest", "2", "--fields", "host,task,duration")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	rows, meta := decode(t, out)
	if len(rows) != 2 || rows[0]["host"] != "web2" || rows[0]["duration"] != 2220.0 || rows[1]["host"] != "web1" || rows[1]["duration"] != 90.5 {
		t.Errorf("the slowest two, slowest first: %v", rows)
	}
	if !strings.Contains(asked, "event__in=runner_on_ok") || !strings.Contains(asked, "page_size") {
		t.Errorf("--slowest reads task results only: %s", asked)
	}
	if notes := fmt.Sprint(meta["notes"]); !strings.Contains(notes, "slowest") {
		t.Errorf("meta.notes says what was read: %v", meta)
	}
	if _, errOut, code := runCLI(t, env, "get", "job-events", "700", "--slowest", "0"); code != 5 || !strings.Contains(errOut, "how many") {
		t.Errorf("--slowest 0: %d %s", code, errOut)
	}
}

// A note must never break the JSON on standard output, whatever the caller does
// with standard error: in JSON it is part of meta.
func TestNotesAreInTheJSONNotBesideIt(t *testing.T) {
	srv, env := lab(t, "awx", "readonly")
	srv.Add("/hosts/", map[string]any{"id": 40, "name": "web1", "inventory": 9, "summary_fields": map[string]any{"inventory": map[string]any{"name": "other"}}})
	sum := map[string]any{"count": 1, "results": []map[string]any{{"host_name": "web1", "summary_fields": map[string]any{"job": map[string]any{"id": 1, "name": "j", "status": "successful"}}}}}
	srv.Handle("/hosts/3/job_host_summaries/", func(w http.ResponseWriter, _ *http.Request) { fakeawx.JSON(w, 200, sum) })
	srv.Handle("/hosts/40/job_host_summaries/", func(w http.ResponseWriter, _ *http.Request) { fakeawx.JSON(w, 200, sum) })
	out, errOut, code := runCLI(t, env, "get", "host-jobs", "web1")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	_, meta := decode(t, out+errOut) // as a caller that merges the two streams sees it
	if notes := fmt.Sprint(meta["notes"]); !strings.Contains(notes, "2 hosts in different inventories") {
		t.Errorf("the note belongs in meta.notes: %v", meta)
	}
	// A table has no meta: there the note goes to standard error.
	out, errOut, _ = runCLI(t, env, "get", "host-jobs", "web1", "-o", "table")
	if strings.Contains(out, "different inventories") || !strings.Contains(errOut, "note: 2 hosts in different inventories") {
		t.Errorf("table: the note goes to stderr:\n%s\n%s", out, errOut)
	}
	// Unknown field names are an error that lists the real ones.
	_, errOut, code = runCLI(t, env, "get", "host-jobs", "web1", "--fields", "id,name,status,finished")
	if code != 5 || !strings.Contains(errOut, `no such field \"id\"`) || !strings.Contains(errOut, "job_id") {
		t.Errorf("unknown --fields: %d %s", code, errOut)
	}
}

// A weekly workflow three levels deep: one call names the job that broke.
func TestDiagnoseRecursiveFollowsNestedWorkflows(t *testing.T) {
	srv, env := lab(t, "awx", "readonly")
	job := func(id int, typ, name, status string) {
		srv.Add("/unified_jobs/", map[string]any{"id": id, "type": typ})
		coll := "/jobs/"
		if typ == "workflow_job" {
			coll = "/workflow_jobs/"
		}
		srv.Add(coll, map[string]any{"id": id, "name": name, "status": status, "failed": status == "failed"})
	}
	node := func(id, jobID int, name, typ, status string) map[string]any {
		n := map[string]any{"id": id, "summary_fields": map[string]any{"unified_job_template": map[string]any{"name": name, "unified_job_type": typ}}}
		if jobID != 0 {
			n["summary_fields"].(map[string]any)["job"] = map[string]any{"id": jobID, "status": status, "failed": status == "failed"}
		} else {
			n["do_not_run"] = false
		}
		return n
	}
	nodes := func(wf int, list ...map[string]any) {
		srv.Handle(fmt.Sprintf("/workflow_jobs/%d/workflow_nodes/", wf), func(w http.ResponseWriter, _ *http.Request) {
			fakeawx.JSON(w, 200, map[string]any{"count": len(list), "results": list})
		})
	}
	job(900, "workflow_job", "Weekly", "failed")
	job(910, "workflow_job", "Patch region", "failed")
	job(920, "workflow_job", "Patch batch", "failed")
	job(930, "job", "Apply updates", "failed")
	job(931, "job", "Reboot", "successful")
	nodes(900, node(1, 910, "Patch region", "workflow_job", "failed"), node(2, 931, "Reboot", "job", "successful"), node(3, 0, "Report", "job", ""))
	nodes(910, node(4, 920, "Patch batch", "workflow_job", "failed"))
	nodes(920, node(5, 930, "Apply updates", "job", "failed"))
	srv.Handle("/jobs/930/job_events/", func(w http.ResponseWriter, _ *http.Request) {
		fakeawx.JSON(w, 200, map[string]any{"count": 1, "results": []map[string]any{{"id": 1, "counter": 9, "event": "runner_on_failed", "failed": true, "host_name": "web1",
			"task": "Install updates", "play": "Patch", "event_data": map[string]any{"res": map[string]any{"msg": "no space left on device"}}}}})
	})
	srv.Handle("/jobs/930/job_host_summaries/", func(w http.ResponseWriter, _ *http.Request) {
		fakeawx.JSON(w, 200, map[string]any{"count": 0, "results": []any{}})
	})
	// Without the flag: as before, the nodes and a pointer.
	out, errOut, code := runCLI(t, env, "diagnose", "job", "900")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	rows, _ := decode(t, out)
	if _, has := rows[0]["leaf_failures"]; has || !strings.Contains(fmt.Sprint(rows[0]["verdict"]), "diagnose their job IDs") {
		t.Errorf("without --recursive nothing changes: %v", rows[0]["verdict"])
	}
	out, errOut, code = runCLI(t, env, "diagnose", "job", "900", "--recursive")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	rows, _ = decode(t, out)
	leaves, _ := rows[0]["leaf_failures"].([]any)
	if len(leaves) != 1 {
		t.Fatalf("one job is at the end of the failed branch: %v", rows[0]["leaf_failures"])
	}
	leaf := leaves[0].(map[string]any)
	if leaf["job_id"] != float64(930) || leaf["path"] != "Patch region > Patch batch > Apply updates" || !strings.Contains(fmt.Sprint(leaf["verdict"]), "Install updates") || !strings.Contains(fmt.Sprint(leaf["failures"]), "no space left") {
		t.Errorf("the leaf: %v", leaf)
	}
	v := fmt.Sprint(rows[0]["verdict"])
	if !strings.Contains(v, "job 930") || !strings.Contains(v, "Install updates") || !strings.Contains(v, "1 node(s) did not run a job") {
		t.Errorf("the verdict names the job that broke: %s", v)
	}
	// A plain job is diagnosed as ever.
	out, _, code = runCLI(t, env, "diagnose", "job", "930", "--recursive")
	if rows, _ = decode(t, out); code != 0 || rows[0]["leaf_failures"] != nil || !strings.Contains(fmt.Sprint(rows[0]["verdict"]), "Install updates") {
		t.Errorf("--recursive on a plain job: %d %v", code, rows[0]["verdict"])
	}
}

// Where the time went: per task, per play and per host, from the task results.
func TestJobTiming(t *testing.T) {
	srv, env := lab(t, "awx", "readonly")
	srv.Add("/unified_jobs/", map[string]any{"id": 710, "type": "job"})
	srv.Add("/jobs/", map[string]any{"id": 710, "name": "Patch", "status": "successful"})
	srv.Add("/unified_jobs/", map[string]any{"id": 711, "type": "workflow_job"})
	srv.Add("/workflow_jobs/", map[string]any{"id": 711, "name": "Weekly", "status": "successful"})
	n := 0
	ev := func(event, host, play, task, taskID string, changed bool, start, end string) map[string]any {
		n++
		return map[string]any{"id": n, "counter": n, "event": event, "host_name": host, "play": play, "task": task, "changed": changed,
			"event_data": map[string]any{"task_uuid": taskID, "play_uuid": "p-" + play, "start": "2026-03-01T10:" + start, "end": "2026-03-01T10:" + end}}
	}
	var asked string
	srv.Handle("/jobs/710/job_events/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("event__in") == "" { // the reads of the recap check
			fakeawx.JSON(w, 200, map[string]any{"count": 0, "results": []any{}})
			return
		}
		asked = r.URL.RawQuery
		fakeawx.JSON(w, 200, map[string]any{"count": 6, "results": []map[string]any{
			ev("runner_on_ok", "web1", "Patch", "Install", "t1", true, "00:00.000000", "00:10.000000"),
			ev("runner_on_ok", "web2", "Patch", "Install", "t1", false, "00:00.000000", "00:04.000000"),
			ev("runner_on_ok", "web1", "Patch", "Wait", "t2", false, "00:10.000000", "00:40.000000"),
			ev("runner_on_failed", "web2", "Patch", "Wait", "t2", false, "00:10.000000", "05:10.000000"),
			ev("runner_on_skipped", "web1", "Report", "Mail", "t3", false, "05:10.000000", "05:10.500000"),
			// the same task name again in another play is another task
			ev("runner_on_ok", "web1", "Report", "Install", "t4", true, "05:11.000000", "05:12.000000"),
		}})
	})
	out, errOut, code := runCLI(t, env, "get", "job-timing", "710")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	rows, meta := decode(t, out)
	if len(rows) != 4 || rows[0]["task"] != "Install" || rows[1]["task"] != "Wait" || rows[3]["play"] != "Report" {
		t.Fatalf("one row per task, in the order they ran: %v", rows)
	}
	install, wait := rows[0], rows[1]
	if install["wall_seconds"] != 10.0 || install["host_seconds"] != 14.0 || install["slowest_host"] != "web1" || install["changed"] != 1.0 || install["unchanged"] != 1.0 || install["hosts"] != 2.0 {
		t.Errorf("Install: %v", install)
	}
	if wait["wall_seconds"] != 300.0 || wait["host_seconds"] != 330.0 || wait["slowest_host"] != "web2" || wait["slowest_seconds"] != 300.0 || wait["failed"] != 1.0 {
		t.Errorf("Wait: %v", wait)
	}
	if !strings.Contains(asked, "event__in=runner_on_ok") || !strings.Contains(fmt.Sprint(meta["notes"]), "6 task results") || meta["total"] != 4.0 {
		t.Errorf("asked %s, meta %v", asked, meta)
	}
	out, _, _ = runCLI(t, env, "get", "job-timing", "710", "--sort", "wall", "--limit", "1")
	if rows, meta = decode(t, out); len(rows) != 1 || rows[0]["task"] != "Wait" || meta["limit_reached"] != true {
		t.Errorf("--sort wall --limit 1: %v %v", rows, meta)
	}
	out, _, _ = runCLI(t, env, "get", "job-timing", "710", "--changed")
	if rows, _ = decode(t, out); len(rows) != 2 || rows[1]["play"] != "Report" {
		t.Errorf("--changed: %v", rows)
	}
	out, _, _ = runCLI(t, env, "get", "job-timing", "710", "--by", "play")
	if rows, _ = decode(t, out); len(rows) != 2 || rows[0]["play"] != "Patch" || rows[0]["wall_seconds"] != 310.0 || rows[0]["tasks"] != 2.0 || rows[0]["hosts"] != 2.0 || rows[0]["slowest_host"] != "web2" {
		t.Errorf("--by play: %v", rows)
	}
	// Per host, the costliest first: which host held the play.
	out, _, _ = runCLI(t, env, "get", "job-timing", "710", "--by", "host")
	if rows, _ = decode(t, out); len(rows) != 2 || rows[0]["host"] != "web2" || rows[0]["host_seconds"] != 304.0 || rows[0]["slowest_task"] != "Wait" || rows[1]["tasks"] != 4.0 {
		t.Errorf("--by host: %v", rows)
	}
	for _, c := range []struct {
		want string
		args []string
	}{
		{"--by is task, play, role or host", []string{"get", "job-timing", "710", "--by", "weekday"}},
		{"--sort is run", []string{"get", "job-timing", "710", "--sort", "name"}},
		{"only a playbook run", []string{"get", "job-timing", "711"}},
	} {
		if _, errOut, code := runCLI(t, env, c.args...); code != 5 || !strings.Contains(errOut, c.want) {
			t.Errorf("%v: %d %s (want %q)", c.args, code, errOut, c.want)
		}
	}
}

// A play that runs its hosts in serial batches repeats every task under the same
// task ID. Each batch is a row of its own; merged, the wall clock of the task
// would span everything that ran between the batches.
func TestJobTimingSerialBatchesRolesAndSummary(t *testing.T) {
	srv, env := lab(t, "awx", "readonly")
	srv.Add("/unified_jobs/", map[string]any{"id": 720, "type": "job"})
	srv.Add("/jobs/", map[string]any{"id": 720, "name": "Rolling", "status": "failed", "elapsed": 700.5})
	n := 0
	ev := func(event, host, playID, role, task, taskID string, changed bool, start, end string) map[string]any {
		n++
		return map[string]any{"id": n, "counter": n, "event": event, "host_name": host, "play": "Rolling restart", "role": role, "task": task, "changed": changed,
			"event_data": map[string]any{"task_uuid": taskID, "play_uuid": playID, "start": "2026-03-01T10:" + start, "end": "2026-03-01T10:" + end}}
	}
	srv.Handle("/jobs/720/job_events/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("event__in") == "" { // loop items, includes: none here
			fakeawx.JSON(w, 200, map[string]any{"count": 0, "results": []any{}})
			return
		}
		fakeawx.JSON(w, 200, map[string]any{"count": 7, "results": []map[string]any{
			// batch 1: web1
			ev("runner_on_ok", "web1", "batch-1", "reboot", "Reboot", "t1", true, "00:00.000000", "01:00.000000"),
			ev("runner_on_ok", "web1", "batch-1", "reboot", "Wait for the host", "t2", false, "01:00.000000", "03:00.000000"),
			ev("runner_on_skipped", "web1", "batch-1", "", "Notify", "t3", false, "03:00.000000", "03:00.100000"),
			// batch 2: web2, the same tasks again, five minutes later
			ev("runner_on_ok", "web2", "batch-2", "reboot", "Reboot", "t1", true, "05:00.000000", "06:00.000000"),
			ev("runner_on_failed", "web2", "batch-2", "reboot", "Wait for the host", "t2", false, "06:00.000000", "11:00.000000"),
		}})
	})
	out, errOut, code := runCLI(t, env, "get", "job-timing", "720")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	rows, meta := decode(t, out)
	if len(rows) != 5 {
		t.Fatalf("one row per task per batch: %d rows %v", len(rows), rows)
	}
	w1, w2 := rows[1], rows[4]
	if w1["task"] != "Wait for the host" || w1["wall_seconds"] != 120.0 || w1["hosts"] != 1.0 || w1["play_run"] != 1.0 ||
		w2["task"] != "Wait for the host" || w2["wall_seconds"] != 300.0 || w2["play_run"] != 2.0 || w2["failed"] != 1.0 {
		t.Errorf("the two batches of one task, each with its own wall clock:\n%v\n%v", w1, w2)
	}
	// What the time went on: each task once, under one heading.
	sum, _ := meta["summary"].(map[string]any)
	secs, _ := sum["seconds_by_outcome"].(map[string]any)
	counts, _ := sum["tasks_by_outcome"].(map[string]any)
	if secs["changed"] != 120.0 || secs["unchanged"] != 120.0 || secs["failed"] != 300.0 || secs["skipped"] != 0.1 || sum["task_seconds"] != 540.1 || sum["job_seconds"] != 700.5 ||
		counts["changed"] != 2.0 || counts["failed"] != 1.0 || counts["skipped"] != 1.0 || counts["unchanged"] != 1.0 {
		t.Errorf("summary: %v", sum)
	}
	// Per role: the role's tasks summed over both batches; tasks outside a role apart.
	out, _, _ = runCLI(t, env, "get", "job-timing", "720", "--by", "role")
	rows, meta = decode(t, out)
	if len(rows) != 2 || rows[0]["role"] != "reboot" || rows[0]["wall_seconds"] != 540.0 || rows[0]["tasks"] != 4.0 || rows[0]["slowest_task"] != "Wait for the host" || rows[0]["changed"] != 2.0 ||
		rows[1]["role"] != "" || rows[1]["skipped"] != 1.0 {
		t.Errorf("--by role: %v", rows)
	}
	if s2, _ := meta["summary"].(map[string]any); s2["task_seconds"] != 540.1 {
		t.Errorf("the summary is about the run, whatever the grouping: %v", meta["summary"])
	}
	// --by play: the two batches are two runs of the play.
	out, _, _ = runCLI(t, env, "get", "job-timing", "720", "--by", "play")
	if rows, _ = decode(t, out); len(rows) != 2 || rows[0]["play_run"] != 1.0 || rows[1]["play_run"] != 2.0 || rows[1]["wall_seconds"] != 360.0 {
		t.Errorf("--by play: %v", rows)
	}
	out, errOut, _ = runCLI(t, env, "get", "job-timing", "720", "-o", "table")
	if !strings.Contains(errOut, "summary: 5 tasks, 540 s in tasks: changed 120 s (22%, 2 tasks)") || !strings.Contains(out, "RUN") {
		t.Errorf("the table view:\n%s\n%s", out, errOut)
	}
	// With --host the summary says it is partial.
	out, _, _ = runCLI(t, env, "get", "job-timing", "720", "--host", "web1")
	if _, meta = decode(t, out); meta["summary"].(map[string]any)["partial"] == nil || meta["summary"].(map[string]any)["job_seconds"] != nil {
		t.Errorf("--host: %v", meta["summary"])
	}
}

// The task results, counted the way Ansible's recap counts, agree with the
// run's own count per host; when they do not, the summary says on which hosts.
func TestJobTimingRecapCheck(t *testing.T) {
	srv, env := lab(t, "awx", "readonly")
	srv.Add("/unified_jobs/", map[string]any{"id": 730, "type": "job"})
	srv.Add("/jobs/", map[string]any{"id": 730, "name": "Patch", "status": "successful", "elapsed": 60.0})
	n := 0
	ev := func(event, host, task, action string, changed bool, extra map[string]any) map[string]any {
		n++
		data := map[string]any{"task_uuid": task, "play_uuid": "p1", "task_action": action, "start": "2026-03-01T10:00:00.000000", "end": "2026-03-01T10:00:01.000000"}
		for k, v := range extra {
			data[k] = v
		}
		return map[string]any{"id": n, "counter": n, "event": event, "host_name": host, "play": "Patch", "task": task, "changed": changed, "event_data": data}
	}
	results := []map[string]any{
		ev("runner_on_ok", "web1", "install", "package", true, nil),
		ev("runner_on_ok", "web1", "facts", "setup", false, nil),
		// a skipped meta task is reported, but the recap does not count it
		ev("runner_on_skipped", "web1", "stop here", "ansible.builtin.meta", false, nil),
		ev("runner_on_skipped", "web1", "optional", "command", false, nil),
		// a failure that is ignored is "ok" in the recap, not "failed"
		ev("runner_on_failed", "web1", "probe", "command", false, map[string]any{"ignore_errors": true}),
		// include_role leaves a task result AND an include: counted once
		ev("runner_on_ok", "web1", "role", "ansible.builtin.include_role", false, nil),
		// a loop whose item changed something, while the task's own result is not "changed"
		ev("runner_on_failed", "web1", "loop", "copy", false, map[string]any{"ignore_errors": true}),
		ev("runner_on_ok", "web2", "install", "package", false, nil),
		ev("runner_on_failed", "web2", "probe", "command", false, nil),
		ev("runner_on_unreachable", "web3", "install", "package", false, nil),
		ev("runner_on_ok", "web4", "install", "package", false, nil),
	}
	includes := []map[string]any{
		{"id": 900, "counter": 900, "event": "playbook_on_include", "play": "Patch", "task": "role", "stdout": "included: /runner/project/roles/x/tasks/main.yml for web1", "event_data": map[string]any{"task_uuid": "role"}},
		// include_tasks: no task result, one "ok" per host named; a looped include names the item
		{"id": 901, "counter": 901, "event": "playbook_on_include", "play": "Patch", "task": "more", "stdout": "\x1b[0;36mincluded: /runner/project/more.yml for web1, web2 => (item=a)\x1b[0m", "event_data": map[string]any{"task_uuid": "more"}},
	}
	items := []map[string]any{
		{"id": 950, "counter": 950, "event": "runner_item_on_ok", "host_name": "web1", "changed": true, "play": "Patch", "task": "loop", "event_data": map[string]any{"task_uuid": "loop"}},
		// an item of a task that says "changed" itself adds nothing
		{"id": 951, "counter": 951, "event": "runner_item_on_ok", "host_name": "web1", "changed": true, "play": "Patch", "task": "install", "event_data": map[string]any{"task_uuid": "install"}},
	}
	srv.Handle("/jobs/730/job_events/", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		list := results
		switch {
		case q.Get("event") == "playbook_on_include":
			list = includes
		case q.Get("event") == "runner_item_on_ok":
			if q.Get("changed") != "true" {
				t.Errorf("only the changed items are read: %s", r.URL.RawQuery)
			}
			list = items
		}
		fakeawx.JSON(w, 200, map[string]any{"count": len(list), "results": list})
	})
	recap := []map[string]any{
		// web1: ok = install + facts + probe (ignored) + role (once) + loop (ignored) + include of more.yml = 6
		{"host_name": "web1", "ok": 6, "changed": 2, "failures": 0, "skipped": 1, "dark": 0, "ignored": 2},
		{"host_name": "web2", "ok": 2, "changed": 0, "failures": 1, "skipped": 0, "dark": 0},
		{"host_name": "web3", "ok": 0, "changed": 0, "failures": 0, "skipped": 0, "dark": 1},
		// a rescue cannot be read from the events with certainty: left out, not called a mismatch
		{"host_name": "web4", "ok": 3, "changed": 0, "failures": 0, "skipped": 0, "dark": 0, "rescued": 1},
	}
	srv.Handle("/jobs/730/job_host_summaries/", func(w http.ResponseWriter, _ *http.Request) {
		fakeawx.JSON(w, 200, map[string]any{"count": len(recap), "results": recap})
	})
	check := func() map[string]any {
		t.Helper()
		out, errOut, code := runCLI(t, env, "get", "job-timing", "730")
		if code != 0 {
			t.Fatalf("exit %d: %s", code, errOut)
		}
		_, meta := decode(t, out)
		rc, _ := meta["summary"].(map[string]any)["recap_check"].(map[string]any)
		return rc
	}
	if rc := check(); rc["matches"] != true || rc["hosts"] != 4.0 || rc["hosts_matching"] != 3.0 || rc["hosts_unverifiable"] != 1.0 {
		t.Errorf("every rule applied, the three verifiable hosts agree: %v", rc)
	}
	// A result that went missing shows as a host that does not add up.
	recap[1]["ok"] = 5
	rc := check()
	if rc["matches"] != false || rc["hosts_differing"] != 1.0 || !strings.Contains(fmt.Sprint(rc["first_differences"]), "web2") || !strings.Contains(fmt.Sprint(rc["first_differences"]), "recap:5") {
		t.Errorf("a host that does not add up is named: %v", rc)
	}
	_, errOut, _ := runCLI(t, env, "get", "job-timing", "730", "-o", "table")
	if !strings.Contains(errOut, "on 1 of 4 hosts the task results do not add up") {
		t.Errorf("a person is told too: %s", errOut)
	}
	// A run that has not finished has no recap to compare with.
	srv.Collection("/jobs/")[len(srv.Collection("/jobs/"))-1]["status"] = "running"
	if rc := check(); rc != nil {
		t.Errorf("no check while the job runs: %v", rc)
	}
}

// A loop reports per item. Its own final result can say "not changed" although
// every item changed something: the task did change that host, and its time is
// time spent changing things.
func TestJobTimingLoopItemsMakeATaskChanged(t *testing.T) {
	srv, env := lab(t, "awx", "readonly")
	srv.Add("/unified_jobs/", map[string]any{"id": 740, "type": "job"})
	srv.Add("/jobs/", map[string]any{"id": 740, "name": "Cleanup", "status": "successful", "elapsed": 2200.0})
	res := func(n int, host, task, id string, changed bool, start, end string) map[string]any {
		return map[string]any{"id": n, "counter": n, "event": "runner_on_ok", "host_name": host, "play": "Cleanup", "role": "cleanup", "task": task, "changed": changed,
			"event_data": map[string]any{"task_uuid": id, "play_uuid": "p1", "start": "2026-03-01T10:" + start, "end": "2026-03-01T10:" + end}}
	}
	var itemQuery string
	srv.Handle("/jobs/740/job_events/", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		switch {
		case q.Get("event") == "runner_item_on_ok":
			itemQuery = r.URL.RawQuery
			fakeawx.JSON(w, 200, map[string]any{"count": 2, "results": []map[string]any{
				{"id": 90, "counter": 90, "event": "runner_item_on_ok", "host_name": "web1", "changed": true, "play": "Cleanup", "task": "Delete old folders", "event_data": map[string]any{"task_uuid": "t1"}},
				{"id": 91, "counter": 91, "event": "runner_item_on_ok", "host_name": "web1", "changed": true, "play": "Cleanup", "task": "Delete old folders", "event_data": map[string]any{"task_uuid": "t1"}},
			}})
		case q.Get("event__in") != "":
			fakeawx.JSON(w, 200, map[string]any{"count": 3, "results": []map[string]any{
				// the loop: its own result says not changed
				res(1, "web1", "Delete old folders", "t1", false, "00:00.000000", "36:00.000000"),
				// the same task on a host where no item changed
				res(2, "web2", "Delete old folders", "t1", false, "00:00.000000", "00:05.000000"),
				res(3, "web1", "Report", "t2", false, "36:00.000000", "36:10.000000"),
			}})
		default:
			fakeawx.JSON(w, 200, map[string]any{"count": 0, "results": []any{}})
		}
	})
	srv.Handle("/jobs/740/job_host_summaries/", func(w http.ResponseWriter, _ *http.Request) {
		fakeawx.JSON(w, 200, map[string]any{"count": 2, "results": []map[string]any{
			{"host_name": "web1", "ok": 2, "changed": 1, "failures": 0, "skipped": 0, "dark": 0},
			{"host_name": "web2", "ok": 1, "changed": 0, "failures": 0, "skipped": 0, "dark": 0}}})
	})
	out, errOut, code := runCLI(t, env, "get", "job-timing", "740")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	rows, meta := decode(t, out)
	loop := rows[0]
	if loop["task"] != "Delete old folders" || loop["changed"] != 1.0 || loop["unchanged"] != 1.0 || loop["wall_seconds"] != 2160.0 {
		t.Errorf("the loop changed web1 and not web2: %v", loop)
	}
	sum := meta["summary"].(map[string]any)
	secs := sum["seconds_by_outcome"].(map[string]any)
	if secs["changed"] != 2160.0 || secs["unchanged"] != 10.0 {
		t.Errorf("the loop's time is time spent changing things: %v", secs)
	}
	if rc := sum["recap_check"].(map[string]any); rc["matches"] != true {
		t.Errorf("and the recap still agrees: %v", rc)
	}
	// The same from the other views.
	out, _, _ = runCLI(t, env, "get", "job-timing", "740", "--by", "host")
	if rows, _ = decode(t, out); rows[0]["host"] != "web1" || rows[0]["changed"] != 1.0 || rows[0]["unchanged"] != 1.0 || rows[1]["changed"] != 0.0 {
		t.Errorf("--by host: %v", rows)
	}
	out, _, _ = runCLI(t, env, "get", "job-timing", "740", "--by", "role")
	if rows, _ = decode(t, out); rows[0]["changed"] != 1.0 || rows[0]["unchanged"] != 2.0 {
		t.Errorf("--by role: %v", rows)
	}
	if out, _, _ = runCLI(t, env, "get", "job-timing", "740", "--changed"); len(func() []map[string]any { r, _ := decode(t, out); return r }()) != 1 {
		t.Errorf("--changed keeps the loop: %s", out)
	}
	// --host and --task narrow the item read too.
	runCLI(t, env, "get", "job-timing", "740", "--host", "web1", "--task", "Delete")
	if !strings.Contains(itemQuery, "host_name=web1") || !strings.Contains(itemQuery, "task__icontains=Delete") || !strings.Contains(itemQuery, "changed=true") {
		t.Errorf("the item read follows the filters: %s", itemQuery)
	}
}

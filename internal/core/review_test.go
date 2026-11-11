package core

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mwtrigg/awxctl/internal/fakeawx"
)

// Tests added after the 2026-11-11 review: behaviours that were claimed and not
// pinned by any test.

func TestUpdateIsVerified(t *testing.T) {
	c, srv := liveClient(t, "awx")
	srv.Add("/hosts/", map[string]any{"id": 3, "name": "web1", "description": "old", "enabled": true})
	hosts := FindResource("hosts")
	ctx := context.Background()
	// The controller answers 200 but did not take the value.
	srv.Handle("/hosts/3/", func(w http.ResponseWriter, r *http.Request) {
		fakeawx.JSON(w, 200, map[string]any{"id": 3, "name": "web1", "description": "old", "enabled": true})
	})
	p, err := c.PlanUpdate(ctx, hosts, []string{"id:3"}, nil, map[string]any{"description": "new"})
	if err != nil {
		t.Fatal(err)
	}
	var uo *OutcomeUnknownError
	if err := p.Apply(ctx, c, nil); !errors.As(err, &uo) || p.Steps[0].Status != "unknown" {
		t.Errorf("an update the controller did not take must not end as applied: %v %s", err, p.Steps[0].Status)
	}
}

func TestUpdateOfTextWithANumberIsNotAFalseAlarm(t *testing.T) {
	// A text field stores 42 as "42": that is the change that was asked for.
	c, srv := liveClient(t, "awx")
	srv.Add("/hosts/", map[string]any{"id": 3, "name": "web1", "description": "old"})
	srv.Handle("/hosts/3/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			fakeawx.JSON(w, 200, map[string]any{"id": 3, "name": "web1", "description": "42"})
			return
		}
		fakeawx.JSON(w, 200, map[string]any{"id": 3, "name": "web1", "description": "old"})
	})
	ctx := context.Background()
	p, err := c.PlanUpdate(ctx, FindResource("hosts"), []string{"id:3"}, nil, map[string]any{"description": int64(42)})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Apply(ctx, c, nil); err != nil || p.Steps[0].Status != "applied" {
		t.Errorf("42 stored as \"42\" is applied, not unknown: %v %s", err, p.Steps[0].Status)
	}
	if !same("42", json.Number("42")) || !same(true, true) || same("42", "43") || same(map[string]any{"a": 1}, "x") {
		t.Error("same: text and number that read alike are equal; different values are not")
	}
}

func TestDeleteIsVerified(t *testing.T) {
	c, srv := liveClient(t, "awx")
	srv.Add("/hosts/", map[string]any{"id": 3, "name": "web1"})
	srv.Handle("/hosts/3/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(204) // says done, but the object stays
			return
		}
		fakeawx.JSON(w, 200, map[string]any{"id": 3, "name": "web1"})
	})
	ctx := context.Background()
	p, err := c.PlanDelete(ctx, FindResource("hosts"), []string{"id:3"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var uo *OutcomeUnknownError
	if err := p.Apply(ctx, c, nil); !errors.As(err, &uo) || p.Steps[0].Status != "unknown" {
		t.Errorf("a delete after which the object is still there must not end as applied: %v %s", err, p.Steps[0].Status)
	}
}

func TestStepErrorsKeepTheirKind(t *testing.T) {
	c, srv := liveClient(t, "awx")
	srv.Add("/hosts/", map[string]any{"id": 1, "name": "a", "enabled": true})
	srv.Add("/hosts/", map[string]any{"id": 2, "name": "b", "enabled": true})
	ctx := context.Background()
	p, _ := c.PlanSetEnabled(ctx, FindResource("hosts"), []string{"a"}, nil, false)
	srv.FailNext("/api/v2/hosts/1/", 403)
	err := p.Apply(ctx, c, nil)
	if !errors.Is(err, ErrDenied) || !errors.Is(err, ErrSteps) || !strings.Contains(err.Error(), "HTTP 403") {
		t.Errorf("a denied change must be recognisable as denied and say so: %v", err)
	}
	p, _ = c.PlanSetEnabled(ctx, FindResource("hosts"), []string{"a", "b"}, nil, false)
	srv.FailNext("/api/v2/hosts/1/", 404)
	err = p.Apply(ctx, c, nil)
	if !errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "1 of 2 changes failed; first:") {
		t.Errorf("several steps: the count and the first failure: %v", err)
	}
}

func TestInterruptedBeforeAStepSendsNothing(t *testing.T) {
	c, srv := liveClient(t, "awx")
	srv.Add("/hosts/", map[string]any{"id": 1, "name": "a", "enabled": true})
	p, _ := c.PlanSetEnabled(context.Background(), FindResource("hosts"), []string{"a"}, nil, false)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	dir := t.TempDir()
	err := p.Apply(ctx, c, &FileRecorder{Dir: dir})
	var uo *OutcomeUnknownError
	if err == nil || errors.As(err, &uo) || !errors.Is(err, context.Canceled) {
		t.Errorf("interrupted before sending is not an unknown outcome: %v", err)
	}
	if p.Steps[0].Status != "skipped" || len(writes(srv)) != 0 {
		t.Errorf("status %s, %d writes", p.Steps[0].Status, len(writes(srv)))
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	if len(files) != 1 {
		t.Fatalf("the record must exist: %v", files)
	}
	if data, _ := os.ReadFile(files[0]); !strings.Contains(string(data), `"status": "skipped"`) {
		t.Errorf("the record must say skipped:\n%s", data)
	}
}

func TestPruneKeepsEvidenceAndOnlyTouchesRecords(t *testing.T) {
	dir := t.TempDir()
	write := func(name, status string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(`{"steps": [{"status": "`+status+`"}]}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("20300101T000001Z-enable-host-aaaaaaaa.json", "unknown") // oldest: a change nobody has confirmed
	write("20300101T000002Z-enable-host-bbbbbbbb.json", "planned") // an interrupted run
	write("20300101T000003Z-enable-host-cccccccc.json", "applied")
	write("20300101T000004Z-enable-host-dddddddd.json", "applied")
	write("20300101T000005Z-enable-host-eeeeeeee.json", "applied")
	write("notes.json", "applied")           // not a record: someone else's file
	write("20300101-report.json", "applied") // not a record either
	(&FileRecorder{Dir: dir, Keep: 1}).Prune()
	left := map[string]bool{}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		left[e.Name()] = true
	}
	for _, keep := range []string{"20300101T000001Z-enable-host-aaaaaaaa.json", "20300101T000002Z-enable-host-bbbbbbbb.json", "20300101T000005Z-enable-host-eeeeeeee.json", "notes.json", "20300101-report.json"} {
		if !left[keep] {
			t.Errorf("%s must be kept", keep)
		}
	}
	for _, gone := range []string{"20300101T000003Z-enable-host-cccccccc.json", "20300101T000004Z-enable-host-dddddddd.json"} {
		if left[gone] {
			t.Errorf("%s is an old applied record beyond the limit and must be pruned", gone)
		}
	}
	if names, _ := RecordNames(dir); len(names) != 3 {
		t.Errorf("only awxctl's own records are listed: %v", names)
	}
}

func TestStalledCountsEveryKindOfReturn(t *testing.T) {
	c, srv := liveClient(t, "awx")
	srv.Add("/unified_jobs/", map[string]any{"id": 50, "type": "job"})
	srv.Add("/jobs/", map[string]any{"id": 50, "status": "canceled"})
	ev := func(n int, event, host string) map[string]any {
		return map[string]any{"counter": n, "event": event, "host_name": host, "task": "t", "parent_uuid": "task-1", "created": "2030-01-01T00:00:00Z"}
	}
	srv.Handle("/jobs/50/job_events/", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		switch {
		case q.Get("event__in") != "":
			fakeawx.JSON(w, 200, map[string]any{"count": 0, "results": []any{}})
		case q.Get("order_by") == "-counter":
			fakeawx.JSON(w, 200, map[string]any{"count": 1, "results": []map[string]any{ev(9, "runner_on_skipped", "d")}})
		default:
			fakeawx.JSON(w, 200, map[string]any{"count": 9, "results": []map[string]any{
				ev(1, "runner_on_start", "a"), ev(2, "runner_on_start", "b"), ev(3, "runner_on_start", "c"), ev(4, "runner_on_start", "d"), ev(5, "runner_on_start", "e"),
				ev(6, "runner_on_ok", "a"), ev(7, "runner_on_failed", "b"), ev(8, "runner_on_unreachable", "c"), ev(9, "runner_on_skipped", "d")}})
		}
	})
	srv.Handle("/jobs/50/job_host_summaries/", func(w http.ResponseWriter, _ *http.Request) {
		fakeawx.JSON(w, 200, map[string]any{"count": 0, "results": []any{}})
	})
	srv.Handle("/jobs/50/stdout/", func(w http.ResponseWriter, _ *http.Request) {
		fakeawx.JSON(w, 200, map[string]any{"range": map[string]any{"start": 0, "end": 0, "absolute_end": 0}, "content": ""})
	})
	d, err := c.Diagnose(context.Background(), 50)
	if err != nil || d.Stalled == nil {
		t.Fatalf("%+v %v", d, err)
	}
	missing := d.Stalled["hosts_not_returned"].([]string)
	if len(missing) != 1 || missing[0] != "e" || d.Stalled["hosts_returned"] != 4 {
		t.Errorf("ok, failed, unreachable and skipped all count as returned; only e did not: %v (returned %v)", missing, d.Stalled["hosts_returned"])
	}
}

func TestLaunchInfoHidesSurveyPasswordDefaults(t *testing.T) {
	c, srv := liveClient(t, "awx")
	tmpl := srv.Add("/job_templates/", map[string]any{"id": 7, "name": "Patch"})
	srv.Handle("/job_templates/7/launch/", func(w http.ResponseWriter, _ *http.Request) {
		fakeawx.JSON(w, 200, map[string]any{"survey_enabled": true, "ask_limit_on_launch": true, "defaults": map[string]any{"extra_vars": "db_password: hunter2\n"}})
	})
	srv.Handle("/job_templates/7/survey_spec/", func(w http.ResponseWriter, _ *http.Request) {
		fakeawx.JSON(w, 200, map[string]any{"spec": []map[string]any{{"variable": "pw", "question_name": "Password", "type": "password", "default": "hunter2"}, {"variable": "env", "type": "text", "default": "dev"}}})
	})
	info, err := c.LaunchInfo(context.Background(), FindResource("job-templates"), tmpl)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := json.Marshal(info)
	if strings.Contains(string(out), "hunter2") || !strings.Contains(string(out), `"default":"dev"`) || !strings.Contains(string(out), `"prompts":["limit"]`) {
		t.Errorf("a password default must be hidden, other defaults shown: %s", out)
	}
}

func TestListCutsAtALimitAboveOnePage(t *testing.T) {
	c, srv := liveClient(t, "awx")
	for i := 0; i < 450; i++ {
		srv.Add("/hosts/", map[string]any{"name": "h"})
	}
	objs, total, err := c.List(context.Background(), "/hosts/", nil, 250)
	if err != nil || len(objs) != 250 || total != 450 {
		t.Errorf("limit 250 of 450: got %d (total %d) %v", len(objs), total, err)
	}
}

func TestAnAnswerThatIsTooLargeIsRefusedWhole(t *testing.T) {
	old := maxAnswer
	maxAnswer = 1000
	t.Cleanup(func() { maxAnswer = old })
	c, srv := liveClient(t, "awx")
	srv.Add("/unified_jobs/", map[string]any{"id": 12, "type": "job"})
	srv.Add("/jobs/", map[string]any{"id": 12, "status": "failed"})
	srv.Handle("/jobs/12/stdout/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("format") == "txt_download" {
			_, _ = w.Write([]byte(strings.Repeat("x\n", 2000) + "fatal: THE REAL LAST LINE\n"))
			return
		}
		fakeawx.JSON(w, 200, map[string]any{"range": map[string]any{"start": 0, "end": 1, "absolute_end": 1}, "content": "Standard Output too large to display (1 bytes)"})
	})
	j, _ := c.FindJob(context.Background(), 12)
	text, _, err := c.Output(context.Background(), j, 0)
	if !errors.Is(err, ErrTooLarge) || text != "" {
		t.Errorf("output beyond the limit must be an error, never a cut text: %d bytes, %v", len(text), err)
	}
}

func TestDiagnoseCountsOnlyRealFailures(t *testing.T) {
	c, srv := liveClient(t, "awx")
	srv.Add("/unified_jobs/", map[string]any{"id": 60, "type": "ad_hoc_command"})
	srv.Add("/ad_hoc_commands/", map[string]any{"id": 60, "name": "x", "status": "failed", "event_processing_finished": false})
	ev := func(n int, event, host, task, uuid, msg string, data map[string]any) map[string]any {
		if data == nil {
			data = map[string]any{}
		}
		data["res"] = map[string]any{"msg": msg}
		return map[string]any{"counter": n, "event": event, "host_name": host, "task": task, "parent_uuid": uuid, "play": "p", "event_data": data}
	}
	srv.Handle("/ad_hoc_commands/60/events/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("event__in") == "" {
			fakeawx.JSON(w, 200, map[string]any{"count": 0, "results": []any{}})
			return
		}
		fakeawx.JSON(w, 200, map[string]any{"count": 8, "results": []map[string]any{
			ev(1, "runner_on_failed", "a", "probe", "t1", "not running", map[string]any{"ignore_errors": true}),
			ev(2, "runner_item_on_failed", "a", "install", "t2", "no package x", nil),
			ev(3, "runner_item_on_failed", "a", "install", "t2", "no package y", nil),
			ev(4, "runner_on_failed", "a", "install", "t2", "One or more items failed", nil),
			ev(5, "runner_on_failed", "a", "send", "t3", "Data could not be sent to a", nil),
			ev(6, "runner_on_failed", "b", "send", "t3", "Data could not be sent to b", nil),
			ev(7, "runner_on_unreachable", "web1", "gather", "t4", "Failed to connect to the host via ssh: connect to host 192.0.2.10 port 22", map[string]any{"remote_addr": "192.0.2.10"}),
			ev(8, "runner_on_unreachable", "web2", "gather", "t4", "Failed to connect to the host via ssh: connect to host 192.0.2.11 port 22", map[string]any{"remote_addr": "192.0.2.11"}),
		}})
	})
	d, err := c.Diagnose(context.Background(), 60)
	if err != nil {
		t.Fatal(err)
	}
	byTask := map[string][]map[string]any{}
	for _, f := range d.Failures {
		byTask[f["task"].(string)] = append(byTask[f["task"].(string)], f)
	}
	if byTask["probe"] != nil || d.Job["ignored_errors"] != 1 {
		t.Errorf("an ignored error is not a failure: %v, ignored=%v", byTask["probe"], d.Job["ignored_errors"])
	}
	if len(byTask["install"]) != 2 {
		t.Errorf("a failed loop is its failed items, not also the summary: %+v", byTask["install"])
	}
	if len(byTask["send"]) != 1 || byTask["send"][0]["message"] != "Data could not be sent to <host>" || byTask["send"][0]["host_count"] != 2 {
		t.Errorf("host a must not rewrite the letter a inside words: %+v", byTask["send"])
	}
	if len(byTask["gather"]) != 1 || byTask["gather"][0]["host_count"] != 2 {
		t.Errorf("unreachable hosts differ only in their address and are one failure: %+v", byTask["gather"])
	}
	if !strings.Contains(d.Verdict, "on 4 host(s)") || !strings.Contains(d.Verdict, "still storing") || d.Job["events_complete"] != false {
		t.Errorf("hosts are counted from the failures (a, b, web1, web2), and unfinished event storage is said: %s", d.Verdict)
	}
	if got := replaceWord("a: ran on a.example.com and a. Also a", "a", "<host>"); got != "<host>: ran on a.example.com and <host>. Also <host>" {
		t.Errorf("replaceWord: %q", got)
	}
}

func TestSecretsInYAMLFlowTextAreMasked(t *testing.T) {
	jt := FindResource("job-templates")
	row := jt.Row(Object{"id": 1, "name": "t", "extra_vars": "{db_password: hunter2, n: 1}"})
	if out, _ := json.Marshal(row["extra_vars"]); strings.Contains(string(out), "hunter2") {
		t.Errorf("YAML written with braces must be masked like JSON: %s", out)
	}
	// Text no parser here accepts, but that names a secret: hidden whole.
	if got := maskText("db_password: hunter2\ndb_password: again\n\t- broken"); got != "******" {
		t.Errorf("unparseable text with a secret-looking name must be hidden: %q", got)
	}
	if got := maskText("just a note: [unclosed"); got != "just a note: [unclosed" {
		t.Errorf("unparseable text without one stays: %q", got)
	}
	masked := Mask(map[string]any{"pwd": "a", "db_pw": "b", "ssl_key": "c", "client_key": "d", "access_key": "e", "host_config_key": "f", "keyboard": "g", "monkey": "h", "key_name": "i"}).(map[string]any)
	for _, k := range []string{"pwd", "db_pw", "ssl_key", "client_key", "access_key", "host_config_key", "key_name"} {
		if masked[k] != "******" {
			t.Errorf("%s must be masked", k)
		}
	}
	if masked["keyboard"] != "g" || masked["monkey"] != "h" {
		t.Errorf("words that merely contain key are not secrets: %v", masked)
	}
}

func TestWorkflowNodeSecretIsAlwaysSent(t *testing.T) {
	c, srv := liveClient(t, "awx")
	srv.Add("/workflow_job_templates/", map[string]any{"id": 11, "name": "Release"})
	srv.Add("/unified_job_templates/", map[string]any{"id": 7, "name": "Build", "type": "job_template"})
	srv.Add("/workflow_job_templates/11/workflow_nodes/", map[string]any{"id": 100, "identifier": "build", "unified_job_template": 7,
		"summary_fields": map[string]any{"unified_job_template": map[string]any{"name": "Build", "unified_job_type": "job"}},
		"extra_data":     map[string]any{"db_password": "old", "n": 1}, "success_nodes": []any{}, "failure_nodes": []any{}, "always_nodes": []any{}})
	p, err := c.PlanGraph(context.Background(), "Release", []GraphNode{{ID: "build", Template: "Build", ExtraData: map[string]any{"db_password": "new", "n": 1}}})
	if err != nil || len(p.Steps) != 1 || p.Steps[0].Action != "update node" {
		t.Fatalf("a changed secret must produce an update: %+v %v", p, err)
	}
	if shown, _ := json.Marshal(p); strings.Contains(string(shown), `"new"`) {
		t.Errorf("the plan must not show the secret: %s", shown)
	}
}

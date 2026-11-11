package core

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/mwtrigg/awxctl/internal/fakeawx"
)

func TestTokenValueStaysOutOfPlanAndRecord(t *testing.T) {
	for _, flavor := range []string{"awx", "aap"} {
		c, srv := liveClient(t, flavor)
		coll := "/tokens/"
		if flavor == "aap" {
			coll = "gw:/tokens/" // tokens are the gateway's on AAP
		}
		srv.Add(coll, map[string]any{"id": 1, "description": "old"})
		ctx := context.Background()
		if _, err := c.PlanTokenCreate(ctx, "", "", "read"); !errors.Is(err, ErrUsage) {
			t.Errorf("a token needs a description: %v", err)
		}
		if _, err := c.PlanTokenCreate(ctx, "", "x", "admin"); !errors.Is(err, ErrUsage) {
			t.Errorf("scope must be read or write: %v", err)
		}
		p, err := c.PlanTokenCreate(ctx, "", "agent", "read")
		if err != nil {
			t.Fatal(err)
		}
		// the fake store echoes the body back with an id: give it a token value
		srv.FailNext("unused")
		rec := &FileRecorder{Dir: t.TempDir()}
		// the generic store answers without a token, which must be an unknown outcome
		err = p.Apply(ctx, c, rec)
		var uo *OutcomeUnknownError
		if !errors.As(err, &uo) {
			t.Errorf("%s: an answer without a token cannot be confirmed: %v", flavor, err)
		}
		last := srv.Requests()[len(srv.Requests())-1]
		wantPath := map[string]string{"awx": "/api/v2/tokens/", "aap": "/api/gateway/v1/tokens/"}[flavor]
		if last.Method != http.MethodPost || last.Path != wantPath {
			t.Errorf("%s: token created at %s %s, want %s", flavor, last.Method, last.Path, wantPath)
		}
	}
	// With a real answer the value is in Raw only.
	c, srv := liveClient(t, "awx")
	srv.Handle("/tokens/", func(w http.ResponseWriter, _ *http.Request) {
		fakeawx.JSON(w, 201, map[string]any{"id": 9, "token": "s3cr3t-value", "scope": "read", "expires": "2030-01-01T00:00:00Z"})
	})
	p, _ := c.PlanTokenCreate(context.Background(), "", "agent", "read")
	rec := &FileRecorder{Dir: t.TempDir()}
	if err := p.Apply(context.Background(), c, rec); err != nil {
		t.Fatal(err)
	}
	shown, _ := json.Marshal(p)
	data, _ := os.ReadFile(rec.Path())
	if strings.Contains(string(shown)+string(data), "s3cr3t-value") || p.Steps[0].Raw()["token"] != "s3cr3t-value" {
		t.Errorf("the token value belongs in Raw only:\n%s\n%s", shown, data)
	}
}

func TestGrantAndRevoke(t *testing.T) {
	// On AAP the role, the user and the assignment are the gateway's; the object
	// is the controller's. Their IDs differ between the two.
	c, srv := liveClient(t, "aap")
	srv.Add("gw:/role_definitions/", map[string]any{"id": 23, "name": "Inventory Use", "content_type": "awx.inventory"})
	srv.Add("/role_definitions/", map[string]any{"id": 19, "name": "Inventory Use", "content_type": "awx.inventory"})
	srv.Add("gw:/role_definitions/", map[string]any{"id": 30, "name": "Platform Auditor", "content_type": nil})
	srv.Add("gw:/users/", map[string]any{"id": 2, "username": "ann"})
	srv.Add("/users/", map[string]any{"id": 1, "username": "ann"})
	srv.Add("gw:/teams/", map[string]any{"id": 6, "name": "ops"})
	srv.Add("/inventories/", map[string]any{"id": 61, "name": "lab"})
	srv.Add("gw:/role_user_assignments/", map[string]any{"id": 500, "role_definition": 30, "user": 2, "object_id": nil, "content_type": nil})
	srv.Add("gw:/role_team_assignments/", map[string]any{"id": 600, "role_definition": 99, "team": 6, "object_id": 1})
	ctx := context.Background()
	inv := FindResource("inventories")

	p, err := c.PlanGrant(ctx, "Inventory Use", "ann", "", inv, "lab", nil, false)
	if err != nil || len(p.Steps) != 1 {
		t.Fatalf("%+v %v", p, err)
	}
	if s := p.Steps[0].Send; s["role_definition"] != 23 || s["user"] != 2 || s["object_id"] != 61 {
		t.Errorf("gateway IDs for role and user, controller ID for the object: %v", s)
	}
	if err := p.Apply(ctx, c, nil); err != nil {
		t.Fatal(err)
	}
	if last := srv.Requests()[len(srv.Requests())-1]; last.Path != "/api/gateway/v1/role_user_assignments/" {
		t.Errorf("granted at %s", last.Path)
	}
	if again, _ := c.PlanGrant(ctx, "Inventory Use", "ann", "", inv, "lab", nil, false); len(again.Steps) != 0 {
		t.Error("a role that is held is not granted again")
	}
	rows, err := c.Access(ctx, inv, "lab", nil)
	if err == nil && len(rows) != 0 {
		// the fake stores no content_type on a created assignment: nothing listed
		t.Logf("access rows: %v", rows)
	}
	rev, err := c.PlanGrant(ctx, "Inventory Use", "ann", "", inv, "lab", nil, true)
	if err != nil || len(rev.Steps) != 1 || rev.Steps[0].Method != http.MethodDelete {
		t.Fatalf("revoke: %+v %v", rev, err)
	}
	// wrong uses
	for name, call := range map[string]func() error{
		"a role of another kind": func() error {
			_, e := c.PlanGrant(ctx, "Inventory Use", "ann", "", FindResource("projects"), "x", nil, false)
			return e
		},
		"an object role without --on": func() error { _, e := c.PlanGrant(ctx, "Inventory Use", "ann", "", nil, "", nil, false); return e },
		"user and team together": func() error {
			_, e := c.PlanGrant(ctx, "Inventory Use", "ann", "ops", inv, "lab", nil, false)
			return e
		},
		"neither user nor team": func() error { _, e := c.PlanGrant(ctx, "Inventory Use", "", "", inv, "lab", nil, false); return e },
		"a kind without roles": func() error {
			_, e := c.PlanGrant(ctx, "Inventory Use", "ann", "", FindResource("hosts"), "x", nil, false)
			return e
		},
	} {
		if err := call(); !errors.Is(err, ErrUsage) {
			t.Errorf("%s must be refused: %v", name, err)
		}
	}
	// a system-wide role that is already held
	if p, err := c.PlanGrant(ctx, "Platform Auditor", "ann", "", nil, "", nil, false); err != nil || len(p.Steps) != 0 {
		t.Errorf("system-wide role already held: %+v %v", p, err)
	}
}

func TestMergedVars(t *testing.T) {
	c, srv := liveClient(t, "awx")
	srv.Handle("/inventories/3/script/", func(w http.ResponseWriter, _ *http.Request) {
		fakeawx.JSON(w, 200, map[string]any{
			"all":   map[string]any{"children": []string{"prod", "ungrouped"}, "vars": map[string]any{"site": "lab", "ntp": "inv"}},
			"prod":  map[string]any{"children": []string{"web"}, "vars": map[string]any{"tier": "prod", "role": "prod"}},
			"web":   map[string]any{"hosts": []string{"web1"}, "vars": map[string]any{"ntp": "web", "role": "web"}},
			"other": map[string]any{"hosts": []string{"db1"}, "vars": map[string]any{"role": "db"}},
			"_meta": map[string]any{"hostvars": map[string]any{"web1": map[string]any{"ntp": "host", "ansible_password": "hunter2", "remote_tower_id": 5}}},
		})
	})
	host := Object{"id": 5, "name": "web1", "inventory": 3}
	vars, src, err := c.MergedVars(context.Background(), host, false)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"site": "inventory", "tier": "group prod", "role": "group web", "ntp": "host", "ansible_password": "host"}
	for k, from := range want {
		if src[k] != from {
			t.Errorf("%s comes from %q, want %q", k, src[k], from)
		}
	}
	if vars["role"] != "web" || vars["ntp"] != "host" || vars["ansible_password"] != "******" || vars["remote_tower_id"] != nil || len(vars) != len(want) {
		t.Errorf("merged: %v", vars)
	}
	if shown, _, _ := c.MergedVars(context.Background(), host, true); shown["ansible_password"] != "hunter2" {
		t.Error("--show-secrets must show the value")
	}
}

func TestSurveySet(t *testing.T) {
	c, srv := liveClient(t, "awx")
	srv.Add("/job_templates/", map[string]any{"id": 7, "name": "Patch", "survey_enabled": false})
	srv.Handle("/job_templates/7/survey_spec/", func(w http.ResponseWriter, r *http.Request) {
		fakeawx.JSON(w, 200, map[string]any{})
	})
	jt := FindResource("job-templates")
	ctx := context.Background()
	q := []map[string]any{{"variable": "release", "default": "1.0"}, {"variable": "env", "type": "multiplechoice", "choices": []any{"dev", "prod"}},
		{"variable": "pw", "type": "password", "default": "hunter2", "required": false}}
	p, err := c.PlanSurveySet(ctx, jt, "Patch", q, false)
	if err != nil || len(p.Steps) != 2 || p.Steps[1].Action != "switch survey on" {
		t.Fatalf("%+v %v", p, err)
	}
	shown, _ := json.Marshal(p)
	if strings.Contains(string(shown), "hunter2") || !strings.Contains(string(shown), `"question_name":"release"`) {
		t.Errorf("the plan: %s", shown)
	}
	if err := p.Apply(ctx, c, nil); err != nil {
		t.Fatal(err)
	}
	var sent string
	for _, r := range srv.Requests() {
		if r.Method == http.MethodPost {
			sent = r.Body
		}
	}
	if !strings.Contains(sent, `"default":"hunter2"`) || !strings.Contains(sent, `"required":false`) {
		t.Errorf("the real default must be sent: %s", sent)
	}
	for name, bad := range map[string][]map[string]any{
		"no variable":       {{"question": "x"}},
		"twice":             {{"variable": "a"}, {"variable": "a"}},
		"unknown type":      {{"variable": "a", "type": "date"}},
		"choices missing":   {{"variable": "a", "type": "multiselect"}},
		"unknown key":       {{"variable": "a", "requird": true}},
		"the mask as value": {{"variable": "a", "type": "password", "default": "******"}},
		"no questions":      {},
	} {
		if _, err := c.PlanSurveySet(ctx, jt, "Patch", bad, false); !errors.Is(err, ErrUsage) {
			t.Errorf("%s must be refused: %v", name, err)
		}
	}
}

func TestWorkflowGraph(t *testing.T) {
	c, srv := liveClient(t, "awx")
	srv.Add("/workflow_job_templates/", map[string]any{"id": 11, "name": "Release"})
	srv.Add("/unified_job_templates/", map[string]any{"id": 7, "name": "Build", "type": "job_template"})
	srv.Add("/unified_job_templates/", map[string]any{"id": 8, "name": "Deploy", "type": "job_template"})
	srv.Add("/job_templates/", map[string]any{"id": 7, "name": "Build"})
	jt := func(id int, name string) map[string]any {
		return map[string]any{"unified_job_template": map[string]any{"name": name, "unified_job_type": "job"}}
	}
	// current graph: old -> build (success); gone has lost its template
	nodes := "/workflow_job_templates/11/workflow_nodes/"
	srv.Add(nodes, map[string]any{"id": 100, "identifier": "build", "unified_job_template": 7, "summary_fields": jt(7, "Build"), "success_nodes": []any{}, "failure_nodes": []any{}, "always_nodes": []any{}})
	srv.Add(nodes, map[string]any{"id": 101, "identifier": "old", "unified_job_template": 8, "summary_fields": jt(8, "Deploy"), "success_nodes": []any{100}, "failure_nodes": []any{}, "always_nodes": []any{}})
	srv.Add(nodes, map[string]any{"id": 102, "identifier": "gone", "unified_job_template": nil, "summary_fields": map[string]any{}, "success_nodes": []any{}, "failure_nodes": []any{}, "always_nodes": []any{}})
	ctx := context.Background()
	wf := srv.Collection("/workflow_job_templates/")[0]
	have, err := c.ReadGraph(ctx, wf)
	if err != nil || len(have) != 3 || have[1].OnSuccess[0] != "build" || !have[2].Missing {
		t.Fatalf("read: %+v %v", have, err)
	}
	// the same graph again: nothing to do
	same := []GraphNode{{ID: "build", Template: "Build"}, {ID: "old", Template: "Deploy", OnSuccess: []string{"build"}}, {ID: "gone", Missing: true}}
	if p, err := c.PlanGraph(ctx, "Release", same); err != nil || len(p.Steps) != 0 {
		t.Fatalf("an unchanged graph must need no step: %+v %v", p, err)
	}
	// new graph: build -> deploy (new), old and gone removed
	want := []GraphNode{{ID: "build", Template: "Build", OnSuccess: []string{"deploy"}, AllParents: true}, {ID: "deploy", Template: "Deploy", Type: "job", Limit: "web"}}
	srv.Add("/job_templates/", map[string]any{"id": 8, "name": "Deploy"})
	p, err := c.PlanGraph(ctx, "Release", want)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, s := range p.Steps {
		got = append(got, s.Action+" "+s.Target[strings.LastIndex(s.Target, ": ")+2:])
	}
	wantSteps := "delete node node gone|delete node node old|update node node build|create node node deploy|link build -[on_success]-> deploy"
	if strings.Join(got, "|") != wantSteps {
		t.Errorf("steps:\n got %s\nwant %s", strings.Join(got, "|"), wantSteps)
	}
	if len(writes(srv)) != 0 {
		t.Fatal("planning a graph must send nothing")
	}
	var linked string
	srv.Handle("/workflow_job_template_nodes/100/success_nodes/", func(w http.ResponseWriter, r *http.Request) {
		linked = "sent"
		w.WriteHeader(204)
	})
	srv.Handle("/workflow_job_template_nodes/100/", func(w http.ResponseWriter, r *http.Request) { fakeawx.JSON(w, 200, map[string]any{"id": 100}) })
	srv.Handle("/workflow_job_template_nodes/101/", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	srv.Handle("/workflow_job_template_nodes/102/", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	if err := p.Apply(ctx, c, nil); err != nil {
		t.Fatalf("apply: %v (%+v)", err, p.Steps)
	}
	var linkBody string
	for _, r := range writes(srv) {
		if strings.HasSuffix(r.Path, "/100/success_nodes/") {
			linkBody = r.Body
		}
	}
	created := srv.Collection(nodes)
	newID := created[len(created)-1]["id"]
	if linked == "" || !strings.Contains(linkBody, `"id":`+strings.TrimSpace(strings.Trim(mustJSON(newID), `"`))) {
		t.Errorf("the link must use the ID the new node got (%v): %s", newID, linkBody)
	}
	// wrong graphs are refused before anything is read or sent
	for name, bad := range map[string][]GraphNode{
		"a cycle":           {{ID: "a", Template: "Build", OnSuccess: []string{"b"}}, {ID: "b", Template: "Build", OnFailure: []string{"a"}}},
		"an unknown target": {{ID: "a", Template: "Build", Always: []string{"zzz"}}},
		"a duplicate id":    {{ID: "a", Template: "Build"}, {ID: "a", Template: "Build"}},
		"no template":       {{ID: "a"}},
		"a new missing":     {{ID: "brand-new", Missing: true}},
		"approval+template": {{ID: "a", Template: "Build", Approval: &GraphApproval{Name: "ok?"}}},
	} {
		if _, err := c.PlanGraph(ctx, "Release", bad); !errors.Is(err, ErrUsage) {
			t.Errorf("%s must be refused: %v", name, err)
		}
	}
}

func mustJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

func TestCopyAndSchedulePreview(t *testing.T) {
	c, srv := liveClient(t, "awx")
	srv.Add("/inventories/", map[string]any{"id": 2, "name": "lab"})
	srv.Add("/inventories/", map[string]any{"id": 3, "name": "taken"})
	srv.Handle("/inventories/2/copy/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			fakeawx.JSON(w, 200, map[string]any{"can_copy": true})
			return
		}
		fakeawx.JSON(w, 201, map[string]any{"id": 4, "name": "lab2"})
	})
	ctx := context.Background()
	inv := FindResource("inventories")
	if _, err := c.PlanCopy(ctx, inv, "lab", "taken", nil); !errors.Is(err, ErrUsage) {
		t.Errorf("a name that exists must be refused: %v", err)
	}
	if _, err := c.PlanCopy(ctx, FindResource("hosts"), "x", "y", nil); !errors.Is(err, ErrUsage) {
		t.Errorf("hosts cannot be copied: %v", err)
	}
	p, err := c.PlanCopy(ctx, inv, "lab", "lab2", nil)
	if err != nil || len(writes(srv)) != 0 {
		t.Fatalf("%v", err)
	}
	if err := p.Apply(ctx, c, nil); err != nil || p.Steps[0].Result["id"] != 4 {
		t.Errorf("copy: %v %+v", err, p.Steps[0])
	}
	srv.Handle("/schedules/preview/", func(w http.ResponseWriter, r *http.Request) {
		fakeawx.JSON(w, 200, map[string]any{"utc": []string{"2030-01-01T00:00:00Z"}, "local": []string{"2029-12-31T19:00:00-05:00"}})
	})
	utc, local, err := c.SchedulePreview(ctx, "DTSTART:20300101T000000Z RRULE:FREQ=DAILY;INTERVAL=1")
	if err != nil || len(utc) != 1 || len(local) != 1 {
		t.Errorf("preview: %v %v %v", utc, local, err)
	}
}

func TestGroupRuns(t *testing.T) {
	rows := []map[string]any{ // newest first
		{"id": 9, "template": "Patch", "status": "failed", "elapsed": 30.0, "started": "2030-01-03T10:00:00Z"},
		{"id": 8, "template": "Patch", "status": "successful", "elapsed": 10.0, "started": "2030-01-02T10:00:00Z"},
		{"id": 7, "template": "Patch", "status": "successful", "elapsed": 20.0, "started": "2030-01-02T09:00:00Z"},
		{"id": 6, "template": "Patch", "status": "running", "elapsed": 99.0, "started": "2030-01-02T08:00:00Z"},
		{"id": 5, "template": "Patch", "status": "canceled", "elapsed": 500.0, "started": "2030-01-01T08:00:00Z"},
		{"id": 4, "template": nil, "status": "successful", "elapsed": 1.0, "started": "2030-01-01T07:00:00Z"},
	}
	g := GroupRuns(rows, "template")
	if len(g) != 2 || g[0].Key != "Patch" || g[1].Key != "(none)" {
		t.Fatalf("groups: %+v", g)
	}
	row := g[0].Row("template")
	want := map[string]any{"runs": 5, "successful": 2, "failed": 1, "canceled": 1, "running": 1, "failure_rate": 0.333, "last_id": 9, "last_status": "failed",
		"seconds_min": 10.0, "seconds_max": 30.0, "seconds_avg": 20.0, "seconds_median": 20.0, "seconds_p95": 30.0, "seconds_total": 60.0}
	for k, v := range want {
		if row[k] != v {
			t.Errorf("%s = %v, want %v (running and canceled runs do not count as runtimes)", k, row[k], v)
		}
	}
	days := GroupRuns(rows, "day")
	if len(days) != 3 || days[0].Key != "2030-01-03" || days[1].Runs != 3 {
		t.Errorf("by day, newest first: %+v %+v", days[0], days[1])
	}
}

func TestHostOutputAndLatestJob(t *testing.T) {
	c, srv := liveClient(t, "awx")
	srv.Handle("/jobs/30/job_events/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("host_name") != "web1" {
			t.Errorf("only this host's events must be asked for: %s", r.URL.RawQuery)
		}
		fakeawx.JSON(w, 200, map[string]any{"count": 4, "results": []map[string]any{
			{"counter": 4, "event": "runner_on_start", "task": "Install", "play": "p", "stdout": ""},
			{"counter": 5, "event": "runner_on_ok", "task": "Install", "play": "p", "stdout": "\x1b[0;32mok: [web1]\x1b[0m"},
			{"counter": 8, "event": "runner_on_failed", "task": "Restart", "play": "p", "stdout": "fatal: [web1]: FAILED! => {\r\n    \"msg\": \"boom\"\r\n}"},
		}})
	})
	text, tasks, err := c.HostOutput(context.Background(), 30, "web1", false)
	want := "TASK [Install]\nok: [web1]\nTASK [Restart]\nfatal: [web1]: FAILED! => {\n    \"msg\": \"boom\"\n}\n"
	if err != nil || tasks != 2 || text != want {
		t.Errorf("host output (%d tasks, %v):\n%q\nwant\n%q", tasks, err, text, want)
	}
	srv.Handle("/hosts/5/job_host_summaries/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("job__job_template__name") == "Nope" {
			fakeawx.JSON(w, 200, map[string]any{"count": 0, "results": []any{}})
			return
		}
		fakeawx.JSON(w, 200, map[string]any{"count": 1, "results": []map[string]any{{"summary_fields": map[string]any{"job": map[string]any{"id": 30}}}}})
	})
	host := Object{"id": 5, "name": "web1"}
	if id, err := c.LatestHostJob(context.Background(), host, "Patch", false); err != nil || id != 30 {
		t.Errorf("latest job: %d %v", id, err)
	}
	if _, err := c.LatestHostJob(context.Background(), host, "Nope", false); !errors.Is(err, ErrNotFound) {
		t.Errorf("no job of that template: %v", err)
	}
}

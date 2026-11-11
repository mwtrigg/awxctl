package core

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/mwtrigg/awxctl/internal/config"
	"github.com/mwtrigg/awxctl/internal/fakeawx"
)

func liveClient(t *testing.T, flavor string) (*Client, *fakeawx.Server) {
	t.Helper()
	srv := fakeawx.New(flavor)
	t.Cleanup(srv.Close)
	return newTestClient(t, srv, func(ctx *config.Context) { ctx.Flavor = flavor }), srv
}

// writes returns the changing requests the fake received.
func writes(srv *fakeawx.Server) []fakeawx.Request {
	var out []fakeawx.Request
	for _, r := range srv.Requests() {
		if r.Method != http.MethodGet {
			out = append(out, r)
		}
	}
	return out
}

func TestListFollowsPagesAndStopsAtLimit(t *testing.T) {
	for _, flavor := range []string{"awx", "aap"} {
		c, srv := liveClient(t, flavor)
		for i := 0; i < 450; i++ {
			srv.Add("/hosts/", map[string]any{"name": "h"})
		}
		all, total, err := c.List(context.Background(), "/hosts/", nil, 0)
		if err != nil || len(all) != 450 || total != 450 {
			t.Fatalf("%s: got %d of %d, %v", flavor, len(all), total, err)
		}
		some, total, _ := c.List(context.Background(), "/hosts/", url.Values{"name": {"h"}}, 7)
		if len(some) != 7 || total != 450 {
			t.Errorf("%s: limit 7 gave %d (total %d)", flavor, len(some), total)
		}
	}
}

func TestListNeverFollowsAnotherHost(t *testing.T) {
	c, srv := liveClient(t, "awx")
	calls := 0
	srv.Handle("/jobs/", func(w http.ResponseWriter, _ *http.Request) {
		calls++
		fakeawx.JSON(w, 200, map[string]any{"count": 2, "next": "https://other.example.com/api/v2/jobs/?page=2", "results": []map[string]any{{"id": 1}}})
	})
	objs, _, err := c.List(context.Background(), "/jobs/", nil, 0)
	if err != nil || len(objs) != 1 || calls != 1 {
		t.Errorf("a next link to another host must end the list: %d objects, %d calls, %v", len(objs), calls, err)
	}
}

func TestResolve(t *testing.T) {
	c, srv := liveClient(t, "awx")
	hosts := FindResource("hosts")
	srv.Add("/hosts/", map[string]any{"id": 5, "name": "web1", "inventory": 1})
	srv.Add("/hosts/", map[string]any{"id": 6, "name": "web1", "inventory": 2})
	srv.Add("/hosts/", map[string]any{"id": 7, "name": "db1", "inventory": 1})
	srv.Add("/hosts/", map[string]any{"id": 8, "name": "2024", "inventory": 1})
	ctx := context.Background()
	if o, err := c.Resolve(ctx, hosts, "db1", nil); err != nil || ID(o) != 7 {
		t.Errorf("by name: %v %v", o, err)
	}
	if o, err := c.Resolve(ctx, hosts, "id:5", nil); err != nil || ID(o) != 5 {
		t.Errorf("by id: %v %v", o, err)
	}
	if o, err := c.Resolve(ctx, hosts, "2024", nil); err != nil || ID(o) != 8 {
		t.Errorf("a number that is no ID falls back to the name: %v %v", o, err)
	}
	var amb *AmbiguousError
	if _, err := c.Resolve(ctx, hosts, "web1", nil); !errors.As(err, &amb) || len(amb.Matches) != 2 {
		t.Errorf("two hosts named web1 must be ambiguous: %v", err)
	}
	if o, err := c.Resolve(ctx, hosts, "web1", url.Values{"inventory": {"2"}}); err != nil || ID(o) != 6 {
		t.Errorf("the scope picks one: %v %v", o, err)
	}
	if _, err := c.Resolve(ctx, hosts, "nope", nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("got %v, want not found", err)
	}
	if _, err := c.Resolve(ctx, FindResource("jobs"), "nightly", nil); !errors.Is(err, ErrUsage) {
		t.Errorf("a job has no name to look up: %v", err)
	}
}

func launchFake(srv *fakeawx.Server, info map[string]any) {
	srv.Add("/job_templates/", map[string]any{"id": 7, "name": "Patch"})
	srv.Handle("/job_templates/7/launch/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			fakeawx.JSON(w, 200, info)
			return
		}
		fakeawx.JSON(w, 201, map[string]any{"id": 41, "job": 41, "status": "pending", "type": "job"})
	})
}

func TestLaunchRefusesWhatTheControllerWouldIgnore(t *testing.T) {
	c, srv := liveClient(t, "awx")
	launchFake(srv, map[string]any{"ask_limit_on_launch": false, "ask_variables_on_launch": false, "variables_needed_to_start": []string{}})
	jt := FindResource("job-templates")
	_, err := c.PlanLaunch(context.Background(), jt, "Patch", LaunchOptions{Limit: "web1", Verbosity: -1})
	if !errors.Is(err, ErrUsage) || !strings.Contains(err.Error(), "ask_limit_on_launch") {
		t.Fatalf("a limit the template does not prompt for must be refused: %v", err)
	}
	if _, err := c.PlanLaunch(context.Background(), jt, "Patch", LaunchOptions{ExtraVars: map[string]any{"a": 1}, Verbosity: -1}); !errors.Is(err, ErrUsage) {
		t.Errorf("extra vars without the prompt must be refused: %v", err)
	}
	if len(writes(srv)) != 0 {
		t.Errorf("planning must send nothing: %v", writes(srv))
	}
}

func TestLaunchPlanAndApply(t *testing.T) {
	c, srv := liveClient(t, "aap")
	launchFake(srv, map[string]any{"ask_limit_on_launch": true, "survey_enabled": true, "variables_needed_to_start": []string{"release"}})
	jt := FindResource("job-templates")
	ctx := context.Background()
	if _, err := c.PlanLaunch(ctx, jt, "Patch", LaunchOptions{Limit: "web1", Verbosity: -1}); !errors.Is(err, ErrUsage) || !strings.Contains(err.Error(), "release") {
		t.Fatalf("a required survey variable must be asked for: %v", err)
	}
	p, err := c.PlanLaunch(ctx, jt, "Patch", LaunchOptions{Limit: "web1", Check: false, ExtraVars: map[string]any{"release": "1.2", "db_password": "hunter2"}, Verbosity: -1})
	if err != nil {
		t.Fatal(err)
	}
	if len(writes(srv)) != 0 {
		t.Fatal("a plan must send nothing")
	}
	shown, _ := json.Marshal(p)
	if strings.Contains(string(shown), "hunter2") {
		t.Errorf("the plan shows a secret: %s", shown)
	}
	dir := t.TempDir()
	rec := &FileRecorder{Dir: dir}
	if err := p.Apply(ctx, c, rec); err != nil {
		t.Fatal(err)
	}
	w := writes(srv)
	if len(w) != 1 || w[0].Path != "/api/controller/v2/job_templates/7/launch/" || !strings.Contains(w[0].Body, `"db_password":"hunter2"`) || !strings.Contains(w[0].Body, `"limit":"web1"`) {
		t.Errorf("the launch must be sent once with the real values: %+v", w)
	}
	if p.Steps[0].Status != "applied" || p.Steps[0].Result["job_id"] != 41 {
		t.Errorf("step after apply: %+v", p.Steps[0])
	}
	data, _ := os.ReadFile(rec.Path())
	if strings.Contains(string(data), "hunter2") || !strings.Contains(string(data), `"status": "applied"`) {
		t.Errorf("the record must hold the outcome and no secret:\n%s", data)
	}
	if fi, _ := os.Stat(rec.Path()); fi.Mode().Perm() != 0o600 {
		t.Errorf("record mode %v, want 0600", fi.Mode().Perm())
	}
}

func TestApplyOutcomes(t *testing.T) {
	c, srv := liveClient(t, "awx")
	for i, name := range []string{"a", "b", "c", "d"} {
		srv.Add("/hosts/", map[string]any{"id": i + 1, "name": name, "enabled": true})
	}
	hosts := FindResource("hosts")
	ctx := context.Background()
	p, err := c.PlanSetEnabled(ctx, hosts, []string{"a", "b", "c", "d"}, nil, false)
	if err != nil || len(p.Steps) != 4 || p.Tier != "operator" {
		t.Fatalf("plan: %+v %v", p, err)
	}
	srv.FailNext("/api/v2/hosts/2/", 400) // a definite failure: go on
	srv.FailNext("/api/v2/hosts/3/", 502) // unknown outcome: stop
	err = p.Apply(ctx, c, &FileRecorder{Dir: t.TempDir()})
	var uo *OutcomeUnknownError
	if !errors.As(err, &uo) {
		t.Fatalf("got %v, want an unknown outcome", err)
	}
	got := []string{}
	for _, s := range p.Steps {
		got = append(got, s.Status)
	}
	if strings.Join(got, ",") != "applied,failed,unknown,skipped" {
		t.Errorf("statuses %v", got)
	}
	if n := len(writes(srv)); n != 3 {
		t.Errorf("%d changing requests, want 3 (the fourth is never sent)", n)
	}

	// A record that cannot be written means nothing is sent.
	c2, srv2 := liveClient(t, "awx")
	srv2.Add("/hosts/", map[string]any{"id": 1, "name": "a", "enabled": true})
	p2, _ := c2.PlanSetEnabled(ctx, hosts, []string{"a"}, nil, false)
	file := t.TempDir() + "/not-a-dir"
	_ = os.WriteFile(file, nil, 0o600)
	if err := p2.Apply(ctx, c2, &FileRecorder{Dir: file}); err == nil || len(writes(srv2)) != 0 {
		t.Errorf("no record, no change: err %v, writes %d", err, len(writes(srv2)))
	}
}

func TestUpdateSendsOnlyDifferences(t *testing.T) {
	c, srv := liveClient(t, "awx")
	srv.Add("/organizations/", map[string]any{"id": 3, "name": "Ops"})
	srv.Add("/inventories/", map[string]any{"id": 9, "name": "lab", "description": "old", "organization": 3})
	inv := FindResource("inventories")
	ctx := context.Background()
	p, err := c.PlanUpdate(ctx, inv, []string{"lab"}, nil, map[string]any{"description": "new", "organization": "Ops"})
	if err != nil || len(p.Steps) != 1 {
		t.Fatalf("%+v %v", p, err)
	}
	if s := p.Steps[0]; len(s.Send) != 1 || s.Send["description"] != "new" || s.Before["description"] != "old" {
		t.Errorf("only the description differs: send %v before %v", s.Send, s.Before)
	}
	if p, _ := c.PlanUpdate(ctx, inv, []string{"lab"}, nil, map[string]any{"description": "old"}); len(p.Steps) != 0 {
		t.Error("nothing differs: no step")
	}
	if _, err := c.PlanUpdate(ctx, inv, []string{"lab"}, nil, map[string]any{"total_hosts": 5}); !errors.Is(err, ErrUsage) {
		t.Errorf("an unknown field must be refused: %v", err)
	}
	if _, err := c.PlanCreate(ctx, inv, map[string]any{"name": "lab", "organization": "Ops"}); !errors.Is(err, ErrUsage) {
		t.Errorf("creating what exists must be refused: %v", err)
	}
	if _, err := c.PlanUpdate(ctx, FindResource("users"), []string{"x"}, nil, map[string]any{"email": "a"}); !errors.Is(err, ErrUsage) {
		t.Errorf("users are read-only here: %v", err)
	}
}

func TestDeleteShowsImpactAndVerifies(t *testing.T) {
	c, srv := liveClient(t, "awx")
	srv.Add("/inventories/", map[string]any{"id": 9, "name": "lab", "total_hosts": 4, "total_groups": 2, "related": map[string]any{"x": 1}})
	srv.Add("/job_templates/", map[string]any{"id": 20, "name": "t", "inventory": 9})
	ctx := context.Background()
	p, err := c.PlanDelete(ctx, FindResource("inventories"), []string{"lab", "id:9"}, nil)
	if err != nil || len(p.Steps) != 1 {
		t.Fatalf("the same object named twice is one step: %+v %v", p, err)
	}
	s := p.Steps[0]
	if !strings.Contains(s.Impact, "4 hosts deleted") || !strings.Contains(s.Impact, "1 job templates left without an inventory") {
		t.Errorf("impact: %q", s.Impact)
	}
	if s.Before["name"] != "lab" || s.Before["related"] != nil {
		t.Errorf("before-state: %v", s.Before)
	}
	if err := p.Apply(ctx, c, nil); err != nil || len(srv.Collection("/inventories/")) != 0 {
		t.Errorf("apply: %v", err)
	}
	if _, err := c.PlanDelete(ctx, FindResource("users"), []string{"x"}, nil); !errors.Is(err, ErrUsage) {
		t.Errorf("users cannot be deleted here: %v", err)
	}
}

func TestMask(t *testing.T) {
	in := map[string]any{"name": "x", "password": "p", "inputs": map[string]any{"username": "u"}, "nested": map[string]any{"api_token": "t", "ok": 1},
		"list": []any{map[string]any{"ssh_key_data": "k"}}, "passwords_needed_to_start": []any{"ssh_password"}, "become_enabled": false}
	out, _ := json.Marshal(Mask(in))
	for _, leak := range []string{`"p"`, `"u"`, `"t"`, `"k"`} {
		if strings.Contains(string(out), leak) {
			t.Errorf("leaked %s in %s", leak, out)
		}
	}
	if !strings.Contains(string(out), `"ssh_password"`) || !strings.Contains(string(out), `"ok":1`) || in["password"] != "p" {
		t.Errorf("names of needed passwords and plain values stay, and the input is not changed: %s", out)
	}
}

func TestOutputAndFilter(t *testing.T) {
	c, srv := liveClient(t, "awx")
	srv.Add("/unified_jobs/", map[string]any{"id": 12, "type": "job"})
	srv.Add("/jobs/", map[string]any{"id": 12, "status": "failed"})
	lines := []string{"PLAY [x]", "TASK [one]", "\x1b[0;32mok: [web1]\x1b[0m", "TASK [two]", "fatal: [web1]: FAILED!", "PLAY RECAP"}
	srv.Handle("/jobs/12/stdout/", func(w http.ResponseWriter, r *http.Request) {
		start := 0
		if v := r.URL.Query().Get("start_line"); v != "" {
			for _, ch := range v {
				start = start*10 + int(ch-'0')
			}
		}
		end := min(start+2, len(lines)) // two lines per answer: the client must loop
		fakeawx.JSON(w, 200, map[string]any{"range": map[string]any{"start": start, "end": end, "absolute_end": len(lines)}, "content": strings.Join(lines[start:end], "\n") + "\n"})
	})
	j, err := c.FindJob(context.Background(), 12)
	if err != nil {
		t.Fatal(err)
	}
	text, next, err := c.Output(context.Background(), j, 0)
	if err != nil || next != 6 || strings.Contains(text, "\x1b") || strings.Count(text, "\n") != 6 {
		t.Fatalf("output: next %d, %q, %v", next, text, err)
	}
	got := LineFilter{Grep: regexp.MustCompile("fatal"), Context: 1}.Apply(text)
	if len(got) != 3 || got[1].N != 5 || got[0].Text != "TASK [two]" {
		t.Errorf("grep with context: %+v", got)
	}
	if tail := (LineFilter{Tail: 2}).Apply(text); len(tail) != 2 || tail[1].Text != "PLAY RECAP" {
		t.Errorf("tail: %+v", tail)
	}
	if _, _, err := c.Output(context.Background(), &Job{ID: 1, Type: "workflow_job"}, 0); !errors.Is(err, ErrUsage) {
		t.Errorf("a workflow job has no output: %v", err)
	}
}

func TestDiagnose(t *testing.T) {
	c, srv := liveClient(t, "aap")
	srv.Add("/unified_jobs/", map[string]any{"id": 30, "type": "job"})
	srv.Add("/jobs/", map[string]any{"id": 30, "name": "Patch", "status": "failed", "failed": true})
	srv.Handle("/jobs/30/job_events/", func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Query().Get("event__in"), "runner_on_unreachable") {
			t.Errorf("diagnose must ask for failed and unreachable results: %s", r.URL.RawQuery)
		}
		fakeawx.JSON(w, 200, map[string]any{"count": 1, "results": []map[string]any{{"counter": 9, "event": "runner_on_failed", "host_name": "web2", "task": "Install", "failed": true,
			"event_data": map[string]any{"res": map[string]any{"msg": "No package matching"}}}}})
	})
	srv.Handle("/jobs/30/job_host_summaries/", func(w http.ResponseWriter, _ *http.Request) {
		fakeawx.JSON(w, 200, map[string]any{"count": 1, "results": []map[string]any{{"host_name": "web2", "failed": true, "failures": 1, "dark": 0}}})
	})
	d, err := c.Diagnose(context.Background(), 30)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Failures) != 1 || d.Failures[0]["message"] != "No package matching" || d.Hosts[0]["host"] != "web2" || !strings.Contains(d.Verdict, `task "Install" on host web2`) {
		t.Errorf("diagnosis: %+v", d)
	}
	if _, err := c.Diagnose(context.Background(), 999); !errors.Is(err, ErrNotFound) {
		t.Errorf("got %v, want not found", err)
	}
}

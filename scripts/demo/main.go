// Command demo serves a small made-up controller on localhost, so awxctl can be
// tried without an AWX or AAP at hand:
//
//	go run ./scripts/demo            # prints the two variables to export
//	export AWXCTL_URL=http://127.0.0.1:8043 AWXCTL_API_TOKEN=fake-token
//	awxctl get jobs --failed -o table
//	awxctl diagnose job 1042
//
// Everything it serves is invented (hosts under example.com). It keeps changes
// in memory, so launch, update and delete can be tried with --apply too; a
// launched job simply stays pending. It is the fake the tests use, with a
// little data in it: it answers the calls awxctl makes, not the whole API.
package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/mwtrigg/awxctl/internal/fakeawx"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8043", "address to listen on")
	flavor := flag.String("flavor", "awx", "awx or aap: which API layout to serve")
	flag.Parse()
	srv, err := fakeawx.NewLocal(*flavor, *addr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "demo:", err)
		os.Exit(1)
	}
	seed(srv)
	fmt.Printf("demo controller (%s layout) on %s; made-up data, kept in memory\n\n", *flavor, srv.URL)
	fmt.Printf("  export AWXCTL_URL=%s AWXCTL_API_TOKEN=%s AWXCTL_PERMISSIONS=operator\n\n", srv.URL, fakeawx.Token)
	fmt.Println("try:  awxctl get jobs --failed -o table")
	fmt.Println("      awxctl diagnose job 1042 --compare-last-success -o table")
	fmt.Println("      awxctl launch \"Patch servers\" --limit web2.example.com")
	fmt.Println("\nCtrl-C stops it.")
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt)
	<-stop
	srv.Close()
}

func ago(d time.Duration) string {
	return time.Now().UTC().Add(-d).Format("2006-01-02T15:04:05.000000Z")
}

func named(id int, name string) map[string]any { return map[string]any{"id": id, "name": name} }

func seed(s *fakeawx.Server) {
	s.Add("/organizations/", map[string]any{"id": 1, "name": "Default"})
	inv := named(3, "production")
	s.Add("/inventories/", map[string]any{"id": 3, "name": "production", "organization": 1, "kind": "", "total_hosts": 4, "total_groups": 2,
		"hosts_with_active_failures": 2, "has_inventory_sources": true, "summary_fields": map[string]any{"organization": named(1, "Default")}})
	s.Add("/inventories/", map[string]any{"id": 4, "name": "staging", "organization": 1, "kind": "", "total_hosts": 2, "total_groups": 1,
		"hosts_with_active_failures": 0, "has_inventory_sources": false, "summary_fields": map[string]any{"organization": named(1, "Default")}})
	s.Add("/inventories/3/inventory_sources/", map[string]any{"id": 9, "name": "from git", "source": "scm", "source_path": "inventory/production.yml",
		"status": "successful", "last_updated": ago(3 * time.Hour), "update_on_launch": true, "overwrite": true,
		"summary_fields": map[string]any{"source_project": named(6, "infrastructure")}})
	for i, h := range []string{"web1", "web2", "web3", "db1"} {
		s.Add("/hosts/", map[string]any{"id": 11 + i, "name": h + ".example.com", "inventory": 3, "enabled": true,
			"has_active_failures": h == "web2" || h == "web3", "summary_fields": map[string]any{"inventory": inv}})
	}
	project := named(6, "infrastructure")
	s.Add("/projects/", map[string]any{"id": 6, "name": "infrastructure", "organization": 1, "scm_type": "git",
		"scm_url": "https://git.example.com/ops/infrastructure.git", "scm_branch": "main", "status": "successful",
		"scm_revision": "9f3c2ab41d07e5b8c6a1f0d2e4b7a9c3d5e6f708", "summary_fields": map[string]any{"organization": named(1, "Default")}})

	templates := []struct {
		id       int
		name, pb string
	}{{21, "Patch servers", "playbooks/patch.yml"}, {22, "Deploy app", "playbooks/deploy.yml"}, {23, "Rotate certificates", "playbooks/certs.yml"}}
	for _, t := range templates {
		s.Add("/job_templates/", map[string]any{"id": t.id, "name": t.name, "playbook": t.pb, "inventory": 3, "project": 6, "job_type": "run",
			"ask_limit_on_launch": true, "ask_variables_on_launch": t.id == 22,
			"summary_fields": map[string]any{"inventory": inv, "project": project, "organization": named(1, "Default")}})
		s.Add("/unified_job_templates/", map[string]any{"id": t.id, "name": t.name, "type": "job_template"})
	}

	const oldRev, newRev = "4b1d9e07a2c35f6e8d90b1a2c3d4e5f607182930", "9f3c2ab41d07e5b8c6a1f0d2e4b7a9c3d5e6f708"
	job := func(id, tmpl int, name, status, rev, limit, launch, by string, age time.Duration, elapsed float64, expl string) {
		started := ago(age)
		var finished any = ago(age - time.Duration(elapsed*float64(time.Second)))
		if status == "running" {
			finished = nil
		}
		pb := "playbooks/patch.yml"
		for _, t := range templates {
			if t.id == tmpl {
				pb = t.pb
			}
		}
		o := map[string]any{"id": id, "name": name, "status": status, "failed": status == "failed", "job_template": tmpl, "job_type": "run",
			"launch_type": launch, "limit": limit, "playbook": pb, "scm_revision": rev, "created": started, "started": started, "finished": finished,
			"elapsed": elapsed, "job_explanation": expl, "extra_vars": "{}", "event_processing_finished": true,
			"launched_by": map[string]any{"name": by, "type": map[string]string{"scheduled": "schedule", "manual": "user"}[launch]},
			"summary_fields": map[string]any{"job_template": named(tmpl, name), "unified_job_template": named(tmpl, name), "inventory": inv, "project": project,
				"execution_environment": named(2, "default-ee")}}
		s.Add("/jobs/", o)
		s.Add("/unified_jobs/", map[string]any{"id": id, "type": "job", "name": name, "status": status, "failed": status == "failed",
			"launch_type": launch, "created": started, "started": started, "finished": finished, "elapsed": elapsed, "launched_by": o["launched_by"]})
	}
	job(1038, 23, "Rotate certificates", "failed", oldRev, "", "scheduled", "weekly certificates", 20*time.Hour, 61.4, "")
	job(1040, 22, "Deploy app", "successful", oldRev, "web*", "manual", "ann", 26*time.Hour, 143.8, "")
	job(1041, 21, "Patch servers", "successful", oldRev, "", "scheduled", "nightly patching", 25*time.Hour, 312.6, "")
	job(1042, 21, "Patch servers", "failed", newRev, "", "scheduled", "nightly patching", 70*time.Minute, 97.3, "")
	job(1043, 22, "Deploy app", "running", newRev, "web*", "manual", "ann", 2*time.Minute, 118.0, "")

	// Job 1042: the new revision asks for a package the repositories do not have.
	failure := func(n int, host string) map[string]any {
		return map[string]any{"id": 9000 + n, "counter": n, "event": "runner_on_failed", "host_name": host, "play": "Patch servers", "role": "packages",
			"task": "Install the pinned OpenSSL", "failed": true, "changed": false, "parent_uuid": "task-7", "created": ago(69 * time.Minute),
			"stdout":     "fatal: [" + host + "]: FAILED! => {\"changed\": false, \"msg\": \"No package matching 'openssl-3.2.4' is available\"}",
			"event_data": map[string]any{"task_path": "/runner/project/roles/packages/tasks/main.yml:18", "task_action": "ansible.builtin.package", "res": map[string]any{"msg": "No package matching 'openssl-3.2.4' is available"}}}
	}
	fails := []map[string]any{failure(31, "web2.example.com"), failure(32, "web3.example.com"), failure(33, "db1.example.com")}
	s.Handle("/jobs/1042/job_events/", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		out := fails
		if host := q.Get("host_name"); host != "" {
			out = nil
			for _, f := range fails {
				if f["host_name"] == host {
					out = append(out, f)
				}
			}
		}
		if out == nil {
			out = []map[string]any{}
		}
		fakeawx.JSON(w, 200, map[string]any{"count": len(out), "next": nil, "results": out})
	})
	summary := func(host string, ok, failures int) map[string]any {
		return map[string]any{"host_name": host, "failed": failures > 0, "ok": ok, "changed": 1, "failures": failures, "dark": 0, "skipped": 2, "rescued": 0, "ignored": 0,
			"created": ago(68 * time.Minute), "summary_fields": map[string]any{"job": map[string]any{"id": 1042, "name": "Patch servers", "status": "failed", "elapsed": 97.3, "job_template_name": "Patch servers"}}}
	}
	sums := []map[string]any{summary("db1.example.com", 6, 1), summary("web1.example.com", 14, 0), summary("web2.example.com", 6, 1), summary("web3.example.com", 6, 1)}
	hostSums := func(n int) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			out := []map[string]any{}
			for _, x := range sums {
				if r.URL.Query().Get("failed") != "true" || x["failed"] == true {
					out = append(out, x)
				}
			}
			if n > 0 {
				out = make([]map[string]any, n)
			}
			fakeawx.JSON(w, 200, map[string]any{"count": len(out), "next": nil, "results": out})
		}
	}
	s.Handle("/jobs/1042/job_host_summaries/", hostSums(0))
	s.Handle("/jobs/1041/job_host_summaries/", hostSums(4))
	output := strings.Join([]string{
		"PLAY [Patch servers] ***********************************************************",
		"", "TASK [Gathering Facts] *********************************************************",
		"ok: [web1.example.com]", "ok: [web2.example.com]", "ok: [web3.example.com]", "ok: [db1.example.com]",
		"", "TASK [packages : Install the pinned OpenSSL] ***********************************",
		"ok: [web1.example.com]",
		"fatal: [web2.example.com]: FAILED! => {\"changed\": false, \"msg\": \"No package matching 'openssl-3.2.4' is available\"}",
		"fatal: [web3.example.com]: FAILED! => {\"changed\": false, \"msg\": \"No package matching 'openssl-3.2.4' is available\"}",
		"fatal: [db1.example.com]: FAILED! => {\"changed\": false, \"msg\": \"No package matching 'openssl-3.2.4' is available\"}",
		"", "PLAY RECAP *********************************************************************",
		"db1.example.com   : ok=6   changed=1   unreachable=0   failed=1   skipped=2",
		"web1.example.com  : ok=14  changed=1   unreachable=0   failed=0   skipped=2",
		"web2.example.com  : ok=6   changed=1   unreachable=0   failed=1   skipped=2",
		"web3.example.com  : ok=6   changed=1   unreachable=0   failed=1   skipped=2", "",
	}, "\n")
	s.Handle("/jobs/1042/stdout/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("format") != "json" {
			_, _ = w.Write([]byte(output))
			return
		}
		lines := strings.SplitAfter(output, "\n")
		start := 0
		_, _ = fmt.Sscan(r.URL.Query().Get("start_line"), &start)
		start = min(start, len(lines))
		fakeawx.JSON(w, 200, map[string]any{"range": map[string]any{"start": start, "end": len(lines), "absolute_end": len(lines)}, "content": strings.Join(lines[start:], "")})
	})

	// Launching: the three templates prompt for a limit; a launch creates a pending job.
	next := 1043
	for _, t := range templates {
		t := t
		s.Handle(fmt.Sprintf("/job_templates/%d/launch/", t.id), func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet {
				fakeawx.JSON(w, 200, map[string]any{"ask_limit_on_launch": true, "ask_variables_on_launch": t.id == 22, "can_start_without_user_input": true,
					"survey_enabled": false, "variables_needed_to_start": []string{}, "passwords_needed_to_start": []string{},
					"defaults": map[string]any{"inventory": inv, "limit": "", "job_type": "run", "extra_vars": ""}})
				return
			}
			next++
			s.Add("/unified_jobs/", map[string]any{"id": next, "type": "job", "name": t.name, "status": "pending"})
			s.Add("/jobs/", map[string]any{"id": next, "name": t.name, "status": "pending", "job_template": t.id, "created": ago(0), "launch_type": "manual",
				"summary_fields": map[string]any{"job_template": named(t.id, t.name), "inventory": inv}})
			fakeawx.JSON(w, 201, map[string]any{"id": next, "job": next, "status": "pending", "type": "job"})
		})
	}
}

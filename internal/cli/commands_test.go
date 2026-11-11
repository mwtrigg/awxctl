package cli

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mwtrigg/awxctl/internal/config"
	"github.com/mwtrigg/awxctl/internal/fakeawx"
)

// lab starts a fake with a few objects and returns the environment to reach it.
func lab(t *testing.T, flavor, tier string) (*fakeawx.Server, map[string]string) {
	t.Helper()
	srv := fakeawx.New(flavor)
	t.Cleanup(srv.Close)
	srv.Add("/organizations/", map[string]any{"id": 1, "name": "Default"})
	srv.Add("/inventories/", map[string]any{"id": 2, "name": "lab", "organization": 1, "total_hosts": 2, "total_groups": 0})
	srv.Add("/hosts/", map[string]any{"id": 3, "name": "web1", "inventory": 2, "enabled": true, "summary_fields": map[string]any{"inventory": map[string]any{"name": "lab"}}})
	srv.Add("/hosts/", map[string]any{"id": 4, "name": "web2", "inventory": 2, "enabled": true})
	srv.Add("/job_templates/", map[string]any{"id": 7, "name": "Patch", "inventory": 2})
	srv.Add("/jobs/", map[string]any{"id": 12, "name": "Patch", "status": "failed", "failed": true, "launched_by": map[string]any{"name": "ann"}})
	srv.Add("/unified_jobs/", map[string]any{"id": 12, "type": "job"})
	srv.Add("/schedules/", map[string]any{"id": 30, "name": "nightly", "unified_job_template": 7, "enabled": true})
	srv.Handle("/job_templates/7/launch/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			fakeawx.JSON(w, 200, map[string]any{"ask_limit_on_launch": true, "ask_variables_on_launch": true})
			return
		}
		fakeawx.JSON(w, 201, map[string]any{"id": 41, "status": "pending", "type": "job"})
	})
	state := t.TempDir()
	return srv, map[string]string{"AWXCTL_URL": srv.URL, "AWXCTL_API_TOKEN": fakeawx.Token, "AWXCTL_INSECURE": "1", "AWXCTL_PERMISSIONS": tier, "AWXCTL_STATE_DIR": state}
}

func changing(srv *fakeawx.Server) []fakeawx.Request {
	var out []fakeawx.Request
	for _, r := range srv.Requests() {
		if r.Method != http.MethodGet {
			out = append(out, r)
		}
	}
	return out
}

func with(env map[string]string, kv ...string) map[string]string {
	out := map[string]string{}
	for k, v := range env {
		out[k] = v
	}
	for i := 0; i+1 < len(kv); i += 2 {
		out[kv[i]] = kv[i+1]
	}
	return out
}

func TestGetListAndOne(t *testing.T) {
	for _, flavor := range []string{"awx", "aap"} {
		srv, env := lab(t, flavor, "readonly")
		out, errOut, code := runCLI(t, env, "get", "hosts", "--inventory", "lab", "--name", "web", "--filter", "enabled=true", "--limit", "1")
		if code != 0 {
			t.Fatalf("%s: exit %d: %s", flavor, code, errOut)
		}
		last := srv.Requests()[len(srv.Requests())-1].Path
		for _, want := range []string{"inventory__name=lab", "name__icontains=web", "enabled=true", "page_size=1", "order_by=name"} {
			if !strings.Contains(last, want) {
				t.Errorf("%s: the query lacks %s: %s", flavor, want, last)
			}
		}
		var env1 struct {
			Data []map[string]any `json:"data"`
			Meta map[string]any   `json:"meta"`
		}
		if err := json.Unmarshal([]byte(out), &env1); err != nil || len(env1.Data) != 1 || env1.Meta["limit_reached"] != true {
			t.Errorf("%s: envelope: %s", flavor, out)
		}
		out, _, code = runCLI(t, env, "get", "hosts", "id:3", "--field", "inventory")
		if code != 0 || strings.TrimSpace(out) != "lab" {
			t.Errorf("%s: one object: %q exit %d", flavor, out, code)
		}
	}
}

func TestGetErrors(t *testing.T) {
	_, env := lab(t, "awx", "readonly")
	for _, tc := range []struct {
		args []string
		code int
	}{
		{[]string{"get", "nope"}, 5},
		{[]string{"get", "hosts", "missing"}, 2},
		{[]string{"get", "hosts", "--since", "1h"}, 5},
		{[]string{"get", "jobs", "--since", "yesterday-ish"}, 5},
		{[]string{"get", "jobs", "--filter", "page_size=5000"}, 5},
		{[]string{"get", "job-output", "abc"}, 5},
		{[]string{"diagnose"}, 0},
		{[]string{"diagnose", "host"}, 5},
	} {
		if _, errOut, code := runCLI(t, env, tc.args...); code != tc.code {
			t.Errorf("%v: exit %d, want %d (%s)", tc.args, code, tc.code, errOut)
		}
	}
}

// Every changing command, run without --apply, must send nothing.
func TestDryRunSendsNothing(t *testing.T) {
	srv, env := lab(t, "awx", "write")
	srv.Handle("/jobs/12/relaunch/", func(w http.ResponseWriter, _ *http.Request) { fakeawx.JSON(w, 200, map[string]any{}) })
	srv.Handle("/jobs/12/cancel/", func(w http.ResponseWriter, _ *http.Request) { fakeawx.JSON(w, 200, map[string]any{"can_cancel": true}) })
	srv.Add("/projects/", map[string]any{"id": 50, "name": "ops"})
	srv.Handle("/projects/50/update/", func(w http.ResponseWriter, _ *http.Request) { fakeawx.JSON(w, 200, map[string]any{"can_update": true}) })
	srv.Add("/groups/", map[string]any{"id": 60, "name": "webservers", "inventory": 2})
	srv.Add("/groups/60/hosts/", map[string]any{"id": 4, "name": "web2"})
	srv.Add("/workflow_approvals/", map[string]any{"id": 70, "name": "ok?", "status": "pending"})
	srv.Add("/credentials/", map[string]any{"id": 80, "name": "deploy"})
	srv.Add("/users/", map[string]any{"id": 90, "username": "ann"})
	srv.Add("/role_definitions/", map[string]any{"id": 91, "name": "Inventory Use", "content_type": "awx.inventory"})
	srv.Add("/role_user_assignments/", map[string]any{"id": 92, "role_definition": 91, "user": 90, "object_id": 2})
	srv.Add("/role_user_assignments/", map[string]any{"id": 93, "role_definition": 999, "user": 90, "object_id": 999})
	srv.Add("/tokens/", map[string]any{"id": 94, "description": "x"})
	srv.Handle("/inventories/2/copy/", func(w http.ResponseWriter, _ *http.Request) { fakeawx.JSON(w, 200, map[string]any{"can_copy": true}) })
	srv.Handle("/job_templates/7/survey_spec/", func(w http.ResponseWriter, _ *http.Request) {
		fakeawx.JSON(w, 200, map[string]any{"spec": []map[string]any{{"variable": "old", "question_name": "Old?", "type": "text", "required": true}}})
	})
	survey := filepath.Join(t.TempDir(), "q.yml")
	_ = os.WriteFile(survey, []byte("questions:\n  - variable: release\n"), 0o600)
	cases := map[string][]string{
		"token":    {"token", "create", "--description", "agent", "--scope", "read"},
		"grant":    {"grant", "Inventory Use", "--user", "ann", "--on", "inventories=lab", "--dry-run"},
		"revoke":   {"revoke", "Inventory Use", "--user", "ann", "--on", "inventories=lab"},
		"copy":     {"copy", "inventories", "lab", "--name", "lab2"},
		"set":      {"set", "survey", "Patch", "--file", survey},
		"launch":   {"launch", "Patch", "--limit", "web1", "-e", "a=1"},
		"relaunch": {"relaunch", "12"},
		"cancel":   {"cancel", "12"},
		"sync":     {"sync", "projects", "ops"},
		"approve":  {"approve", "70"},
		"deny":     {"deny", "70"},
		"create":   {"create", "hosts", "web9", "--set", "inventory=lab"},
		"update":   {"update", "hosts", "web1", "--set", "description=x"},
		"delete":   {"delete", "inventories", "lab"},
		"enable":   {"disable", "hosts", "web1", "web2"},
		"disable":  {"disable", "schedules", "nightly"},
		"attach":   {"attach", "hosts", "web1", "--to", "groups=webservers"},
		"detach":   {"detach", "hosts", "web2", "--to", "groups=webservers"},
		"run":      {"run", "command", "--inventory", "lab", "--limit", "web1", "--credential", "deploy", "--module", "ping"},
		"api":      {"api", "delete", "/hosts/3/"},
	}
	for name, args := range cases {
		out, errOut, code := runCLI(t, env, args...)
		if code != 0 {
			t.Errorf("%s: dry run failed, exit %d: %s", name, code, errOut)
			continue
		}
		if name == "grant" { // ann already has the role: nothing to do
			if !strings.Contains(errOut, "nothing to change") {
				t.Errorf("grant of a role that is already held: %s", errOut)
			}
			continue
		}
		if !strings.Contains(out, `"status": "planned"`) || !strings.Contains(errOut, "dry run: nothing was changed") {
			t.Errorf("%s: not shown as a plan:\n%s\n%s", name, out, errOut)
		}
	}
	if w := changing(srv); len(w) != 0 {
		t.Errorf("dry runs sent changing requests: %+v", w)
	}
	// Every changing top-level command has a case above: a new one must be added
	// here and to writeCommands, or this fails.
	for name := range writeCommands {
		if _, ok := cases[name]; !ok {
			t.Errorf("no dry-run case for the changing command %q", name)
		}
	}
	read := map[string]bool{"upgrade": true, "stats": true, "doctor": true, "auth": true, "get": true, "diagnose": true, "wait": true, "context": true, "state": true, "api": true, "llm": true, "mcp": true, "version": true, "help": true, "completion": true}
	for _, c := range rootCmd.Commands() {
		if !writeCommands[c.Name()] && !read[c.Name()] {
			t.Errorf("command %q is neither in writeCommands nor known as read-only", c.Name())
		}
	}
}

func TestApplyTiersExpectAndRecord(t *testing.T) {
	srv, env := lab(t, "awx", "readonly")
	if _, errOut, code := runCLI(t, env, "launch", "Patch", "--apply"); code != 8 || len(changing(srv)) != 0 {
		t.Fatalf("readonly must not launch: exit %d %s", code, errOut)
	}
	op := with(env, "AWXCTL_PERMISSIONS", "operator")
	if _, _, code := runCLI(t, op, "delete", "hosts", "web1", "--apply"); code != 8 {
		t.Errorf("operator must not delete: exit %d", code)
	}
	if _, _, code := runCLI(t, op, "disable", "hosts", "web1", "web2", "--expect", "3", "--apply"); code != 5 || len(changing(srv)) != 0 {
		t.Errorf("--expect mismatch must change nothing: exit %d", code)
	}
	out, errOut, code := runCLI(t, op, "launch", "Patch", "--limit", "web1", "-e", "db_password=hunter2", "--apply")
	if code != 0 || !strings.Contains(out, `"job_id": 41`) {
		t.Fatalf("launch: exit %d\n%s\n%s", code, out, errOut)
	}
	if w := changing(srv); len(w) != 1 || !strings.Contains(w[0].Body, "hunter2") {
		t.Errorf("the real value must be sent, once: %+v", w)
	}
	files, _ := filepath.Glob(filepath.Join(env["AWXCTL_STATE_DIR"], "*.json"))
	if len(files) != 1 {
		t.Fatalf("one audit record expected, got %v", files)
	}
	data, _ := os.ReadFile(files[0])
	if strings.Contains(string(data), "hunter2") || strings.Contains(out+errOut, "hunter2") || strings.Contains(string(data), fakeawx.Token) {
		t.Errorf("a secret reached the record or the output:\n%s", data)
	}
	out, _, code = runCLI(t, op, "state", "list", "--field", "outcome")
	if code != 0 || strings.TrimSpace(out) != "1 applied" {
		t.Errorf("state list: %q", out)
	}
	if _, _, code := runCLI(t, op, "state", "show", "../../etc/passwd"); code != 5 {
		t.Errorf("state show must take a file name only: exit %d", code)
	}
	// mode: apply applies without the flag; --dry-run still wins.
	apply := with(op, "AWXCTL_MODE", "apply")
	before := len(changing(srv))
	if _, _, code := runCLI(t, apply, "disable", "hosts", "web1", "--dry-run"); code != 0 || len(changing(srv)) != before {
		t.Errorf("--dry-run must win over mode apply")
	}
	if _, _, code := runCLI(t, apply, "disable", "hosts", "web1"); code != 0 || len(changing(srv)) != before+1 {
		t.Errorf("mode apply must apply")
	}
	if _, _, code := runCLI(t, op, "disable", "hosts", "web1", "--apply", "--dry-run"); code != 5 {
		t.Errorf("--apply with --dry-run must be refused: exit %d", code)
	}
}

func TestSecretsStayOffTheCommandLine(t *testing.T) {
	_, env := lab(t, "awx", "write")
	_, errOut, code := runCLI(t, env, "create", "credentials", "x", "--set", "inputs={\"password\":\"p\"}")
	if code != 5 || !strings.Contains(errOut, "--set-file") {
		t.Errorf("a secret given with --set must be refused: exit %d %s", code, errOut)
	}
	if a := strings.Join(maskArgs([]string{"create", "hosts", "h", "--set", `variables={"ansible_password":"hunter2","ntp":"a"}`, `--set=variables={"become_pass":"hunter3"}`}), " "); strings.Contains(a, "hunter") || !strings.Contains(a, "ntp") {
		t.Errorf("maskArgs must look inside variables: %s", a)
	}
	got := maskArgs([]string{"launch", "t", "-e", "api_token=abc", "--extra-vars={\"db_password\":\"x\",\"n\":1}", "--limit=web1", "note=fine"})
	joined := strings.Join(got, " ")
	if strings.Contains(joined, "abc") || strings.Contains(joined, `"x"`) || !strings.Contains(joined, "--limit=web1") || !strings.Contains(joined, "note=fine") {
		t.Errorf("maskArgs: %v", got)
	}
}

func TestMCPChildPolicy(t *testing.T) {
	srv, env := lab(t, "awx", "write")
	mcp := with(env, "AWXCTL_FROM_MCP", "1", "AWXCTL_MODE", "dry-run")
	for _, args := range [][]string{
		{"run", "command", "--inventory", "lab", "--limit", "web1", "--credential", "c", "--module", "ping"},
		{"api", "get", "/hosts/"},
		{"context", "use", "x"},
		{"get", "job-output", "12", "--save", "/tmp/x"},
		{"create", "hosts", "h", "--file", "/etc/passwd"},
		{"get", "hosts", "--insecure"},
		{"disable", "hosts", "web1", "--apply"}, // no AWXCTL_MCP_WRITES
	} {
		if _, errOut, code := runCLI(t, mcp, args...); code != 5 || !strings.Contains(errOut, "MCP") {
			t.Errorf("%v must be refused over MCP: exit %d %s", args, code, errOut)
		}
	}
	// Launching over MCP needs the allowlist, even with writes on.
	writesOn := with(mcp, "AWXCTL_MCP_WRITES", "1")
	if _, errOut, code := runCLI(t, writesOn, "launch", "Patch", "--apply"); code != 8 {
		t.Errorf("a launch without an allowlist must be refused over MCP: exit %d %s", code, errOut)
	}
	if len(changing(srv)) != 0 {
		t.Errorf("refused commands sent requests: %+v", changing(srv))
	}
	if _, _, code := runCLI(t, mcp, "get", "hosts"); code != 0 {
		t.Errorf("a read must work over MCP: exit %d", code)
	}
}

func TestMCPToolsAndArgv(t *testing.T) {
	names := func(tools []mcpTool) string {
		var n []string
		for _, tl := range tools {
			n = append(n, tl.Name)
		}
		return " " + strings.Join(n, " ") + " "
	}
	read, all := names(mcpTools(false, false)), names(mcpTools(true, false))
	for _, never := range []string{" run_command ", " api ", " context_use ", " mcp ", " wait "} {
		if strings.Contains(all, never) {
			t.Errorf("%s must never be an MCP tool", never)
		}
	}
	for _, w := range []string{" launch ", " delete ", " create ", " cancel "} {
		if strings.Contains(read, w) || !strings.Contains(all, w) {
			t.Errorf("%s must be offered only with --allow-writes", w)
		}
	}
	for _, r := range []string{" get ", " diagnose_job ", " get_job_output ", " get_job_events ", " get_host_jobs "} {
		if !strings.Contains(read, r) {
			t.Errorf("read tool %s is missing", r)
		}
	}
	var get, launch mcpTool
	for _, tl := range mcpTools(true, false) {
		switch tl.Name {
		case "get":
			get = tl
		case "launch":
			launch = tl
		}
		props := tl.InputSchema["properties"].(map[string]any)
		for _, denied := range []string{"save", "file", "set_file", "id_file", "extra_vars_file", "context", "insecure", "follow", "output"} {
			if _, ok := props[denied]; ok {
				t.Errorf("tool %s offers the flag %s", tl.Name, denied)
			}
		}
	}
	argv, err := mcpArgv(get, map[string]any{"kind": "jobs", "failed": true, "limit": float64(5), "filter": []any{"status=failed"}, "args": []any{"--insecure"}}, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(argv, " "); got != "get jobs --failed=true --filter=status=failed --limit=5 --output=json -- --insecure" {
		t.Errorf("argv: %s", got)
	}
	if _, err := mcpArgv(get, map[string]any{"kind": "job-output"}, false); err == nil {
		t.Error("only object kinds are valid for get")
	}
	if _, err := mcpArgv(get, map[string]any{"kind": "jobs", "save": "x"}, false); err == nil {
		t.Error("an unknown parameter must be refused")
	}
	if _, err := mcpArgv(launch, map[string]any{"args": []any{"Patch"}, "apply": true}, false); err == nil {
		t.Error("apply without --allow-writes must be refused")
	}
	argv, _ = mcpArgv(launch, map[string]any{"args": []any{"Patch"}, "apply": true}, true)
	if got := strings.Join(argv, " "); got != "launch --apply --output=json -- Patch" {
		t.Errorf("argv: %s", got)
	}
}

func TestCapabilities(t *testing.T) {
	out, _, code := runCLI(t, nil, "llm", "capabilities")
	var doc struct {
		Commands []CommandInfo    `json:"commands"`
		Kinds    []map[string]any `json:"kinds"`
	}
	if code != 0 || json.Unmarshal([]byte(out), &doc) != nil || len(doc.Kinds) < 25 {
		t.Fatalf("capabilities: exit %d", code)
	}
	seen := map[string]bool{}
	for _, c := range doc.Commands {
		seen[c.Command] = true
		if c.Summary == "" {
			t.Errorf("command %q has no summary", c.Command)
		}
		if c.Writes != writeCommands[strings.Fields(c.Command)[0]] && c.Command != "context use" {
			t.Errorf("command %q: writes flag is wrong", c.Command)
		}
	}
	for _, want := range []string{"launch", "diagnose job", "get job-output", "delete", "auth status"} {
		if !seen[want] {
			t.Errorf("capabilities lacks %q", want)
		}
	}
}

func TestLaunchAllowlist(t *testing.T) {
	patch := map[string]any{"id": 7, "name": "Patch"}
	other := map[string]any{"id": 8, "name": "Wipe"}
	open := &config.Context{}
	listed := &config.Context{AllowedTemplates: []string{"Patch", "id:9"}}
	if allowed(open, other) != nil {
		t.Error("without a list a person may launch anything the token allows")
	}
	if allowed(listed, patch) != nil || allowed(listed, map[string]any{"id": 9, "name": "x"}) != nil {
		t.Error("a listed name or id must be allowed")
	}
	var pe *config.PermissionError
	if err := allowed(listed, other); !errors.As(err, &pe) {
		t.Errorf("an unlisted template must be refused: %v", err)
	}
	t.Setenv("AWXCTL_FROM_MCP", "1")
	if allowed(open, patch) == nil {
		t.Error("over MCP an empty list allows nothing")
	}
	if allowed(listed, patch) != nil {
		t.Error("over MCP a listed template is allowed")
	}
}

func TestDoctorReportsWhatIsMissing(t *testing.T) {
	// The bare fake serves only a few paths: doctor must say so and exit 1,
	// and it must only read.
	srv, env := lab(t, "aap", "readonly")
	out, errOut, code := runCLI(t, env, "doctor", "-o", "jsonl")
	if code != 1 || !strings.Contains(errOut, "checks are not ok") {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	status := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		var row struct{ Check, Status, Detail string }
		if json.Unmarshal([]byte(line), &row) == nil {
			status[row.Check] = row.Status
		}
	}
	if status["list hosts"] != "ok" || status["list credentials"] != "missing" || status["host facts"] != "missing" || status["who am I"] != "ok" {
		t.Errorf("statuses: hosts=%s credentials=%s facts=%s me=%s", status["list hosts"], status["list credentials"], status["host facts"], status["who am I"])
	}
	if !strings.Contains(out, "aap, controller 4.6.2") || !strings.Contains(out, "NOT a version awxctl is tested with") {
		t.Errorf("the version verdict is missing:\n%s", strings.SplitN(out, "\n", 2)[0])
	}
	if w := changing(srv); len(w) != 0 {
		t.Errorf("doctor must send GET requests only by default: %+v", w)
	}
	if status["schedule preview"] != "skipped" {
		t.Errorf("the preview check must be skipped by default: %s", status["schedule preview"])
	}
	runCLI(t, env, "doctor", "--with-preview", "-o", "jsonl")
	if w := changing(srv); len(w) != 1 || !strings.HasSuffix(w[0].Path, "/schedules/preview/") {
		t.Errorf("--with-preview adds exactly the preview POST: %+v", w)
	}
}

func TestHostNameInSeveralInventories(t *testing.T) {
	srv, env := lab(t, "awx", "readonly")
	srv.Add("/hosts/", map[string]any{"id": 40, "name": "web1", "inventory": 9, "summary_fields": map[string]any{"inventory": map[string]any{"name": "other"}}})
	sum := func(job int) map[string]any {
		return map[string]any{"count": 1, "results": []map[string]any{{"host_name": "web1", "failed": false, "ok": 2, "summary_fields": map[string]any{"job": map[string]any{"id": job, "name": "j", "status": "successful", "elapsed": 3.5, "job_template_name": "Patch"}}}}}
	}
	srv.Handle("/hosts/3/job_host_summaries/", func(w http.ResponseWriter, _ *http.Request) { fakeawx.JSON(w, 200, sum(100)) })
	srv.Handle("/hosts/40/job_host_summaries/", func(w http.ResponseWriter, _ *http.Request) { fakeawx.JSON(w, 200, sum(200)) })
	out, errOut, code := runCLI(t, env, "get", "host-jobs", "web1", "-o", "jsonl")
	if code != 0 || !strings.Contains(errOut, "2 hosts in different inventories") {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], `"job_id":200`) || !strings.Contains(lines[0], `"inventory":"other"`) || !strings.Contains(lines[1], `"inventory":"lab"`) || !strings.Contains(lines[0], `"job_seconds":3.5`) {
		t.Errorf("both hosts' jobs, newest first, each with its inventory:\n%s", out)
	}
	// a write still refuses an ambiguous name
	if _, _, code := runCLI(t, env, "disable", "hosts", "web1"); code != 5 {
		t.Errorf("a change to an ambiguous name must be refused: exit %d", code)
	}
	// the envelope says how many match on the server
	out, _, _ = runCLI(t, env, "get", "hosts", "--limit", "1")
	if !strings.Contains(out, `"total": 3`) || !strings.Contains(out, `"count": 1`) {
		t.Errorf("meta.total: %s", out)
	}
}

func TestOneInventoryShowsItsSources(t *testing.T) {
	srv, env := lab(t, "aap", "readonly")
	srv.Handle("/inventories/2/inventory_sources/", func(w http.ResponseWriter, _ *http.Request) {
		fakeawx.JSON(w, 200, map[string]any{"count": 1, "results": []map[string]any{{"id": 31, "name": "from git", "source": "scm", "source_path": "inventory/hosts.yml",
			"status": "successful", "update_on_launch": true, "summary_fields": map[string]any{"source_project": map[string]any{"name": "infra"}}}}})
	})
	out, errOut, code := runCLI(t, env, "get", "inventories", "lab")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	for _, want := range []string{`"total_hosts": 2`, `"source": "scm"`, `"source_project": "infra"`, `"source_path": "inventory/hosts.yml"`, `"status": "successful"`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s in:\n%s", want, out)
		}
	}
	// the list stays one request and has no sources
	before := len(srv.Requests())
	out, _, _ = runCLI(t, env, "get", "inventories")
	if strings.Contains(out, `"sources"`) || len(srv.Requests())-before > 3 {
		t.Errorf("the list must not look up sources: %d requests", len(srv.Requests())-before)
	}
}

func TestValuesKeepTheirText(t *testing.T) {
	for in, want := range map[string]any{
		"1.10": "1.10", "1e3": "1e3", "007": "007", "12345678901234567891": "12345678901234567891", "+5": "+5", " 5": " 5", "0x10": "0x10", "NaN": "NaN", "1.": "1.",
		"5": int64(5), "-3": int64(-3), "2.5": 2.5, "0": int64(0), "true": true, "false": false, "null": nil, "hello": "hello", "": "", "\"quoted\"": "\"quoted\"",
	} {
		if got := parseValue(in); got != want {
			t.Errorf("parseValue(%q) = %#v, want %#v", in, got, want)
		}
	}
	if v, ok := parseValue(`{"n": 12345678901234567891, "v": 1.10}`).(map[string]any); !ok || mustJSONText(v) != `{"n":12345678901234567891,"v":1.10}` {
		t.Errorf("numbers inside a JSON object must stay exactly as written: %v", mustJSONText(v))
	}
	if k, v, err := splitAssignment("-e", "count:=3"); err != nil || k != "count" || mustJSONText(v) != "3" {
		t.Errorf(":= forces JSON: %v %v %v", k, v, err)
	}
	if k, v, err := splitAssignment("-e", `version:="1.10"`); err != nil || k != "version" || v != "1.10" {
		t.Errorf(":= with a JSON string: %v %v %v", k, v, err)
	}
	if _, _, err := splitAssignment("-e", "x:=not json"); err == nil {
		t.Error(":= with bad JSON must be refused")
	}
	// end to end: what is sent
	srv, env := lab(t, "awx", "operator")
	out, errOut, code := runCLI(t, env, "launch", "Patch", "-e", "app_version=1.10", "-e", "ticket=12345678901234567891", "-e", "build=1e3", "-e", "n=5")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	for _, want := range []string{`"app_version": "1.10"`, `"ticket": "12345678901234567891"`, `"build": "1e3"`, `"n": 5`} {
		if !strings.Contains(out, want) {
			t.Errorf("the plan lacks %s:\n%s", want, out)
		}
	}
	_ = srv
}

func mustJSONText(v any) string { b, _ := json.Marshal(v); return string(b) }

// The launch allowlist must hold over MCP: nothing an MCP agent is offered may
// change what a template runs, schedule one, or answer for a person.
func TestMCPCannotGetAroundTheAllowlist(t *testing.T) {
	srv, env := lab(t, "awx", "write")
	mcp := with(env, "AWXCTL_FROM_MCP", "1", "AWXCTL_MODE", "dry-run", "AWXCTL_MCP_WRITES", "1")
	srv.Add("/workflow_approvals/", map[string]any{"id": 70, "name": "ok?", "status": "pending"})
	srv.Add("/projects/", map[string]any{"id": 50, "name": "ops"})
	for _, args := range [][]string{
		{"enable", "schedules", "nightly", "--apply"},
		{"disable", "schedules", "nightly", "--apply"},
		{"create", "schedules", "x", "--set", "unified_job_template=Patch", "--apply"},
		{"update", "job-templates", "Patch", "--set", "playbook=other.yml", "--apply"},
		{"update", "job-templates", "Patch", "--set", "name=Renamed", "--apply"},
		{"update", "projects", "ops", "--set", "scm_branch=evil", "--apply"},
		{"delete", "job-templates", "Patch", "--apply"},
		{"copy", "job-templates", "Patch", "--name", "Patch2", "--apply"},
		{"grant", "Inventory Use", "--user", "ann", "--on", "inventories=lab", "--apply"},
		{"set", "survey", "Patch", "--remove", "--apply"},
		{"approve", "70", "--apply"},
		{"deny", "70", "--apply"},
		{"attach", "credentials", "deploy", "--to", "job-templates=Patch", "--apply"},
		{"attach", "hosts", "web1", "--to", "job-templates=Patch", "--apply"},
		{"launch", "Patch", "--scm-branch", "evil", "--apply"},
		{"launch", "Patch", "--inventory", "lab", "--apply"},
		{"context", "list"},
		{"token", "create", "--description", "x", "--apply"},
	} {
		if _, errOut, code := runCLI(t, mcp, args...); code != 5 || !strings.Contains(errOut, "not allowed over MCP") {
			t.Errorf("%v must be refused over MCP: exit %d %s", args, code, errOut)
		}
	}
	if w := changing(srv); len(w) != 0 {
		t.Fatalf("refused commands sent requests: %+v", w)
	}
	// What stays possible: hosts, groups, inventories.
	if _, errOut, code := runCLI(t, mcp, "disable", "hosts", "web1", "--apply"); code != 0 {
		t.Errorf("a host change must work over MCP with writes on: exit %d %s", code, errOut)
	}
	// Approvals only with the explicit switch.
	srv.Handle("/workflow_approvals/70/approve/", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) })
	if _, errOut, code := runCLI(t, with(mcp, "AWXCTL_MCP_APPROVALS", "1"), "approve", "70", "--apply"); code != 0 {
		t.Errorf("approve with --allow-approvals: exit %d %s", code, errOut)
	}
	// The tool list matches: none of the refused commands is offered.
	offered := map[string]bool{}
	for _, tl := range mcpTools(true, false) {
		offered[tl.Name] = true
		if tl.Name == "launch" {
			props := tl.InputSchema["properties"].(map[string]any)
			if props["scm_branch"] != nil || props["inventory"] != nil {
				t.Error("launch over MCP must not offer scm_branch or inventory")
			}
		}
	}
	for _, never := range []string{"approve", "deny", "grant", "revoke", "copy", "set_survey", "set_workflow_graph", "context_list", "token_create", "upgrade"} {
		if offered[never] {
			t.Errorf("%s must not be an MCP tool without its switch", never)
		}
	}
	withApprovals := map[string]bool{}
	for _, tl := range mcpTools(true, true) {
		withApprovals[tl.Name] = true
	}
	if !withApprovals["approve"] || !withApprovals["deny"] || !offered["launch"] || !offered["update"] {
		t.Error("approve and deny appear with --allow-approvals; launch and update stay")
	}
}

func TestDeniedChangeExitsEight(t *testing.T) {
	srv, env := lab(t, "awx", "operator")
	srv.Handle("/job_templates/7/launch/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			fakeawx.JSON(w, 200, map[string]any{})
			return
		}
		fakeawx.JSON(w, 403, map[string]any{"detail": "You do not have permission to perform this action."})
	})
	_, errOut, code := runCLI(t, env, "launch", "Patch", "--apply")
	if code != 8 || !strings.Contains(errOut, "HTTP 403") || !strings.Contains(errOut, `"kind":"permission"`) {
		t.Errorf("a change the controller denies exits 8 and says why: exit %d %s", code, errOut)
	}
}

func TestMCPRunsNeverPruneRecords(t *testing.T) {
	_, env := lab(t, "awx", "operator")
	env = with(env, "AWXCTL_STATE_KEEP", "1")
	count := func() int {
		files, _ := filepath.Glob(filepath.Join(env["AWXCTL_STATE_DIR"], "*.json"))
		return len(files)
	}
	mcp := with(env, "AWXCTL_FROM_MCP", "1", "AWXCTL_MODE", "dry-run", "AWXCTL_MCP_WRITES", "1")
	for _, verb := range []string{"disable", "enable", "disable"} {
		if _, errOut, code := runCLI(t, mcp, verb, "hosts", "web1", "--apply"); code != 0 {
			t.Fatalf("exit %d: %s", code, errOut)
		}
	}
	if n := count(); n != 3 {
		t.Errorf("an agent's changes must not push older records out: %d records, want 3", n)
	}
	if _, _, code := runCLI(t, env, "enable", "hosts", "web1", "--apply"); code != 0 || count() != 1 {
		t.Errorf("a person's run prunes to state_keep: %d records", count())
	}
}

func TestListDetails(t *testing.T) {
	_, env := lab(t, "awx", "readonly")
	out, _, code := runCLI(t, env, "get", "hosts", "--filter", "name=no-such-host")
	if code != 0 || !strings.Contains(out, `"total": 0`) {
		t.Errorf("a list with no match still says total 0: exit %d %s", code, out)
	}
	if _, errOut, code := runCLI(t, env, "get", "hosts", "--limit", "0"); code != 5 || !strings.Contains(errOut, "meta.total") {
		t.Errorf("--limit 0 must be refused with a pointer to the count: exit %d %s", code, errOut)
	}
	if _, errOut, code := runCLI(t, with(env, "AWXCTL_MODE", "aply"), "get", "hosts"); code != 5 || !strings.Contains(errOut, "aply") {
		t.Errorf("a misspelt mode must be refused: exit %d %s", code, errOut)
	}
}

func TestDevBuildIsNewerThanItsRelease(t *testing.T) {
	for _, tc := range []struct {
		current, latest string
		older           bool
	}{
		{"v0.1.0-3-gdf7ff83", "v0.1.0", true},
		{"0.1.0-3-gdf7ff83-dirty", "v0.1.0", true},
		{"v0.1.0-3-gdf7ff83", "v0.1.1", false},
		{"v0.1.0-rc.1", "v0.1.0", false},
		{"v0.1.0", "v0.1.0-rc.1", true},
		{"v0.2.0", "v0.1.9", true},
	} {
		if got := olderRelease(tc.current, tc.latest); got != tc.older {
			t.Errorf("olderRelease(%s, %s) = %v, want %v", tc.current, tc.latest, got, tc.older)
		}
	}
}

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
	read := map[string]bool{"auth": true, "get": true, "diagnose": true, "wait": true, "context": true, "state": true, "api": true, "llm": true, "mcp": true, "version": true, "help": true, "completion": true}
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
	read, all := names(mcpTools(false)), names(mcpTools(true))
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
	for _, tl := range mcpTools(true) {
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

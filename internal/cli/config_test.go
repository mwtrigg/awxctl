package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigInitWritesAPrivateFileOnce(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AWXCTL_TEST_HOME", home)
	path := filepath.Join(home, ".config", "awxctl", "config.yaml")
	out, _, code := runCLI(t, nil, "config", "path")
	if code != 0 || strings.TrimSpace(out) != path {
		t.Fatalf("config path: %d %q", code, out)
	}
	// The token comes in on standard input, never as a flag.
	r, w, _ := os.Pipe()
	_, _ = w.WriteString("fake-token\n")
	w.Close()
	old := os.Stdin
	os.Stdin = r
	_, errOut, code := runCLI(t, nil, "config", "init", "--context", "lab", "--url", "http://awx.example.com/", "--token-stdin")
	os.Stdin = old
	if code == 0 || !strings.Contains(errOut, "https") {
		t.Fatalf("a plain http address is not written into a config: %d %s", code, errOut)
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("a refused init must write nothing")
	}
	r, w, _ = os.Pipe()
	_, _ = w.WriteString("fake-token\n")
	w.Close()
	os.Stdin = r
	_, errOut, code = runCLI(t, nil, "config", "init", "--context", "lab", "--url", "https://awx.example.com/", "--token-stdin", "--permissions", "operator")
	os.Stdin = old
	if code != 0 || !strings.Contains(errOut, "wrote "+path) {
		t.Fatalf("config init: %d %s", code, errOut)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("the file is private: %v %v", err, info)
	}
	data, _ := os.ReadFile(path)
	for _, want := range []string{"current_context: lab", "url: https://awx.example.com\n", `api_token: "fake-token"`, "permissions: operator"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("the file lacks %q:\n%s", want, data)
		}
	}
	// It is a config awxctl reads: the context is there with its tier.
	out, errOut, code = runCLI(t, nil, "context", "list", "-o", "jsonl")
	if code != 0 || !strings.Contains(out, `"name":"lab"`) || !strings.Contains(out, `"permissions":"operator"`) {
		t.Errorf("the written config is read back: %d %s %s", code, out, errOut)
	}
	// Never over an existing file.
	if _, errOut, code := runCLI(t, nil, "config", "init", "--url", "https://other.example.com"); code != 5 || !strings.Contains(errOut, "already exists") {
		t.Errorf("a second init: %d %s", code, errOut)
	}
	if again, _ := os.ReadFile(path); string(again) != string(data) {
		t.Error("the existing file was changed")
	}
	for _, c := range []struct {
		want string
		args []string
	}{
		{"--permissions is", []string{"config", "init", "--permissions", "admin"}},
		{"--context is", []string{"config", "init", "--context", "a b"}},
		{"unknown flag: --token", []string{"config", "init", "--token", "x"}},
	} {
		if _, errOut, code := runCLI(t, nil, c.args...); code != 5 || !strings.Contains(errOut, c.want) {
			t.Errorf("%v: %d %s (want %q)", c.args, code, errOut, c.want)
		}
	}
}

// One object as a table is printed down the page, one field a line.
func TestOneObjectAsATableIsARecord(t *testing.T) {
	_, env := lab(t, "awx", "readonly")
	out, errOut, code := runCLI(t, env, "get", "hosts", "web1", "-o", "table")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) < 3 || strings.Contains(out, "----") {
		t.Fatalf("one field a line, no table rule:\n%s", out)
	}
	found := false
	for _, l := range lines {
		f := strings.Fields(l)
		found = found || (len(f) == 2 && f[0] == "name" && f[1] == "web1")
	}
	if !found {
		t.Errorf("a line 'name  web1':\n%s", out)
	}
	// A list stays a table, and JSON is unchanged.
	if out, _, _ := runCLI(t, env, "get", "hosts", "-o", "table"); !strings.Contains(out, "----") {
		t.Errorf("a list is still a table:\n%s", out)
	}
	if out, _, _ := runCLI(t, env, "get", "hosts", "web1"); !strings.Contains(out, `"name": "web1"`) {
		t.Errorf("JSON is as before:\n%s", out)
	}
}

// A context that applies by default is a person's convenience: a command
// started by the MCP server still only shows what it would do.
func TestMCPChildDoesNotApplyBecauseOfTheContextMode(t *testing.T) {
	srv, env := lab(t, "awx", "write")
	env["AWXCTL_MODE"] = "apply"
	env["AWXCTL_FROM_MCP"] = "1"
	_, errOut, code := runCLI(t, env, "disable", "hosts", "web1")
	if code != 0 || !strings.Contains(errOut, "dry run") || len(changing(srv)) != 0 {
		t.Errorf("mode: apply must not reach an MCP child: exit %d %s, %d changes", code, errOut, len(changing(srv)))
	}
	// A person with the same context does apply.
	delete(env, "AWXCTL_FROM_MCP")
	if _, errOut, code := runCLI(t, env, "disable", "hosts", "web1"); code != 0 || len(changing(srv)) != 1 {
		t.Errorf("mode: apply for a person: exit %d %s, %d changes", code, errOut, len(changing(srv)))
	}
}

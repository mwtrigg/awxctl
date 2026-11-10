package cli

import (
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/spf13/pflag"

	"github.com/mwtrigg/awxctl/internal/fakeawx"
)

// runCLI runs one command in-process with a clean HOME and environment and
// returns stdout, stderr and the exit code.
func runCLI(t *testing.T, env map[string]string, args ...string) (string, string, int) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	for _, k := range []string{"AWXCTL_URL", "AWXCTL_API_TOKEN", "AWXCTL_CONTEXT", "AWXCTL_INSECURE", "AWXCTL_FLAVOR", "AWXCTL_PERMISSIONS", "AWXCTL_MODE", "AWXCTL_USERNAME", "AWXCTL_PASSWORD"} {
		t.Setenv(k, "")
	}
	for k, v := range env {
		t.Setenv(k, v)
	}
	rootCmd.PersistentFlags().VisitAll(func(f *pflag.Flag) { _ = f.Value.Set(f.DefValue); f.Changed = false })

	oldOut, oldErr := os.Stdout, os.Stderr
	outR, outW, _ := os.Pipe()
	errR, errW, _ := os.Pipe()
	os.Stdout, os.Stderr = outW, errW
	rootCmd.SetArgs(args)
	code := Execute()
	outW.Close()
	errW.Close()
	os.Stdout, os.Stderr = oldOut, oldErr
	out, _ := io.ReadAll(outR)
	errOut, _ := io.ReadAll(errR)
	return string(out), string(errOut), code
}

func TestAuthStatusBothFlavors(t *testing.T) {
	for flavor, path := range map[string]string{"awx": "/api/v2", "aap": "/api/controller/v2"} {
		t.Run(flavor, func(t *testing.T) {
			srv := fakeawx.New(flavor)
			defer srv.Close()
			out, errOut, code := runCLI(t, map[string]string{"AWXCTL_URL": srv.URL, "AWXCTL_API_TOKEN": fakeawx.Token, "AWXCTL_INSECURE": "1"}, "auth", "status")
			if code != 0 {
				t.Fatalf("exit %d: %s", code, errOut)
			}
			for _, want := range []string{`"flavor": "` + flavor + `"`, `"api_path": "` + path + `"`, `"username": "admin"`, `"permissions": "readonly"`, `"mode": "dry-run"`, `"auth": "token"`} {
				if !strings.Contains(out, want) {
					t.Errorf("missing %s in:\n%s", want, out)
				}
			}
			if strings.Contains(out+errOut, fakeawx.Token) {
				t.Error("the token must never be printed")
			}
		})
	}
}

func TestExitCodes(t *testing.T) {
	srv := fakeawx.New("awx")
	defer srv.Close()
	good := map[string]string{"AWXCTL_URL": srv.URL, "AWXCTL_API_TOKEN": fakeawx.Token, "AWXCTL_INSECURE": "1"}
	for _, tc := range []struct {
		name string
		env  map[string]string
		args []string
		code int
		kind string
	}{
		{"not configured", nil, []string{"auth", "status"}, 5, "usage"},
		{"bad token", map[string]string{"AWXCTL_URL": srv.URL, "AWXCTL_API_TOKEN": "nope", "AWXCTL_INSECURE": "1"}, []string{"auth", "status"}, 3, "auth"},
		{"untrusted certificate", map[string]string{"AWXCTL_URL": srv.URL, "AWXCTL_API_TOKEN": fakeawx.Token}, []string{"auth", "status"}, 4, "connection"},
		{"unknown context", good, []string{"auth", "status", "-c", "nope"}, 1, ""},
		{"unknown flag", good, []string{"auth", "status", "--nope"}, 5, "usage"},
		{"unknown format", good, []string{"version", "-o", "xml"}, 5, "usage"},
		{"bad flavor", map[string]string{"AWXCTL_URL": srv.URL, "AWXCTL_FLAVOR": "tower"}, []string{"auth", "status"}, 5, "usage"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, errOut, code := runCLI(t, tc.env, tc.args...)
			if code != tc.code {
				t.Fatalf("exit %d, want %d: %s", code, tc.code, errOut)
			}
			if tc.kind == "" {
				return
			}
			var e struct {
				Error string `json:"error"`
				Kind  string `json:"kind"`
				Code  int    `json:"code"`
			}
			if err := json.Unmarshal([]byte(errOut), &e); err != nil || e.Code != tc.code || e.Error == "" {
				t.Fatalf("stderr is not the structured error: %q", errOut)
			}
			if !strings.HasPrefix(e.Kind, tc.kind[:4]) {
				t.Errorf("kind %q, want %q", e.Kind, tc.kind)
			}
		})
	}
}

func TestFlagBeatsEnvironment(t *testing.T) {
	srv := fakeawx.New("awx")
	defer srv.Close()
	// No AWXCTL_INSECURE: the --insecure flag alone must be enough.
	_, errOut, code := runCLI(t, map[string]string{"AWXCTL_URL": srv.URL, "AWXCTL_API_TOKEN": fakeawx.Token}, "auth", "status", "--insecure", "-o", "table")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
}

func TestVersion(t *testing.T) {
	out, _, code := runCLI(t, nil, "version", "--field", "version")
	if code != 0 || strings.TrimSpace(out) != "dev" {
		t.Errorf("got %q, exit %d", out, code)
	}
}

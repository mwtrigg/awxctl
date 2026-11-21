package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mwtrigg/awxctl/internal/version"
)

// "upgrade" is the decision: no question, no --apply. Only --check and
// --dry-run hold it back.
func TestUpgradeInstallsWithoutBeingAsked(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"tag_name": "v9.9.9", "assets": []}`))
	}))
	defer srv.Close()
	oldAPI, oldInstall, oldVersion := githubReleaseAPI, installRelease, version.Version
	defer func() { githubReleaseAPI, installRelease, version.Version = oldAPI, oldInstall, oldVersion }()
	githubReleaseAPI, version.Version = srv.URL, "1.0.0"
	installed := 0
	installRelease = func(_ context.Context, rel *githubRelease) error {
		if rel.TagName != "v9.9.9" {
			t.Errorf("installing %q", rel.TagName)
		}
		installed++
		return nil
	}
	// Standard input is not a terminal here, as for a script or an agent.
	out, errOut, code := runCLI(t, nil, "upgrade")
	if code != 0 || installed != 1 || !strings.Contains(out, `"updated": true`) || strings.Contains(errOut, "continue?") {
		t.Fatalf("upgrade must install without a flag or a question: exit %d, %d installs\n%s\n%s", code, installed, out, errOut)
	}
	for _, args := range [][]string{{"upgrade", "--check"}, {"upgrade", "--dry-run"}} {
		out, errOut, code := runCLI(t, nil, args...)
		if code != 0 || installed != 1 || !strings.Contains(out, `"update_available": true`) {
			t.Errorf("%v only looks: exit %d, %d installs\n%s\n%s", args, code, installed, out, errOut)
		}
	}
	// The old spelling keeps working, for scripts that have it.
	for _, flag := range []string{"--apply", "--yes"} {
		before := installed
		if _, errOut, code := runCLI(t, nil, "upgrade", flag); code != 0 || installed != before+1 {
			t.Errorf("upgrade %s: exit %d %s", flag, code, errOut)
		}
	}
	// Nothing newer: nothing is installed.
	version.Version = "9.9.9"
	before := installed
	if out, _, code := runCLI(t, nil, "upgrade"); code != 0 || installed != before || !strings.Contains(out, `"update_available": false`) {
		t.Errorf("up to date: exit %d, %s", code, out)
	}
	// A newer build than the latest release is never taken back to it.
	version.Version = "10.0.0"
	if _, _, code := runCLI(t, nil, "upgrade"); code != 0 || installed != before {
		t.Errorf("never downgrades: exit %d, %d installs", code, installed-before)
	}
}

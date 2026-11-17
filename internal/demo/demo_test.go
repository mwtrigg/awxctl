package demo

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/mwtrigg/awxctl/internal/fakeawx"
)

// The demo must come up and serve the objects its help text points at.
func TestDemoServesItsStory(t *testing.T) {
	srv, err := fakeawx.NewLocal("awx", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	seed(srv)
	get := func(path string) map[string]any {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+path, nil)
		req.Header.Set("Authorization", "Bearer "+fakeawx.Token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || resp.StatusCode != 200 {
			t.Fatalf("%s: status %d, %v", path, resp.StatusCode, err)
		}
		return out
	}
	if n := get("/api/v2/jobs/?failed=true")["count"]; n != float64(2) {
		t.Errorf("failed jobs: %v, want 2", n)
	}
	if n := get("/api/v2/jobs/1042/job_events/?event__in=runner_on_failed")["count"]; n != float64(3) {
		t.Errorf("failures of job 1042: %v, want 3", n)
	}
	if get("/api/v2/job_templates/21/launch/")["ask_limit_on_launch"] != true {
		t.Error("the template must prompt for a limit")
	}
	if get("/api/v2/ping/")["version"] == nil {
		t.Error("ping must answer")
	}
}

package core

import (
	"context"
	"encoding/pem"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mwtrigg/awxctl/internal/config"
	"github.com/mwtrigg/awxctl/internal/fakeawx"
)

// newTestClient trusts the fake's certificate through ca_file, the way a real
// self-signed controller is configured.
func newTestClient(t *testing.T, srv *fakeawx.Server, mod func(*config.Context)) *Client {
	t.Helper()
	ca := filepath.Join(t.TempDir(), "ca.pem")
	block := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	if err := os.WriteFile(ca, block, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := &config.Context{URL: srv.URL + "/", APIToken: fakeawx.Token, CAFile: ca}
	if mod != nil {
		mod(ctx)
	}
	c, err := NewClient(ctx, Options{Retries: 2})
	if err != nil {
		t.Fatal(err)
	}
	c.backoff = func(int) time.Duration { return time.Millisecond }
	return c
}

func TestDetectFlavors(t *testing.T) {
	for _, tc := range []struct{ flavor, path, version string }{
		{"awx", "/api/v2", "24.6.1"},
		{"aap", "/api/controller/v2", "4.6.2"},
	} {
		t.Run(tc.flavor, func(t *testing.T) {
			srv := fakeawx.New(tc.flavor)
			defer srv.Close()
			c := newTestClient(t, srv, nil)
			info, err := c.Detect(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if string(info.Flavor) != tc.flavor || info.APIPath != tc.path || info.Version != tc.version {
				t.Errorf("got %+v", info)
			}
			me, err := c.Me(context.Background())
			if err != nil || me.Username != "admin" || !me.IsSuperuser {
				t.Errorf("me = %+v, %v", me, err)
			}
		})
	}
}

func TestPinnedFlavorIsNotProbed(t *testing.T) {
	srv := fakeawx.New("awx")
	defer srv.Close()
	c := newTestClient(t, srv, func(ctx *config.Context) { ctx.Flavor = "awx" })
	if _, err := c.Detect(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, r := range srv.Requests() {
		if strings.Contains(r.Path, "/controller/") {
			t.Errorf("a pinned awx flavor must not ask the gateway path: %s", r.Path)
		}
	}
	// Pinning the wrong flavor fails instead of silently using the other one.
	c = newTestClient(t, srv, func(ctx *config.Context) { ctx.Flavor = "aap" })
	if _, err := c.Detect(context.Background()); !errors.Is(err, ErrNotFound) {
		t.Errorf("wrong pin: got %v, want not found", err)
	}
	if _, err := NewClient(&config.Context{URL: srv.URL, Flavor: "tower"}, Options{}); err == nil {
		t.Error("an unknown flavor must be refused")
	}
}

func TestDetectStopsOnRealErrors(t *testing.T) {
	// Not a controller at all: both paths 404.
	srv := fakeawx.New("awx")
	srv.Prefix = "/somewhere/else"
	defer srv.Close()
	c := newTestClient(t, srv, nil)
	if _, err := c.Detect(context.Background()); !errors.Is(err, ErrConnect) {
		t.Errorf("got %v, want a connect error", err)
	}
	// Nothing listening.
	srv2 := fakeawx.New("awx")
	c2 := newTestClient(t, srv2, nil)
	srv2.Close()
	if _, err := c2.Detect(context.Background()); !errors.Is(err, ErrConnect) {
		t.Errorf("got %v, want a connect error", err)
	}
}

func TestReadsRetryWritesDoNot(t *testing.T) {
	srv := fakeawx.New("awx")
	defer srv.Close()
	c := newTestClient(t, srv, func(ctx *config.Context) { ctx.Flavor = "awx" })
	if _, err := c.Detect(context.Background()); err != nil {
		t.Fatal(err)
	}

	srv.FailNext("/api/v2/me/", 503, 502)
	if _, err := c.Me(context.Background()); err != nil {
		t.Fatalf("a read must survive two gateway errors with two retries: %v", err)
	}
	srv.FailNext("/api/v2/me/", 503, 503, 503)
	if _, err := c.Me(context.Background()); !errors.Is(err, ErrConnect) {
		t.Errorf("retries exhausted: got %v, want a connect error", err)
	}
	srv.FailNext("/api/v2/me/") // nothing queued; clear state for the count below

	before := len(srv.Requests())
	srv.FailNext("/api/v2/jobs/7/cancel/", 502)
	err := c.do(context.Background(), http.MethodPost, "/api/v2/jobs/7/cancel/", map[string]any{}, nil)
	var uo *OutcomeUnknownError
	if !errors.As(err, &uo) {
		t.Errorf("a write answered 502 has an unknown outcome: got %v", err)
	}
	if n := len(srv.Requests()) - before; n != 1 {
		t.Errorf("a write must be sent exactly once, sent %d times", n)
	}
}

func TestErrorKinds(t *testing.T) {
	srv := fakeawx.New("awx")
	defer srv.Close()
	bad := newTestClient(t, srv, func(ctx *config.Context) { ctx.APIToken = "wrong"; ctx.Flavor = "awx" })
	if _, err := bad.Detect(context.Background()); err != nil {
		t.Fatal(err) // ping needs no login
	}
	_, err := bad.Me(context.Background())
	if !errors.Is(err, ErrAuth) {
		t.Errorf("got %v, want an auth error", err)
	}
	if strings.Contains(err.Error(), "wrong") {
		t.Error("an error must never repeat the token")
	}

	c := newTestClient(t, srv, func(ctx *config.Context) { ctx.Flavor = "awx" })
	srv.Handle("/hosts/", func(w http.ResponseWriter, _ *http.Request) {
		fakeawx.JSON(w, 400, map[string]any{"name": []string{"This field is required."}, "detail": "Bad request."})
	})
	err = c.do(context.Background(), http.MethodPost, "/api/v2/hosts/", map[string]any{}, nil)
	var api *APIError
	if !errors.As(err, &api) || api.Status != 400 || api.Detail != "Bad request." || len(api.Fields["name"]) != 1 {
		t.Errorf("field messages must be kept: %#v", err)
	}
	if err := c.get(context.Background(), "/api/v2/nothing/", nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("got %v, want not found", err)
	}
	srv.FailNext("/api/v2/me/", 403)
	if _, err := c.Me(context.Background()); !errors.Is(err, ErrDenied) {
		t.Errorf("got %v, want denied", err)
	}
}

func TestRedirectsAreNotFollowed(t *testing.T) {
	srv := fakeawx.New("awx")
	defer srv.Close()
	c := newTestClient(t, srv, func(ctx *config.Context) { ctx.Flavor = "awx" })
	srv.Handle("/moved/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://other.example.com/", http.StatusFound)
	})
	err := c.get(context.Background(), "/api/v2/moved/", nil)
	var api *APIError
	if !errors.As(err, &api) || api.Status != http.StatusFound {
		t.Errorf("a redirect must come back as an error, not be followed: %v", err)
	}
}

func TestUntrustedCertificateIsRefused(t *testing.T) {
	srv := fakeawx.New("awx")
	defer srv.Close()
	c, err := NewClient(&config.Context{URL: srv.URL, APIToken: fakeawx.Token}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Detect(context.Background()); !errors.Is(err, ErrConnect) {
		t.Errorf("a self-signed certificate without ca_file or --insecure must fail: %v", err)
	}
}

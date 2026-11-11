package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Flavor is which product serves the controller API.
type Flavor string

const (
	FlavorAuto Flavor = ""    // detect
	FlavorAWX  Flavor = "awx" // AWX, and AAP before the 2.5 gateway: /api/v2
	FlavorAAP  Flavor = "aap" // AAP 2.5+ platform gateway: /api/controller/v2
)

// ParseFlavor reads the config value.
func ParseFlavor(s string) (Flavor, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "auto":
		return FlavorAuto, nil
	case "awx":
		return FlavorAWX, nil
	case "aap":
		return FlavorAAP, nil
	}
	return "", fmt.Errorf("unknown flavor %q: use awx, aap or auto", s)
}

func (f Flavor) apiPath() string {
	if f == FlavorAAP {
		return "/api/controller/v2"
	}
	return "/api/v2"
}

// Info is what the controller says about itself.
type Info struct {
	Flavor  Flavor
	APIPath string
	// GatewayPath is where the AAP platform gateway serves users, teams,
	// organizations, tokens and role assignments ("" on AWX).
	GatewayPath string
	Version     string // controller version from ping, e.g. "24.6.1" (AWX) or "4.6.x" (AAP)
}

type pingResponse struct {
	Version    string `json:"version"`
	ActiveNode string `json:"active_node"`
}

// Detect finds the flavor (unless the config pins one) and reads the controller
// version. It reads the API index first: the AAP gateway lists its services there
// with their paths, so a moved prefix is followed instead of guessed. If the index
// says nothing useful it asks the two known paths.
func (c *Client) Detect(ctx context.Context) (*Info, error) {
	if c.flavor == FlavorAuto {
		var index struct {
			APIs           map[string]string `json:"apis"`
			CurrentVersion string            `json:"current_version"`
		}
		err := c.get(ctx, "/api/", &index)
		switch {
		case err != nil && !errors.Is(err, ErrNotFound) && !isWrongPath(err):
			return nil, err // no connection, bad certificate: the same everywhere
		case err == nil && safePath(index.APIs["controller"]):
			c.flavor = FlavorAAP
			c.apiPath = strings.TrimRight(index.APIs["controller"], "/") + "/v2"
			if safePath(index.APIs["gateway"]) {
				c.gwPath = strings.TrimRight(index.APIs["gateway"], "/") + "/v1"
			}
		case err == nil && safePath(index.CurrentVersion):
			c.flavor = FlavorAWX
			c.apiPath = strings.TrimRight(index.CurrentVersion, "/")
		}
	}
	candidates := []Flavor{FlavorAAP, FlavorAWX}
	if c.flavor != FlavorAuto {
		candidates = []Flavor{c.flavor}
	}
	var firstErr error
	for _, f := range candidates {
		path := f.apiPath()
		if c.apiPath != "" {
			path = c.apiPath
		}
		var p pingResponse
		err := c.get(ctx, path+"/ping/", &p)
		if err == nil && p.Version != "" {
			c.flavor, c.apiPath = f, path
			if f == FlavorAAP && c.gwPath == "" {
				c.gwPath = "/api/gateway/v1"
			}
			return &Info{Flavor: f, APIPath: c.apiPath, GatewayPath: c.gwPath, Version: p.Version}, nil
		}
		if err == nil {
			err = fmt.Errorf("%w: %s/ping/ answered without a version (is the url a controller?)", ErrConnect, path)
		}
		// A missing path means "not this flavor": try the next one. Anything else
		// (no connection, bad certificate, 401) would fail the same way there.
		if !errors.Is(err, ErrNotFound) && len(candidates) > 1 && !isWrongPath(err) {
			return nil, err
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	if len(candidates) > 1 {
		return nil, fmt.Errorf("%w: neither /api/controller/v2/ping/ nor /api/v2/ping/ answered like a controller at %s", ErrConnect, c.BaseURL())
	}
	return nil, firstErr
}

// safePath accepts only a plain path on this server from an API index.
func safePath(p string) bool {
	return strings.HasPrefix(p, "/api/") && !strings.Contains(p, "//") && !strings.Contains(p, "..") && !strings.ContainsAny(p, "?#@:")
}

// isWrongPath reports an answer that means the path is not served here: a non-JSON
// page or a redirect (a gateway or proxy answering for an unknown path).
func isWrongPath(err error) bool {
	var api *APIError
	if errors.As(err, &api) {
		return api.Status >= 300 && api.Status < 400
	}
	return strings.Contains(err.Error(), "is not JSON")
}

// User is the account the credentials belong to.
type User struct {
	ID          int    `json:"id"`
	Username    string `json:"username"`
	IsSuperuser bool   `json:"is_superuser"`
	IsAuditor   bool   `json:"is_system_auditor"`
}

// Me returns the authenticated user. Detect must have run (or a flavor be pinned).
func (c *Client) Me(ctx context.Context) (*User, error) {
	if c.apiPath == "" {
		return nil, errors.New("internal: Me called before Detect")
	}
	var page struct {
		Results []User `json:"results"`
	}
	if err := c.get(ctx, c.apiPath+"/me/", &page); err != nil {
		return nil, err
	}
	if len(page.Results) == 0 {
		return nil, fmt.Errorf("%w: /me/ returned no user", ErrAuth)
	}
	return &page.Results[0], nil
}

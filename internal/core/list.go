package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// Object is one API object as the controller returned it.
type Object = map[string]any

// maxPageSize is the largest page the controller serves by default.
const maxPageSize = 200

// ready makes sure the API path is known, detecting the flavor on first use.
func (c *Client) ready(ctx context.Context) error {
	if c.apiPath != "" {
		return nil
	}
	_, err := c.Detect(ctx)
	return err
}

// APIPath is the detected API prefix ("" before the first request).
func (c *Client) APIPath() string { return c.apiPath }

// OnGateway reports whether identity and access live on the AAP gateway.
func (c *Client) OnGateway() bool { return c.gwPath != "" }

type controllerOnlyKey struct{}

// ControllerOnly makes requests under ctx go to the controller even for the
// kinds the gateway owns. Controller objects point at the controller's own IDs
// of organizations and users, which are not the gateway's IDs.
func ControllerOnly(ctx context.Context) context.Context {
	return context.WithValue(ctx, controllerOnlyKey{}, true)
}

// gatewayOwned are the collections that moved from the controller to the
// platform gateway in AAP 2.5: the controller's copies are deprecated there.
var gatewayOwned = []string{"/users/", "/teams/", "/organizations/", "/tokens/", "/role_definitions/", "/role_user_assignments/", "/role_team_assignments/"}

// abs turns a path under the API prefix into a server path. On AAP the kinds the
// gateway owns are read from and written to the gateway.
func (c *Client) abs(ctx context.Context, rel string) string {
	if c.gwPath != "" && ctx.Value(controllerOnlyKey{}) == nil && !strings.Contains(rel, "/instance_groups/") {
		for _, g := range gatewayOwned {
			if strings.HasPrefix(rel, g) {
				return c.gwPath + rel
			}
		}
	}
	return c.apiPath + rel
}

// List reads a collection (rel is relative to the API prefix, e.g. "/jobs/") and
// follows the pages until limit objects are read; limit <= 0 reads everything.
// total is the server's count of matching objects.
func (c *Client) List(ctx context.Context, rel string, q url.Values, limit int) (objs []Object, total int, err error) {
	if err := c.ready(ctx); err != nil {
		return nil, 0, err
	}
	if q == nil {
		q = url.Values{}
	}
	size := maxPageSize
	if limit > 0 && limit < size {
		size = limit
	}
	q.Set("page_size", strconv.Itoa(size))
	next := c.abs(ctx, rel) + "?" + q.Encode()
	for next != "" {
		var page struct {
			Count   int      `json:"count"`
			Next    *string  `json:"next"`
			Results []Object `json:"results"`
		}
		if err := c.get(ctx, next, &page); err != nil {
			return nil, 0, err
		}
		total = page.Count
		objs = append(objs, page.Results...)
		if limit > 0 && len(objs) >= limit {
			return objs[:limit], total, nil
		}
		next = ""
		if page.Next != nil {
			// "next" is a server path that already carries the API prefix. Only a
			// path on this server is followed, never another host.
			if n := *page.Next; strings.HasPrefix(n, "/") && !strings.HasPrefix(n, "//") {
				next = n
			} else if u, err := url.Parse(n); err == nil && u.Host == c.base.Host {
				next = u.RequestURI()
			}
		}
	}
	return objs, total, nil
}

// Get reads one object (rel like "/jobs/12/").
func (c *Client) Get(ctx context.Context, rel string) (Object, error) {
	if err := c.ready(ctx); err != nil {
		return nil, err
	}
	var o Object
	if err := c.get(ctx, c.abs(ctx, rel), &o); err != nil {
		return nil, err
	}
	return o, nil
}

// GetText reads a plain-text answer (job output).
func (c *Client) GetText(ctx context.Context, rel string) (string, error) {
	if err := c.ready(ctx); err != nil {
		return "", err
	}
	var raw rawText
	if err := c.get(ctx, c.abs(ctx, rel), &raw); err != nil {
		return "", err
	}
	return string(raw), nil
}

// rawText takes the body as it is instead of decoding JSON.
type rawText string

// Send makes a changing request (POST, PATCH, PUT, DELETE). It is never retried.
func (c *Client) Send(ctx context.Context, method, rel string, body any) (Object, error) {
	if err := c.ready(ctx); err != nil {
		return nil, err
	}
	var raw json.RawMessage
	if err := c.do(ctx, method, c.abs(ctx, rel), body, &raw); err != nil {
		return nil, err
	}
	var o Object
	if len(raw) > 0 {
		_ = decodeJSON(raw, &o) // some endpoints answer with a list or nothing
	}
	return o, nil
}

// AmbiguousError means a name matched more than one object.
type AmbiguousError struct {
	Kind    string
	Name    string
	Matches []string
}

func (e *AmbiguousError) Error() string {
	return fmt.Sprintf("%s %q is ambiguous, it matches %s: use id:<n>", e.Kind, e.Name, strings.Join(e.Matches, ", "))
}

// NotFoundError names what was looked for.
type NotFoundError struct{ Kind, Name string }

func (e *NotFoundError) Error() string { return fmt.Sprintf("%s %q not found", e.Kind, e.Name) }
func (e *NotFoundError) Unwrap() error { return ErrNotFound }

// Resolve finds one object of a kind by "id:<n>", "name:<text>", a number (tried as
// an ID first, then as a name) or a name. extra narrows the search (for example the
// inventory of a host).
func (c *Client) Resolve(ctx context.Context, r *Resource, ref string, extra url.Values) (Object, error) {
	byID := func(id string) (Object, error) {
		o, err := c.Get(ctx, r.Path+id+"/")
		if errors.Is(err, ErrNotFound) {
			return nil, &NotFoundError{r.Singular, "id:" + id}
		}
		return o, err
	}
	name := ref
	switch {
	case strings.HasPrefix(ref, "id:"):
		id := strings.TrimPrefix(ref, "id:")
		if !isDigits(id) {
			return nil, usagef("%q is not a numeric ID", ref)
		}
		return byID(id)
	case strings.HasPrefix(ref, "name:"):
		name = strings.TrimPrefix(ref, "name:")
	case isDigits(ref):
		o, err := byID(ref)
		if err == nil || !errors.Is(err, ErrNotFound) || r.NameField == "" {
			return o, err
		}
	}
	if r.NameField == "" {
		return nil, usagef("a %s is addressed by its numeric ID, not %q", r.Singular, ref)
	}
	q := url.Values{}
	for k, v := range extra {
		q[k] = v
	}
	q.Set(r.NameField, name)
	objs, _, err := c.List(ctx, r.Path, q, 20)
	if err != nil {
		return nil, err
	}
	switch len(objs) {
	case 0:
		return nil, &NotFoundError{r.Singular, name}
	case 1:
		return objs[0], nil
	}
	amb := &AmbiguousError{Kind: r.Singular, Name: name}
	for _, o := range objs {
		m := "id:" + idOf(o)
		for _, parent := range []string{"inventory", "organization"} {
			if n := Lookup(o, "summary_fields."+parent+".name"); n != nil {
				m += fmt.Sprintf(" (%s %v)", parent, n)
				break
			}
		}
		amb.Matches = append(amb.Matches, m)
	}
	return nil, amb
}

// ResolveAll is Resolve for reading about a name that several objects share: the
// same host name in more than one inventory. It returns every exact match.
func (c *Client) ResolveAll(ctx context.Context, r *Resource, ref string, extra url.Values) ([]Object, error) {
	o, err := c.Resolve(ctx, r, ref, extra)
	if err == nil {
		return []Object{o}, nil
	}
	var amb *AmbiguousError
	if !errors.As(err, &amb) {
		return nil, err
	}
	q := url.Values{}
	for k, v := range extra {
		q[k] = v
	}
	q.Set(r.NameField, strings.TrimPrefix(ref, "name:"))
	objs, _, err := c.List(ctx, r.Path, q, 50)
	return objs, err
}

// ErrUsage marks a request that cannot be right whatever the server says.
var ErrUsage = errors.New("usage")

type usageError struct{ msg string }

func (e *usageError) Error() string { return e.msg }
func (e *usageError) Unwrap() error { return ErrUsage }

func usagef(format string, args ...any) error {
	return &usageError{fmt.Sprintf(format, args...)}
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// idOf returns an object's ID as text.
func idOf(o Object) string { return fmt.Sprint(asInt(o["id"])) }

// ID returns an object's numeric ID.
func ID(o Object) int { return asInt(o["id"]) }

// asFloat reads a number however it was decoded.
func asFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}

func asInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	case string:
		i, _ := strconv.Atoi(n)
		return i
	}
	return 0
}

// Lookup reads a dotted path ("summary_fields.inventory.name") from an object.
func Lookup(o Object, path string) any {
	var cur any = o
	for _, part := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[part]
	}
	return cur
}

// GetAny reads a path whose answer is not an object (a list of file names).
func (c *Client) GetAny(ctx context.Context, rel string, out any) error {
	if err := c.ready(ctx); err != nil {
		return err
	}
	return c.get(ctx, c.abs(ctx, rel), out)
}

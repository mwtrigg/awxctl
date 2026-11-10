// Package core talks to the AWX / AAP controller API and returns data. It never
// prints: the CLI, the MCP server and the TUI all sit on top of it.
package core

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/mwtrigg/awxctl/internal/config"
)

// Errors a caller can test for with errors.Is.
var (
	ErrConnect  = errors.New("cannot reach the controller")
	ErrAuth     = errors.New("authentication failed")
	ErrNotFound = errors.New("not found")
	ErrDenied   = errors.New("permission denied by the controller")
)

// APIError is an error response from the controller. Fields holds the per-field
// validation messages of a 400, Detail the general message.
type APIError struct {
	Status int
	Method string
	Path   string
	Detail string
	Fields map[string][]string
	kind   error
}

func (e *APIError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s: HTTP %d", e.Method, e.Path, e.Status)
	if e.Detail != "" {
		b.WriteString(": " + e.Detail)
	}
	keys := make([]string, 0, len(e.Fields))
	for k := range e.Fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, "; %s: %s", k, strings.Join(e.Fields[k], " "))
	}
	return b.String()
}

// Unwrap lets errors.Is match ErrAuth, ErrNotFound and ErrDenied.
func (e *APIError) Unwrap() error { return e.kind }

// OutcomeUnknownError means a write was sent but no definite answer came back
// (timeout, dropped connection, 502/504): it may or may not have happened.
type OutcomeUnknownError struct {
	Method string
	Path   string
	Cause  error
}

func (e *OutcomeUnknownError) Error() string {
	return fmt.Sprintf("%s %s: outcome unknown (%v): check before retrying", e.Method, e.Path, e.Cause)
}

func (e *OutcomeUnknownError) Unwrap() error { return e.Cause }

// Options are the per-run settings that do not come from the config file.
type Options struct {
	Timeout time.Duration // per request; 0 means 180s
	Retries int           // retries of read-only calls; writes are never retried
}

// Client is a connection to one controller.
type Client struct {
	base    *url.URL
	http    *http.Client
	token   string
	user    string
	pass    string
	retries int

	flavor  Flavor // pinned by config, or set by Detect
	apiPath string // "/api/v2" or "/api/controller/v2"; empty until Detect

	// backoff is the wait before retry n (1-based); tests shorten it.
	backoff func(n int) time.Duration
}

// NewClient builds a client from a resolved context. It makes no request.
func NewClient(ctx *config.Context, opts Options) (*Client, error) {
	u, err := url.Parse(strings.TrimRight(ctx.URL, "/"))
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, fmt.Errorf("invalid url %q: want https://host[:port]", ctx.URL)
	}
	flavor, err := ParseFlavor(ctx.Flavor)
	if err != nil {
		return nil, err
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	// A host that does not answer fails in seconds, not after the request timeout.
	tr.DialContext = (&net.Dialer{Timeout: 10 * time.Second}).DialContext
	tr.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	if ctx.TLSVerify != nil && !*ctx.TLSVerify {
		tr.TLSClientConfig.InsecureSkipVerify = true
	}
	if ctx.CAFile != "" {
		pem, err := os.ReadFile(ctx.CAFile)
		if err != nil {
			return nil, fmt.Errorf("reading ca_file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("ca_file %s holds no PEM certificate", ctx.CAFile)
		}
		tr.TLSClientConfig.RootCAs = pool
	}
	if ctx.Proxy != "" {
		pu, err := url.Parse(ctx.Proxy)
		if err != nil {
			return nil, fmt.Errorf("invalid proxy %q: %w", ctx.Proxy, err)
		}
		tr.Proxy = http.ProxyURL(pu)
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 180 * time.Second
	}
	c := &Client{
		base: u,
		// A redirect is never followed: it could carry the token to another host.
		http: &http.Client{Transport: tr, Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}},
		token:   ctx.APIToken,
		user:    ctx.Username,
		pass:    ctx.Password,
		retries: max(opts.Retries, 0),
		flavor:  flavor,
		backoff: func(n int) time.Duration { return time.Duration(n) * 500 * time.Millisecond },
	}
	if flavor != FlavorAuto {
		c.apiPath = flavor.apiPath()
	}
	return c, nil
}

// BaseURL is the controller URL without a trailing slash.
func (c *Client) BaseURL() string { return c.base.String() }

// get fetches an absolute server path (starting with "/") and decodes the JSON
// answer into out. Reads are retried on connection errors and 502/503/504.
func (c *Client) get(ctx context.Context, path string, out any) error {
	return c.do(ctx, http.MethodGet, path, nil, out)
}

// do sends one request. body, when not nil, is sent as JSON. Only GET is retried.
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return err
		}
	}
	read := method == http.MethodGet
	attempts := 1
	if read {
		attempts += c.retries
	}
	var last error
	for n := 1; n <= attempts; n++ {
		if n > 1 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(c.backoff(n - 1)):
			}
		}
		retry, err := c.once(ctx, method, path, payload, out)
		if err == nil {
			return nil
		}
		last = err
		if !retry {
			break
		}
	}
	if !read {
		var api *APIError
		if !errors.As(last, &api) || api.Status == http.StatusBadGateway || api.Status == http.StatusGatewayTimeout {
			return &OutcomeUnknownError{Method: method, Path: path, Cause: last}
		}
	}
	return last
}

// once sends the request one time. retry reports whether a read may be tried again.
func (c *Client) once(ctx context.Context, method, path string, payload []byte, out any) (retry bool, err error) {
	var rd io.Reader
	if payload != nil {
		rd = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base.String()+path, rd)
	if err != nil {
		return false, err
	}
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	switch {
	case c.token != "":
		req.Header.Set("Authorization", "Bearer "+c.token)
	case c.user != "":
		req.SetBasicAuth(c.user, c.pass)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		return true, fmt.Errorf("%w: %s %s: %v", ErrConnect, method, path, scrub(err))
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return true, fmt.Errorf("%w: %s %s: reading the answer: %v", ErrConnect, method, path, err)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if out == nil || len(data) == 0 {
			return false, nil
		}
		if err := json.Unmarshal(data, out); err != nil {
			return false, fmt.Errorf("%w: %s %s: the answer is not JSON (is the url right?)", ErrConnect, method, path)
		}
		return false, nil
	}
	e := &APIError{Status: resp.StatusCode, Method: method, Path: path}
	parseErrorBody(data, e)
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		e.kind = ErrAuth
	case http.StatusForbidden:
		e.kind = ErrDenied
	case http.StatusNotFound:
		e.kind = ErrNotFound
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		e.kind = ErrConnect
		return true, e
	}
	return false, e
}

// parseErrorBody reads the controller's error JSON: {"detail": "..."} or
// {"field": ["message", ...], ...}. Anything else (an HTML error page) is ignored.
func parseErrorBody(data []byte, e *APIError) {
	var obj map[string]json.RawMessage
	if json.Unmarshal(data, &obj) != nil {
		return
	}
	for k, raw := range obj {
		var s string
		var list []string
		switch {
		case json.Unmarshal(raw, &s) == nil:
			list = []string{s}
		case json.Unmarshal(raw, &list) == nil:
		default:
			continue
		}
		if k == "detail" {
			e.Detail = strings.Join(list, " ")
			continue
		}
		if e.Fields == nil {
			e.Fields = map[string][]string{}
		}
		e.Fields[k] = list
	}
}

// scrub drops the *url.Error wrapper, whose text repeats the full URL.
func scrub(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

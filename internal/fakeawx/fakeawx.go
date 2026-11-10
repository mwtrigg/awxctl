// Package fakeawx is a fake controller for tests. It answers like AWX (/api/v2) or
// like the AAP gateway (/api/controller/v2), records every request, checks the
// token, and can be told to fail.
package fakeawx

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
)

// Token is the bearer token the fake accepts.
const Token = "fake-token"

// Request is one recorded request.
type Request struct {
	Method string
	Path   string // with query
	Auth   string // Authorization header
	Body   string
}

// Server is a running fake controller.
type Server struct {
	*httptest.Server
	Prefix  string // "/api/v2" or "/api/controller/v2"
	Version string

	mu       sync.Mutex
	requests []Request
	fail     map[string][]int // path -> statuses to answer with first, one per request
	handlers map[string]http.HandlerFunc
}

// New starts a fake of the given flavor: "awx" or "aap".
func New(flavor string) *Server {
	s := &Server{Prefix: "/api/v2", Version: "24.6.1", fail: map[string][]int{}, handlers: map[string]http.HandlerFunc{}}
	if flavor == "aap" {
		s.Prefix, s.Version = "/api/controller/v2", "4.6.2"
	}
	s.Server = httptest.NewTLSServer(http.HandlerFunc(s.serve))
	return s
}

// FailNext makes the next requests for path (without query) answer with these
// statuses, in order, before normal service resumes.
func (s *Server) FailNext(path string, statuses ...int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fail[path] = append(s.fail[path], statuses...)
}

// Handle serves path (relative to the API prefix, e.g. "/jobs/") with h.
func (s *Server) Handle(path string, h http.HandlerFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handlers[s.Prefix+path] = h
}

// Requests returns a copy of everything received so far.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.requests...)
}

// JSON writes v with the status.
func JSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	s.requests = append(s.requests, Request{Method: r.Method, Path: r.URL.RequestURI(), Auth: r.Header.Get("Authorization"), Body: string(body)})
	var forced int
	if q := s.fail[r.URL.Path]; len(q) > 0 {
		forced, s.fail[r.URL.Path] = q[0], q[1:]
	}
	h := s.handlers[r.URL.Path]
	s.mu.Unlock()

	if forced != 0 {
		w.WriteHeader(forced)
		return
	}
	if !strings.HasPrefix(r.URL.Path, s.Prefix+"/") {
		// The other flavor's path: a real server answers 404 with an HTML page.
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, "<html><body>Not Found</body></html>")
		return
	}
	if r.URL.Path == s.Prefix+"/ping/" { // ping needs no login
		JSON(w, http.StatusOK, map[string]any{"version": s.Version, "active_node": "node1", "ha": false})
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+Token {
		JSON(w, http.StatusUnauthorized, map[string]any{"detail": "Authentication credentials were not provided."})
		return
	}
	if h != nil {
		h(w, r)
		return
	}
	if r.URL.Path == s.Prefix+"/me/" {
		JSON(w, http.StatusOK, map[string]any{"count": 1, "results": []map[string]any{
			{"id": 1, "username": "admin", "is_superuser": true, "is_system_auditor": false},
		}})
		return
	}
	JSON(w, http.StatusNotFound, map[string]any{"detail": "Not found."})
}

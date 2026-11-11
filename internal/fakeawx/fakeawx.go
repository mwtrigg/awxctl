// Package fakeawx is a fake controller for tests. It answers like AWX (/api/v2) or
// like the AAP gateway (/api/controller/v2), records every request, checks the
// token, and can be told to fail.
package fakeawx

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
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
	NoIndex bool   // true: /api/ is not served, as behind some proxies
	Version string

	mu       sync.Mutex
	store    map[string][]map[string]any // collection path under the prefix -> objects
	nextID   int
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
	if r.URL.Path == "/api/" && !s.NoIndex {
		if s.Prefix == "/api/v2" {
			JSON(w, http.StatusOK, map[string]any{"description": "AWX REST API", "current_version": "/api/v2/", "available_versions": map[string]any{"v2": "/api/v2/"}})
		} else {
			JSON(w, http.StatusOK, map[string]any{"description": "AAP gateway REST API", "apis": map[string]any{"gateway": "/api/gateway/", "controller": "/api/controller/"}})
		}
		return
	}
	if gw := "/api/gateway/v1"; s.Prefix != "/api/v2" && strings.HasPrefix(r.URL.Path, gw+"/") {
		// the gateway half of an AAP: its own collections, stored under "gw:"
		if r.Header.Get("Authorization") != "Bearer "+Token {
			JSON(w, http.StatusUnauthorized, map[string]any{"detail": "Authentication credentials were not provided."})
			return
		}
		if !s.serveStoreAt(w, r, body, gw, "gw:") {
			JSON(w, http.StatusNotFound, map[string]any{"detail": "Not found."})
		}
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
	if s.serveStore(w, r, body) {
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

// Add puts an object into a collection ("/hosts/") and returns it with its ID. A
// collection that has objects is served generically: list with exact-match
// filters and paging, get, create, patch and delete.
func (s *Server) Add(collection string, obj map[string]any) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.store == nil {
		s.store = map[string][]map[string]any{}
	}
	if obj["id"] == nil {
		s.nextID++
		obj["id"] = s.nextID
	} else if id, _ := obj["id"].(int); id > s.nextID {
		s.nextID = id
	}
	s.store[collection] = append(s.store[collection], obj)
	return obj
}

// Collection returns a copy of the objects of a collection.
func (s *Server) Collection(collection string) []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]map[string]any(nil), s.store[collection]...)
}

func (s *Server) serveStore(w http.ResponseWriter, r *http.Request, body []byte) bool {
	return s.serveStoreAt(w, r, body, s.Prefix, "")
}

// serveStoreAt serves the collections stored under keyPrefix at the URL prefix.
// Gateway collections are added as "gw:/users/".
func (s *Server) serveStoreAt(w http.ResponseWriter, r *http.Request, body []byte, urlPrefix, keyPrefix string) bool {
	rel := keyPrefix + strings.TrimPrefix(r.URL.Path, urlPrefix)
	s.mu.Lock()
	defer s.mu.Unlock()
	if objs, ok := s.store[rel]; ok {
		switch r.Method {
		case http.MethodGet:
			var match []map[string]any
			for _, o := range objs {
				keep := true
				for k, vals := range r.URL.Query() {
					if k == "page" || k == "page_size" || k == "order_by" || strings.Contains(k, "__") { // lookups are not modelled
						continue
					}
					if fmt.Sprint(o[k]) != vals[0] {
						keep = false
					}
				}
				if keep {
					match = append(match, o)
				}
			}
			sort.SliceStable(match, func(i, j int) bool { return match[i]["id"].(int) < match[j]["id"].(int) })
			size, _ := strconv.Atoi(r.URL.Query().Get("page_size"))
			if size <= 0 {
				size = 25
			}
			page, _ := strconv.Atoi(r.URL.Query().Get("page"))
			if page <= 0 {
				page = 1
			}
			start, end := min((page-1)*size, len(match)), min(page*size, len(match))
			var next any
			if end < len(match) {
				q := r.URL.Query()
				q.Set("page", strconv.Itoa(page+1))
				next = r.URL.Path + "?" + q.Encode()
			}
			results := match[start:end]
			if results == nil {
				results = []map[string]any{}
			}
			JSON(w, http.StatusOK, map[string]any{"count": len(match), "next": next, "results": results})
		case http.MethodPost:
			var obj map[string]any
			if json.Unmarshal(body, &obj) != nil {
				JSON(w, http.StatusBadRequest, map[string]any{"detail": "bad JSON"})
				return true
			}
			s.nextID++
			obj["id"] = s.nextID
			s.store[rel] = append(s.store[rel], obj)
			JSON(w, http.StatusCreated, obj)
		default:
			JSON(w, http.StatusMethodNotAllowed, map[string]any{"detail": "Method not allowed."})
		}
		return true
	}
	// an item: /collection/<id>/
	trim := strings.TrimSuffix(rel, "/")
	cut := strings.LastIndex(trim, "/")
	if cut < 0 {
		return false
	}
	coll, idText := trim[:cut+1], trim[cut+1:]
	id, err := strconv.Atoi(idText)
	objs, ok := s.store[coll]
	if err != nil || !ok {
		return false
	}
	for i, o := range objs {
		if o["id"] != id {
			continue
		}
		switch r.Method {
		case http.MethodGet:
			JSON(w, http.StatusOK, o)
		case http.MethodPatch, http.MethodPut:
			var change map[string]any
			_ = json.Unmarshal(body, &change)
			for k, v := range change {
				o[k] = v
			}
			JSON(w, http.StatusOK, o)
		case http.MethodDelete:
			s.store[coll] = append(objs[:i:i], objs[i+1:]...)
			w.WriteHeader(http.StatusNoContent)
		default:
			JSON(w, http.StatusMethodNotAllowed, map[string]any{"detail": "Method not allowed."})
		}
		return true
	}
	JSON(w, http.StatusNotFound, map[string]any{"detail": "Not found."})
	return true
}

// Package fakeawx is a fake controller for tests. It answers like AWX (/api/v2) or
// like the AAP gateway (/api/controller/v2), records every request, checks the
// token, and can be told to fail.
package fakeawx

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
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
	URL string // "https://127.0.0.1:port" (or http for a plain one)

	http *http.Server
	cert *x509.Certificate

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

// New starts a fake of the given flavor ("awx" or "aap") over TLS on a free
// local port, with a certificate made for the occasion (see Certificate).
func New(flavor string) *Server {
	s := newServer(flavor)
	cert, parsed, err := selfSigned()
	if err != nil {
		panic("fakeawx: " + err.Error())
	}
	l, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
	if err != nil {
		panic("fakeawx: " + err.Error())
	}
	s.cert = parsed
	s.start(l, "https")
	return s
}

// NewLocal starts a fake that speaks plain HTTP on addr ("127.0.0.1:8043"), for
// trying awxctl by hand without a controller (awxctl demo).
func NewLocal(flavor, addr string) (*Server, error) {
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	s := newServer(flavor)
	s.start(l, "http")
	return s, nil
}

func newServer(flavor string) *Server {
	s := &Server{Prefix: "/api/v2", Version: "24.6.1", fail: map[string][]int{}, handlers: map[string]http.HandlerFunc{}}
	if flavor == "aap" {
		s.Prefix, s.Version = "/api/controller/v2", "4.6.2"
	}
	return s
}

func (s *Server) start(l net.Listener, scheme string) {
	s.URL = scheme + "://" + l.Addr().String()
	// ErrorLog is quiet: a client that refuses the certificate is part of some tests.
	s.http = &http.Server{Handler: http.HandlerFunc(s.serve), ReadHeaderTimeout: 10 * time.Second, ErrorLog: log.New(io.Discard, "", 0)}
	go func() { _ = s.http.Serve(l) }()
}

// Close stops the server and drops its connections.
func (s *Server) Close() { _ = s.http.Close() }

// Certificate is the certificate a TLS fake presents (nil for a plain one), for
// a client that should trust it.
func (s *Server) Certificate() *x509.Certificate { return s.cert }

// selfSigned makes a certificate for 127.0.0.1 and localhost that is valid for a day.
func selfSigned() (tls.Certificate, *x509.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: "awxctl fake controller"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true, IsCA: true,
		IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback}, DNSNames: []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, parsed, nil
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
			if strings.HasPrefix(r.URL.Query().Get("order_by"), "-") { // newest first, as lists of runs ask
				for i, j := 0, len(match)-1; i < j; i, j = i+1, j-1 {
					match[i], match[j] = match[j], match[i]
				}
			}
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

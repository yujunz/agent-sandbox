// Copyright 2026 The Kubernetes Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package portal

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"
)

// xterm.js creates runtime style elements and style attributes; scripts remain restricted to self-hosted assets.
const contentSecurityPolicy = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; connect-src 'self'; img-src 'self' data:; object-src 'none'; base-uri 'none'; frame-ancestors 'none'"

const invalidHostMessage = "Host is not allowed"

//go:embed web
var embeddedWeb embed.FS

// ServerOptions configures the portal HTTP handler.
type ServerOptions struct {
	Inventory *Inventory
	Terminal  http.Handler
	Log       logr.Logger
}

type server struct {
	inventory *Inventory
	log       logr.Logger
}

// NewServer creates the portal's private HTTP routing surface.
func NewServer(opts ServerOptions) (http.Handler, error) {
	if opts.Inventory == nil {
		return nil, errors.New("inventory is required")
	}
	webRoot, err := fs.Sub(embeddedWeb, "web")
	if err != nil {
		return nil, fmt.Errorf("open embedded portal assets: %w", err)
	}

	s := &server{inventory: opts.Inventory, log: opts.Log}
	mux := http.NewServeMux()
	mux.Handle("/healthz", onlyMethod(http.MethodGet, http.HandlerFunc(healthHandler)))
	if opts.Terminal != nil {
		mux.Handle(
			"/api/v1/namespaces/{namespace}/sandboxes/{sandbox}/terminal",
			apiMethod(http.MethodGet, opts.Terminal),
		)
	}
	mux.Handle("/api/v1/sandboxes", apiMethod(http.MethodGet, http.HandlerFunc(s.inventoryHandler)))
	mux.HandleFunc("/api/v1", apiNotFound)
	mux.HandleFunc("/api/v1/", apiNotFound)
	mux.HandleFunc("/api/", apiNotFound)
	mux.HandleFunc("/api", apiNotFound)
	mux.Handle("/", exactStaticFiles(webRoot))
	return securityHeaders(mux), nil
}

func healthHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func (s *server) inventoryHandler(w http.ResponseWriter, r *http.Request) {
	response, err := s.inventory.List(r.Context())
	if err != nil {
		s.log.Error(errors.New("inventory unavailable"), "list Sandbox inventory")
		writeAPIError(w, http.StatusServiceUnavailable, "Sandbox inventory is temporarily unavailable")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(response); err != nil {
		s.log.Error(errors.New("encode inventory response"), "write Sandbox inventory response")
	}
}

func writeAPIError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}

func apiNotFound(w http.ResponseWriter, _ *http.Request) {
	writeAPIError(w, http.StatusNotFound, "API endpoint not found")
}

func apiMethod(method string, next http.Handler) http.Handler {
	return onlyMethod(method, next)
}

func onlyMethod(method string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == method {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Allow", method)
		writeAPIError(w, http.StatusMethodNotAllowed, "Method not allowed")
	})
}

func exactStaticFiles(root fs.FS) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		name := strings.TrimPrefix(r.URL.Path, "/")
		if name == "" {
			name = "index.html"
		}
		if name != path.Clean(name) || strings.HasPrefix(name, ".") {
			http.NotFound(w, r)
			return
		}
		info, err := fs.Stat(root, name)
		if err != nil || info.IsDir() {
			http.NotFound(w, r)
			return
		}
		contents, err := fs.ReadFile(root, name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(contents))
	})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", contentSecurityPolicy)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Frame-Options", "DENY")
		if r.URL.Path == "/api" || strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Cache-Control", "no-store")
		}
		if !loopbackAuthority(r.Host) {
			if r.URL.Path == "/api" || strings.HasPrefix(r.URL.Path, "/api/") {
				writeAPIError(w, http.StatusForbidden, invalidHostMessage)
			} else {
				http.Error(w, invalidHostMessage, http.StatusForbidden)
			}
			return
		}
		next.ServeHTTP(w, r)
	})
}

func loopbackAuthority(authority string) bool {
	if authority == "" || strings.ContainsAny(authority, "/?#@") {
		return false
	}

	hostname := authority
	if strings.HasPrefix(authority, "[") {
		closingBracket := strings.IndexByte(authority, ']')
		if closingBracket < 2 {
			return false
		}
		hostname = authority[1:closingBracket]
		suffix := authority[closingBracket+1:]
		if suffix != "" && (!strings.HasPrefix(suffix, ":") || !validAuthorityPort(suffix[1:])) {
			return false
		}
	} else {
		switch strings.Count(authority, ":") {
		case 0:
		case 1:
			var port string
			var err error
			hostname, port, err = net.SplitHostPort(authority)
			if err != nil || !validAuthorityPort(port) {
				return false
			}
		default:
			return false
		}
	}

	return loopbackHostname(hostname)
}

func validAuthorityPort(port string) bool {
	if port == "" {
		return false
	}
	_, err := strconv.ParseUint(port, 10, 16)
	return err == nil
}

func loopbackHostname(hostname string) bool {
	if strings.EqualFold(hostname, "localhost") {
		return true
	}
	ip := net.ParseIP(hostname)
	return ip != nil && ip.IsLoopback()
}

func websocketResponseHeaders(header http.Header) http.Header {
	responseHeader := make(http.Header, 5)
	for _, name := range [...]string{
		"Content-Security-Policy",
		"X-Content-Type-Options",
		"Referrer-Policy",
		"X-Frame-Options",
		"Cache-Control",
	} {
		if values := header.Values(name); len(values) != 0 {
			responseHeader[name] = append([]string(nil), values...)
		}
	}
	return responseHeader
}

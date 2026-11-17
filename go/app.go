// app.go — the forge's HTTP server: one process serving git smart HTTP,
// REST v3, GraphQL and MCP over the in-memory store. Only the Go standard
// library is used. No wall clock appears in any response.

package main

import (
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
)

const jsonType = "application/json; charset=utf-8"
const maxBodyBytes = 64 * 1024 * 1024

var requestCounter uint64

func nextRequestID() string {
	n := atomic.AddUint64(&requestCounter, 1)
	return "req-" + strconv.FormatUint(n, 10)
}

type appHandler struct {
	store         *Store
	expectedToken string
}

func (h *appHandler) sendJSON(w http.ResponseWriter, status int, body []byte, headers map[string]string) {
	// body is the JSON payload WITHOUT the trailing newline; Node's
	// sendJson appends one.
	if status == 204 {
		for k, v := range headers {
			w.Header().Set(k, v)
		}
		w.Header().Set("x-github-request-id", nextRequestID())
		w.WriteHeader(204)
		return
	}
	text := append(append([]byte(nil), body...), '\n')
	for k, v := range headers {
		w.Header().Set(k, v)
	}
	w.Header().Set("content-type", jsonType)
	w.Header().Set("content-length", strconv.Itoa(len(text)))
	w.Header().Set("x-github-request-id", nextRequestID())
	w.WriteHeader(status)
	_, _ = w.Write(text)
}

func (h *appHandler) sendRawJSON(w http.ResponseWriter, status int, body []byte, headers map[string]string) {
	// body already includes its trailing newline (prebuilt cache bytes).
	for k, v := range headers {
		w.Header().Set(k, v)
	}
	w.Header().Set("content-type", jsonType)
	w.Header().Set("content-length", strconv.Itoa(len(body)))
	w.Header().Set("x-github-request-id", nextRequestID())
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func (h *appHandler) sendRaw(w http.ResponseWriter, status int, contentType string, body []byte) {
	w.Header().Set("content-type", contentType)
	w.Header().Set("content-length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func (h *appHandler) sendError(w http.ResponseWriter, status int, message string) {
	h.sendJSON(w, status, marshalJSONNoNL(map[string]string{"message": message}), nil)
}

// splitURL mirrors Node's splitUrl: a fast raw slice when the path has no
// percent-escapes, no '#', and no dot segments; otherwise a full URL parse.
func splitURL(raw string) (string, url.Values) {
	q := strings.Index(raw, "?")
	path := raw
	var qs string
	if q >= 0 {
		path = raw[:q]
		qs = raw[q+1:]
	}
	if !strings.Contains(path, "%") && !strings.Contains(path, "#") && !strings.Contains(path, "/./") && !strings.Contains(path, "/../") {
		if q >= 0 {
			vals, _ := url.ParseQuery(qs)
			return path, vals
		}
		return path, url.Values{}
	}
	u, err := url.Parse("http://entry" + raw)
	if err != nil {
		return "/", url.Values{}
	}
	p := u.Path
	if strings.Contains(path, "/./") || strings.Contains(path, "/../") {
		p = cleanPath(p)
	}
	return p, u.Query()
}

func cleanPath(p string) string {
	// Resolve dot segments the way WHATWG URL does (keep a trailing slash
	// when the original had one, unlike path.Clean).
	trailing := strings.HasSuffix(p, "/")
	parts := strings.Split(p, "/")
	var out []string
	for _, seg := range parts {
		switch seg {
		case ".", "":
			// "." drops; empty segments are kept only as the leading one.
			if len(out) == 0 {
				out = append(out, seg)
			}
		case "..":
			if len(out) > 1 {
				out = out[:len(out)-1]
			}
		default:
			out = append(out, seg)
		}
	}
	s := strings.Join(out, "/")
	if trailing && !strings.HasSuffix(s, "/") {
		s += "/"
	}
	if s == "" {
		s = "/"
	}
	return s
}

var gitRouteRegexp = regexp.MustCompile(`^/([^/]+)/([^/]+)\.git/(info/refs|git-upload-pack|git-receive-pack)$`)

func (h *appHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	method := r.Method
	path, query := splitURL(r.URL.RequestURI())

	// The env's boot check calls this twice: 200 {"ok":true}, no auth.
	if method == "GET" && path == "/healthz" {
		h.sendJSON(w, 200, marshalJSONNoNL(map[string]bool{"ok": true}), nil)
		return
	}

	// Everything else needs the shop token when one is configured.
	if h.expectedToken != "" {
		if r.Header.Get("Authorization") != "Bearer "+h.expectedToken {
			h.sendError(w, 401, "Bad credentials")
			return
		}
	}

	needsBody := method == "POST" || method == "PUT" || method == "PATCH" || method == "DELETE"
	var body []byte
	if needsBody {
		limited := io.LimitReader(r.Body, maxBodyBytes+1)
		data, err := io.ReadAll(limited)
		if err != nil {
			h.sendError(w, 500, err.Error())
			return
		}
		if len(data) > maxBodyBytes {
			h.sendError(w, 500, "body too large")
			return
		}
		body = data
	} else {
		body = []byte{}
	}

	// The store is single-writer: hold the lock for the whole dispatch.
	// (Body reading happens above, outside the lock, so network I/O never
	// blocks another request.)
	h.store.Lock()
	defer h.store.Unlock()

	defer func() {
		// Node's catch: 500 with the error message when headers are not
		// sent yet. (Go panics are the only way to reach this.)
		if rec := recover(); rec != nil {
			msg := "internal error"
			if err, ok := rec.(error); ok {
				msg = err.Error()
			} else if s, ok := rec.(string); ok {
				msg = s
			}
			// The header may already be sent; WriteHeader would be a no-op.
			h.sendError(w, 500, msg)
		}
	}()

	// MCP: POST /mcp
	if method == "POST" && path == "/mcp" {
		out := mcpRoute(h.store, string(body))
		if out.status == 202 {
			w.WriteHeader(202)
			return
		}
		h.sendJSON(w, out.status, marshalOMap(out.json), nil)
		return
	}

	// GraphQL: POST /api/graphql
	if method == "POST" && path == "/api/graphql" {
		status, out := graphqlRoute(h.store, string(body))
		// graphqlRoute returns bytes without a trailing newline; sendJson
		// adds one.
		h.sendJSON(w, status, out, nil)
		return
	}

	// REST v3: /api/v3[/...]
	if path == "/api/v3" || strings.HasPrefix(path, "/api/v3/") {
		restPath := ""
		if path != "/api/v3" {
			restPath = path[len("/api/v3/"):]
		}
		var segments []string
		if restPath != "" {
			for _, s := range strings.Split(restPath, "/") {
				if s != "" {
					segments = append(segments, s)
				}
			}
		}
		host := r.Host
		if host == "" {
			host = "127.0.0.1"
		}
		ctx := &RestCtx{Method: method, Segments: segments, Query: query, BodyText: string(body), Host: host}
		out := RestRoute(h.store, ctx)
		if out.Raw != nil {
			h.sendRawJSON(w, out.Status, out.Raw, out.Headers)
			return
		}
		if out.Status == 204 {
			h.sendJSON(w, 204, nil, out.Headers)
			return
		}
		h.sendJSON(w, out.Status, marshalJSONNoNL(out.JSON), out.Headers)
		return
	}

	// Git smart HTTP: /{owner}/{repo}.git/...
	if m := gitRouteRegexp.FindStringSubmatch(path); m != nil {
		owner := decodeURIComponent(m[1])
		repoName := decodeURIComponent(m[2])
		action := m[3]
		protoHeader := r.Header.Get("Git-Protocol")
		protocolVersion := strings.TrimPrefix(protoHeader, "version=")
		if protocolVersion == "" {
			protocolVersion = ""
		}
		var out GitResult
		switch {
		case action == "info/refs" && method == "GET":
			service := query.Get("service")
			out = GitInfoRefs(h.store, owner, repoName, service, protocolVersion)
		case action == "git-upload-pack" && method == "POST":
			out = GitUploadPack(h.store, owner, repoName, body, protocolVersion)
		case action == "git-receive-pack" && method == "POST":
			out = GitReceivePack(h.store, owner, repoName, body)
		default:
			h.sendError(w, 404, "Not Found")
			return
		}
		h.sendRaw(w, out.Status, out.ContentType, out.Body)
		return
	}

	// Everything else: GitHub's 404 body.
	h.sendError(w, 404, "Not Found")
}

func newAppHandler() *appHandler {
	return &appHandler{store: NewStore(), expectedToken: os.Getenv("MAKEFUN_SHOP_TOKEN")}
}

func newHTTPServer() *http.Server {
	return &http.Server{
		Handler: newAppHandler(),
		// No timeouts: the Node original sets none either.
		ReadTimeout:  0,
		WriteTimeout: 0,
		IdleTimeout:  0,
	}
}

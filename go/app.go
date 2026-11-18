// app.go — the forge's HTTP dispatch: git smart HTTP, REST v3, GraphQL
// and MCP over the in-memory store. Only the Go standard library is used.
// No wall clock appears in any response.
//
// Transport note: requests arrive as rawRequest values from rawhttp.go
// (the hand-rolled HTTP/1.1 server that replaced net/http); responses are
// built in memory as rawResponse values and written out in one Write.
// The routing, auth, body cap and store-lock semantics are unchanged
// from the net/http version.

package main

import (
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

func copyHeaders(in map[string]string) map[string]string {
	out := make(map[string]string, len(in)+3)
	for k, v := range in {
		out[k] = v
	}
	return out
}

func (h *appHandler) sendJSON(status int, body []byte, headers map[string]string) rawResponse {
	// body is the JSON payload WITHOUT the trailing newline; Node's
	// sendJson appends one.
	hdrs := copyHeaders(headers)
	hdrs["x-github-request-id"] = nextRequestID()
	if status == 204 {
		return rawResponse{status: 204, headers: hdrs}
	}
	text := append(append([]byte(nil), body...), '\n')
	hdrs["content-type"] = jsonType
	hdrs["content-length"] = strconv.Itoa(len(text))
	return rawResponse{status: status, headers: hdrs, body: text}
}

func (h *appHandler) sendRawJSON(status int, body []byte, headers map[string]string) rawResponse {
	// body already includes its trailing newline (prebuilt cache bytes).
	hdrs := copyHeaders(headers)
	hdrs["content-type"] = jsonType
	hdrs["content-length"] = strconv.Itoa(len(body))
	hdrs["x-github-request-id"] = nextRequestID()
	return rawResponse{status: status, headers: hdrs, body: body}
}

func (h *appHandler) sendRaw(status int, contentType string, body []byte) rawResponse {
	return rawResponse{status: status, headers: map[string]string{
		"content-type":   contentType,
		"content-length": strconv.Itoa(len(body)),
	}, body: body}
}

func (h *appHandler) sendError(status int, message string) rawResponse {
	return h.sendJSON(status, marshalJSONNoNL(map[string]string{"message": message}), nil)
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

// handle dispatches one fully-read request and builds the response in
// memory. A handler panic becomes a 500 with the error message, exactly
// as the net/http version's recover did.
func (h *appHandler) handle(req *rawRequest) (resp rawResponse) {
	method := req.method
	path, query := splitURL(req.target)

	// The env's boot check calls this twice: 200 {"ok":true}, no auth.
	if method == "GET" && path == "/healthz" {
		return h.sendJSON(200, marshalJSONNoNL(map[string]bool{"ok": true}), nil)
	}

	// Everything else needs the shop token when one is configured.
	if h.expectedToken != "" {
		if req.headers["authorization"] != "Bearer "+h.expectedToken {
			return h.sendError(401, "Bad credentials")
		}
	}

	needsBody := method == "POST" || method == "PUT" || method == "PATCH" || method == "DELETE"
	var body []byte
	if needsBody {
		if req.bodyTooLarge {
			return h.sendError(500, "body too large")
		}
		body = req.body
	} else {
		body = []byte{}
	}

	// The store is single-writer: hold the lock for the whole dispatch.
	// (Body reading happens in the transport, outside the lock, so
	// network I/O never blocks another request.)
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
			resp = h.sendError(500, msg)
		}
	}()

	// MCP: POST /mcp
	if method == "POST" && path == "/mcp" {
		out := mcpRoute(h.store, string(body))
		if out.status == 202 {
			return rawResponse{status: 202}
		}
		return h.sendJSON(out.status, marshalOMap(out.json), nil)
	}

	// GraphQL: POST /api/graphql
	if method == "POST" && path == "/api/graphql" {
		status, out := graphqlRoute(h.store, string(body))
		// graphqlRoute returns bytes without a trailing newline; sendJson
		// adds one.
		return h.sendJSON(status, out, nil)
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
		host := req.host
		if host == "" {
			host = "127.0.0.1"
		}
		ctx := &RestCtx{Method: method, Segments: segments, Query: query, BodyText: string(body), Host: host}
		out := RestRoute(h.store, ctx)
		if out.Raw != nil {
			return h.sendRawJSON(out.Status, out.Raw, out.Headers)
		}
		if out.Status == 204 {
			return h.sendJSON(204, nil, out.Headers)
		}
		return h.sendJSON(out.Status, marshalJSONNoNL(out.JSON), out.Headers)
	}

	// Git smart HTTP: /{owner}/{repo}.git/...
	if m := gitRouteRegexp.FindStringSubmatch(path); m != nil {
		owner := decodeURIComponent(m[1])
		repoName := decodeURIComponent(m[2])
		action := m[3]
		protoHeader := req.headers["git-protocol"]
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
			return h.sendError(404, "Not Found")
		}
		return h.sendRaw(out.Status, out.ContentType, out.Body)
	}

	// Everything else: GitHub's 404 body.
	return h.sendError(404, "Not Found")
}

func newAppHandler() *appHandler {
	return &appHandler{store: NewStore(), expectedToken: os.Getenv("MAKEFUN_SHOP_TOKEN")}
}

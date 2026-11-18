// rawhttp.go — a minimal HTTP/1.1 server replacing net/http for the forge.
//
// Why: net/http's per-request cost (request/response allocation, header
// maps, bufio, goroutine bookkeeping) is pure overhead on the bench's hot
// paths, where the handlers themselves are microseconds and mostly cached.
// This server does the least work that still speaks correct HTTP/1.1 to
// the harness client (undici fetch):
//   - origin-form request targets, Content-Length and chunked bodies,
//     Expect: 100-continue, keep-alive (1.1 default, 1.0 opt-in),
//     Connection: close, HEAD (headers only, no body bytes).
//   - one goroutine per connection, one Write per response (headers +
//     body joined), no per-request heap churn beyond a small header map.
//   - no timeouts, mirroring the net/http server this replaces.
// Anything malformed gets a plain 400 and the connection closes; the
// handlers never see a half-parsed request.

package main

import (
	"bufio"
	"io"
	"net"
	"strconv"
	"strings"
	"time"
)

// rawRequest is the whole of a parsed HTTP request the handlers need.
type rawRequest struct {
	method    string
	target    string // raw RequestURI, exactly as sent
	version   string // "HTTP/1.1" or "HTTP/1.0"
	headers   map[string]string
	host      string
	body      []byte
	bodyTooLarge bool // body passed maxBodyBytes; handler answers 500
	keepAlive bool
	headOnly  bool
}

// rawResponse is a complete HTTP response, built in memory.
type rawResponse struct {
	status  int
	headers map[string]string
	body    []byte // nil means no body
}

var reasonPhrases = map[int]string{
	200: "OK",
	201: "Created",
	202: "Accepted",
	204: "No Content",
	301: "Moved Permanently",
	302: "Found",
	304: "Not Modified",
	400: "Bad Request",
	401: "Unauthorized",
	403: "Forbidden",
	404: "Not Found",
	405: "Method Not Allowed",
	409: "Conflict",
	422: "Unprocessable Entity",
	500: "Internal Server Error",
}

// serveRaw accepts connections on ln until it errors; each connection is
// served by its own goroutine, exactly like net/http's Serve.
func serveRaw(ln net.Listener, h *appHandler) error {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return err
		}
		go serveRawConn(conn, h)
	}
}

func serveRawConn(conn net.Conn, h *appHandler) {
	defer conn.Close()
	br := bufio.NewReaderSize(conn, 65536)
	for {
		tParse := time.Now()
		req, ok := readRawRequest(br, conn)
		profRecordIO("parse", time.Since(tParse).Microseconds())
		if !ok {
			return
		}
		resp := h.handle(req)
		tWrite := time.Now()
		if !writeRawResponse(conn, req, resp) {
			profRecordIO("write", time.Since(tWrite).Microseconds())
			return
		}
		profRecordIO("write", time.Since(tWrite).Microseconds())
		if !req.keepAlive {
			return
		}
	}
}

// readRawRequest parses one request. ok=false means the connection is
// dead or malformed; the caller closes it. A malformed request line or
// header already got a 400 written by this function.
func readRawRequest(br *bufio.Reader, conn net.Conn) (*rawRequest, bool) {
	line, err := br.ReadString('\n')
	if err != nil {
		return nil, false
	}
	line = strings.TrimRight(line, "\r\n")
	// method SP target SP version
	sp1 := strings.IndexByte(line, ' ')
	if sp1 <= 0 {
		writeRaw400(conn)
		return nil, false
	}
	rest := line[sp1+1:]
	sp2 := strings.IndexByte(rest, ' ')
	if sp2 <= 0 {
		writeRaw400(conn)
		return nil, false
	}
	method := line[:sp1]
	target := rest[:sp2]
	version := rest[sp2+1:]
	if version != "HTTP/1.1" && version != "HTTP/1.0" {
		writeRaw400(conn)
		return nil, false
	}
	if method == "" || target == "" {
		writeRaw400(conn)
		return nil, false
	}

	headers := make(map[string]string, 16)
	for {
		hline, err := br.ReadString('\n')
		if err != nil {
			return nil, false
		}
		hline = strings.TrimRight(hline, "\r\n")
		if hline == "" {
			break
		}
		colon := strings.IndexByte(hline, ':')
		if colon <= 0 {
			writeRaw400(conn)
			return nil, false
		}
		key := strings.ToLower(strings.TrimRight(hline[:colon], " \t"))
		val := strings.TrimLeft(hline[colon+1:], " \t")
		// Last value wins; the handlers only read single-value headers.
		headers[key] = val
	}

	req := &rawRequest{
		method:  method,
		target:  target,
		version: version,
		headers: headers,
		host:    headers["host"],
	}
	if method == "HEAD" {
		req.headOnly = true
	}
	// Keep-alive: 1.1 keeps unless told to close; 1.0 closes unless
	// told to keep.
	connTok := strings.ToLower(headers["connection"])
	if version == "HTTP/1.1" {
		req.keepAlive = !strings.Contains(connTok, "close")
	} else {
		req.keepAlive = strings.Contains(connTok, "keep-alive")
	}

	// Expect: 100-continue — answer before reading the body.
	if strings.EqualFold(headers["expect"], "100-continue") {
		if _, err := conn.Write([]byte("HTTP/1.1 100 Continue\r\n\r\n")); err != nil {
			return nil, false
		}
	}

	// Body. The handlers cap at maxBodyBytes; mirror net/http's flow
	// (read up to cap+1, then 500 "body too large") by flagging overrun
	// and letting the handler answer — but a body longer than the cap
	// leaves unread bytes, so the connection must close afterwards.
	overrun := false
	te := strings.ToLower(headers["transfer-encoding"])
	if strings.Contains(te, "chunked") {
		body, ok, over := readChunkedBody(br)
		if !ok {
			return nil, false
		}
		req.body = body
		overrun = over
	} else if cl := headers["content-length"]; cl != "" {
		n, err := strconv.Atoi(strings.TrimSpace(cl))
		if err != nil || n < 0 {
			writeRaw400(conn)
			return nil, false
		}
		if n > maxBodyBytes+1 {
			// Read the cap so the client is not left hanging, then
			// flag overrun; the connection closes after the 500.
			buf := make([]byte, maxBodyBytes+1)
			if _, err := io.ReadFull(br, buf); err != nil {
				return nil, false
			}
			req.body = buf
			overrun = true
		} else if n > 0 {
			buf := make([]byte, n)
			if _, err := io.ReadFull(br, buf); err != nil {
				return nil, false
			}
			req.body = buf
		}
	}
	if overrun {
		req.bodyTooLarge = true
		req.keepAlive = false
	}
	return req, true
}

// readChunkedBody decodes a chunked request body. Returns the body, false
// on a framing error, and over=true when the body passed maxBodyBytes+1
// (bytes past the cap are discarded, the connection must close).
func readChunkedBody(br *bufio.Reader) ([]byte, bool, bool) {
	var body []byte
	over := false
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return nil, false, false
		}
		line = strings.TrimRight(line, "\r\n")
		if i := strings.IndexByte(line, ';'); i >= 0 {
			line = line[:i]
		}
		size, err := strconv.ParseInt(strings.TrimSpace(line), 16, 64)
		if err != nil || size < 0 {
			return nil, false, false
		}
		if size == 0 {
			// Trailer part: read lines until the empty line.
			for {
				tline, err := br.ReadString('\n')
				if err != nil {
					return nil, false, false
				}
				if strings.TrimRight(tline, "\r\n") == "" {
					break
				}
			}
			return body, true, over
		}
		if !over {
			if int64(len(body))+size > int64(maxBodyBytes+1) {
				over = true
			}
		}
		if over {
			// Discard this chunk and the rest of the body.
			if _, err := io.CopyN(io.Discard, br, size+2); err != nil {
				return nil, false, false
			}
			// Keep consuming chunks until the terminal 0-chunk.
			continue
		}
		chunk := make([]byte, size+2) // payload + CRLF
		if _, err := io.ReadFull(br, chunk); err != nil {
			return nil, false, false
		}
		body = append(body, chunk[:size]...)
	}
}

func writeRaw400(conn net.Conn) {
	_, _ = conn.Write([]byte("HTTP/1.1 400 Bad Request\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"))
}

// writeRawResponse writes one response in a single Write. Returns false
// when the write failed (caller closes the connection).
func writeRawResponse(conn net.Conn, req *rawRequest, resp rawResponse) bool {
	reason, ok := reasonPhrases[resp.status]
	if !ok {
		reason = "Status"
	}
	// Rough size: status line + headers + body.
	out := make([]byte, 0, 256+len(resp.body))
	out = append(out, "HTTP/1.1 "...)
	out = append(out, strconv.Itoa(resp.status)...)
	out = append(out, ' ')
	out = append(out, reason...)
	out = append(out, "\r\n"...)
	hasLength := false
	for k, v := range resp.headers {
		out = append(out, k...)
		out = append(out, ": "...)
		out = append(out, v...)
		out = append(out, "\r\n"...)
		if strings.EqualFold(k, "content-length") {
			hasLength = true
		}
	}
	// net/http sends Content-Length: 0 for a bodyless non-204/304.
	if !hasLength && resp.status != 204 && resp.status != 304 {
		out = append(out, "Content-Length: 0\r\n"...)
	}
	out = append(out, "\r\n"...)
	if !req.headOnly && len(resp.body) > 0 {
		out = append(out, resp.body...)
	}
	_, err := conn.Write(out)
	return err == nil
}

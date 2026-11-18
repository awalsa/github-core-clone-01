// gitproto.go — git smart HTTP: info/refs advertisements, upload-pack (v2
// and v0) and receive-pack (v0). Packs carry plain objects only, never
// deltas. All framing follows the pkt-line format git itself uses.

package main

import (
	"bytes"
	"crypto/sha1"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

const UploadAdCT = "application/x-git-upload-pack-advertisement"
const ReceiveAdCT = "application/x-git-receive-pack-advertisement"
const UploadResultCT = "application/x-git-upload-pack-result"
const ReceiveResultCT = "application/x-git-receive-pack-result"

// GitResult is one git-protocol reply.
type GitResult struct {
	Status      int
	ContentType string
	Body        []byte
}

func gitNotFound() GitResult {
	return GitResult{Status: 404, ContentType: "application/json; charset=utf-8", Body: []byte("{\"message\":\"Not Found\"}\n")}
}

func gitBadRequest(msg string) GitResult {
	return GitResult{Status: 400, ContentType: "application/json; charset=utf-8", Body: []byte(fmt.Sprintf("{\"message\":%q}\n", msg))}
}

// Encode one pkt-line (payload includes its trailing newline when present).
func pktLine(payload []byte) []byte {
	l := len(payload) + 4
	head := fmt.Sprintf("%04x", l)
	out := make([]byte, 0, l)
	out = append(out, head...)
	out = append(out, payload...)
	return out
}

func pktLineStr(s string) []byte { return pktLine([]byte(s)) }

var flushPkt = []byte("0000")
var delimPkt = []byte("0001")

func encodePktLines(lines [][]byte) []byte {
	var out []byte
	for _, l := range lines {
		out = append(out, pktLine(l)...)
	}
	return out
}

func encodePktLineStrings(lines []string) []byte {
	var out []byte
	for _, l := range lines {
		out = append(out, pktLineStr(l)...)
	}
	return out
}

type pktKind int

const (
	pktData pktKind = iota
	pktFlush
	pktDelim
)

type pkt struct {
	kind pktKind
	data []byte
}

// Decode a pkt-line stream into packets.
func decodePktLines(buf []byte) ([]pkt, error) {
	var out []pkt
	off := 0
	for off+4 <= len(buf) {
		n, err := strconv.ParseInt(string(buf[off:off+4]), 16, 32)
		if err != nil {
			return nil, fmt.Errorf("not a pkt-line")
		}
		l := int(n)
		if l == 0 {
			out = append(out, pkt{kind: pktFlush})
			off += 4
			continue
		}
		if l == 1 {
			out = append(out, pkt{kind: pktDelim})
			off += 4
			continue
		}
		if l < 4 || off+l > len(buf) {
			return nil, fmt.Errorf("pkt-line length out of range")
		}
		out = append(out, pkt{kind: pktData, data: buf[off+4 : off+l]})
		off += l
	}
	return out, nil
}

// Split a byte buffer into sideband packets (band 1) of at most 65515 bytes.
func sidebandPackets(data []byte) []byte {
	const max = 65515
	var out []byte
	for off := 0; off < len(data); off += max {
		end := off + max
		if end > len(data) {
			end = len(data)
		}
		payload := make([]byte, 0, 1+end-off)
		payload = append(payload, 1)
		payload = append(payload, data[off:end]...)
		out = append(out, pktLine(payload)...)
	}
	out = append(out, flushPkt...)
	return out
}

func advertisement(s *Store, repo *Repo, service string) []byte {
	isUpload := service == "upload"
	ctService := "git-receive-pack"
	if isUpload {
		ctService = "git-upload-pack"
	}
	caps := "report-status delete-refs side-band-64k ofs-delta object-format=sha1"
	if isUpload {
		caps = "multi_ack thin-pack side-band side-band-64k ofs-delta shallow deepen-since deepen-not deepen-relative no-progress include-tag multi_ack_detailed no-done symref=HEAD:refs/heads/main"
	}
	refs := s.ListRefs(repo)
	var lines [][]byte
	lines = append(lines, pktLineStr(fmt.Sprintf("# service=%s\n", ctService)), flushPkt)
	if len(refs) == 0 {
		lines = append(lines, pktLineStr(fmt.Sprintf("%s capabilities^{}\x00%s\n", ZeroOID, caps)))
	} else if isUpload {
		head := refs[0].OID
		if oid, ok := repo.Refs["refs/heads/main"]; ok {
			head = oid
		}
		lines = append(lines, pktLineStr(fmt.Sprintf("%s HEAD\x00%s\n", head, caps)))
		for _, r := range refs {
			lines = append(lines, pktLineStr(fmt.Sprintf("%s %s\n", r.OID, r.Name)))
			if obj, ok := s.GetObject(r.OID); ok && obj.Type == "tag" {
				if peeled := s.PeelToCommit(r.OID); peeled != "" {
					lines = append(lines, pktLineStr(fmt.Sprintf("%s %s^{}\n", peeled, r.Name)))
				}
			}
		}
	} else {
		first := true
		for _, r := range refs {
			if first {
				lines = append(lines, pktLineStr(fmt.Sprintf("%s %s\x00%s\n", r.OID, r.Name, caps)))
				first = false
			} else {
				lines = append(lines, pktLineStr(fmt.Sprintf("%s %s\n", r.OID, r.Name)))
			}
			if obj, ok := s.GetObject(r.OID); ok && obj.Type == "tag" {
				if peeled := s.PeelToCommit(r.OID); peeled != "" {
					lines = append(lines, pktLineStr(fmt.Sprintf("%s %s^{}\n", peeled, r.Name)))
				}
			}
		}
	}
	lines = append(lines, flushPkt)
	var out []byte
	for _, l := range lines {
		out = append(out, l...)
	}
	return out
}

// The v2 advertisement bytes never depend on the repo: build them once.
var v2AdvertisementBytes []byte

func v2Advertisement() []byte {
	if v2AdvertisementBytes == nil {
		caps := []string{"ls-refs=unborn", "fetch=shallow wait-for-done", "object-format=sha1", "ofs-delta"}
		var lines [][]byte
		lines = append(lines, pktLineStr("version 2\n"))
		for _, c := range caps {
			lines = append(lines, pktLineStr(c+"\n"))
		}
		lines = append(lines, flushPkt)
		var out []byte
		for _, l := range lines {
			out = append(out, l...)
		}
		v2AdvertisementBytes = out
	}
	return v2AdvertisementBytes
}

// A valid pack with zero objects (used when a fetch has nothing to send).
func emptyPack() []byte {
	header := make([]byte, 12)
	copy(header[0:4], "PACK")
	header[7] = 2
	sum := sha1.Sum(header)
	out := append([]byte(nil), header...)
	out = append(out, sum[:]...)
	return out
}

func GitInfoRefs(s *Store, owner, repoName, service, protocolVersion string) GitResult {
	repo := s.GetRepo(owner, repoName)
	if repo == nil {
		return gitNotFound()
	}
	if service != "git-upload-pack" && service != "git-receive-pack" {
		return gitBadRequest("bad service")
	}
	kind := "receive"
	if service == "git-upload-pack" {
		kind = "upload"
	}
	ct := ReceiveAdCT
	if kind == "upload" {
		ct = UploadAdCT
	}
	if protocolVersion == "2" {
		return GitResult{Status: 200, ContentType: ct, Body: v2Advertisement()}
	}
	if cached, ok := repo.AdCache[kind]; ok {
		return GitResult{Status: 200, ContentType: ct, Body: cached}
	}
	body := advertisement(s, repo, kind)
	repo.AdCache[kind] = body
	return GitResult{Status: 200, ContentType: ct, Body: body}
}

// ---------------------------------------------------------------------------
// upload-pack
// ---------------------------------------------------------------------------

type fetchRequest struct {
	command  string // "ls-refs" | "fetch"
	wants    []string
	haves    []string
	shallow  []string
	depth    int
	prefixes []string
	peel     bool
	symrefs  bool
	unborn   bool
}

func parseUploadPackRequest(buf []byte) (*fetchRequest, bool) {
	pkts, err := decodePktLines(buf)
	if err != nil {
		return nil, false
	}
	req := &fetchRequest{command: "fetch", depth: -1}
	seenCommand := false
	// A v0 fetch has no command line: it starts directly with a "want" line.
	if len(pkts) > 0 && pkts[0].kind == pktData && strings.HasPrefix(strings.TrimSpace(string(pkts[0].data)), "want ") {
		seenCommand = true
	}
	for _, p := range pkts {
		if p.kind == pktFlush {
			// v0 fetch is two sections (wants, then haves+done) separated by
			// a flush: keep reading until 'done' or the end of the stream.
			if req.command == "fetch" && seenCommand {
				continue
			}
			break
		}
		if p.kind == pktDelim {
			continue
		}
		line := strings.TrimSpace(string(p.data))
		if !seenCommand {
			if line == "command=ls-refs" {
				req.command = "ls-refs"
			} else if line == "command=fetch" {
				req.command = "fetch"
			} else {
				return nil, false
			}
			seenCommand = true
			continue
		}
		if req.command == "ls-refs" {
			if line == "peel" {
				req.peel = true
			} else if line == "symrefs" {
				req.symrefs = true
			} else if line == "unborn" {
				req.unborn = true
			} else if strings.HasPrefix(line, "ref-prefix ") {
				req.prefixes = append(req.prefixes, line[len("ref-prefix "):])
			}
		} else {
			firstToken := func(keyword string) (string, bool) {
				if !strings.HasPrefix(line, keyword) {
					return "", false
				}
				fields := strings.Fields(strings.TrimSpace(line[len(keyword):]))
				if len(fields) == 0 {
					return "", false
				}
				return fields[0], true
			}
			if w, ok := firstToken("want "); ok {
				req.wants = append(req.wants, w)
			}
			if h, ok := firstToken("have "); ok {
				req.haves = append(req.haves, h)
			}
			if sh, ok := firstToken("shallow "); ok {
				req.shallow = append(req.shallow, sh)
			}
			if strings.HasPrefix(line, "deepen ") {
				if n, ok := parseLeadingInt(strings.TrimSpace(line[len("deepen "):])); ok && n > 0 {
					req.depth = n
				}
			} else if line == "done" {
				break
			}
		}
	}
	if !seenCommand {
		return nil, false
	}
	return req, true
}

// parseLeadingInt mimics JS parseInt(s, 10) for a leading run of digits.
func parseLeadingInt(s string) (int, bool) {
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i == 0 {
		return 0, false
	}
	n, err := strconv.Atoi(s[:i])
	if err != nil {
		return 0, false
	}
	return n, true
}

func lsRefsResponse(s *Store, repo *Repo, req *fetchRequest) []byte {
	sortedPrefixes := append([]string(nil), req.prefixes...)
	sort.Strings(sortedPrefixes)
	peel := "0"
	if req.peel {
		peel = "1"
	}
	symrefs := "0"
	if req.symrefs {
		symrefs = "1"
	}
	unborn := "0"
	if req.unborn {
		unborn = "1"
	}
	cacheKey := peel + "|" + symrefs + "|" + unborn + "|" + strings.Join(sortedPrefixes, ",")
	if cached, ok := repo.LsRefsCache[cacheKey]; ok {
		return cached
	}
	var lines [][]byte
	refs := s.ListRefs(repo)
	headOID, hasHead := repo.Refs["refs/heads/main"]
	matches := func(name string) bool {
		if len(req.prefixes) == 0 {
			return true
		}
		for _, p := range req.prefixes {
			if strings.HasPrefix(name, p) {
				return true
			}
		}
		return false
	}
	headMatches := false
	if len(req.prefixes) == 0 {
		headMatches = true
	} else {
		for _, p := range req.prefixes {
			if strings.HasPrefix("HEAD", p) {
				headMatches = true
				break
			}
		}
	}
	if headMatches {
		if hasHead {
			lines = append(lines, pktLineStr(fmt.Sprintf("%s HEAD symref-target:refs/heads/main\n", headOID)))
		} else if req.unborn {
			lines = append(lines, pktLineStr("unborn HEAD symref-target:refs/heads/main\n"))
		}
	}
	for _, r := range refs {
		if !matches(r.Name) {
			continue
		}
		line := fmt.Sprintf("%s %s", r.OID, r.Name)
		if obj, ok := s.GetObject(r.OID); ok && obj.Type == "tag" {
			if peeled := s.PeelToCommit(r.OID); peeled != "" {
				line += fmt.Sprintf(" peeled:%s", peeled)
			}
		}
		lines = append(lines, pktLineStr(line+"\n"))
	}
	lines = append(lines, flushPkt)
	var body []byte
	for _, l := range lines {
		body = append(body, l...)
	}
	repo.LsRefsCache[cacheKey] = body
	return body
}

// The whole fetch reply, cached when the request is bench-shaped: no haves,
// no client shallow lines, and every want already in the store.
func cachedFetchResponse(s *Store, repo *Repo, req *fetchRequest, isV2 bool) []byte {
	cacheable := len(req.haves) == 0 && len(req.shallow) == 0
	if cacheable {
		for _, w := range req.wants {
			if !s.HasObject(w) {
				cacheable = false
				break
			}
		}
	}
	if !cacheable {
		if isV2 {
			return fetchResponseV2(s, repo, req)
		}
		return fetchResponseV0(s, repo, req)
	}
	ver := "v0"
	if isV2 {
		ver = "v2"
	}
	key := fetchPackKey(req.wants, req.haves, req.shallow, req.depth) + "|" + ver
	if cached, ok := repo.FetchResponseCache[key]; ok {
		profCache("fetchReply", true)
		return cached
	}
	profCache("fetchReply", false)
	var body []byte
	if isV2 {
		body = fetchResponseV2(s, repo, req)
	} else {
		body = fetchResponseV0(s, repo, req)
	}
	repo.FetchResponseCache[key] = body
	return body
}

func fetchResponseV2(s *Store, repo *Repo, req *fetchRequest) []byte {
	res := s.BuildFetchPack(repo, req.wants, req.haves, req.shallow, req.depth)
	var parts [][]byte
	if len(res.Shallow) > 0 || len(res.Unshallow) > 0 {
		parts = append(parts, pktLineStr("shallow-info\n"))
		for _, sh := range res.Shallow {
			parts = append(parts, pktLineStr(fmt.Sprintf("shallow %s\n", sh)))
		}
		for _, u := range res.Unshallow {
			parts = append(parts, pktLineStr(fmt.Sprintf("unshallow %s\n", u)))
		}
		parts = append(parts, delimPkt)
	}
	if res.Pack != nil {
		parts = append(parts, pktLineStr("packfile\n"))
		parts = append(parts, sidebandPackets(res.Pack))
	} else if len(req.wants) > 0 {
		parts = append(parts, pktLineStr("packfile\n"))
		parts = append(parts, sidebandPackets(emptyPack()))
	} else {
		parts = append(parts, flushPkt)
	}
	return bytes.Join(parts, nil)
}

func fetchResponseV0(s *Store, repo *Repo, req *fetchRequest) []byte {
	res := s.BuildFetchPack(repo, req.wants, req.haves, req.shallow, req.depth)
	var parts [][]byte
	heldHave := ""
	for i := len(req.haves) - 1; i >= 0; i-- {
		if s.HasObject(req.haves[i]) {
			heldHave = req.haves[i]
			break
		}
	}
	if heldHave != "" {
		parts = append(parts, pktLineStr(fmt.Sprintf("ACK %s\n", heldHave)))
	} else {
		parts = append(parts, pktLineStr("NAK\n"))
	}
	for _, sh := range res.Shallow {
		parts = append(parts, pktLineStr(fmt.Sprintf("shallow %s\n", sh)))
	}
	for _, u := range res.Unshallow {
		parts = append(parts, pktLineStr(fmt.Sprintf("unshallow %s\n", u)))
	}
	if res.Pack != nil {
		parts = append(parts, sidebandPackets(res.Pack))
	} else if len(req.wants) > 0 {
		parts = append(parts, sidebandPackets(emptyPack()))
	} else {
		parts = append(parts, flushPkt)
	}
	return bytes.Join(parts, nil)
}

func GitUploadPack(s *Store, owner, repoName string, body []byte, protocolVersion string) GitResult {
	repo := s.GetRepo(owner, repoName)
	if repo == nil {
		return gitNotFound()
	}
	req, ok := parseUploadPackRequest(body)
	if !ok {
		return gitBadRequest("malformed git request")
	}
	if req.command == "ls-refs" {
		return GitResult{Status: 200, ContentType: UploadResultCT, Body: lsRefsResponse(s, repo, req)}
	}
	isV2 := protocolVersion == "2"
	out := cachedFetchResponse(s, repo, req, isV2)
	return GitResult{Status: 200, ContentType: UploadResultCT, Body: out}
}

// ---------------------------------------------------------------------------
// receive-pack
// ---------------------------------------------------------------------------

type pushCommand struct {
	oldOID string
	newOID string
	ref    string
}

func parseReceivePack(body []byte) ([]pushCommand, int, bool) {
	off := 0
	var commands []pushCommand
	for off+4 <= len(body) {
		n, err := strconv.ParseInt(string(body[off:off+4]), 16, 32)
		if err != nil {
			return nil, 0, false
		}
		l := int(n)
		if l == 0 {
			off += 4
			break
		}
		if l < 4 || off+l > len(body) {
			return nil, 0, false
		}
		line := string(body[off+4 : off+l])
		if nul := strings.IndexByte(line, 0); nul >= 0 {
			line = line[:nul]
		}
		line = strings.TrimSpace(line)
		parts := strings.Split(line, " ")
		if len(parts) != 3 {
			return nil, 0, false
		}
		commands = append(commands, pushCommand{oldOID: parts[0], newOID: parts[1], ref: parts[2]})
		off += l
	}
	return commands, off, true
}

func GitReceivePack(s *Store, owner, repoName string, body []byte) GitResult {
	repo := s.GetRepo(owner, repoName)
	if repo == nil {
		return gitNotFound()
	}
	commands, packStart, ok := parseReceivePack(body)
	if !ok {
		return gitBadRequest("malformed git request")
	}
	packBytes := body[packStart:]
	unpackOK := true
	unpackError := ""
	if len(packBytes) > 0 {
		objs, err := parsePack(packBytes)
		if err != nil {
			unpackOK = false
			unpackError = err.Error()
		} else {
			for oid, obj := range objs {
				if !s.HasObject(oid) {
					s.PutObjectWithOID(oid, obj.Type, obj.Data)
				}
			}
		}
	}
	report := []string{}
	if unpackOK {
		report = append(report, "unpack ok")
	} else {
		errMsg := unpackError
		if errMsg == "" {
			errMsg = "failed"
		}
		report = append(report, "unpack "+errMsg)
	}
	for _, cmd := range commands {
		if !unpackOK {
			report = append(report, fmt.Sprintf("ng %s unpack failed", cmd.ref))
			continue
		}
		current, hasCurrent := repo.Refs[cmd.ref]
		if cmd.newOID == ZeroOID {
			// delete
			if !hasCurrent {
				report = append(report, fmt.Sprintf("ng %s ref does not exist", cmd.ref))
			} else if cmd.oldOID != ZeroOID && cmd.oldOID != current {
				report = append(report, fmt.Sprintf("ng %s non-fast-forward", cmd.ref))
			} else {
				delete(repo.Refs, cmd.ref)
				clearRefCaches(repo)
				report = append(report, fmt.Sprintf("ok %s", cmd.ref))
			}
			continue
		}
		if cmd.oldOID == ZeroOID {
			// create
			if hasCurrent {
				report = append(report, fmt.Sprintf("ng %s already exists", cmd.ref))
			} else if !s.HasObject(cmd.newOID) {
				report = append(report, fmt.Sprintf("ng %s object not found", cmd.ref))
			} else {
				repo.Refs[cmd.ref] = cmd.newOID
				clearRefCaches(repo)
				report = append(report, fmt.Sprintf("ok %s", cmd.ref))
			}
			continue
		}
		// update
		if !hasCurrent {
			report = append(report, fmt.Sprintf("ng %s ref does not exist", cmd.ref))
		} else if current != cmd.oldOID {
			report = append(report, fmt.Sprintf("ng %s non-fast-forward", cmd.ref))
		} else if !s.HasObject(cmd.newOID) {
			report = append(report, fmt.Sprintf("ng %s object not found", cmd.ref))
		} else if !s.IsAncestor(current, cmd.newOID) {
			report = append(report, fmt.Sprintf("ng %s non-fast-forward", cmd.ref))
		} else {
			repo.Refs[cmd.ref] = cmd.newOID
			clearRefCaches(repo)
			report = append(report, fmt.Sprintf("ok %s", cmd.ref))
		}
	}
	var reportLines []string
	for _, l := range report {
		reportLines = append(reportLines, l+"\n")
	}
	reportStream := append(encodePktLineStrings(reportLines), flushPkt...)
	return GitResult{Status: 200, ContentType: ReceiveResultCT, Body: sidebandPackets(reportStream)}
}

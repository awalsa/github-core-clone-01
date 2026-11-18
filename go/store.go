// store.go — the forge's whole state: one in-memory store holding git objects
// (content-addressed by SHA-1) plus repos, issues, pull requests, labels and
// comments. No disk, no clock: every timestamp the API returns is the fixed
// scenario epoch, so repeated runs are byte-identical.
//
// The viewer is the scenario's first user (adaiversen). It is derived from
// the sealed scenario's fixed seed with the same deterministic generator the
// suite uses, with a hard-coded fallback if anything ever disagrees.
//
// Concurrency: the HTTP layer holds Store.mu for the whole request dispatch
// (after reading the body), so no method here locks. Keep it that way: the
// store is single-writer by construction, exactly like the Node original.

package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"sync"
)

var tagTargetRegexp = regexp.MustCompile(`(?m)^object ([0-9a-f]{40})$`)

const ZeroOID = "0000000000000000000000000000000000000000"
const FixedTS = "2025-01-01T00:00:00Z"
const FixedEpochSec = 1735689600
const ViewerFallback = "adaiversen"

func sha1Hex(data []byte) string {
	sum := sha1.Sum(data)
	return hex.EncodeToString(sum[:])
}

func hashObject(typ string, data []byte) string {
	h := sha1.New()
	h.Write([]byte(fmt.Sprintf("%s %d\x00", typ, len(data))))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

// ---------------------------------------------------------------------------
// Deterministic scenario generator (same fixed seed the suite uses).
// ---------------------------------------------------------------------------

const scenarioSeed uint64 = 0x6a09e667f3bcc909

var firstNames = []string{"ada", "bram", "cora", "dmitri", "elena", "felix", "greta", "hugo", "iris", "jonas", "kira", "lars", "mira", "nico", "olga", "pavel", "quinn", "rosa", "sven", "tess", "ulla", "vito", "wren", "yara", "zane"}
var lastNames = []string{"anders", "bauer", "costa", "dahl", "eriksen", "fischer", "garcia", "holm", "iversen", "jansen", "keller", "lind", "moreau", "novak", "olsen", "peters", "quist", "rossi", "sato", "turi", "ulrich", "vega", "weber", "xu", "yilmaz", "zimmer"}

func fnv1a64(input string) uint64 {
	h := uint64(0xcbf29ce484222325)
	for i := 0; i < len(input); i++ {
		h ^= uint64(input[i])
		h *= 0x100000001b3
	}
	return h
}

// mix64 is splitmix64's finalizer.
func mix64(value uint64) uint64 {
	z := value + 0x9e3779b97f4a7c15
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

func scenarioSeedFor(label string) uint64 {
	return mix64(scenarioSeed ^ fnv1a64(label))
}

// Rng is xorshift64*: the suite's deterministic RNG.
type Rng struct{ state uint64 }

func newRng(seed uint64) *Rng {
	if seed == 0 {
		seed = scenarioSeed
	}
	return &Rng{state: seed}
}

func (r *Rng) nextU64() uint64 {
	x := r.state
	x ^= x >> 12
	x ^= x << 25
	x ^= x >> 27
	r.state = x
	return x * 0x2545f4914f6cdd1d
}

func (r *Rng) nextU32() uint32 {
	return uint32(r.nextU64() >> 32)
}

func (r *Rng) nextInt(n int) int {
	if n == 1 {
		return 0
	}
	const range32 = uint64(4294967296)
	limit := range32 - (range32 % uint64(n))
	for {
		v := uint64(r.nextU32())
		if v < limit {
			return int(v % uint64(n))
		}
	}
}

func (r *Rng) pick(arr []string) string {
	return arr[r.nextInt(len(arr))]
}

func scenarioViewerLogin() string {
	defer func() {
		// The Node original falls back on any error; the arithmetic here
		// cannot fail, but keep the same contract.
		_ = recover()
	}()
	rng := newRng(scenarioSeedFor("users"))
	used := map[string]bool{}
	for i := 0; i < 12; i++ {
		first := rng.pick(firstNames)
		last := rng.pick(lastNames)
		base := sanitizeLogin(first + last)
		login := base
		suffix := 2
		for used[login] {
			login = fmt.Sprintf("%s-%d", base, suffix)
			suffix++
		}
		used[login] = true
		if i == 0 {
			return login
		}
	}
	return ViewerFallback
}

func sanitizeLogin(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' {
			b.WriteByte(c)
		}
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// Git objects
// ---------------------------------------------------------------------------

type GitObject struct {
	Type string
	Data []byte
}

type CommitInfo struct {
	Tree    string
	Parents []string
	Message string
}

type TreeEntry struct {
	Mode  string
	Name  string
	OID   string
	IsDir bool
}

func parseCommit(data []byte) CommitInfo {
	text := string(data)
	head := text
	message := ""
	if idx := strings.Index(text, "\n\n"); idx >= 0 {
		head = text[:idx]
		message = text[idx+2:]
	}
	var tree string
	var parents []string
	for _, line := range strings.Split(head, "\n") {
		if strings.HasPrefix(line, "tree ") {
			tree = strings.TrimSpace(line[5:])
		} else if strings.HasPrefix(line, "parent ") {
			parents = append(parents, strings.TrimSpace(line[7:]))
		}
	}
	return CommitInfo{Tree: tree, Parents: parents, Message: message}
}

func parseTree(data []byte) []TreeEntry {
	var entries []TreeEntry
	off := 0
	for off < len(data) {
		sp := bytes.IndexByte(data[off:], 0x20)
		if sp < 0 {
			break
		}
		sp += off
		nul := bytes.IndexByte(data[sp:], 0x00)
		if nul < 0 {
			break
		}
		nul += sp
		mode := string(data[off:sp])
		name := string(data[sp+1 : nul])
		if nul+21 > len(data) {
			break
		}
		oid := hex.EncodeToString(data[nul+1 : nul+21])
		entries = append(entries, TreeEntry{Mode: mode, Name: name, OID: oid, IsDir: mode == "40000"})
		off = nul + 21
	}
	return entries
}

func buildCommit(tree string, parents []string, message string, epochSec int64) []byte {
	who := fmt.Sprintf("Conformance <conformance@make.fun> %d +0000", epochSec)
	var b strings.Builder
	b.WriteString("tree " + tree + "\n")
	for _, p := range parents {
		b.WriteString("parent " + p + "\n")
	}
	b.WriteString("author " + who + "\ncommitter " + who + "\n\n" + message)
	return []byte(b.String())
}

func buildTag(target, targetType, name, message string, epochSec int64) []byte {
	who := fmt.Sprintf("Conformance <conformance@make.fun> %d +0000", epochSec)
	return []byte(fmt.Sprintf("object %s\ntype %s\ntag %s\ntagger %s\n\n%s", target, targetType, name, who, message))
}

// Build a tree object from sorted entries; entries must already be git-sorted.
func buildTree(entries []TreeEntry) []byte {
	var buf bytes.Buffer
	for _, e := range entries {
		buf.WriteString(e.Mode + " " + e.Name + "\x00")
		raw, _ := hex.DecodeString(e.OID)
		buf.Write(raw)
	}
	return buf.Bytes()
}

// git's tree sort: directory names compare as if they ended in '/'.
func treeSortKey(name string, isDir bool) string {
	if isDir {
		return name + "/"
	}
	return name
}

func sortTreeEntries(entries []TreeEntry) []TreeEntry {
	sort.Slice(entries, func(i, j int) bool {
		ka := treeSortKey(entries[i].Name, entries[i].IsDir)
		kb := treeSortKey(entries[j].Name, entries[j].IsDir)
		return ka < kb
	})
	return entries
}

// ---------------------------------------------------------------------------
// Pack files (plain objects only — no deltas)
// ---------------------------------------------------------------------------

func encodeObjectHeader(typeNum int, size int) []byte {
	var bytesOut []byte
	b := byte((typeNum << 4) | (size & 0x0f))
	size >>= 4
	for size > 0 {
		bytesOut = append(bytesOut, b|0x80)
		b = byte(size & 0x7f)
		size >>= 7
	}
	bytesOut = append(bytesOut, b)
	return bytesOut
}

var typeNum = map[string]int{"commit": 1, "tree": 2, "blob": 3, "tag": 4}
var numType = map[int]string{1: "commit", 2: "tree", 3: "blob", 4: "tag"}

func buildPack(objects map[string]GitObject) []byte {
	oids := make([]string, 0, len(objects))
	for oid := range objects {
		oids = append(oids, oid)
	}
	sort.Strings(oids)
	var chunks [][]byte
	header := make([]byte, 12)
	copy(header[0:4], "PACK")
	header[4] = 0
	header[5] = 0
	header[6] = 0
	header[7] = 2
	n := len(oids)
	header[8] = byte(n >> 24)
	header[9] = byte(n >> 16)
	header[10] = byte(n >> 8)
	header[11] = byte(n)
	chunks = append(chunks, header)
	for _, oid := range oids {
		obj := objects[oid]
		chunks = append(chunks, encodeObjectHeader(typeNum[obj.Type], len(obj.Data)))
		var zb bytes.Buffer
		zw, _ := zlib.NewWriterLevel(&zb, 1)
		zw.Write(obj.Data)
		zw.Close()
		chunks = append(chunks, zb.Bytes())
	}
	trailer := sha1.New()
	for _, c := range chunks {
		trailer.Write(c)
	}
	chunks = append(chunks, trailer.Sum(nil))
	var out bytes.Buffer
	for _, c := range chunks {
		out.Write(c)
	}
	return out.Bytes()
}

// inflateOne inflates a single zlib stream at the start of buf and returns
// the data plus the number of compressed bytes the stream consumed
// (2-byte header + deflate stream + 4-byte Adler32).
func inflateOne(buf []byte) ([]byte, int, error) {
	br := bytes.NewReader(buf)
	zr, err := zlib.NewReader(br)
	if err != nil {
		return nil, 0, err
	}
	data, err := io.ReadAll(zr)
	if err != nil {
		zr.Close()
		return nil, 0, err
	}
	if err := zr.Close(); err != nil {
		return nil, 0, err
	}
	consumed := len(buf) - br.Len()
	return data, consumed, nil
}

// Parse a pack; throws on a bad header, bad zlib stream or bad trailer.
func parsePack(buf []byte) (map[string]GitObject, error) {
	if len(buf) < 32 || string(buf[0:4]) != "PACK" {
		return nil, fmt.Errorf("bad pack header")
	}
	version := uint32(buf[4])<<24 | uint32(buf[5])<<16 | uint32(buf[6])<<8 | uint32(buf[7])
	if version != 2 {
		return nil, fmt.Errorf("unsupported pack version")
	}
	count := int(uint32(buf[8])<<24 | uint32(buf[9])<<16 | uint32(buf[10])<<8 | uint32(buf[11]))
	trailer := buf[len(buf)-20:]
	expect := sha1.Sum(buf[:len(buf)-20])
	if !bytes.Equal(expect[:], trailer) {
		return nil, fmt.Errorf("pack checksum mismatch")
	}
	out := make(map[string]GitObject, count)
	off := 12
	for i := 0; i < count; i++ {
		if off >= len(buf) {
			return nil, fmt.Errorf("pack truncated")
		}
		b := buf[off]
		off++
		typeNum := (b >> 4) & 0x7
		size := int(b & 0x0f)
		shift := uint(4)
		for b&0x80 != 0 {
			if off >= len(buf) {
				return nil, fmt.Errorf("pack truncated")
			}
			b = buf[off]
			off++
			size |= int(b&0x7f) << shift
			shift += 7
		}
		typ, ok := numType[int(typeNum)]
		if !ok {
			return nil, fmt.Errorf("unknown object type in pack")
		}
		data, consumed, err := inflateOne(buf[off:])
		if err != nil {
			return nil, err
		}
		if len(data) != size {
			return nil, fmt.Errorf("object size mismatch")
		}
		if consumed <= 0 {
			return nil, fmt.Errorf("cannot measure zlib stream")
		}
		off += consumed
		out[hashObject(typ, data)] = GitObject{Type: typ, Data: data}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Domain types
// ---------------------------------------------------------------------------

type Comment struct {
	ID   int
	Body string
}

type Issue struct {
	ID     int
	Number int
	Title  string
	Body   string
	State  string // "open" | "closed"
	Labels []string
	Comments []Comment
	Seq    int
	// Cached JSON body (with trailing newline) for GET single issue; nil when stale.
	ViewCache []byte
}

type PullRequest struct {
	ID       int
	Number   int
	Title    string
	Body     string
	State    string // "open" | "closed"
	HeadRef  string
	BaseRef  string
	HeadSha  string
	BaseSha  string
	Merged   bool
	MergedAt *string
	MergeSha *string
	Seq      int
}

type Repo struct {
	ID            int
	Name          string
	Owner         string
	Private       bool
	Description   string
	DefaultBranch string
	Refs          map[string]string
	Issues        map[int]*Issue
	Pulls         map[int]*PullRequest
	NextNumber    int
	Labels        map[string]Label
	Seq           int
	PackCache     map[string]FetchPackResult
	AdCache       map[string][]byte
	LsRefsCache   map[string][]byte
	FetchResponseCache map[string][]byte
}

type Label struct {
	Name  string
	Color string
}

type FileChange struct {
	Path      string
	Status    string // "added" | "removed" | "modified"
	Additions int
	Deletions int
}

type FetchPackResult struct {
	Pack      []byte // nil when nothing to send
	Shallow   []string
	Unshallow []string
}

// Clear every cache derived from a repo's refs (packs, advertisements, ls-refs, fetch replies).
func clearRefCaches(repo *Repo) {
	repo.PackCache = map[string]FetchPackResult{}
	repo.AdCache = map[string][]byte{}
	repo.LsRefsCache = map[string][]byte{}
	repo.FetchResponseCache = map[string][]byte{}
}

// The cache key for a fetch pack (shared by buildFetchPack and the reply cache).
func fetchPackKey(wants, haves, clientShallow []string, depth int) string {
	sorted := func(s []string) []string {
		c := append([]string(nil), s...)
		sort.Strings(c)
		return c
	}
	return strings.Join(sorted(wants), ",") + "|" + strings.Join(sorted(haves), ",") + "|" + strings.Join(sorted(clientShallow), ",") + "|" + fmt.Sprint(depth)
}

// ---------------------------------------------------------------------------
// The store
// ---------------------------------------------------------------------------

type Store struct {
	mu sync.Mutex

	ViewerLogin   string
	Objects       map[string]GitObject
	Repos         map[string]*Repo
	ReposByID     map[int]*Repo
	commitCache   map[string]CommitInfo
	treeCache     map[string][]TreeEntry
	fileMapCache  map[string]map[string]FileRef
	repoIDCounter int
	objectIDCounter int
	seqCounter    int
}

type FileRef struct {
	OID  string
	Mode string
}

func NewStore() *Store {
	return &Store{
		ViewerLogin:   scenarioViewerLogin(),
		Objects:       map[string]GitObject{},
		Repos:         map[string]*Repo{},
		ReposByID:     map[int]*Repo{},
		commitCache:   map[string]CommitInfo{},
		treeCache:     map[string][]TreeEntry{},
		fileMapCache:  map[string]map[string]FileRef{},
		repoIDCounter: 1,
		objectIDCounter: 1,
		seqCounter:    1,
	}
}

// Locking: the HTTP layer holds mu for the whole dispatch.
func (s *Store) Lock()   { s.mu.Lock() }
func (s *Store) Unlock() { s.mu.Unlock() }

// -- object store ---------------------------------------------------------

func (s *Store) PutObject(typ string, data []byte) string {
	oid := hashObject(typ, data)
	if _, ok := s.Objects[oid]; !ok {
		s.Objects[oid] = GitObject{Type: typ, Data: data}
	}
	return oid
}

// Store an object whose oid is already known (e.g. a pack just parsed it).
func (s *Store) PutObjectWithOID(oid, typ string, data []byte) {
	if _, ok := s.Objects[oid]; !ok {
		s.Objects[oid] = GitObject{Type: typ, Data: data}
	}
}

func (s *Store) GetObject(oid string) (GitObject, bool) {
	o, ok := s.Objects[oid]
	return o, ok
}

func (s *Store) HasObject(oid string) bool {
	_, ok := s.Objects[oid]
	return ok
}

func (s *Store) ParseCommit(oid string) (CommitInfo, bool) {
	if c, ok := s.commitCache[oid]; ok {
		return c, true
	}
	obj, ok := s.Objects[oid]
	if !ok || obj.Type != "commit" {
		return CommitInfo{}, false
	}
	c := parseCommit(obj.Data)
	s.commitCache[oid] = c
	return c, true
}

func (s *Store) ParseTree(oid string) ([]TreeEntry, bool) {
	if t, ok := s.treeCache[oid]; ok {
		return t, true
	}
	obj, ok := s.Objects[oid]
	if !ok || obj.Type != "tree" {
		return nil, false
	}
	t := parseTree(obj.Data)
	s.treeCache[oid] = t
	return t, true
}

// Peel a tag (possibly nested) to its commit oid; "" if unresolvable.
func (s *Store) ResolveToCommit(oid string) string {
	cur := oid
	for i := 0; i < 16; i++ {
		obj, ok := s.Objects[cur]
		if !ok {
			return ""
		}
		if obj.Type == "commit" {
			return cur
		}
		if obj.Type == "tag" {
			m := tagTargetRegexp.FindStringSubmatch(string(obj.Data))
			if m == nil {
				return ""
			}
			cur = m[1]
			continue
		}
		return ""
	}
	return ""
}

// Peel a tag to the commit it points at (for peeled: lines).
func (s *Store) PeelToCommit(oid string) string {
	return s.ResolveToCommit(oid)
}

// -- repos ----------------------------------------------------------------

func (s *Store) RepoKey(owner, name string) string {
	return owner + "/" + name
}

func (s *Store) GetRepo(owner, name string) *Repo {
	if owner != s.ViewerLogin {
		return nil
	}
	return s.Repos[s.RepoKey(owner, name)]
}

func (s *Store) CreateRepo(name, description string, isPrivate bool) (*Repo, string) {
	if name == "" || strings.Contains(name, "/") {
		return nil, "invalid name"
	}
	key := s.RepoKey(s.ViewerLogin, name)
	if _, ok := s.Repos[key]; ok {
		return nil, "already exists"
	}
	repo := &Repo{
		ID:            s.repoIDCounter,
		Name:          name,
		Owner:         s.ViewerLogin,
		Private:       isPrivate,
		Description:   description,
		DefaultBranch: "main",
		Refs:          map[string]string{},
		Issues:        map[int]*Issue{},
		Pulls:         map[int]*PullRequest{},
		NextNumber:    1,
		Labels:        map[string]Label{},
		Seq:           s.seqCounter,
		PackCache:     map[string]FetchPackResult{},
		AdCache:       map[string][]byte{},
		LsRefsCache:   map[string][]byte{},
		FetchResponseCache: map[string][]byte{},
	}
	s.repoIDCounter++
	s.seqCounter++
	s.Repos[key] = repo
	s.ReposByID[repo.ID] = repo
	return repo, ""
}

func (s *Store) DeleteRepo(owner, name string) bool {
	if owner != s.ViewerLogin {
		return false
	}
	key := s.RepoKey(owner, name)
	repo, ok := s.Repos[key]
	if !ok {
		return false
	}
	delete(s.Repos, key)
	delete(s.ReposByID, repo.ID)
	return true
}

func (s *Store) ListViewerRepos() []*Repo {
	list := make([]*Repo, 0, len(s.Repos))
	for _, r := range s.Repos {
		list = append(list, r)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Seq < list[j].Seq })
	return list
}

// -- refs -----------------------------------------------------------------

type RefEntry struct {
	Name string
	OID  string
}

func (s *Store) ListRefs(repo *Repo) []RefEntry {
	out := make([]RefEntry, 0, len(repo.Refs))
	for name, oid := range repo.Refs {
		out = append(out, RefEntry{Name: name, OID: oid})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (s *Store) GetRef(repo *Repo, ref string) (string, bool) {
	oid, ok := repo.Refs[ref]
	return oid, ok
}

func (s *Store) SetRef(repo *Repo, ref, oid string) {
	repo.Refs[ref] = oid
	clearRefCaches(repo)
}

func (s *Store) DeleteRef(repo *Repo, ref string) bool {
	_, ok := repo.Refs[ref]
	if ok {
		delete(repo.Refs, ref)
		clearRefCaches(repo)
	}
	return ok
}

// -- commits & trees --------------------------------------------------------

// Ordered set of strings (insertion order preserved, like a JS Set).
type orderedSet struct {
	items []string
	seen  map[string]bool
}

func newOrderedSet() *orderedSet { return &orderedSet{seen: map[string]bool{}} }

func (s *orderedSet) add(v string) {
	if !s.seen[v] {
		s.seen[v] = true
		s.items = append(s.items, v)
	}
}

func (s *orderedSet) has(v string) bool { return s.seen[v] }

func (s *orderedSet) list() []string { return s.items }

// ListBranches returns {name, sha} for refs/heads/*, sorted by name.
func (s *Store) ListBranches(repo *Repo) []RefEntry {
	var out []RefEntry
	for name, oid := range repo.Refs {
		if strings.HasPrefix(name, "refs/heads/") {
			out = append(out, RefEntry{Name: strings.TrimPrefix(name, "refs/heads/"), OID: oid})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (s *Store) GetBranchSha(repo *Repo, branch string) (string, bool) {
	oid, ok := repo.Refs["refs/heads/"+branch]
	return oid, ok
}

// Resolve a refish: full ref, branch, tag, or a 40-hex object id.
func (s *Store) ResolveRefish(repo *Repo, refish string) (string, bool) {
	if strings.HasPrefix(refish, "refs/") {
		oid, ok := repo.Refs[refish]
		return oid, ok
	}
	if oid, ok := repo.Refs["refs/heads/"+refish]; ok {
		return oid, true
	}
	if oid, ok := repo.Refs["refs/tags/"+refish]; ok {
		return oid, true
	}
	if len(refish) == 40 && isHex40(refish) {
		if _, ok := s.Objects[refish]; ok {
			return refish, true
		}
	}
	return "", false
}

func isHex40(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// -- ancestry ---------------------------------------------------------------

func (s *Store) IsAncestor(ancestorOID, descendantOID string) bool {
	anc := s.ResolveToCommit(ancestorOID)
	desc := s.ResolveToCommit(descendantOID)
	if anc == "" || desc == "" {
		return false
	}
	if anc == desc {
		return true
	}
	seen := map[string]bool{desc: true}
	stack := []string{desc}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		info, ok := s.ParseCommit(cur)
		if !ok {
			continue
		}
		for _, p := range info.Parents {
			if p == anc {
				return true
			}
			if !seen[p] {
				seen[p] = true
				stack = append(stack, p)
			}
		}
	}
	return false
}

func (s *Store) AncestorsOf(oid string) map[string]bool {
	start := s.ResolveToCommit(oid)
	out := map[string]bool{}
	if start == "" {
		return out
	}
	stack := []string{start}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if out[cur] {
			continue
		}
		out[cur] = true
		info, ok := s.ParseCommit(cur)
		if !ok {
			continue
		}
		stack = append(stack, info.Parents...)
	}
	return out
}

func (s *Store) MergeBase(aOID, bOID string) string {
	aAnc := s.AncestorsOf(aOID)
	b := s.ResolveToCommit(bOID)
	if b == "" {
		return ""
	}
	stack := []string{b}
	seen := map[string]bool{}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if aAnc[cur] {
			return cur
		}
		if seen[cur] {
			continue
		}
		seen[cur] = true
		info, ok := s.ParseCommit(cur)
		if !ok {
			continue
		}
		stack = append(stack, info.Parents...)
	}
	return ""
}

type CommitEntry struct {
	Sha     string
	Message string
}

// ListCommits walks the ancestry of a refish (or the default branch) in
// tip-first DFS order. Returns nil when the refish cannot be resolved.
func (s *Store) ListCommits(repo *Repo, refish string) ([]CommitEntry, bool) {
	var tip string
	if refish != "" {
		t, ok := s.ResolveRefish(repo, refish)
		if !ok {
			return nil, false
		}
		tip = t
	} else {
		t, ok := repo.Refs["refs/heads/"+repo.DefaultBranch]
		if !ok {
			return []CommitEntry{}, true
		}
		tip = t
	}
	start := s.ResolveToCommit(tip)
	if start == "" {
		return []CommitEntry{}, true
	}
	var out []CommitEntry
	seen := map[string]bool{}
	stack := []string{start}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[cur] {
			continue
		}
		seen[cur] = true
		info, ok := s.ParseCommit(cur)
		if !ok {
			continue
		}
		out = append(out, CommitEntry{Sha: cur, Message: info.Message})
		for i := len(info.Parents) - 1; i >= 0; i-- {
			stack = append(stack, info.Parents[i])
		}
	}
	return out, true
}

// -- file maps ---------------------------------------------------------------

// FilesAtCommit resolves a commit's full file map (path -> oid/mode), cached.
func (s *Store) FilesAtCommit(commitOID string) map[string]FileRef {
	if m, ok := s.fileMapCache[commitOID]; ok {
		return m
	}
	out := map[string]FileRef{}
	if info, ok := s.ParseCommit(commitOID); ok {
		s.walkTreeFiles(info.Tree, "", out)
	}
	s.fileMapCache[commitOID] = out
	return out
}

func (s *Store) walkTreeFiles(treeOID, prefix string, out map[string]FileRef) {
	entries, ok := s.ParseTree(treeOID)
	if !ok {
		return
	}
	for _, e := range entries {
		p := e.Name
		if prefix != "" {
			p = prefix + "/" + e.Name
		}
		if e.IsDir {
			s.walkTreeFiles(e.OID, p, out)
		} else {
			out[p] = FileRef{OID: e.OID, Mode: e.Mode}
		}
	}
}

// Build a tree for a full file map (recursive, sorted by git's rules).
func (s *Store) BuildTreeForFiles(files map[string]FileRef) string {
	entries := []treeNode{}
	for path, ref := range files {
		entries = append(entries, treeNode{Path: path, OID: ref.OID, Mode: ref.Mode})
	}
	return s.buildTreeRecursive(entries)
}

type treeNode struct {
	Path string
	OID  string
	Mode string
}

func (s *Store) buildTreeRecursive(nodes []treeNode) string {
	dirs := map[string][]treeNode{}
	var direct []TreeEntry
	for _, n := range nodes {
		if idx := strings.Index(n.Path, "/"); idx >= 0 {
			dir := n.Path[:idx]
			dirs[dir] = append(dirs[dir], treeNode{Path: n.Path[idx+1:], OID: n.OID, Mode: n.Mode})
		} else {
			direct = append(direct, TreeEntry{Mode: n.Mode, Name: n.Path, OID: n.OID, IsDir: false})
		}
	}
	for dir, kids := range dirs {
		oid := s.buildTreeRecursive(kids)
		direct = append(direct, TreeEntry{Mode: "40000", Name: dir, OID: oid, IsDir: true})
	}
	sortTreeEntries(direct)
	return s.PutObject("tree", buildTree(direct))
}

// CommitFiles creates a commit on top of parentOid ("" = root commit)
// applying file changes (content nil = delete). Timestamps are always the
// fixed epoch. It does NOT move any ref — the caller does that.
func (s *Store) CommitFiles(parentOID string, changes map[string]*string, message string, epochSec int64) string {
	files := map[string]FileRef{}
	if parentOID != "" {
		base := s.ResolveToCommit(parentOID)
		if base == "" {
			base = parentOID
		}
		for p, r := range s.FilesAtCommit(base) {
			files[p] = r
		}
	}
	for path, content := range changes {
		if content == nil {
			delete(files, path)
		} else {
			oid := s.PutObject("blob", []byte(*content))
			files[path] = FileRef{OID: oid, Mode: "100644"}
		}
	}
	treeOID := s.BuildTreeForFiles(files)
	var parents []string
	if parentOID != "" {
		p := s.ResolveToCommit(parentOID)
		if p == "" {
			p = parentOID
		}
		parents = []string{p}
	}
	return s.PutObject("commit", buildCommit(treeOID, parents, message, epochSec))
}

// -- diffs -------------------------------------------------------------------

func lineDiff(oldText, newText string) (int, int) {
	oldLines := strings.Split(oldText, "\n")
	newLines := strings.Split(newText, "\n")
	dropEmpty := func(lines []string) []string {
		if len(lines) > 0 && lines[len(lines)-1] == "" {
			return lines[:len(lines)-1]
		}
		return lines
	}
	oldLines = dropEmpty(oldLines)
	newLines = dropEmpty(newLines)
	counts := map[string]int{}
	for _, l := range oldLines {
		counts[l]++
	}
	additions := 0
	for _, l := range newLines {
		if counts[l] > 0 {
			counts[l]--
		} else {
			additions++
		}
	}
	deletions := 0
	for _, c := range counts {
		deletions += c
	}
	return additions, deletions
}

// DiffFileMaps compares two file maps; sorted by path.
func (s *Store) DiffFileMaps(oldFiles, newFiles map[string]FileRef) []FileChange {
	paths := map[string]bool{}
	for p := range oldFiles {
		paths[p] = true
	}
	for p := range newFiles {
		paths[p] = true
	}
	var list []string
	for p := range paths {
		list = append(list, p)
	}
	sort.Strings(list)
	var changes []FileChange
	for _, path := range list {
		oldF, hasOld := oldFiles[path]
		newF, hasNew := newFiles[path]
		if !hasOld && hasNew {
			var text string
			if obj, ok := s.Objects[newF.OID]; ok {
				text = string(obj.Data)
			}
			add, _ := lineDiff("", text)
			changes = append(changes, FileChange{Path: path, Status: "added", Additions: add, Deletions: 0})
		} else if hasOld && !hasNew {
			var text string
			if obj, ok := s.Objects[oldF.OID]; ok {
				text = string(obj.Data)
			}
			_, del := lineDiff(text, "")
			changes = append(changes, FileChange{Path: path, Status: "removed", Additions: 0, Deletions: del})
		} else if oldF.OID != newF.OID {
			var oldText, newText string
			if obj, ok := s.Objects[oldF.OID]; ok {
				oldText = string(obj.Data)
			}
			if obj, ok := s.Objects[newF.OID]; ok {
				newText = string(obj.Data)
			}
			add, del := lineDiff(oldText, newText)
			changes = append(changes, FileChange{Path: path, Status: "modified", Additions: add, Deletions: del})
		}
	}
	return changes
}

// -- reachability / packs ----------------------------------------------------

type reachableResult struct {
	objects *orderedSet
	commits *orderedSet
}

func (s *Store) collectReachable(roots []string, maxDepth int, stopAt map[string]bool) reachableResult {
	objects := newOrderedSet()
	commits := newOrderedSet()
	var visitCommit func(oid string, depth int)
	visitCommit = func(oid string, depth int) {
		if commits.has(oid) {
			return
		}
		obj, ok := s.Objects[oid]
		if !ok {
			return
		}
		if obj.Type == "tag" {
			objects.add(oid)
			m := tagTargetRegexp.FindStringSubmatch(string(obj.Data))
			if m != nil {
				target := m[1]
				if tobj, ok := s.Objects[target]; ok {
					if tobj.Type == "commit" || tobj.Type == "tag" {
						visitCommit(target, depth)
					}
				}
			}
			return
		}
		if obj.Type != "commit" {
			return
		}
		commits.add(oid)
		objects.add(oid)
		info, ok := s.ParseCommit(oid)
		if !ok {
			return
		}
		s.collectTree(info.Tree, objects)
		if stopAt[oid] {
			return
		}
		if maxDepth >= 0 && depth+1 >= maxDepth {
			return
		}
		for _, p := range info.Parents {
			visitCommit(p, depth+1)
		}
	}
	for _, r := range roots {
		visitCommit(r, 0)
	}
	return reachableResult{objects: objects, commits: commits}
}

func (s *Store) collectTree(treeOID string, objects *orderedSet) {
	if objects.has(treeOID) {
		return
	}
	obj, ok := s.Objects[treeOID]
	if !ok || obj.Type != "tree" {
		return
	}
	objects.add(treeOID)
	entries, ok := s.ParseTree(treeOID)
	if !ok {
		return
	}
	for _, e := range entries {
		if e.IsDir {
			s.collectTree(e.OID, objects)
		} else {
			objects.add(e.OID)
		}
	}
}

// BuildFetchPack builds the pack for a fetch. depth < 0 means unlimited.
// Returns the pack (nil when nothing to send) plus shallow/unshallow lines.
func (s *Store) BuildFetchPack(repo *Repo, wants, haves, clientShallow []string, depth int) FetchPackResult {
	key := fetchPackKey(wants, haves, clientShallow, depth)
	if cached, ok := repo.PackCache[key]; ok {
		profCache("pack", true)
		return cached
	}
	profCache("pack", false)
	stopAt := map[string]bool{}
	for _, c := range clientShallow {
		stopAt[c] = true
	}
	held := s.collectReachable(haves, -1, stopAt)
	send := s.collectReachable(wants, depth, map[string]bool{})
	sendObjects := newOrderedSet()
	for _, oid := range send.objects.list() {
		if !held.objects.has(oid) {
			sendObjects.add(oid)
		}
	}
	var shallow []string
	for _, c := range send.commits.list() {
		info, ok := s.ParseCommit(c)
		if !ok {
			continue
		}
		parentMissing := false
		for _, p := range info.Parents {
			if !send.commits.has(p) && !held.commits.has(p) {
				parentMissing = true
				break
			}
		}
		if parentMissing {
			shallow = append(shallow, c)
		}
	}
	var unshallow []string
	for _, sc := range clientShallow {
		info, ok := s.ParseCommit(sc)
		if !ok {
			continue
		}
		all := true
		for _, p := range info.Parents {
			if !send.commits.has(p) && !held.commits.has(p) {
				all = false
				break
			}
		}
		if all {
			unshallow = append(unshallow, sc)
		}
	}
	if len(sendObjects.list()) == 0 {
		result := FetchPackResult{Pack: nil, Shallow: shallow, Unshallow: unshallow}
		repo.PackCache[key] = result
		return result
	}
	objs := map[string]GitObject{}
	for _, oid := range sendObjects.list() {
		if o, ok := s.Objects[oid]; ok {
			objs[oid] = o
		}
	}
	result := FetchPackResult{Pack: buildPack(objs), Shallow: shallow, Unshallow: unshallow}
	repo.PackCache[key] = result
	return result
}

// -- issues -----------------------------------------------------------------

func (s *Store) CreateIssue(repo *Repo, title, body string, labels []string) (*Issue, string) {
	for _, l := range labels {
		if _, ok := repo.Labels[l]; !ok {
			return nil, fmt.Sprintf("label %q does not exist", l)
		}
	}
	number := repo.NextNumber
	issue := &Issue{
		ID: s.objectIDCounter, Number: number, Title: title, Body: body, State: "open",
		Labels: append([]string(nil), labels...), Comments: []Comment{}, Seq: s.seqCounter,
	}
	s.objectIDCounter++
	repo.NextNumber++
	s.seqCounter++
	repo.Issues[number] = issue
	return issue, ""
}

func (s *Store) GetIssue(repo *Repo, number int) *Issue {
	return repo.Issues[number]
}

func (s *Store) ListIssues(repo *Repo, state, direction string) []*Issue {
	var list []*Issue
	for _, i := range repo.Issues {
		if state == "open" && i.State != "open" {
			continue
		}
		if state == "closed" && i.State != "closed" {
			continue
		}
		list = append(list, i)
	}
	dir := 1
	if direction == "desc" {
		dir = -1
	}
	sort.Slice(list, func(a, b int) bool {
		if dir == 1 {
			return list[a].Seq < list[b].Seq
		}
		return list[a].Seq > list[b].Seq
	})
	return list
}

func (s *Store) UpdateIssue(repo *Repo, number int, fields map[string]interface{}) *Issue {
	issue := repo.Issues[number]
	if issue == nil {
		return nil
	}
	if v, ok := fields["title"].(string); ok {
		issue.Title = v
	}
	if v, ok := fields["body"].(string); ok {
		issue.Body = v
	}
	if v, ok := fields["state"].(string); ok {
		issue.State = v
	}
	if v, ok := fields["labels"].([]string); ok {
		issue.Labels = v
	}
	issue.ViewCache = nil
	return issue
}

func (s *Store) CreateComment(repo *Repo, issueNumber int, body string) (*Comment, string) {
	issue := repo.Issues[issueNumber]
	if issue == nil {
		return nil, "issue not found"
	}
	comment := Comment{ID: s.objectIDCounter, Body: body}
	s.objectIDCounter++
	issue.Comments = append(issue.Comments, comment)
	issue.ViewCache = nil
	c := comment
	return &c, ""
}

// -- labels -----------------------------------------------------------------

func (s *Store) EnsureLabel(repo *Repo, name string) string {
	if _, ok := repo.Labels[name]; ok {
		return name
	}
	repo.Labels[name] = Label{Name: name, Color: "ededed"}
	return name
}

func (s *Store) CreateLabel(repo *Repo, name, color string) Label {
	label := Label{Name: name, Color: color}
	repo.Labels[name] = label
	return label
}

func (s *Store) ListLabels(repo *Repo) []Label {
	var list []Label
	for _, l := range repo.Labels {
		list = append(list, l)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	return list
}

// -- pull requests ----------------------------------------------------------

func (s *Store) CreatePull(repo *Repo, title, body, head, base string) (*PullRequest, string) {
	headSha, ok := s.GetBranchSha(repo, head)
	if !ok {
		return nil, "head or base branch not found"
	}
	baseSha, ok := s.GetBranchSha(repo, base)
	if !ok {
		return nil, "head or base branch not found"
	}
	for _, pr := range repo.Pulls {
		if pr.State == "open" && pr.HeadRef == head && pr.BaseRef == base {
			return nil, "a pull request already exists for this branch"
		}
	}
	number := repo.NextNumber
	pr := &PullRequest{
		ID: s.objectIDCounter, Number: number, Title: title, Body: body, State: "open",
		HeadRef: head, BaseRef: base, HeadSha: headSha, BaseSha: baseSha,
		Merged: false, Seq: s.seqCounter,
	}
	s.objectIDCounter++
	repo.NextNumber++
	s.seqCounter++
	repo.Pulls[number] = pr
	return pr, ""
}

func (s *Store) GetPull(repo *Repo, number int) *PullRequest {
	return repo.Pulls[number]
}

func (s *Store) ListPulls(repo *Repo, state string) []*PullRequest {
	var list []*PullRequest
	for _, p := range repo.Pulls {
		if state == "open" && p.State != "open" {
			continue
		}
		if state == "closed" && p.State != "closed" {
			continue
		}
		list = append(list, p)
	}
	sort.Slice(list, func(a, b int) bool { return list[a].Seq < list[b].Seq })
	return list
}

// currentBaseTip is the base branch's CURRENT tip (a PR's recorded baseSha
// goes stale once the base advances).
func (s *Store) currentBaseTip(repo *Repo, pr *PullRequest) string {
	if oid, ok := repo.Refs["refs/heads/"+pr.BaseRef]; ok {
		return oid
	}
	return pr.BaseSha
}

func resolveOrSelf(s *Store, oid string) string {
	if c := s.ResolveToCommit(oid); c != "" {
		return c
	}
	return oid
}

// PullChanges returns the files changed between the merge-base and the head tip.
func (s *Store) PullChanges(repo *Repo, pr *PullRequest) []FileChange {
	baseTip := s.currentBaseTip(repo, pr)
	base := s.MergeBase(pr.HeadSha, baseTip)
	baseCommit := base
	if baseCommit == "" {
		baseCommit = baseTip
	}
	headCommit := s.ResolveToCommit(pr.HeadSha)
	baseCommitOID := s.ResolveToCommit(baseCommit)
	if headCommit == "" || baseCommitOID == "" {
		return nil
	}
	return s.DiffFileMaps(s.FilesAtCommit(baseCommitOID), s.FilesAtCommit(headCommit))
}

// PullMergeable reports whether the head's changes can apply cleanly onto
// the CURRENT base tip (three-way conflict check).
func (s *Store) PullMergeable(repo *Repo, pr *PullRequest) bool {
	baseTip := s.currentBaseTip(repo, pr)
	base := s.MergeBase(pr.HeadSha, baseTip)
	if base == "" {
		return true
	}
	baseFiles := s.FilesAtCommit(resolveOrSelf(s, base))
	headFiles := s.FilesAtCommit(resolveOrSelf(s, pr.HeadSha))
	baseTipFiles := s.FilesAtCommit(resolveOrSelf(s, baseTip))
	headChanges := s.DiffFileMaps(baseFiles, headFiles)
	baseChanges := s.DiffFileMaps(baseFiles, baseTipFiles)
	baseChanged := map[string]string{}
	for _, c := range baseChanges {
		oid := ""
		if f, ok := baseTipFiles[c.Path]; ok {
			oid = f.OID
		}
		baseChanged[c.Path] = oid
	}
	for _, c := range headChanges {
		if _, ok := baseChanged[c.Path]; !ok {
			continue
		}
		headOID := ""
		if f, ok := headFiles[c.Path]; ok {
			headOID = f.OID
		}
		if baseChanged[c.Path] != headOID {
			return false
		}
	}
	return true
}

// MergePull applies the PR with a single-parent merge commit on the base tip.
func (s *Store) MergePull(repo *Repo, pr *PullRequest) (string, string) {
	if pr.State == "closed" {
		if pr.Merged {
			return "", "already merged"
		}
		return "", "already closed"
	}
	if !s.PullMergeable(repo, pr) {
		return "", "conflict"
	}
	baseTip := s.currentBaseTip(repo, pr)
	base := s.MergeBase(pr.HeadSha, baseTip)
	mergeBaseOID := base
	if mergeBaseOID == "" {
		mergeBaseOID = baseTip
	}
	baseFiles := s.FilesAtCommit(resolveOrSelf(s, mergeBaseOID))
	headFiles := s.FilesAtCommit(resolveOrSelf(s, pr.HeadSha))
	headChanges := s.DiffFileMaps(baseFiles, headFiles)
	mergedFiles := map[string]FileRef{}
	for p, f := range s.FilesAtCommit(resolveOrSelf(s, baseTip)) {
		mergedFiles[p] = f
	}
	for _, c := range headChanges {
		if c.Status == "removed" {
			delete(mergedFiles, c.Path)
		} else if hf, ok := headFiles[c.Path]; ok {
			mergedFiles[c.Path] = hf
		}
	}
	treeOID := s.BuildTreeForFiles(mergedFiles)
	baseTipCommit := resolveOrSelf(s, baseTip)
	message := fmt.Sprintf("Merge pull request #%d from %s", pr.Number, pr.HeadRef)
	commitOID := s.PutObject("commit", buildCommit(treeOID, []string{baseTipCommit}, message, FixedEpochSec))
	repo.Refs["refs/heads/"+pr.BaseRef] = commitOID
	clearRefCaches(repo)
	pr.State = "closed"
	pr.Merged = true
	mergedAt := FixedTS
	pr.MergedAt = &mergedAt
	pr.MergeSha = &commitOID
	return commitOID, ""
}

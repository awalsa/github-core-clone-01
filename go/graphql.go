// graphql.go — a small GraphQL executor for the fixed document set the
// suite sends. It parses queries, mutations, variables, aliases, inline
// fragments, input objects and lists, and resolves them against the same
// store the REST and git surfaces use. Unknown fields answer HTTP 200 with
// an errors array naming the field, exactly as GitHub does.

package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// Ordered JSON: field order follows the document's selection order, like
// Node's object literals do.
// ---------------------------------------------------------------------------

type KV struct {
	Key string
	Val interface{}
}

type OMap []KV

func (m OMap) Get(key string) (interface{}, bool) {
	for _, kv := range m {
		if kv.Key == key {
			return kv.Val, true
		}
	}
	return nil, false
}

// marshalOMap renders an OMap (and nested values) as JSON, with the same
// escaping rules as encoding/json with SetEscapeHTML(false).
func marshalOMap(v interface{}) []byte {
	var b strings.Builder
	writeJSONValue(&b, v)
	return []byte(b.String())
}

func writeJSONValue(b *strings.Builder, v interface{}) {
	switch t := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		if t {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case string:
		writeJSONString(b, t)
	case int:
		b.WriteString(strconv.Itoa(t))
	case int64:
		b.WriteString(strconv.FormatInt(t, 10))
	case float64:
		b.WriteString(jsFloat(t))
	case OMap:
		b.WriteByte('{')
		for i, kv := range t {
			if i > 0 {
				b.WriteByte(',')
			}
			writeJSONString(b, kv.Key)
			b.WriteByte(':')
			writeJSONValue(b, kv.Val)
		}
		b.WriteByte('}')
	case []interface{}:
		b.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				b.WriteByte(',')
			}
			writeJSONValue(b, e)
		}
		b.WriteByte(']')
	case *gqlNode:
		writeJSONValue(b, t.toOMap())
	case map[string]interface{}:
		// Fallback: sorted keys (only used for values that never reach output
		// in practice).
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			writeJSONString(b, k)
			b.WriteByte(':')
			writeJSONValue(b, t[k])
		}
		b.WriteByte('}')
	default:
		b.WriteString(fmt.Sprintf("%v", t))
	}
}

// jsFloat formats a float the way JS JSON.stringify does: plain decimal
// for 1e-6 <= |f| < 1e21, exponent otherwise (with a one-digit exponent),
// -0 as "0", NaN/Infinity as null.
func jsFloat(f float64) string {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return "null"
	}
	if f == 0 {
		return "0"
	}
	abs := math.Abs(f)
	if abs < 1e-6 || abs >= 1e21 {
		s := strconv.FormatFloat(f, 'e', -1, 64)
		// Go writes "1e-07"; JS writes "1e-7".
		if idx := strings.IndexByte(s, 'e'); idx >= 0 {
			mant, exp := s[:idx], s[idx+1:]
			sign := ""
			if strings.HasPrefix(exp, "+") || strings.HasPrefix(exp, "-") {
				sign = exp[:1]
				exp = exp[1:]
			}
			exp = strings.TrimLeft(exp, "0")
			if exp == "" {
				exp = "0"
			}
			s = mant + "e" + sign + exp
		}
		return s
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}

func writeJSONString(b *strings.Builder, s string) {
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString("\\\"")
		case '\\':
			b.WriteString("\\\\")
		case '\n':
			b.WriteString("\\n")
		case '\r':
			b.WriteString("\\r")
		case '\t':
			b.WriteString("\\t")
		case '\b':
			b.WriteString("\\b")
		case '\f':
			b.WriteString("\\f")
		default:
			if r < 0x20 {
				b.WriteString(fmt.Sprintf("\\u%04x", r))
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
}

// ---------------------------------------------------------------------------
// Tokenizer + parser (subset of GraphQL: enough for the suite's documents)
// ---------------------------------------------------------------------------

type tokenKind int

const (
	tokName tokenKind = iota
	tokInt
	tokFloat
	tokString
	tokPunct
)

type token struct {
	kind  tokenKind
	value string
}

func tokenize(src string) ([]token, error) {
	var tokens []token
	i := 0
	for i < len(src) {
		c := src[i]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ',' {
			i++
			continue
		}
		if c == '#' {
			for i < len(src) && src[i] != '\n' {
				i++
			}
			continue
		}
		if c == '.' && i+3 <= len(src) && src[i:i+3] == "..." {
			tokens = append(tokens, token{kind: tokPunct, value: "..."})
			i += 3
			continue
		}
		if strings.ContainsRune("{}():$![]=", rune(c)) {
			tokens = append(tokens, token{kind: tokPunct, value: string(c)})
			i++
			continue
		}
		if c == '"' {
			j := i + 1
			var out strings.Builder
			for j < len(src) && src[j] != '"' {
				if src[j] == '\\' && j+1 < len(src) {
					esc := src[j+1]
					switch esc {
					case 'n':
						out.WriteByte('\n')
					case 't':
						out.WriteByte('\t')
					case 'r':
						out.WriteByte('\r')
					default:
						out.WriteByte(esc)
					}
					j += 2
				} else {
					out.WriteByte(src[j])
					j++
				}
			}
			tokens = append(tokens, token{kind: tokString, value: out.String()})
			i = j + 1
			continue
		}
		if c >= '0' && c <= '9' || c == '-' {
			j := i
			for j < len(src) && (src[j] >= '0' && src[j] <= '9' || src[j] == '.') {
				j++
			}
			text := src[i:j]
			kind := tokInt
			if strings.Contains(text, ".") {
				kind = tokFloat
			}
			tokens = append(tokens, token{kind: kind, value: text})
			i = j
			continue
		}
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c == '_' {
			j := i
			for j < len(src) && (src[j] >= 'A' && src[j] <= 'Z' || src[j] >= 'a' && src[j] <= 'z' || src[j] >= '0' && src[j] <= '9' || src[j] == '_') {
				j++
			}
			tokens = append(tokens, token{kind: tokName, value: src[i:j]})
			i = j
			continue
		}
		return nil, fmt.Errorf("unexpected character %c", c)
	}
	return tokens, nil
}

type valueNode struct {
	kind   string // variable|int|float|string|boolean|null|enum|list|object
	name   string
	num    float64
	str    string
	b      bool
	values []*valueNode
	fields []valueField
}

type valueField struct {
	name  string
	value *valueNode
}

type fieldNode struct {
	name       string
	alias      string
	args       []argNode
	selections []selectionNode
}

type argNode struct {
	name  string
	value *valueNode
}

type fragmentNode struct {
	typeCondition string
	selections    []selectionNode
}

type selectionNode struct {
	isFragment bool
	field      *fieldNode
	fragment   *fragmentNode
}

type operation struct {
	typ        string // query | mutation
	name       string
	varDefs    []string
	selections []selectionNode
}

type parser struct {
	tokens []token
	pos    int
}

func (p *parser) peek() *token {
	if p.pos < len(p.tokens) {
		return &p.tokens[p.pos]
	}
	return nil
}

func (p *parser) next() (token, error) {
	t := p.peek()
	if t == nil {
		return token{}, fmt.Errorf("unexpected end of document")
	}
	p.pos++
	return *t, nil
}

func (p *parser) expectPunct(v string) error {
	t, err := p.next()
	if err != nil {
		return err
	}
	if t.kind != tokPunct || t.value != v {
		return fmt.Errorf("expected %s, got %s", v, t.value)
	}
	return nil
}

func (p *parser) acceptPunct(v string) bool {
	t := p.peek()
	if t != nil && t.kind == tokPunct && t.value == v {
		p.pos++
		return true
	}
	return false
}

func (p *parser) parseDocument() ([]*operation, error) {
	var ops []*operation
	for p.pos < len(p.tokens) {
		op, err := p.parseOperation()
		if err != nil {
			return nil, err
		}
		ops = append(ops, op)
	}
	return ops, nil
}

func (p *parser) parseOperation() (*operation, error) {
	typ := "query"
	name := ""
	t := p.peek()
	if t != nil && t.kind == tokName && (t.value == "query" || t.value == "mutation") {
		typ = t.value
		p.pos++
		if n := p.peek(); n != nil && n.kind == tokName {
			name = n.value
			p.pos++
		}
	}
	var varDefs []string
	if p.acceptPunct("(") {
		for !p.acceptPunct(")") {
			if err := p.expectPunct("$"); err != nil {
				return nil, err
			}
			vname, err := p.next()
			if err != nil {
				return nil, err
			}
			if err := p.expectPunct(":"); err != nil {
				return nil, err
			}
			if err := p.parseType(); err != nil {
				return nil, err
			}
			if p.acceptPunct("=") {
				if _, err := p.parseValue(); err != nil {
					return nil, err
				}
			}
			varDefs = append(varDefs, vname.value)
		}
	}
	sels, err := p.parseSelectionSet()
	if err != nil {
		return nil, err
	}
	return &operation{typ: typ, name: name, varDefs: varDefs, selections: sels}, nil
}

func (p *parser) parseType() error {
	t := p.peek()
	if t != nil && t.kind == tokPunct && t.value == "[" {
		p.pos++
		if err := p.parseType(); err != nil {
			return err
		}
		if err := p.expectPunct("]"); err != nil {
			return err
		}
	} else {
		if _, err := p.next(); err != nil {
			return err
		}
	}
	p.acceptPunct("!")
	return nil
}

func (p *parser) parseSelectionSet() ([]selectionNode, error) {
	if err := p.expectPunct("{"); err != nil {
		return nil, err
	}
	var out []selectionNode
	for !p.acceptPunct("}") {
		sel, err := p.parseSelection()
		if err != nil {
			return nil, err
		}
		out = append(out, sel)
	}
	return out, nil
}

func (p *parser) parseSelection() (selectionNode, error) {
	t := p.peek()
	if t != nil && t.kind == tokPunct && t.value == "..." {
		p.pos++
		typeCondition := ""
		if n := p.peek(); n != nil && n.kind == tokName && n.value == "on" {
			p.pos++
			tn, err := p.next()
			if err != nil {
				return selectionNode{}, err
			}
			typeCondition = tn.value
		}
		sels, err := p.parseSelectionSet()
		if err != nil {
			return selectionNode{}, err
		}
		return selectionNode{isFragment: true, fragment: &fragmentNode{typeCondition: typeCondition, selections: sels}}, nil
	}
	first, err := p.next()
	if err != nil {
		return selectionNode{}, err
	}
	if first.kind != tokName {
		return selectionNode{}, fmt.Errorf("expected field name")
	}
	alias := ""
	name := first.value
	if p.acceptPunct(":") {
		alias = name
		nm, err := p.next()
		if err != nil {
			return selectionNode{}, err
		}
		name = nm.value
	}
	var args []argNode
	if p.acceptPunct("(") {
		for !p.acceptPunct(")") {
			argName, err := p.next()
			if err != nil {
				return selectionNode{}, err
			}
			if err := p.expectPunct(":"); err != nil {
				return selectionNode{}, err
			}
			value, err := p.parseValue()
			if err != nil {
				return selectionNode{}, err
			}
			args = append(args, argNode{name: argName.value, value: value})
		}
	}
	var sels []selectionNode
	if t := p.peek(); t != nil && t.kind == tokPunct && t.value == "{" {
		sels, err = p.parseSelectionSet()
		if err != nil {
			return selectionNode{}, err
		}
	}
	return selectionNode{field: &fieldNode{name: name, alias: alias, args: args, selections: sels}}, nil
}

func (p *parser) parseValue() (*valueNode, error) {
	t, err := p.next()
	if err != nil {
		return nil, err
	}
	if t.kind == tokPunct && t.value == "$" {
		nm, err := p.next()
		if err != nil {
			return nil, err
		}
		return &valueNode{kind: "variable", name: nm.value}, nil
	}
	if t.kind == tokPunct && t.value == "[" {
		var values []*valueNode
		for !p.acceptPunct("]") {
			v, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			values = append(values, v)
		}
		return &valueNode{kind: "list", values: values}, nil
	}
	if t.kind == tokPunct && t.value == "{" {
		var fields []valueField
		for !p.acceptPunct("}") {
			fname, err := p.next()
			if err != nil {
				return nil, err
			}
			if err := p.expectPunct(":"); err != nil {
				return nil, err
			}
			v, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			fields = append(fields, valueField{name: fname.value, value: v})
		}
		return &valueNode{kind: "object", fields: fields}, nil
	}
	if t.kind == tokInt {
		n, _ := strconv.ParseInt(t.value, 10, 64)
		return &valueNode{kind: "int", num: float64(n)}, nil
	}
	if t.kind == tokFloat {
		n, _ := strconv.ParseFloat(t.value, 64)
		return &valueNode{kind: "float", num: n}, nil
	}
	if t.kind == tokString {
		return &valueNode{kind: "string", str: t.value}, nil
	}
	if t.kind == tokName {
		if t.value == "true" {
			return &valueNode{kind: "boolean", b: true}, nil
		}
		if t.value == "false" {
			return &valueNode{kind: "boolean", b: false}, nil
		}
		if t.value == "null" {
			return &valueNode{kind: "null"}, nil
		}
		return &valueNode{kind: "enum", str: t.value}, nil
	}
	return nil, fmt.Errorf("unexpected value token %s", t.value)
}

func resolveValue(node *valueNode, variables map[string]interface{}) interface{} {
	switch node.kind {
	case "variable":
		return variables[node.name]
	case "int", "float":
		return node.num
	case "string", "enum":
		return node.str
	case "boolean":
		return node.b
	case "null":
		return nil
	case "list":
		out := make([]interface{}, 0, len(node.values))
		for _, v := range node.values {
			out = append(out, resolveValue(v, variables))
		}
		return out
	case "object":
		out := map[string]interface{}{}
		for _, f := range node.fields {
			out[f.name] = resolveValue(f.value, variables)
		}
		return out
	}
	return nil
}

// ---------------------------------------------------------------------------
// Global ids (opaque, stable, never the plain database id)
// ---------------------------------------------------------------------------

func encodeGlobalID(kind string, parts ...interface{}) string {
	strs := []string{kind}
	for _, p := range parts {
		strs = append(strs, fmt.Sprintf("%v", p))
	}
	return base64.RawURLEncoding.EncodeToString([]byte(strings.Join(strs, ":")))
}

func sanitizeB64URL(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_' {
			b.WriteByte(c)
		}
	}
	return b.String()
}

func decodeGlobalID(id string) (kind string, parts []string, ok bool) {
	data, err := base64.RawURLEncoding.DecodeString(sanitizeB64URL(id))
	if err != nil {
		// Node's Buffer.from is lenient; try padded standard decoding too.
		data2, err2 := base64.URLEncoding.DecodeString(sanitizeB64URL(id))
		if err2 != nil {
			return "", nil, false
		}
		data = data2
	}
	ps := strings.Split(string(data), ":")
	if len(ps) < 2 {
		return "", nil, false
	}
	return ps[0], ps[1:], true
}

// ---------------------------------------------------------------------------
// Schema table for __type introspection
// ---------------------------------------------------------------------------

type schemaInfo struct {
	fields     []string
	enumValues []string
}

var schemaTable = map[string]schemaInfo{
	"Issue": {
		fields: []string{"id", "number", "title", "state", "body", "createdAt", "updatedAt", "author", "labels", "comments", "url", "repository"},
	},
	"PullRequest": {
		fields: []string{"id", "number", "title", "body", "state", "merged", "mergeable", "headRefName", "baseRefName", "headRefOid", "baseRefOid", "files", "author", "createdAt", "updatedAt", "mergedAt", "mergeCommit", "url", "repository"},
	},
	"Repository": {
		fields: []string{"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequests", "pullRequest", "description", "private", "url", "createdAt", "updatedAt"},
	},
	"SearchType": {
		enumValues: []string{"ISSUE", "PULL_REQUEST", "REPOSITORY", "USER", "DISCUSSION", "COMMIT", "CODE"},
	},
	"User": {
		fields: []string{"login", "id", "name", "email", "url"},
	},
}

func encodeCursor(index int) string {
	return base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf("cursor:%d", index)))
}

var cursorRegexp = regexp.MustCompile(`^cursor:(\d+)$`)

func decodeCursor(cursor string) (int, bool) {
	data, err := base64.RawURLEncoding.DecodeString(sanitizeB64URL(cursor))
	if err != nil {
		return 0, false
	}
	m := cursorRegexp.FindStringSubmatch(string(data))
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, false
	}
	return n, true
}

// ---------------------------------------------------------------------------
// Executor
// ---------------------------------------------------------------------------

type gqlCtx struct {
	store     *Store
	variables map[string]interface{}
	errors    []OMap
}

// gqlNode is one resolved object; payload holds the __-prefixed data the
// resolver needs (repo, issue, pr, login, ...).
type gqlNode struct {
	typename string
	payload  map[string]interface{}
}

func (n *gqlNode) toOMap() OMap {
	out := OMap{{Key: "__typename", Val: n.typename}}
	// Scalar payloads leak into the output when a node is returned without
	// sub-selections (mirroring Node's JSON.stringify of the value object).
	keys := make([]string, 0, len(n.payload))
	for k := range n.payload {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := n.payload[k]
		switch t := v.(type) {
		case string, int, bool:
			out = append(out, KV{Key: k, Val: t})
		case *Comment:
			out = append(out, KV{Key: k, Val: OMap{{Key: "id", Val: t.ID}, {Key: "body", Val: t.Body}}})
		}
	}
	return out
}

func repoNode(repo *Repo) *gqlNode {
	return &gqlNode{typename: "Repository", payload: map[string]interface{}{"__repo": repo}}
}

func issueNode(repo *Repo, issue *Issue) *gqlNode {
	return &gqlNode{typename: "Issue", payload: map[string]interface{}{"__repo": repo, "__issue": issue}}
}

func pullNode(repo *Repo, pr *PullRequest) *gqlNode {
	return &gqlNode{typename: "PullRequest", payload: map[string]interface{}{"__repo": repo, "__pr": pr}}
}

func userNode(login string) *gqlNode {
	return &gqlNode{typename: "User", payload: map[string]interface{}{"__login": login}}
}

func issueState(issue *Issue) string {
	if issue.State == "open" {
		return "OPEN"
	}
	return "CLOSED"
}

func pullState(pr *PullRequest) string {
	if pr.Merged {
		return "MERGED"
	}
	if pr.State == "open" {
		return "OPEN"
	}
	return "CLOSED"
}

func executeSelections(parentType string, parent *gqlNode, selections []selectionNode, ctx *gqlCtx) OMap {
	if parent == nil {
		return nil
	}
	out := OMap{}
	for _, sel := range selections {
		if sel.isFragment {
			if sel.fragment.typeCondition != "" && sel.fragment.typeCondition != parentType && sel.fragment.typeCondition != parent.typename {
				continue
			}
			sub := executeSelections(parentType, parent, sel.fragment.selections, ctx)
			out = append(out, sub...)
			continue
		}
		key := sel.field.name
		if sel.field.alias != "" {
			key = sel.field.alias
		}
		value, known := resolveField(parentType, parent, sel.field, ctx)
		if !known {
			ctx.errors = append(ctx.errors, OMap{{Key: "message", Val: fmt.Sprintf("Cannot query field %q on type %q.", sel.field.name, parentType)}})
			out = append(out, KV{Key: key, Val: nil})
			continue
		}
		out = append(out, KV{Key: key, Val: applySelections(sel.field, value, ctx)})
	}
	return out
}

func applySelections(sel *fieldNode, value interface{}, ctx *gqlCtx) interface{} {
	if value == nil {
		return nil
	}
	if len(sel.selections) == 0 {
		return value
	}
	if arr, ok := value.([]interface{}); ok {
		out := make([]interface{}, 0, len(arr))
		for _, v := range arr {
			if node, ok := v.(*gqlNode); ok {
				out = append(out, executeSelections(node.typename, node, sel.selections, ctx))
			} else {
				out = append(out, v)
			}
		}
		return out
	}
	if node, ok := value.(*gqlNode); ok {
		return executeSelections(node.typename, node, sel.selections, ctx)
	}
	return value
}

func argsOf(sel *fieldNode, ctx *gqlCtx) map[string]interface{} {
	out := map[string]interface{}{}
	for _, a := range sel.args {
		out[a.name] = resolveValue(a.value, ctx.variables)
	}
	return out
}

// toIntArg converts a GraphQL argument to an int the way JS Number()/parseInt
// do for the lookups below (strings parsed, floats truncated when integral).
func toIntArg(v interface{}) (int, bool) {
	switch t := v.(type) {
	case float64:
		if t == float64(int(t)) {
			return int(t), true
		}
		return 0, false
	case int:
		return t, true
	case string:
		n, ok := jsParseInt(t)
		if !ok {
			return 0, false
		}
		return n, true
	}
	return 0, false
}

func resolveField(parentType string, parent *gqlNode, sel *fieldNode, ctx *gqlCtx) (interface{}, bool) {
	store := ctx.store
	args := argsOf(sel, ctx)
	var repo *Repo
	var issue *Issue
	var pr *PullRequest
	if parent != nil {
		if r, ok := parent.payload["__repo"].(*Repo); ok {
			repo = r
		}
		if i, ok := parent.payload["__issue"].(*Issue); ok {
			issue = i
		}
		if p, ok := parent.payload["__pr"].(*PullRequest); ok {
			pr = p
		}
	}
	switch parentType {
	case "Query":
		switch sel.name {
		case "viewer":
			return userNode(store.ViewerLogin), true
		case "repository":
			owner, _ := args["owner"].(string)
			name, _ := args["name"].(string)
			r := store.GetRepo(owner, name)
			if r == nil {
				return nil, true
			}
			return repoNode(r), true
		case "node":
			id, _ := args["id"].(string)
			kind, parts, ok := decodeGlobalID(id)
			if !ok {
				return nil, true
			}
			rid, ok1 := toIntArg(parts[0])
			if !ok1 {
				return nil, true
			}
			switch kind {
			case "Repository":
				r := store.ReposByID[rid]
				if r == nil {
					return nil, true
				}
				return repoNode(r), true
			case "Issue":
				if len(parts) < 2 {
					return nil, true
				}
				num, ok2 := toIntArg(parts[1])
				if !ok2 {
					return nil, true
				}
				r := store.ReposByID[rid]
				if r == nil {
					return nil, true
				}
				i := r.Issues[num]
				if i == nil {
					return nil, true
				}
				return issueNode(r, i), true
			case "PullRequest":
				if len(parts) < 2 {
					return nil, true
				}
				num, ok2 := toIntArg(parts[1])
				if !ok2 {
					return nil, true
				}
				r := store.ReposByID[rid]
				if r == nil {
					return nil, true
				}
				p := r.Pulls[num]
				if p == nil {
					return nil, true
				}
				return pullNode(r, p), true
			}
			return nil, true
		case "__type":
			name, _ := args["name"].(string)
			info, ok := schemaTable[name]
			if !ok {
				return nil, true
			}
			return &gqlNode{typename: "__Type", payload: map[string]interface{}{"__typeName": name, "__fields": info.fields, "__enumValues": info.enumValues}}, true
		}
		return nil, false
	case "Mutation":
		switch sel.name {
		case "createIssue":
			input, _ := args["input"].(map[string]interface{})
			rid, _ := input["repositoryId"].(string)
			kind, parts, ok := decodeGlobalID(rid)
			var r *Repo
			if ok && kind == "Repository" {
				if n, ok2 := toIntArg(parts[0]); ok2 {
					r = store.ReposByID[n]
				}
			}
			if r == nil {
				return nil, true
			}
			title, _ := input["title"].(string)
			body, _ := input["body"].(string)
			created, _ := store.CreateIssue(r, title, body, []string{})
			if created == nil {
				return nil, true
			}
			return &gqlNode{typename: "CreateIssuePayload", payload: map[string]interface{}{"__issue": created, "__repo": r}}, true
		case "closeIssue":
			input, _ := args["input"].(map[string]interface{})
			iid, _ := input["issueId"].(string)
			kind, parts, ok := decodeGlobalID(iid)
			var r *Repo
			var i *Issue
			if ok && kind == "Issue" && len(parts) >= 2 {
				if n, ok2 := toIntArg(parts[0]); ok2 {
					r = store.ReposByID[n]
				}
				if r != nil {
					if num, ok2 := toIntArg(parts[1]); ok2 {
						i = r.Issues[num]
					}
				}
			}
			if r == nil || i == nil {
				return nil, true
			}
			i.State = "closed"
			return &gqlNode{typename: "CloseIssuePayload", payload: map[string]interface{}{"__issue": i, "__repo": r}}, true
		case "createPullRequest":
			input, _ := args["input"].(map[string]interface{})
			rid, _ := input["repositoryId"].(string)
			kind, parts, ok := decodeGlobalID(rid)
			var r *Repo
			if ok && kind == "Repository" {
				if n, ok2 := toIntArg(parts[0]); ok2 {
					r = store.ReposByID[n]
				}
			}
			if r == nil {
				return nil, true
			}
			title, _ := input["title"].(string)
			head, _ := input["headRefName"].(string)
			base, _ := input["baseRefName"].(string)
			created, errStr := store.CreatePull(r, title, "", head, base)
			if errStr != "" || created == nil {
				return nil, true
			}
			return &gqlNode{typename: "CreatePullRequestPayload", payload: map[string]interface{}{"__pr": created, "__repo": r}}, true
		case "mergePullRequest":
			input, _ := args["input"].(map[string]interface{})
			pid, _ := input["pullRequestId"].(string)
			kind, parts, ok := decodeGlobalID(pid)
			var r *Repo
			var p *PullRequest
			if ok && kind == "PullRequest" && len(parts) >= 2 {
				if n, ok2 := toIntArg(parts[0]); ok2 {
					r = store.ReposByID[n]
				}
				if r != nil {
					if num, ok2 := toIntArg(parts[1]); ok2 {
						p = r.Pulls[num]
					}
				}
			}
			if r == nil || p == nil {
				return nil, true
			}
			store.MergePull(r, p)
			return &gqlNode{typename: "MergePullRequestPayload", payload: map[string]interface{}{"__pr": p, "__repo": r}}, true
		case "addComment":
			input, _ := args["input"].(map[string]interface{})
			sid, _ := input["subjectId"].(string)
			kind, parts, ok := decodeGlobalID(sid)
			var r *Repo
			var i *Issue
			if ok && kind == "Issue" && len(parts) >= 2 {
				if n, ok2 := toIntArg(parts[0]); ok2 {
					r = store.ReposByID[n]
				}
				if r != nil {
					if num, ok2 := toIntArg(parts[1]); ok2 {
						i = r.Issues[num]
					}
				}
			}
			if r == nil || i == nil {
				return nil, true
			}
			body, _ := input["body"].(string)
			comment, _ := store.CreateComment(r, i.Number, body)
			if comment == nil {
				return nil, true
			}
			edge := &gqlNode{typename: "IssueCommentEdge", payload: map[string]interface{}{"__node": &gqlNode{typename: "IssueComment", payload: map[string]interface{}{"__comment": comment}}}}
			return &gqlNode{typename: "AddCommentPayload", payload: map[string]interface{}{"__commentEdge": edge}}, true
		}
		return nil, false
	case "Repository":
		if repo == nil {
			return nil, false
		}
		switch sel.name {
		case "id":
			return encodeGlobalID("Repository", repo.ID), true
		case "databaseId":
			return repo.ID, true
		case "name":
			return repo.Name, true
		case "nameWithOwner":
			return repo.Owner + "/" + repo.Name, true
		case "owner":
			return userNode(repo.Owner), true
		case "defaultBranchRef":
			sha, ok := repo.Refs["refs/heads/"+repo.DefaultBranch]
			if !ok {
				return nil, true
			}
			_ = sha
			return &gqlNode{typename: "Ref", payload: map[string]interface{}{"__refName": repo.DefaultBranch}}, true
		case "viewerPermission":
			return "ADMIN", true
		case "hasIssuesEnabled":
			return true, true
		case "description":
			return repo.Description, true
		case "private":
			return repo.Private, true
		case "issue":
			num, ok := toIntArg(args["number"])
			if !ok {
				return nil, true
			}
			i := repo.Issues[num]
			if i == nil {
				return nil, true
			}
			return issueNode(repo, i), true
		case "issues":
			first := 30
			if f, ok := args["first"].(float64); ok {
				first = int(f)
			}
			var states []string
			if arr, ok := args["states"].([]interface{}); ok {
				for _, v := range arr {
					if s, ok := v.(string); ok {
						states = append(states, s)
					}
				}
			}
			orderBy, _ := args["orderBy"].(map[string]interface{})
			after, _ := args["after"].(string)
			list := make([]*Issue, 0, len(repo.Issues))
			for _, i := range repo.Issues {
				list = append(list, i)
			}
			// Map iteration is random in Go; the Node original iterates in
			// insertion (creation) order, which is seq order here.
			sort.Slice(list, func(a, b int) bool { return list[a].Seq < list[b].Seq })
			if states != nil {
				wanted := map[string]bool{}
				for _, s := range states {
					if s == "OPEN" {
						wanted["open"] = true
					} else {
						wanted["closed"] = true
					}
				}
				filtered := list[:0]
				for _, i := range list {
					if wanted[i.State] {
						filtered = append(filtered, i)
					}
				}
				list = filtered
			}
			dir := -1
			if d, ok := orderBy["direction"].(string); ok && d == "ASC" {
				dir = 1
			}
			field := "CREATED_AT"
			if f, ok := orderBy["field"].(string); ok {
				field = f
			}
			sort.SliceStable(list, func(a, b int) bool {
				if field == "COMMENTS" {
					d := (len(list[a].Comments) - len(list[b].Comments)) * dir
					if d != 0 {
						return d < 0
					}
					return (list[a].Seq-list[b].Seq)*dir < 0
				}
				return (list[a].Seq-list[b].Seq)*dir < 0
			})
			start := 0
			if after != "" {
				if n, ok := decodeCursor(after); ok {
					start = n + 1
				} else {
					start = 0
				}
			}
			if start < 0 {
				start = 0
			}
			if start > len(list) {
				start = len(list)
			}
			end := start + first
			if end > len(list) {
				end = len(list)
			}
			nodes := list[start:end]
			var endCursor interface{}
			if len(nodes) > 0 {
				endCursor = encodeCursor(start + len(nodes) - 1)
			}
			nodeVals := make([]interface{}, 0, len(nodes))
			for _, i := range nodes {
				nodeVals = append(nodeVals, issueNode(repo, i))
			}
			pageInfo := &gqlNode{typename: "PageInfo", payload: map[string]interface{}{"__hasNextPage": start + len(nodes) < len(list), "__endCursor": endCursor}}
			return &gqlNode{typename: "IssueConnection", payload: map[string]interface{}{"__nodes": nodeVals, "__pageInfo": pageInfo}}, true
		case "pullRequests":
			first := 30
			if f, ok := args["first"].(float64); ok {
				first = int(f)
			}
			list := make([]*PullRequest, 0, len(repo.Pulls))
			for _, p := range repo.Pulls {
				list = append(list, p)
			}
			sort.Slice(list, func(a, b int) bool { return list[a].Seq < list[b].Seq })
			if first < len(list) {
				list = list[:first]
			}
			nodeVals := make([]interface{}, 0, len(list))
			for _, p := range list {
				nodeVals = append(nodeVals, pullNode(repo, p))
			}
			return &gqlNode{typename: "PullRequestConnection", payload: map[string]interface{}{"__nodes": nodeVals}}, true
		case "pullRequest":
			num, ok := toIntArg(args["number"])
			if !ok {
				return nil, true
			}
			p := repo.Pulls[num]
			if p == nil {
				return nil, true
			}
			return pullNode(repo, p), true
		}
		return nil, false
	case "Issue":
		if issue == nil {
			return nil, false
		}
		switch sel.name {
		case "id":
			return encodeGlobalID("Issue", repo.ID, issue.Number), true
		case "number":
			return issue.Number, true
		case "title":
			return issue.Title, true
		case "state":
			return issueState(issue), true
		case "body":
			return issue.Body, true
		case "createdAt", "updatedAt":
			return FixedTS, true
		}
		return nil, false
	case "PullRequest":
		if pr == nil || repo == nil {
			return nil, false
		}
		switch sel.name {
		case "id":
			return encodeGlobalID("PullRequest", repo.ID, pr.Number), true
		case "number":
			return pr.Number, true
		case "title":
			return pr.Title, true
		case "body":
			return pr.Body, true
		case "state":
			return pullState(pr), true
		case "merged":
			return pr.Merged, true
		case "headRefName":
			return pr.HeadRef, true
		case "baseRefName":
			return pr.BaseRef, true
		case "mergeable":
			if store.PullMergeable(repo, pr) {
				return "MERGEABLE", true
			}
			return "CONFLICTING", true
		case "mergedAt":
			if pr.MergedAt == nil {
				return nil, true
			}
			return *pr.MergedAt, true
		case "files":
			first := 30
			if f, ok := args["first"].(float64); ok {
				first = int(f)
			}
			changes := store.PullChanges(repo, pr)
			if changes == nil {
				changes = []FileChange{}
			}
			if first < len(changes) {
				changes = changes[:first]
			}
			nodeVals := make([]interface{}, 0, len(changes))
			for _, c := range changes {
				nodeVals = append(nodeVals, &gqlNode{typename: "PullRequestChangedFile", payload: map[string]interface{}{"__path": c.Path}})
			}
			return &gqlNode{typename: "PullRequestChangedFileConnection", payload: map[string]interface{}{"__nodes": nodeVals}}, true
		case "createdAt", "updatedAt":
			return FixedTS, true
		}
		return nil, false
	case "User":
		login, _ := parent.payload["__login"].(string)
		switch sel.name {
		case "login":
			return login, true
		case "id":
			return encodeGlobalID("User", login), true
		case "name":
			return login, true
		}
		return nil, false
	case "Ref":
		if sel.name == "name" {
			if v, ok := parent.payload["__refName"].(string); ok {
				return v, true
			}
			return nil, true
		}
		return nil, false
	case "IssueConnection":
		switch sel.name {
		case "nodes":
			if v, ok := parent.payload["__nodes"].([]interface{}); ok {
				return v, true
			}
			return []interface{}{}, true
		case "pageInfo":
			if v, ok := parent.payload["__pageInfo"].(*gqlNode); ok {
				return v, true
			}
			return nil, true
		}
		return nil, false
	case "PullRequestConnection":
		if sel.name == "nodes" {
			if v, ok := parent.payload["__nodes"].([]interface{}); ok {
				return v, true
			}
			return []interface{}{}, true
		}
		return nil, false
	case "PullRequestChangedFileConnection":
		if sel.name == "nodes" {
			if v, ok := parent.payload["__nodes"].([]interface{}); ok {
				return v, true
			}
			return []interface{}{}, true
		}
		return nil, false
	case "PullRequestChangedFile":
		if sel.name == "path" {
			if v, ok := parent.payload["__path"].(string); ok {
				return v, true
			}
			return nil, true
		}
		return nil, false
	case "PageInfo":
		switch sel.name {
		case "hasNextPage":
			if v, ok := parent.payload["__hasNextPage"].(bool); ok {
				return v, true
			}
			return false, true
		case "endCursor":
			if v, ok := parent.payload["__endCursor"]; ok {
				return v, true
			}
			return nil, true
		}
		return nil, false
	case "__Type":
		switch sel.name {
		case "name":
			if v, ok := parent.payload["__typeName"].(string); ok {
				return v, true
			}
			return nil, true
		case "fields":
			fields, _ := parent.payload["__fields"].([]string)
			out := make([]interface{}, 0, len(fields))
			for _, f := range fields {
				out = append(out, &gqlNode{typename: "__Field", payload: map[string]interface{}{"__fieldName": f}})
			}
			return out, true
		case "enumValues":
			vals, _ := parent.payload["__enumValues"].([]string)
			out := make([]interface{}, 0, len(vals))
			for _, e := range vals {
				out = append(out, &gqlNode{typename: "__EnumValue", payload: map[string]interface{}{"__enumName": e}})
			}
			return out, true
		}
		return nil, false
	case "__Field":
		if sel.name == "name" {
			if v, ok := parent.payload["__fieldName"].(string); ok {
				return v, true
			}
			return nil, true
		}
		return nil, false
	case "__EnumValue":
		if sel.name == "name" {
			if v, ok := parent.payload["__enumName"].(string); ok {
				return v, true
			}
			return nil, true
		}
		return nil, false
	case "CreateIssuePayload":
		if sel.name == "issue" {
			i, _ := parent.payload["__issue"].(*Issue)
			r, _ := parent.payload["__repo"].(*Repo)
			if i != nil && r != nil {
				return issueNode(r, i), true
			}
			return nil, true
		}
		return nil, false
	case "CloseIssuePayload":
		if sel.name == "issue" {
			i, _ := parent.payload["__issue"].(*Issue)
			r, _ := parent.payload["__repo"].(*Repo)
			if i != nil && r != nil {
				return issueNode(r, i), true
			}
			return nil, true
		}
		return nil, false
	case "CreatePullRequestPayload":
		if sel.name == "pullRequest" {
			p, _ := parent.payload["__pr"].(*PullRequest)
			r, _ := parent.payload["__repo"].(*Repo)
			if p != nil && r != nil {
				return pullNode(r, p), true
			}
			return nil, true
		}
		return nil, false
	case "MergePullRequestPayload":
		if sel.name == "pullRequest" {
			p, _ := parent.payload["__pr"].(*PullRequest)
			r, _ := parent.payload["__repo"].(*Repo)
			if p != nil && r != nil {
				return pullNode(r, p), true
			}
			return nil, true
		}
		return nil, false
	case "AddCommentPayload":
		if sel.name == "commentEdge" {
			if v, ok := parent.payload["__commentEdge"].(*gqlNode); ok {
				return v, true
			}
			return nil, true
		}
		return nil, false
	case "IssueCommentEdge":
		if sel.name == "node" {
			if v, ok := parent.payload["__node"].(*gqlNode); ok {
				return v, true
			}
			return nil, true
		}
		return nil, false
	case "IssueComment":
		c, _ := parent.payload["__comment"].(*Comment)
		switch sel.name {
		case "id":
			if c != nil {
				return encodeGlobalID("IssueComment", c.ID), true
			}
			return nil, true
		case "body":
			if c != nil {
				return c.Body, true
			}
			return nil, true
		}
		return nil, false
	}
	return nil, false
}

// ---------------------------------------------------------------------------
// Entry point
// ---------------------------------------------------------------------------

func runOperation(store *Store, op *operation, variables map[string]interface{}) OMap {
	ctx := &gqlCtx{store: store, variables: variables}
	rootType := "Query"
	if op.typ == "mutation" {
		rootType = "Mutation"
	}
	root := &gqlNode{typename: rootType, payload: map[string]interface{}{}}
	data := executeSelections(rootType, root, op.selections, ctx)
	if len(ctx.errors) > 0 {
		errVals := make([]interface{}, 0, len(ctx.errors))
		for _, e := range ctx.errors {
			errVals = append(errVals, e)
		}
		return OMap{{Key: "data", Val: nil}, {Key: "errors", Val: errVals}}
	}
	return OMap{{Key: "data", Val: data}}
}

func graphqlRoute(store *Store, bodyText string) (int, []byte) {
	var body interface{}
	if err := json.Unmarshal([]byte(bodyText), &body); err != nil {
		return 400, marshalOMap(OMap{{Key: "errors", Val: []interface{}{OMap{{Key: "message", Val: "malformed JSON body"}}}}})
	}
	runOne := func(req map[string]interface{}) OMap {
		query, _ := req["query"].(string)
		variables, _ := req["variables"].(map[string]interface{})
		if variables == nil {
			variables = map[string]interface{}{}
		}
		operationName, _ := req["operationName"].(string)
		tokens, err := tokenize(query)
		if err != nil {
			return OMap{{Key: "data", Val: nil}, {Key: "errors", Val: []interface{}{OMap{{Key: "message", Val: err.Error()}}}}}
		}
		ops, err := (&parser{tokens: tokens}).parseDocument()
		if err != nil {
			return OMap{{Key: "data", Val: nil}, {Key: "errors", Val: []interface{}{OMap{{Key: "message", Val: err.Error()}}}}}
		}
		var op *operation
		if len(ops) > 0 {
			op = ops[0]
		}
		if len(ops) > 1 && operationName != "" {
			for _, o := range ops {
				if o.name == operationName {
					op = o
					break
				}
			}
		}
		if op == nil {
			return OMap{{Key: "data", Val: nil}, {Key: "errors", Val: []interface{}{OMap{{Key: "message", Val: "no operation"}}}}}
		}
		return runOperation(store, op, variables)
	}
	if arr, ok := body.([]interface{}); ok {
		out := make([]interface{}, 0, len(arr))
		for _, b := range arr {
			if m, ok := b.(map[string]interface{}); ok {
				out = append(out, runOne(m))
			} else {
				out = append(out, runOne(map[string]interface{}{}))
			}
		}
		return 200, marshalOMap(out)
	}
	if m, ok := body.(map[string]interface{}); ok {
		return 200, marshalOMap(runOne(m))
	}
	return 200, marshalOMap(runOne(map[string]interface{}{}))
}

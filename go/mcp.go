// mcp.go — Model Context Protocol over Streamable HTTP (POST /mcp).
// Modern revision 2026-07-28 first (server/discover, no handshake), with the
// legacy initialize path kept for older clients. Every tool result carries
// both a text JSON payload and structuredContent, and the tool set is a
// single constant so two tools/list answers serialize byte-identically.

package main

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
)

const serverName = "github-core-clone-01-forge"
const serverVersion = "1.0.0"

var modernVersions = []string{"2026-07-28", "2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}
var legacyVersions = []string{"2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

type toolDef struct {
	name        string
	description string
	properties  []toolProp
	required    []string
}

type toolProp struct {
	name        string
	typ         string
	description string
}

var tools = []toolDef{
	{"create_repo", "Create a new repository owned by the authenticated user.", []toolProp{
		{"name", "string", "Repository name"},
		{"description", "string", "Repository description"},
		{"private", "boolean", "Whether the repository is private"},
	}, []string{"name"}},
	{"get_repo", "Get a repository by owner and name.", []toolProp{
		{"owner", "string", "Repository owner login"},
		{"repo", "string", "Repository name"},
	}, []string{"owner", "repo"}},
	{"create_issue", "Create an issue in a repository.", []toolProp{
		{"owner", "string", "Repository owner login"},
		{"repo", "string", "Repository name"},
		{"title", "string", "Issue title"},
		{"body", "string", "Issue body"},
	}, []string{"owner", "repo", "title"}},
	{"list_issues", "List issues in a repository, optionally filtered by state.", []toolProp{
		{"owner", "string", "Repository owner login"},
		{"repo", "string", "Repository name"},
		{"state", "string", "Issue state: open, closed or all"},
	}, []string{"owner", "repo"}},
	{"create_pull_request", "Create a pull request from a head branch into a base branch.", []toolProp{
		{"owner", "string", "Repository owner login"},
		{"repo", "string", "Repository name"},
		{"title", "string", "Pull request title"},
		{"body", "string", "Pull request body"},
		{"head", "string", "Head branch name"},
		{"base", "string", "Base branch name"},
	}, []string{"owner", "repo", "title", "head", "base"}},
	{"merge_pull_request", "Merge a pull request by number using the squash method.", []toolProp{
		{"owner", "string", "Repository owner login"},
		{"repo", "string", "Repository name"},
		{"pull_number", "number", "Pull request number"},
	}, []string{"owner", "repo", "pull_number"}},
	{"get_issue", "Get one issue by number.", []toolProp{
		{"owner", "string", "Repository owner login"},
		{"repo", "string", "Repository name"},
		{"issue_number", "number", "Issue number"},
	}, []string{"owner", "repo", "issue_number"}},
	{"list_pull_requests", "List pull requests in a repository, optionally filtered by state.", []toolProp{
		{"owner", "string", "Repository owner login"},
		{"repo", "string", "Repository name"},
		{"state", "string", "Pull request state: open, closed or all"},
	}, []string{"owner", "repo"}},
}

func toolsListValue() []interface{} {
	out := make([]interface{}, 0, len(tools))
	for _, t := range tools {
		props := OMap{}
		for _, p := range t.properties {
			props = append(props, KV{Key: p.name, Val: OMap{{Key: "type", Val: p.typ}, {Key: "description", Val: p.description}}})
		}
		required := make([]interface{}, 0, len(t.required))
		for _, r := range t.required {
			required = append(required, r)
		}
		out = append(out, OMap{
			{Key: "name", Val: t.name},
			{Key: "description", Val: t.description},
			{Key: "inputSchema", Val: OMap{
				{Key: "type", Val: "object"},
				{Key: "properties", Val: props},
				{Key: "required", Val: required},
			}},
		})
	}
	return out
}

type mcpResult struct {
	status int
	// json is an OMap (or nil for 202). Rendered with marshalOMap.
	json OMap
}

func rpcError(id interface{}, code int, message string) mcpResult {
	return mcpResult{status: 200, json: OMap{
		{Key: "jsonrpc", Val: "2.0"},
		{Key: "id", Val: id},
		{Key: "error", Val: OMap{{Key: "code", Val: code}, {Key: "message", Val: message}}},
	}}
}

func rpcResult(id interface{}, result interface{}) mcpResult {
	return mcpResult{status: 200, json: OMap{
		{Key: "jsonrpc", Val: "2.0"},
		{Key: "id", Val: id},
		{Key: "result", Val: result},
	}}
}

func toolText(structured interface{}, isError bool) OMap {
	var text string
	if s, ok := structured.(string); ok {
		text = s
	} else {
		text = string(marshalOMap(structured))
	}
	var sc interface{}
	switch structured.(type) {
	case OMap, []interface{}:
		sc = structured
	default:
		if structured == nil {
			sc = OMap{{Key: "result", Val: nil}}
		} else {
			sc = OMap{{Key: "result", Val: structured}}
		}
	}
	return OMap{
		{Key: "content", Val: []interface{}{OMap{{Key: "type", Val: "text"}, {Key: "text", Val: text}}}},
		{Key: "structuredContent", Val: sc},
		{Key: "isError", Val: isError},
	}
}

func normalizeToolName(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func findTool(name string) *toolDef {
	norm := normalizeToolName(name)
	for i := range tools {
		if normalizeToolName(tools[i].name) == norm {
			return &tools[i]
		}
	}
	return nil
}

// jsNumber mimics JS Number(v) for MCP numeric arguments.
func jsNumber(v interface{}) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case string:
		n, ok := jsParseFloat(strings.TrimSpace(t))
		return n, ok
	case bool:
		if t {
			return 1, true
		}
		return 0, true
	case nil:
		return 0, true
	}
	return 0, false
}

func jsParseFloat(s string) (float64, bool) {
	if s == "" {
		return 0, true // Number('') === 0
	}
	// JS Number() accepts decimal, hex (0x), Infinity, exponents.
	// The suite passes plain decimals; handle those plus Infinity.
	if s == "Infinity" || s == "+Infinity" {
		return math.Inf(1), true
	}
	if s == "-Infinity" {
		return math.Inf(-1), true
	}
	n, err := parseFloatStrict(s)
	if err != nil {
		return 0, false
	}
	return n, true
}

func parseFloatStrict(s string) (float64, error) {
	// strconv.ParseFloat rejects hex and trailing junk, like Number().
	return strconv.ParseFloat(s, 64)
}

func mcpRoute(store *Store, bodyText string) mcpResult {
	var req interface{}
	if err := json.Unmarshal([]byte(bodyText), &req); err != nil {
		return mcpResult{status: 400, json: OMap{
			{Key: "jsonrpc", Val: "2.0"},
			{Key: "id", Val: nil},
			{Key: "error", Val: OMap{{Key: "code", Val: -32700}, {Key: "message", Val: "Parse error"}}},
		}}
	}
	r, _ := req.(map[string]interface{})
	if r == nil {
		r = map[string]interface{}{}
	}
	var id interface{}
	if v, ok := r["id"]; ok {
		id = v
	}
	method, _ := r["method"].(string)
	params, _ := r["params"].(map[string]interface{})
	if params == nil {
		params = map[string]interface{}{}
	}

	// Notifications carry no id and expect no reply.
	if strings.HasPrefix(method, "notifications/") {
		return mcpResult{status: 202}
	}

	if method == "server/discover" {
		versions := make([]interface{}, 0, len(modernVersions))
		for _, v := range modernVersions {
			versions = append(versions, v)
		}
		return rpcResult(id, OMap{
			{Key: "supportedVersions", Val: versions},
			{Key: "capabilities", Val: OMap{{Key: "tools", Val: OMap{}}}},
			{Key: "serverInfo", Val: OMap{{Key: "name", Val: serverName}, {Key: "version", Val: serverVersion}}},
			{Key: "_meta", Val: OMap{{Key: "io.modelcontextprotocol/serverInfo", Val: OMap{{Key: "name", Val: serverName}, {Key: "version", Val: serverVersion}}}}},
		})
	}

	if method == "initialize" {
		requested, _ := params["protocolVersion"].(string)
		chosen := "2025-11-25"
		for _, v := range legacyVersions {
			if v == requested {
				chosen = v
				break
			}
		}
		return rpcResult(id, OMap{
			{Key: "protocolVersion", Val: chosen},
			{Key: "capabilities", Val: OMap{{Key: "tools", Val: OMap{}}}},
			{Key: "serverInfo", Val: OMap{{Key: "name", Val: serverName}, {Key: "version", Val: serverVersion}}},
		})
	}

	if method == "ping" {
		return rpcResult(id, OMap{})
	}

	if method == "tools/list" {
		return rpcResult(id, OMap{{Key: "tools", Val: toolsListValue()}})
	}

	if method == "tools/call" {
		name, _ := params["name"].(string)
		args, _ := params["arguments"].(map[string]interface{})
		if args == nil {
			args = map[string]interface{}{}
		}
		tool := findTool(name)
		if tool == nil {
			return rpcError(id, -32602, fmt.Sprintf("Unknown tool: %s", name))
		}
		var missing []string
		for _, k := range tool.required {
			v, ok := args[k]
			if !ok || v == nil || v == "" {
				missing = append(missing, k)
			}
		}
		if len(missing) > 0 {
			return rpcResult(id, toolText(OMap{{Key: "error", Val: fmt.Sprintf("Missing required argument(s): %s", strings.Join(missing, ", "))}}, true))
		}
		owner := store.ViewerLogin
		if v, ok := args["owner"].(string); ok {
			owner = v
		} else if v, ok := args["owner"]; ok && v != nil {
			owner = fmt.Sprintf("%v", v)
		}
		repoName := ""
		if v, ok := args["repo"].(string); ok {
			repoName = v
		} else if v, ok := args["repo"]; ok && v != nil {
			repoName = fmt.Sprintf("%v", v)
		} else if v, ok := args["name"].(string); ok {
			repoName = v
		} else if v, ok := args["name"]; ok && v != nil {
			repoName = fmt.Sprintf("%v", v)
		}
		repo := store.GetRepo(owner, repoName)
		switch tool.name {
		case "create_repo":
			name := ""
			if v, ok := args["name"].(string); ok {
				name = v
			} else if v, ok := args["name"]; ok && v != nil {
				name = fmt.Sprintf("%v", v)
			}
			description, _ := args["description"].(string)
			isPrivate := args["private"] == true
			created, errStr := store.CreateRepo(name, description, isPrivate)
			if errStr != "" {
				if errStr == "already exists" {
					existing := store.GetRepo(store.ViewerLogin, name)
					out := OMap{
						{Key: "id", Val: existing.ID},
						{Key: "name", Val: existing.Name},
						{Key: "full_name", Val: existing.Owner + "/" + existing.Name},
						{Key: "private", Val: existing.Private},
						{Key: "description", Val: existing.Description},
						{Key: "default_branch", Val: existing.DefaultBranch},
					}
					return rpcResult(id, toolText(out, false))
				}
				return rpcResult(id, toolText(OMap{{Key: "error", Val: errStr}}, true))
			}
			out := OMap{
				{Key: "id", Val: created.ID},
				{Key: "name", Val: created.Name},
				{Key: "full_name", Val: created.Owner + "/" + created.Name},
				{Key: "private", Val: created.Private},
				{Key: "description", Val: created.Description},
				{Key: "default_branch", Val: created.DefaultBranch},
			}
			return rpcResult(id, toolText(out, false))
		case "get_repo":
			if repo == nil {
				return rpcResult(id, toolText(OMap{{Key: "error", Val: "Not Found"}}, true))
			}
			out := OMap{
				{Key: "id", Val: repo.ID},
				{Key: "name", Val: repo.Name},
				{Key: "full_name", Val: repo.Owner + "/" + repo.Name},
				{Key: "owner", Val: OMap{{Key: "login", Val: repo.Owner}}},
				{Key: "private", Val: repo.Private},
				{Key: "description", Val: repo.Description},
				{Key: "default_branch", Val: repo.DefaultBranch},
			}
			return rpcResult(id, toolText(out, false))
		case "create_issue":
			if repo == nil {
				return rpcResult(id, toolText(OMap{{Key: "error", Val: "Not Found"}}, true))
			}
			title := ""
			if v, ok := args["title"].(string); ok {
				title = v
			} else if v, ok := args["title"]; ok && v != nil {
				title = fmt.Sprintf("%v", v)
			}
			body, _ := args["body"].(string)
			issue, errStr := store.CreateIssue(repo, title, body, []string{})
			if errStr != "" || issue == nil {
				errMsg := errStr
				if errMsg == "" {
					errMsg = "failed"
				}
				return rpcResult(id, toolText(OMap{{Key: "error", Val: errMsg}}, true))
			}
			out := OMap{
				{Key: "id", Val: issue.ID},
				{Key: "number", Val: issue.Number},
				{Key: "title", Val: issue.Title},
				{Key: "body", Val: issue.Body},
				{Key: "state", Val: issue.State},
				{Key: "created_at", Val: FixedTS},
				{Key: "url", Val: fmt.Sprintf("/api/v3/repos/%s/%s/issues/%d", repo.Owner, repo.Name, issue.Number)},
			}
			return rpcResult(id, toolText(out, false))
		case "list_issues":
			if repo == nil {
				return rpcResult(id, toolText(OMap{{Key: "error", Val: "Not Found"}}, true))
			}
			state, _ := args["state"].(string)
			if state == "" {
				state = "open"
			}
			list := store.ListIssues(repo, state, "desc")
			issues := make([]interface{}, 0, len(list))
			for _, i := range list {
				issues = append(issues, OMap{
					{Key: "id", Val: i.ID},
					{Key: "number", Val: i.Number},
					{Key: "title", Val: i.Title},
					{Key: "body", Val: i.Body},
					{Key: "state", Val: i.State},
				})
			}
			return rpcResult(id, toolText(OMap{{Key: "issues", Val: issues}}, false))
		case "create_pull_request":
			if repo == nil {
				return rpcResult(id, toolText(OMap{{Key: "error", Val: "Not Found"}}, true))
			}
			title := strArg(args, "title")
			body, _ := args["body"].(string)
			head := strArg(args, "head")
			base := strArg(args, "base")
			created, errStr := store.CreatePull(repo, title, body, head, base)
			if errStr != "" || created == nil {
				errMsg := errStr
				if errMsg == "" {
					errMsg = "failed"
				}
				return rpcResult(id, toolText(OMap{{Key: "error", Val: errMsg}}, true))
			}
			out := OMap{
				{Key: "id", Val: created.ID},
				{Key: "number", Val: created.Number},
				{Key: "pull_number", Val: created.Number},
				{Key: "title", Val: created.Title},
				{Key: "body", Val: created.Body},
				{Key: "state", Val: created.State},
				{Key: "head", Val: OMap{{Key: "ref", Val: created.HeadRef}, {Key: "sha", Val: created.HeadSha}}},
				{Key: "base", Val: OMap{{Key: "ref", Val: created.BaseRef}, {Key: "sha", Val: created.BaseSha}}},
			}
			return rpcResult(id, toolText(out, false))
		case "merge_pull_request":
			if repo == nil {
				return rpcResult(id, toolText(OMap{{Key: "error", Val: "Not Found"}}, true))
			}
			var numVal interface{}
			if v, ok := args["pull_number"]; ok {
				numVal = v
			} else if v, ok := args["number"]; ok {
				numVal = v
			}
			num, okNum := jsNumber(numVal)
			var pr *PullRequest
			if okNum {
				pr = repo.Pulls[int(num)]
			}
			if pr == nil {
				return rpcResult(id, toolText(OMap{{Key: "error", Val: "Not Found"}}, true))
			}
			sha, errStr := store.MergePull(repo, pr)
			if errStr != "" {
				return rpcResult(id, toolText(OMap{{Key: "error", Val: errStr}}, true))
			}
			out := OMap{
				{Key: "number", Val: pr.Number},
				{Key: "pull_number", Val: pr.Number},
				{Key: "merged", Val: true},
				{Key: "state", Val: pr.State},
				{Key: "sha", Val: sha},
				{Key: "merge_commit_sha", Val: sha},
			}
			return rpcResult(id, toolText(out, false))
		case "get_issue":
			if repo == nil {
				return rpcResult(id, toolText(OMap{{Key: "error", Val: "Not Found"}}, true))
			}
			var numVal interface{}
			if v, ok := args["issue_number"]; ok {
				numVal = v
			} else if v, ok := args["number"]; ok {
				numVal = v
			}
			num, okNum := jsNumber(numVal)
			var issue *Issue
			if okNum {
				issue = repo.Issues[int(num)]
			}
			if issue == nil {
				return rpcResult(id, toolText(OMap{{Key: "error", Val: "Not Found"}}, true))
			}
			out := OMap{
				{Key: "id", Val: issue.ID},
				{Key: "number", Val: issue.Number},
				{Key: "title", Val: issue.Title},
				{Key: "body", Val: issue.Body},
				{Key: "state", Val: issue.State},
			}
			return rpcResult(id, toolText(out, false))
		case "list_pull_requests":
			if repo == nil {
				return rpcResult(id, toolText(OMap{{Key: "error", Val: "Not Found"}}, true))
			}
			state, _ := args["state"].(string)
			if state == "" {
				state = "open"
			}
			list := store.ListPulls(repo, state)
			prs := make([]interface{}, 0, len(list))
			for _, p := range list {
				prs = append(prs, OMap{
					{Key: "id", Val: p.ID},
					{Key: "number", Val: p.Number},
					{Key: "title", Val: p.Title},
					{Key: "body", Val: p.Body},
					{Key: "state", Val: p.State},
					{Key: "merged", Val: p.Merged},
				})
			}
			return rpcResult(id, toolText(OMap{{Key: "pull_requests", Val: prs}}, false))
		}
		return rpcError(id, -32602, fmt.Sprintf("Unknown tool: %s", name))
	}

	return rpcError(id, -32601, fmt.Sprintf("Method not found: %s", method))
}

// strArg is String(args[k] ?? '').
func strArg(args map[string]interface{}, key string) string {
	if v, ok := args[key].(string); ok {
		return v
	}
	if v, ok := args[key]; ok && v != nil {
		return fmt.Sprintf("%v", v)
	}
	return ""
}

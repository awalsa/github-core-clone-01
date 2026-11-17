// rest.go — the REST v3 surface (/api/v3). Every reply is JSON with a
// non-empty X-GitHub-Request-Id; errors are {"message": string}.

package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

type RestResult struct {
	Status  int
	JSON    interface{} // marshalled when Raw is nil
	Headers map[string]string
	// Prebuilt JSON body bytes (with trailing newline); when set, JSON is ignored.
	Raw []byte
}

type RestCtx struct {
	Method   string
	Segments []string
	Query    url.Values
	BodyText string
	Host     string
}

func restOK(status int, v interface{}, headers map[string]string) RestResult {
	return RestResult{Status: status, JSON: v, Headers: headers}
}

func restErr(status int, message string) RestResult {
	return RestResult{Status: status, JSON: map[string]string{"message": message}}
}

// marshalJSON renders v exactly the way Node's JSON.stringify would for
// these payloads: no HTML escaping, no indentation, UTF-8 left raw.
func marshalJSON(v interface{}) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	// Encoder appends '\n'; callers that need raw bytes keep it, others trim.
	return buf.Bytes()
}

func marshalJSONNoNL(v interface{}) []byte {
	b := marshalJSON(v)
	return bytes.TrimSuffix(b, []byte("\n"))
}

func parseBody(ctx *RestCtx) (interface{}, bool) {
	if ctx.BodyText == "" || strings.TrimSpace(ctx.BodyText) == "" {
		return map[string]interface{}{}, false
	}
	var v interface{}
	dec := json.NewDecoder(strings.NewReader(ctx.BodyText))
	if err := dec.Decode(&v); err != nil {
		return nil, true
	}
	return v, false
}

func asRecord(v interface{}) map[string]interface{} {
	if m, ok := v.(map[string]interface{}); ok {
		return m
	}
	return map[string]interface{}{}
}

func asString(m map[string]interface{}, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func asBool(m map[string]interface{}, key string) bool {
	if v, ok := m[key].(bool); ok {
		return v
	}
	return false
}

// -- views (field order matches Node's object literals) ---------------------

type ownerView struct {
	Login string `json:"login"`
	ID    int    `json:"id"`
	Type  string `json:"type,omitempty"`
}

type repoView struct {
	ID            int       `json:"id"`
	Name          string    `json:"name"`
	FullName      string    `json:"full_name"`
	Owner         ownerView `json:"owner"`
	Private       bool      `json:"private"`
	Description   string    `json:"description"`
	DefaultBranch string    `json:"default_branch"`
	CreatedAt     string    `json:"created_at"`
	UpdatedAt     string    `json:"updated_at"`
	HTMLURL       string    `json:"html_url"`
	URL           string    `json:"url"`
}

func makeRepoView(repo *Repo) repoView {
	return repoView{
		ID: repo.ID, Name: repo.Name, FullName: repo.Owner + "/" + repo.Name,
		Owner: ownerView{Login: repo.Owner, ID: 1000, Type: "User"},
		Private: repo.Private, Description: repo.Description, DefaultBranch: repo.DefaultBranch,
		CreatedAt: FixedTS, UpdatedAt: FixedTS,
		HTMLURL: "https://example.invalid/" + repo.Owner + "/" + repo.Name,
		URL:     "/api/v3/repos/" + repo.Owner + "/" + repo.Name,
	}
}

type labelView struct {
	Name  string `json:"name"`
	Color string `json:"color"`
}

type issueUserView struct {
	Login string `json:"login"`
	ID    int    `json:"id"`
}

type issueView struct {
	ID        int         `json:"id"`
	Number    int         `json:"number"`
	Title     string      `json:"title"`
	Body      string      `json:"body"`
	State     string      `json:"state"`
	Labels    []labelView `json:"labels"`
	User      issueUserView `json:"user"`
	CreatedAt string      `json:"created_at"`
	UpdatedAt string      `json:"updated_at"`
	Comments  int         `json:"comments"`
	URL       string      `json:"url"`
}

func makeIssueView(issue *Issue, repo *Repo) issueView {
	labels := []labelView{}
	for _, l := range issue.Labels {
		color := "000000"
		if lab, ok := repo.Labels[l]; ok {
			color = lab.Color
		}
		labels = append(labels, labelView{Name: l, Color: color})
	}
	return issueView{
		ID: issue.ID, Number: issue.Number, Title: issue.Title, Body: issue.Body, State: issue.State,
		Labels: labels, User: issueUserView{Login: repo.Owner, ID: 1000},
		CreatedAt: FixedTS, UpdatedAt: FixedTS, Comments: len(issue.Comments),
		URL: "/api/v3/repos/" + repo.Owner + "/" + repo.Name + "/issues/" + strconv.Itoa(issue.Number),
	}
}

type pullRefView struct {
	Ref   string `json:"ref"`
	Sha   string `json:"sha"`
	Label string `json:"label"`
}

type pullView struct {
	ID             int         `json:"id"`
	Number         int         `json:"number"`
	Title          string      `json:"title"`
	Body           string      `json:"body"`
	State          string      `json:"state"`
	Head           pullRefView `json:"head"`
	Base           pullRefView `json:"base"`
	Merged         bool        `json:"merged"`
	MergeCommitSha *string     `json:"merge_commit_sha"`
	MergedAt       *string     `json:"merged_at"`
	User           issueUserView `json:"user"`
	CreatedAt      string      `json:"created_at"`
	UpdatedAt      string      `json:"updated_at"`
	URL            string      `json:"url"`
}

func makePullView(pr *PullRequest, repo *Repo) pullView {
	return pullView{
		ID: pr.ID, Number: pr.Number, Title: pr.Title, Body: pr.Body, State: pr.State,
		Head: pullRefView{Ref: pr.HeadRef, Sha: pr.HeadSha, Label: repo.Owner + ":" + pr.HeadRef},
		Base: pullRefView{Ref: pr.BaseRef, Sha: pr.BaseSha, Label: repo.Owner + ":" + pr.BaseRef},
		Merged: pr.Merged, MergeCommitSha: pr.MergeSha, MergedAt: pr.MergedAt,
		User: issueUserView{Login: repo.Owner, ID: 1000},
		CreatedAt: FixedTS, UpdatedAt: FixedTS,
		URL: "/api/v3/repos/" + repo.Owner + "/" + repo.Name + "/pulls/" + strconv.Itoa(pr.Number),
	}
}

// -- pagination ------------------------------------------------------------

// jsParseInt mimics JS parseInt(s, 10): leading sign + digits, NaN if none.
func jsParseInt(s string) (int, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	i := 0
	if s[0] == '-' || s[0] == '+' {
		i = 1
	}
	start := i
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i == start {
		return 0, false
	}
	n, err := strconv.Atoi(s[:i])
	if err != nil {
		return 0, false
	}
	return n, true
}

func pageParams(query url.Values) (perPage, page int) {
	perPage = 30
	if v, ok := jsParseInt(query.Get("per_page")); ok && v != 0 {
		perPage = v
	}
	if perPage < 1 {
		perPage = 1
	}
	if perPage > 100 {
		perPage = 100
	}
	page = 1
	if v, ok := jsParseInt(query.Get("page")); ok && v != 0 {
		page = v
	}
	if page < 1 {
		page = 1
	}
	return perPage, page
}

func paginate[T any](items []T, perPage, page int) ([]T, bool) {
	start := (page - 1) * perPage
	if start >= len(items) {
		return []T{}, false
	}
	end := start + perPage
	if end > len(items) {
		end = len(items)
	}
	return items[start:end], start+perPage < len(items)
}

func linkHeader(base string, perPage, page int, hasNext bool) map[string]string {
	var links []string
	if hasNext {
		links = append(links, fmt.Sprintf("<%s?per_page=%d&page=%d>; rel=\"next\"", base, perPage, page+1))
	}
	if page > 1 {
		links = append(links, fmt.Sprintf("<%s?per_page=%d&page=1>; rel=\"first\"", base, perPage))
	}
	if len(links) == 0 {
		return nil
	}
	return map[string]string{"link": strings.Join(links, ", ")}
}

func decodeURIComponent(s string) string {
	if v, err := url.PathUnescape(s); err == nil {
		return v
	}
	return s
}

func base64Lenient(s string) []byte {
	// Node's Buffer.from(str, 'base64') ignores characters outside the
	// alphabet instead of throwing; mimic that, then decode.
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '+' || c == '/' || c == '=' {
			b.WriteByte(c)
		}
	}
	clean := b.String()
	data, err := base64.StdEncoding.DecodeString(clean)
	if err != nil {
		// Tolerate missing padding like Node does.
		if m := len(clean) % 4; m != 0 {
			clean += strings.Repeat("=", 4-m)
			data, err = base64.StdEncoding.DecodeString(clean)
		}
		if err != nil {
			return []byte{}
		}
	}
	return data
}

func basename(p string) string {
	if idx := strings.LastIndex(p, "/"); idx >= 0 {
		return p[idx+1:]
	}
	return p
}

// RestRoute dispatches one REST v3 request.
func RestRoute(s *Store, ctx *RestCtx) RestResult {
	method := ctx.Method
	segs := ctx.Segments

	// GET /api/v3/ — API root
	if len(segs) == 0 {
		if method != "GET" {
			return restErr(404, "Not Found")
		}
		return restOK(200, map[string]string{"current_user_url": "/api/v3/user", "rate_limit_url": "/api/v3/rate_limit"}, map[string]string{"x-oauth-scopes": "repo, read:org"})
	}

	// GET /api/v3/meta
	if len(segs) == 1 && segs[0] == "meta" {
		if method != "GET" {
			return restErr(404, "Not Found")
		}
		return restOK(200, map[string]interface{}{"installed_version": "1.0.0", "verifiable_password_authentication": true}, nil)
	}

	// GET /api/v3/user ; POST /api/v3/user/repos ; GET /api/v3/user/repos
	if segs[0] == "user" {
		if len(segs) == 1 {
			if method != "GET" {
				return restErr(404, "Not Found")
			}
			return restOK(200, map[string]interface{}{"login": s.ViewerLogin, "id": 1000, "type": "User", "name": s.ViewerLogin, "email": s.ViewerLogin + "@example.invalid"}, nil)
		}
		if len(segs) == 2 && segs[1] == "repos" {
			if method == "POST" {
				value, malformed := parseBody(ctx)
				if malformed {
					return restErr(400, "malformed JSON body")
				}
				b := asRecord(value)
				name := asString(b, "name")
				description := asString(b, "description")
				isPrivate := asBool(b, "private")
				repo, errStr := s.CreateRepo(name, description, isPrivate)
				if errStr == "already exists" {
					return restErr(422, fmt.Sprintf("Repository %s/%s already exists", s.ViewerLogin, name))
				}
				if errStr != "" {
					return restErr(422, errStr)
				}
				return restOK(201, makeRepoView(repo), nil)
			}
			if method == "GET" {
				perPage, page := pageParams(ctx.Query)
				all := s.ListViewerRepos()
				views := make([]repoView, 0, len(all))
				for _, r := range all {
					views = append(views, makeRepoView(r))
				}
				slice, hasNext := paginate(views, perPage, page)
				base := "http://" + ctx.Host + "/api/v3/user/repos"
				return restOK(200, slice, linkHeader(base, perPage, page, hasNext))
			}
			return restErr(404, "Not Found")
		}
		return restErr(404, "Not Found")
	}

	// /api/v3/repos/{owner}/{repo}[...]
	if segs[0] == "repos" && len(segs) >= 3 {
		owner := decodeURIComponent(segs[1])
		name := decodeURIComponent(segs[2])
		repo := s.GetRepo(owner, name)
		if repo == nil {
			return restErr(404, "Not Found")
		}

		// /repos/{o}/{r}
		if len(segs) == 3 {
			if method == "GET" {
				return restOK(200, makeRepoView(repo), nil)
			}
			if method == "PATCH" {
				value, malformed := parseBody(ctx)
				if malformed {
					return restErr(400, "malformed JSON body")
				}
				b := asRecord(value)
				if v, ok := b["description"].(string); ok {
					repo.Description = v
				}
				if v, ok := b["private"].(bool); ok {
					repo.Private = v
				}
				return restOK(200, makeRepoView(repo), nil)
			}
			if method == "DELETE" {
				s.DeleteRepo(owner, name)
				return RestResult{Status: 204}
			}
			return restErr(404, "Not Found")
		}

		tail := segs[3:]

		// /git/refs
		if tail[0] == "git" && len(tail) >= 2 && tail[1] == "refs" {
			if method == "POST" && len(tail) == 2 {
				value, malformed := parseBody(ctx)
				if malformed {
					return restErr(400, "malformed JSON body")
				}
				b := asRecord(value)
				ref := asString(b, "ref")
				sha := asString(b, "sha")
				if !strings.HasPrefix(ref, "refs/") || len(sha) != 40 || !isHex40(sha) || !s.HasObject(sha) {
					return restErr(422, "invalid ref or sha")
				}
				if _, ok := s.GetRef(repo, ref); ok {
					return restErr(422, "ref already exists")
				}
				s.SetRef(repo, ref, sha)
				return restOK(201, map[string]interface{}{"ref": ref, "object": map[string]string{"sha": sha}}, nil)
			}
			if method == "GET" && len(tail) >= 4 && tail[2] == "heads" {
				branch := decodeURIComponent(strings.Join(tail[3:], "/"))
				sha, ok := s.GetBranchSha(repo, branch)
				if !ok {
					return restErr(404, "Not Found")
				}
				return restOK(200, map[string]interface{}{"ref": "refs/heads/" + branch, "object": map[string]string{"sha": sha}}, nil)
			}
			return restErr(404, "Not Found")
		}

		// /branches
		if tail[0] == "branches" {
			if method == "GET" && len(tail) == 1 {
				branches := s.ListBranches(repo)
				out := make([]map[string]interface{}, 0, len(branches))
				for _, br := range branches {
					out = append(out, map[string]interface{}{"name": br.Name, "commit": map[string]string{"sha": br.OID}})
				}
				return restOK(200, out, nil)
			}
			if method == "GET" && len(tail) >= 2 {
				branch := decodeURIComponent(strings.Join(tail[1:], "/"))
				sha, ok := s.GetBranchSha(repo, branch)
				if !ok {
					return restErr(404, "Not Found")
				}
				return restOK(200, map[string]interface{}{"name": branch, "commit": map[string]string{"sha": sha}}, nil)
			}
			return restErr(404, "Not Found")
		}

		// /commits
		if tail[0] == "commits" {
			if method == "GET" && len(tail) == 1 {
				sha := ctx.Query.Get("sha")
				list, ok := s.ListCommits(repo, sha)
				if !ok {
					return restErr(404, "Not Found")
				}
				out := make([]map[string]interface{}, 0, len(list))
				for _, c := range list {
					out = append(out, map[string]interface{}{"sha": c.Sha, "commit": map[string]string{"message": c.Message}})
				}
				return restOK(200, out, nil)
			}
			return restErr(404, "Not Found")
		}

		// /contents/{path...}
		if tail[0] == "contents" {
			parts := make([]string, 0, len(tail)-1)
			for _, p := range tail[1:] {
				parts = append(parts, decodeURIComponent(p))
			}
			filePath := strings.Join(parts, "/")
			if filePath == "" {
				return restErr(404, "Not Found")
			}
			if method == "GET" {
				ref := ctx.Query.Get("ref")
				var tip string
				if ref != "" {
					t, ok := s.ResolveRefish(repo, ref)
					if !ok {
						return restErr(404, "Not Found")
					}
					tip = t
				} else {
					t, ok := repo.Refs["refs/heads/"+repo.DefaultBranch]
					if !ok {
						return restErr(404, "Not Found")
					}
					tip = t
				}
				commitOID := s.ResolveToCommit(tip)
				if commitOID == "" {
					return restErr(404, "Not Found")
				}
				files := s.FilesAtCommit(commitOID)
				if entry, ok := files[filePath]; ok {
					var data []byte
					if obj, ok := s.GetObject(entry.OID); ok {
						data = obj.Data
					} else {
						data = []byte{}
					}
					return restOK(200, map[string]interface{}{
						"type": "file", "encoding": "base64", "size": len(data),
						"name": basename(filePath), "path": filePath,
						"content": base64.StdEncoding.EncodeToString(data), "sha": entry.OID,
					}, nil)
				}
				// directory listing
				prefix := filePath + "/"
				type child struct {
					name string
					typ  string
					path string
					sha  string
				}
				children := map[string]*child{}
				var order []string
				for p, f := range files {
					if p == filePath || !strings.HasPrefix(p, prefix) {
						continue
					}
					rest := p[len(prefix):]
					first := strings.Split(rest, "/")[0]
					childPath := filePath + "/" + first
					if strings.Contains(rest, "/") {
						if _, ok := children[first]; !ok {
							children[first] = &child{name: first, typ: "dir", path: childPath, sha: ""}
							order = append(order, first)
						}
					} else {
						if _, ok := children[first]; !ok {
							order = append(order, first)
						}
						children[first] = &child{name: first, typ: "file", path: childPath, sha: f.OID}
					}
				}
				if len(children) == 0 {
					return restErr(404, "Not Found")
				}
				sort.Strings(order)
				out := make([]map[string]string, 0, len(order))
				for _, n := range order {
					c := children[n]
					out = append(out, map[string]string{"name": c.name, "type": c.typ, "path": c.path, "sha": c.sha})
				}
				return restOK(200, out, nil)
			}
			if method == "PUT" {
				value, malformed := parseBody(ctx)
				if malformed {
					return restErr(400, "malformed JSON body")
				}
				b := asRecord(value)
				message := "update"
				if v, ok := b["message"].(string); ok {
					message = v
				}
				contentB64 := asString(b, "content")
				branch := repo.DefaultBranch
				if v, ok := b["branch"].(string); ok {
					branch = v
				}
				sha, hasSha := b["sha"].(string)
				content := base64Lenient(strings.ReplaceAll(contentB64, " ", ""))
				tip, hasTip := s.GetBranchSha(repo, branch)
				if !hasTip {
					if defaultTip, ok := s.GetBranchSha(repo, repo.DefaultBranch); ok {
						s.SetRef(repo, "refs/heads/"+branch, defaultTip)
						tip = defaultTip
						hasTip = true
					}
				}
				var commitOID string
				if hasTip {
					commitOID = s.ResolveToCommit(tip)
				}
				var files map[string]FileRef
				if commitOID != "" {
					files = s.FilesAtCommit(commitOID)
				} else {
					files = map[string]FileRef{}
				}
				existing, hasExisting := files[filePath]
				if hasExisting && hasSha && sha != existing.OID {
					return restErr(409, "sha does not match")
				}
				if hasExisting && !hasSha {
					return restErr(409, "sha required to update an existing file")
				}
				changes := map[string]*string{filePath: strPtr(string(content))}
				parent := ""
				if commitOID != "" {
					parent = commitOID
				}
				newCommit := s.CommitFiles(parent, changes, message, FixedEpochSec)
				s.SetRef(repo, "refs/heads/"+branch, newCommit)
				blobOID := s.FilesAtCommit(newCommit)[filePath].OID
				status := 201
				if hasExisting {
					status = 200
				}
				return restOK(status, map[string]interface{}{
					"content": map[string]interface{}{"name": basename(filePath), "path": filePath, "sha": blobOID, "size": len(content), "type": "file", "encoding": "base64"},
					"commit":  map[string]string{"sha": newCommit},
				}, nil)
			}
			if method == "DELETE" {
				value, malformed := parseBody(ctx)
				if malformed {
					return restErr(400, "malformed JSON body")
				}
				b := asRecord(value)
				message := "delete"
				if v, ok := b["message"].(string); ok {
					message = v
				}
				sha, hasSha := b["sha"].(string)
				branch := repo.DefaultBranch
				if v, ok := b["branch"].(string); ok {
					branch = v
				}
				tip, ok := s.GetBranchSha(repo, branch)
				if !ok {
					return restErr(404, "Not Found")
				}
				commitOID := s.ResolveToCommit(tip)
				if commitOID == "" {
					return restErr(404, "Not Found")
				}
				files := s.FilesAtCommit(commitOID)
				existing, hasExisting := files[filePath]
				if !hasExisting {
					return restErr(404, "Not Found")
				}
				if hasSha && sha != existing.OID {
					return restErr(409, "sha does not match")
				}
				changes := map[string]*string{filePath: nil}
				newCommit := s.CommitFiles(commitOID, changes, message, FixedEpochSec)
				s.SetRef(repo, "refs/heads/"+branch, newCommit)
				return restOK(200, map[string]interface{}{"commit": map[string]string{"sha": newCommit}}, nil)
			}
			return restErr(404, "Not Found")
		}

		// /labels
		if tail[0] == "labels" {
			if method == "POST" && len(tail) == 1 {
				value, malformed := parseBody(ctx)
				if malformed {
					return restErr(400, "malformed JSON body")
				}
				b := asRecord(value)
				name := asString(b, "name")
				color := "000000"
				if v, ok := b["color"].(string); ok {
					color = v
				}
				if name == "" {
					return restErr(422, "label name required")
				}
				label := s.CreateLabel(repo, name, color)
				return restOK(201, map[string]string{"name": label.Name, "color": label.Color}, nil)
			}
			return restErr(404, "Not Found")
		}

		// /issues...
		if tail[0] == "issues" {
			if len(tail) == 1 {
				if method == "POST" {
					value, malformed := parseBody(ctx)
					if malformed {
						return restErr(400, "malformed JSON body")
					}
					b := asRecord(value)
					title := asString(b, "title")
					body := asString(b, "body")
					var labels []string
					if arr, ok := b["labels"].([]interface{}); ok {
						for _, l := range arr {
							if str, ok := l.(string); ok {
								labels = append(labels, str)
							}
						}
					}
					issue, errStr := s.CreateIssue(repo, title, body, labels)
					if errStr != "" {
						return restErr(422, errStr)
					}
					return restOK(201, makeIssueView(issue, repo), nil)
				}
				if method == "GET" {
					state := ctx.Query.Get("state")
					if state == "" {
						state = "open"
					}
					direction := ctx.Query.Get("direction")
					if direction == "" {
						direction = "desc"
					}
					all := s.ListIssues(repo, state, direction)
					perPage, page := pageParams(ctx.Query)
					slice, hasNext := paginate(all, perPage, page)
					views := make([]issueView, 0, len(slice))
					for _, i := range slice {
						views = append(views, makeIssueView(i, repo))
					}
					base := "http://" + ctx.Host + "/api/v3/repos/" + owner + "/" + name + "/issues"
					return restOK(200, views, linkHeader(base, perPage, page, hasNext))
				}
				return restErr(404, "Not Found")
			}
			num, ok := jsParseInt(tail[1])
			if !ok {
				return restErr(404, "Not Found")
			}
			issue := s.GetIssue(repo, num)
			if len(tail) == 2 {
				if issue == nil {
					return restErr(404, "Not Found")
				}
				if method == "GET" {
					if issue.ViewCache == nil {
						issue.ViewCache = marshalJSON(makeIssueView(issue, repo))
					}
					return RestResult{Status: 200, Raw: issue.ViewCache}
				}
				if method == "PATCH" {
					value, malformed := parseBody(ctx)
					if malformed {
						return restErr(400, "malformed JSON body")
					}
					b := asRecord(value)
					if v, ok := b["state"].(string); ok && (v == "open" || v == "closed") {
						issue.State = v
					}
					if v, ok := b["title"].(string); ok {
						issue.Title = v
					}
					if v, ok := b["body"].(string); ok {
						issue.Body = v
					}
					issue.ViewCache = nil
					return restOK(200, makeIssueView(issue, repo), nil)
				}
				return restErr(404, "Not Found")
			}
			if len(tail) == 3 && tail[2] == "comments" {
				if issue == nil {
					return restErr(404, "Not Found")
				}
				if method == "POST" {
					value, malformed := parseBody(ctx)
					if malformed {
						return restErr(400, "malformed JSON body")
					}
					b := asRecord(value)
					body := asString(b, "body")
					comment, errStr := s.CreateComment(repo, num, body)
					if errStr != "" {
						return restErr(404, errStr)
					}
					return restOK(201, map[string]interface{}{"id": comment.ID, "body": comment.Body, "user": map[string]interface{}{"login": repo.Owner, "id": 1000}, "created_at": FixedTS}, nil)
				}
				if method == "GET" {
					out := make([]map[string]interface{}, 0, len(issue.Comments))
					for _, c := range issue.Comments {
						out = append(out, map[string]interface{}{"id": c.ID, "body": c.Body, "user": map[string]interface{}{"login": repo.Owner, "id": 1000}, "created_at": FixedTS})
					}
					return restOK(200, out, nil)
				}
				return restErr(404, "Not Found")
			}
			return restErr(404, "Not Found")
		}

		// /pulls...
		if tail[0] == "pulls" {
			if len(tail) == 1 {
				if method == "POST" {
					value, malformed := parseBody(ctx)
					if malformed {
						return restErr(400, "malformed JSON body")
					}
					b := asRecord(value)
					title := asString(b, "title")
					body := asString(b, "body")
					head := asString(b, "head")
					base := asString(b, "base")
					pr, errStr := s.CreatePull(repo, title, body, head, base)
					if errStr != "" {
						return restErr(422, errStr)
					}
					return restOK(201, makePullView(pr, repo), nil)
				}
				if method == "GET" {
					state := ctx.Query.Get("state")
					if state == "" {
						state = "open"
					}
					all := s.ListPulls(repo, state)
					perPage, page := pageParams(ctx.Query)
					slice, hasNext := paginate(all, perPage, page)
					views := make([]pullView, 0, len(slice))
					for _, p := range slice {
						views = append(views, makePullView(p, repo))
					}
					base := "http://" + ctx.Host + "/api/v3/repos/" + owner + "/" + name + "/pulls"
					return restOK(200, views, linkHeader(base, perPage, page, hasNext))
				}
				return restErr(404, "Not Found")
			}
			num, ok := jsParseInt(tail[1])
			if !ok {
				return restErr(404, "Not Found")
			}
			pr := s.GetPull(repo, num)
			if len(tail) == 2 {
				if pr == nil {
					return restErr(404, "Not Found")
				}
				if method == "GET" {
					return restOK(200, makePullView(pr, repo), nil)
				}
				return restErr(404, "Not Found")
			}
			if len(tail) == 3 && tail[2] == "files" {
				if pr == nil {
					return restErr(404, "Not Found")
				}
				if method != "GET" {
					return restErr(404, "Not Found")
				}
				changes := s.PullChanges(repo, pr)
				if changes == nil {
					return restErr(404, "Not Found")
				}
				out := make([]map[string]interface{}, 0, len(changes))
				for _, c := range changes {
					out = append(out, map[string]interface{}{"filename": c.Path, "status": c.Status, "additions": c.Additions, "deletions": c.Deletions, "changes": c.Additions + c.Deletions})
				}
				return restOK(200, out, nil)
			}
			if len(tail) == 3 && tail[2] == "merge" {
				if pr == nil {
					return restErr(404, "Not Found")
				}
				if method != "PUT" {
					return restErr(404, "Not Found")
				}
				sha, errStr := s.MergePull(repo, pr)
				if errStr == "conflict" {
					return restErr(405, "merge conflict")
				}
				if errStr != "" {
					return restErr(405, errStr)
				}
				return restOK(200, map[string]interface{}{"sha": sha, "merged": true, "message": fmt.Sprintf("Pull Request #%d merged", pr.Number)}, nil)
			}
			return restErr(404, "Not Found")
		}

		return restErr(404, "Not Found")
	}

	return restErr(404, "Not Found")
}

func strPtr(s string) *string { return &s }

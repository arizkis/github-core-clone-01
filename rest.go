package main

import (
	"encoding/base64"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

var hotIssuePattern = regexp.MustCompile(`^/api/v3/repos/([^/?]+)/([^/?]+)/issues/(\d+)$`)
var createIssuePattern = regexp.MustCompile(`^/api/v3/repos/([^/?]+)/([^/?]+)/issues$`)
var issueDotPattern = regexp.MustCompile(`(?i)/(?:\.|%2e){1,2}/`)
var lastIssueHost string
var hasLastIssueHost bool

func dispatch(req *ForgeRequest) *ForgeResponse {
	store.mu.Lock()
	defer store.mu.Unlock()
	requestID := store.RequestHeader()
	rawPath, rawQuery, _ := strings.Cut(req.URL, "?")
	authorized := func() bool { return tokenIsAuthorized(req.Headers["authorization"]) }
	if req.Method == "GET" && strings.HasPrefix(rawPath, "/api/v3/repos/") {
		if m := hotIssuePattern.FindStringSubmatch(rawPath); m != nil && authorized() {
			r := store.Repository(decodeSegment(m[1]), decodeSegment(m[2]))
			if r != nil && r.Owner == viewerLogin {
				if i := issueAt(r, m[3]); i != nil {
					return sendJSON(200, rawJSON(i.Serialized), requestID)
				}
			}
		}
	}
	if req.Method == "POST" && strings.HasPrefix(rawPath, "/api/v3/repos/") && !strings.ContainsAny(rawPath, "\\#\t\r\n") && !issueDotPattern.MatchString(rawPath) {
		if m := createIssuePattern.FindStringSubmatch(rawPath); m != nil {
			host, ok := req.Headers["host"]
			if !ok {
				host = "entry"
			}
			if !hasLastIssueHost || host != lastIssueHost {
				if _, valid := parseForgeURL("", host); !valid {
					return fail(400, "Invalid URL", requestID)
				}
				lastIssueHost = host
				hasLastIssueHost = true
			}
			if !authorized() {
				return fail(401, "Bad credentials", requestID)
			}
			if req.BodyError {
				return fail(413, "Request body too large", requestID)
			}
			return createIssueFast(decodeSegment(m[1]), decodeSegment(m[2]), req.Body, requestID)
		}
	}
	if isGitRoute(rawPath) {
		if !authorized() {
			return fail(401, "Bad credentials", requestID)
		}
		var data []byte
		if req.Method == "POST" {
			if req.BodyError {
				return fail(413, "Request body too large", requestID)
			}
			data = req.Body
		}
		return handleGit(req, rawPath, rawQuery, data)
	}
	host, ok := req.Headers["host"]
	if !ok {
		host = "entry"
	}
	u, valid := parseForgeURL(req.URL, host)
	if !valid {
		return fail(400, "Invalid URL", requestID)
	}
	if req.Method == "GET" && u.Pathname == "/healthz" {
		return sendJSON(200, obj("ok", true), requestID)
	}
	if req.Method == "GET" && (u.Pathname == "/api/v3" || u.Pathname == "/api/v3/") {
		if !authorized() {
			return fail(401, "Bad credentials", requestID)
		}
		return sendJSON(200, obj("current_user_url", "/api/v3/user", "rate_limit_url", "/api/v3/rate_limit"), requestID, header{"x-oauth-scopes", "repo, read:org"})
	}
	if u.Pathname == "/mcp" && req.Method == "POST" {
		if req.BodyError {
			return fail(400, "Request body too large", requestID)
		}
		return handleMcp(req.Body, authorized())
	}
	if u.Pathname == "/api/graphql" && req.Method == "POST" {
		if !authorized() {
			return fail(401, "Bad credentials", requestID)
		}
		if req.BodyError {
			return fail(413, "Request body too large", requestID)
		}
		if strings.Contains(req.Headers["content-type"], "json") {
			valid := false
			func() { defer func() { _ = recover() }(); parseJSON(req.Body); valid = true }()
			if !valid {
				return fail(400, "Invalid JSON body", requestID)
			}
		}
		return handleGraphql(req.Body, requestID)
	}
	if strings.HasPrefix(u.Pathname, "/api/") {
		if !authorized() {
			return fail(401, "Bad credentials", requestID)
		}
		var data []byte
		if slices.Contains([]string{"POST", "PATCH", "PUT", "DELETE"}, req.Method) {
			if req.BodyError {
				return fail(413, "Request body too large", requestID)
			}
			data = req.Body
		}
		if res := handleRest(req, u, data, requestID); res != nil {
			return res
		}
	}
	return fail(404, "Not Found", requestID)
}
func recoveredResponse(id string, cause any) *ForgeResponse {
	message := "Invalid request"
	switch e := cause.(type) {
	case string:
		message = e
	case error:
		message = e.Error()
	}
	return fail(400, message, id)
}
func createIssueFast(owner, name string, data []byte, id string) (res *ForgeResponse) {
	defer func() {
		if cause := recover(); cause != nil {
			res = recoveredResponse(id, cause)
		}
	}()
	r := store.Repository(owner, name)
	if r == nil || r.Owner != viewerLogin {
		return fail(404, "Not Found", id)
	}
	return createIssueResponse(r, parseBody(data), id)
}
func createIssueResponse(r *Repository, b body, id string) *ForgeResponse {
	labels := make([]*Label, 0)
	if names, ok := b["labels"].([]any); ok {
		for _, v := range names {
			name := jsString(v)
			label := r.Labels.Get(jsLower(name))
			if label == nil {
				return fail(422, "Label "+name+" does not exist", id)
			}
			labels = append(labels, label)
		}
	}
	i := store.CreateIssue(r, b.str("title", ""), b.str("body", ""), labels)
	return sendJSON(201, rawJSON(i.SerializedResponse), id)
}
func part(path []string, index int) string {
	if index < len(path) {
		return path[index]
	}
	return ""
}
func issueAt(r *Repository, s string) *Issue {
	n := jsNumber(s)
	if math.IsNaN(n) || math.IsInf(n, 0) || n != math.Trunc(n) || n < 1 || n > float64(r.NextIssueNumber) {
		return nil
	}
	return r.Issues.Get(int(n))
}
func pullAt(r *Repository, s string) *PullRequest {
	n := jsNumber(s)
	if math.IsNaN(n) || math.IsInf(n, 0) || n != math.Trunc(n) || n < 1 || n > float64(r.NextPullNumber) {
		return nil
	}
	return r.Pulls.Get(int(n))
}
func pagination(u *forgeURL) (page, per float64) {
	page = jsNumber(u.Get("page", ""))
	if page == 0 || math.IsNaN(page) {
		page = 1
	}
	page = math.Max(1, page)
	per = jsNumber(u.Get("per_page", ""))
	if per == 0 || math.IsNaN(per) {
		per = 30
	}
	per = math.Max(1, math.Min(100, per))
	return
}
func pageSlice(rows []any, page, per float64) []any {
	start, end := (page-1)*per, page*per
	toIndex := func(v float64) int {
		if math.IsNaN(v) || v <= 0 {
			return 0
		}
		if v >= float64(len(rows)) {
			return len(rows)
		}
		return int(v)
	}
	a, b := toIndex(start), toIndex(end)
	if b < a {
		b = a
	}
	return rows[a:b]
}
func link(u *forgeURL, page, per float64, total int) []header {
	if page*per >= float64(total) {
		return nil
	}
	query := querySet(u.Query, "per_page", jsNumberString(per))
	query = querySet(query, "page", jsNumberString(page+1))
	return []header{{"link", "<" + u.Origin + u.Pathname + "?" + encodeQuery(query) + ">; rel=\"next\""}}
}
func cached(r *Repository, key string, value any) rawJSON { return store.Cached(r, key, value) }

func handleRest(req *ForgeRequest, u *forgeURL, data []byte, id string) (res *ForgeResponse) {
	if !strings.HasPrefix(u.Pathname, "/api/v3/") {
		return nil
	}
	path := strings.Split(strings.TrimPrefix(u.Pathname, "/api/v3/"), "/")
	// decodeURIComponent is outside the TypeScript route's try/catch.
	for i := range path {
		path[i] = decodeSegment(path[i])
	}
	defer func() {
		if cause := recover(); cause != nil {
			res = recoveredResponse(id, cause)
		}
	}()
	method := req.Method
	body := func() body { return parseBody(data) }
	if method == "GET" && part(path, 0) == "user" && len(path) == 1 {
		return sendJSON(200, obj("login", viewerLogin, "id", 1000, "node_id", globalID("User", 1000)), id)
	}
	if method == "GET" && part(path, 0) == "meta" {
		return sendJSON(200, obj("installed_version", "3.0.0"), id)
	}
	if part(path, 0) == "user" && part(path, 1) == "repos" {
		if method == "POST" {
			b := body()
			name, ok := b.stringValue("name")
			if !ok || name == "" {
				return fail(422, "Repository name is required", id)
			}
			r := store.CreateRepository(viewerLogin, name, b.str("description", ""), b["private"] == true)
			return sendJSON(201, publicRepo(r), id)
		}
		if method == "GET" {
			rows := make([]any, 0)
			for _, k := range store.Repositories.keys {
				r := store.Repositories.Get(k)
				if r.Owner == viewerLogin {
					rows = append(rows, publicRepo(r))
				}
			}
			page, per := pagination(u)
			return sendJSON(200, pageSlice(rows, page, per), id, link(u, page, per, len(rows))...)
		}
	}
	if part(path, 0) != "repos" || len(path) < 3 {
		return fail(404, "Not Found", id)
	}
	r := store.Repository(path[1], path[2])
	if r == nil || r.Owner != viewerLogin {
		return fail(404, "Not Found", id)
	}
	tail := path[3:]
	if len(tail) == 0 {
		switch method {
		case "GET":
			return sendJSON(200, cached(r, "repository", publicRepo(r)), id)
		case "PATCH":
			b := body()
			if description, ok := b.stringValue("description"); ok {
				r.Description = description
			}
			delete(r.Cache, "repository")
			return sendJSON(200, publicRepo(r), id)
		case "DELETE":
			store.Repositories.Delete(jsLower(r.Owner) + "/" + jsLower(r.Name))
			return &ForgeResponse{Status: 204, Headers: []header{{"content-type", jsonType}, {"x-github-request-id", id}}}
		}
	}
	if part(tail, 0) == "git" && part(tail, 1) == "refs" && method == "POST" {
		b := body()
		name, sha := b.str("ref", ""), b.str("sha", "")
		if !(strings.HasPrefix(name, "refs/heads/") || strings.HasPrefix(name, "refs/tags/")) || lookupObject(sha) == nil {
			return fail(422, "Reference target is invalid", id)
		}
		if r.Refs.Has(name) {
			return fail(422, "Reference already exists", id)
		}
		r.Refs.Set(name, sha)
		r.RefsVersion++
		store.Invalidate(r, func(k string) bool { return strings.HasPrefix(k, "branches:") })
		nodeID := globalID("Ref", store.NextID)
		store.NextID++
		return sendJSON(201, obj("ref", name, "node_id", nodeID, "object", obj("sha", sha, "type", "commit", "url", "")), id)
	}
	if part(tail, 0) == "branches" && method == "GET" {
		rows := make([]any, 0)
		for _, name := range r.Refs.keys {
			if strings.HasPrefix(name, "refs/heads/") {
				rows = append(rows, obj("name", name[11:], "commit", obj("sha", r.Refs.Get(name))))
			}
		}
		return sendJSON(200, cached(r, "branches:"+u.Search, rows), id)
	}
	if part(tail, 0) == "commits" && method == "GET" {
		tip := ref(r, u.Get("sha", r.DefaultBranch))
		rows := make([]any, 0)
		current := tip
		for current != zeroOID && len(rows) < 100 {
			o := lookupObject(current)
			if o == nil || o.Type != "commit" {
				break
			}
			c := parseCommit(o.Data)
			n, _ := strconv.ParseInt(current[:8], 16, 64)
			rows = append(rows, obj("sha", current, "node_id", globalID("Commit", int(n)), "commit", obj("message", strings.TrimSuffix(c.Message, "\n"), "tree", obj("sha", c.Tree), "author", obj("date", fixedTime), "committer", obj("date", fixedTime))))
			current = zeroOID
			if len(c.Parents) > 0 {
				current = c.Parents[0]
			}
		}
		return sendJSON(200, cached(r, "commits:"+tip, rows), id)
	}
	if part(tail, 0) == "contents" && len(tail) > 1 {
		filePath := strings.Join(tail[1:], "/")
		branch := ""
		if u.Has("branch") {
			branch = u.Get("branch", "")
		} else if u.Has("ref") {
			branch = u.Get("ref", "")
		} else if method == "PUT" || method == "DELETE" {
			branch = body().str("branch", r.DefaultBranch)
		} else {
			branch = r.DefaultBranch
		}
		tip := ref(r, branch)
		files := newOrderedMap[string, string]()
		if tip != zeroOID {
			files = flatten(commitTree(tip))
		}
		switch method {
		case "PUT":
			b := body()
			current := files.Get(filePath)
			if current != "" && b["sha"] != current {
				return fail(409, "SHA does not match", id)
			}
			if current == "" && jsTruthy(b["sha"]) {
				return fail(409, "File does not exist at supplied SHA", id)
			}
			content := decodeBase64(b.str("content", ""))
			message := b.str("message", "update file")
			result := updatePath(r, branch, filePath, content, message, false)
			status := 201
			if current != "" {
				status = 200
			}
			return sendJSON(status, obj("content", obj("name", tail[len(tail)-1], "path", filePath, "sha", result.Blob, "encoding", "base64", "content", base64.StdEncoding.EncodeToString(content), "size", len(content)), "commit", obj("sha", result.Commit, "message", message, "tree", obj("sha", commitTree(result.Commit)))), id)
		case "DELETE":
			b := body()
			current := files.Get(filePath)
			if current == "" {
				return fail(404, "File not found", id)
			}
			if b["sha"] != current {
				return fail(409, "SHA does not match", id)
			}
			message := b.str("message", "delete file")
			result := updatePath(r, branch, filePath, nil, message, true)
			return sendJSON(200, obj("commit", obj("sha", result.Commit, "message", message)), id)
		case "GET":
			if sha := files.Get(filePath); sha != "" {
				content := store.GitObjects.Get(sha)
				return sendJSON(200, cached(r, "content:"+branch+":"+filePath, obj("type", "file", "encoding", "base64", "content", base64.StdEncoding.EncodeToString(content), "sha", sha, "size", len(content), "path", filePath)), id)
			}
			prefix := filePath + "/"
			entries := newOrderedMap[string, object]()
			for _, path := range files.keys {
				if strings.HasPrefix(path, prefix) {
					rest := strings.TrimPrefix(path, prefix)
					name, _, dir := strings.Cut(rest, "/")
					kind := "file"
					if dir {
						kind = "dir"
					}
					entries.Set(name, obj("name", name, "path", prefix+name, "type", kind, "sha", files.Get(path)))
				}
			}
			if len(entries.keys) > 0 {
				rows := make([]any, 0, len(entries.keys))
				for _, k := range entries.keys {
					rows = append(rows, entries.Get(k))
				}
				return sendJSON(200, rows, id)
			}
			return fail(404, "File not found", id)
		}
	}
	if part(tail, 0) == "labels" {
		if method == "POST" {
			b := body()
			name := b.str("name", "")
			if name == "" {
				return fail(422, "Label name is required", id)
			}
			label := &Label{store.NextID, name, b.str("color", "ededed")}
			store.NextID++
			r.Labels.Set(jsLower(name), label)
			return sendJSON(201, label.Public(), id)
		}
		if method == "GET" {
			rows := make([]any, 0)
			for _, k := range r.Labels.keys {
				rows = append(rows, r.Labels.Get(k).Public())
			}
			return sendJSON(200, rows, id)
		}
	}
	if part(tail, 0) == "issues" && len(tail) == 1 && method == "POST" {
		return createIssueResponse(r, body(), id)
	}
	if part(tail, 0) == "issues" && len(tail) == 1 && method == "GET" {
		state, sort, dir := u.Get("state", "open"), u.Get("sort", "created"), u.Get("direction", "desc")
		issues := make([]*Issue, 0)
		for _, n := range r.Issues.keys {
			i := r.Issues.Get(n)
			if state == "all" || i.State == state {
				issues = append(issues, i)
			}
		}
		slices.SortFunc(issues, func(a, b *Issue) int {
			if dir == "desc" {
				return b.Number - a.Number
			}
			return a.Number - b.Number
		})
		rows := make([]any, 0, len(issues))
		for _, i := range issues {
			rows = append(rows, publicIssue(i))
		}
		page, per := pagination(u)
		key := "issues:" + state + ":" + sort + ":" + dir + ":" + jsNumberString(page) + ":" + jsNumberString(per)
		return sendJSON(200, cached(r, key, pageSlice(rows, page, per)), id, link(u, page, per, len(rows))...)
	}
	if part(tail, 0) == "issues" && len(tail) >= 2 {
		i := issueAt(r, tail[1])
		if i == nil {
			return fail(404, "Issue not found", id)
		}
		if len(tail) == 2 && method == "GET" {
			return sendJSON(200, rawJSON(i.Serialized), id)
		}
		if len(tail) == 2 && method == "PATCH" {
			b := body()
			if title, ok := b.stringValue("title"); ok {
				i.Title = title
			}
			if text, ok := b.stringValue("body"); ok {
				i.Body = text
			}
			if b["state"] == "open" || b["state"] == "closed" {
				i.State = b["state"].(string)
			}
			if names, ok := b["labels"].([]any); ok {
				labels := make([]*Label, 0, len(names))
				for _, name := range names {
					label := r.Labels.Get(jsLower(jsString(name)))
					if label == nil {
						store.Invalidate(r, func(k string) bool { return strings.HasPrefix(k, "issues:") })
						refreshIssue(i)
						return fail(422, "Unknown label", id)
					}
					labels = append(labels, label)
				}
				i.Labels = labels
			}
			store.Invalidate(r, func(k string) bool { return strings.HasPrefix(k, "issues:") })
			refreshIssue(i)
			return sendJSON(200, rawJSON(i.SerializedResponse), id)
		}
		if part(tail, 2) == "comments" && len(tail) == 3 && method == "POST" {
			b := body()
			n := i.Number
			c := &Comment{ID: store.NextID, Body: b.str("body", ""), CreatedAt: fixedTime, IssueNumber: &n}
			store.NextID++
			r.Comments.Set(c.ID, c)
			i.Comments++
			store.Invalidate(r, func(k string) bool { return strings.HasPrefix(k, "issues:") })
			refreshIssue(i)
			return sendJSON(201, c.Public(), id)
		}
		if part(tail, 2) == "comments" && len(tail) == 3 && method == "GET" {
			rows := make([]any, 0)
			for _, n := range r.Comments.keys {
				c := r.Comments.Get(n)
				if c.IssueNumber != nil && *c.IssueNumber == i.Number {
					rows = append(rows, c.Public())
				}
			}
			return sendJSON(200, rows, id)
		}
	}
	if part(tail, 0) == "pulls" && len(tail) == 1 && method == "POST" {
		b := body()
		head, base := b.str("head", ""), b.str("base", r.DefaultBranch)
		if !r.Refs.Has("refs/heads/"+head) || !r.Refs.Has("refs/heads/"+base) {
			return fail(422, "Head or base branch does not exist", id)
		}
		p := store.CreatePull(r, b.str("title", ""), b.str("body", ""), head, base)
		mergeable := pullMergeable(p)
		p.Mergeable = &mergeable
		return sendJSON(201, publicPull(p), id)
	}
	if part(tail, 0) == "pulls" && len(tail) == 1 && method == "GET" {
		state := u.Get("state", "open")
		rows := make([]any, 0)
		for _, n := range r.Pulls.keys {
			p := r.Pulls.Get(n)
			if state == "all" || p.State == state {
				rows = append(rows, publicPull(p))
			}
		}
		page, per := pagination(u)
		return sendJSON(200, cached(r, "pulls:"+state+":"+jsNumberString(page)+":"+jsNumberString(per), pageSlice(rows, page, per)), id, link(u, page, per, len(rows))...)
	}
	if part(tail, 0) == "pulls" && len(tail) >= 2 {
		p := pullAt(r, tail[1])
		if p == nil {
			return fail(404, "Pull request not found", id)
		}
		if len(tail) == 2 && method == "GET" {
			return sendJSON(200, cached(r, fmt.Sprintf("pull:%d", p.Number), publicPull(p)), id)
		}
		paths := pullFiles(p)
		if part(tail, 2) == "files" && method == "GET" {
			rows := make([]any, 0, len(paths))
			for _, f := range paths {
				rows = append(rows, obj("filename", f.Path, "status", f.Status, "additions", f.Additions, "deletions", f.Deletions, "changes", f.Additions+f.Deletions))
			}
			return sendJSON(200, rows, id)
		}
		if part(tail, 2) == "merge" && method == "PUT" {
			if p.Mergeable != nil && !*p.Mergeable {
				return fail(405, "Pull request is not mergeable", id)
			}
			sha := mergePull(p)
			if sha == "" {
				return fail(405, "Pull request is not mergeable", id)
			}
			p.State = "closed"
			p.Merged = true
			stamp := fixedTime
			p.MergedAt = &stamp
			p.MergeCommitSHA = &sha
			p.UpdatedAt = stamp
			store.Invalidate(r, func(k string) bool {
				return k == fmt.Sprintf("pull:%d", p.Number) || strings.HasPrefix(k, "pulls:") || strings.HasPrefix(k, "branches:")
			})
			return sendJSON(200, obj("sha", sha, "merged", true, "message", "Pull request successfully merged"), id)
		}
	}
	return fail(404, "Not Found", id)
}

func decodeBase64(s string) []byte {
	// Buffer.from(..., 'base64') accepts URL-safe symbols and skips other text.
	out := make([]byte, 0, len(s)*3/4)
	var bits uint32
	count := 0
	for _, u := range jsUTF16(s) {
		c := byte(u)
		if c == '=' {
			break
		}
		v := -1
		switch {
		case c >= 'A' && c <= 'Z':
			v = int(c - 'A')
		case c >= 'a' && c <= 'z':
			v = int(c-'a') + 26
		case c >= '0' && c <= '9':
			v = int(c-'0') + 52
		case c == '+' || c == '-':
			v = 62
		case c == '/' || c == '_':
			v = 63
		}
		if v < 0 {
			continue
		}
		bits = bits<<6 | uint32(v)
		count += 6
		if count >= 8 {
			count -= 8
			out = append(out, byte(bits>>count))
		}
	}
	return out
}

type pullFile struct {
	Path, Status         string
	Additions, Deletions int
	Old, Next            string
}

func unionKeys(maps ...*orderedMap[string, string]) []string {
	out := make([]string, 0)
	seen := make(map[string]bool)
	for _, m := range maps {
		for _, k := range m.keys {
			if !seen[k] {
				seen[k] = true
				out = append(out, k)
			}
		}
	}
	return out
}
func ancestors(start string) *orderedMap[string, bool] {
	set := newOrderedMap[string, bool]()
	pending := []string{start}
	for len(pending) > 0 {
		h := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if set.Has(h) || h == zeroOID {
			continue
		}
		set.Set(h, true)
		if o := lookupObject(h); o != nil && o.Type == "commit" {
			pending = append(pending, parseCommit(o.Data).Parents...)
		}
	}
	return set
}
func nonemptyLines(data []byte) int {
	n := 0
	for _, line := range strings.Split(utf8Text(data), "\n") {
		if line != "" {
			n++
		}
	}
	return n
}
func pullFiles(p *PullRequest) []pullFile {
	base, head := flatten(commitTree(p.Base.SHA)), flatten(commitTree(p.Head.SHA))
	out := make([]pullFile, 0)
	for _, path := range unionKeys(base, head) {
		a, b := base.Get(path), head.Get(path)
		if a == b {
			continue
		}
		f := pullFile{Path: path, Status: "modified", Old: a, Next: b}
		if a == "" {
			f.Status = "added"
		} else if b == "" {
			f.Status = "removed"
		}
		if a != "" {
			f.Deletions = nonemptyLines(store.GitObjects.Get(a))
		}
		if b != "" {
			f.Additions = nonemptyLines(store.GitObjects.Get(b))
		}
		out = append(out, f)
	}
	return out
}
func pullMergeable(p *PullRequest) bool {
	baseTip := ref(p.Repository, p.Base.Ref)
	baseAnc, headAnc := ancestors(baseTip), ancestors(p.Head.SHA)
	common := baseTip
	for _, h := range headAnc.keys {
		if baseAnc.Has(h) {
			common = h
			break
		}
	}
	base, ours, theirs := flatten(commitTree(common)), flatten(commitTree(baseTip)), flatten(commitTree(p.Head.SHA))
	for _, path := range unionKeys(base, ours, theirs) {
		b, o, t := base.Get(path), ours.Get(path), theirs.Get(path)
		if o != b && t != b && o != t {
			return false
		}
	}
	return true
}
func mergePull(p *PullRequest) string {
	r := p.Repository
	baseTip := ref(r, p.Base.Ref)
	mergeable := pullMergeable(p)
	p.Mergeable = &mergeable
	if !mergeable {
		return ""
	}
	base, theirs := flatten(commitTree(baseTip)), flatten(commitTree(p.Head.SHA))
	result := base.Clone()
	for _, path := range unionKeys(base, theirs) {
		b, t := base.Get(path), theirs.Get(path)
		if t != b {
			if t != "" {
				result.Set(path, t)
			} else {
				result.Delete(path)
			}
		}
	}
	tree := writeTree(result)
	sha := createCommit(tree, &baseTip, fmt.Sprintf("Squash merge pull request #%d", p.Number))
	r.Refs.Set("refs/heads/"+p.Base.Ref, sha)
	r.RefsVersion++
	p.MergeCommitSHA = &sha
	return sha
}

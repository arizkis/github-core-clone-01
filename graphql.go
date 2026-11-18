package main

import (
	"encoding/base64"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

var graphTypeFields = map[string][]string{
	"Issue":       {"id", "number", "title", "state", "body"},
	"PullRequest": {"id", "number", "headRefName", "baseRefName", "mergeable", "state", "merged", "files"},
	"Repository":  {"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequests", "pullRequest"},
	"User":        {"login"}, "IssueConnection": {"nodes", "pageInfo"}, "PageInfo": {"hasNextPage", "endCursor"},
	"PullRequestConnection": {"nodes"}, "PullRequestChangedFile": {"path"},
	"__Type": {"name", "fields", "enumValues"}, "__Field": {"name"}, "__EnumValue": {"name"},
	"IssueState": {"OPEN", "CLOSED"}, "SearchType": {"ISSUE", "REPOSITORY", "USER", "COMMIT", "ISSUE", "DISCUSSION"},
}
var graphKnown = map[string][]string{"RootQuery": {"viewer", "repository", "node", "__type"}, "RootMutation": {"createIssue", "closeIssue", "createPullRequest", "mergePullRequest", "addComment"}}
var graphTokens = regexp.MustCompile(`\.\.\.|"(?:\\[^\n\r\x{2028}\x{2029}]|[^"\\])*"|[_A-Za-z][_0-9A-Za-z]*|-?\d+|[!$():=@\[\]{},]`)
var graphInteger = regexp.MustCompile(`^-?\d+$`)
var graphMutation = regexp.MustCompile(`^mutation\b`)

type graphField struct {
	Name, Alias string
	Args        body
	Children    []graphField
}
type graphParser struct {
	tokens    []string
	at        int
	variables any
}

func (p *graphParser) peek() string {
	if p.at < len(p.tokens) {
		return p.tokens[p.at]
	}
	return ""
}
func (p *graphParser) take() string { v := p.peek(); p.at++; return v }
func (p *graphParser) document() []graphField {
	for p.peek() != "" && p.peek() != "{" {
		if p.take() == "(" {
			n := 1
			for n > 0 && p.peek() != "" {
				t := p.take()
				if t == "(" {
					n++
				}
				if t == ")" {
					n--
				}
			}
		}
	}
	return p.selection()
}
func (p *graphParser) selection() []graphField {
	if p.take() != "{" {
		panic("Expected selection set")
	}
	out := make([]graphField, 0)
	for p.peek() != "" && p.peek() != "}" {
		if p.peek() == "..." {
			p.take()
			if p.take() == "on" {
				p.take()
				out = append(out, p.selection()...)
			}
			continue
		}
		first := p.take()
		f := graphField{Name: first, Alias: first, Args: make(body)}
		if p.peek() == ":" {
			p.take()
			f.Name = p.take()
		}
		if p.peek() == "(" {
			p.take()
			for p.peek() != "" && p.peek() != ")" {
				key := p.take()
				if key == "," {
					continue
				}
				if p.take() != ":" {
					panic("Expected argument colon")
				}
				f.Args[key] = p.value()
			}
			p.take()
		}
		if p.peek() == "{" {
			f.Children = p.selection()
		}
		out = append(out, f)
	}
	p.take()
	return out
}
func (p *graphParser) value() any {
	t := p.take()
	switch t {
	case "$":
		return jsProperty(p.variables, p.take())
	case "[":
		out := make([]any, 0)
		for p.peek() != "" && p.peek() != "]" {
			if p.peek() == "," {
				p.take()
				continue
			}
			out = append(out, p.value())
		}
		p.take()
		return out
	case "{":
		out := make(body)
		for p.peek() != "" && p.peek() != "}" {
			if p.peek() == "," {
				p.take()
				continue
			}
			key := p.take()
			if p.take() != ":" {
				panic("Expected input colon")
			}
			if key == "__proto__" {
				p.value()
			} else {
				out[key] = p.value()
			}
		}
		p.take()
		return out
	case "true":
		return true
	case "false":
		return false
	case "null":
		return nil
	}
	if strings.HasPrefix(t, "\"") {
		return parseJSON([]byte(t))
	}
	if graphInteger.MatchString(t) {
		return jsNumber(t)
	}
	return t
}
func jsProperty(value any, key string) any {
	switch v := value.(type) {
	case *inputObject:
		if x, ok := v.values[key]; ok {
			return x
		}
	case body:
		if x, ok := v[key]; ok {
			return x
		}
	case object:
		for _, f := range v {
			if f.name == key {
				return f.value
			}
		}
	case graphValue:
		if key == "__type" {
			return v.Kind
		}
		return jsProperty(v.Fields, key)
	case []any:
		if key == "length" {
			return len(v)
		}
		if n, ok := objectIndex(key); ok && n < uint64(len(v)) {
			return v[n]
		}
	case string:
		if key == "length" {
			return len(jsUTF16(v))
		}
	}
	if !isNullish(value) {
		switch key {
		case "toString", "toLocaleString", "valueOf", "constructor", "hasOwnProperty", "isPrototypeOf", "propertyIsEnumerable", "__defineGetter__", "__defineSetter__", "__lookupGetter__", "__lookupSetter__":
			return jsFunction{}
		case "__proto__":
			return object{}
		}
	}
	return undefinedValue
}
func graphRecord(value any) any {
	switch value.(type) {
	case *inputObject, body, []any:
		return value
	}
	return body{}
}
func nullishDefault(value, fallback any) any {
	if isNullish(value) {
		return fallback
	}
	return value
}
func graphString(value any, fallback string) string { return jsString(nullishDefault(value, fallback)) }
func graphArg(args body, key string) any {
	if value, ok := args[key]; ok {
		return value
	}
	return undefinedValue
}
func jsSlice(rows []any, start, end float64) []any {
	index := func(n float64) int {
		if math.IsNaN(n) {
			return 0
		}
		n = math.Trunc(n)
		if n < 0 {
			n = math.Max(float64(len(rows))+n, 0)
		}
		if n > float64(len(rows)) {
			return len(rows)
		}
		return int(n)
	}
	a, b := index(start), index(end)
	if b < a {
		b = a
	}
	return rows[a:b]
}

type graphValue struct {
	Kind   string
	Repo   *Repository
	Issue  *Issue
	Pull   *PullRequest
	Fields object
}

func graphObj(kind string, fields ...any) graphValue {
	return graphValue{Kind: kind, Fields: obj(fields...)}
}
func graphRepo(r *Repository) any {
	if r == nil {
		return nil
	}
	return graphValue{Kind: "Repository", Repo: r}
}
func graphIssue(i *Issue) any {
	if i == nil {
		return nil
	}
	return graphValue{Kind: "Issue", Issue: i}
}
func graphPull(p *PullRequest) any {
	if p == nil {
		return nil
	}
	return graphValue{Kind: "PullRequest", Pull: p}
}
func graphIssueNumber(r *Repository, value any) *Issue {
	n := jsNumber(value)
	if math.IsNaN(n) || math.IsInf(n, 0) || n != math.Trunc(n) || n < 1 || n >= float64(r.NextIssueNumber) {
		return nil
	}
	return r.Issues.Get(int(n))
}
func graphPullNumber(r *Repository, value any) *PullRequest {
	n := jsNumber(value)
	if math.IsNaN(n) || math.IsInf(n, 0) || n != math.Trunc(n) || n < 1 || n >= float64(r.NextPullNumber) {
		return nil
	}
	return r.Pulls.Get(int(n))
}
func graphObjectType(value any) string {
	if v, ok := value.(graphValue); ok {
		return v.Kind
	}
	return "Object"
}
func graphID(value any) (string, float64, bool) {
	decoded := utf8Text(decodeBase64(graphString(value, "")))
	at := strings.LastIndexByte(decoded, ':')
	if at < 0 {
		return "", 0, false
	}
	return decoded[:at], jsNumber(decoded[at+1:]), true
}
func graphRepoByID(value any) *Repository {
	kind, id, ok := graphID(value)
	if !ok || kind != "Repository" {
		return nil
	}
	for _, k := range store.Repositories.keys {
		r := store.Repositories.Get(k)
		if float64(r.ID) == id {
			return r
		}
	}
	return nil
}
func graphIssueByID(value any) *Issue {
	kind, id, ok := graphID(value)
	if !ok || kind != "Issue" {
		return nil
	}
	for _, k := range store.Repositories.keys {
		r := store.Repositories.Get(k)
		for _, n := range r.Issues.keys {
			i := r.Issues.Get(n)
			if float64(i.ID) == id {
				return i
			}
		}
	}
	return nil
}
func graphPullByID(value any) *PullRequest {
	kind, id, ok := graphID(value)
	if !ok || kind != "PullRequest" {
		return nil
	}
	for _, k := range store.Repositories.keys {
		r := store.Repositories.Get(k)
		for _, n := range r.Pulls.keys {
			p := r.Pulls.Get(n)
			if float64(p.ID) == id {
				return p
			}
		}
	}
	return nil
}
func graphNode(value any) any {
	kind, id, ok := graphID(value)
	if !ok {
		return nil
	}
	for _, k := range store.Repositories.keys {
		r := store.Repositories.Get(k)
		if kind == "Repository" && float64(r.ID) == id {
			return graphRepo(r)
		}
		for _, n := range r.Issues.keys {
			i := r.Issues.Get(n)
			if kind == "Issue" && float64(i.ID) == id {
				return graphIssue(i)
			}
		}
		for _, n := range r.Pulls.keys {
			p := r.Pulls.Get(n)
			if kind == "PullRequest" && float64(p.ID) == id {
				return graphPull(p)
			}
		}
	}
	return nil
}
func graphCursor(index float64) string {
	return base64.StdEncoding.EncodeToString([]byte("issue:" + jsNumberString(index)))
}
func graphCursorIndex(cursor any) float64 {
	s, ok := cursor.(string)
	if !ok {
		return -1
	}
	text := utf8Text(decodeBase64(s))
	n := jsNumber(text[strings.LastIndexByte(text, ':')+1:])
	if math.IsNaN(n) || math.IsInf(n, 0) || n != math.Trunc(n) {
		return -1
	}
	return n
}
func graphNumberOrZero(value any) float64 {
	n := jsNumber(value)
	if n == 0 || math.IsNaN(n) {
		return 0
	}
	return n
}
func graphResolve(value any, f graphField) any {
	kind, args := graphObjectType(value), f.Args
	v, _ := value.(graphValue)
	if kind == "RootQuery" {
		switch f.Name {
		case "viewer":
			return graphObj("User", "login", viewerLogin)
		case "repository":
			return graphRepo(store.Repository(graphString(graphArg(args, "owner"), ""), graphString(graphArg(args, "name"), "")))
		case "node":
			return graphNode(graphArg(args, "id"))
		case "__type":
			return graphObj("__Type", "name", graphString(graphArg(args, "name"), ""))
		}
	}
	if kind == "RootMutation" {
		return graphMutate(f)
	}
	if kind == "Repository" {
		r := v.Repo
		switch f.Name {
		case "id", "node_id":
			return r.NodeID
		case "databaseId":
			return r.ID
		case "name":
			return r.Name
		case "nameWithOwner":
			return r.FullName
		case "owner":
			return graphObj("User", "login", r.Owner)
		case "defaultBranchRef":
			return obj("name", r.DefaultBranch)
		case "viewerPermission":
			return "ADMIN"
		case "hasIssuesEnabled":
			return true
		case "issue":
			return graphIssue(graphIssueNumber(r, graphArg(args, "number")))
		case "pullRequest":
			return graphPull(graphPullNumber(r, graphArg(args, "number")))
		case "issues":
			var states []any
			stateValue := graphArg(args, "states")
			if _, missing := stateValue.(jsUndefined); !missing {
				var array bool
				states, array = stateValue.([]any)
				if !array {
					states = []any{stateValue}
				}
			}
			rows := make([]*Issue, 0, len(r.Issues.keys))
			for _, n := range r.Issues.keys {
				i := r.Issues.Get(n)
				if len(states) == 0 || slices.ContainsFunc(states, func(x any) bool { s, ok := x.(string); return ok && s == strings.ToUpper(i.State) }) {
					rows = append(rows, i)
				}
			}
			order := graphArg(args, "orderBy")
			descending := jsProperty(order, "field") == "CREATED_AT" && jsProperty(order, "direction") == "DESC"
			slices.SortFunc(rows, func(a, b *Issue) int {
				if descending {
					return b.Number - a.Number
				}
				return a.Number - b.Number
			})
			first := math.Max(0, math.Min(100, graphNumberOrZero(nullishDefault(graphArg(args, "first"), len(rows)))))
			offset := graphCursorIndex(graphArg(args, "after")) + 1
			all := make([]any, 0, len(rows))
			for _, i := range rows {
				all = append(all, graphIssue(i))
			}
			nodes := jsSlice(all, offset, offset+first)
			var cursor any
			if len(nodes) > 0 {
				cursor = graphCursor(offset + float64(len(nodes)) - 1)
			}
			return graphObj("IssueConnection", "nodes", nodes, "pageInfo", graphObj("PageInfo", "hasNextPage", offset+float64(len(nodes)) < float64(len(rows)), "endCursor", cursor))
		case "pullRequests":
			all := make([]any, 0, len(r.Pulls.keys))
			for _, n := range r.Pulls.keys {
				all = append(all, graphPull(r.Pulls.Get(n)))
			}
			limit := math.Max(0, jsNumber(nullishDefault(graphArg(args, "first"), len(r.Pulls.keys))))
			return graphObj("PullRequestConnection", "nodes", jsSlice(all, 0, limit))
		}
	}
	if kind == "Issue" {
		i := v.Issue
		switch f.Name {
		case "id":
			return i.NodeID
		case "number":
			return i.Number
		case "title":
			return i.Title
		case "state":
			return strings.ToUpper(i.State)
		case "body":
			return i.Body
		}
		return undefinedValue
	}
	if kind == "PullRequest" {
		p := v.Pull
		switch f.Name {
		case "id":
			return p.NodeID
		case "number":
			return p.Number
		case "headRefName":
			return p.Head.Ref
		case "baseRefName":
			return p.Base.Ref
		case "mergeable":
			if pullMergeable(p) {
				return "MERGEABLE"
			}
			return "CONFLICTING"
		case "state":
			if p.Merged {
				return "MERGED"
			}
			return strings.ToUpper(p.State)
		case "merged":
			return p.Merged
		case "files":
			all := make([]any, 0)
			for _, f := range pullFiles(p) {
				all = append(all, graphObj("PullRequestChangedFile", "path", f.Path))
			}
			return graphObj("PullRequestConnection", "nodes", jsSlice(all, 0, math.Max(0, jsNumber(nullishDefault(graphArg(args, "first"), 100)))))
		}
	}
	if kind == "User" && f.Name == "login" || kind == "IssueConnection" && (f.Name == "nodes" || f.Name == "pageInfo") || kind == "PageInfo" || kind == "PullRequestConnection" && f.Name == "nodes" || kind == "PullRequestChangedFile" && f.Name == "path" {
		return jsProperty(v.Fields, f.Name)
	}
	if kind == "__Type" {
		name := jsProperty(v.Fields, "name").(string)
		if f.Name == "name" {
			return name
		}
		if f.Name == "fields" || f.Name == "enumValues" {
			if _, ok := graphTypeFields[name]; !ok {
				switch name {
				case "__proto__", "constructor", "toString", "valueOf":
					panic("(TYPE_FIELDS[t] ?? []).map is not a function")
				}
			}
			out := make([]any, 0)
			childKind := "__Field"
			if f.Name == "enumValues" {
				childKind = "__EnumValue"
			}
			for _, n := range graphTypeFields[name] {
				out = append(out, graphObj(childKind, "name", n))
			}
			return out
		}
	}
	if (kind == "__Field" || kind == "__EnumValue") && f.Name == "name" {
		return jsProperty(v.Fields, "name")
	}
	switch value.(type) {
	case graphValue, object, *inputObject, body, []any:
		if x := jsProperty(value, f.Name); !isOmitted(x) {
			return x
		} else if _, ok := x.(jsFunction); ok {
			return x
		}
	}
	panic("Cannot query field \"" + f.Name + "\" on type \"" + kind + "\".")
}
func graphMutate(f graphField) any {
	input := nullishDefault(graphArg(f.Args, "input"), body{})
	get := func(key string) any { return jsProperty(input, key) }
	switch f.Name {
	case "createIssue":
		r := graphRepoByID(get("repositoryId"))
		if r == nil {
			panic("Repository not found")
		}
		return obj("issue", graphIssue(store.CreateIssue(r, graphString(get("title"), ""), graphString(get("body"), ""), nil)))
	case "closeIssue":
		i := graphIssueByID(get("issueId"))
		if i == nil {
			panic("Issue not found")
		}
		i.State = "closed"
		refreshIssue(i)
		store.Invalidate(i.Repository, func(k string) bool { return k == "issue:"+strconv.Itoa(i.Number) || strings.HasPrefix(k, "issues:") })
		return obj("issue", graphIssue(i))
	case "createPullRequest":
		r := graphRepoByID(get("repositoryId"))
		if r == nil {
			panic("Repository not found")
		}
		p := store.CreatePull(r, graphString(get("title"), ""), graphString(get("body"), ""), graphString(get("headRefName"), ""), graphString(get("baseRefName"), r.DefaultBranch))
		mergeable := pullMergeable(p)
		p.Mergeable = &mergeable
		return obj("pullRequest", graphPull(p))
	case "mergePullRequest":
		p := graphPullByID(get("pullRequestId"))
		if p == nil {
			panic("Pull request not found")
		}
		sha := mergePull(p)
		if sha == "" {
			panic("Pull request is not mergeable")
		}
		p.State = "closed"
		p.Merged = true
		stamp := fixedTime
		p.MergedAt = &stamp
		p.MergeCommitSHA = &sha
		store.Invalidate(p.Repository, func(k string) bool {
			return k == "pull:"+strconv.Itoa(p.Number) || strings.HasPrefix(k, "pulls:") || strings.HasPrefix(k, "branches:")
		})
		return obj("pullRequest", graphPull(p))
	case "addComment":
		i, p := graphIssueByID(get("subjectId")), graphPullByID(get("subjectId"))
		if i == nil && p == nil {
			panic("Comment subject not found")
		}
		var r *Repository
		var number int
		c := &Comment{ID: store.NextID, Body: graphString(get("body"), ""), CreatedAt: fixedTime}
		store.NextID++
		if i != nil {
			r = i.Repository
			number = i.Number
			c.IssueNumber = &number
		} else {
			r = p.Repository
			number = p.Number
			c.PullNumber = &number
		}
		r.Comments.Set(c.ID, c)
		if i != nil {
			i.Comments++
			refreshIssue(i)
			store.Invalidate(r, func(k string) bool { return k == "issue:"+strconv.Itoa(i.Number) || strings.HasPrefix(k, "issues:") })
		}
		return obj("commentEdge", obj("node", obj("id", globalID("Comment", c.ID), "body", c.Body)))
	}
	panic("Cannot query field \"" + f.Name + "\" on type \"Mutation\".")
}
func graphProject(value any, fields []graphField, errors *[]any) any {
	if rows, ok := value.([]any); ok {
		out := make([]any, 0, len(rows))
		for _, row := range rows {
			out = append(out, graphProject(row, fields, errors))
		}
		return out
	}
	if isNullish(value) {
		return nil
	}
	kind := graphObjectType(value)
	out := object{}
	allowed, known := graphKnown[kind]
	if !known {
		allowed = graphTypeFields[kind]
	}
	for _, f := range fields {
		if len(allowed) > 0 && !slices.Contains(allowed, f.Name) {
			name := kind
			if name == "RootQuery" {
				name = "Query"
			} else if name == "RootMutation" {
				name = "Mutation"
			}
			*errors = append(*errors, obj("message", "Cannot query field \""+f.Name+"\" on type \""+name+"\"."))
			continue
		}
		func() {
			defer func() {
				if cause := recover(); cause != nil {
					*errors = append(*errors, obj("message", panicMessage(cause)))
					out = setObjectField(out, f.Alias, nil)
				}
			}()
			child := graphResolve(value, f)
			if len(f.Children) > 0 {
				child = graphProject(child, f.Children, errors)
			}
			out = setObjectField(out, f.Alias, child)
		}()
	}
	return out
}
func graphExecute(document string, variables any) object {
	tokens := graphTokens.FindAllString(document, -1)
	if len(tokens) == 0 {
		panic("Empty GraphQL document")
	}
	parser := graphParser{tokens: tokens, variables: variables}
	fields := parser.document()
	kind := "RootQuery"
	if graphMutation.MatchString(strings.TrimLeftFunc(document, jsSpace)) {
		kind = "RootMutation"
	}
	errors := make([]any, 0)
	data := graphProject(graphObj(kind), fields, &errors)
	if len(errors) > 0 {
		return obj("data", data, "errors", errors)
	}
	return obj("data", data)
}
func handleGraphql(data []byte, id string) (res *ForgeResponse) {
	var input any
	valid := false
	func() { defer func() { _ = recover() }(); input = parseJSON(data); valid = true }()
	if !valid {
		return &ForgeResponse{Status: 400, Headers: []header{{"content-type", jsonType}, {"x-github-request-id", id}}, Body: jsonBytes(obj("message", "Invalid JSON body"))}
	}
	defer func() {
		if cause := recover(); cause != nil {
			res = sendJSON(200, rawJSON(jsonBytes(obj("errors", []any{obj("message", panicMessage(cause))}))), id)
		}
	}()
	one := func(entry any) any {
		query, ok := jsProperty(entry, "query").(string)
		if !ok {
			return obj("errors", []any{obj("message", "A query document is required")})
		}
		return graphExecute(query, graphRecord(jsProperty(entry, "variables")))
	}
	var output any
	if rows, ok := input.([]any); ok {
		all := make([]any, 0, len(rows))
		for _, row := range rows {
			all = append(all, one(row))
		}
		output = all
	} else {
		output = one(input)
	}
	return sendJSON(200, output, id)
}

// The parser permits selections without children. Preserve the resulting
// internal-object serialization as well, including Map -> {} and Buffer JSON.
func internalRepo(r *Repository) object {
	return obj("id", r.ID, "node_id", r.NodeID, "owner", r.Owner, "name", r.Name, "full_name", r.FullName, "description", r.Description, "private", r.Private, "default_branch", r.DefaultBranch, "created_at", r.CreatedAt, "updated_at", r.UpdatedAt, "issues", object{}, "pulls", object{}, "labels", object{}, "comments", object{}, "refs", object{}, "refsVersion", r.RefsVersion, "cache", object{}, "nextIssueNumber", r.NextIssueNumber, "nextPullNumber", r.NextPullNumber)
}
func bufferJSON(data []byte) object {
	values := make([]any, 0, len(data))
	for _, b := range data {
		values = append(values, int(b))
	}
	return obj("type", "Buffer", "data", values)
}
func (v graphValue) serialized() object {
	out := obj("__type", v.Kind)
	if v.Repo != nil {
		return append(out, field{"repo", internalRepo(v.Repo)})
	}
	if i := v.Issue; i != nil {
		labels := make([]any, 0, len(i.Labels))
		for _, l := range i.Labels {
			labels = append(labels, l.Public())
		}
		return append(out, field{"issue", obj("id", i.ID, "node_id", i.NodeID, "number", i.Number, "title", i.Title, "body", i.Body, "state", i.State, "labels", labels, "created_at", i.CreatedAt, "updated_at", i.UpdatedAt, "comments", i.Comments, "repository", internalRepo(i.Repository), "serialized", bufferJSON(i.Serialized), "serializedResponse", bufferJSON(i.SerializedResponse))})
	}
	if p := v.Pull; p != nil {
		row := mcpPublicPull(p)
		row = append(row, field{"repository", internalRepo(p.Repository)})
		if p.Mergeable != nil {
			row = append(row, field{"mergeable", *p.Mergeable})
		}
		if p.MergeCommitSHA != nil {
			row = append(row, field{"merge_commit_sha", *p.MergeCommitSHA})
		}
		return append(out, field{"pull", row})
	}
	return append(out, v.Fields...)
}

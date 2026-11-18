package main

import (
	"strconv"
	"strings"
)

const mcpRevision = "2026-07-28"
const mcpLegacy = "2025-11-25"

type mcpToolDefinition struct {
	Name     string
	Required []string
	Public   object
}

func mcpTool(name, description string, properties object, required ...string) mcpToolDefinition {
	return mcpToolDefinition{name, required, obj("name", name, "description", description, "inputSchema", obj("type", "object", "properties", properties, "required", required, "additionalProperties", false))}
}
func mcpString() object { return obj("type", "string") }

var mcpTools = []mcpToolDefinition{
	mcpTool("create_repo", "Create a repository for the authenticated user.", obj("name", mcpString(), "owner", mcpString(), "description", mcpString(), "private", obj("type", "boolean")), "name"),
	mcpTool("get_repo", "Read a repository by owner and name.", obj("owner", mcpString(), "repo", mcpString()), "owner", "repo"),
	mcpTool("create_issue", "Create an issue in a repository.", obj("owner", mcpString(), "repo", mcpString(), "title", mcpString(), "body", mcpString()), "owner", "repo", "title"),
	mcpTool("list_issues", "List issues in a repository.", obj("owner", mcpString(), "repo", mcpString(), "state", obj("type", "string", "enum", []string{"open", "closed", "all"})), "owner", "repo"),
	mcpTool("create_pull_request", "Open a pull request from one branch into another.", obj("owner", mcpString(), "repo", mcpString(), "title", mcpString(), "body", mcpString(), "head", mcpString(), "base", mcpString()), "owner", "repo", "title", "head", "base"),
	mcpTool("merge_pull_request", "Merge a pull request.", obj("owner", mcpString(), "repo", mcpString(), "pull_number", obj("type", "integer")), "owner", "repo", "pull_number"),
}
var mcpToolsPublic = func() []any {
	out := make([]any, 0, len(mcpTools))
	for _, t := range mcpTools {
		out = append(out, t.Public)
	}
	return out
}()
var mcpToolsBytes = jsonBytes(obj("tools", mcpToolsPublic))

func mcpReply(status int, value any) *ForgeResponse {
	data := jsonBytes(value)
	return &ForgeResponse{Status: status, Headers: []header{{"content-type", jsonType}, {"content-length", strconv.Itoa(len(data))}}, Body: data}
}
func mcpError(id any, code int, message string) object {
	return obj("jsonrpc", "2.0", "id", nullishDefault(id, nil), "error", obj("code", code, "message", message))
}
func mcpResult(id, value any) object {
	return obj("jsonrpc", "2.0", "id", nullishDefault(id, nil), "result", value)
}
func mcpContent(value any, isError bool) object {
	out := obj("content", []any{obj("type", "text", "text", string(jsonBytes(value)))}, "structuredContent", value)
	if isError {
		out = append(out, field{"isError", true})
	}
	return out
}
func isJSONObject(value any) bool { _, ok := value.(*inputObject); return ok }
func mcpRepoFrom(args body) *Repository {
	full := []string{}
	if s, ok := args["full_name"].(string); ok {
		full = strings.Split(s, "/")
	}
	owner, ok := args["owner"].(string)
	if !ok {
		owner = part(full, 0)
	}
	name, ok := args["repo"].(string)
	if !ok {
		name, ok = args["name"].(string)
		if !ok {
			name = part(full, 1)
		}
	}
	if owner == "" || name == "" {
		return nil
	}
	return store.Repository(owner, name)
}
func mcpCallTool(tool mcpToolDefinition, args body) (any, bool) {
	missing := make([]string, 0)
	for _, key := range tool.Required {
		v, exists := args[key]
		if !exists || isNullish(v) || v == "" {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		return obj("message", "Missing required arguments: "+strings.Join(missing, ", ")), true
	}
	if tool.Name == "create_repo" {
		owner, ok := args["owner"].(string)
		if !ok {
			owner = viewerLogin
		}
		return mcpPublicRepo(store.CreateRepository(owner, jsString(args["name"]), args.str("description", ""), args["private"] == true)), false
	}
	r := mcpRepoFrom(args)
	if r == nil {
		return obj("message", "Not Found"), true
	}
	switch tool.Name {
	case "get_repo":
		return mcpPublicRepo(r), false
	case "create_issue":
		return mcpPublicIssue(store.CreateIssue(r, jsString(args["title"]), args.str("body", ""), nil)), false
	case "list_issues":
		state, ok := args["state"].(string)
		if ok {
			state = jsLower(state)
		} else {
			state = "open"
		}
		rows := make([]any, 0)
		for _, n := range r.Issues.keys {
			i := r.Issues.Get(n)
			if state == "all" || i.State == state {
				rows = append(rows, mcpPublicIssue(i))
			}
		}
		return rows, false
	case "create_pull_request":
		return mcpPublicPull(store.CreatePull(r, jsString(args["title"]), args.str("body", ""), jsString(args["head"]), jsString(args["base"]))), false
	case "merge_pull_request":
		p := graphPullNumber(r, args["pull_number"])
		if p == nil {
			return obj("message", "Pull request not found"), true
		}
		p.State = "closed"
		p.Merged = true
		stamp := p.UpdatedAt
		p.MergedAt = &stamp
		store.Invalidate(r, func(k string) bool { return k == "pull:"+strconv.Itoa(p.Number) || strings.HasPrefix(k, "pulls:") })
		sha := p.Head.SHA
		if sha == "" {
			sha = p.Base.SHA
		}
		if sha == "" {
			sha = zeroOID
		}
		return obj("merged", true, "sha", sha, "message", "Pull request successfully merged"), false
	}
	return obj("message", "Unknown tool: "+tool.Name), true
}
func mcpPublicRepo(r *Repository) object { out := publicRepo(r); return out[:len(out)-1] }
func mcpPublicIssue(i *Issue) object {
	labels := make([]any, 0, len(i.Labels))
	for _, l := range i.Labels {
		labels = append(labels, l.Public())
	}
	return obj("id", i.ID, "node_id", i.NodeID, "number", i.Number, "title", i.Title, "body", i.Body, "state", i.State, "labels", labels, "created_at", i.CreatedAt, "updated_at", i.UpdatedAt, "comments", i.Comments)
}
func mcpPublicPull(p *PullRequest) object {
	return obj("id", p.ID, "node_id", p.NodeID, "number", p.Number, "title", p.Title, "body", p.Body, "state", p.State, "merged", p.Merged, "head", p.Head.Public(), "base", p.Base.Public(), "created_at", p.CreatedAt, "updated_at", p.UpdatedAt, "merged_at", nullableString(p.MergedAt))
}
func handleMcp(data []byte, authorized bool) *ForgeResponse {
	if !authorized {
		return mcpReply(401, obj("message", "Bad credentials"))
	}
	var input any
	valid := false
	func() { defer func() { _ = recover() }(); input = parseJSON(data); valid = true }()
	if !valid {
		return mcpReply(400, mcpError(nil, -32700, "Parse error"))
	}
	id := jsProperty(input, "id")
	method, methodOK := jsProperty(input, "method").(string)
	if !isJSONObject(input) || jsProperty(input, "jsonrpc") != "2.0" || !methodOK {
		if !isJSONObject(input) {
			id = nil
		}
		return mcpReply(400, mcpError(id, -32600, "Invalid request"))
	}
	revision := ""
	meta := jsProperty(input, "_meta")
	if isJSONObject(meta) {
		revision, _ = jsProperty(meta, "protocolVersion").(string)
	}
	switch method {
	case "notifications/initialized":
		return &ForgeResponse{Status: 202}
	case "server/discover":
		return mcpReply(200, mcpResult(id, obj("_meta", obj("io.modelcontextprotocol/serverInfo", obj("name", "github-core-clone", "version", "1.0.0")), "capabilities", obj("tools", object{}), "supportedVersions", []string{mcpRevision, mcpLegacy})))
	case "initialize":
		return mcpReply(200, mcpResult(id, obj("protocolVersion", mcpLegacy, "serverInfo", obj("name", "github-core-clone", "version", "1.0.0"), "capabilities", obj("tools", object{}))))
	case "ping":
		return mcpReply(200, mcpResult(id, object{}))
	case "tools/list":
		return mcpReply(200, mcpResult(id, obj("tools", mcpToolsPublic)))
	case "tools/call":
		params := jsProperty(input, "params")
		name, ok := jsProperty(params, "name").(string)
		if !isJSONObject(params) || !ok {
			return mcpReply(200, mcpError(id, -32602, "Tool name is required"))
		}
		var tool *mcpToolDefinition
		for i := range mcpTools {
			if mcpTools[i].Name == name {
				tool = &mcpTools[i]
				break
			}
		}
		if tool == nil {
			return mcpReply(200, mcpError(id, -32601, "Unknown tool: "+name))
		}
		args := body{}
		if value, ok := jsProperty(params, "arguments").(*inputObject); ok {
			args = value.values
		}
		value, isError := mcpCallTool(*tool, args)
		return mcpReply(200, mcpResult(id, mcpContent(value, isError)))
	}
	suffix := ""
	if revision != "" {
		suffix = " (" + revision + ")"
	}
	return mcpReply(200, mcpError(id, -32601, "Method not found: "+method+suffix))
}

package main

import (
	"bytes"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
)

var gitRoutePattern = regexp.MustCompile(`^/([^/]+)/([^/]+)/(info/refs|git-upload-pack|git-receive-pack)$`)
var v2Advertisement = pktLines("version 2\n", "agent=makefun-forge/1.0\n", "ls-refs=unborn\n", "fetch=shallow wait-for-done\n", "object-format=sha1\n", "flush")

const receiveCapabilities = "report-status side-band-64k delete-refs ofs-delta agent=makefun-forge/1.0 object-format=sha1"

var depthOneCache = make(map[string][]byte)
var depthOneResponseCache = make(map[string][]byte)
var lsRefsResponseCache = make(map[string][]byte)
var reportResponseCache = make(map[string][]byte)
var advertisementCache = make(map[string]advertisementTemplate)

type fetchKey struct {
	Repository, Generation int
	Body                   string
}
type fetchFlight struct {
	ready    chan struct{}
	response *ForgeResponse
	failure  any
}
type fetchResponses struct {
	mu      sync.Mutex
	cached  map[fetchKey]*ForgeResponse
	flights map[fetchKey]*fetchFlight
}

var fetchResponseCache = fetchResponses{cached: make(map[fetchKey]*ForgeResponse), flights: make(map[fetchKey]*fetchFlight)}

func (c *fetchResponses) get(key fetchKey, build func() *ForgeResponse) *ForgeResponse {
	c.mu.Lock()
	if r := c.cached[key]; r != nil {
		c.mu.Unlock()
		return r
	}
	if flight := c.flights[key]; flight != nil {
		c.mu.Unlock()
		<-flight.ready
		if flight.failure != nil {
			panic(flight.failure)
		}
		return flight.response
	}
	f := &fetchFlight{ready: make(chan struct{})}
	c.flights[key] = f
	c.mu.Unlock()
	func() { defer func() { f.failure = recover() }(); f.response = build() }()
	c.mu.Lock()
	if f.failure == nil {
		c.cached[key] = f.response
	}
	delete(c.flights, key)
	close(f.ready)
	c.mu.Unlock()
	if f.failure != nil {
		panic(f.failure)
	}
	return f.response
}
func gitResponse(status int, contentType string, data []byte) *ForgeResponse {
	return &ForgeResponse{Status: status, Headers: []header{{"content-type", contentType}, {"content-length", strconv.Itoa(len(data))}, {"cache-control", "no-cache"}}, Body: data}
}
func gitError(status int, message string) *ForgeResponse {
	return gitResponse(status, "text/plain; charset=utf-8", []byte(message+"\n"))
}
func gitFailure(cause any) *ForgeResponse { return gitError(400, panicMessage(cause)) }
func panicMessage(cause any) string {
	switch e := cause.(type) {
	case string:
		return e
	case error:
		return e.Error()
	default:
		return jsString(e)
	}
}
func isGitRoute(path string) bool { return gitRoutePattern.MatchString(path) }
func handleGit(req *ForgeRequest, path, query string, data []byte) (res *ForgeResponse) {
	m := gitRoutePattern.FindStringSubmatch(path)
	if m == nil {
		return nil
	}
	name := strings.TrimSuffix(m[2], ".git")
	repo := store.Repository(decodeSegment(m[1]), decodeSegment(name))
	if repo == nil {
		return gitError(404, "Repository not found")
	}
	endpoint := m[3]
	if endpoint == "info/refs" && req.Method == "GET" {
		service := ""
		if strings.HasPrefix(query, "service=") && !strings.ContainsAny(query, "&%+") {
			service = query[8:]
		} else {
			for _, row := range strings.Split(query, "&") {
				k, v, _ := strings.Cut(row, "=")
				if queryUnescape(k) == "service" {
					service = queryUnescape(v)
					break
				}
			}
		}
		switch service {
		case "git-upload-pack":
			return gitResponse(200, "application/x-git-upload-pack-advertisement", v2Advertisement)
		case "git-receive-pack":
			return gitResponse(200, "application/x-git-receive-pack-advertisement", v0Advertisement(repo, service))
		default:
			return gitError(400, "Unknown git service")
		}
	}
	if endpoint == "git-upload-pack" && req.Method == "POST" {
		defer func() {
			if cause := recover(); cause != nil {
				res = gitFailure(cause)
			}
		}()
		lsKey := ""
		if bytes.Contains(data, []byte("command=ls-refs")) {
			lsKey = strconv.Itoa(repo.ID) + ":" + strconv.Itoa(repo.RefsVersion) + ":" + repo.DefaultBranch + ":" + string(data)
			if cached := lsRefsResponseCache[lsKey]; cached != nil {
				return gitResponse(200, "application/x-git-upload-pack-result", cached)
			}
		}
		commands := parseServiceRequest(data)
		command := ""
		for _, line := range commands {
			if strings.HasPrefix(line, "command=") {
				command = line[8:]
				break
			}
		}
		switch command {
		case "ls-refs":
			body := lsRefs(repo, commands)
			if lsKey != "" {
				lsRefsResponseCache[lsKey] = body
			}
			return gitResponse(200, "application/x-git-upload-pack-result", body)
		case "fetch":
			key := fetchKey{repo.ID, repo.RefsVersion, string(data)}
			return fetchResponseCache.get(key, func() *ForgeResponse {
				return frameGitResponse(gitResponse(200, "application/x-git-upload-pack-result", fetchPack(repo, commands)))
			})
		default:
			return gitError(400, "Unsupported upload-pack command")
		}
	}
	if endpoint == "git-receive-pack" && req.Method == "POST" {
		defer func() {
			if cause := recover(); cause != nil {
				res = gitFailure(cause)
			}
		}()
		return gitResponse(200, "application/x-git-receive-pack-result", receivePack(repo, data))
	}
	return gitError(404, "Not found")
}
func branches(repo *Repository) []string {
	out := slices.Clone(repo.Refs.keys)
	slices.SortFunc(out, jsCompare)
	return out
}
func defaultHead(repo *Repository) (string, string) {
	name := "refs/heads/" + repo.DefaultBranch
	oid := repo.Refs.Get(name)
	if !repo.Refs.Has(name) {
		oid = zeroOID
	}
	return oid, name
}
func tagTarget(hash string) string {
	if store.GitObjectTypes[hash] != "tag" {
		return ""
	}
	data := store.GitObjects.Get(hash)
	for _, line := range strings.FieldsFunc(utf8Text(data), func(r rune) bool { return r == '\r' || r == '\n' || r == 0x2028 || r == 0x2029 }) {
		if len(line) == 47 && strings.HasPrefix(line, "object ") && isLowerOID([]byte(line[7:])) {
			return line[7:]
		}
	}
	return ""
}
func peeled(hash string) string {
	current := hash
	visited := make(map[string]bool)
	for !visited[current] {
		visited[current] = true
		target := tagTarget(current)
		if target == "" {
			if current != hash {
				return current
			}
			return ""
		}
		current = target
	}
	return ""
}

type advertisementRow struct{ OID, Name string }
type advertisementTemplate struct {
	Bytes   []byte
	Offsets []int
}

func advertisementRows(repo *Repository) []advertisementRow {
	h, _ := defaultHead(repo)
	rows := []advertisementRow{{h, "HEAD"}}
	for _, name := range branches(repo) {
		oid := repo.Refs.Get(name)
		rows = append(rows, advertisementRow{oid, name})
		if strings.HasPrefix(name, "refs/tags/") {
			if target := peeled(oid); target != "" {
				rows = append(rows, advertisementRow{target, name + "^{}"})
			}
		}
	}
	return rows
}
func buildV0Advertisement(rows []advertisementRow, service, headRef string) advertisementTemplate {
	out := pktLines("# service="+service+"\n", "flush")
	offsets := make([]int, 0, len(rows))
	if len(rows) == 0 {
		out = appendPktText(out, zeroOID+" capabilities^{}\x00"+receiveCapabilities+" symref=HEAD:"+headRef+"\n")
	} else {
		for i, row := range rows {
			caps := ""
			if i == 0 {
				caps = "\x00" + receiveCapabilities + " symref=HEAD:" + headRef
			}
			offsets = append(offsets, len(out)+4)
			out = appendPktText(out, row.OID+" "+row.Name+caps+"\n")
		}
	}
	out = append(out, "0000"...)
	return advertisementTemplate{out, offsets}
}
func v0Advertisement(repo *Repository, service string) []byte {
	rows := advertisementRows(repo)
	headRef := "refs/heads/" + repo.DefaultBranch
	valid := true
	names := make([]string, 0, len(rows))
	for _, r := range rows {
		names = append(names, r.Name)
		if len(r.OID) != 40 {
			valid = false
		} else {
			for _, c := range r.OID {
				if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
					valid = false
					break
				}
			}
		}
	}
	if len(rows) == 0 || !valid {
		return buildV0Advertisement(rows, service, headRef).Bytes
	}
	key := string(jsonBytes([]any{repo.ID, service, repo.DefaultBranch, names}))
	if cached, ok := advertisementCache[key]; ok {
		out := bytes.Clone(cached.Bytes)
		for i, row := range rows {
			copy(out[cached.Offsets[i]:cached.Offsets[i]+40], row.OID)
		}
		return out
	}
	template := buildV0Advertisement(rows, service, headRef)
	advertisementCache[key] = template
	return template.Bytes
}
func commandArguments(commands []string, prefix string) []string {
	out := make([]string, 0)
	for _, line := range commands {
		if strings.HasPrefix(line, prefix) {
			out = append(out, strings.TrimFunc(line[len(prefix):], jsSpace))
		}
	}
	return out
}
func lsRefs(repo *Repository, commands []string) []byte {
	prefixes := make([]string, 0)
	for _, line := range commands {
		if strings.HasPrefix(line, "ref-prefix ") {
			prefixes = append(prefixes, line[11:])
		}
	}
	matches := func(name string) bool {
		if len(prefixes) == 0 {
			return true
		}
		for _, p := range prefixes {
			if strings.HasPrefix(name, p) {
				return true
			}
		}
		return false
	}
	symrefs, peel := slices.Contains(commands, "symrefs"), slices.Contains(commands, "peel")
	oid, head := defaultHead(repo)
	var out []byte
	if matches("HEAD") {
		line := oid + " HEAD"
		if oid == zeroOID {
			line = "unborn HEAD"
		}
		if symrefs {
			line += " symref-target:" + head
		}
		out = appendPktText(out, line+"\n")
	}
	for _, name := range branches(repo) {
		if !matches(name) {
			continue
		}
		line := repo.Refs.Get(name) + " " + name
		if peel && strings.HasPrefix(name, "refs/tags/") {
			if h := peeled(repo.Refs.Get(name)); h != "" {
				line += " peeled:" + h
			}
		}
		out = appendPktText(out, line+"\n")
	}
	return append(out, "0000"...)
}

type refCommand struct{ Old, Next, Ref string }

func isLowerOID(data []byte) bool {
	if len(data) != 40 {
		return false
	}
	for _, b := range data {
		if !(b >= '0' && b <= '9' || b >= 'a' && b <= 'f') {
			return false
		}
	}
	return true
}
func commandLines(data []byte) ([]refCommand, []byte) {
	commands := make([]refCommand, 0)
	offset := 0
	for {
		if offset+4 > len(data) {
			panic("Receive-pack command list is truncated")
		}
		n, ok := hexPacketLength(data[offset : offset+4])
		if !ok {
			panic("Invalid receive-pack pkt-line")
		}
		offset += 4
		if n == 0 {
			break
		}
		if n < 4 || n-4 > len(data)-offset {
			panic("Truncated receive-pack command")
		}
		line := strings.TrimSuffix(utf8Text(data[offset:offset+n-4]), "\n")
		line, _, _ = strings.Cut(line, "\x00")
		offset += n - 4
		if len(line) < 83 || line[40] != ' ' || line[81] != ' ' || !isLowerOID([]byte(line[:40])) || !isLowerOID([]byte(line[41:81])) || strings.IndexFunc(line[82:], jsSpace) >= 0 {
			panic("Malformed receive-pack command")
		}
		commands = append(commands, refCommand{line[:40], line[41:81], line[82:]})
	}
	return commands, data[offset:]
}

var gitCommitParents = make(map[string][]string)

func commitParents(hash string, data []byte) []string {
	if parents, ok := gitCommitParents[hash]; ok {
		return parents
	}
	parents := parseCommit(data).Parents
	gitCommitParents[hash] = parents
	return parents
}
func isAncestor(old, next string) bool {
	if old == zeroOID {
		return true
	}
	pending := []string{next}
	seen := make(map[string]bool)
	for len(pending) > 0 {
		h := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if h == old {
			return true
		}
		if seen[h] {
			continue
		}
		seen[h] = true
		if o := lookupObject(h); o != nil && o.Type == "commit" {
			parents := commitParents(h, o.Data)
			if len(parents) > 0 && parents[len(parents)-1] == old {
				return true
			}
			pending = append(pending, parents...)
		}
	}
	return false
}
func reportResponse(report []string) []byte {
	key := strings.Join(report, "\x00")
	if cached := reportResponseCache[key]; cached != nil {
		return cached
	}
	out := appendSideband(nil, 1, pktLines(report...))
	reportResponseCache[key] = out
	return out
}
func ingestPack(pack []byte) (failure any) {
	defer func() { failure = recover() }()
	objects := parsePack(pack)
	for _, hash := range objects.keys {
		o := objects.Get(hash)
		h := hashHex(hash)
		if !store.GitObjects.Has(h) || store.GitObjectTypes[h] == "" {
			store.GitObjects.Set(h, o.Data)
			store.GitObjectTypes[h] = o.Type
		}
		if o.Packed != nil {
			store.GitPackedEntries[h] = o.Packed
		}
	}
	return nil
}
func receivePack(repo *Repository, data []byte) []byte {
	commands, pack := commandLines(data)
	if len(pack) > 0 {
		if failure := ingestPack(pack); failure != nil {
			report := []string{"unpack " + panicMessage(failure) + "\n"}
			for _, cmd := range commands {
				report = append(report, "ng "+cmd.Ref+" unpacker error\n")
			}
			report = append(report, "flush")
			return appendSideband(nil, 1, pktLines(report...))
		}
	}
	report := []string{"unpack ok\n"}
	changed := false
	for _, cmd := range commands {
		current := repo.Refs.Get(cmd.Ref)
		if !repo.Refs.Has(cmd.Ref) {
			current = zeroOID
		}
		if current != cmd.Old {
			report = append(report, "ng "+cmd.Ref+" stale info\n")
			continue
		}
		if cmd.Next == zeroOID {
			changed = repo.Refs.Has(cmd.Ref) || changed
			repo.Refs.Delete(cmd.Ref)
			report = append(report, "ok "+cmd.Ref+"\n")
			continue
		}
		if lookupObject(cmd.Next) == nil {
			report = append(report, "ng "+cmd.Ref+" missing object\n")
			continue
		}
		if strings.HasPrefix(cmd.Ref, "refs/heads/") && !isAncestor(current, cmd.Next) {
			report = append(report, "ng "+cmd.Ref+" non-fast-forward\n")
			continue
		}
		if current != cmd.Next {
			changed = true
		}
		repo.Refs.Set(cmd.Ref, cmd.Next)
		report = append(report, "ok "+cmd.Ref+"\n")
	}
	report = append(report, "flush")
	if changed {
		repo.RefsVersion++
	}
	return reportResponse(report)
}
func shallowReachable(tips, boundaries []string) *orderedMap[string, bool] {
	seen := newOrderedMap[string, bool]()
	pending := slices.Clone(tips)
	for len(pending) > 0 {
		h := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if h == zeroOID || seen.Has(h) {
			continue
		}
		o := lookupObject(h)
		if o == nil {
			continue
		}
		seen.Set(h, true)
		switch o.Type {
		case "commit":
			c := parseCommit(o.Data)
			pending = append(pending, c.Tree)
			if !slices.Contains(boundaries, h) {
				pending = append(pending, c.Parents...)
			}
		case "tree":
			for _, e := range parseTree(o.Data) {
				if e.Mode != "160000" {
					pending = append(pending, e.Hash)
				}
			}
		case "tag":
			if target := tagTarget(h); target != "" {
				pending = append(pending, target)
			}
		}
	}
	return seen
}
func commitTreeClosure(hash string, withBlobs bool) []string {
	c := lookupObject(hash)
	if c == nil || c.Type != "commit" {
		return nil
	}
	tree := parseCommit(c.Data).Tree
	out := []string{hash, tree}
	if t := lookupObject(tree); withBlobs && t != nil && t.Type == "tree" {
		for _, e := range parseTree(t.Data) {
			if e.Mode != "160000" {
				out = append(out, e.Hash)
			}
		}
	}
	return out
}
func uniqueHashes(hashes []string) []string {
	out := make([]string, 0, len(hashes))
	seen := make(map[string]bool)
	for _, h := range hashes {
		if !seen[h] {
			seen[h] = true
			out = append(out, h)
		}
	}
	return out
}
func hashObjects(hashes []string) []*GitObject {
	out := make([]*GitObject, 0, len(hashes))
	for _, h := range hashes {
		if o := lookupObject(h); o != nil {
			out = append(out, o)
		}
	}
	return out
}
func cacheDepthOnePack(repo *Repository, tip string) []byte {
	key := strconv.Itoa(repo.ID) + ":" + strconv.Itoa(repo.RefsVersion) + ":" + tip + ":1"
	if cached := depthOneCache[key]; cached != nil {
		return cached
	}
	pack := buildPack(hashObjects(uniqueHashes(commitTreeClosure(tip, true))))
	depthOneCache[key] = pack
	return pack
}
func cachedDepthOnePackCount() int { return len(depthOneCache) }
func fetchPack(repo *Repository, commands []string) []byte {
	wants, haves := commandArguments(commands, "want "), commandArguments(commands, "have ")
	oldShallow := uniqueHashes(commandArguments(commands, "shallow "))
	depth := float64(0)
	for _, line := range commands {
		if strings.HasPrefix(line, "deepen ") {
			depth = jsNumber(line[7:])
			if depth == 0 || math.IsNaN(depth) {
				depth = 1
			}
			depth = math.Max(1, depth)
			break
		}
	}
	shallowKey := ""
	if depth == 1 && len(wants) == 1 && len(haves) == 0 {
		shallowKey = string(jsonBytes([]any{repo.ID, repo.RefsVersion, commands}))
		if cached := depthOneResponseCache[shallowKey]; cached != nil {
			return cached
		}
	}
	objects, shallow, unshallow := make([]string, 0), make([]string, 0), make([]string, 0)
	held := shallowReachable(haves, oldShallow)
	if depth > 1 && len(oldShallow) > 0 {
		for _, boundary := range oldShallow {
			c := lookupObject(boundary)
			if c == nil || c.Type != "commit" {
				continue
			}
			parents := parseCommit(c.Data).Parents
			if len(parents) == 0 {
				continue
			}
			parent := parents[0]
			for _, h := range commitTreeClosure(parent, false) {
				if !held.Has(h) {
					objects = append(objects, h)
				}
			}
			shallow = append(shallow, parent)
			unshallow = append(unshallow, boundary)
		}
	} else if depth == 1 {
		for _, want := range wants {
			for _, h := range commitTreeClosure(want, true) {
				if !held.Has(h) {
					objects = append(objects, h)
				}
			}
			if o := lookupObject(want); o != nil && o.Type == "commit" && len(parseCommit(o.Data).Parents) > 0 {
				shallow = append(shallow, want)
			}
		}
	} else {
		for _, h := range reachableDifference(wants, haves) {
			if len(oldShallow) == 0 || !held.Has(h) {
				objects = append(objects, h)
			}
		}
	}
	objects = uniqueHashes(objects)
	out := pktLine("acknowledgments\n")
	validHaves := make([]string, 0)
	for _, h := range haves {
		if lookupObject(h) != nil {
			validHaves = append(validHaves, h)
		}
	}
	if len(validHaves) > 0 {
		for _, h := range validHaves {
			out = appendPktText(out, "ACK "+h+" common\n")
		}
		out = appendPktText(out, "ready\n")
	} else {
		out = appendPktText(out, "NAK\n")
	}
	out = append(out, "0000"...)
	if len(shallow) > 0 || len(unshallow) > 0 {
		out = appendPktText(out, "shallow-info\n")
		for _, h := range shallow {
			out = appendPktText(out, "shallow "+h+"\n")
		}
		for _, h := range unshallow {
			out = appendPktText(out, "unshallow "+h+"\n")
		}
		out = append(out, "0000"...)
	}
	if len(objects) > 0 {
		out = appendPktText(out, "packfile\n")
		var pack []byte
		if depth == 1 && len(wants) == 1 && len(haves) == 0 {
			pack = cacheDepthOnePack(repo, wants[0])
		} else {
			pack = buildPack(hashObjects(objects))
		}
		out = appendSideband(out, 1, pack)
		out = append(out, "0000"...)
	}
	if shallowKey != "" {
		depthOneResponseCache[shallowKey] = out
	}
	return out
}

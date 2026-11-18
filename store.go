package main

import (
	"bytes"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

const fixedTime = "2025-01-01T00:00:00.000Z"
const zeroOID = "0000000000000000000000000000000000000000"

type orderedMap[K comparable, V any] struct {
	keys   []K
	values map[K]V
}

func newOrderedMap[K comparable, V any]() *orderedMap[K, V] {
	return &orderedMap[K, V]{keys: make([]K, 0), values: make(map[K]V)}
}
func (m *orderedMap[K, V]) Get(k K) V    { return m.values[k] }
func (m *orderedMap[K, V]) Has(k K) bool { _, ok := m.values[k]; return ok }
func (m *orderedMap[K, V]) Set(k K, v V) {
	if !m.Has(k) {
		m.keys = append(m.keys, k)
	}
	m.values[k] = v
}
func (m *orderedMap[K, V]) Delete(k K) {
	if m.Has(k) {
		delete(m.values, k)
		i := slices.Index(m.keys, k)
		m.keys = slices.Delete(m.keys, i, i+1)
	}
}
func (m *orderedMap[K, V]) Clone() *orderedMap[K, V] {
	c := newOrderedMap[K, V]()
	for _, k := range m.keys {
		c.Set(k, m.Get(k))
	}
	return c
}

type Repository struct {
	ID                                         int
	NodeID, Owner, Name, FullName, Description string
	Private                                    bool
	DefaultBranch, CreatedAt, UpdatedAt        string
	Issues                                     *orderedMap[int, *Issue]
	Pulls                                      *orderedMap[int, *PullRequest]
	Labels                                     *orderedMap[string, *Label]
	Comments                                   *orderedMap[int, *Comment]
	Refs                                       *orderedMap[string, string]
	RefsVersion                                int
	Cache                                      map[string][]byte
	NextIssueNumber, NextPullNumber            int
}
type Issue struct {
	ID, Number                     int
	NodeID, Title, Body, State     string
	Labels                         []*Label
	CreatedAt, UpdatedAt           string
	Comments                       int
	Repository                     *Repository
	Serialized, SerializedResponse []byte
}
type PullRef struct{ Ref, SHA string }

func (r PullRef) Public() object { return obj("ref", r.Ref, "sha", r.SHA) }

type PullRequest struct {
	ID, Number                 int
	NodeID, Title, Body, State string
	Merged                     bool
	Head, Base                 PullRef
	CreatedAt, UpdatedAt       string
	MergedAt                   *string
	Mergeable                  *bool
	MergeCommitSHA             *string
	Repository                 *Repository
}
type Label struct {
	ID          int
	Name, Color string
}

func (l *Label) Public() object { return obj("id", l.ID, "name", l.Name, "color", l.Color) }

type Comment struct {
	ID                      int
	Body, CreatedAt         string
	IssueNumber, PullNumber *int
}

func (c *Comment) Public() object {
	v := obj("id", c.ID, "body", c.Body, "created_at", c.CreatedAt)
	if c.IssueNumber != nil {
		v = append(v, field{"issue_number", *c.IssueNumber})
	}
	if c.PullNumber != nil {
		v = append(v, field{"pull_number", *c.PullNumber})
	}
	return v
}
func globalID(kind string, id int) string {
	return base64.StdEncoding.EncodeToString([]byte(kind + ":" + strconv.Itoa(id)))
}

type ForgeStore struct {
	// All dispatches hold this lock, so cross-resource mutation and snapshot
	// serialization are atomic. Cached byte slices are never changed in place.
	mu                sync.Mutex
	Repositories      *orderedMap[string, *Repository]
	GitObjects        *orderedMap[string, []byte]
	GitObjectTypes    map[string]string
	GitPackedEntries  map[string][]byte
	RequestID, NextID int
}

func newForgeStore() *ForgeStore {
	return &ForgeStore{Repositories: newOrderedMap[string, *Repository](), GitObjects: newOrderedMap[string, []byte](), GitObjectTypes: make(map[string]string), GitPackedEntries: make(map[string][]byte), NextID: 1}
}

var store = newForgeStore()

func (s *ForgeStore) RequestHeader() string {
	s.RequestID++
	return fmt.Sprintf("MF%010d", s.RequestID)
}
func (s *ForgeStore) Repository(owner, name string) *Repository {
	return s.Repositories.Get(jsLower(owner) + "/" + jsLower(name))
}
func (s *ForgeStore) CreateRepository(owner, name, description string, private bool) *Repository {
	key := jsLower(owner) + "/" + jsLower(name)
	if current := s.Repositories.Get(key); current != nil {
		return current
	}
	id := s.NextID
	s.NextID++
	r := &Repository{ID: id, NodeID: globalID("Repository", id), Owner: owner, Name: name, FullName: owner + "/" + name, Description: description, Private: private, DefaultBranch: "main", CreatedAt: fixedTime, UpdatedAt: fixedTime, Issues: newOrderedMap[int, *Issue](), Pulls: newOrderedMap[int, *PullRequest](), Labels: newOrderedMap[string, *Label](), Comments: newOrderedMap[int, *Comment](), Refs: newOrderedMap[string, string](), Cache: make(map[string][]byte), NextIssueNumber: 1, NextPullNumber: 1}
	s.Repositories.Set(key, r)
	return r
}
func (s *ForgeStore) CreateIssue(r *Repository, title, body string, labels []*Label) *Issue {
	id := s.NextID
	s.NextID++
	i := &Issue{ID: id, NodeID: globalID("Issue", id), Number: r.NextIssueNumber, Title: title, Body: body, State: "open", Labels: labels, CreatedAt: fixedTime, UpdatedAt: fixedTime, Repository: r}
	r.NextIssueNumber++
	r.Issues.Set(i.Number, i)
	s.Invalidate(r, func(k string) bool { return k == fmt.Sprintf("issue:%d", i.Number) || strings.HasPrefix(k, "issues:") })
	serializeNewIssue(i)
	return i
}
func (s *ForgeStore) CreatePull(r *Repository, title, body, head, base string) *PullRequest {
	id := s.NextID
	s.NextID++
	p := &PullRequest{ID: id, NodeID: globalID("PullRequest", id), Number: r.NextPullNumber, Title: title, Body: body, State: "open", Head: PullRef{head, r.Refs.Get("refs/heads/" + head)}, Base: PullRef{base, r.Refs.Get("refs/heads/" + base)}, CreatedAt: fixedTime, UpdatedAt: fixedTime, Repository: r}
	r.NextPullNumber++
	r.Pulls.Set(p.Number, p)
	s.Invalidate(r, func(k string) bool { return strings.HasPrefix(k, "pulls:") })
	return p
}
func (s *ForgeStore) Invalidate(r *Repository, matches func(string) bool) {
	for k := range r.Cache {
		if matches(k) {
			delete(r.Cache, k)
		}
	}
}
func (s *ForgeStore) Cached(r *Repository, key string, value any) rawJSON {
	b, ok := r.Cache[key]
	if !ok {
		b = jsonBytes(value)
		r.Cache[key] = b
	}
	return rawJSON(b)
}
func objectHash(kind string, data []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "%s %d\x00", kind, len(data))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}
func (s *ForgeStore) PutObject(data []byte) string { return s.PutGitObject("blob", data) }
func (s *ForgeStore) PutGitObject(kind string, data []byte) string {
	h := objectHash(kind, data)
	if !s.GitObjects.Has(h) {
		s.GitObjects.Set(h, bytes.Clone(data))
	}
	s.GitObjectTypes[h] = kind
	return h
}
func putObject(kind string, data []byte) string {
	h := objectHash(kind, data)
	store.GitObjects.Set(h, bytes.Clone(data))
	store.GitObjectTypes[h] = kind
	return h
}

func publicRepo(r *Repository) object {
	return obj("id", r.ID, "node_id", r.NodeID, "name", r.Name, "full_name", r.FullName, "owner", obj("login", r.Owner), "private", r.Private, "description", r.Description, "default_branch", r.DefaultBranch, "created_at", r.CreatedAt, "updated_at", r.UpdatedAt, "has_issues", true)
}
func publicIssue(i *Issue) object {
	labels := make([]any, 0, len(i.Labels))
	for _, l := range i.Labels {
		labels = append(labels, l.Public())
	}
	return obj("id", i.ID, "node_id", i.NodeID, "number", i.Number, "title", i.Title, "body", i.Body, "state", i.State, "labels", labels, "comments", i.Comments, "created_at", i.CreatedAt, "updated_at", i.UpdatedAt)
}
func refreshIssue(i *Issue) {
	b := jsonBytes(publicIssue(i))
	i.Serialized = b
	i.SerializedResponse = append(bytes.Clone(b), '\n')
}

// New issues have fixed state, comment count, and timestamps. Build both cached
// forms in one owned buffer; updates still replace them through refreshIssue.
func serializeNewIssue(i *Issue) {
	b := make([]byte, 0, 256+len(i.NodeID)+len(i.Title)+len(i.Body))
	b = append(b, `{"id":`...)
	b = strconv.AppendInt(b, int64(i.ID), 10)
	b = append(b, `,"node_id":`...)
	b = appendJSONString(b, i.NodeID)
	b = append(b, `,"number":`...)
	b = strconv.AppendInt(b, int64(i.Number), 10)
	b = append(b, `,"title":`...)
	b = appendJSONString(b, i.Title)
	b = append(b, `,"body":`...)
	b = appendJSONString(b, i.Body)
	b = append(b, `,"state":"open","labels":[`...)
	for index, label := range i.Labels {
		if index != 0 {
			b = append(b, ',')
		}
		b = appendJSON(b, label.Public())
	}
	b = append(b, `],"comments":0,"created_at":"`+fixedTime+`","updated_at":"`+fixedTime+`"}`...)
	b = append(b, '\n')
	i.SerializedResponse = b
	i.Serialized = b[: len(b)-1 : len(b)-1]
}
func nullableString(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}
func publicPull(p *PullRequest) object {
	return obj("id", p.ID, "node_id", p.NodeID, "number", p.Number, "title", p.Title, "body", p.Body, "state", p.State, "merged", p.Merged, "head", p.Head.Public(), "base", p.Base.Public(), "created_at", p.CreatedAt, "updated_at", p.UpdatedAt, "merged_at", nullableString(p.MergedAt), "mergeable", pullMergeable(p), "merge_commit_sha", nullableString(p.MergeCommitSHA))
}

type GitObject struct {
	Type         string
	Data, Packed []byte
}
type TreeEntry struct{ Mode, Name, Hash string }
type CommitData struct {
	Tree                       string
	Parents                    []string
	Author, Committer, Message string
}

func lookupObject(hash string) *GitObject {
	data, exists := store.GitObjects.values[hash]
	kind := store.GitObjectTypes[hash]
	if !exists || kind == "" {
		return nil
	}
	return &GitObject{kind, data, store.GitPackedEntries[hash]}
}
func parseTree(data []byte) []TreeEntry {
	entries := make([]TreeEntry, 0)
	for offset := 0; offset < len(data); {
		space := bytes.IndexByte(data[offset:], ' ')
		if space < 0 {
			panic("Malformed tree object")
		}
		space += offset
		nul := bytes.IndexByte(data[space+1:], 0)
		if nul < 0 {
			panic("Malformed tree object")
		}
		nul += space + 1
		if nul+21 > len(data) {
			panic("Malformed tree object")
		}
		mode := bytes.Clone(data[offset:space])
		for i := range mode {
			mode[i] &= 127
		}
		entries = append(entries, TreeEntry{string(mode), utf8Text(data[space+1 : nul]), hex.EncodeToString(data[nul+1 : nul+21])})
		offset = nul + 21
	}
	return entries
}
func parseCommit(data []byte) CommitData {
	s := utf8Text(data)
	headers, message, found := strings.Cut(s, "\n\n")
	c := CommitData{Parents: make([]string, 0)}
	if found {
		c.Message = message
	}
	for _, line := range strings.Split(headers, "\n") {
		key, value, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		switch key {
		case "tree":
			c.Tree = value
		case "parent":
			c.Parents = append(c.Parents, value)
		case "author":
			c.Author = value
		case "committer":
			c.Committer = value
		}
	}
	if len(c.Tree) != 40 || strings.IndexFunc(c.Tree, func(r rune) bool { return !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') }) >= 0 {
		panic("Commit has no valid tree header")
	}
	return c
}
func utf8Text(data []byte) string {
	if utf8.Valid(data) {
		return string(data)
	}
	var b strings.Builder
	for i := 0; i < len(data); {
		r, n := utf8.DecodeRune(data[i:])
		if r != utf8.RuneError || n != 1 {
			b.Write(data[i : i+n])
			i += n
			continue
		}
		// Node replaces a maximal valid prefix of a truncated sequence once,
		// while invalid leading/continuation bytes are replaced individually.
		c := data[i]
		want := 1
		if c >= 0xc2 && c <= 0xdf {
			want = 2
		} else if c >= 0xe0 && c <= 0xef {
			want = 3
		} else if c >= 0xf0 && c <= 0xf4 {
			want = 4
		}
		n = 1
		for n < want && i+n < len(data) {
			t := data[i+n]
			if t < 0x80 || t > 0xbf {
				break
			}
			if n == 1 && (c == 0xe0 && t < 0xa0 || c == 0xed && t >= 0xa0 || c == 0xf0 && t < 0x90 || c == 0xf4 && t >= 0x90) {
				break
			}
			n++
		}
		b.WriteRune(utf8.RuneError)
		i += n
	}
	return b.String()
}
func commitTree(hash string) string {
	o := lookupObject(hash)
	if o != nil && o.Type == "commit" {
		return parseCommit(o.Data).Tree
	}
	return ""
}
func ref(repo *Repository, branch string) string {
	if !strings.HasPrefix(branch, "refs/") {
		branch = "refs/heads/" + branch
	}
	if h := repo.Refs.Get(branch); h != "" {
		return h
	}
	return zeroOID
}
func flatten(treeHash string) *orderedMap[string, string] {
	out := newOrderedMap[string, string]()
	flattenInto(treeHash, "", out)
	return out
}
func flattenInto(treeHash, prefix string, out *orderedMap[string, string]) {
	tree := lookupObject(treeHash)
	if tree == nil || tree.Type != "tree" {
		return
	}
	for _, e := range parseTree(tree.Data) {
		path := e.Name
		if prefix != "" {
			path = prefix + "/" + path
		}
		if e.Mode == "40000" || e.Mode == "040000" {
			flattenInto(e.Hash, path, out)
		} else if e.Mode != "160000" {
			out.Set(path, e.Hash)
		}
	}
}

type treeNode struct {
	files *orderedMap[string, string]
	dirs  *orderedMap[string, *treeNode]
}

func newTreeNode() *treeNode {
	return &treeNode{newOrderedMap[string, string](), newOrderedMap[string, *treeNode]()}
}
func writeTree(files *orderedMap[string, string]) string {
	root := newTreeNode()
	for _, path := range files.keys {
		bits := strings.FieldsFunc(path, func(r rune) bool { return r == '/' })
		if len(bits) == 0 {
			continue
		}
		node := root
		for _, part := range bits[:len(bits)-1] {
			next := node.dirs.Get(part)
			if next == nil {
				next = newTreeNode()
				node.dirs.Set(part, next)
			}
			node = next
		}
		node.files.Set(bits[len(bits)-1], files.Get(path))
	}
	return buildTree(root)
}
func buildTree(node *treeNode) string {
	rows := make([]TreeEntry, 0, len(node.files.keys)+len(node.dirs.keys))
	for _, name := range node.files.keys {
		rows = append(rows, TreeEntry{"100644", name, node.files.Get(name)})
	}
	for _, name := range node.dirs.keys {
		rows = append(rows, TreeEntry{"40000", name, buildTree(node.dirs.Get(name))})
	}
	slices.SortStableFunc(rows, func(a, b TreeEntry) int {
		ak, bk := a.Name, b.Name
		if a.Mode == "40000" {
			ak += "/"
		}
		if b.Mode == "40000" {
			bk += "/"
		}
		return jsCompare(ak, bk)
	})
	var data []byte
	for _, row := range rows {
		data = append(data, row.Mode+" "+row.Name...)
		data = append(data, 0)
		h, _ := hex.DecodeString(row.Hash)
		data = append(data, h...)
	}
	return putObject("tree", data)
}
func createCommit(tree string, parent *string, message string) string {
	text := "tree " + tree + "\n"
	if parent != nil && *parent != "" {
		text += "parent " + *parent + "\n"
	}
	text += "author Makefun Forge <forge@example.invalid> 1735689600 +0000\ncommitter Makefun Forge <forge@example.invalid> 1735689600 +0000\n\n" + message + "\n"
	return putObject("commit", []byte(text))
}

type pathUpdate struct{ Commit, Blob, Old string }

func updatePath(repo *Repository, branch, path string, content []byte, message string, remove bool) pathUpdate {
	branchRef := "refs/heads/" + branch
	var parent *string
	files := newOrderedMap[string, string]()
	if h := repo.Refs.Get(branchRef); h != "" {
		parent = &h
		files = flatten(commitTree(h))
	}
	r := pathUpdate{Old: files.Get(path)}
	if remove {
		files.Delete(path)
	} else {
		r.Blob = putObject("blob", content)
		files.Set(path, r.Blob)
	}
	tree := writeTree(files)
	r.Commit = createCommit(tree, parent, message)
	repo.Refs.Set(branchRef, r.Commit)
	repo.RefsVersion++
	store.Invalidate(repo, func(k string) bool {
		return strings.HasPrefix(k, "branches:") || strings.HasPrefix(k, "commits:") || k == "repository" || strings.HasPrefix(k, "content:"+branch+":")
	})
	return r
}
func jsCompare(a, b string) int {
	return slices.Compare(jsUTF16(a), jsUTF16(b))
}
func jsLower(s string) string {
	// Unicode Default Case Conversion includes dotted I and final sigma, unlike
	// Go's simple per-rune lowercasing.
	units := jsUTF16(s)
	r := make([]rune, 0, len(units))
	for i := 0; i < len(units); i++ {
		u := units[i]
		if u >= 0xd800 && u <= 0xdbff && i+1 < len(units) && units[i+1] >= 0xdc00 && units[i+1] <= 0xdfff {
			r = append(r, rune(u-0xd800)*1024+rune(units[i+1]-0xdc00)+0x10000)
			i++
		} else {
			r = append(r, rune(u))
		}
	}
	var b strings.Builder
	for i, c := range r {
		if c >= 0xd800 && c <= 0xdfff {
			b.Write(appendWTF8(nil, uint16(c)))
			continue
		}
		if c == 0x130 {
			b.WriteString("i\u0307")
			continue
		}
		if c == 0x3a3 {
			before, after := false, false
			for j := i - 1; j >= 0; j-- {
				if caseIgnorable(r[j]) {
					continue
				}
				before = unicode.IsUpper(r[j]) || unicode.IsLower(r[j]) || unicode.IsTitle(r[j])
				break
			}
			for j := i + 1; j < len(r); j++ {
				if caseIgnorable(r[j]) {
					continue
				}
				after = unicode.IsUpper(r[j]) || unicode.IsLower(r[j]) || unicode.IsTitle(r[j])
				break
			}
			if before && !after {
				b.WriteRune(0x3c2)
				continue
			}
		}
		b.WriteRune(unicode.ToLower(c))
	}
	return b.String()
}
func caseIgnorable(r rune) bool {
	return unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Me, r) || unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Lm, r) || unicode.Is(unicode.Sk, r) || r == '\'' || r == 0x2019
}

package main

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// ---------- git objects ----------

type Object struct {
	Type string // "commit", "tree", "blob", "tag"
	Data []byte
}

func gitHash(typ string, data []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "%s %d\x00", typ, len(data))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

// ---------- issues / pulls ----------

type Comment struct {
	ID        int       `json:"id"`
	Body      string    `json:"body"`
	User      string    `json:"user"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type Issue struct {
	Number    int        `json:"number"`
	Title     string     `json:"title"`
	Body      string     `json:"body"`
	State     string     `json:"state"` // open | closed
	Labels    []string   `json:"labels"`
	Assignees []string   `json:"assignees"`
	Comments  []*Comment `json:"-"`
	User      string     `json:"user"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	ClosedAt  *time.Time `json:"closed_at,omitempty"`
}

type Pull struct {
	Number    int        `json:"number"`
	Title     string     `json:"title"`
	Body      string     `json:"body"`
	State     string     `json:"state"` // open | closed | merged
	Head      string     `json:"head"`
	Base      string     `json:"base"`
	HeadSHA   string     `json:"head_sha"`
	BaseSHA   string     `json:"base_sha"`
	Merged    bool       `json:"merged"`
	MergeSHA  string     `json:"merge_commit_sha,omitempty"`
	Mergeable bool       `json:"mergeable"`
	Draft     bool       `json:"draft"`
	User      string     `json:"user"`
	Comments  []*Comment `json:"-"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	ClosedAt  *time.Time `json:"closed_at,omitempty"`
	MergedAt  *time.Time `json:"merged_at,omitempty"`
}

// ---------- repo ----------

type Label struct {
	Name        string `json:"name"`
	Color       string `json:"color"`
	Description string `json:"description"`
}

type Repo struct {
	Owner         string
	Name          string
	FullName      string
	Description   string
	Private       bool
	DefaultBranch string
	CreatedAt     time.Time
	UpdatedAt     time.Time
	PushedAt      time.Time
	Objects       map[string]*Object // sha -> object
	Refs          map[string]string  // "refs/heads/main" -> sha
	Issues        map[int]*Issue
	Pulls         map[int]*Pull
	Labels        []Label
	NextIssue     int
	NextPull      int
	NextComment   int
	Stars         int
	Watchers      int
	Forks         int
	Topics        []string
	Homepage      string
	Language      string
	Archived      bool
	Disabled      bool
}

func (r *Repo) headSHA() string {
	if s, ok := r.Refs["refs/heads/"+r.DefaultBranch]; ok {
		return s
	}
	return ""
}

// ---------- store ----------

type Store struct {
	mu    sync.RWMutex
	repos map[string]*Repo // "owner/name"
	users map[string]bool
}

func NewStore() *Store {
	return &Store{repos: map[string]*Repo{}, users: map[string]bool{}}
}

var store = NewStore()

func (s *Store) GetRepo(owner, name string) *Repo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.repos[strings.ToLower(owner)+"/"+strings.ToLower(name)]
}

func (s *Store) CreateRepo(owner, name, desc string, private bool) (*Repo, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := strings.ToLower(owner) + "/" + strings.ToLower(name)
	if _, ok := s.repos[key]; ok {
		return nil, false
	}
	now := time.Now().UTC()
	r := &Repo{
		Owner: owner, Name: name, FullName: owner + "/" + name,
		Description: desc, Private: private, DefaultBranch: "main",
		CreatedAt: now, UpdatedAt: now, PushedAt: now,
		Objects: map[string]*Object{}, Refs: map[string]string{},
		Issues: map[int]*Issue{}, Pulls: map[int]*Pull{},
		NextIssue: 1, NextPull: 1, NextComment: 1,
	}
	s.repos[key] = r
	s.users[strings.ToLower(owner)] = true
	return r, true
}

func (s *Store) DeleteRepo(owner, name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := strings.ToLower(owner) + "/" + strings.ToLower(name)
	if _, ok := s.repos[key]; !ok {
		return false
	}
	delete(s.repos, key)
	return true
}

func (s *Store) ListRepos(owner string) []*Repo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*Repo
	for _, r := range s.repos {
		if owner == "" || strings.EqualFold(r.Owner, owner) {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].FullName < out[j].FullName })
	return out
}

func (s *Store) UserExists(u string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.users[strings.ToLower(u)]
}

// ---------- object helpers (call with repo lock held or single-threaded per request) ----------

func (r *Repo) putObject(typ string, data []byte) string {
	sha := gitHash(typ, data)
	if _, ok := r.Objects[sha]; !ok {
		cp := make([]byte, len(data))
		copy(cp, data)
		r.Objects[sha] = &Object{Type: typ, Data: cp}
	}
	return sha
}

func (r *Repo) getObject(sha string) *Object {
	return r.Objects[sha]
}

// parseCommit extracts tree, parents, message from a commit object.
type CommitInfo struct {
	Tree    string
	Parents []string
	Author  string
	Message string
	Raw     []byte
}

func parseCommit(data []byte) *CommitInfo {
	ci := &CommitInfo{Raw: data}
	hdrEnd := -1
	for i := 0; i+1 < len(data); i++ {
		if data[i] == '\n' && data[i+1] == '\n' {
			hdrEnd = i
			break
		}
	}
	var hdr, msg []byte
	if hdrEnd >= 0 {
		hdr = data[:hdrEnd]
		msg = data[hdrEnd+2:]
	} else {
		hdr = data
	}
	ci.Message = string(msg)
	for _, line := range strings.Split(string(hdr), "\n") {
		switch {
		case strings.HasPrefix(line, "tree "):
			ci.Tree = strings.TrimSpace(line[5:])
		case strings.HasPrefix(line, "parent "):
			ci.Parents = append(ci.Parents, strings.TrimSpace(line[7:]))
		case strings.HasPrefix(line, "author "):
			ci.Author = strings.TrimSpace(line[7:])
		}
	}
	return ci
}

// commitClosure returns all commit SHAs reachable from tips (BFS, parents).
func (r *Repo) commitClosure(tips []string) map[string]bool {
	out := map[string]bool{}
	queue := append([]string{}, tips...)
	for len(queue) > 0 {
		sha := queue[0]
		queue = queue[1:]
		if sha == "" || out[sha] {
			continue
		}
		out[sha] = true
		obj := r.Objects[sha]
		if obj == nil || obj.Type != "commit" {
			continue
		}
		ci := parseCommit(obj.Data)
		queue = append(queue, ci.Parents...)
	}
	return out
}

// treeClosure returns all tree/blob SHAs reachable from a tree (recursive).
func (r *Repo) treeClosure(treeSHA string, out map[string]bool) {
	if treeSHA == "" || out[treeSHA] {
		return
	}
	out[treeSHA] = true
	obj := r.Objects[treeSHA]
	if obj == nil || obj.Type != "tree" {
		return
	}
	for _, e := range parseTree(obj.Data) {
		if e.Type == "tree" {
			r.treeClosure(e.SHA, out)
		} else {
			out[e.SHA] = true
		}
	}
}

type TreeEntry struct {
	Mode string
	Name string
	SHA  string
	Type string // "tree" or "blob"
}

func parseTree(data []byte) []TreeEntry {
	var out []TreeEntry
	i := 0
	for i < len(data) {
		sp := strings.IndexByte(string(data[i:]), ' ')
		if sp < 0 {
			break
		}
		mode := string(data[i : i+sp])
		i += sp + 1
		nul := strings.IndexByte(string(data[i:]), 0)
		if nul < 0 || i+nul+21 > len(data) {
			break
		}
		name := string(data[i : i+nul])
		i += nul + 1
		sha := hex.EncodeToString(data[i : i+20])
		i += 20
		typ := "blob"
		if mode == "40000" {
			typ = "tree"
		}
		out = append(out, TreeEntry{Mode: mode, Name: name, SHA: sha, Type: typ})
	}
	return out
}

func buildTree(entries []TreeEntry) []byte {
	sort.Slice(entries, func(i, j int) bool {
		ni, nj := entries[i].Name, entries[j].Name
		if entries[i].Type == "tree" {
			ni += "/"
		}
		if entries[j].Type == "tree" {
			nj += "/"
		}
		return ni < nj
	})
	var b strings.Builder
	for _, e := range entries {
		mode := e.Mode
		if e.Type == "tree" {
			mode = "40000"
		}
		raw, _ := hex.DecodeString(e.SHA)
		b.WriteString(mode + " " + e.Name + "\x00")
		b.Write(raw)
	}
	return []byte(b.String())
}

// sortedRefs returns ref names sorted.
func (r *Repo) sortedRefNames() []string {
	names := make([]string, 0, len(r.Refs))
	for n := range r.Refs {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

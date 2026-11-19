package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"hash/crc32"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

func nowUTC() time.Time { return time.Now().UTC() }

func ts(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-OAuth-Scopes", "repo, user")
	var rid [8]byte
	if _, err := rand.Read(rid[:]); err == nil {
		w.Header().Set("X-GitHub-Request-Id", hex.EncodeToString(rid[:]))
	}
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"message": msg, "documentation_url": "https://docs.github.com/rest"})
}

func readBodyJSON(r *http.Request) map[string]any {
	m, _ := readBodyJSONErr(r)
	return m
}

func readBodyJSONErr(r *http.Request) (map[string]any, error) {
	var m map[string]any
	if r.Body == nil {
		return map[string]any{}, nil
	}
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	if m == nil {
		return map[string]any{}, nil
	}
	return m, nil
}

func strField(m map[string]any, k string) string {
	if v, ok := m[k].(string); ok {
		return v
	}
	return ""
}

func boolField(m map[string]any, k string, def bool) bool {
	if v, ok := m[k].(bool); ok {
		return v
	}
	return def
}

func strSliceField(m map[string]any, k string) []string {
	v, ok := m[k].([]any)
	if !ok {
		return nil
	}
	var out []string
	for _, x := range v {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func currentUser(r *http.Request) string {
	// Resolve the caller from the Authorization header. The conformance
	// kit authenticates with the fixed test token, which belongs to the
	// fixed test user "adaiversen". Any other non-empty token is a
	// different (foreign) authenticated user; a missing token is
	// unauthenticated.
	auth := r.Header.Get("Authorization")
	tok := ""
	switch {
	case strings.HasPrefix(auth, "Bearer "):
		tok = strings.TrimSpace(auth[len("Bearer "):])
	case strings.HasPrefix(auth, "token "):
		tok = strings.TrimSpace(auth[len("token "):])
	case strings.HasPrefix(auth, "Basic "):
		if raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(auth[len("Basic "):])); err == nil {
			u, p, _ := strings.Cut(string(raw), ":")
			if p == "test-token" || u == "test-token" {
				tok = "test-token"
			} else if p != "" {
				tok = p
			} else {
				tok = u
			}
		}
	}
	if tok == "" {
		return ""
	}
	if tok == "test-token" {
		return "adaiversen"
	}
	return "foreign-user"
}

func numID(s string) int {
	return int(crc32.ChecksumIEEE([]byte(s))) + 1
}

func userJSON(login string) map[string]any {
	return map[string]any{
		"id": numID("user:" + login), "login": login, "type": "User",
		"html_url": "https://github.com/" + login,
		"url":      baseURL() + "/users/" + login,
		"avatar_url": "https://avatars.githubusercontent.com/u/" + strconv.Itoa(numID("user:"+login)) + "?v=4",
	}
}

func baseURL() string { return serverBaseURL }

func repoJSON(r *Repo) map[string]any {
	openIssues := 0
	for _, is := range r.Issues {
		if is.State == "open" {
			openIssues++
		}
	}
	for _, p := range r.Pulls {
		if p.State == "open" {
			openIssues++
		}
	}
	return map[string]any{
		"id": numID("repo:" + r.FullName), "node_id": "R_" + r.FullName,
		"name": r.Name, "full_name": r.FullName, "private": r.Private,
		"owner": userJSON(r.Owner), "html_url": "https://github.com/" + r.FullName,
		"description": r.Description, "fork": false,
		"url": baseURL() + "/repos/" + r.FullName,
		"created_at": ts(r.CreatedAt), "updated_at": ts(r.UpdatedAt), "pushed_at": ts(r.PushedAt),
		"git_url": "git://github.com/" + r.FullName + ".git",
		"ssh_url": "git@github.com:" + r.FullName + ".git",
		"clone_url": "https://github.com/" + r.FullName + ".git",
		"homepage": r.Homepage, "size": len(r.Objects),
		"stargazers_count": r.Stars, "watchers_count": r.Watchers, "forks_count": r.Forks,
		"language": r.Language, "has_issues": true, "has_projects": true,
		"has_downloads": true, "has_wiki": true, "has_pages": false,
		"archived": r.Archived, "disabled": r.Disabled,
		"open_issues_count": openIssues, "license": map[string]any{"key": "mit", "name": "MIT License", "spdx_id": "MIT"},
		"allow_forking": true, "is_template": false,
		"topics": r.Topics, "visibility": map[bool]string{true: "private", false: "public"}[r.Private],
		"default_branch": r.DefaultBranch,
	}
}

func paginate(r *http.Request, n int) (page, perPage int) {
	page, perPage = 1, 30
	if v := r.URL.Query().Get("page"); v != "" {
		if p, err := strconv.Atoi(v); err == nil && p > 0 {
			page = p
		}
	}
	if v := r.URL.Query().Get("per_page"); v != "" {
		if p, err := strconv.Atoi(v); err == nil && p > 0 && p <= 100 {
			perPage = p
		}
	}
	return page, perPage
}

func slicePage[T any](items []T, r *http.Request) []T {
	page, per := paginate(r, len(items))
	start := (page - 1) * per
	if start >= len(items) {
		return []T{}
	}
	end := start + per
	if end > len(items) {
		end = len(items)
	}
	return items[start:end]
}

// pageLinks emits GitHub-style Link headers (next/last/first/prev) for a
// paginated list. The URL preserves the request path (including the
// /api/v3 prefix, when present) and the host the client used.
func pageLinks(w http.ResponseWriter, r *http.Request, total int) {
	page, per := paginate(r, total)
	last := (total + per - 1) / per
	if last < 1 {
		last = 1
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	base := scheme + "://" + r.Host + r.URL.Path
	var links []string
	if page < last {
		links = append(links, "<"+base+"?page="+strconv.Itoa(page+1)+"&per_page="+strconv.Itoa(per)+">; rel=\"next\"")
		links = append(links, "<"+base+"?page="+strconv.Itoa(last)+"&per_page="+strconv.Itoa(per)+">; rel=\"last\"")
	}
	if page > 1 {
		links = append(links, "<"+base+"?page=1&per_page="+strconv.Itoa(per)+">; rel=\"first\"")
		links = append(links, "<"+base+"?page="+strconv.Itoa(page-1)+"&per_page="+strconv.Itoa(per)+">; rel=\"prev\"")
	}
	if len(links) > 0 {
		w.Header().Set("Link", strings.Join(links, ", "))
	}
}

// ---------- repo handlers ----------

func handleCreateRepo(w http.ResponseWriter, r *http.Request, owner string) {
	m, err := readBodyJSONErr(r)
	if err != nil {
		writeErr(w, 400, "Bad Request")
		return
	}
	name := strField(m, "name")
	if name == "" {
		writeErr(w, 422, "name is required")
		return
	}
	repo, ok := store.CreateRepo(owner, name, strField(m, "description"), boolField(m, "private", false))
	if !ok {
		writeErr(w, 422, "Repository already exists")
		return
	}
	if v := strField(m, "default_branch"); v != "" {
		repo.DefaultBranch = v
	}
	if hp := strField(m, "homepage"); hp != "" {
		repo.Homepage = hp
	}
	repo.Topics = strSliceField(m, "topics")
	writeJSON(w, 201, repoJSON(repo))
}

func handleGetRepo(w http.ResponseWriter, r *http.Request, repo *Repo) {
	writeJSON(w, 200, repoJSON(repo))
}

func handlePatchRepo(w http.ResponseWriter, r *http.Request, repo *Repo) {
	m := readBodyJSON(r)
	store.mu.Lock()
	if v := strField(m, "description"); v != "" {
		repo.Description = v
	}
	if v := strField(m, "default_branch"); v != "" {
		repo.DefaultBranch = v
	}
	if v := strField(m, "homepage"); v != "" {
		repo.Homepage = v
	}
	if v, ok := m["private"].(bool); ok {
		repo.Private = v
	}
	if v := strSliceField(m, "topics"); v != nil {
		repo.Topics = v
	}
	repo.UpdatedAt = nowUTC()
	store.mu.Unlock()
	writeJSON(w, 200, repoJSON(repo))
}

// ---------- branches / commits / trees / blobs / refs ----------

func branchJSON(repo *Repo, name string) map[string]any {
	sha := repo.Refs["refs/heads/"+name]
	return map[string]any{
		"name": name,
		"commit": map[string]any{
			"sha": sha,
			"url": baseURL() + "/repos/" + repo.FullName + "/commits/" + sha,
		},
		"protected": false,
	}
}

func handleListBranches(w http.ResponseWriter, r *http.Request, repo *Repo) {
	store.mu.RLock()
	defer store.mu.RUnlock()
	var names []string
	for ref := range repo.Refs {
		if strings.HasPrefix(ref, "refs/heads/") {
			names = append(names, strings.TrimPrefix(ref, "refs/heads/"))
		}
	}
	sort.Strings(names)
	var out []any
	for _, n := range names {
		out = append(out, branchJSON(repo, n))
	}
	if out == nil {
		out = []any{}
	}
	pageLinks(w, r, len(out))
	writeJSON(w, 200, slicePage(out, r))
}

func commitJSON(repo *Repo, sha string) map[string]any {
	o := repo.Objects[sha]
	if o == nil || o.Type != "commit" {
		return nil
	}
	ci := parseCommit(o.Data)
	msg := strings.TrimRight(ci.Message, "\n")
	author := parsePerson(ci.Author)
	var parents []any
	for _, p := range ci.Parents {
		parents = append(parents, map[string]any{
			"sha": p, "url": baseURL() + "/repos/" + repo.FullName + "/commits/" + p,
			"html_url": "https://github.com/" + repo.FullName + "/commit/" + p,
		})
	}
	if parents == nil {
		parents = []any{}
	}
	return map[string]any{
		"sha": sha,
		"commit": map[string]any{
			"author": author, "committer": author,
			"message": msg,
			"tree": map[string]any{
				"sha": ci.Tree,
				"url": baseURL() + "/repos/" + repo.FullName + "/git/trees/" + ci.Tree,
			},
			"comment_count": 0,
		},
		"author": userJSON(loginFromPerson(ci.Author)),
		"committer": userJSON(loginFromPerson(ci.Author)),
		"parents": parents,
		"url": baseURL() + "/repos/" + repo.FullName + "/commits/" + sha,
		"html_url": "https://github.com/" + repo.FullName + "/commit/" + sha,
	}
}

func parsePerson(s string) map[string]any {
	// "Name <email> timestamp tz"
	name, email, date := "", "", ""
	if i := strings.Index(s, "<"); i >= 0 {
		name = strings.TrimSpace(s[:i])
		rest := s[i+1:]
		if j := strings.Index(rest, ">"); j >= 0 {
			email = rest[:j]
			rest = strings.TrimSpace(rest[j+1:])
			parts := strings.Fields(rest)
			if len(parts) > 0 {
				if ts64, err := strconv.ParseInt(parts[0], 10, 64); err == nil {
					date = time.Unix(ts64, 0).UTC().Format(time.RFC3339)
				}
			}
		}
	}
	if name == "" {
		name = s
	}
	if date == "" {
		date = ts(nowUTC())
	}
	return map[string]any{"name": name, "email": email, "date": date}
}

func loginFromPerson(s string) string {
	name, _, _ := strings.Cut(s, "<")
	name = strings.TrimSpace(name)
	if name == "" {
		return "test-user"
	}
	return strings.ToLower(strings.ReplaceAll(name, " ", "-"))
}

func resolveRef(repo *Repo, ref string) string {
	if sha, ok := repo.Refs["refs/heads/"+ref]; ok {
		return sha
	}
	if sha, ok := repo.Refs["refs/tags/"+ref]; ok {
		return sha
	}
	if sha, ok := repo.Refs[ref]; ok {
		return sha
	}
	if len(ref) >= 7 {
		for sha := range repo.Objects {
			if strings.HasPrefix(sha, ref) {
				return sha
			}
		}
	}
	return ""
}

func handleListCommits(w http.ResponseWriter, r *http.Request, repo *Repo) {
	store.mu.RLock()
	defer store.mu.RUnlock()
	sha := repo.headSHA()
	if v := r.URL.Query().Get("sha"); v != "" {
		if s := resolveRef(repo, v); s != "" {
			sha = s
		}
	}
	var out []any
	seen := map[string]bool{}
	queue := []string{sha}
	for len(queue) > 0 {
		c := queue[0]
		queue = queue[1:]
		if c == "" || seen[c] {
			continue
		}
		seen[c] = true
		cj := commitJSON(repo, c)
		if cj == nil {
			continue
		}
		out = append(out, cj)
		ci := parseCommit(repo.Objects[c].Data)
		queue = append(queue, ci.Parents...)
	}
	if out == nil {
		out = []any{}
	}
	pageLinks(w, r, len(out))
	writeJSON(w, 200, slicePage(out, r))
}

func handleGetCommit(w http.ResponseWriter, r *http.Request, repo *Repo, ref string) {
	store.mu.RLock()
	defer store.mu.RUnlock()
	sha := resolveRef(repo, ref)
	if sha == "" {
		sha = ref
	}
	cj := commitJSON(repo, sha)
	if cj == nil {
		writeErr(w, 404, "Not Found")
		return
	}
	writeJSON(w, 200, cj)
}

// flat tree: path -> entry
func flatTree(repo *Repo, treeSHA string) map[string]TreeEntry {
	out := map[string]TreeEntry{}
	var walk func(tsha, prefix string)
	walk = func(tsha, prefix string) {
		o := repo.Objects[tsha]
		if o == nil || o.Type != "tree" {
			return
		}
		for _, e := range parseTree(o.Data) {
			p := prefix + e.Name
			if e.Type == "tree" {
				walk(e.SHA, p+"/")
			} else {
				out[p] = e
			}
		}
	}
	walk(treeSHA, "")
	return out
}

// build nested trees from flat path map; returns root tree SHA. Mutates repo objects.
func buildTreeFromFlat(repo *Repo, flat map[string]TreeEntry) string {
	type node struct {
		trees map[string]*node
		files map[string]TreeEntry
	}
	root := &node{trees: map[string]*node{}, files: map[string]TreeEntry{}}
	for path, e := range flat {
		parts := strings.Split(path, "/")
		cur := root
		for _, p := range parts[:len(parts)-1] {
			if cur.trees[p] == nil {
				cur.trees[p] = &node{trees: map[string]*node{}, files: map[string]TreeEntry{}}
			}
			cur = cur.trees[p]
		}
		cur.files[parts[len(parts)-1]] = e
	}
	var build func(n *node) string
	build = func(n *node) string {
		var entries []TreeEntry
		for name, e := range n.files {
			entries = append(entries, TreeEntry{Mode: e.Mode, Name: name, SHA: e.SHA, Type: "blob"})
		}
		for name, sub := range n.trees {
			sha := build(sub)
			entries = append(entries, TreeEntry{Mode: "40000", Name: name, SHA: sha, Type: "tree"})
		}
		return repo.putObject("tree", buildTree(entries))
	}
	return build(root)
}

func handleGetTree(w http.ResponseWriter, r *http.Request, repo *Repo, sha string) {
	store.mu.RLock()
	defer store.mu.RUnlock()
	if s := resolveRef(repo, sha); s != "" {
		sha = s
	}
	o := repo.Objects[sha]
	if o == nil || o.Type != "tree" {
		writeErr(w, 404, "Not Found")
		return
	}
	recursive := r.URL.Query().Get("recursive") == "1"
	var entries []any
	if recursive {
		flat := flatTree(repo, sha)
		paths := make([]string, 0, len(flat))
		for p := range flat {
			paths = append(paths, p)
		}
		sort.Strings(paths)
		for _, p := range paths {
			e := flat[p]
			entries = append(entries, map[string]any{
				"path": p, "mode": e.Mode, "type": "blob", "sha": e.SHA,
				"size": len(repo.Objects[e.SHA].Data),
				"url": baseURL() + "/repos/" + repo.FullName + "/git/blobs/" + e.SHA,
			})
		}
	} else {
		for _, e := range parseTree(o.Data) {
			ent := map[string]any{
				"path": e.Name, "mode": e.Mode, "type": e.Type, "sha": e.SHA,
				"url": baseURL() + "/repos/" + repo.FullName + "/git/" + e.Type + "s/" + e.SHA,
			}
			if e.Type == "blob" {
				ent["size"] = len(repo.Objects[e.SHA].Data)
			}
			entries = append(entries, ent)
		}
	}
	if entries == nil {
		entries = []any{}
	}
	writeJSON(w, 200, map[string]any{
		"sha": sha, "url": baseURL() + "/repos/" + repo.FullName + "/git/trees/" + sha,
		"tree": entries, "truncated": false,
	})
}

func handleCreateTree(w http.ResponseWriter, r *http.Request, repo *Repo) {
	m := readBodyJSON(r)
	store.mu.Lock()
	defer store.mu.Unlock()
	var base string
	if v := strField(m, "base_tree"); v != "" {
		base = v
	}
	flat := map[string]TreeEntry{}
	if base != "" {
		if s := resolveRef(repo, base); s != "" {
			base = s
		}
		flat = flatTree(repo, base)
	}
	if arr, ok := m["tree"].([]any); ok {
		for _, item := range arr {
			im, _ := item.(map[string]any)
			if im == nil {
				continue
			}
			path := strField(im, "path")
			typ := strField(im, "type")
			mode := strField(im, "mode")
			if mode == "" {
				mode = "100644"
			}
			if typ == "tree" {
				continue
			}
			var sha string
			if s := strField(im, "sha"); s != "" {
				sha = s
			} else if content, ok := im["content"].(string); ok {
				sha = repo.putObject("blob", []byte(content))
			}
			if sha == "" {
				continue
			}
			flat[path] = TreeEntry{Mode: mode, Name: path, SHA: sha, Type: "blob"}
		}
	}
	rootSHA := buildTreeFromFlat(repo, flat)
	writeJSON(w, 201, map[string]any{
		"sha": rootSHA,
		"url": baseURL() + "/repos/" + repo.FullName + "/git/trees/" + rootSHA,
		"tree": []any{}, "truncated": false,
	})
}

func handleGetBlob(w http.ResponseWriter, r *http.Request, repo *Repo, sha string) {
	store.mu.RLock()
	defer store.mu.RUnlock()
	o := repo.Objects[sha]
	if o == nil || o.Type != "blob" {
		writeErr(w, 404, "Not Found")
		return
	}
	writeJSON(w, 200, map[string]any{
		"sha": sha, "size": len(o.Data), "encoding": "base64",
		"content": base64.StdEncoding.EncodeToString(o.Data),
		"url": baseURL() + "/repos/" + repo.FullName + "/git/blobs/" + sha,
	})
}

func handleCreateBlob(w http.ResponseWriter, r *http.Request, repo *Repo) {
	m := readBodyJSON(r)
	content := strField(m, "content")
	enc := strField(m, "encoding")
	var data []byte
	if enc == "base64" {
		data, _ = base64.StdEncoding.DecodeString(content)
	} else {
		data = []byte(content)
	}
	store.mu.Lock()
	sha := repo.putObject("blob", data)
	store.mu.Unlock()
	writeJSON(w, 201, map[string]any{
		"sha": sha, "size": len(data),
		"url": baseURL() + "/repos/" + repo.FullName + "/git/blobs/" + sha,
	})
}

func handleCreateCommit(w http.ResponseWriter, r *http.Request, repo *Repo) {
	m := readBodyJSON(r)
	msg := strField(m, "message")
	tree := strField(m, "tree")
	author := currentUser(r) + " <" + currentUser(r) + "@users.noreply.github.com>"
	if am, ok := m["author"].(map[string]any); ok {
		n := strField(am, "name")
		e := strField(am, "email")
		if n != "" {
			author = n + " <" + e + ">"
		}
	}
	now := time.Now().Unix()
	var parents []string
	if arr, ok := m["parents"].([]any); ok {
		for _, p := range arr {
			if s, ok := p.(string); ok {
				parents = append(parents, s)
			}
		}
	}
	var b strings.Builder
	b.WriteString("tree " + tree + "\n")
	for _, p := range parents {
		b.WriteString("parent " + p + "\n")
	}
	b.WriteString("author " + author + " " + strconv.FormatInt(now, 10) + " +0000\n")
	b.WriteString("committer " + author + " " + strconv.FormatInt(now, 10) + " +0000\n")
	b.WriteString("\n" + msg + "\n")
	store.mu.Lock()
	sha := repo.putObject("commit", []byte(b.String()))
	store.mu.Unlock()
	store.mu.RLock()
	cj := commitJSON(repo, sha)
	store.mu.RUnlock()
	writeJSON(w, 201, cj)
}

func handleGetRef(w http.ResponseWriter, r *http.Request, repo *Repo, ref string) {
	store.mu.RLock()
	defer store.mu.RUnlock()
	full := "refs/" + ref
	sha, ok := repo.Refs[full]
	if !ok {
		writeErr(w, 404, "Not Found")
		return
	}
	typ := "commit"
	if o := repo.Objects[sha]; o != nil {
		typ = o.Type
	}
	writeJSON(w, 200, map[string]any{
		"ref": full, "node_id": "REF_" + full,
		"url": baseURL() + "/repos/" + repo.FullName + "/git/refs/" + ref,
		"object": map[string]any{
			"sha": sha, "type": typ,
			"url": baseURL() + "/repos/" + repo.FullName + "/git/" + typ + "s/" + sha,
		},
	})
}

func handleCreateTag(w http.ResponseWriter, r *http.Request, repo *Repo) {
	m := readBodyJSON(r)
	tag := strField(m, "tag")
	if tag == "" {
		tag = strField(m, "name")
	}
	msg := strField(m, "message")
	// object may be a plain SHA string or {"sha": ..., "type": ...}.
	obj := ""
	typ := strField(m, "type")
	switch v := m["object"].(type) {
	case string:
		obj = v
	case map[string]any:
		obj = strField(v, "sha")
		if typ == "" {
			typ = strField(v, "type")
		}
	}
	if typ == "" {
		typ = "commit"
	}
	if tag == "" || obj == "" {
		writeErr(w, 422, "tag and object are required")
		return
	}
	// tagger may be an object or a plain name string.
	var name, email string
	var unix int64 = time.Now().Unix()
	switch tg := m["tagger"].(type) {
	case string:
		name = tg
	case map[string]any:
		name = strField(tg, "name")
		email = strField(tg, "email")
		if v, ok := tg["date"]; ok {
			switch t := v.(type) {
			case string:
				if tm, err := time.Parse(time.RFC3339, t); err == nil {
					unix = tm.Unix()
				}
			case float64:
				unix = int64(t)
			}
		}
	}
	if name == "" {
		name = currentUser(r)
	}
	if email == "" {
		email = name + "@users.noreply.github.com"
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if _, ok := repo.Objects[obj]; !ok {
		writeErr(w, 422, "Object does not exist")
		return
	}
	data := "object " + obj + "\ntype " + typ + "\ntag " + tag + "\ntagger " + name + " <" + email + "> " + strconv.FormatInt(unix, 10) + " +0000\n\n" + msg + "\n"
	sha := repo.putObject("tag", []byte(data))
	writeJSON(w, 201, map[string]any{
		"sha": sha, "tag": tag, "message": msg,
		"object": map[string]any{"sha": obj, "type": typ},
		"tagger": map[string]any{"name": name, "email": email, "date": time.Unix(unix, 0).UTC().Format(time.RFC3339)},
		"url": baseURL() + "/repos/" + repo.FullName + "/git/tags/" + sha,
	})
}

func handleCreateRef(w http.ResponseWriter, r *http.Request, repo *Repo) {
	m := readBodyJSON(r)
	ref := strField(m, "ref")
	sha := strField(m, "sha")
	if ref == "" || sha == "" {
		writeErr(w, 422, "ref and sha required")
		return
	}
	if !strings.HasPrefix(ref, "refs/") {
		ref = "refs/" + ref
	}
	store.mu.Lock()
	if _, ok := repo.Refs[ref]; ok {
		store.mu.Unlock()
		writeErr(w, 422, "Reference already exists")
		return
	}
	if _, ok := repo.Objects[sha]; !ok {
		store.mu.Unlock()
		writeErr(w, 422, "Object does not exist")
		return
	}
	repo.Refs[ref] = sha
	store.mu.Unlock()
	store.mu.RLock()
	defer store.mu.RUnlock()
	typ := "commit"
	if o := repo.Objects[sha]; o != nil {
		typ = o.Type
	}
	writeJSON(w, 201, map[string]any{
		"ref": ref, "node_id": "REF_" + ref,
		"url": baseURL() + "/repos/" + repo.FullName + "/git/refs/" + strings.TrimPrefix(ref, "refs/"),
		"object": map[string]any{"sha": sha, "type": typ, "url": baseURL() + "/repos/" + repo.FullName + "/git/" + typ + "s/" + sha},
	})
}

func handlePatchRef(w http.ResponseWriter, r *http.Request, repo *Repo, ref string) {
	m := readBodyJSON(r)
	sha := strField(m, "sha")
	store.mu.Lock()
	full := "refs/" + ref
	if _, ok := repo.Refs[full]; !ok {
		store.mu.Unlock()
		writeErr(w, 404, "Not Found")
		return
	}
	if _, ok := repo.Objects[sha]; !ok {
		store.mu.Unlock()
		writeErr(w, 422, "Object does not exist")
		return
	}
	repo.Refs[full] = sha
	repo.UpdatedAt = nowUTC()
	store.mu.Unlock()
	handleGetRef(w, r, repo, ref)
}

func handleDeleteRef(w http.ResponseWriter, r *http.Request, repo *Repo, ref string) {
	store.mu.Lock()
	defer store.mu.Unlock()
	full := "refs/" + ref
	if _, ok := repo.Refs[full]; !ok {
		writeErr(w, 404, "Not Found")
		return
	}
	delete(repo.Refs, full)
	w.WriteHeader(204)
}

// ---------- contents ----------

func handleGetContents(w http.ResponseWriter, r *http.Request, repo *Repo, path string) {
	store.mu.RLock()
	defer store.mu.RUnlock()
	branch := r.URL.Query().Get("ref")
	if branch == "" {
		branch = repo.DefaultBranch
	}
	sha := repo.Refs["refs/heads/"+branch]
	if sha == "" {
		writeErr(w, 404, "Not Found")
		return
	}
	ci := parseCommit(repo.Objects[sha].Data)
	flat := flatTree(repo, ci.Tree)
	if path == "" {
		// root listing
		seen := map[string]bool{}
		var out []any
		for p, e := range flat {
			top := strings.SplitN(p, "/", 2)[0]
			if seen[top] {
				continue
			}
			seen[top] = true
			if strings.Contains(p, "/") {
				out = append(out, map[string]any{
					"type": "dir", "name": top, "path": top, "sha": "",
					"url": baseURL() + "/repos/" + repo.FullName + "/contents/" + top,
					"html_url": "https://github.com/" + repo.FullName + "/tree/" + branch + "/" + top,
				})
			} else {
				out = append(out, contentFileJSON(repo, top, e, branch))
			}
		}
		if out == nil {
			out = []any{}
		}
		writeJSON(w, 200, out)
		return
	}
	// exact file?
	if e, ok := flat[path]; ok {
		writeJSON(w, 200, contentFileJSON(repo, path, e, branch))
		return
	}
	// directory?
	prefix := path + "/"
	var out []any
	seen := map[string]bool{}
	for p, e := range flat {
		if !strings.HasPrefix(p, prefix) {
			continue
		}
		rest := strings.TrimPrefix(p, prefix)
		top := strings.SplitN(rest, "/", 2)[0]
		if seen[top] {
			continue
		}
		seen[top] = true
		if strings.Contains(rest, "/") {
			out = append(out, map[string]any{
				"type": "dir", "name": top, "path": path + "/" + top, "sha": "",
				"url": baseURL() + "/repos/" + repo.FullName + "/contents/" + path + "/" + top,
				"html_url": "https://github.com/" + repo.FullName + "/tree/" + branch + "/" + path + "/" + top,
			})
		} else {
			out = append(out, contentFileJSON(repo, p, e, branch))
		}
	}
	if out == nil {
		writeErr(w, 404, "Not Found")
		return
	}
	writeJSON(w, 200, out)
}

func contentFileJSON(repo *Repo, path string, e TreeEntry, branch string) map[string]any {
	data := repo.Objects[e.SHA].Data
	return map[string]any{
		"type": "file", "encoding": "base64", "size": len(data),
		"name": path[strings.LastIndex(path, "/")+1:], "path": path, "sha": e.SHA,
		"content": base64.StdEncoding.EncodeToString(data),
		"url": baseURL() + "/repos/" + repo.FullName + "/contents/" + path,
		"html_url": "https://github.com/" + repo.FullName + "/blob/" + branch + "/" + path,
		"git_url": baseURL() + "/repos/" + repo.FullName + "/git/blobs/" + e.SHA,
		"download_url": baseURL() + "/repos/" + repo.FullName + "/contents/" + path + "?raw=1",
	}
}

func handlePutContents(w http.ResponseWriter, r *http.Request, repo *Repo, path string) {
	m := readBodyJSON(r)
	msg := strField(m, "message")
	if msg == "" {
		msg = "Update " + path
	}
	contentB64 := strField(m, "content")
	data, _ := base64.StdEncoding.DecodeString(contentB64)
	branch := strField(m, "branch")
	if branch == "" {
		branch = repo.DefaultBranch
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	head := repo.Refs["refs/heads/"+branch]
	var flat map[string]TreeEntry
	parent := ""
	if head == "" {
		// Empty repo (or brand-new branch): the first contents write
		// creates the initial commit instead of failing.
		flat = map[string]TreeEntry{}
	} else {
		ci := parseCommit(repo.Objects[head].Data)
		flat = flatTree(repo, ci.Tree)
		parent = head
	}
	blobSHA := repo.putObject("blob", data)
	if wantSHA := strField(m, "sha"); wantSHA != "" {
		cur, ok := flat[path]
		if !ok || cur.SHA != wantSHA {
			writeErr(w, 409, "sha does not match")
			return
		}
	}
	flat[path] = TreeEntry{Mode: "100644", Name: path, SHA: blobSHA, Type: "blob"}
	newTree := buildTreeFromFlat(repo, flat)
	now := time.Now().Unix()
	author := currentUser(r) + " <" + currentUser(r) + "@users.noreply.github.com>"
	commitData := "tree " + newTree + "\n"
	if parent != "" {
		commitData += "parent " + parent + "\n"
	}
	commitData += "author " + author + " " + strconv.FormatInt(now, 10) + " +0000\ncommitter " + author + " " + strconv.FormatInt(now, 10) + " +0000\n\n" + msg + "\n"
	newCommit := repo.putObject("commit", []byte(commitData))
	repo.Refs["refs/heads/"+branch] = newCommit
	repo.UpdatedAt = nowUTC()
	repo.PushedAt = nowUTC()
	writeJSON(w, 201, map[string]any{
		"content": contentFileJSON(repo, path, TreeEntry{Mode: "100644", SHA: blobSHA}, branch),
		"commit":  commitJSON(repo, newCommit),
	})
}

func handleDeleteContents(w http.ResponseWriter, r *http.Request, repo *Repo, path string) {
	m := readBodyJSON(r)
	msg := strField(m, "message")
	if msg == "" {
		msg = "Delete " + path
	}
	branch := strField(m, "branch")
	if branch == "" {
		branch = repo.DefaultBranch
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	head := repo.Refs["refs/heads/"+branch]
	if head == "" {
		writeErr(w, 404, "Not Found")
		return
	}
	ci := parseCommit(repo.Objects[head].Data)
	flat := flatTree(repo, ci.Tree)
	cur, ok := flat[path]
	if !ok {
		writeErr(w, 404, "Not Found")
		return
	}
	if wantSHA := strField(m, "sha"); wantSHA != "" && wantSHA != cur.SHA {
		writeErr(w, 409, "sha does not match")
		return
	}
	delete(flat, path)
	newTree := buildTreeFromFlat(repo, flat)
	now := time.Now().Unix()
	author := currentUser(r) + " <" + currentUser(r) + "@users.noreply.github.com>"
	commitData := "tree " + newTree + "\nparent " + head + "\nauthor " + author + " " + strconv.FormatInt(now, 10) + " +0000\ncommitter " + author + " " + strconv.FormatInt(now, 10) + " +0000\n\n" + msg + "\n"
	newCommit := repo.putObject("commit", []byte(commitData))
	repo.Refs["refs/heads/"+branch] = newCommit
	repo.UpdatedAt = nowUTC()
	repo.PushedAt = nowUTC()
	writeJSON(w, 200, map[string]any{
		"content": nil,
		"commit":  commitJSON(repo, newCommit),
	})
}

// ---------- labels ----------

func labelJSON(repo *Repo, l Label) map[string]any {
	return map[string]any{
		"id":          numID("label:" + repo.FullName + ":" + l.Name),
		"node_id":     "L_" + repo.FullName + "_" + l.Name,
		"name":        l.Name,
		"color":       l.Color,
		"description": l.Description,
		"default":     false,
		"url":         baseURL() + "/repos/" + repo.FullName + "/labels/" + l.Name,
	}
}

func repoHasLabel(repo *Repo, name string) bool {
	for _, l := range repo.Labels {
		if strings.EqualFold(l.Name, name) {
			return true
		}
	}
	return false
}

func handleListLabels(w http.ResponseWriter, r *http.Request, repo *Repo) {
	store.mu.RLock()
	defer store.mu.RUnlock()
	var out []any
	for _, l := range repo.Labels {
		out = append(out, labelJSON(repo, l))
	}
	if out == nil {
		out = []any{}
	}
	writeJSON(w, 200, out)
}

func handleCreateLabel(w http.ResponseWriter, r *http.Request, repo *Repo) {
	m := readBodyJSON(r)
	name := strField(m, "name")
	if name == "" {
		writeErr(w, 422, "name is required")
		return
	}
	color := strField(m, "color")
	if color == "" {
		color = "ededed"
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	for _, l := range repo.Labels {
		if strings.EqualFold(l.Name, name) {
			writeErr(w, 422, "Label already exists")
			return
		}
	}
	l := Label{Name: name, Color: color, Description: strField(m, "description")}
	repo.Labels = append(repo.Labels, l)
	writeJSON(w, 201, labelJSON(repo, l))
}

// labelNameList extracts label names from a request field that may hold
// plain strings or {"name": ...} objects.
func labelNameList(m map[string]any, k string) []string {
	v, ok := m[k].([]any)
	if !ok {
		return nil
	}
	var out []string
	for _, x := range v {
		switch t := x.(type) {
		case string:
			out = append(out, t)
		case map[string]any:
			if s := strField(t, "name"); s != "" {
				out = append(out, s)
			}
		}
	}
	return out
}

// ---------- issues ----------

func issueJSON(repo *Repo, is *Issue) map[string]any {
	var labels []any
	for _, l := range is.Labels {
		labels = append(labels, map[string]any{"name": l, "color": "ededed"})
	}
	if labels == nil {
		labels = []any{}
	}
	var assignees []any
	for _, a := range is.Assignees {
		assignees = append(assignees, userJSON(a))
	}
	if assignees == nil {
		assignees = []any{}
	}
	return map[string]any{
		"id": numID("issue:" + repo.FullName + ":" + strconv.Itoa(is.Number)),
		"node_id": "I_" + repo.FullName + "_" + strconv.Itoa(is.Number),
		"number": is.Number, "title": is.Title, "body": is.Body, "state": is.State,
		"user": userJSON(is.User), "labels": labels, "assignees": assignees,
		"comments": len(is.Comments),
		"created_at": ts(is.CreatedAt), "updated_at": ts(is.UpdatedAt),
		"closed_at": nilIfTime(is.ClosedAt),
		"html_url": "https://github.com/" + repo.FullName + "/issues/" + strconv.Itoa(is.Number),
		"url": baseURL() + "/repos/" + repo.FullName + "/issues/" + strconv.Itoa(is.Number),
		"comments_url": baseURL() + "/repos/" + repo.FullName + "/issues/" + strconv.Itoa(is.Number) + "/comments",
	}
}

func nilIfTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return ts(*t)
}

func handleListIssues(w http.ResponseWriter, r *http.Request, repo *Repo) {
	store.mu.RLock()
	defer store.mu.RUnlock()
	state := r.URL.Query().Get("state")
	var issues []*Issue
	nums := make([]int, 0, len(repo.Issues))
	for n := range repo.Issues {
		nums = append(nums, n)
	}
	sort.Ints(nums)
	for _, n := range nums {
		is := repo.Issues[n]
		if state != "" && state != "all" && is.State != state {
			continue
		}
		issues = append(issues, is)
	}
	// GitHub sorts by created/desc by default; honour sort + direction.
	sortParam := r.URL.Query().Get("sort")
	dir := r.URL.Query().Get("direction")
	if dir == "" {
		dir = "desc"
	}
	desc := dir == "desc"
	sort.Slice(issues, func(i, j int) bool {
		a, b := issues[i], issues[j]
		switch sortParam {
		case "updated":
			if !a.UpdatedAt.Equal(b.UpdatedAt) {
				if desc {
					return a.UpdatedAt.After(b.UpdatedAt)
				}
				return a.UpdatedAt.Before(b.UpdatedAt)
			}
		case "comments":
			if len(a.Comments) != len(b.Comments) {
				if desc {
					return len(a.Comments) > len(b.Comments)
				}
				return len(a.Comments) < len(b.Comments)
			}
		default: // created
			if !a.CreatedAt.Equal(b.CreatedAt) {
				if desc {
					return a.CreatedAt.After(b.CreatedAt)
				}
				return a.CreatedAt.Before(b.CreatedAt)
			}
		}
		// Equal timestamps (created in the same instant): the higher
		// number is the newer issue.
		if desc {
			return a.Number > b.Number
		}
		return a.Number < b.Number
	})
	var out []any
	for _, is := range issues {
		out = append(out, issueJSON(repo, is))
	}
	if out == nil {
		out = []any{}
	}
	pageLinks(w, r, len(out))
	writeJSON(w, 200, slicePage(out, r))
}

func handleCreateIssue(w http.ResponseWriter, r *http.Request, repo *Repo) {
	m := readBodyJSON(r)
	title := strField(m, "title")
	if title == "" {
		writeErr(w, 422, "title is required")
		return
	}
	labels := labelNameList(m, "labels")
	if labels == nil {
		labels = strSliceField(m, "labels")
	}
	store.mu.Lock()
	for _, ln := range labels {
		if !repoHasLabel(repo, ln) {
			store.mu.Unlock()
			writeErr(w, 422, "Label does not exist")
			return
		}
	}
	n := repo.NextIssue
	repo.NextIssue++
	now := nowUTC()
	is := &Issue{
		Number: n, Title: title, Body: strField(m, "body"), State: "open",
		Labels: labels, Assignees: strSliceField(m, "assignees"),
		User: currentUser(r), CreatedAt: now, UpdatedAt: now,
	}
	if is.Labels == nil {
		is.Labels = []string{}
	}
	if is.Assignees == nil {
		is.Assignees = []string{}
	}
	repo.Issues[n] = is
	store.mu.Unlock()
	writeJSON(w, 201, issueJSON(repo, is))
}

func handleGetIssue(w http.ResponseWriter, r *http.Request, repo *Repo, n int) {
	store.mu.RLock()
	defer store.mu.RUnlock()
	is, ok := repo.Issues[n]
	if !ok {
		writeErr(w, 404, "Not Found")
		return
	}
	writeJSON(w, 200, issueJSON(repo, is))
}

func handlePatchIssue(w http.ResponseWriter, r *http.Request, repo *Repo, n int) {
	m := readBodyJSON(r)
	store.mu.Lock()
	is, ok := repo.Issues[n]
	if !ok {
		store.mu.Unlock()
		writeErr(w, 404, "Not Found")
		return
	}
	if v := strField(m, "title"); v != "" {
		is.Title = v
	}
	if v, ok := m["body"].(string); ok {
		is.Body = v
	}
	if v := strField(m, "state"); v == "open" || v == "closed" {
		is.State = v
		if v == "closed" {
			now := nowUTC()
			is.ClosedAt = &now
		} else {
			is.ClosedAt = nil
		}
	}
	if v := labelNameList(m, "labels"); v != nil {
		for _, ln := range v {
			if !repoHasLabel(repo, ln) {
				store.mu.Unlock()
				writeErr(w, 422, "Label does not exist")
				return
			}
		}
		is.Labels = v
	} else if v := strSliceField(m, "labels"); v != nil {
		for _, ln := range v {
			if !repoHasLabel(repo, ln) {
				store.mu.Unlock()
				writeErr(w, 422, "Label does not exist")
				return
			}
		}
		is.Labels = v
	}
	if v := strSliceField(m, "assignees"); v != nil {
		is.Assignees = v
	}
	is.UpdatedAt = nowUTC()
	store.mu.Unlock()
	writeJSON(w, 200, issueJSON(repo, is))
}

func commentJSON(repo *Repo, c *Comment, kind string, n int) map[string]any {
	return map[string]any{
		"id": c.ID, "node_id": "C_" + repo.FullName + "_" + strconv.Itoa(c.ID),
		"body": c.Body, "user": userJSON(c.User),
		"created_at": ts(c.CreatedAt), "updated_at": ts(c.UpdatedAt),
		"html_url": "https://github.com/" + repo.FullName + "/" + kind + "/" + strconv.Itoa(n) + "#issuecomment-" + strconv.Itoa(c.ID),
		"url": baseURL() + "/repos/" + repo.FullName + "/" + kind + "/" + strconv.Itoa(n) + "/comments/" + strconv.Itoa(c.ID),
	}
}

func handleAddIssueLabels(w http.ResponseWriter, r *http.Request, repo *Repo, n int) {
	m := readBodyJSON(r)
	labels := labelNameList(m, "labels")
	if labels == nil {
		labels = strSliceField(m, "labels")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	is, ok := repo.Issues[n]
	if !ok {
		writeErr(w, 404, "Not Found")
		return
	}
	for _, ln := range labels {
		if !repoHasLabel(repo, ln) {
			writeErr(w, 422, "Label does not exist")
			return
		}
	}
	for _, ln := range labels {
		found := false
		for _, e := range is.Labels {
			if strings.EqualFold(e, ln) {
				found = true
				break
			}
		}
		if !found {
			is.Labels = append(is.Labels, ln)
		}
	}
	is.UpdatedAt = nowUTC()
	var out []any
	for _, l := range is.Labels {
		out = append(out, map[string]any{"name": l, "color": "ededed"})
	}
	if out == nil {
		out = []any{}
	}
	writeJSON(w, 200, out)
}

func handleListIssueComments(w http.ResponseWriter, r *http.Request, repo *Repo, n int) {
	store.mu.RLock()
	defer store.mu.RUnlock()
	is, ok := repo.Issues[n]
	if !ok {
		writeErr(w, 404, "Not Found")
		return
	}
	var out []any
	for _, c := range is.Comments {
		out = append(out, commentJSON(repo, c, "issues", n))
	}
	if out == nil {
		out = []any{}
	}
	pageLinks(w, r, len(out))
	writeJSON(w, 200, slicePage(out, r))
}

func handleCreateIssueComment(w http.ResponseWriter, r *http.Request, repo *Repo, n int) {
	m := readBodyJSON(r)
	body := strField(m, "body")
	if body == "" {
		writeErr(w, 422, "body is required")
		return
	}
	store.mu.Lock()
	is, ok := repo.Issues[n]
	if !ok {
		store.mu.Unlock()
		writeErr(w, 404, "Not Found")
		return
	}
	id := repo.NextComment
	repo.NextComment++
	now := nowUTC()
	c := &Comment{ID: id, Body: body, User: currentUser(r), CreatedAt: now, UpdatedAt: now}
	is.Comments = append(is.Comments, c)
	is.UpdatedAt = now
	store.mu.Unlock()
	writeJSON(w, 201, commentJSON(repo, c, "issues", n))
}

// ---------- pulls ----------

func pullJSON(repo *Repo, p *Pull) map[string]any {
	headSHA := p.HeadSHA
	if headSHA == "" {
		headSHA = repo.Refs["refs/heads/"+p.Head]
	}
	baseSHA := p.BaseSHA
	if baseSHA == "" {
		baseSHA = repo.Refs["refs/heads/"+p.Base]
	}
	return map[string]any{
		"id": numID("pull:" + repo.FullName + ":" + strconv.Itoa(p.Number)),
		"node_id": "PR_" + repo.FullName + "_" + strconv.Itoa(p.Number),
		"number": p.Number, "title": p.Title, "body": p.Body,
		"state": map[bool]string{true: "closed", false: p.State}[p.Merged],
		"user": userJSON(p.User),
		"head": map[string]any{
			"label": repo.Owner + ":" + p.Head, "ref": p.Head, "sha": headSHA,
			"repo": repoJSON(repo),
		},
		"base": map[string]any{
			"label": repo.Owner + ":" + p.Base, "ref": p.Base, "sha": baseSHA,
			"repo": repoJSON(repo),
		},
		"merged": p.Merged, "mergeable": p.Mergeable, "draft": p.Draft,
		"merge_commit_sha": p.MergeSHA,
		"comments": len(p.Comments),
		"created_at": ts(p.CreatedAt), "updated_at": ts(p.UpdatedAt),
		"closed_at": nilIfTime(p.ClosedAt), "merged_at": nilIfTime(p.MergedAt),
		"html_url": "https://github.com/" + repo.FullName + "/pull/" + strconv.Itoa(p.Number),
		"url": baseURL() + "/repos/" + repo.FullName + "/pulls/" + strconv.Itoa(p.Number),
		"commits_url": baseURL() + "/repos/" + repo.FullName + "/pulls/" + strconv.Itoa(p.Number) + "/commits",
		"review_comments_url": baseURL() + "/repos/" + repo.FullName + "/pulls/" + strconv.Itoa(p.Number) + "/comments",
		"statuses_url": baseURL() + "/repos/" + repo.FullName + "/statuses/" + headSHA,
	}
}

func handleListPulls(w http.ResponseWriter, r *http.Request, repo *Repo) {
	store.mu.RLock()
	defer store.mu.RUnlock()
	state := r.URL.Query().Get("state")
	if state == "" {
		state = "open"
	}
	var out []any
	nums := make([]int, 0, len(repo.Pulls))
	for n := range repo.Pulls {
		nums = append(nums, n)
	}
	sort.Ints(nums)
	for _, n := range nums {
		p := repo.Pulls[n]
		if state != "all" {
			if state == "open" && p.State != "open" {
				continue
			}
			if state == "closed" && p.State == "open" {
				continue
			}
		}
		out = append(out, pullJSON(repo, p))
	}
	if out == nil {
		out = []any{}
	}
	pageLinks(w, r, len(out))
	writeJSON(w, 200, slicePage(out, r))
}

func handleCreatePull(w http.ResponseWriter, r *http.Request, repo *Repo) {
	m := readBodyJSON(r)
	title := strField(m, "title")
	head := strField(m, "head")
	base := strField(m, "base")
	if title == "" || head == "" || base == "" {
		writeErr(w, 422, "title, head and base are required")
		return
	}
	if i := strings.Index(head, ":"); i >= 0 {
		head = head[i+1:]
	}
	if i := strings.Index(base, ":"); i >= 0 {
		base = base[i+1:]
	}
	store.mu.Lock()
	headSHA := repo.Refs["refs/heads/"+head]
	baseSHA := repo.Refs["refs/heads/"+base]
	if headSHA == "" {
		store.mu.Unlock()
		writeErr(w, 422, "Head branch does not exist")
		return
	}
	if baseSHA == "" {
		store.mu.Unlock()
		writeErr(w, 422, "Base branch does not exist")
		return
	}
	n := repo.NextPull
	repo.NextPull++
	now := nowUTC()
	p := &Pull{
		Number: n, Title: title, Body: strField(m, "body"), State: "open",
		Head: head, Base: base, HeadSHA: headSHA, BaseSHA: baseSHA,
		Merged: false, Mergeable: true, Draft: boolField(m, "draft", false),
		User: currentUser(r), CreatedAt: now, UpdatedAt: now,
	}
	repo.Pulls[n] = p
	store.mu.Unlock()
	writeJSON(w, 201, pullJSON(repo, p))
}

func handleGetPull(w http.ResponseWriter, r *http.Request, repo *Repo, n int) {
	store.mu.RLock()
	defer store.mu.RUnlock()
	p, ok := repo.Pulls[n]
	if !ok {
		writeErr(w, 404, "Not Found")
		return
	}
	writeJSON(w, 200, pullJSON(repo, p))
}

func handlePatchPull(w http.ResponseWriter, r *http.Request, repo *Repo, n int) {
	m := readBodyJSON(r)
	store.mu.Lock()
	p, ok := repo.Pulls[n]
	if !ok {
		store.mu.Unlock()
		writeErr(w, 404, "Not Found")
		return
	}
	if v := strField(m, "title"); v != "" {
		p.Title = v
	}
	if v, ok := m["body"].(string); ok {
		p.Body = v
	}
	if v := strField(m, "state"); v == "open" || v == "closed" {
		p.State = v
		if v == "closed" {
			now := nowUTC()
			p.ClosedAt = &now
		} else {
			p.ClosedAt = nil
		}
	}
	if v, ok := m["base"].(string); ok && v != "" {
		p.Base = v
	}
	p.UpdatedAt = nowUTC()
	store.mu.Unlock()
	writeJSON(w, 200, pullJSON(repo, p))
}

func handleListPullCommits(w http.ResponseWriter, r *http.Request, repo *Repo, n int) {
	store.mu.RLock()
	defer store.mu.RUnlock()
	p, ok := repo.Pulls[n]
	if !ok {
		writeErr(w, 404, "Not Found")
		return
	}
	baseSet := repo.commitClosure([]string{p.BaseSHA})
	var out []any
	seen := map[string]bool{}
	queue := []string{p.HeadSHA}
	for len(queue) > 0 {
		c := queue[0]
		queue = queue[1:]
		if c == "" || seen[c] || baseSet[c] {
			continue
		}
		seen[c] = true
		cj := commitJSON(repo, c)
		if cj == nil {
			continue
		}
		out = append(out, cj)
		ci := parseCommit(repo.Objects[c].Data)
		queue = append(queue, ci.Parents...)
	}
	if out == nil {
		out = []any{}
	}
	pageLinks(w, r, len(out))
	writeJSON(w, 200, slicePage(out, r))
}

type pullFile struct {
	SHA       string
	Path      string
	Status    string // added | modified | removed
	Additions int
	Deletions int
}

// pullFiles diffs a pull request's head against its base (per file).
// It uses the current branch tips, like the merge does.
func pullFiles(repo *Repo, p *Pull) []pullFile {
	headSHA := p.HeadSHA
	if s := repo.Refs["refs/heads/"+p.Head]; s != "" {
		headSHA = s
	}
	baseSHA := p.BaseSHA
	if s := repo.Refs["refs/heads/"+p.Base]; s != "" {
		baseSHA = s
	}
	headCI := parseCommit(repo.Objects[headSHA].Data)
	baseCI := parseCommit(repo.Objects[baseSHA].Data)
	headFlat := flatTree(repo, headCI.Tree)
	baseFlat := flatTree(repo, baseCI.Tree)
	paths := map[string]bool{}
	for k := range headFlat {
		paths[k] = true
	}
	for k := range baseFlat {
		paths[k] = true
	}
	sorted := make([]string, 0, len(paths))
	for k := range paths {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)
	var out []pullFile
	for _, path := range sorted {
		h, hok := headFlat[path]
		b, bok := baseFlat[path]
		status := "modified"
		if !bok {
			status = "added"
		} else if !hok {
			status = "removed"
		} else if h.SHA == b.SHA {
			continue
		}
		additions, deletions := 0, 0
		if hok {
			additions = len(strings.Split(string(repo.Objects[h.SHA].Data), "\n"))
		}
		if bok {
			deletions = len(strings.Split(string(repo.Objects[b.SHA].Data), "\n"))
		}
		out = append(out, pullFile{SHA: h.SHA, Path: path, Status: status, Additions: additions, Deletions: deletions})
	}
	return out
}

func handleListPullFiles(w http.ResponseWriter, r *http.Request, repo *Repo, n int) {
	store.mu.RLock()
	defer store.mu.RUnlock()
	p, ok := repo.Pulls[n]
	if !ok {
		writeErr(w, 404, "Not Found")
		return
	}
	var out []any
	for _, f := range pullFiles(repo, p) {
		out = append(out, map[string]any{
			"sha": f.SHA, "filename": f.Path, "status": f.Status,
			"additions": f.Additions, "deletions": f.Deletions, "changes": f.Additions + f.Deletions,
			"blob_url": baseURL() + "/repos/" + repo.FullName + "/git/blobs/" + f.SHA,
			"raw_url": baseURL() + "/repos/" + repo.FullName + "/contents/" + f.Path + "?raw=1",
			"contents_url": baseURL() + "/repos/" + repo.FullName + "/contents/" + f.Path,
			"patch": "",
		})
	}
	if out == nil {
		out = []any{}
	}
	pageLinks(w, r, len(out))
	writeJSON(w, 200, slicePage(out, r))
}

func handleMergePull(w http.ResponseWriter, r *http.Request, repo *Repo, n int) {
	store.mu.Lock()
	defer store.mu.Unlock()
	p, ok := repo.Pulls[n]
	if !ok {
		writeErr(w, 404, "Not Found")
		return
	}
	if p.Merged {
		writeErr(w, 405, "Pull request already merged")
		return
	}
	if p.State != "open" {
		writeErr(w, 405, "Pull request is closed")
		return
	}
	author := currentUser(r) + " <" + currentUser(r) + "@users.noreply.github.com>"
	newSHA, conflict := doMergePull(repo, p, author)
	if conflict {
		writeErr(w, 405, "Merge conflict")
		return
	}
	p.Merged = true
	p.MergeSHA = newSHA
	p.State = "closed"
	nowT := nowUTC()
	p.MergedAt = &nowT
	p.ClosedAt = &nowT
	p.UpdatedAt = nowT
	repo.UpdatedAt = nowT
	repo.PushedAt = nowT
	writeJSON(w, 200, map[string]any{
		"sha": newSHA, "merged": true,
		"message": "Pull Request successfully merged",
	})
}

// doMergePull merges p into its base branch with a real per-file 3-way
// merge. Returns (mergeSHA, conflict). Call with store.mu held. On
// conflict the repo is left untouched. The merge uses the CURRENT branch
// tips (a pull request merges the live branches, not the snapshot taken
// when it was opened).
func doMergePull(repo *Repo, p *Pull, author string) (string, bool) {
	headSHA := p.HeadSHA
	if s := repo.Refs["refs/heads/"+p.Head]; s != "" {
		headSHA = s
	}
	baseSHA := p.BaseSHA
	if s := repo.Refs["refs/heads/"+p.Base]; s != "" {
		baseSHA = s
	}
	p.HeadSHA, p.BaseSHA = headSHA, baseSHA
	headCI := parseCommit(repo.Objects[headSHA].Data)
	baseSet := repo.commitClosure([]string{baseSHA})
	now := time.Now().Unix()
	if baseSet[headSHA] {
		// Head already contains the base tip: nothing to merge.
		return headSHA, false
	}
	if isAncestor(repo, baseSHA, headSHA) {
		// Fast-forward.
		repo.Refs["refs/heads/"+p.Base] = headSHA
		return headSHA, false
	}
	base := mergeBase(repo, baseSHA, headSHA)
	var baseFlat map[string]TreeEntry
	if base != "" {
		baseCI := parseCommit(repo.Objects[base].Data)
		baseFlat = flatTree(repo, baseCI.Tree)
	} else {
		baseFlat = map[string]TreeEntry{}
	}
	headFlat := flatTree(repo, headCI.Tree)
	baseTipCI := parseCommit(repo.Objects[baseSHA].Data)
	tipFlat := flatTree(repo, baseTipCI.Tree)
	merged := map[string]TreeEntry{}
	paths := map[string]bool{}
	for k := range baseFlat {
		paths[k] = true
	}
	for k := range headFlat {
		paths[k] = true
	}
	for k := range tipFlat {
		paths[k] = true
	}
	for path := range paths {
		b, bok := baseFlat[path]
		h, hok := headFlat[path]
		t, tok := tipFlat[path]
		switch {
		case hok && tok && h.SHA == t.SHA:
			merged[path] = h
		case bok && hok && h.SHA == b.SHA:
			// Head unchanged; take the base-tip version (or delete).
			if tok {
				merged[path] = t
			}
		case bok && tok && t.SHA == b.SHA:
			// Base tip unchanged; take the head version (or delete).
			if hok {
				merged[path] = h
			}
		case !bok && hok && !tok:
			merged[path] = h
		case !bok && tok && !hok:
			merged[path] = t
		default:
			// Both sides changed the same path differently (or one
			// side modified while the other deleted it).
			return "", true
		}
	}
	newTree := buildTreeFromFlat(repo, merged)
	commitData := "tree " + newTree + "\nparent " + p.BaseSHA + "\nparent " + p.HeadSHA + "\nauthor " + author + " " + strconv.FormatInt(now, 10) + " +0000\ncommitter " + author + " " + strconv.FormatInt(now, 10) + " +0000\n\nMerge pull request #" + strconv.Itoa(p.Number) + " from " + p.Head + "\n\n" + p.Title + "\n"
	newSHA := repo.putObject("commit", []byte(commitData))
	repo.Refs["refs/heads/"+p.Base] = newSHA
	return newSHA, false
}

// mergeBase returns the best common ancestor of a and b: a commit that is
// an ancestor of both and not an ancestor of any other common ancestor.
func mergeBase(repo *Repo, a, b string) string {
	ca := repo.commitClosure([]string{a})
	cb := repo.commitClosure([]string{b})
	var candidates []string
	for sha := range ca {
		if cb[sha] {
			candidates = append(candidates, sha)
		}
	}
	for _, c := range candidates {
		dominated := false
		for _, o := range candidates {
			if o == c {
				continue
			}
			if repo.commitClosure([]string{o})[c] {
				dominated = true
				break
			}
		}
		if !dominated {
			return c
		}
	}
	if len(candidates) > 0 {
		return candidates[0]
	}
	return ""
}

func isAncestor(repo *Repo, anc, tip string) bool {
	return repo.commitClosure([]string{tip})[anc]
}

func handleListPullReviews(w http.ResponseWriter, r *http.Request, repo *Repo, n int) {
	store.mu.RLock()
	defer store.mu.RUnlock()
	if _, ok := repo.Pulls[n]; !ok {
		writeErr(w, 404, "Not Found")
		return
	}
	writeJSON(w, 200, []any{})
}

func handleCreatePullReview(w http.ResponseWriter, r *http.Request, repo *Repo, n int) {
	m := readBodyJSON(r)
	store.mu.RLock()
	defer store.mu.RUnlock()
	p, ok := repo.Pulls[n]
	if !ok {
		writeErr(w, 404, "Not Found")
		return
	}
	writeJSON(w, 200, map[string]any{
		"id": numID("review:" + repo.FullName + ":" + strconv.Itoa(n) + ":" + strField(m, "event")),
		"node_id": "PRR_" + repo.FullName + "_" + strconv.Itoa(n),
		"user": userJSON(currentUser(r)),
		"body": strField(m, "body"),
		"state": map[string]string{"APPROVE": "APPROVED", "REQUEST_CHANGES": "CHANGES_REQUESTED", "COMMENT": "COMMENTED"}[strField(m, "event")],
		"submitted_at": ts(nowUTC()),
		"html_url": "https://github.com/" + repo.FullName + "/pull/" + strconv.Itoa(n),
		"pull_request_url": baseURL() + "/repos/" + repo.FullName + "/pulls/" + strconv.Itoa(n),
		"commit_id": p.HeadSHA,
	})
}

// ---------- tags / misc ----------

func handleListTags(w http.ResponseWriter, r *http.Request, repo *Repo) {
	store.mu.RLock()
	defer store.mu.RUnlock()
	var out []any
	for ref, sha := range repo.Refs {
		if !strings.HasPrefix(ref, "refs/tags/") {
			continue
		}
		name := strings.TrimPrefix(ref, "refs/tags/")
		typ := "commit"
		if o := repo.Objects[sha]; o != nil {
			typ = o.Type
		}
		out = append(out, map[string]any{
			"name": name, "commit": map[string]any{"sha": sha, "url": baseURL() + "/repos/" + repo.FullName + "/commits/" + sha},
			"zipball_url": baseURL() + "/repos/" + repo.FullName + "/zipball/" + name,
			"tarball_url": baseURL() + "/repos/" + repo.FullName + "/tarball/" + name,
			"node_id": "TAG_" + repo.FullName + "_" + name,
			"_type": typ,
		})
	}
	if out == nil {
		out = []any{}
	}
	pageLinks(w, r, len(out))
	writeJSON(w, 200, slicePage(out, r))
}

func handleListReleases(w http.ResponseWriter, r *http.Request, repo *Repo) {
	writeJSON(w, 200, []any{})
}

func handleGetLanguages(w http.ResponseWriter, r *http.Request, repo *Repo) {
	writeJSON(w, 200, map[string]any{"Go": 100})
}

func handleGetReadme(w http.ResponseWriter, r *http.Request, repo *Repo) {
	store.mu.RLock()
	defer store.mu.RUnlock()
	sha := repo.headSHA()
	if sha == "" {
		writeErr(w, 404, "Not Found")
		return
	}
	ci := parseCommit(repo.Objects[sha].Data)
	flat := flatTree(repo, ci.Tree)
	for _, cand := range []string{"README.md", "README", "readme.md"} {
		if e, ok := flat[cand]; ok {
			writeJSON(w, 200, contentFileJSON(repo, cand, e, repo.DefaultBranch))
			return
		}
	}
	writeErr(w, 404, "Not Found")
}

func handleCompare(w http.ResponseWriter, r *http.Request, repo *Repo, basehead string) {
	parts := strings.Split(basehead, "...")
	if len(parts) != 2 {
		writeErr(w, 404, "Not Found")
		return
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	baseSHA := resolveRef(repo, parts[0])
	headSHA := resolveRef(repo, parts[1])
	if baseSHA == "" || headSHA == "" {
		writeErr(w, 404, "Not Found")
		return
	}
	baseSet := repo.commitClosure([]string{baseSHA})
	var commits []any
	seen := map[string]bool{}
	queue := []string{headSHA}
	for len(queue) > 0 {
		c := queue[0]
		queue = queue[1:]
		if c == "" || seen[c] || baseSet[c] {
			continue
		}
		seen[c] = true
		cj := commitJSON(repo, c)
		if cj == nil {
			continue
		}
		commits = append(commits, cj)
		ci := parseCommit(repo.Objects[c].Data)
		queue = append(queue, ci.Parents...)
	}
	if commits == nil {
		commits = []any{}
	}
	writeJSON(w, 200, map[string]any{
		"url": baseURL() + "/repos/" + repo.FullName + "/compare/" + basehead,
		"html_url": "https://github.com/" + repo.FullName + "/compare/" + basehead,
		"base_commit": commitJSON(repo, baseSHA),
		"merge_base_commit": commitJSON(repo, baseSHA),
		"status": "ahead", "ahead_by": len(commits), "behind_by": 0,
		"total_commits": len(commits), "commits": commits, "files": []any{},
	})
}

// ---------- search ----------

func handleSearchRepos(w http.ResponseWriter, r *http.Request) {
	q := strings.ToLower(r.URL.Query().Get("q"))
	var items []any
	for _, repo := range store.ListRepos("") {
		if q == "" || strings.Contains(strings.ToLower(repo.FullName), q) || strings.Contains(strings.ToLower(repo.Description), q) {
			items = append(items, repoJSON(repo))
		}
	}
	if items == nil {
		items = []any{}
	}
	writeJSON(w, 200, map[string]any{"total_count": len(items), "incomplete_results": false, "items": slicePage(items, r)})
}

func handleSearchIssues(w http.ResponseWriter, r *http.Request) {
	q := strings.ToLower(r.URL.Query().Get("q"))
	var items []any
	for _, repo := range store.ListRepos("") {
		store.mu.RLock()
		for _, is := range repo.Issues {
			if q == "" || strings.Contains(strings.ToLower(is.Title), q) || strings.Contains(strings.ToLower(is.Body), q) {
				j := issueJSON(repo, is)
				j["pull_request"] = nil
				items = append(items, j)
			}
		}
		for _, p := range repo.Pulls {
			if q == "" || strings.Contains(strings.ToLower(p.Title), q) || strings.Contains(strings.ToLower(p.Body), q) {
				j := pullJSON(repo, p)
				items = append(items, j)
			}
		}
		store.mu.RUnlock()
	}
	if items == nil {
		items = []any{}
	}
	writeJSON(w, 200, map[string]any{"total_count": len(items), "incomplete_results": false, "items": slicePage(items, r)})
}

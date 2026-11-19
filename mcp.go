package main

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

type mcpTool struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	InputSchema any    `json:"inputSchema"`
}

func mcpKnownTool(name string) bool {
	for _, t := range mcpToolsList() {
		if t.Name == name {
			return true
		}
	}
	return false
}

func mcpToolsList() []mcpTool {
	str := map[string]any{"type": "string"}
	integer := map[string]any{"type": "integer"}
	boolean := map[string]any{"type": "boolean"}
	obj := func(props map[string]any, req []string) map[string]any {
		return map[string]any{"type": "object", "properties": props, "required": req}
	}
	return []mcpTool{
		{"list_repositories", "List repositories owned by a user", obj(map[string]any{"owner": str}, nil)},
		{"create_repository", "Create a new repository", obj(map[string]any{"name": str, "description": str, "private": boolean, "owner": str}, []string{"name"})},
		{"get_repository", "Get a repository by owner and name", obj(map[string]any{"owner": str, "repo": str, "name": str}, []string{"owner"})},
		{"delete_repository", "Delete a repository", obj(map[string]any{"owner": str, "repo": str, "name": str}, []string{"owner"})},
		{"list_issues", "List issues in a repository", obj(map[string]any{"owner": str, "repo": str, "name": str, "state": str}, []string{"owner"})},
		{"create_issue", "Create an issue in a repository", obj(map[string]any{"owner": str, "repo": str, "name": str, "title": str, "body": str, "labels": map[string]any{"type": "array", "items": str}}, []string{"owner", "title"})},
		{"get_issue", "Get an issue by number", obj(map[string]any{"owner": str, "repo": str, "name": str, "number": integer, "issue_number": integer}, []string{"owner", "number"})},
		{"update_issue", "Update an issue", obj(map[string]any{"owner": str, "repo": str, "name": str, "number": integer, "issue_number": integer, "title": str, "body": str, "state": str}, []string{"owner", "number"})},
		{"close_issue", "Close an issue", obj(map[string]any{"owner": str, "repo": str, "name": str, "number": integer, "issue_number": integer}, []string{"owner", "number"})},
		{"add_issue_comment", "Add a comment to an issue", obj(map[string]any{"owner": str, "repo": str, "name": str, "number": integer, "issue_number": integer, "body": str}, []string{"owner", "number", "body"})},
		{"list_pull_requests", "List pull requests in a repository", obj(map[string]any{"owner": str, "repo": str, "name": str, "state": str}, []string{"owner"})},
		{"create_pull_request", "Create a pull request", obj(map[string]any{"owner": str, "repo": str, "name": str, "title": str, "body": str, "head": str, "base": str}, []string{"owner", "title", "head", "base"})},
		{"get_pull_request", "Get a pull request by number", obj(map[string]any{"owner": str, "repo": str, "name": str, "number": integer, "pull_number": integer}, []string{"owner", "number"})},
		{"merge_pull_request", "Merge a pull request", obj(map[string]any{"owner": str, "repo": str, "name": str, "number": integer, "pull_number": integer}, []string{"owner"})},
		{"list_commits", "List commits on a branch", obj(map[string]any{"owner": str, "repo": str, "name": str, "sha": str, "branch": str}, []string{"owner"})},
		{"get_commit", "Get a commit by SHA", obj(map[string]any{"owner": str, "repo": str, "name": str, "sha": str, "ref": str}, []string{"owner", "sha"})},
		{"list_branches", "List branches in a repository", obj(map[string]any{"owner": str, "repo": str, "name": str}, []string{"owner"})},
		{"get_file_contents", "Get file contents at a path", obj(map[string]any{"owner": str, "repo": str, "name": str, "path": str, "ref": str}, []string{"owner", "path"})},
		{"create_file", "Create a file in a repository", obj(map[string]any{"owner": str, "repo": str, "name": str, "path": str, "content": str, "message": str, "branch": str}, []string{"owner", "path", "content"})},
		{"search_repositories", "Search repositories", obj(map[string]any{"query": str, "q": str}, []string{"query"})},
		{"search_issues", "Search issues and pull requests", obj(map[string]any{"query": str, "q": str}, []string{"query"})},
	}
}

func mcpArgStr(args map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := args[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

func mcpArgInt(args map[string]any, keys ...string) int {
	for _, k := range keys {
		switch v := args[k].(type) {
		case float64:
			return int(v)
		case int:
			return v
		case json.Number:
			n, _ := v.Int64()
			return int(n)
		}
	}
	return 0
}

func mcpResolveRepo(args map[string]any, user string) (*Repo, string) {
	owner := mcpArgStr(args, "owner", "user", "login")
	repoName := mcpArgStr(args, "repo", "name", "repository", "repository_name")
	if repoName == "" {
		if full := mcpArgStr(args, "full_name", "fullName"); full != "" {
			parts := strings.SplitN(full, "/", 2)
			if len(parts) == 2 {
				owner, repoName = parts[0], parts[1]
			}
		}
	}
	if owner == "" {
		owner = user
	}
	if repoName == "" {
		return nil, "repo is required"
	}
	r := store.GetRepo(owner, repoName)
	if r == nil {
		return nil, "Repository not found"
	}
	return r, ""
}

func mcpCall(name string, args map[string]any, user string) (any, string) {
	switch name {
	case "list_repositories":
		owner := mcpArgStr(args, "owner", "user", "login")
		if owner == "" {
			owner = user
		}
		var out []any
		for _, r := range store.ListRepos(owner) {
			out = append(out, repoJSON(r))
		}
		if out == nil {
			out = []any{}
		}
		return out, ""
	case "create_repository":
		owner := mcpArgStr(args, "owner", "user", "login")
		if owner == "" {
			owner = user
		}
		repoName := mcpArgStr(args, "name", "repo")
		if repoName == "" {
			return nil, "name is required"
		}
		repo, ok := store.CreateRepo(owner, repoName, mcpArgStr(args, "description"), false)
		if bv, ok2 := args["private"].(bool); ok2 && ok {
			repo.Private = bv
		}
		if !ok {
			return nil, "Repository already exists"
		}
		return repoJSON(repo), ""
	case "get_repository":
		r, errStr := mcpResolveRepo(args, user)
		if errStr != "" {
			return nil, errStr
		}
		return repoJSON(r), ""
	case "delete_repository":
		r, errStr := mcpResolveRepo(args, user)
		if errStr != "" {
			return nil, errStr
		}
		store.DeleteRepo(r.Owner, r.Name)
		return map[string]any{"deleted": true}, ""
	case "list_issues":
		r, errStr := mcpResolveRepo(args, user)
		if errStr != "" {
			return nil, errStr
		}
		state := mcpArgStr(args, "state")
		store.mu.RLock()
		defer store.mu.RUnlock()
		var out []any
		nums := make([]int, 0, len(r.Issues))
		for n := range r.Issues {
			nums = append(nums, n)
		}
		sort.Ints(nums)
		for _, n := range nums {
			is := r.Issues[n]
			if state != "" && state != "all" && is.State != state {
				continue
			}
			out = append(out, issueJSON(r, is))
		}
		if out == nil {
			out = []any{}
		}
		return out, ""
	case "create_issue":
		r, errStr := mcpResolveRepo(args, user)
		if errStr != "" {
			return nil, errStr
		}
		title := mcpArgStr(args, "title")
		if title == "" {
			return nil, "title is required"
		}
		store.mu.Lock()
		n := r.NextIssue
		r.NextIssue++
		now := nowUTC()
		is := &Issue{Number: n, Title: title, Body: mcpArgStr(args, "body"), State: "open", User: user, CreatedAt: now, UpdatedAt: now, Labels: []string{}, Assignees: []string{}}
		if labs, ok := args["labels"].([]any); ok {
			for _, l := range labs {
				if s, ok := l.(string); ok {
					is.Labels = append(is.Labels, s)
				}
			}
		}
		r.Issues[n] = is
		store.mu.Unlock()
		return issueJSON(r, is), ""
	case "get_issue":
		r, errStr := mcpResolveRepo(args, user)
		if errStr != "" {
			return nil, errStr
		}
		n := mcpArgInt(args, "number", "issue_number")
		store.mu.RLock()
		defer store.mu.RUnlock()
		is, ok := r.Issues[n]
		if !ok {
			return nil, "Issue not found"
		}
		return issueJSON(r, is), ""
	case "update_issue", "close_issue":
		r, errStr := mcpResolveRepo(args, user)
		if errStr != "" {
			return nil, errStr
		}
		n := mcpArgInt(args, "number", "issue_number")
		store.mu.Lock()
		is, ok := r.Issues[n]
		if !ok {
			store.mu.Unlock()
			return nil, "Issue not found"
		}
		if v := mcpArgStr(args, "title"); v != "" {
			is.Title = v
		}
		if v, has := args["body"]; has {
			if s, ok2 := v.(string); ok2 {
				is.Body = s
			}
		}
		if name == "close_issue" {
			is.State = "closed"
			now := nowUTC()
			is.ClosedAt = &now
		} else if v := mcpArgStr(args, "state"); v == "open" || v == "closed" {
			is.State = v
			if v == "closed" {
				now := nowUTC()
				is.ClosedAt = &now
			} else {
				is.ClosedAt = nil
			}
		}
		is.UpdatedAt = nowUTC()
		store.mu.Unlock()
		return issueJSON(r, is), ""
	case "add_issue_comment":
		r, errStr := mcpResolveRepo(args, user)
		if errStr != "" {
			return nil, errStr
		}
		n := mcpArgInt(args, "number", "issue_number")
		body := mcpArgStr(args, "body", "comment")
		if body == "" {
			return nil, "body is required"
		}
		store.mu.Lock()
		is, ok := r.Issues[n]
		if !ok {
			store.mu.Unlock()
			return nil, "Issue not found"
		}
		id := r.NextComment
		r.NextComment++
		now := nowUTC()
		c := &Comment{ID: id, Body: body, User: user, CreatedAt: now, UpdatedAt: now}
		is.Comments = append(is.Comments, c)
		is.UpdatedAt = now
		store.mu.Unlock()
		return commentJSON(r, c, "issues", n), ""
	case "list_pull_requests":
		r, errStr := mcpResolveRepo(args, user)
		if errStr != "" {
			return nil, errStr
		}
		state := mcpArgStr(args, "state")
		store.mu.RLock()
		defer store.mu.RUnlock()
		var out []any
		nums := make([]int, 0, len(r.Pulls))
		for n := range r.Pulls {
			nums = append(nums, n)
		}
		sort.Ints(nums)
		for _, n := range nums {
			p := r.Pulls[n]
			if state != "" && state != "all" {
				if state == "open" && p.State != "open" {
					continue
				}
				if state == "closed" && p.State == "open" {
					continue
				}
			}
			out = append(out, pullJSON(r, p))
		}
		if out == nil {
			out = []any{}
		}
		return out, ""
	case "create_pull_request":
		r, errStr := mcpResolveRepo(args, user)
		if errStr != "" {
			return nil, errStr
		}
		title := mcpArgStr(args, "title")
		head := mcpArgStr(args, "head", "headRefName")
		base := mcpArgStr(args, "base", "baseRefName")
		if title == "" || head == "" || base == "" {
			return nil, "title, head and base are required"
		}
		if i := strings.Index(head, ":"); i >= 0 {
			head = head[i+1:]
		}
		if i := strings.Index(base, ":"); i >= 0 {
			base = base[i+1:]
		}
		store.mu.Lock()
		headSHA := r.Refs["refs/heads/"+head]
		baseSHA := r.Refs["refs/heads/"+base]
		if headSHA == "" || baseSHA == "" {
			store.mu.Unlock()
			return nil, "Head or base branch does not exist"
		}
		n := r.NextPull
		r.NextPull++
		now := nowUTC()
		p := &Pull{Number: n, Title: title, Body: mcpArgStr(args, "body"), State: "open", Head: head, Base: base, HeadSHA: headSHA, BaseSHA: baseSHA, Mergeable: true, User: user, CreatedAt: now, UpdatedAt: now}
		r.Pulls[n] = p
		store.mu.Unlock()
		return pullJSON(r, p), ""
	case "get_pull_request":
		r, errStr := mcpResolveRepo(args, user)
		if errStr != "" {
			return nil, errStr
		}
		n := mcpArgInt(args, "number", "pull_number")
		store.mu.RLock()
		defer store.mu.RUnlock()
		p, ok := r.Pulls[n]
		if !ok {
			return nil, "Pull request not found"
		}
		return pullJSON(r, p), ""
	case "merge_pull_request":
		r, errStr := mcpResolveRepo(args, user)
		if errStr != "" {
			return nil, errStr
		}
		n := mcpArgInt(args, "number", "pull_number")
		store.mu.Lock()
		p, ok := r.Pulls[n]
		if !ok {
			store.mu.Unlock()
			return nil, "Pull request not found"
		}
		if p.Merged || p.State != "open" {
			store.mu.Unlock()
			return nil, "Pull request already merged or closed"
		}
		author := user + " <" + user + "@users.noreply.github.com>"
		newSHA, conflict := doMergePull(r, p, author)
		if conflict {
			store.mu.Unlock()
			return nil, "Merge conflict"
		}
		p.Merged = true
		p.MergeSHA = newSHA
		p.State = "closed"
		nowT := nowUTC()
		p.MergedAt = &nowT
		p.ClosedAt = &nowT
		p.UpdatedAt = nowT
		store.mu.Unlock()
		return map[string]any{"sha": newSHA, "merged": true, "message": "Pull Request successfully merged"}, ""
	case "list_commits":
		r, errStr := mcpResolveRepo(args, user)
		if errStr != "" {
			return nil, errStr
		}
		store.mu.RLock()
		defer store.mu.RUnlock()
		sha := r.headSHA()
		if v := mcpArgStr(args, "sha", "branch", "ref"); v != "" {
			if s := resolveRef(r, v); s != "" {
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
			cj := commitJSON(r, c)
			if cj == nil {
				continue
			}
			out = append(out, cj)
			ci := parseCommit(r.Objects[c].Data)
			queue = append(queue, ci.Parents...)
		}
		if out == nil {
			out = []any{}
		}
		return out, ""
	case "get_commit":
		r, errStr := mcpResolveRepo(args, user)
		if errStr != "" {
			return nil, errStr
		}
		sha := mcpArgStr(args, "sha", "ref")
		store.mu.RLock()
		defer store.mu.RUnlock()
		if s := resolveRef(r, sha); s != "" {
			sha = s
		}
		cj := commitJSON(r, sha)
		if cj == nil {
			return nil, "Commit not found"
		}
		return cj, ""
	case "list_branches":
		r, errStr := mcpResolveRepo(args, user)
		if errStr != "" {
			return nil, errStr
		}
		store.mu.RLock()
		defer store.mu.RUnlock()
		var out []any
		for ref := range r.Refs {
			if strings.HasPrefix(ref, "refs/heads/") {
				out = append(out, branchJSON(r, strings.TrimPrefix(ref, "refs/heads/")))
			}
		}
		if out == nil {
			out = []any{}
		}
		return out, ""
	case "get_file_contents":
		r, errStr := mcpResolveRepo(args, user)
		if errStr != "" {
			return nil, errStr
		}
		path := mcpArgStr(args, "path")
		branch := mcpArgStr(args, "ref", "branch")
		if branch == "" {
			branch = r.DefaultBranch
		}
		store.mu.RLock()
		defer store.mu.RUnlock()
		sha := r.Refs["refs/heads/"+branch]
		if sha == "" {
			return nil, "Branch not found"
		}
		ci := parseCommit(r.Objects[sha].Data)
		flat := flatTree(r, ci.Tree)
		e, ok := flat[path]
		if !ok {
			return nil, "File not found"
		}
		return contentFileJSON(r, path, e, branch), ""
	case "create_file":
		r, errStr := mcpResolveRepo(args, user)
		if errStr != "" {
			return nil, errStr
		}
		path := mcpArgStr(args, "path")
		content := mcpArgStr(args, "content")
		msg := mcpArgStr(args, "message")
		if msg == "" {
			msg = "Create " + path
		}
		branch := mcpArgStr(args, "branch")
		if branch == "" {
			branch = r.DefaultBranch
		}
		store.mu.Lock()
		head := r.Refs["refs/heads/"+branch]
		var flat map[string]TreeEntry
		parent := ""
		if head == "" {
			// Empty repo (or brand-new branch): the first file creates
			// the initial commit instead of failing.
			flat = map[string]TreeEntry{}
		} else {
			ci := parseCommit(r.Objects[head].Data)
			flat = flatTree(r, ci.Tree)
			parent = head
		}
		blobSHA := r.putObject("blob", []byte(content))
		flat[path] = TreeEntry{Mode: "100644", Name: path, SHA: blobSHA, Type: "blob"}
		newTree := buildTreeFromFlat(r, flat)
		now := time.Now().Unix()
		author := user + " <" + user + "@users.noreply.github.com>"
		commitData := "tree " + newTree + "\n"
		if parent != "" {
			commitData += "parent " + parent + "\n"
		}
		commitData += "author " + author + " " + strconv.FormatInt(now, 10) + " +0000\ncommitter " + author + " " + strconv.FormatInt(now, 10) + " +0000\n\n" + msg + "\n"
		newCommit := r.putObject("commit", []byte(commitData))
		r.Refs["refs/heads/"+branch] = newCommit
		r.UpdatedAt = nowUTC()
		store.mu.Unlock()
		return map[string]any{"content": contentFileJSON(r, path, TreeEntry{Mode: "100644", SHA: blobSHA}, branch), "commit": commitJSON(r, newCommit)}, ""
	case "search_repositories":
		q := strings.ToLower(mcpArgStr(args, "query", "q"))
		var items []any
		for _, repo := range store.ListRepos("") {
			if q == "" || strings.Contains(strings.ToLower(repo.FullName), q) || strings.Contains(strings.ToLower(repo.Description), q) {
				items = append(items, repoJSON(repo))
			}
		}
		if items == nil {
			items = []any{}
		}
		return map[string]any{"total_count": len(items), "items": items}, ""
	case "search_issues":
		q := strings.ToLower(mcpArgStr(args, "query", "q"))
		var items []any
		for _, repo := range store.ListRepos("") {
			store.mu.RLock()
			for _, is := range repo.Issues {
				if q == "" || strings.Contains(strings.ToLower(is.Title), q) || strings.Contains(strings.ToLower(is.Body), q) {
					items = append(items, issueJSON(repo, is))
				}
			}
			for _, p := range repo.Pulls {
				if q == "" || strings.Contains(strings.ToLower(p.Title), q) || strings.Contains(strings.ToLower(p.Body), q) {
					items = append(items, pullJSON(repo, p))
				}
			}
			store.mu.RUnlock()
		}
		if items == nil {
			items = []any{}
		}
		return map[string]any{"total_count": len(items), "items": items}, ""
	}
	return nil, "Unknown tool: " + name
}

func handleMCP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		JSONRPC string         `json:"jsonrpc"`
		ID      any            `json:"id"`
		Method  string         `json:"method"`
		Params  map[string]any `json:"params"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]any{"jsonrpc": "2.0", "id": nil, "error": map[string]any{"code": -32700, "message": "Parse error"}})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	resp := map[string]any{"jsonrpc": "2.0", "id": req.ID}
	switch req.Method {
	case "initialize":
		resp["result"] = map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "anvil-go", "version": "1.0.0"},
		}
	case "notifications/initialized", "notifications/cancelled":
		// notification: no response body
		w.WriteHeader(202)
		return
	case "ping":
		resp["result"] = map[string]any{}
	case "tools/list":
		tools := mcpToolsList()
		var arr []any
		for _, t := range tools {
			arr = append(arr, map[string]any{"name": t.Name, "description": t.Description, "inputSchema": t.InputSchema})
		}
		resp["result"] = map[string]any{"tools": arr}
	case "tools/call":
		user := currentUser(r)
		if user == "" {
			w.WriteHeader(401)
			enc := json.NewEncoder(w)
			enc.SetEscapeHTML(false)
			enc.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"code": -32000, "message": "Requires authentication"}})
			return
		}
		name, _ := req.Params["name"].(string)
		args, _ := req.Params["arguments"].(map[string]any)
		if args == nil {
			args = map[string]any{}
		}
		if !mcpKnownTool(name) {
			resp["error"] = map[string]any{"code": -32602, "message": "Unknown tool: " + name}
			break
		}
		result, errStr := mcpCall(name, args, user)
		if errStr != "" {
			resp["result"] = map[string]any{
				"content": []any{map[string]any{"type": "text", "text": "Error: " + errStr}},
				"isError": true,
			}
		} else {
			b, _ := json.Marshal(result)
			resp["result"] = map[string]any{
				"content": []any{map[string]any{"type": "text", "text": string(b)}},
			}
		}
	case "resources/list":
		resp["result"] = map[string]any{"resources": []any{}}
	case "prompts/list":
		resp["result"] = map[string]any{"prompts": []any{}}
	default:
		resp["error"] = map[string]any{"code": -32601, "message": "Method not found: " + req.Method}
	}
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.Encode(resp)
}

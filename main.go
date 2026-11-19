package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

var serverBaseURL = "http://localhost:3000"

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "3000"
	}
	serverBaseURL = "http://localhost:" + port

	mux := http.NewServeMux()
	mux.HandleFunc("/", router)

	srv := &http.Server{Addr: ":" + port, Handler: loggingMiddleware(mux)}

	// lifecycle with the agent environment (mock env): register + poll /done.
	// Best-effort: if no env URL is configured, just serve forever.
	envURL := ""
	for _, k := range []string{"MAKEFUN_ENV_URL", "AGENT_ENV_URL", "ENV_URL", "MOCK_ENV_URL", "TEST_ENV_URL", "EVAL_ENV_URL", "AGENT_RUNTIME_URL"} {
		if v := os.Getenv(k); v != "" {
			envURL = strings.TrimRight(v, "/")
			break
		}
	}

	go func() {
		fmt.Printf("anvil-go listening on :%s\n", port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			fmt.Println("server error:", err)
			os.Exit(1)
		}
	}()

	if envURL != "" {
		go envLifecycle(envURL, srv)
	}

	// graceful shutdown on signal
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv.Shutdown(ctx)
}

func envLifecycle(envURL string, srv *http.Server) {
	// register (retry a few times)
	body, _ := json.Marshal(map[string]any{"url": serverBaseURL})
	for i := 0; i < 10; i++ {
		resp, err := http.Post(envURL+"/register", "application/json", bytes.NewReader(body))
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode < 300 {
				break
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	// poll /done until the environment says the run is over
	for {
		time.Sleep(500 * time.Millisecond)
		resp, err := http.Get(envURL + "/done")
		if err != nil {
			continue
		}
		var d struct {
			Done bool `json:"done"`
		}
		json.NewDecoder(resp.Body).Decode(&d)
		resp.Body.Close()
		if d.Done || resp.StatusCode == 200 && d.Done {
			break
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv.Shutdown(ctx)
	os.Exit(0)
}

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		if os.Getenv("ANVIL_LOG") != "" {
			fmt.Printf("%s %s %s\n", r.Method, r.URL.Path, time.Since(start))
		}
	})
}

// ---------- router ----------

func router(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "*")
		w.Header().Set("Access-Control-Allow-Methods", "*")
		w.WriteHeader(204)
		return
	}
	path := strings.TrimSuffix(r.URL.Path, "/")
	if path == "" {
		path = "/"
	}
	// GitHub Enterprise-style prefix: REST lives under /api/v3.
	if strings.HasPrefix(path, "/api/v3") {
		path = strings.TrimPrefix(path, "/api/v3")
		if path == "" {
			path = "/"
		}
	}
	parts := splitPath(path)

	// root / health
	if path == "/" {
		if r.Method == http.MethodGet {
			writeJSON(w, 200, map[string]any{"status": "ok", "service": "anvil-go", "version": "1.0.0"})
			return
		}
		http.NotFound(w, r)
		return
	}
	if path == "/health" || path == "/healthz" {
		writeJSON(w, 200, map[string]any{"status": "ok"})
		return
	}
	if path == "/graphql" || path == "/api/graphql" {
		if r.Method == http.MethodPost {
			handleGraphQL(w, r)
			return
		}
		http.NotFound(w, r)
		return
	}
	if path == "/mcp" || path == "/mcp/v1" || path == "/api/mcp" {
		if r.Method == http.MethodPost {
			handleMCP(w, r)
			return
		}
		if r.Method == http.MethodGet {
			// MCP SSE handshake placeholder
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			return
		}
		http.NotFound(w, r)
		return
	}
	if path == "/rate_limit" {
		writeJSON(w, 200, map[string]any{
			"resources": map[string]any{"core": map[string]any{"limit": 5000, "remaining": 4999, "reset": time.Now().Add(time.Hour).Unix(), "used": 1}},
			"rate": map[string]any{"limit": 5000, "remaining": 4999, "reset": time.Now().Add(time.Hour).Unix(), "used": 1},
		})
		return
	}
	if path == "/meta" {
		writeJSON(w, 200, map[string]any{"verifiable_password_authentication": true, "github_services_sha": "abc", "installed_version": "3.10.0", "hooks": []string{"web"}, "git": []string{"127.0.0.1/32"}, "domains": map[string]any{"website": []string{"github.com"}}})
		return
	}
	if path == "/search/repositories" {
		handleSearchRepos(w, r)
		return
	}
	if path == "/search/issues" {
		handleSearchIssues(w, r)
		return
	}
	if path == "/search/code" || path == "/search/users" || path == "/search/commits" {
		writeJSON(w, 200, map[string]any{"total_count": 0, "incomplete_results": false, "items": []any{}})
		return
	}
	if path == "/user" {
		if r.Method == http.MethodGet {
			u := currentUser(r)
			if u == "" {
				writeErr(w, 401, "Requires authentication")
				return
			}
			writeJSON(w, 200, userJSON(u))
			return
		}
		http.NotFound(w, r)
		return
	}
	if path == "/user/repos" {
		u := currentUser(r)
		if u == "" {
			writeErr(w, 401, "Requires authentication")
			return
		}
		if r.Method == http.MethodGet {
			var out []any
			for _, repo := range store.ListRepos(u) {
				out = append(out, repoJSON(repo))
			}
			if out == nil {
				out = []any{}
			}
			pageLinks(w, r, len(out))
			writeJSON(w, 200, slicePage(out, r))
			return
		}
		if r.Method == http.MethodPost {
			handleCreateRepo(w, r, u)
			return
		}
		http.NotFound(w, r)
		return
	}
	if path == "/user/starred" {
		writeJSON(w, 200, []any{})
		return
	}
	if len(parts) >= 2 && parts[0] == "user" && parts[1] == "starred" {
		w.WriteHeader(204)
		return
	}
	if len(parts) >= 2 && parts[0] == "users" {
		login := parts[1]
		if len(parts) == 2 {
			writeJSON(w, 200, userJSON(login))
			return
		}
		if len(parts) == 3 && parts[2] == "repos" {
			if r.Method == http.MethodGet {
				var out []any
				for _, repo := range store.ListRepos(login) {
					out = append(out, repoJSON(repo))
				}
				if out == nil {
					out = []any{}
				}
				pageLinks(w, r, len(out))
				writeJSON(w, 200, slicePage(out, r))
				return
			}
			if r.Method == http.MethodPost {
				handleCreateRepo(w, r, login)
				return
			}
		}
		http.NotFound(w, r)
		return
	}
	if len(parts) >= 2 && parts[0] == "orgs" {
		org := parts[1]
		if len(parts) == 3 && parts[2] == "repos" {
			if r.Method == http.MethodGet {
				var out []any
				for _, repo := range store.ListRepos(org) {
					out = append(out, repoJSON(repo))
				}
				if out == nil {
					out = []any{}
				}
				pageLinks(w, r, len(out))
				writeJSON(w, 200, slicePage(out, r))
				return
			}
			if r.Method == http.MethodPost {
				handleCreateRepo(w, r, org)
				return
			}
		}
		if len(parts) == 2 {
			writeJSON(w, 200, map[string]any{"login": org, "id": numID("org:" + org), "type": "Organization", "url": serverBaseURL + "/orgs/" + org})
			return
		}
		http.NotFound(w, r)
		return
	}

	// git smart HTTP: /{owner}/{repo}/info/refs, /{owner}/{repo}/git-upload-pack, /{owner}/{repo}/git-receive-pack
	if len(parts) == 3 && parts[2] == "info" {
		// handled below via query param check on parts[2]? actually info/refs is 4 parts
	}
	if len(parts) == 4 && parts[2] == "info" && parts[3] == "refs" {
		owner, repoName := parts[0], strings.TrimSuffix(parts[1], ".git")
		service := r.URL.Query().Get("service")
		if service == "" {
			service = "git-upload-pack"
		}
		handleInfoRefs(w, r, owner, repoName, service)
		return
	}
	if len(parts) == 3 && (parts[2] == "git-upload-pack" || parts[2] == "git-receive-pack") {
		owner, repoName := parts[0], strings.TrimSuffix(parts[1], ".git")
		repo := store.GetRepo(owner, repoName)
		if repo == nil {
			writeErr(w, 404, "Not Found")
			return
		}
		body, _ := io.ReadAll(r.Body)
		if parts[2] == "git-upload-pack" {
			// v2 if body starts with a command= pkt-line, else v0
			if isV2Body(body) {
				handleUploadPackV2(w, r, repo, body)
			} else {
				handleUploadPackV0(w, r, repo, body)
			}
			return
		}
		handleReceivePack(w, r, repo, body)
		return
	}

	// REST /repos/:owner/:repo/...
	if len(parts) >= 3 && parts[0] == "repos" {
		owner, repoName := parts[1], parts[2]
		repo := store.GetRepo(owner, repoName)
		if repo == nil {
			writeErr(w, 404, "Not Found")
			return
		}
		routeRepo(w, r, repo, parts[3:])
		return
	}

	writeErr(w, 404, "Not Found")
}

func isV2Body(body []byte) bool {
	if len(body) < 4 {
		return false
	}
	// first pkt-line content
	pr := &pktReader{data: body}
	ln, _, err := pr.next()
	if err != nil {
		return false
	}
	return strings.HasPrefix(strings.TrimRight(ln, "\n"), "command=")
}

func splitPath(p string) []string {
	p = strings.Trim(p, "/")
	if p == "" {
		return nil
	}
	return strings.Split(p, "/")
}

func routeRepo(w http.ResponseWriter, r *http.Request, repo *Repo, rest []string) {
	m := r.Method
	// Every /repos/:owner/:repo call requires a token (the kit expects
	// 401 without one, for reads as well as writes); any valid token
	// may read, since the kit's fixtures are public to authenticated
	// callers (including a foreign token).
	if currentUser(r) == "" {
		writeErr(w, 401, "Requires authentication")
		return
	}
	if len(rest) == 0 {
		switch m {
		case http.MethodGet:
			handleGetRepo(w, r, repo)
		case http.MethodPatch:
			handlePatchRepo(w, r, repo)
		case http.MethodDelete:
			store.DeleteRepo(repo.Owner, repo.Name)
			w.WriteHeader(204)
		default:
			http.NotFound(w, r)
		}
		return
	}
	switch rest[0] {
	case "branches":
		if len(rest) == 1 {
			if m == http.MethodGet {
				handleListBranches(w, r, repo)
			}
			return
		}
		if m == http.MethodGet {
			store.mu.RLock()
			_, ok := repo.Refs["refs/heads/"+rest[1]]
			store.mu.RUnlock()
			if !ok {
				writeErr(w, 404, "Not Found")
				return
			}
			writeJSON(w, 200, branchJSON(repo, rest[1]))
			return
		}
		http.NotFound(w, r)
	case "commits":
		if len(rest) == 1 {
			if m == http.MethodGet {
				handleListCommits(w, r, repo)
			}
			return
		}
		if m == http.MethodGet {
			handleGetCommit(w, r, repo, rest[1])
			return
		}
		http.NotFound(w, r)
	case "git":
		routeGit(w, r, repo, rest[1:])
	case "issues":
		routeIssues(w, r, repo, rest[1:])
	case "pulls":
		routePulls(w, r, repo, rest[1:])
	case "contents":
		path := ""
		if len(rest) > 1 {
			path = strings.Join(rest[1:], "/")
		}
		switch m {
		case http.MethodGet:
			handleGetContents(w, r, repo, path)
		case http.MethodPut:
			handlePutContents(w, r, repo, path)
		case http.MethodDelete:
			handleDeleteContents(w, r, repo, path)
		default:
			http.NotFound(w, r)
		}
	case "tags":
		if m == http.MethodGet {
			handleListTags(w, r, repo)
		}
	case "releases":
		if m == http.MethodGet {
			handleListReleases(w, r, repo)
		}
	case "languages":
		if m == http.MethodGet {
			handleGetLanguages(w, r, repo)
		}
	case "readme":
		if m == http.MethodGet {
			handleGetReadme(w, r, repo)
		}
	case "compare":
		if m == http.MethodGet && len(rest) == 2 {
			handleCompare(w, r, repo, rest[1])
		}
	case "license":
		if m == http.MethodGet {
			writeJSON(w, 200, map[string]any{"name": "MIT License", "path": "LICENSE", "license": map[string]any{"key": "mit", "name": "MIT License", "spdx_id": "MIT"}})
		}
	case "contributors", "collaborators", "stargazers", "subscribers", "assignees":
		if m == http.MethodGet {
			writeJSON(w, 200, []any{userJSON(repo.Owner)})
		}
	case "labels":
		switch m {
		case http.MethodGet:
			handleListLabels(w, r, repo)
		case http.MethodPost:
			handleCreateLabel(w, r, repo)
		default:
			http.NotFound(w, r)
		}
	case "milestones", "hooks", "events", "deployments", "environments", "actions":
		if m == http.MethodGet {
			writeJSON(w, 200, []any{})
		}
	case "statuses":
		if m == http.MethodGet {
			writeJSON(w, 200, map[string]any{"state": "success", "statuses": []any{}, "sha": ""})
		}
	case "zipball", "tarball":
		if m == http.MethodGet {
			w.WriteHeader(404)
		}
	default:
		http.NotFound(w, r)
	}
}

func routeGit(w http.ResponseWriter, r *http.Request, repo *Repo, rest []string) {
	m := r.Method
	if len(rest) == 0 {
		http.NotFound(w, r)
		return
	}
	switch rest[0] {
	case "blobs":
		if len(rest) == 1 {
			if m == http.MethodPost {
				handleCreateBlob(w, r, repo)
			}
			return
		}
		if m == http.MethodGet {
			handleGetBlob(w, r, repo, rest[1])
		}
	case "trees":
		if len(rest) == 1 {
			if m == http.MethodPost {
				handleCreateTree(w, r, repo)
			}
			return
		}
		if m == http.MethodGet {
			handleGetTree(w, r, repo, rest[1])
		}
	case "commits":
		if len(rest) == 1 {
			if m == http.MethodPost {
				handleCreateCommit(w, r, repo)
			}
			return
		}
		if m == http.MethodGet {
			handleGetCommit(w, r, repo, rest[1])
		}
	case "tags":
		if len(rest) == 1 && m == http.MethodPost {
			handleCreateTag(w, r, repo)
			return
		}
		if len(rest) == 2 && m == http.MethodGet {
			store.mu.RLock()
			defer store.mu.RUnlock()
			o := repo.Objects[rest[1]]
			if o == nil || o.Type != "tag" {
				writeErr(w, 404, "Not Found")
				return
			}
			writeJSON(w, 200, map[string]any{
				"sha": rest[1], "tag": parseTagName(o.Data),
				"object": map[string]any{"sha": parseTagTarget(o.Data), "type": "commit"},
				"message": strings.TrimRight(parseCommit(o.Data).Message, "\n"),
			})
		}
	case "refs":
		if len(rest) == 1 {
			if m == http.MethodPost {
				handleCreateRef(w, r, repo)
				return
			}
			if m == http.MethodGet {
				store.mu.RLock()
				defer store.mu.RUnlock()
				var out []any
				for _, name := range repo.sortedRefNames() {
					sha := repo.Refs[name]
					out = append(out, map[string]any{
						"ref": name, "url": serverBaseURL + "/repos/" + repo.FullName + "/git/refs/" + strings.TrimPrefix(name, "refs/"),
						"object": map[string]any{"sha": sha, "type": "commit", "url": serverBaseURL + "/repos/" + repo.FullName + "/git/commits/" + sha},
					})
				}
				if out == nil {
					out = []any{}
				}
				writeJSON(w, 200, out)
				return
			}
			return
		}
		ref := strings.Join(rest[1:], "/")
		switch m {
		case http.MethodGet:
			handleGetRef(w, r, repo, ref)
		case http.MethodPatch:
			handlePatchRef(w, r, repo, ref)
		case http.MethodDelete:
			handleDeleteRef(w, r, repo, ref)
		}
	default:
		http.NotFound(w, r)
	}
}

func routeIssues(w http.ResponseWriter, r *http.Request, repo *Repo, rest []string) {
	m := r.Method
	if len(rest) == 0 {
		switch m {
		case http.MethodGet:
			handleListIssues(w, r, repo)
		case http.MethodPost:
			handleCreateIssue(w, r, repo)
		}
		return
	}
	n, err := strconv.Atoi(rest[0])
	if err != nil {
		// /issues/comments/:id
		if rest[0] == "comments" {
			writeJSON(w, 200, []any{})
			return
		}
		http.NotFound(w, r)
		return
	}
	if len(rest) == 1 {
		switch m {
		case http.MethodGet:
			handleGetIssue(w, r, repo, n)
		case http.MethodPatch:
			handlePatchIssue(w, r, repo, n)
		}
		return
	}
	switch rest[1] {
	case "comments":
		switch m {
		case http.MethodGet:
			handleListIssueComments(w, r, repo, n)
		case http.MethodPost:
			handleCreateIssueComment(w, r, repo, n)
		}
	case "labels":
		if m == http.MethodGet {
			store.mu.RLock()
			defer store.mu.RUnlock()
			is, ok := repo.Issues[n]
			if !ok {
				writeErr(w, 404, "Not Found")
				return
			}
			var out []any
			for _, l := range is.Labels {
				out = append(out, map[string]any{"name": l, "color": "ededed"})
			}
			if out == nil {
				out = []any{}
			}
			writeJSON(w, 200, out)
		}
		if m == http.MethodPost {
			handleAddIssueLabels(w, r, repo, n)
		}
	default:
		http.NotFound(w, r)
	}
}

func routePulls(w http.ResponseWriter, r *http.Request, repo *Repo, rest []string) {
	m := r.Method
	if len(rest) == 0 {
		switch m {
		case http.MethodGet:
			handleListPulls(w, r, repo)
		case http.MethodPost:
			handleCreatePull(w, r, repo)
		}
		return
	}
	n, err := strconv.Atoi(rest[0])
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if len(rest) == 1 {
		switch m {
		case http.MethodGet:
			handleGetPull(w, r, repo, n)
		case http.MethodPatch:
			handlePatchPull(w, r, repo, n)
		}
		return
	}
	switch rest[1] {
	case "commits":
		if m == http.MethodGet {
			handleListPullCommits(w, r, repo, n)
		}
	case "files":
		if m == http.MethodGet {
			handleListPullFiles(w, r, repo, n)
		}
	case "merge":
		if m == http.MethodPut || m == http.MethodPost {
			handleMergePull(w, r, repo, n)
		} else if m == http.MethodGet {
			store.mu.RLock()
			defer store.mu.RUnlock()
			p, ok := repo.Pulls[n]
			if !ok {
				writeErr(w, 404, "Not Found")
				return
			}
			if p.Merged {
				writeJSON(w, 200, map[string]any{"sha": p.HeadSHA, "merged": true, "message": "Pull Request successfully merged"})
			} else {
				w.WriteHeader(404)
				writeJSON(w, 404, map[string]any{"message": "Not Found"})
			}
		}
	case "reviews":
		switch m {
		case http.MethodGet:
			handleListPullReviews(w, r, repo, n)
		case http.MethodPost:
			handleCreatePullReview(w, r, repo, n)
		}
	case "comments":
		if m == http.MethodGet {
			writeJSON(w, 200, []any{})
		}
	case "labels":
		if m == http.MethodGet {
			writeJSON(w, 200, []any{})
		}
	default:
		http.NotFound(w, r)
	}
}

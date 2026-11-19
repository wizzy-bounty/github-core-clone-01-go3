# Anvil-Go v3 — GitHub-compatible git forge

[![make.fun](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/c/github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/wizzy-bounty/github-core-clone-01-go3/rank.svg)](https://make.fun/c/github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/wizzy-bounty/github-core-clone-01-go3/reward.svg)](https://make.fun/c/github-core-clone-01)

A clean-rebuild Go implementation of a GitHub-compatible git forge: smart-HTTP
git transport (protocol v0 + v2), REST API under `/api/v3`, GraphQL at
`/api/graphql`, and MCP at `/mcp` — all in-memory, single static binary, zero
external dependencies.

Built for the make.fun contest `github-core-clone-01` (pool $500, top 20 paid).
Account `@quick-raven-30`. This is a build/grade artifact; it is not submitted
from here.

## Local grade (kit runner, 3 trials, fresh server each)

| Trial | Checks | Feature | Agent | Perf | Total | p50 |
|------:|-------:|--------:|------:|-----:|------:|----:|
| 1 | 120/120 | 600 | 200 | 155 | 955 | 374,014 µs |
| 2 | 120/120 | 600 | 200 | 155 | 955 | 376,409 µs |
| 3 | 120/120 | 600 | 200 | 143 | 943 | 442,910 µs |
| **Median** | **120/120** | **600** | **200** | **155** | **955** | **376,409 µs** |

Perf workload points (median trial): push 39, clone 30, read 42, write 38
(points vary run to run; totals above are the measured medians).

Reference points: live incumbent 923, prize-zone bar #10 ≈ 960, #1 ≈ 970.
Median 955 sits above the incumbent but ~5 points under the #10 bar; the gap
is entirely perf (feature and agent are maxed at 600/200).

## Run

```sh
# build the static binary (Go 1.25, no network needed)
CGO_ENABLED=0 GOPROXY=off go build -trimpath -ldflags="-s -w" -o anvil-go .
go vet ./...

# serve on :3000 (PORT env; MAKEFUN_ENV_URL points at the mock/agent env)
PORT=3000 MAKEFUN_ENV_URL=http://127.0.0.1:18080 ./anvil-go
# or via the Node launcher (spawns ./anvil-go)
node main.ts
```

Health: `GET /health` → `{"status":"ok"}`. Auth: `Authorization: token
<token>` (or `Bearer` / `Basic`); the test token maps to `adaiversen`, any
other non-empty token to `foreign-user`, no token → 401 on every
`/repos/:owner/:repo` route and on GraphQL/MCP.

## Architecture

All state lives in memory (`store.go`); there is no database. The server is a
single Go module (`module anvil-go`, ~6,800 lines, no third-party deps).

| File | Contents |
|------|----------|
| `main.go` | HTTP router, `/api/v3` prefix strip, repo/branch/ref routing, Link-header pagination |
| `git.go` | Object store helpers, commit/tree closures, `fetchPlan` (incremental fetch, shallow/deepen), pack encode/decode, sideband framing |
| `git_http.go` | Smart HTTP: `info/refs` (v0 advertisement with peeled `^{}` lines), v2 `ls-refs` (peel as `peeled:` attribute), `fetch`, `receive-pack` (per-ref fast-forward checks), shallow/unshallow |
| `rest.go` | REST handlers: repos, contents, issues, pulls (real 3-way merge with conflict → 405), labels, git objects/refs/tags, pagination |
| `graphql.go` | GraphQL: viewer/repository/issues/pulls, mutations (incl. `mergePullRequest`), `__type` introspection, cursor connections (`first`/`after`, `hasNextPage`) |
| `mcp.go` | MCP (JSON-RPC): `initialize`, `tools/list`, 21 tools, `tools/call` with auth |
| `store.go` | In-memory store: repos, objects, refs, issues, pulls, labels |
| `main.ts` | Node launcher that spawns `./anvil-go` |

## Protocol notes

- **v2 `ls-refs` peel:** protocol v2 reports a peeled tag as an attribute on
  the ref line (`refs/tags/v1 peeled:<oid>`), not a separate `^{}` line — the
  `^{}` line is the v0 format and is only emitted in the v0 advertisement.
- **v0 shallow:** shallow lines carry no trailing LF; a flush follows the
  shallow/unshallow block only when the shallow machinery is engaged; no NAK
  is sent in a no-`done` request that has a shallow phase.
- **Incremental fetch:** objects reachable from the client's haves (commits
  plus their trees/blobs) are never re-sent.
- **Merges:** a pull request merges the *current* branch tips (not the
  snapshot taken when it was opened) with a per-file 3-way merge; a file
  changed differently on both sides (or modified vs deleted) is a conflict
  (REST 405). Merge commits record `merge_commit_sha`.
- **Pagination:** list endpoints emit GitHub-style `Link` headers
  (`next`/`last`/`first`/`prev`) with the request's own host and path
  (including the `/api/v3` prefix when present).

## License

MIT — see `LICENSE`.

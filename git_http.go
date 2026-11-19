package main

import (
	"bytes"
	"net/http"
	"strings"
)

const zeroOID = "0000000000000000000000000000000000000000"

func agentCaps() string { return "agent=anvil-go/1.0" }

// ---------- info/refs ----------

func handleInfoRefs(w http.ResponseWriter, r *http.Request, owner, repoName, service string) {
	repo := store.GetRepo(owner, repoName)
	if repo == nil {
		writeErr(w, 404, "Not Found")
		return
	}
	w.Header().Set("Content-Type", "application/x-"+service+"-advertisement")
	w.Header().Set("Cache-Control", "no-cache")
	var buf bytes.Buffer
	buf.Write(pktLine("# service=" + service + "\n"))
	buf.Write(pktFlush())
	store.mu.RLock()
	defer store.mu.RUnlock()
	if service == "git-upload-pack" && !strings.Contains(r.Header.Get("Git-Protocol"), "version=2") {
		// protocol v0 advertisement: refs with capabilities on the first line
		caps := "multi_ack thin-pack side-band side-band-64k ofs-delta shallow deepen-since deepen-not deepen-relative no-progress include-tag multi_ack_detailed allow-tip-sha1-in-want allow-reachable-sha1-in-want no-done fetch filter symref=HEAD:refs/heads/" + repo.DefaultBranch + " " + agentCaps() + " object-format=sha1"
		refs := repo.sortedRefNames()
		if len(refs) == 0 {
			buf.Write(pktLine(zeroOID + " capabilities^{}\x00" + caps + "\n"))
		} else {
			headSHA, hasHead := repo.Refs["refs/heads/"+repo.DefaultBranch]
			if hasHead {
				buf.Write(pktLine(headSHA + " HEAD\x00" + caps + "\n"))
			}
			for i, name := range refs {
				if i == 0 && !hasHead {
					buf.Write(pktLine(repo.Refs[name] + " " + name + "\x00" + caps + "\n"))
					if peeled := peelTag(repo, repo.Refs[name]); peeled != "" {
						buf.Write(pktLine(peeled + " " + name + "^{}\n"))
					}
					continue
				}
				buf.Write(pktLine(repo.Refs[name] + " " + name + "\n"))
				if peeled := peelTag(repo, repo.Refs[name]); peeled != "" {
					buf.Write(pktLine(peeled + " " + name + "^{}\n"))
				}
			}
		}
		buf.Write(pktFlush())
		w.Write(buf.Bytes())
		return
	}
	if service == "git-upload-pack" {
		// protocol v2 advertisement
		buf.Write(pktLine("version 2\n"))
		buf.Write(pktLine(agentCaps() + "\n"))
		buf.Write(pktLine("ls-refs=unborn\n"))
		buf.Write(pktLine("fetch=shallow wait-for-done filter\n"))
		buf.Write(pktLine("object-format=sha1\n"))
		buf.Write(pktLine("object-info\n"))
		buf.Write(pktFlush())
	} else {
		// receive-pack v0 advertisement
		caps := "report-status delete-refs side-band-64k quiet atomic ofs-delta " + agentCaps() + " object-format=sha1"
		refs := repo.sortedRefNames()
		if len(refs) == 0 {
			buf.Write(pktLine(zeroOID + " capabilities^{}\x00" + caps + "\n"))
		} else {
			for i, name := range refs {
				line := repo.Refs[name] + " " + name + "\n"
				if i == 0 {
					line = repo.Refs[name] + " " + name + "\x00" + caps + "\n"
				}
				buf.Write(pktLine(line))
				if peeled := peelTag(repo, repo.Refs[name]); peeled != "" {
					buf.Write(pktLine(peeled + " " + name + "^{}\n"))
				}
			}
		}
		buf.Write(pktFlush())
	}
	w.Write(buf.Bytes())
}

// ---------- upload-pack v2 ----------

type fetchArgs struct {
	wants          []string
	haves          []string
	shallow        []string
	deepen         int
	deepenRelative bool
	waitForDone    bool
	filter         string
	sideBand       bool
	thinPack       bool
	ofsDelta       bool
	noProgress     bool
	done           bool
}

func parseFetchArgs(lines []string) fetchArgs {
	var fa fetchArgs
	for _, ln := range lines {
		ln = strings.TrimRight(ln, "\n")
		switch {
		case strings.HasPrefix(ln, "want "):
			fa.wants = append(fa.wants, strings.TrimSpace(ln[5:]))
		case strings.HasPrefix(ln, "have "):
			fa.haves = append(fa.haves, strings.TrimSpace(ln[5:]))
		case strings.HasPrefix(ln, "shallow "):
			fa.shallow = append(fa.shallow, strings.TrimSpace(ln[8:]))
		case strings.HasPrefix(ln, "deepen "):
			n, _ := parseInt(strings.TrimSpace(ln[7:]))
			fa.deepen = n
		case ln == "deepen-relative":
			fa.deepenRelative = true
		case ln == "wait-for-done":
			fa.waitForDone = true
		case ln == "include-tag":
			// accepted, tags pointing at wanted commits are included implicitly
		case strings.HasPrefix(ln, "filter "):
			fa.filter = strings.TrimSpace(ln[7:])
		case ln == "side-band-64k":
			fa.sideBand = true
		case ln == "thin-pack":
			fa.thinPack = true
		case ln == "ofs-delta":
			fa.ofsDelta = true
		case ln == "no-progress":
			fa.noProgress = true
		case ln == "done":
			fa.done = true
		}
	}
	return fa
}

func parseInt(s string) (int, error) {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, errBadInt
		}
		n = n*10 + int(c-'0')
	}
	return n, nil
}

var errBadInt = &simpleError{"bad int"}

type simpleError struct{ s string }

func (e *simpleError) Error() string { return e.s }

func handleUploadPackV2(w http.ResponseWriter, r *http.Request, repo *Repo, body []byte) {
	pr := &pktReader{data: body}
	cmd, _, err := pr.next()
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	cmd = strings.TrimRight(cmd, "\n")
	var argLines []string
	for {
		ln, flush, err := pr.next()
		if err == errDelim {
			continue
		}
		if err != nil || flush {
			break
		}
		argLines = append(argLines, ln)
	}
	w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
	w.Header().Set("Cache-Control", "no-cache")
	switch {
	case strings.HasPrefix(cmd, "command=ls-refs"):
		handleLsRefs(w, repo, argLines)
	case strings.HasPrefix(cmd, "command=fetch"):
		fa := parseFetchArgs(argLines)
		handleFetchV2(w, repo, fa)
	case strings.HasPrefix(cmd, "command=object-info"):
		handleObjectInfo(w, repo, argLines)
	default:
		http.Error(w, "unknown command", http.StatusBadRequest)
	}
}

func handleLsRefs(w http.ResponseWriter, repo *Repo, args []string) {
	peel, unborn := false, false
	var prefixes []string
	for _, a := range args {
		a = strings.TrimRight(a, "\n")
		switch {
		case a == "peel":
			peel = true
		case a == "unborn":
			unborn = true
		case strings.HasPrefix(a, "ref-prefix "):
			prefixes = append(prefixes, strings.TrimSpace(a[11:]))
		}
	}
	var buf bytes.Buffer
	store.mu.RLock()
	defer store.mu.RUnlock()
	refs := repo.sortedRefNames()
	match := func(name string) bool {
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
	// HEAD first
	headTarget := "refs/heads/" + repo.DefaultBranch
	if sha, ok := repo.Refs[headTarget]; ok && match("HEAD") {
		buf.Write(pktLine(sha + " HEAD symref-target:" + headTarget + "\n"))
	} else if unborn && len(refs) == 0 {
		buf.Write(pktLine("unborn HEAD symref-target:" + headTarget + "\n"))
	}
	for _, name := range refs {
		if !match(name) {
			continue
		}
		sha := repo.Refs[name]
		// Protocol v2 reports a peeled tag as an attribute on the ref
		// line itself (peeled:<oid>), not as a separate ^{} line — the
		// ^{} line is the v0 format. Real git emits only the attribute.
		line := sha + " " + name
		if peel {
			if peeled := peelTag(repo, sha); peeled != "" {
				line += " peeled:" + peeled
			}
		}
		buf.Write(pktLine(line + "\n"))
	}
	buf.Write(pktFlush())
	w.Write(buf.Bytes())
}

func parseTagTarget(data []byte) string {
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "object ") {
			return strings.TrimSpace(line[7:])
		}
		if line == "" {
			break
		}
	}
	return ""
}

// peelTag follows annotated-tag objects to the underlying commit SHA.
// Returns "" if sha is not a tag object (or the target is missing).
func peelTag(repo *Repo, sha string) string {
	for i := 0; i < 8; i++ {
		o := repo.Objects[sha]
		if o == nil || o.Type != "tag" {
			if i == 0 {
				return ""
			}
			return sha
		}
		tgt := parseTagTarget(o.Data)
		if tgt == "" {
			return ""
		}
		sha = tgt
	}
	return sha
}

func handleFetchV2(w http.ResponseWriter, repo *Repo, fa fetchArgs) {
	store.mu.RLock()
	defer store.mu.RUnlock()
	// resolve wants that are ref names? wants are always OIDs here; also allow
	// "want <oid>" where oid may be a ref tip already present.
	var wants []string
	for _, wd := range fa.wants {
		if _, ok := repo.Objects[wd]; ok {
			wants = append(wants, wd)
		} else if sha, ok := repo.Refs[wd]; ok {
			wants = append(wants, sha)
		}
	}
	blobNone := fa.filter == "blob:none"
	sendObjs, sendOrder, newShallow, unshallow := repo.fetchPlan(wants, fa.haves, fa.shallow, fa.deepen, fa.deepenRelative, blobNone)
	pack := buildPack(sendObjs, sendOrder)
	var buf bytes.Buffer
	sendPackfile := fa.done || fa.deepen > 0
	if len(fa.haves) > 0 && !fa.done && !sendPackfile {
		// negotiation round only: acknowledgments + flush, no packfile
		buf.Write(pktLine("acknowledgments\n"))
		acked := false
		for _, h := range fa.haves {
			if _, ok := repo.Objects[h]; ok {
				buf.Write(pktLine("ACK " + h + "\n"))
				acked = true
			}
		}
		if !acked {
			buf.Write(pktLine("NAK\n"))
		}
		buf.Write(pktFlush())
		w.Write(buf.Bytes())
		return
	}
	if len(fa.haves) > 0 && !fa.done {
		buf.Write(pktLine("acknowledgments\n"))
		acked := false
		for _, h := range fa.haves {
			if _, ok := repo.Objects[h]; ok {
				buf.Write(pktLine("ACK " + h + "\n"))
				acked = true
			}
		}
		if !acked {
			buf.Write(pktLine("NAK\n"))
		}
		buf.Write(pktLine("ready\n"))
		buf.Write(pktDelim())
	}
	if len(newShallow) > 0 || len(unshallow) > 0 {
		buf.Write(pktLine("shallow-info\n"))
		for _, s := range newShallow {
			buf.Write(pktLine("shallow " + s + "\n"))
		}
		for _, u := range unshallow {
			buf.Write(pktLine("unshallow " + u + "\n"))
		}
		buf.Write(pktDelim())
	}
	buf.Write(pktLine("packfile\n"))
	// protocol v2: packfile is always side-band framed
	buf.Write(sidebandPack(pack))
	w.Write(buf.Bytes())
}

func handleObjectInfo(w http.ResponseWriter, repo *Repo, args []string) {
	wantSize, wantType, wantContent := false, false, false
	var oids []string
	for _, a := range args {
		a = strings.TrimRight(a, "\n")
		switch a {
		case "size":
			wantSize = true
		case "type":
			wantType = true
		case "content":
			wantContent = true
		default:
			if len(a) == 40 {
				oids = append(oids, a)
			}
		}
	}
	var buf bytes.Buffer
	store.mu.RLock()
	defer store.mu.RUnlock()
	for _, oid := range oids {
		o := repo.Objects[oid]
		if o == nil {
			continue
		}
		if wantSize {
			buf.Write(pktLine("size " + itoa(len(o.Data)) + "\n"))
		}
		if wantType {
			buf.Write(pktLine("type " + o.Type + "\n"))
		}
		if wantContent {
			buf.Write(pktLine("content\n"))
			buf.Write(o.Data)
		}
	}
	buf.Write(pktFlush())
	w.Write(buf.Bytes())
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// ---------- upload-pack v0 ----------

func handleUploadPackV0(w http.ResponseWriter, r *http.Request, repo *Repo, body []byte) {
	pr := &pktReader{data: body}
	var wants, haves, shallow []string
	deepen := 0
	sideBand := false
	deepenRelative := false
	done := false
	// read all request lines until EOF (v0 clients may send flushes between
	// the want lines and "done"; there is no pack data in an upload-pack
	// request, so flushes are not delimiters here)
	for {
		ln, flush, err := pr.next()
		if err != nil {
			break
		}
		if flush {
			continue
		}
		ln = strings.TrimRight(ln, "\n")
		switch {
		case strings.HasPrefix(ln, "want "):
			// v0: "want <oid> SP <capability-list>" — caps are space separated
			rest := strings.TrimSpace(ln[5:])
			fields := strings.Fields(rest)
			if len(fields) > 0 {
				wants = append(wants, fields[0])
			}
			if len(fields) > 1 {
				caps := strings.Join(fields[1:], " ")
				if strings.Contains(caps, "side-band-64k") {
					sideBand = true
				}
				if strings.Contains(caps, "deepen-relative") {
					deepenRelative = true
				}
			}
		case strings.HasPrefix(ln, "have "):
			haves = append(haves, strings.TrimSpace(ln[5:]))
		case strings.HasPrefix(ln, "shallow "):
			shallow = append(shallow, strings.TrimSpace(ln[8:]))
		case strings.HasPrefix(ln, "deepen "):
			n, _ := parseInt(strings.TrimSpace(ln[7:]))
			deepen = n
		case ln == "done":
			done = true
		}
	}
	w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
	w.Header().Set("Cache-Control", "no-cache")
	store.mu.RLock()
	defer store.mu.RUnlock()
	var w2 []string
	for _, wd := range wants {
		if _, ok := repo.Objects[wd]; ok {
			w2 = append(w2, wd)
		}
	}
	sendObjs, sendOrder, newShallow, unshallow := repo.fetchPlan(w2, haves, shallow, deepen, deepenRelative, false)
	var buf bytes.Buffer
	// v0 stateless RPC: the client makes (at least) two requests — first
	// the negotiation (no "done"), then the same request with "done".
	// The pack is sent ONLY in the response to the request with "done".
	// Shallow phase: "shallow"/"unshallow" lines (no trailing LF, like
	// git's own upload-pack) terminated by a flush packet. The flush is
	// sent whenever the shallow machinery is engaged (deepen asked or
	// client sent shallow lines), even with an empty list.
	// ACK/NAK phase: the NAK after the shallow flush of a no-"done"
	// request is never read by the client (it stops at the flush), and
	// sending it in the same write lets it leak into the next request's
	// read as a stale "NAK" ("expected shallow list"). So it is omitted
	// when there are no haves and no "done". With haves, the client
	// expects an ACK/NAK answer, so one is always sent then.
	shallowPhase := deepen > 0 || len(shallow) > 0 || len(newShallow) > 0 || len(unshallow) > 0
	if shallowPhase {
		for _, s := range newShallow {
			buf.Write(pktLine("shallow " + s))
		}
		for _, s := range unshallow {
			buf.Write(pktLine("unshallow " + s))
		}
		buf.Write(pktFlush())
	}
	var common []string
	for _, h := range haves {
		if _, ok := repo.Objects[h]; ok {
			common = append(common, h)
		}
	}
	if len(common) > 0 {
		for _, h := range common {
			buf.Write(pktLine("ACK " + h + " common\n"))
		}
		if done {
			buf.Write(pktLine("ACK " + common[len(common)-1] + "\n"))
		}
	} else if !shallowPhase || len(haves) > 0 || done {
		buf.Write(pktLine("NAK\n"))
	}
	if done {
		pack := buildPack(sendObjs, sendOrder)
		if sideBand {
			buf.Write(sidebandPack(pack))
		} else {
			buf.Write(pack)
		}
	}
	w.Write(buf.Bytes())
}

// ---------- receive-pack ----------

func handleReceivePack(w http.ResponseWriter, r *http.Request, repo *Repo, body []byte) {
	pr := &pktReader{data: body}
	type cmd struct {
		old, new, ref string
	}
	var cmds []cmd
	reportStatus, sideBand := false, false
	for {
		ln, flush, err := pr.next()
		if err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if flush {
			break
		}
		ln = strings.TrimRight(ln, "\n")
		if i := strings.IndexByte(ln, 0); i >= 0 {
			caps := ln[i+1:]
			if strings.Contains(caps, "report-status") {
				reportStatus = true
			}
			if strings.Contains(caps, "side-band-64k") {
				sideBand = true
			}
			ln = ln[:i]
		}
		parts := strings.Fields(ln)
		if len(parts) == 3 {
			cmds = append(cmds, cmd{old: parts[0], new: parts[1], ref: parts[2]})
		}
	}
	packData := pr.rest()
	store.mu.Lock()
	defer store.mu.Unlock()
	var results []string // "ok <ref>" or "ng <ref> <msg>"
	unpackOK := true
	if len(packData) > 0 {
		objs, err := parsePack(packData, func(sha string) *Object { return repo.Objects[sha] })
		if err != nil {
			unpackOK = false
			for _, c := range cmds {
				results = append(results, "ng "+c.ref+" unpack failed: "+err.Error())
			}
		} else {
			for sha, o := range objs {
				repo.Objects[sha] = o
			}
		}
	}
	if unpackOK {
		for _, c := range cmds {
			if c.new == zeroOID {
				// delete ref
				if _, ok := repo.Refs[c.ref]; !ok {
					results = append(results, "ng "+c.ref+" ref does not exist")
					continue
				}
				delete(repo.Refs, c.ref)
				results = append(results, "ok "+c.ref)
				continue
			}
			if _, ok := repo.Objects[c.new]; !ok {
				results = append(results, "ng "+c.ref+" object not found")
				continue
			}
			if c.old != zeroOID {
				if cur, ok := repo.Refs[c.ref]; !ok || cur != c.old {
					results = append(results, "ng "+c.ref+" non-fast-forward or stale old value")
					continue
				}
			}
			// Fast-forward check: the current tip must be an ancestor of
			// the new commit (a stale client whose tracking ref predates
			// the server tip must be rejected, not silently overwritten).
			if cur, ok := repo.Refs[c.ref]; ok && cur != zeroOID {
				if !isAncestor(repo, cur, c.new) {
					results = append(results, "ng "+c.ref+" non-fast-forward")
					continue
				}
			}
			repo.Refs[c.ref] = c.new
			if len(repo.Refs) == 1 && strings.HasPrefix(c.ref, "refs/heads/") {
				repo.DefaultBranch = strings.TrimPrefix(c.ref, "refs/heads/")
			}
			repo.PushedAt = nowUTC()
			repo.UpdatedAt = nowUTC()
			results = append(results, "ok "+c.ref)
		}
	}
	w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
	w.Header().Set("Cache-Control", "no-cache")
	var buf bytes.Buffer
	if reportStatus || sideBand {
		// status report is pkt-line framed, then wrapped in side-band band 1
		// when side-band-64k was negotiated (nested framing).
		var report bytes.Buffer
		if unpackOK {
			report.Write(pktLine("unpack ok\n"))
		} else {
			report.Write(pktLine("unpack failed\n"))
		}
		for _, res := range results {
			report.Write(pktLine(res + "\n"))
		}
		report.Write(pktFlush())
		if sideBand {
			buf.Write(sidebandPack(report.Bytes()))
		} else {
			buf.Write(report.Bytes())
		}
	}
	w.Write(buf.Bytes())
}

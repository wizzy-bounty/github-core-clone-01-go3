package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
)

// ---------- pkt-line ----------

func pktLine(s string) []byte {
	n := len(s) + 4
	return append([]byte(fmt.Sprintf("%04x", n)), s...)
}

func pktFlush() []byte { return []byte("0000") }

func pktDelim() []byte { return []byte("0001") }

// pktReader parses pkt-lines from a byte stream.
type pktReader struct {
	data []byte
	pos  int
}

func (p *pktReader) next() (string, bool, error) {
	if p.pos+4 > len(p.data) {
		return "", false, io.EOF
	}
	hdr := string(p.data[p.pos : p.pos+4])
	if hdr == "0000" {
		p.pos += 4
		return "", true, nil // flush
	}
	if hdr == "0001" {
		p.pos += 4
		return "", false, errDelim
	}
	n, err := strconv.ParseInt(hdr, 16, 32)
	if err != nil || n < 4 || p.pos+int(n) > len(p.data) {
		return "", false, fmt.Errorf("bad pkt-line %q", hdr)
	}
	line := string(p.data[p.pos+4 : p.pos+int(n)])
	p.pos += int(n)
	return line, false, nil
}

var errDelim = errors.New("delim")

func (p *pktReader) rest() []byte {
	if p.pos > len(p.data) {
		return nil
	}
	return p.data[p.pos:]
}

// ---------- packfile parsing ----------

type packObject struct {
	Type string
	Data []byte
}

const (
	objCommit = 1
	objTree   = 2
	objBlob   = 3
	objTag    = 4
	objOfs    = 6
	objRef    = 7
)

func typeName(t int) string {
	switch t {
	case objCommit:
		return "commit"
	case objTree:
		return "tree"
	case objBlob:
		return "blob"
	case objTag:
		return "tag"
	}
	return ""
}

// parsePack parses a packfile (v2/v3), resolving deltas. lookup resolves
// ref-delta bases that are not in the pack (thin packs: objects already on
// the server). Returns sha -> object.
func parsePack(data []byte, lookup func(string) *Object) (map[string]*Object, error) {
	if len(data) < 12 || string(data[:4]) != "PACK" {
		return nil, errors.New("not a packfile")
	}
	ver := binary.BigEndian.Uint32(data[4:8])
	if ver != 2 && ver != 3 {
		return nil, fmt.Errorf("unsupported pack version %d", ver)
	}
	count := int(binary.BigEndian.Uint32(data[8:12]))
	pos := 12
	type rawEntry struct {
		typ    int
		data   []byte
		ofs    int // start offset in pack (for ofs-delta)
		baseOf int // ofs-delta base offset
		baseSHA string
	}
	entries := make([]rawEntry, 0, count)
	byOffset := map[int]int{}
	for i := 0; i < count; i++ {
		start := pos
		if pos >= len(data) {
			return nil, errors.New("pack truncated")
		}
		b := data[pos]
		pos++
		typ := int((b >> 4) & 7)
		size := int64(b & 0x0f)
		shift := uint(4)
		for b&0x80 != 0 {
			if pos >= len(data) {
				return nil, errors.New("pack truncated")
			}
			b = data[pos]
			pos++
			size |= int64(b&0x7f) << shift
			shift += 7
		}
		var baseOf int
		var baseSHA string
		switch typ {
		case objOfs:
			if pos >= len(data) {
				return nil, errors.New("pack truncated")
			}
			b = data[pos]
			pos++
			off := int(b & 0x7f)
			for b&0x80 != 0 {
				if pos >= len(data) {
					return nil, errors.New("pack truncated")
				}
				b = data[pos]
				pos++
				off = ((off + 1) << 7) | int(b&0x7f)
			}
			baseOf = start - off
		case objRef:
			if pos+20 > len(data) {
				return nil, errors.New("pack truncated")
			}
			baseSHA = hex.EncodeToString(data[pos : pos+20])
			pos += 20
		}
		zr, err := zlib.NewReader(bytes.NewReader(data[pos:]))
		if err != nil {
			return nil, err
		}
		raw, err := io.ReadAll(zr)
		zr.Close()
		if err != nil {
			return nil, err
		}
		// consumed bytes = len(data[pos:]) - remaining in zr's reader: use zlib reader's
		// underlying position via counting: re-derive by compressing length — instead
		// measure with a counting reader.
		consumed := zlibConsumed(data[pos:], raw)
		pos += consumed
		_ = size
		entries = append(entries, rawEntry{typ: typ, data: raw, ofs: start, baseOf: baseOf, baseSHA: baseSHA})
		byOffset[start] = len(entries) - 1
	}
	out := map[string]*Object{}
	var resolve func(idx int, depth int) (string, *Object, error)
	resolve = func(idx int, depth int) (string, *Object, error) {
		if depth > 64 {
			return "", nil, errors.New("delta chain too deep")
		}
		e := entries[idx]
		switch e.typ {
		case objCommit, objTree, objBlob, objTag:
			t := typeName(e.typ)
			sha := gitHash(t, e.data)
			return sha, &Object{Type: t, Data: e.data}, nil
		case objOfs:
			bi, ok := byOffset[e.baseOf]
			if !ok {
				return "", nil, fmt.Errorf("ofs-delta base %d not found", e.baseOf)
			}
			_, base, err := resolve(bi, depth+1)
			if err != nil {
				return "", nil, err
			}
			full, err := applyDelta(base.Data, e.data)
			if err != nil {
				return "", nil, err
			}
			sha := gitHash(base.Type, full)
			return sha, &Object{Type: base.Type, Data: full}, nil
		case objRef:
			// base may be in this pack, or already on the server (thin pack)
			if lookup != nil {
				if base := lookup(e.baseSHA); base != nil {
					full, err := applyDelta(base.Data, e.data)
					if err != nil {
						return "", nil, err
					}
					sha := gitHash(base.Type, full)
					return sha, &Object{Type: base.Type, Data: full}, nil
				}
			}
			for j := range entries {
				if j == idx {
					continue
				}
				s, o, err := resolve(j, depth+1)
				if err == nil && s == e.baseSHA {
					full, err := applyDelta(o.Data, e.data)
					if err != nil {
						return "", nil, err
					}
					sha := gitHash(o.Type, full)
					return sha, &Object{Type: o.Type, Data: full}, nil
				}
			}
			return "", nil, fmt.Errorf("ref-delta base %s not found", e.baseSHA)
		}
		return "", nil, fmt.Errorf("unknown object type %d", e.typ)
	}
	for i := range entries {
		sha, obj, err := resolve(i, 0)
		if err != nil {
			return nil, err
		}
		out[sha] = obj
	}
	return out, nil
}

// zlibConsumed returns how many compressed bytes zlib consumed to produce raw.
func zlibConsumed(comp []byte, raw []byte) int {
	// binary search the smallest prefix that inflates to raw
	lo, hi := 1, len(comp)
	for lo < hi {
		mid := (lo + hi) / 2
		zr, err := zlib.NewReader(bytes.NewReader(comp[:mid]))
		if err != nil {
			lo = mid + 1
			continue
		}
		got, err := io.ReadAll(zr)
		zr.Close()
		if err == nil && bytes.Equal(got, raw) {
			hi = mid
		} else {
			lo = mid + 1
		}
	}
	return lo
}

func applyDelta(base, delta []byte) ([]byte, error) {
	pos := 0
	readVarint := func() (int64, error) {
		var v int64
		shift := uint(0)
		for {
			if pos >= len(delta) {
				return 0, errors.New("delta truncated")
			}
			b := delta[pos]
			pos++
			v |= int64(b&0x7f) << shift
			shift += 7
			if b&0x80 == 0 {
				break
			}
		}
		return v, nil
	}
	srcSize, err := readVarint()
	if err != nil {
		return nil, err
	}
	if int(srcSize) != len(base) {
		return nil, fmt.Errorf("delta base size mismatch %d != %d", srcSize, len(base))
	}
	dstSize, err := readVarint()
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, dstSize)
	for pos < len(delta) {
		op := delta[pos]
		pos++
		if op&0x80 != 0 {
			// copy from base
			var off, size int
			for i := 0; i < 4; i++ {
				if op&(1<<uint(i)) != 0 {
					if pos >= len(delta) {
						return nil, errors.New("delta truncated")
					}
					off |= int(delta[pos]) << (8 * uint(i))
					pos++
				}
			}
			for i := 0; i < 3; i++ {
				if op&(0x10<<uint(i)) != 0 {
					if pos >= len(delta) {
						return nil, errors.New("delta truncated")
					}
					size |= int(delta[pos]) << (8 * uint(i))
					pos++
				}
			}
			if size == 0 {
				size = 0x10000
			}
			if off+size > len(base) {
				return nil, errors.New("delta copy out of range")
			}
			out = append(out, base[off:off+size]...)
		} else if op != 0 {
			if pos+int(op) > len(delta) {
				return nil, errors.New("delta truncated")
			}
			out = append(out, delta[pos:pos+int(op)]...)
			pos += int(op)
		} else {
			return nil, errors.New("delta opcode 0")
		}
	}
	if int64(len(out)) != dstSize {
		return nil, fmt.Errorf("delta size mismatch %d != %d", len(out), dstSize)
	}
	return out, nil
}

// ---------- packfile generation (no deltas) ----------

func buildPack(objs map[string]*Object, order []string) []byte {
	var buf bytes.Buffer
	buf.WriteString("PACK")
	var hdr [8]byte
	binary.BigEndian.PutUint32(hdr[0:4], 2)
	binary.BigEndian.PutUint32(hdr[4:8], uint32(len(order)))
	buf.Write(hdr[:])
	for _, sha := range order {
		o := objs[sha]
		if o == nil {
			continue
		}
		var typ int
		switch o.Type {
		case "commit":
			typ = objCommit
		case "tree":
			typ = objTree
		case "blob":
			typ = objBlob
		case "tag":
			typ = objTag
		}
		size := len(o.Data)
		// first byte: (size & 0x0f) | (typ<<4), continuation if size>>4
		first := byte((size & 0x0f) | (typ << 4))
		size >>= 4
		if size > 0 {
			first |= 0x80
		}
		buf.WriteByte(first)
		for size > 0 {
			b := byte(size & 0x7f)
			size >>= 7
			if size > 0 {
				b |= 0x80
			}
			buf.WriteByte(b)
		}
		zw := zlib.NewWriter(&buf)
		zw.Write(o.Data)
		zw.Close()
	}
	// trailer: sha1 of pack content
	sum := sha1.Sum(buf.Bytes())
	buf.Write(sum[:])
	return buf.Bytes()
}

// ---------- fetch computation ----------

// fetchPlan computes which objects to send.
// wants: tip SHAs requested. haves: SHAs client has. shallowBoundary: client's
// current shallow commits (from "shallow" lines). deepen: 0 = no limit, else max
// depth from wants (depth of want tip = 0; only used when >0). deepenRelative:
// deepen is relative to the client's current shallow depth.
// Returns: objects to send (ordered), new shallow SHAs, unshallow SHAs.
func (r *Repo) fetchPlan(wants, haves, shallowBoundary []string, deepen int, deepenRelative bool, blobNone bool) (sendObjs map[string]*Object, sendOrder []string, newShallow []string, unshallow []string) {
	// want closure
	wantCommits := r.commitClosure(wants)
	// held set: closure of haves, CUT at shallow boundary: boundary commit itself
	// is held, its parents are not traversed.
	held := map[string]bool{}
	queue := append([]string{}, haves...)
	for len(queue) > 0 {
		sha := queue[0]
		queue = queue[1:]
		if sha == "" || held[sha] {
			continue
		}
		held[sha] = true
		isBoundary := false
		for _, b := range shallowBoundary {
			if b == sha {
				isBoundary = true
				break
			}
		}
		if isBoundary {
			continue
		}
		obj := r.Objects[sha]
		if obj == nil || obj.Type != "commit" {
			continue
		}
		ci := parseCommit(obj.Data)
		queue = append(queue, ci.Parents...)
	}
	// depth map from wants
	depth := map[string]int{}
	{
		q := append([]string{}, wants...)
		for _, w := range wants {
			depth[w] = 0
		}
		for len(q) > 0 {
			sha := q[0]
			q = q[1:]
			obj := r.Objects[sha]
			if obj == nil || obj.Type != "commit" {
				continue
			}
			ci := parseCommit(obj.Data)
			for _, p := range ci.Parents {
				if _, ok := depth[p]; !ok {
					depth[p] = depth[sha] + 1
					q = append(q, p)
				}
			}
		}
	}
	sendCommits := map[string]bool{}
	effDeepen := deepen
	if deepenRelative && deepen > 0 && len(shallowBoundary) > 0 {
		// client's current depth = depth of its shallow boundary from wants + 1
		maxB := 0
		for _, b := range shallowBoundary {
			if d, ok := depth[b]; ok && d+1 > maxB {
				maxB = d + 1
			}
		}
		effDeepen = deepen + maxB
	}
	for sha := range wantCommits {
		if held[sha] {
			continue
		}
		if effDeepen > 0 {
			if d, ok := depth[sha]; !ok || d >= effDeepen {
				continue
			}
		}
		sendCommits[sha] = true
	}
	// objects: commits + their trees/blobs, excluding anything the client
	// already holds (held commits and everything reachable from their
	// trees — an unchanged file must not be re-sent).
	heldObjs := map[string]bool{}
	for sha := range held {
		o := r.Objects[sha]
		if o == nil || o.Type != "commit" {
			continue
		}
		heldObjs[sha] = true
		ci := parseCommit(o.Data)
		r.treeClosure(ci.Tree, heldObjs)
	}
	sendObjs = map[string]*Object{}
	for sha := range sendCommits {
		o := r.Objects[sha]
		if o == nil {
			continue
		}
		sendObjs[sha] = o
		ci := parseCommit(o.Data)
		treeSet := map[string]bool{}
		r.treeClosure(ci.Tree, treeSet)
		for t := range treeSet {
			if heldObjs[t] {
				continue
			}
			o := r.Objects[t]
			if o == nil {
				continue
			}
			if blobNone && o.Type == "blob" {
				continue
			}
			sendObjs[t] = o
		}
	}
	// order: commits first (topological-ish), then trees, then blobs
	var commits, trees, blobs []string
	for sha := range sendObjs {
		switch sendObjs[sha].Type {
		case "commit":
			commits = append(commits, sha)
		case "tree":
			trees = append(trees, sha)
		default:
			blobs = append(blobs, sha)
		}
	}
	sort.Strings(commits)
	sort.Strings(trees)
	sort.Strings(blobs)
	sendOrder = append(append(commits, trees...), blobs...)
	// new shallow: sent commits whose parent is not sent and not held
	for sha := range sendCommits {
		o := r.Objects[sha]
		if o == nil {
			continue
		}
		ci := parseCommit(o.Data)
		for _, p := range ci.Parents {
			if !sendCommits[p] && !held[p] && r.Objects[p] != nil {
				newShallow = append(newShallow, sha)
				break
			}
		}
	}
	sort.Strings(newShallow)
	// unshallow: client's old boundary commits whose parents are now all available
	for _, b := range shallowBoundary {
		o := r.Objects[b]
		if o == nil || o.Type != "commit" {
			continue
		}
		ci := parseCommit(o.Data)
		allAvail := true
		for _, p := range ci.Parents {
			if !sendCommits[p] && !held[p] {
				allAvail = false
				break
			}
		}
		if allAvail {
			unshallow = append(unshallow, b)
		}
	}
	sort.Strings(unshallow)
	return sendObjs, sendOrder, newShallow, unshallow
}

// ---------- side-band ----------

func sidebandPack(pack []byte) []byte {
	var buf bytes.Buffer
	const max = 65515 // pkt-line = 4 hex + 1 band byte + chunk ≤ 65520
	for len(pack) > 0 {
		n := len(pack)
		if n > max {
			n = max
		}
		chunk := pack[:n]
		pack = pack[n:]
		// pkt-line: 4 hex len + 1 band byte + data
		total := 4 + 1 + len(chunk)
		buf.WriteString(fmt.Sprintf("%04x", total))
		buf.WriteByte(0x01)
		buf.Write(chunk)
	}
	buf.WriteString("0000")
	return buf.Bytes()
}

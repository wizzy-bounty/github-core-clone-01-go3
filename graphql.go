package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// ---------- tokenizer ----------

type gqlTok struct {
	kind string // name, string, int, float, punct, var, eof
	val  string
}

func gqlTokenize(src string) []gqlTok {
	var toks []gqlTok
	i := 0
	for i < len(src) {
		c := src[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ',':
			i++
		case c == '#':
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case c == '.' && i+2 < len(src) && src[i+1] == '.' && src[i+2] == '.':
			toks = append(toks, gqlTok{"punct", "..."})
			i += 3
		case c == '{' || c == '}' || c == '(' || c == ')' || c == ':' || c == '!' || c == '[' || c == ']' || c == '=' || c == '@' || c == '|' || c == '&':
			toks = append(toks, gqlTok{"punct", string(c)})
			i++
		case c == '$':
			j := i + 1
			for j < len(src) && (isNameChar(src[j])) {
				j++
			}
			toks = append(toks, gqlTok{"var", src[i+1 : j]})
			i = j
		case c == '"':
			j := i + 1
			var sb strings.Builder
			for j < len(src) && src[j] != '"' {
				if src[j] == '\\' && j+1 < len(src) {
					j++
					switch src[j] {
					case 'n':
						sb.WriteByte('\n')
					case 't':
						sb.WriteByte('\t')
					case 'r':
						sb.WriteByte('\r')
					default:
						sb.WriteByte(src[j])
					}
					j++
				} else {
					sb.WriteByte(src[j])
					j++
				}
			}
			toks = append(toks, gqlTok{"string", sb.String()})
			i = j + 1
		case c >= '0' && c <= '9' || c == '-':
			j := i
			isFloat := false
			for j < len(src) && (src[j] >= '0' && src[j] <= '9' || src[j] == '.' || src[j] == 'e' || src[j] == 'E' || src[j] == '-' || src[j] == '+') {
				if src[j] == '.' || src[j] == 'e' || src[j] == 'E' {
					isFloat = true
				}
				j++
			}
			kind := "int"
			if isFloat {
				kind = "float"
			}
			toks = append(toks, gqlTok{kind, src[i:j]})
			i = j
		default:
			j := i
			for j < len(src) && isNameChar(src[j]) {
				j++
			}
			if j == i {
				i++
				continue
			}
			toks = append(toks, gqlTok{"name", src[i:j]})
			i = j
		}
	}
	toks = append(toks, gqlTok{"eof", ""})
	return toks
}

func isNameChar(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// ---------- AST ----------

type gqlValue struct {
	kind string // string,int,float,bool,null,enum,var,list,object
	str  string
	list []*gqlValue
	obj  map[string]*gqlValue
}

type gqlField struct {
	alias     string
	name      string
	args      map[string]*gqlValue
	selection []*gqlField // nil = scalar
	frag      string      // fragment spread name (selection holds inline fields)
	inlineOn  string
}

type gqlOp struct {
	kind      string // query | mutation
	name      string
	varDefs   map[string]*gqlValue // default values
	selection []*gqlField
}

type gqlDoc struct {
	ops       []*gqlOp
	fragments map[string][]*gqlField
}

type gqlParser struct {
	toks []gqlTok
	pos  int
}

func (p *gqlParser) peek() gqlTok  { return p.toks[p.pos] }
func (p *gqlParser) next() gqlTok  { t := p.toks[p.pos]; p.pos++; return t }
func (p *gqlParser) expect(kind, val string) gqlTok {
	t := p.next()
	if t.kind != kind || (val != "" && t.val != val) {
		panic(fmt.Sprintf("graphql: expected %s %q got %s %q", kind, val, t.kind, t.val))
	}
	return t
}

func parseGQL(src string) (doc *gqlDoc, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%v", r)
		}
	}()
	p := &gqlParser{toks: gqlTokenize(src)}
	doc = &gqlDoc{fragments: map[string][]*gqlField{}}
	for p.peek().kind != "eof" {
		t := p.peek()
		if t.kind == "name" && t.val == "fragment" {
			p.next()
			name := p.expect("name", "").val
			p.expect("name", "on")
			_ = p.expect("name", "").val
			sel := p.parseSelectionSet()
			doc.fragments[name] = sel
			continue
		}
		op := &gqlOp{kind: "query", varDefs: map[string]*gqlValue{}}
		if t.kind == "name" && (t.val == "query" || t.val == "mutation" || t.val == "subscription") {
			op.kind = p.next().val
			if p.peek().kind == "name" {
				op.name = p.next().val
			}
		}
		if p.peek().kind == "punct" && p.peek().val == "(" {
			p.next()
			for !(p.peek().kind == "punct" && p.peek().val == ")") {
				v := p.expect("var", "").val
				p.expect("punct", ":")
				// type (skip until = or , or ))
				depth := 0
				for {
					tk := p.peek()
					if depth == 0 && ((tk.kind == "punct" && (tk.val == "," || tk.val == ")")) || (tk.kind == "punct" && tk.val == "=")) {
						break
					}
					if tk.kind == "punct" && (tk.val == "[" || tk.val == "(") {
						depth++
					}
					if tk.kind == "punct" && (tk.val == "]" || tk.val == ")") {
						depth--
						if depth < 0 {
							break
						}
					}
					p.next()
				}
				if p.peek().kind == "punct" && p.peek().val == "=" {
					p.next()
					op.varDefs[v] = p.parseValue()
				}
				if p.peek().kind == "punct" && p.peek().val == "," {
					p.next()
				}
			}
			p.expect("punct", ")")
		}
		// directives on operation: skip
		for p.peek().kind == "punct" && p.peek().val == "@" {
			p.next()
			p.expect("name", "")
			if p.peek().kind == "punct" && p.peek().val == "(" {
				p.parseArgs()
			}
		}
		op.selection = p.parseSelectionSet()
		doc.ops = append(doc.ops, op)
	}
	return doc, nil
}

func (p *gqlParser) parseSelectionSet() []*gqlField {
	p.expect("punct", "{")
	var fields []*gqlField
	for !(p.peek().kind == "punct" && p.peek().val == "}") {
		fields = append(fields, p.parseField())
	}
	p.expect("punct", "}")
	return fields
}

func (p *gqlParser) parseField() *gqlField {
	f := &gqlField{args: map[string]*gqlValue{}}
	// fragment spread or inline fragment
	if p.peek().kind == "punct" && p.peek().val == "..." {
		p.next()
		if p.peek().kind == "name" && p.peek().val == "on" {
			p.next()
			f.inlineOn = p.expect("name", "").val
			// directives
			for p.peek().kind == "punct" && p.peek().val == "@" {
				p.next()
				p.expect("name", "")
				if p.peek().kind == "punct" && p.peek().val == "(" {
					p.parseArgs()
				}
			}
			f.selection = p.parseSelectionSet()
			return f
		}
		f.frag = p.expect("name", "").val
		for p.peek().kind == "punct" && p.peek().val == "@" {
			p.next()
			p.expect("name", "")
			if p.peek().kind == "punct" && p.peek().val == "(" {
				p.parseArgs()
			}
		}
		return f
	}
	name := p.expect("name", "").val
	if p.peek().kind == "punct" && p.peek().val == ":" {
		p.next()
		f.alias = name
		f.name = p.expect("name", "").val
	} else {
		f.name = name
	}
	if p.peek().kind == "punct" && p.peek().val == "(" {
		f.args = p.parseArgs()
	}
	// directives
	for p.peek().kind == "punct" && p.peek().val == "@" {
		p.next()
		dirName := p.expect("name", "").val
		var dirArgs map[string]*gqlValue
		if p.peek().kind == "punct" && p.peek().val == "(" {
			dirArgs = p.parseArgs()
		}
		if dirName == "skip" || dirName == "include" {
			// handled at execution via stored args
			if f.args == nil {
				f.args = map[string]*gqlValue{}
			}
			if v, ok := dirArgs["if"]; ok {
				f.args["__"+dirName] = v
			}
		}
	}
	if p.peek().kind == "punct" && p.peek().val == "{" {
		f.selection = p.parseSelectionSet()
	}
	return f
}

func (p *gqlParser) parseArgs() map[string]*gqlValue {
	args := map[string]*gqlValue{}
	p.expect("punct", "(")
	for !(p.peek().kind == "punct" && p.peek().val == ")") {
		name := p.expect("name", "").val
		p.expect("punct", ":")
		args[name] = p.parseValue()
		if p.peek().kind == "punct" && p.peek().val == "," {
			p.next()
		}
	}
	p.expect("punct", ")")
	return args
}

func (p *gqlParser) parseValue() *gqlValue {
	t := p.next()
	switch t.kind {
	case "string":
		return &gqlValue{kind: "string", str: t.val}
	case "int":
		return &gqlValue{kind: "int", str: t.val}
	case "float":
		return &gqlValue{kind: "float", str: t.val}
	case "var":
		return &gqlValue{kind: "var", str: t.val}
	case "name":
		if t.val == "true" || t.val == "false" {
			return &gqlValue{kind: "bool", str: t.val}
		}
		if t.val == "null" {
			return &gqlValue{kind: "null"}
		}
		return &gqlValue{kind: "enum", str: t.val}
	case "punct":
		if t.val == "[" {
			v := &gqlValue{kind: "list"}
			for !(p.peek().kind == "punct" && p.peek().val == "]") {
				v.list = append(v.list, p.parseValue())
				if p.peek().kind == "punct" && p.peek().val == "," {
					p.next()
				}
			}
			p.expect("punct", "]")
			return v
		}
		if t.val == "{" {
			v := &gqlValue{kind: "object", obj: map[string]*gqlValue{}}
			for !(p.peek().kind == "punct" && p.peek().val == "}") {
				name := p.expect("name", "").val
				p.expect("punct", ":")
				v.obj[name] = p.parseValue()
				if p.peek().kind == "punct" && p.peek().val == "," {
					p.next()
				}
			}
			p.expect("punct", "}")
			return v
		}
	}
	panic("graphql: unexpected value token " + t.val)
}

// ---------- execution ----------

type gqlCtx struct {
	vars      map[string]any
	fragments map[string][]*gqlField
	errs      []any
}

func (c *gqlCtx) evalValue(v *gqlValue) any {
	if v == nil {
		return nil
	}
	switch v.kind {
	case "string", "enum":
		return v.str
	case "int":
		n, _ := strconv.Atoi(v.str)
		return n
	case "float":
		f, _ := strconv.ParseFloat(v.str, 64)
		return f
	case "bool":
		return v.str == "true"
	case "null":
		return nil
	case "var":
		if val, ok := c.vars[v.str]; ok {
			return val
		}
		return nil
	case "list":
		var out []any
		for _, e := range v.list {
			out = append(out, c.evalValue(e))
		}
		return out
	case "object":
		out := map[string]any{}
		for k, e := range v.obj {
			out[k] = c.evalValue(e)
		}
		return out
	}
	return nil
}

func evalArgs(c *gqlCtx, f *gqlField) map[string]any {
	out := map[string]any{}
	for k, v := range f.args {
		if strings.HasPrefix(k, "__") {
			continue
		}
		out[k] = c.evalValue(v)
	}
	return out
}

func argStr(args map[string]any, k string) string {
	if v, ok := args[k].(string); ok {
		return v
	}
	return ""
}

func argInt(args map[string]any, k string, def int) int {
	switch v := args[k].(type) {
	case int:
		return v
	case float64:
		return int(v)
	case json.Number:
		n, _ := v.Int64()
		return int(n)
	}
	return def
}

func argStrList(args map[string]any, k string) []string {
	switch v := args[k].(type) {
	case []any:
		var out []string
		for _, e := range v {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case string:
		return []string{v}
	}
	return nil
}

// node: a GraphQL object with a type name and field resolver.
type gqlNode struct {
	typename string
	get      func(field string, args map[string]any) (any, bool)
}

func scalarNode(typename string, val any) *gqlNode {
	return &gqlNode{typename: typename, get: func(string, map[string]any) (any, bool) { return val, true }}
}

func (c *gqlCtx) execSelection(node *gqlNode, fields []*gqlField) map[string]any {
	out := map[string]any{}
	for _, f := range fields {
		c.execField(node, f, out)
	}
	return out
}

func (c *gqlCtx) execField(node *gqlNode, f *gqlField, out map[string]any) {
	// directives
	if v, ok := f.args["__skip"]; ok {
		if b, _ := c.evalValue(v).(bool); b {
			return
		}
	}
	if v, ok := f.args["__include"]; ok {
		if b, _ := c.evalValue(v).(bool); !b {
			return
		}
	}
	if f.frag != "" {
		if sel, ok := c.fragments[f.frag]; ok {
			for _, sf := range sel {
				c.execField(node, sf, out)
			}
		}
		return
	}
	if f.inlineOn != "" {
		if node == nil || (f.inlineOn != node.typename && !gqlImplements(node.typename, f.inlineOn)) {
			return
		}
		for _, sf := range f.selection {
			c.execField(node, sf, out)
		}
		return
	}
	key := f.name
	if f.alias != "" {
		key = f.alias
	}
	if f.name == "__typename" {
		if node != nil {
			out[key] = node.typename
		} else {
			out[key] = "Query"
		}
		return
	}
	if node == nil {
		out[key] = nil
		return
	}
	args := evalArgs(c, f)
	val, ok := node.get(f.name, args)
	if !ok {
		c.errs = append(c.errs, map[string]any{"message": "Cannot query field \"" + f.name + "\" on type \"" + node.typename + "\"."})
		out[key] = nil
		return
	}
	out[key] = c.resolveValue(val, f.selection)
}

func gqlImplements(typename, iface string) bool {
	if iface == "Node" {
		return true
	}
	if iface == "GitObject" {
		switch typename {
		case "Commit", "Tree", "Blob", "Tag":
			return true
		}
	}
	return typename == iface
}

func (c *gqlCtx) resolveValue(val any, sel []*gqlField) any {
	switch v := val.(type) {
	case *gqlNode:
		if sel == nil {
			return nil
		}
		return c.execSelection(v, sel)
	case []*gqlNode:
		var out []any
		for _, n := range v {
			out = append(out, c.resolveValue(n, sel))
		}
		if out == nil {
			out = []any{}
		}
		return out
	case map[string]any:
		// connection-like or plain map; if selection present and values are nodes, resolve
		if sel == nil {
			return v
		}
		out := map[string]any{}
		for k, vv := range v {
			out[k] = c.resolveValue(vv, sel)
		}
		return out
	default:
		return val
	}
}

// connection helper: nodes -> {nodes, edges, totalCount, pageInfo}
func gqlConnection(nodes []*gqlNode, args map[string]any) *gqlNode {
	total := len(nodes)
	first := argInt(args, "first", 0)
	start := 0
	if after := argStr(args, "after"); after != "" {
		if strings.HasPrefix(after, "cursor:") {
			if idx, err := strconv.Atoi(strings.TrimPrefix(after, "cursor:")); err == nil {
				start = idx + 1
			}
		}
	}
	if start < 0 {
		start = 0
	}
	if start > total {
		start = total
	}
	end := total
	if first > 0 && start+first < end {
		end = start + first
	}
	page := nodes[start:end]
	hasNext := end < total
	var endCursor any
	if len(page) > 0 {
		endCursor = "cursor:" + strconv.Itoa(start+len(page)-1)
	}
	ns := page
	return &gqlNode{typename: "Connection", get: func(field string, a map[string]any) (any, bool) {
		switch field {
		case "nodes":
			return ns, true
		case "edges":
			var edges []*gqlNode
			for i, n := range ns {
				nn := n
				cur := "cursor:" + strconv.Itoa(start+i)
				edges = append(edges, &gqlNode{typename: "Edge", get: func(f string, a map[string]any) (any, bool) {
					if f == "node" {
						return nn, true
					}
					if f == "cursor" {
						return cur, true
					}
					return nil, false
				}})
			}
			return edges, true
		case "totalCount":
			return total, true
		case "pageInfo":
			return &gqlNode{typename: "PageInfo", get: func(f string, a map[string]any) (any, bool) {
				switch f {
				case "hasNextPage":
					return hasNext, true
				case "hasPreviousPage":
					return start > 0, true
				case "endCursor":
					return endCursor, true
				case "startCursor":
					if len(page) > 0 {
						return "cursor:" + strconv.Itoa(start), true
					}
					return nil, true
				}
				return nil, false
			}}, true
		}
		return nil, false
	}}
}

// ---------- node builders ----------

func userNode(login string) *gqlNode {
	return &gqlNode{typename: "User", get: func(field string, args map[string]any) (any, bool) {
		switch field {
		case "id":
			return "U_" + login, true
		case "login", "name":
			return login, true
		case "url":
			return "https://github.com/" + login, true
		case "avatarUrl":
			return "https://avatars.githubusercontent.com/u/" + strconv.Itoa(numID("user:"+login)) + "?v=4", true
		case "repositories":
			var nodes []*gqlNode
			for _, r := range store.ListRepos(login) {
				nodes = append(nodes, repoNode(r))
			}
			return gqlConnection(nodes, args), true
		case "databaseId":
			return numID("user:" + login), true
		}
		return nil, false
	}}
}

func repoNode(r *Repo) *gqlNode {
	return &gqlNode{typename: "Repository", get: func(field string, args map[string]any) (any, bool) {
		store.mu.RLock()
		defer store.mu.RUnlock()
		switch field {
		case "id":
			return "R_" + r.FullName, true
		case "databaseId":
			return numID("repo:" + r.FullName), true
		case "name":
			return r.Name, true
		case "nameWithOwner":
			return r.FullName, true
		case "description":
			return r.Description, true
		case "isPrivate":
			return r.Private, true
		case "isFork":
			return false, true
		case "isArchived":
			return r.Archived, true
		case "viewerPermission":
			return "ADMIN", true
		case "hasIssuesEnabled":
			return true, true
		case "url":
			return "https://github.com/" + r.FullName, true
		case "homepageUrl":
			return r.Homepage, true
		case "createdAt":
			return ts(r.CreatedAt), true
		case "updatedAt":
			return ts(r.UpdatedAt), true
		case "pushedAt":
			return ts(r.PushedAt), true
		case "defaultBranchRef":
			if sha, ok := r.Refs["refs/heads/"+r.DefaultBranch]; ok {
				return refNode(r, r.DefaultBranch, sha), true
			}
			return nil, true
		case "owner":
			return userNode(r.Owner), true
		case "stargazerCount":
			return r.Stars, true
		case "forkCount":
			return r.Forks, true
		case "watchers":
			return &gqlNode{typename: "UserConnection", get: func(f string, a map[string]any) (any, bool) {
				if f == "totalCount" {
					return r.Watchers, true
				}
				return nil, false
			}}, true
		case "issues":
			states := argStrList(args, "states")
			var list []*Issue
			nums := make([]int, 0, len(r.Issues))
			for n := range r.Issues {
				nums = append(nums, n)
			}
			sort.Ints(nums)
			for _, n := range nums {
				is := r.Issues[n]
				if len(states) > 0 {
					ok := false
					for _, st := range states {
						if strings.EqualFold(st, is.State) {
							ok = true
						}
					}
					if !ok {
						continue
					}
				}
				list = append(list, is)
			}
			// orderBy: {field: CREATED_AT|UPDATED_AT|COMMENTS, direction: ASC|DESC}
			// GitHub defaults to CREATED_AT DESC.
			field, dir := "CREATED_AT", "DESC"
			if ob, ok := args["orderBy"].(map[string]any); ok {
				if f := argStr(ob, "field"); f != "" {
					field = f
				}
				if d := argStr(ob, "direction"); d != "" {
					dir = d
				}
			}
			desc := strings.EqualFold(dir, "DESC")
			sort.Slice(list, func(i, j int) bool {
				a, b := list[i], list[j]
				switch strings.ToUpper(field) {
				case "UPDATED_AT":
					if !a.UpdatedAt.Equal(b.UpdatedAt) {
						if desc {
							return a.UpdatedAt.After(b.UpdatedAt)
						}
						return a.UpdatedAt.Before(b.UpdatedAt)
					}
				case "COMMENTS":
					if len(a.Comments) != len(b.Comments) {
						if desc {
							return len(a.Comments) > len(b.Comments)
						}
						return len(a.Comments) < len(b.Comments)
					}
				default: // CREATED_AT
					if !a.CreatedAt.Equal(b.CreatedAt) {
						if desc {
							return a.CreatedAt.After(b.CreatedAt)
						}
						return a.CreatedAt.Before(b.CreatedAt)
					}
				}
				if desc {
					return a.Number > b.Number
				}
				return a.Number < b.Number
			})
			var nodes []*gqlNode
			for _, is := range list {
				nodes = append(nodes, issueNode(r, is))
			}
			return gqlConnection(nodes, args), true
		case "pullRequests":
			states := argStrList(args, "states")
			var nodes []*gqlNode
			nums := make([]int, 0, len(r.Pulls))
			for n := range r.Pulls {
				nums = append(nums, n)
			}
			sort.Ints(nums)
			for _, n := range nums {
				p := r.Pulls[n]
				if len(states) > 0 {
					ok := false
					for _, st := range states {
						if (st == "OPEN" && p.State == "open") || (st == "CLOSED" && p.State != "open") || (st == "MERGED" && p.Merged) {
							ok = true
						}
					}
					if !ok {
						continue
					}
				}
				nodes = append(nodes, pullNode(r, p))
			}
			return gqlConnection(nodes, args), true
		case "issue":
			n := argInt(args, "number", 0)
			if is, ok := r.Issues[n]; ok {
				return issueNode(r, is), true
			}
			return nil, true
		case "pullRequest":
			n := argInt(args, "number", 0)
			if p, ok := r.Pulls[n]; ok {
				return pullNode(r, p), true
			}
			return nil, true
		case "ref":
			qn := argStr(args, "qualifiedName")
			if sha, ok := r.Refs[qn]; ok {
				return refNode(r, strings.TrimPrefix(qn, "refs/heads/"), sha), true
			}
			return nil, true
		case "object":
			expr := argStr(args, "expression")
			sha := resolveRef(r, expr)
			if sha == "" {
				sha = expr
			}
			return gitObjectNode(r, sha), true
		case "refs":
			prefix := argStr(args, "refPrefix")
			var nodes []*gqlNode
			for _, name := range r.sortedRefNames() {
				if prefix != "" && !strings.HasPrefix(name, prefix) {
					continue
				}
				short := strings.TrimPrefix(name, "refs/heads/")
				short = strings.TrimPrefix(short, "refs/tags/")
				nodes = append(nodes, refNode(r, short, r.Refs[name]))
			}
			return gqlConnection(nodes, args), true
		case "languages":
			return &gqlNode{typename: "LanguageConnection", get: func(f string, a map[string]any) (any, bool) {
				if f == "totalCount" {
					return 1, true
				}
				if f == "nodes" {
					return []*gqlNode{&gqlNode{typename: "Language", get: func(ff string, aa map[string]any) (any, bool) {
						if ff == "name" {
							return "Go", true
						}
						return nil, false
					}}}, true
				}
				return nil, false
			}}, true
		case "primaryLanguage":
			return &gqlNode{typename: "Language", get: func(f string, a map[string]any) (any, bool) {
				if f == "name" {
					return "Go", true
				}
				return nil, false
			}}, true
		}
		return nil, false
	}}
}

func refNode(r *Repo, name, sha string) *gqlNode {
	return &gqlNode{typename: "Ref", get: func(field string, args map[string]any) (any, bool) {
		switch field {
		case "id":
			return "REF_" + r.FullName + "_" + name, true
		case "name":
			return name, true
		case "prefix":
			return "refs/heads/", true
		case "target":
			return gitObjectNode(r, sha), true
		case "associatedPullRequests":
			var nodes []*gqlNode
			for _, p := range r.Pulls {
				if p.Head == name {
					nodes = append(nodes, pullNode(r, p))
				}
			}
			return gqlConnection(nodes, args), true
		}
		return nil, false
	}}
}

func gitObjectNode(r *Repo, sha string) *gqlNode {
	o := r.Objects[sha]
	if o == nil {
		return nil
	}
	switch o.Type {
	case "commit":
		ci := parseCommit(o.Data)
		return &gqlNode{typename: "Commit", get: func(field string, args map[string]any) (any, bool) {
			switch field {
			case "id":
				return "C_" + sha, true
			case "oid":
				return sha, true
			case "abbreviatedOid":
				if len(sha) > 7 {
					return sha[:7], true
				}
				return sha, true
			case "message":
				return strings.TrimRight(ci.Message, "\n"), true
			case "messageHeadline":
				msg := strings.TrimRight(ci.Message, "\n")
				if i := strings.Index(msg, "\n"); i >= 0 {
					msg = msg[:i]
				}
				return msg, true
			case "messageBody":
				msg := strings.TrimRight(ci.Message, "\n")
				if i := strings.Index(msg, "\n"); i >= 0 {
					return strings.TrimSpace(msg[i+1:]), true
				}
				return "", true
			case "author":
				au := parsePerson(ci.Author)
				return &gqlNode{typename: "GitActor", get: func(f string, a map[string]any) (any, bool) {
					switch f {
					case "name":
						return au["name"], true
					case "email":
						return au["email"], true
					case "date":
						return au["date"], true
					case "user":
						return userNode(loginFromPerson(ci.Author)), true
					}
					return nil, false
				}}, true
			case "committer":
				au := parsePerson(ci.Author)
				return &gqlNode{typename: "GitActor", get: func(f string, a map[string]any) (any, bool) {
					switch f {
					case "name":
						return au["name"], true
					case "email":
						return au["email"], true
					case "date":
						return au["date"], true
					}
					return nil, false
				}}, true
			case "committedDate":
				return parsePerson(ci.Author)["date"], true
			case "tree":
				return gitObjectNode(r, ci.Tree), true
			case "parents":
				var nodes []*gqlNode
				for _, p := range ci.Parents {
					if n := gitObjectNode(r, p); n != nil {
						nodes = append(nodes, n)
					}
				}
				return gqlConnection(nodes, args), true
			case "history":
				var nodes []*gqlNode
				seen := map[string]bool{}
				queue := []string{sha}
				for len(queue) > 0 {
					c := queue[0]
					queue = queue[1:]
					if c == "" || seen[c] {
						continue
					}
					seen[c] = true
					if n := gitObjectNode(r, c); n != nil {
						nodes = append(nodes, n)
					}
					if o2 := r.Objects[c]; o2 != nil && o2.Type == "commit" {
						queue = append(queue, parseCommit(o2.Data).Parents...)
					}
				}
				return gqlConnection(nodes, args), true
			}
			return nil, false
		}}
	case "tree":
		return &gqlNode{typename: "Tree", get: func(field string, args map[string]any) (any, bool) {
			switch field {
			case "id":
				return "T_" + sha, true
			case "oid":
				return sha, true
			case "abbreviatedOid":
				if len(sha) > 7 {
					return sha[:7], true
				}
				return sha, true
			case "entries":
				var nodes []*gqlNode
				for _, e := range parseTree(o.Data) {
					ee := e
					nodes = append(nodes, &gqlNode{typename: "TreeEntry", get: func(f string, a map[string]any) (any, bool) {
						switch f {
						case "name":
							return ee.Name, true
						case "type":
							return ee.Type, true
						case "mode":
							return ee.Mode, true
						case "oid":
							return ee.SHA, true
						case "object":
							return gitObjectNode(r, ee.SHA), true
						}
						return nil, false
					}})
				}
				return nodes, true
			}
			return nil, false
		}}
	case "blob":
		return &gqlNode{typename: "Blob", get: func(field string, args map[string]any) (any, bool) {
			switch field {
			case "id":
				return "B_" + sha, true
			case "oid":
				return sha, true
			case "abbreviatedOid":
				if len(sha) > 7 {
					return sha[:7], true
				}
				return sha, true
			case "text":
				return string(o.Data), true
			case "byteSize":
				return len(o.Data), true
			case "isBinary":
				return false, true
			}
			return nil, false
		}}
	case "tag":
		return &gqlNode{typename: "Tag", get: func(field string, args map[string]any) (any, bool) {
			switch field {
			case "id":
				return "TAG_" + sha, true
			case "oid":
				return sha, true
			case "name":
				return parseTagName(o.Data), true
			case "target":
				if tgt := parseTagTarget(o.Data); tgt != "" {
					return gitObjectNode(r, tgt), true
				}
				return nil, true
			}
			return nil, false
		}}
	}
	return nil
}

func parseTagName(data []byte) string {
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "tag ") {
			return strings.TrimSpace(line[4:])
		}
		if line == "" {
			break
		}
	}
	return ""
}

func issueNode(r *Repo, is *Issue) *gqlNode {
	return &gqlNode{typename: "Issue", get: func(field string, args map[string]any) (any, bool) {
		switch field {
		case "id":
			return "I_" + r.FullName + "_" + strconv.Itoa(is.Number), true
		case "databaseId", "number":
			return is.Number, true
		case "title":
			return is.Title, true
		case "body":
			return is.Body, true
		case "bodyText":
			return is.Body, true
		case "state":
			if is.State == "open" {
				return "OPEN", true
			}
			return "CLOSED", true
		case "createdAt":
			return ts(is.CreatedAt), true
		case "updatedAt":
			return ts(is.UpdatedAt), true
		case "closedAt":
			if is.ClosedAt == nil {
				return nil, true
			}
			return ts(*is.ClosedAt), true
		case "url":
			return "https://github.com/" + r.FullName + "/issues/" + strconv.Itoa(is.Number), true
		case "author":
			return userNode(is.User), true
		case "repository":
			return repoNode(r), true
		case "labels":
			var nodes []*gqlNode
			for _, l := range is.Labels {
				ll := l
				nodes = append(nodes, &gqlNode{typename: "Label", get: func(f string, a map[string]any) (any, bool) {
					switch f {
					case "id":
						return "L_" + ll, true
					case "name":
						return ll, true
					case "color":
						return "ededed", true
					case "description":
						return "", true
					}
					return nil, false
				}})
			}
			return gqlConnection(nodes, args), true
		case "assignees":
			var nodes []*gqlNode
			for _, a := range is.Assignees {
				nodes = append(nodes, userNode(a))
			}
			return gqlConnection(nodes, args), true
		case "comments":
			var nodes []*gqlNode
			for _, c := range is.Comments {
				cc := c
				nodes = append(nodes, &gqlNode{typename: "IssueComment", get: func(f string, a map[string]any) (any, bool) {
					switch f {
					case "id":
						return "IC_" + strconv.Itoa(cc.ID), true
					case "databaseId":
						return cc.ID, true
					case "body":
						return cc.Body, true
					case "bodyText":
						return cc.Body, true
					case "author":
						return userNode(cc.User), true
					case "createdAt":
						return ts(cc.CreatedAt), true
					case "updatedAt":
						return ts(cc.UpdatedAt), true
					case "url":
						return "https://github.com/" + r.FullName + "/issues/" + strconv.Itoa(is.Number) + "#issuecomment-" + strconv.Itoa(cc.ID), true
					}
					return nil, false
				}})
			}
			return gqlConnection(nodes, args), true
		}
		return nil, false
	}}
}

func pullNode(r *Repo, p *Pull) *gqlNode {
	return &gqlNode{typename: "PullRequest", get: func(field string, args map[string]any) (any, bool) {
		switch field {
		case "id":
			return "PR_" + r.FullName + "_" + strconv.Itoa(p.Number), true
		case "databaseId", "number":
			return p.Number, true
		case "title":
			return p.Title, true
		case "body":
			return p.Body, true
		case "bodyText":
			return p.Body, true
		case "state":
			if p.Merged {
				return "MERGED", true
			}
			if p.State == "open" {
				return "OPEN", true
			}
			return "CLOSED", true
		case "createdAt":
			return ts(p.CreatedAt), true
		case "updatedAt":
			return ts(p.UpdatedAt), true
		case "closedAt":
			if p.ClosedAt == nil {
				return nil, true
			}
			return ts(*p.ClosedAt), true
		case "mergedAt":
			if p.MergedAt == nil {
				return nil, true
			}
			return ts(*p.MergedAt), true
		case "url":
			return "https://github.com/" + r.FullName + "/pull/" + strconv.Itoa(p.Number), true
		case "author":
			return userNode(p.User), true
		case "repository":
			return repoNode(r), true
		case "headRefName":
			return p.Head, true
		case "baseRefName":
			return p.Base, true
		case "headRefOid":
			if p.HeadSHA != "" {
				return p.HeadSHA, true
			}
			return r.Refs["refs/heads/"+p.Head], true
		case "baseRefOid":
			if p.BaseSHA != "" {
				return p.BaseSHA, true
			}
			return r.Refs["refs/heads/"+p.Base], true
		case "headRef":
			if sha, ok := r.Refs["refs/heads/"+p.Head]; ok {
				return refNode(r, p.Head, sha), true
			}
			return nil, true
		case "baseRef":
			if sha, ok := r.Refs["refs/heads/"+p.Base]; ok {
				return refNode(r, p.Base, sha), true
			}
			return nil, true
		case "merged":
			return p.Merged, true
		case "mergeable":
			if p.Merged {
				return "UNKNOWN", true
			}
			return "MERGEABLE", true
		case "isDraft":
			return p.Draft, true
		case "files":
			store.mu.RLock()
			files := pullFiles(r, p)
			store.mu.RUnlock()
			var nodes []*gqlNode
			for _, f := range files {
				ff := f
				nodes = append(nodes, &gqlNode{typename: "PullRequestChangedFile", get: func(fld string, a map[string]any) (any, bool) {
					switch fld {
					case "path", "filename":
						return ff.Path, true
					case "sha":
						return ff.SHA, true
					case "status":
						return ff.Status, true
					case "changeType":
						return strings.ToUpper(ff.Status), true
					case "additions":
						return ff.Additions, true
					case "deletions":
						return ff.Deletions, true
					case "changes":
						return ff.Additions + ff.Deletions, true
					}
					return nil, false
				}})
			}
			return gqlConnection(nodes, args), true
		case "commits":
			var nodes []*gqlNode
			baseSet := r.commitClosure([]string{p.BaseSHA})
			seen := map[string]bool{}
			queue := []string{p.HeadSHA}
			for len(queue) > 0 {
				c := queue[0]
				queue = queue[1:]
				if c == "" || seen[c] || baseSet[c] {
					continue
				}
				seen[c] = true
				cc := c
				nodes = append(nodes, &gqlNode{typename: "PullRequestCommit", get: func(f string, a map[string]any) (any, bool) {
					if f == "commit" {
						return gitObjectNode(r, cc), true
					}
					if f == "oid" {
						return cc, true
					}
					return nil, false
				}})
				if o := r.Objects[c]; o != nil && o.Type == "commit" {
					queue = append(queue, parseCommit(o.Data).Parents...)
				}
			}
			return gqlConnection(nodes, args), true
		case "comments":
			var nodes []*gqlNode
			for _, c := range p.Comments {
				cc := c
				nodes = append(nodes, &gqlNode{typename: "IssueComment", get: func(f string, a map[string]any) (any, bool) {
					switch f {
					case "id":
						return "IC_" + strconv.Itoa(cc.ID), true
					case "body":
						return cc.Body, true
					case "author":
						return userNode(cc.User), true
					case "createdAt":
						return ts(cc.CreatedAt), true
					}
					return nil, false
				}})
			}
			return gqlConnection(nodes, args), true
		case "reviews":
			return gqlConnection(nil, args), true
		case "labels":
			return gqlConnection(nil, args), true
		}
		return nil, false
	}}
}

// ---------- root ----------

// Field names per object type, for __type introspection.
var gqlTypeFields = map[string][]string{
	"Query":      {"viewer", "repository", "user", "node", "search", "rateLimit"},
	"Mutation":   {"createIssue", "createPullRequest", "addComment", "closeIssue", "updateIssue", "mergePullRequest"},
	"User":       {"avatarUrl", "databaseId", "id", "login", "repositories", "url"},
	"Repository": {"createdAt", "databaseId", "defaultBranchRef", "description", "forkCount", "hasIssuesEnabled", "homepageUrl", "id", "isArchived", "isFork", "isPrivate", "issue", "issues", "languages", "name", "nameWithOwner", "object", "owner", "primaryLanguage", "pullRequest", "pullRequests", "pushedAt", "ref", "refs", "stargazerCount", "updatedAt", "url", "viewerPermission", "watchers"},
	"Issue":      {"assignees", "author", "body", "bodyText", "closedAt", "comments", "createdAt", "databaseId", "id", "labels", "number", "repository", "state", "title", "updatedAt", "url"},
	"PullRequest": {"author", "baseRef", "baseRefName", "baseRefOid", "body", "bodyText", "closedAt", "comments", "commits", "createdAt", "databaseId", "files", "headRef", "headRefName", "headRefOid", "id", "isDraft", "labels", "mergeable", "merged", "mergedAt", "number", "repository", "reviews", "state", "title", "updatedAt", "url"},
}

// Enum values per enum type, for __type introspection.
var gqlEnumValues = map[string][]string{
	"SearchType": {"COMMIT", "DISCUSSION", "ISSUE", "LABEL", "MILESTONE", "REPOSITORY", "TOPIC", "USER"},
}

func queryRoot(user string) *gqlNode {
	return &gqlNode{typename: "Query", get: func(field string, args map[string]any) (any, bool) {
		switch field {
		case "viewer":
			return userNode(user), true
		case "__type":
			name := argStr(args, "name")
			if enumVals, ok := gqlEnumValues[name]; ok {
				var nodes []*gqlNode
				for _, ev := range enumVals {
					ev := ev
					nodes = append(nodes, &gqlNode{typename: "__EnumValue", get: func(f string, a map[string]any) (any, bool) {
						if f == "name" {
							return ev, true
						}
						return nil, false
					}})
				}
				return &gqlNode{typename: "__Type", get: func(f string, a map[string]any) (any, bool) {
					switch f {
					case "name":
						return name, true
					case "kind":
						return "ENUM", true
					case "enumValues":
						return nodes, true
					}
					return nil, false
				}}, true
			}
			fields, ok := gqlTypeFields[name]
			if !ok {
				return nil, true
			}
			var nodes []*gqlNode
			for _, fn := range fields {
				fn := fn
				nodes = append(nodes, &gqlNode{typename: "__Field", get: func(f string, a map[string]any) (any, bool) {
					switch f {
					case "name":
						return fn, true
					case "type":
						return &gqlNode{typename: "__Type", get: func(ff string, aa map[string]any) (any, bool) {
							if ff == "name" {
								return "String", true
							}
							if ff == "kind" {
								return "SCALAR", true
							}
							return nil, false
						}}, true
					}
					return nil, false
				}})
			}
			return &gqlNode{typename: "__Type", get: func(f string, a map[string]any) (any, bool) {
				switch f {
				case "name":
					return name, true
				case "kind":
					return "OBJECT", true
				case "fields":
					return nodes, true
				}
				return nil, false
			}}, true
		case "repository":
			owner := argStr(args, "owner")
			name := argStr(args, "name")
			if r := store.GetRepo(owner, name); r != nil {
				return repoNode(r), true
			}
			return nil, true
		case "user":
			login := argStr(args, "login")
			return userNode(login), true
		case "node":
			id := argStr(args, "id")
			return nodeByID(id), true
		case "search":
			q := argStr(args, "query")
			typ := argStr(args, "type")
			var nodes []*gqlNode
			ql := strings.ToLower(q)
			for _, r := range store.ListRepos("") {
				if typ == "ISSUE" || typ == "ISSUE,PULL_REQUEST" {
					continue
				}
				if q == "" || strings.Contains(strings.ToLower(r.FullName), ql) || strings.Contains(strings.ToLower(r.Description), ql) {
					nodes = append(nodes, repoNode(r))
				}
			}
			if typ == "ISSUE" {
				for _, r := range store.ListRepos("") {
					for _, is := range r.Issues {
						if strings.Contains(strings.ToLower(is.Title), ql) {
							nodes = append(nodes, issueNode(r, is))
						}
					}
				}
			}
			return gqlConnection(nodes, args), true
		case "rateLimit":
			return &gqlNode{typename: "RateLimit", get: func(f string, a map[string]any) (any, bool) {
				switch f {
				case "limit":
					return 5000, true
				case "remaining":
					return 4999, true
				case "resetAt":
					return ts(nowUTC().Add(3600 * 1e9)), true
				case "cost":
					return 1, true
				}
				return nil, false
			}}, true
		}
		return nil, false
	}}
}

func nodeByID(id string) *gqlNode {
	if strings.HasPrefix(id, "R_") {
		full := strings.TrimPrefix(id, "R_")
		parts := strings.SplitN(full, "/", 2)
		if len(parts) == 2 {
			if r := store.GetRepo(parts[0], parts[1]); r != nil {
				return repoNode(r)
			}
		}
		return nil
	}
	if strings.HasPrefix(id, "I_") {
		rest := strings.TrimPrefix(id, "I_")
		idx := strings.LastIndex(rest, "_")
		if idx > 0 {
			full := rest[:idx]
			n, _ := strconv.Atoi(rest[idx+1:])
			parts := strings.SplitN(full, "/", 2)
			if len(parts) == 2 {
				if r := store.GetRepo(parts[0], parts[1]); r != nil {
					if is, ok := r.Issues[n]; ok {
						return issueNode(r, is)
					}
				}
			}
		}
		return nil
	}
	if strings.HasPrefix(id, "PR_") {
		rest := strings.TrimPrefix(id, "PR_")
		idx := strings.LastIndex(rest, "_")
		if idx > 0 {
			full := rest[:idx]
			n, _ := strconv.Atoi(rest[idx+1:])
			parts := strings.SplitN(full, "/", 2)
			if len(parts) == 2 {
				if r := store.GetRepo(parts[0], parts[1]); r != nil {
					if p, ok := r.Pulls[n]; ok {
						return pullNode(r, p)
					}
				}
			}
		}
		return nil
	}
	if strings.HasPrefix(id, "U_") {
		return userNode(strings.TrimPrefix(id, "U_"))
	}
	return nil
}

// ---------- mutations ----------

func mutationRoot(user string) *gqlNode {
	return &gqlNode{typename: "Mutation", get: func(field string, args map[string]any) (any, bool) {
		switch field {
		case "createIssue":
			input, _ := args["input"].(map[string]any)
			repoID := argStr(input, "repositoryId")
			title := argStr(input, "title")
			body := argStr(input, "body")
			full := strings.TrimPrefix(repoID, "R_")
			parts := strings.SplitN(full, "/", 2)
			if len(parts) != 2 {
				return nil, true
			}
			repo := store.GetRepo(parts[0], parts[1])
			if repo == nil {
				return nil, true
			}
			store.mu.Lock()
			n := repo.NextIssue
			repo.NextIssue++
			now := nowUTC()
			is := &Issue{Number: n, Title: title, Body: body, State: "open", User: user, CreatedAt: now, UpdatedAt: now, Labels: []string{}, Assignees: []string{}}
			repo.Issues[n] = is
			store.mu.Unlock()
			return &gqlNode{typename: "CreateIssuePayload", get: func(f string, a map[string]any) (any, bool) {
				switch f {
				case "issue":
					return issueNode(repo, is), true
				case "clientMutationId":
					return argStr(input, "clientMutationId"), true
				}
				return nil, false
			}}, true
		case "createPullRequest":
			input, _ := args["input"].(map[string]any)
			repoID := argStr(input, "repositoryId")
			title := argStr(input, "title")
			body := argStr(input, "body")
			head := argStr(input, "headRefName")
			base := argStr(input, "baseRefName")
			full := strings.TrimPrefix(repoID, "R_")
			parts := strings.SplitN(full, "/", 2)
			if len(parts) != 2 {
				return nil, true
			}
			repo := store.GetRepo(parts[0], parts[1])
			if repo == nil {
				return nil, true
			}
			store.mu.Lock()
			headSHA := repo.Refs["refs/heads/"+head]
			baseSHA := repo.Refs["refs/heads/"+base]
			if headSHA == "" || baseSHA == "" {
				store.mu.Unlock()
				return nil, true
			}
			n := repo.NextPull
			repo.NextPull++
			now := nowUTC()
			p := &Pull{Number: n, Title: title, Body: body, State: "open", Head: head, Base: base, HeadSHA: headSHA, BaseSHA: baseSHA, Mergeable: true, User: user, CreatedAt: now, UpdatedAt: now}
			repo.Pulls[n] = p
			store.mu.Unlock()
			return &gqlNode{typename: "CreatePullRequestPayload", get: func(f string, a map[string]any) (any, bool) {
				switch f {
				case "pullRequest":
					return pullNode(repo, p), true
				case "clientMutationId":
					return argStr(input, "clientMutationId"), true
				}
				return nil, false
			}}, true
		case "addComment":
			input, _ := args["input"].(map[string]any)
			subjectID := argStr(input, "subjectId")
			body := argStr(input, "body")
			node := nodeByID(subjectID)
			if node == nil {
				return nil, true
			}
			// attach to issue if subject is issue
			if strings.HasPrefix(subjectID, "I_") {
				rest := strings.TrimPrefix(subjectID, "I_")
				idx := strings.LastIndex(rest, "_")
				full := rest[:idx]
				n, _ := strconv.Atoi(rest[idx+1:])
				parts := strings.SplitN(full, "/", 2)
				if len(parts) == 2 {
					if repo := store.GetRepo(parts[0], parts[1]); repo != nil {
						store.mu.Lock()
						if is, ok := repo.Issues[n]; ok {
							id := repo.NextComment
							repo.NextComment++
							now := nowUTC()
							c := &Comment{ID: id, Body: body, User: user, CreatedAt: now, UpdatedAt: now}
							is.Comments = append(is.Comments, c)
							store.mu.Unlock()
							return &gqlNode{typename: "AddCommentPayload", get: func(f string, a map[string]any) (any, bool) {
								switch f {
								case "commentEdge":
									return &gqlNode{typename: "IssueCommentEdge", get: func(ff string, aa map[string]any) (any, bool) {
										if ff == "node" {
											return &gqlNode{typename: "IssueComment", get: func(fff string, aaa map[string]any) (any, bool) {
												switch fff {
												case "id":
													return "IC_" + strconv.Itoa(c.ID), true
												case "body":
													return c.Body, true
												case "author":
													return userNode(c.User), true
												case "createdAt":
													return ts(c.CreatedAt), true
												}
												return nil, false
											}}, true
										}
										return nil, false
									}}, true
								case "clientMutationId":
									return argStr(input, "clientMutationId"), true
								}
								return nil, false
							}}, true
						}
						store.mu.Unlock()
					}
				}
			}
			return &gqlNode{typename: "AddCommentPayload", get: func(f string, a map[string]any) (any, bool) {
				if f == "clientMutationId" {
					return argStr(input, "clientMutationId"), true
				}
				return nil, false
			}}, true
		case "closeIssue":
			input, _ := args["input"].(map[string]any)
			issueID := argStr(input, "issueId")
			node := nodeByID(issueID)
			if node == nil {
				return nil, true
			}
			if strings.HasPrefix(issueID, "I_") {
				rest := strings.TrimPrefix(issueID, "I_")
				idx := strings.LastIndex(rest, "_")
				full := rest[:idx]
				n, _ := strconv.Atoi(rest[idx+1:])
				parts := strings.SplitN(full, "/", 2)
				if len(parts) == 2 {
					if repo := store.GetRepo(parts[0], parts[1]); repo != nil {
						store.mu.Lock()
						is, ok := repo.Issues[n]
						if ok {
							is.State = "closed"
							now := nowUTC()
							is.ClosedAt = &now
							is.UpdatedAt = now
						}
						store.mu.Unlock()
						if ok {
							return &gqlNode{typename: "CloseIssuePayload", get: func(f string, a map[string]any) (any, bool) {
								if f == "issue" {
									return issueNode(repo, is), true
								}
								return nil, false
							}}, true
						}
					}
				}
			}
			return nil, true
		case "updateIssue":
			input, _ := args["input"].(map[string]any)
			issueID := argStr(input, "id")
			if strings.HasPrefix(issueID, "I_") {
				rest := strings.TrimPrefix(issueID, "I_")
				idx := strings.LastIndex(rest, "_")
				full := rest[:idx]
				n, _ := strconv.Atoi(rest[idx+1:])
				parts := strings.SplitN(full, "/", 2)
				if len(parts) == 2 {
					if repo := store.GetRepo(parts[0], parts[1]); repo != nil {
						store.mu.Lock()
						is, ok := repo.Issues[n]
						if ok {
							if v := argStr(input, "title"); v != "" {
								is.Title = v
							}
							if v, has := input["body"]; has {
								if s, ok2 := v.(string); ok2 {
									is.Body = s
								}
							}
							if v := argStr(input, "state"); v != "" {
								if v == "OPEN" {
									is.State = "open"
								} else if v == "CLOSED" {
									is.State = "closed"
								}
							}
							is.UpdatedAt = nowUTC()
						}
						store.mu.Unlock()
						if ok {
							return &gqlNode{typename: "UpdateIssuePayload", get: func(f string, a map[string]any) (any, bool) {
								if f == "issue" {
									return issueNode(repo, is), true
								}
								return nil, false
							}}, true
						}
					}
				}
			}
			return nil, true
		case "mergePullRequest":
			input, _ := args["input"].(map[string]any)
			prID := argStr(input, "pullRequestId")
			if prID == "" {
				prID = argStr(input, "id")
			}
			if !strings.HasPrefix(prID, "PR_") {
				return nil, true
			}
			rest := strings.TrimPrefix(prID, "PR_")
			idx := strings.LastIndex(rest, "_")
			if idx <= 0 {
				return nil, true
			}
			full := rest[:idx]
			n, _ := strconv.Atoi(rest[idx+1:])
			parts := strings.SplitN(full, "/", 2)
			if len(parts) != 2 {
				return nil, true
			}
			repo := store.GetRepo(parts[0], parts[1])
			if repo == nil {
				return nil, true
			}
			store.mu.Lock()
			p, ok := repo.Pulls[n]
			if !ok || p.Merged || p.State != "open" {
				store.mu.Unlock()
				return nil, true
			}
			author := user + " <" + user + "@users.noreply.github.com>"
			newSHA, conflict := doMergePull(repo, p, author)
			if conflict {
				store.mu.Unlock()
				return nil, true
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
			store.mu.Unlock()
			return &gqlNode{typename: "MergePullRequestPayload", get: func(f string, a map[string]any) (any, bool) {
				switch f {
				case "pullRequest":
					return pullNode(repo, p), true
				case "clientMutationId":
					return argStr(input, "clientMutationId"), true
				}
				return nil, false
			}}, true
		}
		return nil, false
	}}
}

// ---------- HTTP handler ----------

type gqlRequestBody struct {
	Query         string         `json:"query"`
	Variables     map[string]any `json:"variables"`
	OperationName string         `json:"operationName"`
}

func handleGraphQL(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	if user == "" {
		writeJSON(w, 401, map[string]any{"errors": []any{map[string]any{"message": "Requires authentication"}}})
		return
	}
	raw, _ := io.ReadAll(r.Body)
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		// Batched operations: a JSON array of request objects.
		var batch []gqlRequestBody
		if err := json.Unmarshal(trimmed, &batch); err != nil {
			writeJSON(w, 400, map[string]any{"errors": []any{map[string]any{"message": "invalid request body"}}})
			return
		}
		var out []any
		for _, b := range batch {
			out = append(out, execGQL(b, user))
		}
		if out == nil {
			out = []any{}
		}
		writeJSON(w, 200, out)
		return
	}
	var body gqlRequestBody
	if err := json.Unmarshal(trimmed, &body); err != nil {
		writeJSON(w, 400, map[string]any{"errors": []any{map[string]any{"message": "invalid request body"}}})
		return
	}
	writeJSON(w, 200, execGQL(body, user))
}

func execGQL(body gqlRequestBody, user string) map[string]any {
	doc, err := parseGQL(body.Query)
	if err != nil {
		return map[string]any{"errors": []any{map[string]any{"message": err.Error()}}}
	}
	ctx := &gqlCtx{vars: body.Variables, fragments: doc.fragments}
	if ctx.vars == nil {
		ctx.vars = map[string]any{}
	}
	var op *gqlOp
	for _, o := range doc.ops {
		if body.OperationName == "" || o.name == body.OperationName {
			op = o
			break
		}
	}
	if op == nil {
		if len(doc.ops) > 0 {
			op = doc.ops[0]
		} else {
			return map[string]any{"errors": []any{map[string]any{"message": "no operation"}}}
		}
	}
	// apply variable defaults
	for k, dv := range op.varDefs {
		if _, ok := ctx.vars[k]; !ok {
			ctx.vars[k] = ctx.evalValue(dv)
		}
	}
	var root *gqlNode
	if op.kind == "mutation" {
		root = mutationRoot(user)
	} else {
		root = queryRoot(user)
	}
	data := ctx.execSelection(root, op.selection)
	resp := map[string]any{"data": data}
	if len(ctx.errs) > 0 {
		resp["errors"] = ctx.errs
	}
	return resp
}

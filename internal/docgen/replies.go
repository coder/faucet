package main

import (
	"cmp"
	"errors"
	"fmt"
	"go/ast"
	"go/token"
	"io/fs"
	"path"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// cronType is the type of cmd/faucet whose methods serve the cron routes.
const cronType = "cronRoutes"

// replyWriters are the functions of cmd/faucet that write a JSON reply, with
// the value as their last argument.
var replyWriters = []string{"writeJSON", "writeJSONStatus"}

// cronRepliesTable lists the fields of each cron route's reply and the
// result types whose doc comments explain them. It reads both from the
// code: the struct literal that a handler writes, and the type of the value
// that fills each field.
func cronRepliesTable(s source) ([]string, error) {
	l := &loader{fsys: s.fsys, pkgs: map[string]*loadedPackage{}}
	cmd, err := l.load("cmd/faucet")
	if err != nil {
		return nil, err
	}
	routes, err := servedRoutes(cmd.goPackage)
	if err != nil {
		return nil, err
	}
	rows := []string{"| Route | Reply fields | Documented on |", "| --- | --- | --- |"}
	for _, r := range routes {
		h, ok, err := cronHandler(cmd, r)
		if err != nil {
			return nil, err
		}
		if !ok && strings.HasPrefix(r.path, "/cron/") {
			return nil, fmt.Errorf("%s: %s has no handler that is a method of %s, so its reply cannot be read", r.at, r, cronType)
		}
		if !ok {
			continue
		}
		fields, docs, err := l.reply(cmd, h)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", r, err)
		}
		names := make([]string, len(docs))
		for i, t := range docs {
			names[i] = t.String()
		}
		rows = append(rows, fmt.Sprintf("| `%s` | %s | %s |", r, codeList(fields), codeList(names)))
	}
	if len(rows) == 2 {
		return nil, fmt.Errorf("no route of cmd/faucet registers a handler of %s", cronType)
	}
	return rows, nil
}

// cronHandler finds the method of cronType that r registers as its handler,
// one that takes a response writer and a request.
func cronHandler(cmd *loadedPackage, r route) (*ast.FuncDecl, bool, error) {
	var found []*ast.FuncDecl
	inline := false
	ast.Inspect(r.handler, func(n ast.Node) bool {
		if _, ok := n.(*ast.FuncLit); ok {
			inline = true
			return false
		}
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if _, ok := sel.X.(*ast.Ident); !ok {
			return true
		}
		if m, ok := cmd.methods[cronType][sel.Sel.Name]; ok && m.decl.Type.Results == nil && fieldCount(m.decl.Type.Params) == 2 {
			found = append(found, m.decl)
		}
		return true
	})
	switch {
	case inline && strings.HasPrefix(r.path, "/cron/"):
		return nil, false, fmt.Errorf("%s: %s has an inline handler, so its reply cannot be read; make it a method of %s", r.at, r, cronType)
	case len(found) == 0:
		return nil, false, nil
	case len(found) == 1:
		return found[0], true, nil
	default:
		return nil, false, fmt.Errorf("%s registers %d handlers of %s", r, len(found), cronType)
	}
}

func fieldCount(fields *ast.FieldList) int {
	n := 0
	for _, f := range fields.List {
		n += max(1, len(f.Names))
	}
	return n
}

// reply returns the JSON names of the fields that handler h writes, a nested
// struct's as parent.child, and the result types that fill them.
func (l *loader) reply(cmd *loadedPackage, h *ast.FuncDecl) ([]string, []typeName, error) {
	var values []ast.Expr
	ast.Inspect(h.Body, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			if id, ok := call.Fun.(*ast.Ident); ok && slices.Contains(replyWriters, id.Name) && len(call.Args) > 0 {
				values = append(values, call.Args[len(call.Args)-1])
			}
		}
		return true
	})
	if len(values) != 1 {
		return nil, nil, fmt.Errorf("%s writes %d replies with %s, want 1", h.Name.Name, len(values), strings.Join(replyWriters, " or "))
	}
	lit, ok := values[0].(*ast.CompositeLit)
	if !ok {
		return nil, nil, fmt.Errorf("%s writes a reply that is not a struct literal", h.Name.Name)
	}
	sc := handlerScope{l: l, pkg: cmd, fn: h, results: map[string]typeName{}, untyped: map[string]error{}}
	sc.self = typeName{pkg: cmd, name: cronType}
	if names := h.Recv.List[0].Names; len(names) > 0 {
		sc.recv = names[0].Name
	}
	sc.findResults()
	r := &replyFields{}
	if err := sc.walk(lit, "", r); err != nil {
		return nil, nil, err
	}
	if len(r.problems) > 0 {
		return nil, nil, errors.New(strings.Join(r.problems, "; "))
	}
	if len(r.docs) == 0 {
		return nil, nil, fmt.Errorf("no field of the reply of %s comes from a result of a method of the module", h.Name.Name)
	}
	return r.fields, r.docs, nil
}

type replyFields struct {
	fields   []string
	docs     []typeName
	problems []string
}

// handlerScope types the expressions of one handler: its receiver, and the
// variables that hold what a method of the module returned.
type handlerScope struct {
	l       *loader
	pkg     *loadedPackage
	fn      *ast.FuncDecl
	recv    string
	self    typeName
	results map[string]typeName
	// untyped are the variables that hold what a call returned whose type
	// typeOf cannot tell, so that a reply field that reads one fails instead
	// of passing unchecked.
	untyped map[string]error
}

// findResults records each variable that the handler assigns the result of
// a method call, with its type when that is a type of the module.
func (sc handlerScope) findResults() {
	ast.Inspect(sc.fn.Body, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Rhs) != 1 || len(as.Lhs) == 0 {
			return true
		}
		call, ok := as.Rhs[0].(*ast.CallExpr)
		id, isIdent := as.Lhs[0].(*ast.Ident)
		if !ok || !isIdent {
			return true
		}
		if t, err := sc.typeOf(call); err == nil {
			sc.results[id.Name] = t
		} else {
			sc.untyped[id.Name] = err
		}
		return true
	})
}

func (sc handlerScope) typeOf(expr ast.Expr) (typeName, error) {
	switch e := expr.(type) {
	case *ast.Ident:
		if e.Name == sc.recv {
			return sc.self, nil
		}
		if t, ok := sc.results[e.Name]; ok {
			return t, nil
		}
	case *ast.SelectorExpr:
		x, err := sc.typeOf(e.X)
		if err != nil {
			return typeName{}, err
		}
		return sc.l.fieldType(x, e.Sel.Name)
	case *ast.CallExpr:
		sel, ok := e.Fun.(*ast.SelectorExpr)
		if !ok {
			break
		}
		x, err := sc.typeOf(sel.X)
		if err != nil {
			return typeName{}, err
		}
		return sc.l.methodResult(x, sel.Sel.Name)
	}
	return typeName{}, fmt.Errorf("cannot tell the type of %s", exprText(expr))
}

// walk adds the fields of the struct literal lit, with prefix before their
// JSON names, and checks each result field that fills one.
func (sc handlerScope) walk(lit *ast.CompositeLit, prefix string, r *replyFields) error {
	st, err := sc.structOf(lit.Type)
	if err != nil {
		return err
	}
	fields, err := jsonFields(st)
	if err != nil {
		return err
	}
	// encoding/json writes the fields in the order of the struct, whatever
	// the order of the literal.
	values := make([]ast.Expr, len(fields))
	for i, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			if len(lit.Elts) != len(fields) {
				return fmt.Errorf("the reply sets %d of the %d fields of its struct by position", len(lit.Elts), len(fields))
			}
			values[i] = elt
			continue
		}
		key, _ := kv.Key.(*ast.Ident)
		idx := slices.IndexFunc(fields, func(f jsonField) bool { return key != nil && f.goName == key.Name })
		if idx < 0 {
			return fmt.Errorf("the reply sets the unknown field %s", exprText(kv.Key))
		}
		values[idx] = kv.Value
	}
	for i, f := range fields {
		if f.json == "-" {
			continue
		}
		if inner, ok := values[i].(*ast.CompositeLit); ok {
			if err := sc.walk(inner, prefix+f.json+".", r); err != nil {
				return err
			}
			continue
		}
		r.fields = append(r.fields, prefix+f.json)
		if values[i] != nil {
			sc.checkSources(values[i], prefix+f.json, r)
		}
	}
	return nil
}

// checkSources records the result type of each result field that value
// reads, and a problem for one without a doc comment.
func (sc handlerScope) checkSources(value ast.Expr, field string, r *replyFields) {
	ast.Inspect(value, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		id, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}
		if err, ok := sc.untyped[id.Name]; ok {
			r.problems = append(r.problems, fmt.Sprintf("the reply field %s reads %s: %v", field, id.Name, err))
			return false
		}
		t, ok := sc.results[id.Name]
		if !ok {
			return true
		}
		if !slices.Contains(r.docs, t) {
			r.docs = append(r.docs, t)
		}
		if documented, err := documented(t, sel.Sel.Name); err != nil {
			r.problems = append(r.problems, err.Error())
		} else if !documented {
			r.problems = append(r.problems, fmt.Sprintf("the reply field %s comes from %s.%s, which has no doc comment", field, t, sel.Sel.Name))
		}
		return false
	})
}

// structOf returns the struct type of a composite literal: a struct type
// written in place, or one that the handler or its package declares.
func (sc handlerScope) structOf(expr ast.Expr) (*ast.StructType, error) {
	switch e := expr.(type) {
	case *ast.StructType:
		return e, nil
	case *ast.Ident:
		var local *ast.StructType
		ast.Inspect(sc.fn.Body, func(n ast.Node) bool {
			if spec, ok := n.(*ast.TypeSpec); ok && spec.Name.Name == e.Name {
				local, _ = spec.Type.(*ast.StructType)
			}
			return local == nil
		})
		if local != nil {
			return local, nil
		}
		if decl, ok := sc.pkg.types[e.Name]; ok {
			if st, ok := decl.spec.Type.(*ast.StructType); ok {
				return st, nil
			}
		}
	}
	return nil, fmt.Errorf("the reply holds a %s, which is not a struct of the handler or its package", exprText(expr))
}

type jsonField struct {
	goName, json string
}

// jsonFields returns the fields of st in order, with the names that
// encoding/json gives them.
func jsonFields(st *ast.StructType) ([]jsonField, error) {
	var fields []jsonField
	for _, f := range st.Fields.List {
		if len(f.Names) == 0 {
			return nil, fmt.Errorf("the reply embeds %s, which the table cannot list", exprText(f.Type))
		}
		tag := ""
		if f.Tag != nil {
			raw, err := strconv.Unquote(f.Tag.Value)
			if err != nil {
				return nil, err
			}
			tag, _, _ = strings.Cut(reflect.StructTag(raw).Get("json"), ",")
		}
		for _, name := range f.Names {
			fields = append(fields, jsonField{goName: name.Name, json: cmp.Or(tag, name.Name)})
		}
	}
	if len(fields) == 0 {
		return nil, errors.New("the reply has no fields")
	}
	return fields, nil
}

// exprText prints an expression for an error.
func exprText(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.SelectorExpr:
		return exprText(e.X) + "." + e.Sel.Name
	case *ast.StarExpr:
		return "*" + exprText(e.X)
	case *ast.CallExpr:
		return exprText(e.Fun) + "(...)"
	default:
		return fmt.Sprintf("%T", expr)
	}
}

// typeName is a type that a package of the module declares.
type typeName struct {
	pkg  *loadedPackage
	name string
}

func (t typeName) String() string { return t.pkg.name + "." + t.name }

// loadedPackage indexes the declarations of a package of the module.
type loadedPackage struct {
	*goPackage
	name    string
	types   map[string]typeDecl
	methods map[string]map[string]funcDecl
}

type typeDecl struct {
	spec *ast.TypeSpec
	file *ast.File
}

type funcDecl struct {
	decl *ast.FuncDecl
	file *ast.File
}

// loader loads the packages of the module that a reply's types lead to, by
// directory.
type loader struct {
	fsys fs.FS
	pkgs map[string]*loadedPackage
}

func (l *loader) load(dir string) (*loadedPackage, error) {
	if p, ok := l.pkgs[dir]; ok {
		return p, nil
	}
	gp, err := parsePackage(l.fsys, dir)
	if err != nil {
		return nil, err
	}
	p := &loadedPackage{goPackage: gp, types: map[string]typeDecl{}, methods: map[string]map[string]funcDecl{}}
	for _, f := range gp.files {
		p.name = f.Name.Name
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					if ts, ok := spec.(*ast.TypeSpec); ok {
						p.types[ts.Name.Name] = typeDecl{spec: ts, file: f}
					}
				}
			case *ast.FuncDecl:
				if recv := receiverType(d); recv != "" {
					if p.methods[recv] == nil {
						p.methods[recv] = map[string]funcDecl{}
					}
					p.methods[recv][d.Name.Name] = funcDecl{decl: d, file: f}
				}
			}
		}
	}
	l.pkgs[dir] = p
	return p, nil
}

func receiverType(d *ast.FuncDecl) string {
	if d.Recv == nil || len(d.Recv.List) == 0 {
		return ""
	}
	t := d.Recv.List[0].Type
	if star, ok := t.(*ast.StarExpr); ok {
		t = star.X
	}
	if id, ok := t.(*ast.Ident); ok {
		return id.Name
	}
	return ""
}

// resolve returns the named type that expr, written in file of pkg, names.
func (l *loader) resolve(pkg *loadedPackage, file *ast.File, expr ast.Expr) (typeName, error) {
	switch e := expr.(type) {
	case *ast.StarExpr:
		return l.resolve(pkg, file, e.X)
	case *ast.Ident:
		if _, ok := pkg.types[e.Name]; ok {
			return typeName{pkg: pkg, name: e.Name}, nil
		}
	case *ast.SelectorExpr:
		x, ok := e.X.(*ast.Ident)
		if !ok {
			break
		}
		for _, imp := range file.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			name := path.Base(p)
			if imp.Name != nil {
				name = imp.Name.Name
			}
			dir, inModule := strings.CutPrefix(p, module+"/")
			if name != x.Name || !inModule {
				continue
			}
			other, err := l.load(dir)
			if err != nil {
				return typeName{}, err
			}
			if _, ok := other.types[e.Sel.Name]; ok {
				return typeName{pkg: other, name: e.Sel.Name}, nil
			}
		}
	}
	return typeName{}, fmt.Errorf("%s is not a type that a package of the module declares", exprText(expr))
}

func (l *loader) fieldType(t typeName, field string) (typeName, error) {
	decl := t.pkg.types[t.name]
	if st, ok := decl.spec.Type.(*ast.StructType); ok {
		for _, f := range st.Fields.List {
			for _, name := range f.Names {
				if name.Name == field {
					return l.resolve(t.pkg, decl.file, f.Type)
				}
			}
		}
	}
	return typeName{}, fmt.Errorf("%s has no field %s", t, field)
}

func (l *loader) methodResult(t typeName, method string) (typeName, error) {
	m, ok := t.pkg.methods[t.name][method]
	if !ok || m.decl.Type.Results == nil || len(m.decl.Type.Results.List) == 0 {
		return typeName{}, fmt.Errorf("%s has no method %s with a result", t, method)
	}
	return l.resolve(t.pkg, m.file, m.decl.Type.Results.List[0].Type)
}

// documented reports whether the field of struct t has a doc comment: its
// own, or the one above the group of fields it belongs to, with no blank
// line between them, when that comment names it.
func documented(t typeName, field string) (bool, error) {
	st, ok := t.pkg.types[t.name].spec.Type.(*ast.StructType)
	if !ok {
		return false, fmt.Errorf("%s is not a struct", t)
	}
	line := func(p token.Pos) int { return t.pkg.fset.Position(p).Line }
	groupDoc, prevEnd := "", -1
	for _, f := range st.Fields.List {
		switch {
		case f.Doc != nil:
			groupDoc = f.Doc.Text()
		case line(f.Pos()) != prevEnd+1:
			groupDoc = ""
		}
		prevEnd = line(f.End())
		for _, name := range f.Names {
			if name.Name != field {
				continue
			}
			if f.Doc != nil || f.Comment != nil {
				return true, nil
			}
			return regexp.MustCompile(`\b` + regexp.QuoteMeta(field) + `\b`).MatchString(groupDoc), nil
		}
	}
	return false, fmt.Errorf("%s has no field %s", t, field)
}

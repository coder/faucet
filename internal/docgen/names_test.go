package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
)

// problem is something a doc names that the repository does not have.
type problem struct {
	file string
	line int
	name string
	msg  string
}

func (p problem) String() string { return fmt.Sprintf("%s:%d: %s %s", p.file, p.line, p.name, p.msg) }

// namedDocs are the files whose names must exist. A file that does not exist
// is skipped.
var namedDocs = []string{"README.md", "docs/*.md", "docs/diagrams/source.html"}

// exemptPaths are paths that the docs name and that faucet does not serve.
var exemptPaths = []string{
	"/user",                 // GitHub API: the token check in docs/security.md.
	"/repos/{owner}/{name}", // GitHub API: the repository check in docs/security.md.
}

var (
	faucetNameRE = regexp.MustCompile(`FAUCET_[A-Z0-9_]+\*?`)
	methodPathRE = regexp.MustCompile(`\b(GET|POST|PUT|PATCH|DELETE|HEAD) +(/[A-Za-z0-9_.~{}/*-]*)`)
	codeSpanRE   = regexp.MustCompile("`[^`]*`")
	pathSpanRE   = regexp.MustCompile(`^/[A-Za-z0-9_.~{}/-]*\*?$`)
	fenceRE      = regexp.MustCompile("^\\s*(```|~~~)")
	mdLinkRE     = regexp.MustCompile(`\]\(([^)\s]+)(?:\s+"[^"]*")?\)`)
	htmlLinkRE   = regexp.MustCompile(`\b(src|srcset|href)\s*=\s*(?:"([^"]*)"|'([^']*)')`)
	schemeRE     = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9+.-]*:|//)`)
	headingRE    = regexp.MustCompile(`^#{1,6}\s+(.*?)(?:\s+#+)?\s*$`)
)

type checker struct {
	fsys     fs.FS
	literals map[string]bool
	routes   []route
}

// checkNames reports every FAUCET_ variable, route and relative link in the
// docs that the repository does not have.
func checkNames(fsys fs.FS) ([]problem, error) {
	literals, err := stringLiterals(fsys)
	if err != nil {
		return nil, err
	}
	routes, err := servedRoutes(fsys)
	if err != nil {
		return nil, err
	}
	files, err := glob(fsys, namedDocs)
	if err != nil {
		return nil, err
	}
	c := checker{fsys: fsys, literals: literals, routes: routes}
	var problems []problem
	for _, file := range files {
		content, err := fs.ReadFile(fsys, file)
		if err != nil {
			return nil, err
		}
		problems = append(problems, c.check(file, string(content))...)
	}
	return problems, nil
}

func (c checker) check(file, content string) []problem {
	var problems []problem
	fence := ""
	for i, line := range strings.Split(content, "\n") {
		report := func(name, msg string) {
			problems = append(problems, problem{file: file, line: i + 1, name: name, msg: msg})
		}
		for _, name := range faucetNameRE.FindAllString(line, -1) {
			if !c.isLiteral(name) {
				report(name, "is not a string literal of the Go code")
			}
		}
		for _, m := range methodPathRE.FindAllStringSubmatch(line, -1) {
			if p := strings.TrimRight(m[2], "."); !c.serves(m[1], p) {
				report(m[1]+" "+p, "is not a route that cmd/faucet registers")
			}
		}
		var marks bool
		if fence, marks = fenceAfter(fence, line); marks || fence != "" {
			continue
		}
		for _, span := range codeSpanRE.FindAllString(line, -1) {
			if p := strings.Trim(span, "`"); pathSpanRE.MatchString(p) && !c.serves("", p) {
				report(p, "is not a route that cmd/faucet registers")
			}
		}
		for _, target := range links(codeSpanRE.ReplaceAllString(line, "")) {
			if msg := c.resolve(file, target); msg != "" {
				report(target, msg)
			}
		}
	}
	return problems
}

func (c checker) isLiteral(name string) bool {
	prefix, wild := strings.CutSuffix(name, "*")
	if !wild {
		return c.literals[name]
	}
	for lit := range c.literals {
		if strings.HasPrefix(lit, prefix) && faucetNameRE.FindString(lit) == lit {
			return true
		}
	}
	return false
}

// serves reports whether cmd/faucet registers p for method, or for every
// method when method is empty. A p that ends in * names any route under it.
func (c checker) serves(method, p string) bool {
	if slices.Contains(exemptPaths, p) {
		return true
	}
	prefix, wild := strings.CutSuffix(p, "*")
	for _, r := range c.routes {
		if method != "" && r.method != "" && r.method != method {
			continue
		}
		if r.path == p || wild && strings.HasPrefix(r.path, prefix) {
			return true
		}
	}
	return false
}

func links(line string) []string {
	var targets []string
	for _, m := range mdLinkRE.FindAllStringSubmatch(line, -1) {
		targets = append(targets, m[1])
	}
	for _, m := range htmlLinkRE.FindAllStringSubmatch(line, -1) {
		value := m[2] + m[3]
		if m[1] != "srcset" {
			targets = append(targets, value)
			continue
		}
		for _, candidate := range strings.Split(value, ",") {
			if fields := strings.Fields(candidate); len(fields) > 0 {
				targets = append(targets, fields[0])
			}
		}
	}
	return targets
}

// resolve returns why target, linked from file, does not resolve, or "".
func (c checker) resolve(file, target string) string {
	if schemeRE.MatchString(target) {
		return ""
	}
	ref, anchor, _ := strings.Cut(target, "#")
	ref, _, _ = strings.Cut(ref, "?")
	ref, err := url.PathUnescape(ref)
	if err != nil {
		return "is not a valid path"
	}
	dest := file
	switch {
	case strings.HasPrefix(ref, "/"):
		dest = path.Clean(strings.TrimPrefix(ref, "/"))
	case ref != "":
		dest = path.Join(path.Dir(file), ref)
	}
	if _, err := fs.Stat(c.fsys, dest); err != nil {
		return "names no file or directory of the repository"
	}
	if anchor == "" || path.Ext(dest) != ".md" {
		return ""
	}
	content, err := fs.ReadFile(c.fsys, dest)
	if err != nil {
		return "cannot be read"
	}
	if !slices.Contains(headingSlugs(string(content)), anchor) {
		return "names no heading of " + dest
	}
	return ""
}

// fenceAfter returns the code fence that is open after line, given the one
// open before it, and whether line opens or closes a fence.
func fenceAfter(fence, line string) (string, bool) {
	f := fenceRE.FindStringSubmatch(line)
	switch {
	case f == nil:
		return fence, false
	case fence == "":
		return f[1], true
	case f[1] == fence:
		return "", true
	default:
		return fence, false
	}
}

// headingSlugs returns the anchors that GitHub gives the headings of a
// markdown file.
func headingSlugs(content string) []string {
	var slugs []string
	seen := map[string]int{}
	fence := ""
	for _, line := range strings.Split(content, "\n") {
		var marks bool
		if fence, marks = fenceAfter(fence, line); marks || fence != "" {
			continue
		}
		m := headingRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		s := slug(m[1])
		if n := seen[s]; n > 0 {
			slugs = append(slugs, fmt.Sprintf("%s-%d", s, n))
		} else {
			slugs = append(slugs, s)
		}
		seen[s]++
	}
	return slugs
}

func slug(heading string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case unicode.IsLetter(r), unicode.IsDigit(r), r == '-', r == '_':
			return r
		case r == ' ':
			return '-'
		default:
			return -1
		}
	}, strings.ToLower(heading))
}

// skippedDirs hold Go code outside the root module, or code that names
// variables only to describe the docs.
var skippedDirs = []string{"verify", "tools", "internal/docgen"}

func stringLiterals(fsys fs.FS) (map[string]bool, error) {
	literals := map[string]bool{}
	fset := token.NewFileSet()
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case d.IsDir() && (d.Name() == "node_modules" || d.Name() == ".git" || slices.Contains(skippedDirs, p)):
			return fs.SkipDir
		case d.IsDir() || path.Ext(p) != ".go":
			return nil
		}
		src, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		f, err := parser.ParseFile(fset, p, src, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if v, ok := stringLit(n); ok {
				literals[v] = true
			}
			return true
		})
		return nil
	})
	return literals, err
}

func stringLit(n ast.Node) (string, bool) {
	lit, ok := n.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	v, err := strconv.Unquote(lit.Value)
	return v, err == nil
}

// route is a path that cmd/faucet registers. An empty method serves every
// method.
type route struct {
	method string
	path   string
}

// routeMethods maps the chi router methods that register a route to the HTTP
// method they serve. Method takes the HTTP method as its first argument.
var routeMethods = map[string]string{
	"Get": "GET", "Post": "POST", "Put": "PUT", "Patch": "PATCH", "Delete": "DELETE", "Head": "HEAD",
	"Handle": "", "HandleFunc": "", "Method": "",
}

func servedRoutes(fsys fs.FS) ([]route, error) {
	files, err := goFiles(fsys, "cmd/faucet")
	if err != nil {
		return nil, err
	}
	var routes []route
	var unread []string
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			method, ok := routeMethods[sel.Sel.Name]
			args := call.Args
			if !ok || len(args) == 0 {
				return true
			}
			if sel.Sel.Name == "Method" {
				if len(args) < 2 {
					return true
				}
				method, args = httpMethod(args[0]), args[1:]
			}
			if p, ok := stringLit(args[0]); ok && strings.HasPrefix(p, "/") {
				if method == "" && sel.Sel.Name == "Method" {
					unread = append(unread, p)
				}
				routes = append(routes, route{method: method, path: p})
			}
			return true
		})
	}
	if len(unread) > 0 {
		return nil, fmt.Errorf("cannot read the HTTP method of the routes %s in cmd/faucet", strings.Join(unread, ", "))
	}
	return routes, nil
}

// httpMethod reads a method written as a string or as an http.Method*
// constant, or returns "".
func httpMethod(e ast.Expr) string {
	if v, ok := stringLit(e); ok {
		return v
	}
	if sel, ok := e.(*ast.SelectorExpr); ok {
		if name, ok := strings.CutPrefix(sel.Sel.Name, "Method"); ok {
			return strings.ToUpper(name)
		}
	}
	return ""
}

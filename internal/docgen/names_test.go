package main

import (
	"fmt"
	"io/fs"
	"maps"
	"net/url"
	"path"
	"regexp"
	"slices"
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

// exemptVariables are environment variables that the docs name and that no
// code of faucet reads.
var exemptVariables = []string{
	"POSTGRES_*", // docs/deploy.md: Neon's Vercel integration sets them next to DATABASE_URL.
	"PG*",        // docs/deploy.md: the same integration sets libpq's variables too.
}

var (
	faucetNameRE = regexp.MustCompile(`FAUCET_[A-Z0-9_]+\*?`)
	// envSpanRE finds a name with an underscore inside a code span, such as
	// `Bearer $CRON_SECRET`, and wordSpanRE a code span that is one
	// upper-case word, such as `PORT`.
	envSpanRE  = regexp.MustCompile(`\b[A-Z][A-Z0-9]*(?:_[A-Z0-9]*)+(?:\*|\b)`)
	wordSpanRE = regexp.MustCompile(`^\$?([A-Z][A-Z0-9]+\*?)$`)
	// envAssignRE finds a variable that a shell line in a code fence sets.
	envAssignRE = regexp.MustCompile(`^\s*([A-Z][A-Z0-9_]*)=`)
	// placeholderURLRE finds a URL whose host is a placeholder, such as
	// https://<your host>/mcp, and its path.
	placeholderURLRE = regexp.MustCompile("https?://<[^>]+>(/[^\\s`)\"'<>?#]*)?")
	methodPathRE     = regexp.MustCompile(`\b(GET|POST|PUT|PATCH|DELETE|HEAD) +(/[A-Za-z0-9_.~{}/*-]*)`)
	codeSpanRE       = regexp.MustCompile("`[^`]*`")
	pathSpanRE       = regexp.MustCompile(`^/[A-Za-z0-9_.~{}/-]*\*?$`)
	fenceRE          = regexp.MustCompile("^\\s*(```|~~~)")
	mdLinkRE         = regexp.MustCompile(`\]\(([^)\s]+)(?:\s+"[^"]*")?\)`)
	htmlLinkRE       = regexp.MustCompile(`\b(src|srcset|href)\s*=\s*(?:"([^"]*)"|'([^']*)')`)
	schemeRE         = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9+.-]*:|//)`)
	headingRE        = regexp.MustCompile(`^#{1,6}\s+(.*?)(?:\s+#+)?\s*$`)
)

type checker struct {
	fsys fs.FS
	// vars are the environment variables that some non-test file of the
	// module reads.
	vars   []string
	routes []route
}

// checkNames reports every environment variable, route and relative link in
// the docs that the repository does not have.
func checkNames(fsys fs.FS) ([]problem, error) {
	byDir, err := envReads(fsys)
	if err != nil {
		return nil, err
	}
	var reads []string
	for _, names := range byDir {
		reads = append(reads, names...)
	}
	cmd, err := parsePackage(fsys, "cmd/faucet")
	if err != nil {
		return nil, err
	}
	routes, err := servedRoutes(cmd)
	if err != nil {
		return nil, err
	}
	files, err := glob(fsys, namedDocs)
	if err != nil {
		return nil, err
	}
	c := checker{fsys: fsys, vars: reads, routes: routes}
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
		for _, m := range methodPathRE.FindAllStringSubmatch(line, -1) {
			if p := strings.TrimRight(m[2], "."); !c.serves(m[1], p) {
				report(m[1]+" "+p, "is not a route that cmd/faucet registers")
			}
		}
		for _, m := range placeholderURLRE.FindAllStringSubmatch(line, -1) {
			if p := strings.TrimRight(m[1], ".,:;"); p != "" && !c.serves("", p) {
				report(strings.TrimRight(m[0], ".,:;"), "has a path that is not a route that cmd/faucet registers")
			}
		}
		var marks bool
		fence, marks = fenceAfter(fence, line)
		inCode := marks || fence != ""
		names := faucetNameRE.FindAllString(line, -1)
		if inCode {
			names = append(names, assignedVariables(line)...)
		} else {
			names = append(names, spanVariables(line)...)
		}
		slices.Sort(names)
		for _, name := range slices.Compact(names) {
			if !c.reads(name) {
				report(name, "is not an environment variable that a non-test file of the module reads")
			}
		}
		if inCode {
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

// spanVariables returns the names in the code spans of a line of prose
// that look like environment variables.
func spanVariables(line string) []string {
	var names []string
	for _, span := range codeSpanRE.FindAllString(line, -1) {
		span = strings.Trim(span, "`")
		names = append(names, envSpanRE.FindAllString(span, -1)...)
		if m := wordSpanRE.FindStringSubmatch(span); len(m) > 1 && !slices.Contains(slices.Collect(maps.Values(routeMethods)), m[1]) {
			names = append(names, m[1])
		}
	}
	return names
}

// assignedVariables returns the variable that a shell line of a code fence
// sets.
func assignedVariables(line string) []string {
	if m := envAssignRE.FindStringSubmatch(line); len(m) > 1 {
		return m[1:2]
	}
	return nil
}

// reads reports whether the module reads the variable name, or for a name
// that ends in *, a variable that starts with the rest.
func (c checker) reads(name string) bool {
	if slices.Contains(exemptVariables, name) {
		return true
	}
	prefix, wild := strings.CutSuffix(name, "*")
	return slices.ContainsFunc(c.vars, func(read string) bool { return read == name || wild && strings.HasPrefix(read, prefix) })
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

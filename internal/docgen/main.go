// Command docgen rewrites the tables of README.md and docs/*.md that the code
// defines. Each table sits between <!-- BEGIN generated:NAME --> and
// <!-- END generated:NAME -->. Run it from anywhere inside the repository:
//
//	go run ./internal/docgen
package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/coder/superblink-events/faucet"
)

const module = "github.com/coder/superblink-events"

func main() {
	if err := run(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "docgen:", err)
		os.Exit(1)
	}
}

func run() error {
	wd, err := os.Getwd()
	if err != nil {
		return err
	}
	root, err := findRoot(wd)
	if err != nil {
		return err
	}
	updates, err := repoSource(root).regenerate()
	if err != nil {
		return err
	}
	for _, u := range updates {
		p := filepath.Join(root, filepath.FromSlash(u.file))
		info, err := os.Stat(p)
		if err != nil {
			return err
		}
		if err := os.WriteFile(p, u.content, info.Mode().Perm()); err != nil {
			return err
		}
		_, _ = fmt.Printf("%s: rewrote %s\n", u.file, strings.Join(u.blocks, ", "))
	}
	return nil
}

// findRoot walks up from dir to the directory whose go.mod declares the root
// module. tools/ has a go.mod of its own, so the first go.mod is not enough.
func findRoot(dir string) (string, error) {
	for {
		if data, err := os.ReadFile(filepath.Join(dir, "go.mod")); err == nil && modulePath(data) == module {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no go.mod of module %s above the working directory", module)
		}
		dir = parent
	}
}

func modulePath(gomod []byte) string {
	sc := bufio.NewScanner(bytes.NewReader(gomod))
	for sc.Scan() {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(sc.Text()), "module "); ok {
			return strings.Trim(strings.TrimSpace(rest), `"`)
		}
	}
	return ""
}

// source is what the tables are made from: the repository tree, the event
// names and the registries.
type source struct {
	fsys fs.FS
	// tables are the blocks to render, by name. Each must have a block.
	tables    map[string]table
	events    []faucet.EventName
	settings  []setting
	giveUps   map[string]string
	callbacks map[string]callbackReason
}

// table renders a generated block, header rows included.
type table func(source) ([]string, error)

func repoSource(root string) source {
	return source{
		fsys: os.DirFS(root), tables: tables, events: faucet.EventNames(), settings: settings,
		giveUps: giveUpMeanings, callbacks: callbackReasons,
	}
}

// update is a doc whose generated blocks differ from what the code gives.
type update struct {
	file    string
	content []byte
	// blocks are the names of the blocks that changed.
	blocks []string
}

// generatedDocs are the files that may hold generated blocks.
var generatedDocs = []string{"README.md", "docs/*.md"}

// regenerate renders every generated block and returns the docs that change.
// It writes nothing, so an error in one doc leaves every file as it was. It
// fails when a table has no block, so removing a block from a doc cannot
// drop the table unseen.
func (s source) regenerate() ([]update, error) {
	files, err := glob(s.fsys, generatedDocs)
	if err != nil {
		return nil, err
	}
	var updates []update
	seen := map[string]bool{}
	for _, file := range files {
		old, err := fs.ReadFile(s.fsys, file)
		if err != nil {
			return nil, err
		}
		content, blocks, err := s.render(file, old, seen)
		if err != nil {
			return nil, err
		}
		if len(blocks) > 0 {
			updates = append(updates, update{file: file, content: content, blocks: blocks})
		}
	}
	var missing []string
	for name := range s.tables {
		if !seen[name] {
			missing = append(missing, "generated:"+name)
		}
	}
	if len(missing) > 0 {
		slices.Sort(missing)
		return nil, fmt.Errorf("%s has no block in %s", strings.Join(missing, ", "), strings.Join(generatedDocs, " or "))
	}
	return updates, nil
}

func glob(fsys fs.FS, patterns []string) ([]string, error) {
	var files []string
	for _, pattern := range patterns {
		matches, err := fs.Glob(fsys, pattern)
		if err != nil {
			return nil, err
		}
		files = append(files, matches...)
	}
	slices.Sort(files)
	return files, nil
}

var markerRE = regexp.MustCompile(`^<!-- (BEGIN|END) generated:(\S+) -->$`)

func marker(kind, name string) string { return "<!-- " + kind + " generated:" + name + " -->" }

// render returns content with every generated block rewritten, from its BEGIN
// line to its END line, and the names of the blocks whose lines changed. It
// adds the name of each block to seen.
func (s source) render(file string, content []byte, seen map[string]bool) ([]byte, []string, error) {
	lines := strings.Split(string(content), "\n")
	var out, changed []string
	inFile := map[string]bool{}
	open, openName := -1, ""
	for i, line := range lines {
		m := markerRE.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil {
			if open < 0 {
				out = append(out, line)
			}
			continue
		}
		kind, name := m[1], m[2]
		at := fmt.Sprintf("%s:%d", file, i+1)
		switch {
		case kind == "BEGIN" && open >= 0:
			return nil, nil, fmt.Errorf("%s: generated:%s begins inside generated:%s", at, name, openName)
		case kind == "BEGIN" && s.tables[name] == nil:
			return nil, nil, fmt.Errorf("%s: no generated block is named %q", at, name)
		case kind == "BEGIN" && inFile[name]:
			return nil, nil, fmt.Errorf("%s: generated:%s appears a second time", at, name)
		case kind == "BEGIN":
			inFile[name], seen[name] = true, true
			open, openName = i, name
		case open < 0 || name != openName:
			return nil, nil, fmt.Errorf("%s: END generated:%s has no BEGIN", at, name)
		default:
			rows, err := s.tables[name](s)
			if err != nil {
				return nil, nil, fmt.Errorf("%s: generated:%s: %w", at, name, err)
			}
			block := slices.Concat([]string{marker("BEGIN", name)}, rows, []string{marker("END", name)})
			if !slices.Equal(block, lines[open:i+1]) {
				changed = append(changed, name)
			}
			out = append(out, block...)
			open = -1
		}
	}
	if open >= 0 {
		return nil, nil, fmt.Errorf("%s:%d: BEGIN generated:%s has no END", file, open+1, openName)
	}
	return []byte(strings.Join(out, "\n")), changed, nil
}

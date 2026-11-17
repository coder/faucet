package faucet_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// These tests keep each feature under faucet/ in its own compartment, with a
// small, one-way interface to core. A package's role comes from its path:
// faucet is core, faucet/internal/... holds helpers, the adapters below sit
// between core and the features, and every other directory under faucet/ is
// a feature.
const (
	module = "github.com/coder/faucet"
	// maxCoreNames is how many distinct core identifiers the non-test code of
	// one feature may name.
	maxCoreNames = 5
)

// adapters is the one list that is kept by hand. A directory missing from it
// is a feature, so a new adapter fails closed.
var adapters = []string{"faucet/pgstore", "faucet/storetest"}

type role int

const (
	roleCore role = iota
	roleInternal
	roleAdapter
	roleFeature
	roleOther
)

type goImport struct {
	path string
	// local is the name that the file gives the import, empty for the
	// package's own name.
	local string
	// names are the identifiers that the file selects on the import.
	names []string
}

type goFile struct {
	name    string
	imports []goImport
}

func (f goFile) test() bool { return strings.HasSuffix(f.name, "_test.go") }

type goPackage struct {
	// dir is relative to the module root and slash separated.
	dir   string
	files []goFile
}

// roleOf returns the role of a module-relative directory, and the name of the
// feature that it belongs to.
func roleOf(dir string, adapters []string) (role, string) {
	switch {
	case dir == "faucet":
		return roleCore, ""
	case dir == "faucet/internal" || strings.HasPrefix(dir, "faucet/internal/"):
		return roleInternal, ""
	}
	for _, a := range adapters {
		if dir == a || strings.HasPrefix(dir, a+"/") {
			return roleAdapter, ""
		}
	}
	if rest, ok := strings.CutPrefix(dir, "faucet/"); ok {
		feature, _, _ := strings.Cut(rest, "/")
		return roleFeature, feature
	}
	return roleOther, ""
}

// moduleDir returns the module-relative directory of an import path, or false
// for a path outside the module.
func moduleDir(path string) (string, bool) {
	if path == module {
		return "", true
	}
	return strings.CutPrefix(path, module+"/")
}

// compartmentViolations checks the layout against the rules and returns one
// line per violation.
func compartmentViolations(pkgs []goPackage, adapters []string) []string {
	var out []string
	add := func(format string, args ...any) { out = append(out, fmt.Sprintf(format, args...)) }

	dirs := map[string]bool{}
	features := map[string]bool{}
	for _, p := range pkgs {
		dirs[p.dir] = true
		if r, feature := roleOf(p.dir, adapters); r == roleFeature {
			features[feature] = true
		}
	}
	if len(features) == 0 {
		add("self-check: the walk found no feature under faucet/")
	}
	for _, a := range adapters {
		if !dirs[a] {
			add("self-check: the adapter %s does not exist", a)
		}
	}

	coreNames := map[string]map[string]bool{}
	for _, p := range pkgs {
		r, feature := roleOf(p.dir, adapters)
		for _, f := range p.files {
			where := p.dir + "/" + f.name
			for _, imp := range f.imports {
				target, ok := moduleDir(imp.path)
				if !ok {
					continue
				}
				if imp.local == "." {
					add("R5: %s dot-imports %s", where, imp.path)
				}
				tr, tfeature := roleOf(target, adapters)
				switch r {
				case roleCore:
					if !f.test() && tr != roleInternal {
						add("R1: %s imports %s: core imports only faucet/internal", where, imp.path)
					}
					if f.test() && (tr == roleFeature || tr == roleOther) {
						add("R1: %s imports %s: a core test imports no feature", where, imp.path)
					}
				case roleInternal:
					if target != p.dir || !f.test() {
						add("R2: %s imports %s: faucet/internal imports nothing else from the module", where, imp.path)
					}
				case roleFeature:
					allowed := tr == roleCore || tr == roleInternal || (tr == roleFeature && tfeature == feature) || (tr == roleAdapter && f.test())
					if !allowed {
						add("R3: %s imports %s: a feature imports only core, faucet/internal and itself, and its tests also the adapters", where, imp.path)
					}
					if tr == roleCore && !f.test() {
						if coreNames[feature] == nil {
							coreNames[feature] = map[string]bool{}
						}
						for _, name := range imp.names {
							coreNames[feature][name] = true
						}
					}
				case roleAdapter, roleOther:
				}
			}
			if r == roleCore {
				for feature := range features {
					// The feature's name less its last letter, so that the verb
					// (redeliver) counts as well as the noun (redelivery).
					if stem := feature[:len(feature)-1]; strings.HasPrefix(f.name, stem) {
						add("R6: %s is named like the feature %s: its code belongs in faucet/%s", where, feature, feature)
					}
				}
			}
		}
	}
	for feature, names := range coreNames {
		sorted := slices.Sorted(maps.Keys(names))
		if len(sorted) > maxCoreNames {
			add("R4: faucet/%s names %d core identifiers, more than %d: %s", feature, len(sorted), maxCoreNames, strings.Join(sorted, ", "))
		}
		for _, name := range []string{"Service", "Deps"} {
			if names[name] {
				add("R4: faucet/%s names faucet.%s", feature, name)
			}
		}
	}
	slices.Sort(out)
	return out
}

// parseLayout reads every Go file under the faucet directory, whatever its
// build tags, with the identifiers that each file selects on its imports.
func parseLayout(t *testing.T) []goPackage {
	t.Helper()
	byDir := map[string]*goPackage{}
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "testdata" || (path != "." && strings.HasPrefix(d.Name(), ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		dir := filepath.ToSlash(filepath.Join("faucet", filepath.Dir(path)))
		if byDir[dir] == nil {
			byDir[dir] = &goPackage{dir: dir}
		}
		byDir[dir].files = append(byDir[dir].files, goFile{name: d.Name(), imports: importsOf(file)})
		return nil
	})
	require.NoError(t, err)
	var pkgs []goPackage
	for _, p := range byDir {
		pkgs = append(pkgs, *p)
	}
	return pkgs
}

func importsOf(file *ast.File) []goImport {
	imports := make([]goImport, 0, len(file.Imports))
	byLocal := map[string]int{}
	for _, spec := range file.Imports {
		path, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			continue
		}
		imp := goImport{path: path}
		local := filepath.Base(path)
		if spec.Name != nil {
			imp.local, local = spec.Name.Name, spec.Name.Name
		}
		byLocal[local] = len(imports)
		imports = append(imports, imp)
	}
	seen := map[[2]string]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		x, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}
		if i, ok := byLocal[x.Name]; ok && !seen[[2]string{x.Name, sel.Sel.Name}] {
			seen[[2]string{x.Name, sel.Sel.Name}] = true
			imports[i].names = append(imports[i].names, sel.Sel.Name)
		}
		return true
	})
	return imports
}

func TestCompartments(t *testing.T) {
	t.Parallel()
	violations := compartmentViolations(parseLayout(t), adapters)
	require.Empty(t, violations, "move the code instead of widening the rule:\n%s", strings.Join(violations, "\n"))
}

func TestCompartmentRulesRejectEachBreach(t *testing.T) {
	t.Parallel()

	const (
		core     = module + "/faucet"
		helper   = module + "/faucet/internal/githubapi"
		pgtest   = module + "/faucet/internal/pgtest"
		pgstore  = module + "/faucet/pgstore"
		storet   = module + "/faucet/storetest"
		feature  = module + "/faucet/redelivery"
		other    = module + "/faucet/other"
		cmd      = module + "/cmd/faucet"
		external = "golang.org/x/xerrors"
	)
	fileOf := func(name string, imports ...goImport) goFile { return goFile{name: name, imports: imports} }
	use := func(path string, names ...string) goImport { return goImport{path: path, names: names} }
	valid := func() []goPackage {
		return []goPackage{
			{dir: "faucet", files: []goFile{
				fileOf("access.go", use(helper, "Call"), use(external, "New")),
				fileOf("access_test.go", use(core, "New"), use(pgstore, "New"), use(storet, "Run"), use(pgtest, "NewDB")),
			}},
			{dir: "faucet/internal/githubapi", files: []goFile{
				fileOf("githubapi.go", use(external, "New")),
				fileOf("githubapi_test.go", use(helper, "Call")),
			}},
			{dir: "faucet/internal/pgtest", files: []goFile{fileOf("pgtest.go")}},
			{dir: "faucet/redelivery", files: []goFile{
				fileOf("run.go", use(core, "StoresDelivery", "Event"), use(helper, "Call")),
				fileOf("run_test.go", use(core, "Service"), use(feature, "New"), use(pgstore, "New"), use(storet, "RunRedelivery"), use(pgtest, "NewDB")),
			}},
			{dir: "faucet/redelivery/sub", files: []goFile{fileOf("sub.go", use(feature, "Store"), use(core, "Event"))}},
			{dir: "faucet/other", files: []goFile{fileOf("other.go", use(core, "Store"))}},
			{dir: "faucet/pgstore", files: []goFile{fileOf("pgstore.go", use(core, "Store"), use(feature, "Store"))}},
			{dir: "faucet/pgstore/database", files: []goFile{fileOf("db.go")}},
			{dir: "faucet/storetest", files: []goFile{fileOf("storetest.go", use(core, "Store"), use(feature, "Store"))}},
		}
	}
	require.Empty(t, compartmentViolations(valid(), adapters), "the valid layout passes")

	addFile := func(dir string, f goFile) func([]goPackage) []goPackage {
		return func(pkgs []goPackage) []goPackage {
			for i := range pkgs {
				if pkgs[i].dir == dir {
					pkgs[i].files = append(pkgs[i].files, f)
					return pkgs
				}
			}
			return append(pkgs, goPackage{dir: dir, files: []goFile{f}})
		}
	}
	for _, tc := range []struct {
		name   string
		breach func([]goPackage) []goPackage
		want   string
	}{
		{"core code imports an adapter", addFile("faucet", fileOf("x.go", use(pgstore, "New"))), "R1: faucet/x.go imports " + pgstore},
		{"core code imports a feature", addFile("faucet", fileOf("x.go", use(feature, "New"))), "R1: faucet/x.go imports " + feature},
		{"a core test imports a feature", addFile("faucet", fileOf("x_test.go", use(feature, "New"))), "R1: faucet/x_test.go imports " + feature},
		{"a core test imports cmd", addFile("faucet", fileOf("x_test.go", use(cmd, "Run"))), "R1: faucet/x_test.go imports " + cmd},
		{"an internal package imports core", addFile("faucet/internal/githubapi", fileOf("x.go", use(core, "Event"))), "R2: faucet/internal/githubapi/x.go imports " + core},
		{"an internal package imports another", addFile("faucet/internal/githubapi", fileOf("x_test.go", use(pgtest, "NewDB"))), "R2: faucet/internal/githubapi/x_test.go imports " + pgtest},
		{"a feature imports another feature", addFile("faucet/redelivery", fileOf("x.go", use(other, "New"))), "R3: faucet/redelivery/x.go imports " + other},
		{"a feature test imports another feature", addFile("faucet/redelivery", fileOf("x_test.go", use(other, "New"))), "R3: faucet/redelivery/x_test.go imports " + other},
		{"a feature's code imports an adapter", addFile("faucet/redelivery", fileOf("x.go", use(pgstore, "New"))), "R3: faucet/redelivery/x.go imports " + pgstore},
		{"a feature imports cmd", addFile("faucet/redelivery", fileOf("x.go", use(cmd, "Run"))), "R3: faucet/redelivery/x.go imports " + cmd},
		{"an unlisted adapter is a feature", addFile("faucet/sqlite", fileOf("x.go", use(feature, "Store"))), "R3: faucet/sqlite/x.go imports " + feature},
		{"a feature names too many core identifiers", addFile("faucet/redelivery", fileOf("x.go", use(core, "A", "B", "C", "D"))), "R4: faucet/redelivery names 6 core identifiers"},
		{"a feature's sub-package counts toward its limit", addFile("faucet/redelivery/sub", fileOf("x.go", use(core, "A", "B", "C", "D"))), "R4: faucet/redelivery names 6 core identifiers"},
		{"a feature names faucet.Service", addFile("faucet/redelivery", fileOf("x.go", use(core, "Service"))), "R4: faucet/redelivery names faucet.Service"},
		{"a feature names faucet.Deps", addFile("faucet/other", fileOf("x.go", use(core, "Deps"))), "R4: faucet/other names faucet.Deps"},
		{"a dot import", addFile("faucet/pgstore", fileOf("x.go", goImport{path: core, local: "."})), "R5: faucet/pgstore/x.go dot-imports " + core},
		{"a core file named like a feature's verb", addFile("faucet", fileOf("redeliver_probe.go")), "R6: faucet/redeliver_probe.go is named like the feature redelivery"},
		{"a core test named like a feature", addFile("faucet", fileOf("redelivery_test.go")), "R6: faucet/redelivery_test.go is named like the feature redelivery"},
		{"no feature", func(pkgs []goPackage) []goPackage {
			return slices.DeleteFunc(pkgs, func(p goPackage) bool { r, _ := roleOf(p.dir, adapters); return r == roleFeature })
		}, "self-check: the walk found no feature"},
		{"a listed adapter is missing", func(pkgs []goPackage) []goPackage {
			return slices.DeleteFunc(pkgs, func(p goPackage) bool { return p.dir == "faucet/storetest" })
		}, "self-check: the adapter faucet/storetest does not exist"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := compartmentViolations(tc.breach(valid()), adapters)
			require.NotEmpty(t, got)
			require.True(t, slices.ContainsFunc(got, func(v string) bool { return strings.HasPrefix(v, tc.want) }), "want %q in %q", tc.want, got)
		})
	}
}

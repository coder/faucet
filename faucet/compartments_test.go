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
// faucet is core, the internals below hold helpers, the adapters below sit
// between core and the features, and every other directory under faucet/ is
// a feature.
const module = "github.com/coder/superblink-events"

// The two lists that are kept by hand, each entry one package. A directory
// under faucet/internal or under an adapter that the lists do not name is a
// violation, and any other directory missing from adapters is a feature, so a
// new helper or adapter fails closed.
var (
	internals = []string{"faucet/internal/githubapi", "faucet/internal/pgtest"}
	// testHelpers are the internals that only test files import, so that no
	// build of the server links testing and testify.
	testHelpers = []string{"faucet/internal/pgtest"}
	adapters    = []string{"faucet/pgstore", "faucet/pgstore/database", "faucet/storetest"}
)

// coreUses lists, for each feature, the core identifiers that its non-test
// code names. A new name is one reviewed line here, and a name that the code no
// longer uses fails too, so the list says exactly what a feature takes from
// core.
var coreUses = map[string][]string{
	"oauth": {
		"AllScopes", "Authorizer", "ErrInvalidToken", "ErrNoAccess", "ErrUnknownOwner", "ErrUnknownRepository", "HTTPClientOptions",
		"NewHTTPClient", "NewOAuthUser", "NewPrincipal", "NewService", "OAuthSession", "Owner", "ParseScope", "Principal", "Repository",
		"Scope", "ServiceClient", "ServiceGrants",
	},
	"redelivery": {"StoresDelivery"},
}

// banned are the core identifiers that no feature or adapter code names.
var banned = []string{"Service", "Deps"}

type role int

const (
	roleCore role = iota
	roleInternal
	roleAdapter
	roleFeature
	roleOther
	// roleUnlisted is a directory that needs a place in a hand list.
	roleUnlisted
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
func roleOf(dir string) (role, string) {
	switch {
	case dir == "faucet":
		return roleCore, ""
	case slices.Contains(internals, dir):
		return roleInternal, ""
	case slices.Contains(adapters, dir):
		return roleAdapter, ""
	case strings.HasPrefix(dir+"/", "faucet/internal/"),
		slices.ContainsFunc(adapters, func(a string) bool { return strings.HasPrefix(dir, a+"/") }):
		return roleUnlisted, ""
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
func compartmentViolations(pkgs []goPackage, coreUses map[string][]string) []string {
	var out []string
	add := func(format string, args ...any) { out = append(out, fmt.Sprintf(format, args...)) }

	dirs := map[string]bool{}
	features := map[string]bool{}
	for _, p := range pkgs {
		dirs[p.dir] = true
		switch r, feature := roleOf(p.dir); r {
		case roleFeature:
			features[feature] = true
		case roleUnlisted:
			add("R8: %s is in no hand list: move its code, or list a new helper in internals or a new adapter package in adapters", p.dir)
		}
	}
	if len(features) == 0 {
		add("self-check: the walk found no feature under faucet/")
	}
	for kind, list := range map[string][]string{"helper": internals, "adapter": adapters, "test helper": testHelpers} {
		for _, dir := range list {
			if !dirs[dir] {
				add("self-check: the %s %s does not exist", kind, dir)
			}
		}
	}
	for _, dir := range testHelpers {
		if !slices.Contains(internals, dir) {
			add("self-check: the test helper %s is not in internals", dir)
		}
	}

	coreNames := map[string]map[string]bool{}
	for _, p := range pkgs {
		r, feature := roleOf(p.dir)
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
				if slices.ContainsFunc(strings.Split(target, "/"), skipped) {
					add("R7: %s imports %s: the walk reads no testdata, _ or . directory", where, imp.path)
				}
				if slices.Contains(testHelpers, target) && !f.test() {
					add("R9: %s imports %s: only test files import a test helper", where, imp.path)
				}
				tr, tfeature := roleOf(target)
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
				case roleAdapter:
					if tr == roleCore && !f.test() {
						for _, name := range imp.names {
							if slices.Contains(banned, name) {
								add("R4: %s names faucet.%s", where, name)
							}
						}
					}
				case roleOther, roleUnlisted:
				}
			}
			if r == roleCore {
				parts := strings.Split(strings.TrimSuffix(strings.TrimSuffix(f.name, ".go"), "_test"), "_")
				for feature := range features {
					// The feature's name less a trailing y or s is its stem, so
					// that the verb (redeliver) counts as well as the noun
					// (redelivery). A part of the file name matches when it is
					// the name or the stem, or starts with a stem of 4 or more
					// letters, because this repo runs words together
					// (redeliverystore.go). The length keeps a short feature
					// name from matching every core file.
					stem := strings.TrimSuffix(strings.TrimSuffix(feature, "y"), "s")
					if slices.ContainsFunc(parts, func(p string) bool {
						return p == feature || p == stem || (len(stem) >= 4 && strings.HasPrefix(p, stem))
					}) {
						add("R6: %s is named like the feature %s: its code belongs in faucet/%s", where, feature, feature)
					}
				}
			}
		}
	}
	for feature, names := range coreNames {
		for _, name := range slices.Sorted(maps.Keys(names)) {
			switch {
			case slices.Contains(banned, name):
				add("R4: faucet/%s names faucet.%s", feature, name)
			case !slices.Contains(coreUses[feature], name):
				add("R4: faucet/%s names faucet.%s, which coreUses does not list for it", feature, name)
			}
		}
	}
	for _, feature := range slices.Sorted(maps.Keys(coreUses)) {
		for _, name := range coreUses[feature] {
			if !coreNames[feature][name] {
				add("R4: coreUses lists faucet.%s for faucet/%s, which its code does not name", name, feature)
			}
		}
	}
	slices.Sort(out)
	return out
}

// skipped reports whether Go leaves a file or directory of this name out of
// ./..., as the walk does. Code can still import such a package by its path,
// so R7 forbids that.
func skipped(name string) bool {
	return name == "testdata" || strings.HasPrefix(name, "_") || strings.HasPrefix(name, ".")
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
		if path != "." && skipped(d.Name()) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") {
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
	violations := compartmentViolations(parseLayout(t), coreUses)
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
			{dir: "faucet/pgstore", files: []goFile{fileOf("pgstore.go", use(core, "Store"), use(feature, "Store")), fileOf("pgstore_test.go", use(core, "Service", "Deps"))}},
			{dir: "faucet/pgstore/database", files: []goFile{fileOf("db.go")}},
			{dir: "faucet/storetest", files: []goFile{fileOf("storetest.go", use(core, "Store"), use(feature, "Store"))}},
		}
	}
	uses := map[string][]string{"redelivery": {"Event", "StoresDelivery"}, "other": {"Store"}}
	require.Empty(t, compartmentViolations(valid(), uses), "the valid layout passes")
	stale := map[string][]string{"redelivery": {"Event", "Gone", "StoresDelivery"}, "other": {"Store"}}
	require.Contains(t, compartmentViolations(valid(), stale), "R4: coreUses lists faucet.Gone for faucet/redelivery, which its code does not name")

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
	for _, feature := range []string{"events2", "a"} {
		pkgs := addFile("faucet/"+feature, fileOf("x.go"))(addFile("faucet", fileOf("events.go"))(valid()))
		require.Empty(t, compartmentViolations(pkgs, uses), "no core file is named like the feature %s", feature)
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
		{"an unlisted directory under faucet/internal", addFile("faucet/internal/redelivercore", fileOf("x.go")), "R8: faucet/internal/redelivercore is in no hand list"},
		{"core imports an unlisted directory under faucet/internal", addFile("faucet", fileOf("x.go", use(module+"/faucet/internal/redelivercore", "Run"))), "R1: faucet/x.go imports " + module + "/faucet/internal/redelivercore"},
		{"a feature imports another feature", addFile("faucet/redelivery", fileOf("x.go", use(other, "New"))), "R3: faucet/redelivery/x.go imports " + other},
		{"a feature test imports another feature", addFile("faucet/redelivery", fileOf("x_test.go", use(other, "New"))), "R3: faucet/redelivery/x_test.go imports " + other},
		{"a feature's code imports an adapter", addFile("faucet/redelivery", fileOf("x.go", use(pgstore, "New"))), "R3: faucet/redelivery/x.go imports " + pgstore},
		{"a feature imports cmd", addFile("faucet/redelivery", fileOf("x.go", use(cmd, "Run"))), "R3: faucet/redelivery/x.go imports " + cmd},
		{"an unlisted adapter is a feature", addFile("faucet/sqlite", fileOf("x.go", use(feature, "Store"))), "R3: faucet/sqlite/x.go imports " + feature},
		{"a feature names a core identifier that coreUses does not list", addFile("faucet/redelivery", fileOf("x.go", use(core, "Store"))), "R4: faucet/redelivery names faucet.Store, which coreUses does not list for it"},
		{"a new feature names core without a coreUses entry", addFile("faucet/fresh", fileOf("x.go", use(core, "Event"))), "R4: faucet/fresh names faucet.Event, which coreUses does not list for it"},
		{"a feature's sub-package is held to its coreUses entry", addFile("faucet/redelivery/sub", fileOf("x.go", use(core, "Store"))), "R4: faucet/redelivery names faucet.Store, which coreUses does not list for it"},
		{"a feature names faucet.Service", addFile("faucet/redelivery", fileOf("x.go", use(core, "Service"))), "R4: faucet/redelivery names faucet.Service"},
		{"a feature names faucet.Deps", addFile("faucet/other", fileOf("x.go", use(core, "Deps"))), "R4: faucet/other names faucet.Deps"},
		{"an adapter's code names faucet.Service", addFile("faucet/pgstore", fileOf("x.go", use(core, "Service"))), "R4: faucet/pgstore/x.go names faucet.Service"},
		{"an adapter package's code names faucet.Deps", addFile("faucet/pgstore/database", fileOf("x.go", use(core, "Deps"))), "R4: faucet/pgstore/database/x.go names faucet.Deps"},
		{"an unlisted directory under an adapter", addFile("faucet/pgstore/feat", fileOf("x.go", use(core, "Service"))), "R8: faucet/pgstore/feat is in no hand list"},
		{"a dot import", addFile("faucet/pgstore", fileOf("x.go", goImport{path: core, local: "."})), "R5: faucet/pgstore/x.go dot-imports " + core},
		{"a feature test imports its testdata package", addFile("faucet/redelivery", fileOf("x_test.go", use(feature+"/testdata/x", "New"))), "R7: faucet/redelivery/x_test.go imports " + feature + "/testdata/x"},
		{"core imports a package under an underscore directory", addFile("faucet", fileOf("x_test.go", use(module+"/faucet/_scratch", "New"))), "R7: faucet/x_test.go imports " + module + "/faucet/_scratch"},
		{"core imports a package under a dot directory", addFile("faucet", fileOf("x.go", use(helper+"/.x", "New"))), "R7: faucet/x.go imports " + helper + "/.x"},
		{"a core file named like a feature's verb", addFile("faucet", fileOf("redeliver_probe.go")), "R6: faucet/redeliver_probe.go is named like the feature redelivery"},
		{"a core test named like a feature", addFile("faucet", fileOf("redelivery_test.go")), "R6: faucet/redelivery_test.go is named like the feature redelivery"},
		{"a core file that runs a feature's name into another word", addFile("faucet", fileOf("redeliverystore.go")), "R6: faucet/redeliverystore.go is named like the feature redelivery"},
		{"a core test that runs a feature's verb into another word", addFile("faucet", fileOf("redeliverharness_test.go")), "R6: faucet/redeliverharness_test.go is named like the feature redelivery"},
		{"a core file with a part that runs a feature's verb into another word", addFile("faucet", fileOf("hook_redeliverretry.go")), "R6: faucet/hook_redeliverretry.go is named like the feature redelivery"},
		{"code imports a test helper", addFile("faucet/redelivery", fileOf("x.go", use(pgtest, "NewDB"))), "R9: faucet/redelivery/x.go imports " + pgtest},
		{"core code imports a test helper", addFile("faucet", fileOf("x.go", use(pgtest, "NewDB"))), "R9: faucet/x.go imports " + pgtest},
		{"a core file whose last part is a feature's verb", addFile("faucet", fileOf("hook_redeliver.go")), "R6: faucet/hook_redeliver.go is named like the feature redelivery"},
		{"no feature", func(pkgs []goPackage) []goPackage {
			return slices.DeleteFunc(pkgs, func(p goPackage) bool { r, _ := roleOf(p.dir); return r == roleFeature })
		}, "self-check: the walk found no feature"},
		{"a listed adapter is missing", func(pkgs []goPackage) []goPackage {
			return slices.DeleteFunc(pkgs, func(p goPackage) bool { return p.dir == "faucet/storetest" })
		}, "self-check: the adapter faucet/storetest does not exist"},
		{"a listed helper is missing", func(pkgs []goPackage) []goPackage {
			return slices.DeleteFunc(pkgs, func(p goPackage) bool { return p.dir == "faucet/internal/pgtest" })
		}, "self-check: the helper faucet/internal/pgtest does not exist"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := compartmentViolations(tc.breach(valid()), uses)
			require.NotEmpty(t, got)
			require.True(t, slices.ContainsFunc(got, func(v string) bool { return strings.HasPrefix(v, tc.want) }), "want %q in %q", tc.want, got)
		})
	}
}

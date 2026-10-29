package core

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"sort"
	"strings"
	"testing"
)

// AllJobKinds lists every JobKind constant declared in this package, so the
// kind lists derived from it (the /runs filter, ...) cannot drift.
func TestAllJobKindsComplete(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	var declared []string
	for _, p := range pkgs {
		for _, f := range p.Files {
			for _, d := range f.Decls {
				g, ok := d.(*ast.GenDecl)
				if !ok || g.Tok != token.CONST {
					continue
				}
				for _, s := range g.Specs {
					vs := s.(*ast.ValueSpec)
					if id, ok := vs.Type.(*ast.Ident); ok && id.Name == "JobKind" {
						for _, n := range vs.Names {
							declared = append(declared, n.Name)
						}
					}
				}
			}
		}
	}
	if len(declared) == 0 {
		t.Fatal("no JobKind constants found")
	}
	values := map[string]JobKind{
		"JobAutoReview": JobAutoReview, "JobReview": JobReview, "JobFullReview": JobFullReview, "JobChat": JobChat,
		"JobHelp": JobHelp, "JobRefreshTitle": JobRefreshTitle, "JobRefreshStatus": JobRefreshStatus, "JobThreadReply": JobThreadReply, "JobRules": JobRules,
		"JobPause": JobPause, "JobResume": JobResume, "JobIgnore": JobIgnore, "JobResolveAll": JobResolveAll,
		"JobConfig": JobConfig, "JobMemory": JobMemory, "JobCheck": JobCheck,
		"JobForceReview": JobForceReview, "JobReact": JobReact, "JobPipeline": JobPipeline,
	}
	all := map[JobKind]bool{}
	for _, k := range AllJobKinds() {
		if all[k] {
			t.Errorf("duplicate %s", k)
		}
		all[k] = true
	}
	sort.Strings(declared)
	for _, name := range declared {
		v, ok := values[name]
		if !ok {
			t.Errorf("new JobKind constant %s: add it to AllJobKinds (and to this test)", name)
			continue
		}
		if !all[v] {
			t.Errorf("AllJobKinds lacks %s", name)
		}
	}
	if len(all) != len(declared) {
		t.Errorf("AllJobKinds has %d kinds, %d declared", len(all), len(declared))
	}
}

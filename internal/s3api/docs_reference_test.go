package s3api_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// TestDocsReferenceCoverage is a drift-proof lint for the hand-curated
// docs/site/content/reference/s3-api.md table. It AST-walks the dispatch
// functions in this package (ServeHTTP, handleBucket, handleObject,
// handleBucketInventory) and asserts every directly-called *Server method
// either has a row in the markdown table OR carries a `// docs:skip`
// comment immediately above its declaration.
//
// It also catches orphan rows — markdown cells of shape
// `internal/s3api/<file>.go:<line>` that point at a line outside any
// function declaration (handler was renamed or removed).
//
// See `tasks/prd-dx-lab.md` US-004 for the design intent.
func TestDocsReferenceCoverage(t *testing.T) {
	const docsRel = "../../docs/site/content/reference/s3-api.md"

	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read internal/s3api: %v", err)
	}
	var astFiles []*ast.File
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		af, perr := parser.ParseFile(fset, name, nil, parser.ParseComments)
		if perr != nil {
			t.Fatalf("parse %s: %v", name, perr)
		}
		astFiles = append(astFiles, af)
	}

	type funcInfo struct {
		name     string
		file     string
		line     int
		endLine  int
		docsSkip bool
	}
	funcs := map[string]*funcInfo{}
	funcsByFile := map[string][]*funcInfo{}
	for _, file := range astFiles {
		for _, decl := range file.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Recv == nil || len(fd.Recv.List) == 0 {
				continue
			}
			star, ok := fd.Recv.List[0].Type.(*ast.StarExpr)
			if !ok {
				continue
			}
			id, ok := star.X.(*ast.Ident)
			if !ok || id.Name != "Server" {
				continue
			}
			startPos := fset.Position(fd.Pos())
			endPos := fset.Position(fd.End())
			docsSkip := false
			if fd.Doc != nil {
				for _, c := range fd.Doc.List {
					body := strings.TrimSpace(strings.TrimPrefix(c.Text, "//"))
					if body == "docs:skip" {
						docsSkip = true
					}
				}
			}
			info := &funcInfo{
				name:     fd.Name.Name,
				file:     filepath.Base(startPos.Filename),
				line:     startPos.Line,
				endLine:  endPos.Line,
				docsSkip: docsSkip,
			}
			funcs[info.name] = info
			funcsByFile[info.file] = append(funcsByFile[info.file], info)
		}
	}

	dispatchFuncs := map[string]bool{
		"ServeHTTP":             true,
		"handleBucket":          true,
		"handleObject":          true,
		"handleBucketInventory": true,
	}
	// Methods that are dispatchers themselves OR clearly never S3 surface — they
	// don't need a doc row and shouldn't be reported. Everything else either
	// needs a markdown row OR a `// docs:skip` line comment immediately above
	// the func declaration.
	dispatcherCalls := map[string]bool{
		"handleAdmin":           true,
		"handleIAM":             true,
		"handleBucket":          true,
		"handleObject":          true,
		"handleBucketInventory": true,
	}

	called := map[string]bool{}
	for _, file := range astFiles {
		for _, decl := range file.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			if !dispatchFuncs[fd.Name.Name] {
				continue
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				ce, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := ce.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				ident, ok := sel.X.(*ast.Ident)
				if !ok || ident.Name != "s" {
					return true
				}
				name := sel.Sel.Name
				if dispatcherCalls[name] {
					return true
				}
				if _, has := funcs[name]; !has {
					return true
				}
				called[name] = true
				return true
			})
		}
	}

	docsBytes, err := os.ReadFile(docsRel)
	if err != nil {
		t.Fatalf("read %s: %v", docsRel, err)
	}
	docs := string(docsBytes)

	var missing, skipped []string
	for name := range called {
		info := funcs[name]
		if info.docsSkip {
			skipped = append(skipped, fmt.Sprintf("%s (%s:%d)", name, info.file, info.line))
			continue
		}
		nameTok := "`" + name + "`"
		fileLineTok := info.file + ":" + strconv.Itoa(info.line)
		if strings.Contains(docs, nameTok) || strings.Contains(docs, fileLineTok) {
			continue
		}
		missing = append(missing, fmt.Sprintf("%s (%s:%d)", name, info.file, info.line))
	}
	sort.Strings(missing)
	sort.Strings(skipped)
	for _, s := range skipped {
		t.Logf("docs:skip handler tolerated: %s", s)
	}
	for _, m := range missing {
		t.Errorf("dispatch handler missing from docs/site/content/reference/s3-api.md: %s — add a row referencing internal/s3api/<file>:<line> OR put a `// docs:skip` line comment immediately above the func declaration", m)
	}

	// Orphan check: every internal/s3api/<file>:<line> cell in the markdown
	// must resolve to a *Server method whose body spans the referenced line.
	cellRE := regexp.MustCompile(`internal/s3api/([A-Za-z0-9_.\-]+\.go):(\d+)`)
	matches := cellRE.FindAllStringSubmatch(docs, -1)
	seen := map[string]bool{}
	var orphans []string
	for _, m := range matches {
		key := m[1] + ":" + m[2]
		if seen[key] {
			continue
		}
		seen[key] = true
		file := m[1]
		line, _ := strconv.Atoi(m[2])
		infos, ok := funcsByFile[file]
		if !ok {
			orphans = append(orphans, fmt.Sprintf("%s — referenced file not found in package", key))
			continue
		}
		found := false
		for _, info := range infos {
			if info.line <= line && line <= info.endLine {
				found = true
				break
			}
		}
		if !found {
			orphans = append(orphans, fmt.Sprintf("%s — no enclosing *Server method (handler renamed or removed?)", key))
		}
	}
	sort.Strings(orphans)
	for _, o := range orphans {
		t.Errorf("orphan row in docs/site/content/reference/s3-api.md: %s — update the file:line or remove the row", o)
	}
}

// Package analyzer provides Go source code analysis using the standard library.
// It uses go/ast, go/types, and go/parser to build a semantic model of a Go codebase
// without requiring any external dependencies.
package analyzer

import (
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"strings"
	"sync"

	t "github.com/daybeam/vortex/pkg/codeintel/types"
)

// GoAnalyzer holds the analyzed state of a Go codebase.
type GoAnalyzer struct {
	mu       sync.RWMutex
	rootDir  string
	fset     *token.FileSet
	packages map[string]*analyzedPkg // pkgPath -> analyzed package
	overlay  map[string]string       // file -> override content (for virtual edits)
}

type analyzedPkg struct {
	path     string
	name     string
	files    []*ast.File
	info     *types.Info
	pkg      *types.Package
	srcFiles []string // file paths
}

// NewGoAnalyzer creates a new analyzer for the given root directory.
func NewGoAnalyzer(rootDir string) *GoAnalyzer {
	return &GoAnalyzer{
		rootDir:  rootDir,
		fset:     token.NewFileSet(),
		packages: make(map[string]*analyzedPkg),
		overlay:  make(map[string]string),
	}
}

// SetRootDir updates the root directory.
func (a *GoAnalyzer) SetRootDir(rootDir string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.rootDir = rootDir
}

// Index walks the root directory and analyzes all Go packages.
func (a *GoAnalyzer) Index() error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.rootDir == "" {
		return fmt.Errorf("root directory not set")
	}

	a.fset = token.NewFileSet()
	a.packages = make(map[string]*analyzedPkg)

	return filepath.WalkDir(a.rootDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if d.Name() == "vendor" || d.Name() == ".git" || strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return a.indexDir(path)
		}
		return nil
	})
}

func (a *GoAnalyzer) indexDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}

	var goFiles []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".go") && !strings.HasSuffix(e.Name(), "_test.go") {
			goFiles = append(goFiles, filepath.Join(dir, e.Name()))
		}
	}
	if len(goFiles) == 0 {
		return nil
	}

	// Parse all files in the directory
	var astFiles []*ast.File
	for _, f := range goFiles {
		var src interface{}
		if content, ok := a.overlay[f]; ok {
			src = content
		}
		af, err := parser.ParseFile(a.fset, f, src, parser.ParseComments)
		if err != nil {
			continue // skip files with syntax errors
		}
		astFiles = append(astFiles, af)
	}
	if len(astFiles) == 0 {
		return nil
	}

	pkgName := astFiles[0].Name.Name
	pkgPath, _ := filepath.Rel(a.rootDir, dir)
	if pkgPath == "." {
		pkgPath = pkgName
	}

	info := &types.Info{
		Types:      make(map[ast.Expr]types.TypeAndValue),
		Defs:       make(map[*ast.Ident]types.Object),
		Uses:       make(map[*ast.Ident]types.Object),
		Implicits:  make(map[ast.Node]types.Object),
		Selections: make(map[*ast.SelectorExpr]*types.Selection),
		Scopes:     make(map[ast.Node]*types.Scope),
	}

	conf := types.Config{
		Importer: importer.ForCompiler(a.fset, "gc", nil),
		Error:    func(err error) {}, // collect but don't stop
	}

	pkg, _ := conf.Check(pkgPath, a.fset, astFiles, info)
	// pkg may be nil if there are errors, but info is still partially populated

	ap := &analyzedPkg{
		path:     pkgPath,
		name:     pkgName,
		files:    astFiles,
		info:     info,
		pkg:      pkg,
		srcFiles: goFiles,
	}
	a.packages[pkgPath] = ap
	return nil
}

// SetOverlay sets a virtual file content (for uncommitted edits).
func (a *GoAnalyzer) SetOverlay(file, content string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.overlay[file] = content
}

// ClearOverlay removes a virtual file override.
func (a *GoAnalyzer) ClearOverlay(file string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.overlay, file)
}

// ReindexWithOverlay re-analyzes only the packages affected by current overlays.
func (a *GoAnalyzer) ReindexWithOverlay() error {
	a.mu.Lock()
	defer a.mu.Unlock()

	affected := make(map[string]bool)
	for file := range a.overlay {
		dir := filepath.Dir(file)
		rel, _ := filepath.Rel(a.rootDir, dir)
		affected[rel] = true
	}

	for pkgPath := range affected {
		dir := filepath.Join(a.rootDir, pkgPath)
		if err := a.indexDir(dir); err != nil {
			return err
		}
	}
	return nil
}

// FindSymbol looks up a symbol by name across all packages.
func (a *GoAnalyzer) FindSymbol(name string) []t.Symbol {
	a.mu.RLock()
	defer a.mu.RUnlock()

	var results []t.Symbol
	for _, pkg := range a.packages {
		for ident, obj := range pkg.info.Defs {
			if obj == nil || ident.Name != name {
				continue
			}
			if sym, ok := a.objectToSymbol(obj, pkg); ok {
				results = append(results, sym)
			}
		}
	}
	return results
}

// FindReferences returns all usage sites of a symbol.
func (a *GoAnalyzer) FindReferences(name string, defFile string) []t.Reference {
	a.mu.RLock()
	defer a.mu.RUnlock()

	// First, find the target object
	var target types.Object
	for _, pkg := range a.packages {
		for ident, obj := range pkg.info.Defs {
			if obj == nil || ident.Name != name {
				continue
			}
			pos := a.fset.Position(obj.Pos())
			if defFile == "" || strings.HasSuffix(pos.Filename, defFile) {
				target = obj
				break
			}
		}
		if target != nil {
			break
		}
	}

	if target == nil {
		return nil
	}

	var refs []t.Reference
	for _, pkg := range a.packages {
		for ident, obj := range pkg.info.Uses {
			if obj == target {
				pos := a.fset.Position(ident.Pos())
				refs = append(refs, t.Reference{
					File:   pos.Filename,
					Line:   pos.Line,
					Column: pos.Column,
				})
			}
		}
	}
	return refs
}

// GetCallees returns all functions called by the given function.
func (a *GoAnalyzer) GetCallees(funcName string, file string) []t.Symbol {
	a.mu.RLock()
	defer a.mu.RUnlock()

	var results []t.Symbol
	seen := make(map[string]bool)

	for _, pkg := range a.packages {
		for _, f := range pkg.files {
			ast.Inspect(f, func(n ast.Node) bool {
				fd, ok := n.(*ast.FuncDecl)
				if !ok || fd.Name.Name != funcName {
					return true
				}
				fpos := a.fset.Position(fd.Pos())
				if file != "" && !strings.HasSuffix(fpos.Filename, file) {
					return true
				}
				// Found the function, walk its body for calls
				ast.Inspect(fd.Body, func(inner ast.Node) bool {
					call, ok := inner.(*ast.CallExpr)
					if !ok {
						return true
					}
					calleeName := exprName(call.Fun)
					if calleeName == "" || seen[calleeName] {
						return true
					}
					seen[calleeName] = true
					// Look up the callee symbol
					syms := a.findSymbolByExpr(call.Fun, pkg)
					results = append(results, syms...)
					return true
				})
				return false
			})
		}
	}
	return results
}

// GetCallers returns all functions that call the given function.
func (a *GoAnalyzer) GetCallers(funcName string) []t.Symbol {
	a.mu.RLock()
	defer a.mu.RUnlock()

	// Find the target object first
	var target types.Object
	for _, pkg := range a.packages {
		for ident, obj := range pkg.info.Defs {
			if obj != nil && ident.Name == funcName {
				if _, ok := obj.(*types.Func); ok {
					target = obj
					break
				}
			}
		}
		if target != nil {
			break
		}
	}

	if target == nil {
		return nil
	}

	var callers []t.Symbol
	seen := make(map[string]bool)

	for _, pkg := range a.packages {
		for _, f := range pkg.files {
			ast.Inspect(f, func(n ast.Node) bool {
				fd, ok := n.(*ast.FuncDecl)
				if !ok {
					return true
				}
				// Check if this function calls our target
				callsTarget := false
				ast.Inspect(fd.Body, func(inner ast.Node) bool {
					call, ok := inner.(*ast.CallExpr)
					if !ok {
						return true
					}
					if ident, ok := call.Fun.(*ast.Ident); ok {
						if obj := pkg.info.Uses[ident]; obj == target {
							callsTarget = true
						}
					}
					if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
						if obj := pkg.info.Uses[sel.Sel]; obj == target {
							callsTarget = true
						}
					}
					return true
				})

				if callsTarget {
					obj := pkg.info.Defs[fd.Name]
					if obj != nil {
						key := obj.Pkg().Path() + "." + obj.Name()
						if !seen[key] {
							seen[key] = true
							if sym, ok := a.objectToSymbol(obj, pkg); ok {
								callers = append(callers, sym)
							}
						}
					}
				}
				return true
			})
		}
	}
	return callers
}

// GetImplementors returns all types that implement the given interface.
func (a *GoAnalyzer) GetImplementors(interfaceName string) []t.Symbol {
	a.mu.RLock()
	defer a.mu.RUnlock()

	// Find the interface type
	var ifaceType *types.Interface
	for _, pkg := range a.packages {
		if pkg.pkg == nil {
			continue
		}
		scope := pkg.pkg.Scope()
		obj := scope.Lookup(interfaceName)
		if obj == nil {
			continue
		}
		named, ok := obj.Type().(*types.Named)
		if !ok {
			continue
		}
		iface, ok := named.Underlying().(*types.Interface)
		if !ok {
			continue
		}
		ifaceType = iface
		break
	}

	if ifaceType == nil {
		return nil
	}

	var implementors []t.Symbol
	for _, pkg := range a.packages {
		if pkg.pkg == nil {
			continue
		}
		scope := pkg.pkg.Scope()
		for _, name := range scope.Names() {
			obj := scope.Lookup(name)
			if obj == nil {
				continue
			}
			named, ok := obj.Type().(*types.Named)
			if !ok {
				continue
			}
			// Check if pointer or value receiver implements the interface
			if types.Implements(named, ifaceType) || types.Implements(types.NewPointer(named), ifaceType) {
				if sym, ok := a.objectToSymbol(obj, pkg); ok {
					implementors = append(implementors, sym)
				}
			}
		}
	}
	return implementors
}

// GetDiagnostics runs type checking on all packages and returns errors.
func (a *GoAnalyzer) GetDiagnostics() []t.Diagnostic {
	a.mu.RLock()
	defer a.mu.RUnlock()

	var diags []t.Diagnostic

	for pkgPath, pkg := range a.packages {
		conf := types.Config{
			Importer: importer.ForCompiler(a.fset, "gc", nil),
			Error: func(err error) {
				if te, ok := err.(types.Error); ok {
					pos := a.fset.Position(te.Pos)
					diags = append(diags, t.Diagnostic{
						File:    pos.Filename,
						Line:    pos.Line,
						Column:  pos.Column,
						Message: te.Msg,
						Kind:    "error",
					})
				}
			},
		}
		info := &types.Info{
			Types: make(map[ast.Expr]types.TypeAndValue),
			Defs:  make(map[*ast.Ident]types.Object),
			Uses:  make(map[*ast.Ident]types.Object),
		}
		conf.Check(pkgPath, a.fset, pkg.files, info)
	}
	return diags
}

// ValidateOverlay runs type checking using the current overlay content.
func (a *GoAnalyzer) ValidateOverlay() []t.Diagnostic {
	// Re-parse with overlay and run diagnostics
	savedOverlay := make(map[string]string)
	for k, v := range a.overlay {
		savedOverlay[k] = v
	}

	// Temporarily re-index with overlay
	tmpAnalyzer := NewGoAnalyzer(a.rootDir)
	tmpAnalyzer.overlay = savedOverlay
	_ = tmpAnalyzer.Index()
	return tmpAnalyzer.GetDiagnostics()
}

// ListSymbols returns all symbols in the codebase.
func (a *GoAnalyzer) ListSymbols(filter string) []t.Symbol {
	a.mu.RLock()
	defer a.mu.RUnlock()

	var results []t.Symbol
	for _, pkg := range a.packages {
		for ident, obj := range pkg.info.Defs {
			if obj == nil {
				continue
			}
			if filter != "" && !strings.Contains(strings.ToLower(ident.Name), strings.ToLower(filter)) {
				continue
			}
			if sym, ok := a.objectToSymbol(obj, pkg); ok {
				results = append(results, sym)
			}
		}
	}
	return results
}

// SymbolView returns a 360-degree view of a symbol.
func (a *GoAnalyzer) SymbolView(name string, file string) (*t.SymbolView, error) {
	syms := a.FindSymbol(name)
	if len(syms) == 0 {
		return nil, fmt.Errorf("symbol %q not found", name)
	}

	sym := syms[0]
	if file != "" {
		for _, s := range syms {
			if strings.Contains(s.File, file) {
				sym = s
				break
			}
		}
	}

	view := &t.SymbolView{
		Symbol:     sym,
		Callers:    a.GetCallers(name),
		Callees:    a.GetCallees(name, file),
		References: a.FindReferences(name, file),
	}

	// If it's an interface, find implementors
	if sym.Kind == t.KindInterface {
		for _, impl := range a.GetImplementors(name) {
			view.ImplementedBy = append(view.ImplementedBy, impl.Package+"."+impl.Name)
		}
	}

	return view, nil
}

// ImpactAnalysis computes the blast radius of changing a symbol.
func (a *GoAnalyzer) ImpactAnalysis(symbolName string, file string) (*t.ImpactResult, error) {
	syms := a.FindSymbol(symbolName)
	if len(syms) == 0 {
		return nil, fmt.Errorf("symbol %q not found", symbolName)
	}

	directCallers := a.GetCallers(symbolName)

	// BFS for indirect callers (2 levels deep to avoid explosion)
	indirectSeen := make(map[string]bool)
	for _, dc := range directCallers {
		indirectSeen[dc.Package+"."+dc.Name] = true
	}
	var indirect []t.Symbol
	for _, dc := range directCallers {
		secondLevel := a.GetCallers(dc.Name)
		for _, sl := range secondLevel {
			key := sl.Package + "." + sl.Name
			if !indirectSeen[key] {
				indirectSeen[key] = true
				indirect = append(indirect, sl)
			}
		}
	}

	// Collect affected files
	fileSet := make(map[string]bool)
	for _, s := range directCallers {
		fileSet[s.File] = true
	}
	for _, s := range indirect {
		fileSet[s.File] = true
	}
	var affectedFiles []string
	for f := range fileSet {
		affectedFiles = append(affectedFiles, f)
	}

	// Risk assessment
	total := len(directCallers) + len(indirect)
	risk := "low"
	switch {
	case total > 20:
		risk = "critical"
	case total > 10:
		risk = "high"
	case total > 3:
		risk = "medium"
	}

	var implementors []t.Symbol
	if len(syms) > 0 && syms[0].Kind == t.KindInterface {
		implementors = a.GetImplementors(symbolName)
		if len(implementors) > 5 {
			risk = "critical"
		}
	}

	return &t.ImpactResult{
		TargetSymbol:    symbolName,
		DirectCallers:   directCallers,
		IndirectCallers: indirect,
		Implementors:    implementors,
		AffectedFiles:   affectedFiles,
		RiskLevel:       risk,
	}, nil
}

// SearchCode performs a text search across all indexed Go files.
func (a *GoAnalyzer) SearchCode(query string, caseSensitive bool) []t.Reference {
	a.mu.RLock()
	defer a.mu.RUnlock()

	var matches []t.Reference
	q := query
	if !caseSensitive {
		q = strings.ToLower(query)
	}

	for _, pkg := range a.packages {
		for _, file := range pkg.srcFiles {
			content, err := os.ReadFile(file)
			if err != nil {
				continue
			}

			lines := strings.Split(string(content), "\n")
			for i, line := range lines {
				text := line
				if !caseSensitive {
					text = strings.ToLower(line)
				}

				if strings.Contains(text, q) {
					matches = append(matches, t.Reference{
						File: file,
						Line: i + 1,
					})
				}
			}
		}
	}
	return matches
}

// --- helpers ---

func (a *GoAnalyzer) objectToSymbol(obj types.Object, pkg *analyzedPkg) (t.Symbol, bool) {
	if obj == nil || obj.Pkg() == nil {
		return t.Symbol{}, false
	}
	pos := a.fset.Position(obj.Pos())
	sym := t.Symbol{
		Name:     obj.Name(),
		Package:  obj.Pkg().Path(),
		File:     pos.Filename,
		Line:     pos.Line,
		Exported: obj.Exported(),
	}

	switch o := obj.(type) {
	case *types.Func:
		sig := o.Type().(*types.Signature)
		if sig.Recv() != nil {
			sym.Kind = t.KindMethod
			sym.Receiver = sig.Recv().Type().String()
		} else {
			sym.Kind = t.KindFunction
		}
		sym.Signature = types.TypeString(o.Type(), nil)
	case *types.TypeName:
		underlying := o.Type().Underlying()
		switch underlying.(type) {
		case *types.Interface:
			sym.Kind = t.KindInterface
		case *types.Struct:
			sym.Kind = t.KindStruct
		default:
			sym.Kind = t.KindType
		}
		sym.Signature = types.TypeString(o.Type(), nil)
	case *types.Var:
		sym.Kind = t.KindVar
		sym.Signature = types.TypeString(o.Type(), nil)
	case *types.Const:
		sym.Kind = t.KindConst
		sym.Signature = types.TypeString(o.Type(), nil)
	default:
		return t.Symbol{}, false
	}

	// Attach doc comment
	sym.DocComment = a.findDocComment(obj, pkg)
	return sym, true
}

func (a *GoAnalyzer) findDocComment(obj types.Object, pkg *analyzedPkg) string {
	pos := obj.Pos()
	for _, f := range pkg.files {
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if d.Name.Pos() == pos && d.Doc != nil {
					return d.Doc.Text()
				}
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					switch s := spec.(type) {
					case *ast.TypeSpec:
						if s.Name.Pos() == pos {
							if s.Doc != nil {
								return s.Doc.Text()
							}
							if d.Doc != nil {
								return d.Doc.Text()
							}
						}
					case *ast.ValueSpec:
						for _, name := range s.Names {
							if name.Pos() == pos && s.Doc != nil {
								return s.Doc.Text()
							}
						}
					}
				}
			}
		}
	}
	return ""
}

func (a *GoAnalyzer) findSymbolByExpr(expr ast.Expr, pkg *analyzedPkg) []t.Symbol {
	var results []t.Symbol
	switch e := expr.(type) {
	case *ast.Ident:
		if obj := pkg.info.Uses[e]; obj != nil {
			if sym, ok := a.objectToSymbol(obj, pkg); ok {
				results = append(results, sym)
			}
		}
	case *ast.SelectorExpr:
		if obj := pkg.info.Uses[e.Sel]; obj != nil {
			if sym, ok := a.objectToSymbol(obj, pkg); ok {
				results = append(results, sym)
			}
		}
	}
	return results
}

func exprName(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.SelectorExpr:
		return exprName(e.X) + "." + e.Sel.Name
	}
	return ""
}

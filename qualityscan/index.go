package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/ClappFormOrg/static-analysis/qualityscan/internal/gomarker"
)

// File is one parsed Go source file, kept alongside its bytes so the token
// counter can rescan a declaration's exact source range.
type File struct {
	Abs  string // absolute path on disk
	Rel  string // slash-separated, relative to the scan root
	Src  []byte
	AST  *ast.File
	Pkg  *Package
	Line func(token.Pos) int
}

// Package is one directory of Go files. Go has no cross-file declaration
// scoping inside a package, so Decls can index every top-level name in the
// directory to the single file that declares it.
type Package struct {
	Dir        string // slash-separated, relative to the scan root
	ImportPath string
	Files      []*File
	Decls      map[string]*File // top-level declared name -> declaring file
}

// Index is the whole parsed module.
type Index struct {
	Root       string
	ModulePath string
	Fset       *token.FileSet
	Files      []*File
	Pkgs       map[string]*Package // keyed by import path
	Cfg        Config
}

var moduleLine = regexp.MustCompile(`(?m)^module\s+(\S+)`)

// LoadIndex parses every non-excluded Go file under root, which must be a
// module directory (it must contain a go.mod).
func LoadIndex(root string, cfg Config) (*Index, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	gomod, err := os.ReadFile(filepath.Join(abs, "go.mod"))
	if err != nil {
		return nil, fmt.Errorf("scan root %s is not a module directory: %w", root, err)
	}
	m := moduleLine.FindSubmatch(gomod)
	if m == nil {
		return nil, fmt.Errorf("no module directive in %s/go.mod", root)
	}

	idx := &Index{
		Root:       abs,
		ModulePath: string(m[1]),
		Fset:       token.NewFileSet(),
		Pkgs:       map[string]*Package{},
		Cfg:        cfg,
	}

	err = filepath.WalkDir(abs, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel := relSlash(abs, p)
		if d.IsDir() {
			if rel != "." && (strings.HasPrefix(d.Name(), ".") || cfg.excluded(rel)) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".go") {
			return nil
		}
		if !cfg.IncludeTests && strings.HasSuffix(d.Name(), "_test.go") {
			return nil
		}
		return idx.add(p, rel)
	})
	if err != nil {
		return nil, err
	}

	for _, pkg := range idx.Pkgs {
		pkg.indexDecls()
	}
	return idx, nil
}

func (c Config) excluded(rel string) bool {
	return c.excludedFrom(c.ExcludeDirs, rel)
}

// excludedFrom reports whether rel sits under any of dirs, which are path
// prefixes relative to the scan root.
func (c Config) excludedFrom(dirs []string, rel string) bool {
	for _, e := range dirs {
		e = strings.Trim(e, "/")
		if e != "" && (rel == e || strings.HasPrefix(rel, e+"/")) {
			return true
		}
	}
	return false
}

func (idx *Index) add(abs, rel string) error {
	src, err := os.ReadFile(abs)
	if err != nil {
		return err
	}
	// Before the parse, not after: the marker sits in the preamble, so reading
	// it costs a scan of the first few lines, and a large generated tree is not
	// worth building a syntax tree for only to drop it.
	if idx.Cfg.SkipGeneratedFiles && gomarker.IsGenerated(src) {
		return nil
	}

	// SkipObjectResolution: the deprecated Ident.Obj graph is not used, and
	// building it on 400+ files is pure cost.
	af, err := parser.ParseFile(idx.Fset, abs, src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return fmt.Errorf("parse %s: %w", rel, err)
	}

	dir := path.Dir(rel)
	importPath := idx.ModulePath
	if dir != "." {
		importPath += "/" + dir
	}
	pkg := idx.Pkgs[importPath]
	if pkg == nil {
		pkg = &Package{Dir: dir, ImportPath: importPath, Decls: map[string]*File{}}
		idx.Pkgs[importPath] = pkg
	}

	f := &File{Abs: abs, Rel: rel, Src: src, AST: af, Pkg: pkg}
	f.Line = func(p token.Pos) int { return idx.Fset.Position(p).Line }
	pkg.Files = append(pkg.Files, f)
	idx.Files = append(idx.Files, f)
	return nil
}

func (p *Package) indexDecls() {
	for _, f := range p.Files {
		for _, d := range f.AST.Decls {
			switch d := d.(type) {
			case *ast.FuncDecl:
				// Methods are only reachable through a value, which this
				// scanner cannot type-resolve, so only plain funcs are indexed.
				if d.Recv == nil {
					p.Decls[d.Name.Name] = f
				}
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					switch s := spec.(type) {
					case *ast.TypeSpec:
						p.Decls[s.Name.Name] = f
					case *ast.ValueSpec:
						for _, n := range s.Names {
							p.Decls[n.Name] = f
						}
					}
				}
			}
		}
	}
}

// Imports maps each import's local name in this file to its import path.
func (f *File) Imports() map[string]string {
	out := make(map[string]string, len(f.AST.Imports))
	for _, imp := range f.AST.Imports {
		p, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		name := path.Base(p)
		if imp.Name != nil {
			if imp.Name.Name == "_" || imp.Name.Name == "." {
				continue
			}
			name = imp.Name.Name
		}
		out[name] = p
	}
	return out
}

func relSlash(root, p string) string {
	r, err := filepath.Rel(root, p)
	if err != nil {
		return filepath.ToSlash(p)
	}
	return filepath.ToSlash(r)
}

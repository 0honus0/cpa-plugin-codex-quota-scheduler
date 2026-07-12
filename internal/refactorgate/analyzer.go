package refactorgate

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

type Violation struct {
	File   string `json:"file"`
	Type   string `json:"type"`
	Symbol string `json:"symbol"`
	Count  int    `json:"count"`
}

type function struct {
	file    string
	imports map[string]string
	decl    *ast.FuncDecl
}

func Analyze(root, entry string) ([]Violation, error) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, root, func(info os.FileInfo) bool {
		return !strings.HasSuffix(info.Name(), "_test.go")
	}, 0)
	if err != nil {
		return nil, err
	}
	if len(pkgs) == 0 {
		return nil, fmt.Errorf("no Go package in %s", root)
	}
	var pkg *ast.Package
	for _, candidate := range pkgs {
		pkg = candidate
		break
	}
	functions := map[string][]function{}
	for path, file := range pkg.Files {
		imports := map[string]string{}
		for _, spec := range file.Imports {
			importPath, _ := strconv.Unquote(spec.Path.Value)
			name := filepath.Base(importPath)
			if spec.Name != nil {
				name = spec.Name.Name
			}
			imports[name] = importPath
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		for _, decl := range file.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Body != nil {
				functions[fn.Name.Name] = append(functions[fn.Name.Name], function{file: rel, imports: imports, decl: fn})
			}
		}
	}
	if len(functions[entry]) == 0 {
		return nil, fmt.Errorf("entry %s not found", entry)
	}
	queue := []string{entry}
	visited := map[string]bool{}
	counts := map[string]*Violation{}
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		if visited[name] {
			continue
		}
		visited[name] = true
		for _, fn := range functions[name] {
			ast.Inspect(fn.decl.Body, func(node ast.Node) bool {
				switch expr := node.(type) {
				case *ast.CallExpr:
					switch callee := expr.Fun.(type) {
					case *ast.Ident:
						if len(functions[callee.Name]) > 0 {
							queue = append(queue, callee.Name)
						}
						if callee.Name == "DetectHostRoster" || callee.Name == "ListHostAuths" {
							add(counts, fn.file, "host_roster", callee.Name)
						}
					case *ast.SelectorExpr:
						if len(functions[callee.Sel.Name]) > 0 {
							queue = append(queue, callee.Sel.Name)
						}
						if callee.Sel.Name == "Wait" {
							add(counts, fn.file, "blocking_wait", "sync.WaitGroup.Wait")
						}
						if callee.Sel.Name == "ListAuths" || callee.Sel.Name == "ListHostAuths" {
							add(counts, fn.file, "host_roster", callee.Sel.Name)
						}
					}
				case *ast.SelectorExpr:
					ident, ok := expr.X.(*ast.Ident)
					if !ok {
						break
					}
					path := fn.imports[ident.Name]
					symbol := path + "." + expr.Sel.Name
					switch path {
					case "net/http":
						add(counts, fn.file, "network", symbol)
					case "os":
						if expr.Sel.Name == "Open" || expr.Sel.Name == "ReadFile" || expr.Sel.Name == "WriteFile" {
							add(counts, fn.file, "disk", symbol)
						}
					case "time":
						if expr.Sel.Name == "Sleep" {
							add(counts, fn.file, "sleep", symbol)
						}
					}
					if expr.Sel.Name == "MethodHostAuthList" {
						add(counts, fn.file, "host_roster", symbol)
					}
				}
				return true
			})
		}
	}
	result := make([]Violation, 0, len(counts))
	for _, violation := range counts {
		result = append(result, *violation)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].File != result[j].File {
			return result[i].File < result[j].File
		}
		if result[i].Type != result[j].Type {
			return result[i].Type < result[j].Type
		}
		return result[i].Symbol < result[j].Symbol
	})
	return result, nil
}

func add(counts map[string]*Violation, file, kind, symbol string) {
	key := file + "|" + kind + "|" + symbol
	if counts[key] == nil {
		counts[key] = &Violation{File: file, Type: kind, Symbol: symbol}
	}
	counts[key].Count++
}

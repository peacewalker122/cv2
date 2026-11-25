package main

import (
	"go/ast"
	"os"
	"path/filepath"
	"strings"
)

func escalatedReferences(root, file string, line, col int, name string, hits []hit, bad []unresolved) (answer, error) {
	esc, outside, err := gopls(root, "references", file, line, col)
	if err != nil {
		if hits == nil {
			hits = []hit{}
		}
		if bad == nil {
			bad = []unresolved{}
		}
		return answer{Query: "references " + name, Results: dedupe(hits), Unresolved: append(bad, outside...)}, nil
	}
	return answer{Query: "references " + name, Results: tagConfirmed(esc, name), Unresolved: outsideOrEmpty(outside)}, nil
}

func tagConfirmed(hits []hit, name string) []hit {
	if hits == nil {
		return []hit{}
	}
	for i := range hits {
		if name != "" && hits[i].Symbol == "" {
			hits[i].Symbol = name
			hits[i].Signature = name
		}
		hits[i].Confidence = "lsp-confirmed"
		hits[i].Source = "lsp"
	}
	return hits
}

func outsideOrEmpty(items []unresolved) []unresolved {
	if items == nil {
		return []unresolved{}
	}
	return items
}

func readModule(root string) string {
	text, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(text), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "module" {
			return fields[1]
		}
	}
	return ""
}

func pkgImport(module, dir string) string {
	if dir == "." || dir == "" {
		return module
	}
	return module + "/" + dir
}

func importDir(module, path string) string {
	if path == module {
		return "."
	}
	prefix := module + "/"
	if module != "" && strings.HasPrefix(path, prefix) {
		return path[len(prefix):]
	}
	return ""
}

func importsDefining(pf *parsed, module, dir string) bool {
	want := pkgImport(module, dir)
	for _, path := range pf.imports {
		if path == want {
			return true
		}
		if module == "" && dir != "." && strings.HasSuffix(path, "/"+dir) {
			return true
		}
	}
	return false
}

func outsideModule(path, module string) bool {
	if module == "" || path == "" {
		return false
	}
	return path != module && !strings.HasPrefix(path, module+"/")
}

func resolveSymbol(pf *parsed, ident *ast.Ident, defs []binding, module string) (binding, []unresolved) {
	pos := pf.fset.Position(ident.Pos())
	if b := bindingAt(defs, pf.rel, pos.Line, pos.Column, ident.Name); b != nil {
		return *b, nil
	}
	if sel, ok := parentMap(pf.file)[ident].(*ast.SelectorExpr); ok && sel.Sel == ident {
		return resolveSelector(pf, sel, defs, module)
	}
	var matches []binding
	for _, b := range defs {
		if b.name == ident.Name && b.pkgDir == pf.dir && b.pkgName == pf.pkgName && b.kind != "method" {
			matches = append(matches, b)
		}
	}
	return one(pf, ident, matches)
}

func resolveSelector(pf *parsed, sel *ast.SelectorExpr, defs []binding, module string) (binding, []unresolved) {
	name := sel.Sel.Name
	if id, ok := sel.X.(*ast.Ident); ok {
		pos := pf.fset.Position(id.Pos())
		if resolveLocal(pf, pos.Line, pos.Column, id.Name) == nil {
			if path, ok := pf.imports[id.Name]; ok {
				if outsideModule(path, module) {
					return binding{}, []unresolved{{File: pf.rel, Range: identRange(pf, sel.Sel), Reason: "outside module"}}
				}
				dir := importDir(module, path)
				var matches []binding
				for _, b := range defs {
					if b.name == name && b.pkgDir == dir && b.kind != "method" {
						matches = append(matches, b)
					}
				}
				return one(pf, sel.Sel, matches)
			}
		}
		if typ := localType(pf, id); typ != "" {
			var matches []binding
			for _, b := range defs {
				if b.kind == "method" && b.name == name && b.pkgDir == pf.dir && b.pkgName == pf.pkgName && b.recv == typ {
					matches = append(matches, b)
				}
			}
			if len(matches) > 0 {
				return one(pf, sel.Sel, matches)
			}
		}
	}
	var concretes []binding
	for _, b := range defs {
		if b.kind == "method" && !b.iface && b.name == name && b.pkgDir == pf.dir && b.pkgName == pf.pkgName {
			concretes = append(concretes, b)
		}
	}
	if len(concretes) == 1 {
		return concretes[0], nil
	}
	if len(concretes) > 1 {
		return binding{}, ambiguous(pf, sel.Sel, recvNames(concretes))
	}
	var imported []binding
	seen := map[string]bool{}
	for _, b := range defs {
		if b.kind != "method" || b.iface || b.name != name || !importsDefining(pf, module, b.pkgDir) {
			continue
		}
		key := b.pkgDir + ":" + b.recv
		if seen[key] {
			continue
		}
		seen[key] = true
		imported = append(imported, b)
	}
	return one(pf, sel.Sel, imported)
}

func one(pf *parsed, ident *ast.Ident, matches []binding) (binding, []unresolved) {
	if len(matches) == 1 {
		return matches[0], nil
	}
	if len(matches) == 0 {
		return binding{}, []unresolved{{File: pf.rel, Range: identRange(pf, ident), Reason: "unknown"}}
	}
	return binding{}, ambiguous(pf, ident, recvNames(matches))
}

func ambiguous(pf *parsed, ident *ast.Ident, candidates []string) []unresolved {
	return []unresolved{{File: pf.rel, Range: identRange(pf, ident), Reason: "ambiguous", Candidates: candidates}}
}

func recvNames(items []binding) []string {
	var out []string
	for _, b := range items {
		label := b.pkgDir + "." + b.name
		if b.recv != "" {
			label = b.pkgDir + "." + b.recv + "." + b.name
		}
		out = append(out, label)
	}
	return out
}

func localType(pf *parsed, id *ast.Ident) string {
	pos := pf.fset.Position(id.Pos())
	b := resolveLocal(pf, pos.Line, pos.Column, id.Name)
	if b == nil {
		return ""
	}
	return b.signature
}

func collectUses(files []*parsed, defs []binding, target binding, module string) ([]hit, []unresolved) {
	family := []binding{target}
	if target.kind == "method" {
		family = methodFamily(defs, target)
	}
	var hits []hit
	var bad []unresolved
	hits = append(hits, refHit(target.file, target.name, target.line, target.col))
	for _, member := range family {
		hits = append(hits, refHit(member.file, member.name, member.line, member.col))
	}
	for _, pf := range files {
		if pf.dir == target.pkgDir && pf.pkgName == target.pkgName {
			found, amb := samePkgUses(pf, defs, family, target)
			hits = append(hits, found...)
			bad = append(bad, amb...)
			continue
		}
		if !importsDefining(pf, module, target.pkgDir) {
			continue
		}
		hits = append(hits, crossSelectors(pf, target.name, pkgImport(module, target.pkgDir))...)
	}
	return hits, bad
}

func methodFamily(defs []binding, target binding) []binding {
	var family []binding
	seen := map[string]bool{}
	add := func(b binding) {
		key := b.file + ":" + itoa(b.line) + ":" + itoa(b.col)
		if seen[key] {
			return
		}
		seen[key] = true
		family = append(family, b)
	}
	add(target)
	for _, b := range defs {
		if b.pkgDir != target.pkgDir || b.kind != "method" || b.name != target.name || b.arity != target.arity {
			continue
		}
		if target.iface || b.iface {
			add(b)
		}
	}
	return family
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [16]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

func samePkgUses(pf *parsed, defs []binding, family []binding, target binding) ([]hit, []unresolved) {
	parents := parentMap(pf.file)
	defAt := map[string]bool{}
	recvOK := map[string]bool{}
	for _, b := range family {
		defAt[b.file+":"+itoa(b.line)+":"+itoa(b.col)] = true
		if b.recv != "" {
			recvOK[b.recv] = true
		}
	}
	rivals := 0
	for _, b := range defs {
		if b.kind == "method" && !b.iface && b.name == target.name && b.arity == target.arity && b.pkgDir == target.pkgDir && !recvOK[b.recv] {
			rivals++
		}
	}
	var hits []hit
	var bad []unresolved
	ast.Inspect(pf.file, func(n ast.Node) bool {
		id, ok := n.(*ast.Ident)
		if !ok || id.Name != target.name {
			return true
		}
		pos := pf.fset.Position(id.Pos())
		if defAt[pf.rel+":"+itoa(pos.Line)+":"+itoa(pos.Column)] {
			hits = append(hits, refHit(pf.rel, id.Name, pos.Line, pos.Column))
			return true
		}
		sel, isSel := parents[id].(*ast.SelectorExpr)
		if target.kind != "method" {
			if isSel {
				return true
			}
			if resolveLocal(pf, pos.Line, pos.Column, id.Name) != nil {
				return true
			}
			hits = append(hits, refHit(pf.rel, id.Name, pos.Line, pos.Column))
			return true
		}
		if !isSel || sel.Sel != id {
			return true
		}
		typ := selectorRecv(pf, sel)
		if typ != "" {
			if recvOK[typ] {
				hits = append(hits, refHit(pf.rel, id.Name, pos.Line, pos.Column))
			}
			return true
		}
		if rivals == 0 {
			hits = append(hits, refHit(pf.rel, id.Name, pos.Line, pos.Column))
			return true
		}
		bad = append(bad, unresolved{
			File: pf.rel, Range: identRange(pf, id), Reason: "ambiguous", Candidates: recvNames(family),
		})
		return true
	})
	return hits, bad
}

func selectorRecv(pf *parsed, sel *ast.SelectorExpr) string {
	id, ok := sel.X.(*ast.Ident)
	if !ok {
		return ""
	}
	return localType(pf, id)
}

func crossSelectors(pf *parsed, name, ourPath string) []hit {
	var hits []hit
	ast.Inspect(pf.file, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != name {
			return true
		}
		if id, ok := sel.X.(*ast.Ident); ok {
			pos := pf.fset.Position(id.Pos())
			if resolveLocal(pf, pos.Line, pos.Column, id.Name) == nil {
				if path, ok := pf.imports[id.Name]; ok && path != ourPath {
					return true
				}
			}
		}
		pos := pf.fset.Position(sel.Sel.Pos())
		hits = append(hits, refHit(pf.rel, name, pos.Line, pos.Column))
		return true
	})
	return hits
}

func refHit(file, name string, line, col int) hit {
	return hit{
		Symbol: name, File: file, Range: [4]int{line, col, line, col + len(name)},
		Kind: "reference", Signature: name, Confidence: "resolved", Source: "go/ast",
	}
}

func parentMap(f *ast.File) map[ast.Node]ast.Node {
	out := map[ast.Node]ast.Node{}
	var stack []ast.Node
	ast.Inspect(f, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		if len(stack) > 0 {
			out[n] = stack[len(stack)-1]
		}
		stack = append(stack, n)
		return true
	})
	return out
}

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

var (
	methodNameRe = regexp.MustCompile(`(?m)^\s*([A-Za-z_][A-Za-z0-9_]*)\s*\(`)
	receiverRe   = regexp.MustCompile(`^func\s+\(\s*(?:[A-Za-z_][A-Za-z0-9_]*\s+)?\*?([A-Za-z_][A-Za-z0-9_]*)\s*\)`)
	callHeadRe   = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*)\s*\(`)
)

type hit struct {
	Symbol     string `json:"symbol"`
	File       string `json:"file"`
	Range      [4]int `json:"range"`
	Kind       string `json:"kind"`
	Signature  string `json:"signature"`
	Confidence string `json:"confidence"`
	Source     string `json:"source"`
}

type unresolved struct {
	File       string   `json:"file"`
	Range      [4]int   `json:"range"`
	Reason     string   `json:"reason"`
	Candidates []string `json:"candidates,omitempty"`
}

type answer struct {
	Query      string       `json:"query"`
	Results    []hit        `json:"results"`
	Unresolved []unresolved `json:"unresolved"`
}

type binding struct {
	id        string
	name      string
	kind      string
	file      string
	line      int
	col       int
	endLine   int
	signature string
	pkgDir    string
	pkgName   string
	recv      string
	arity     int
	iface     bool
}

type parsed struct {
	rel     string
	dir     string
	pkgName string
	fset    *token.FileSet
	file    *ast.File
	imports map[string]string
}

func findSymbol(root, name string) (answer, error) {
	if hits, err := treeSitterSymbols(root, name); err == nil && len(hits) > 0 {
		return answer{Query: "find_symbol " + name, Results: dedupe(hits), Unresolved: []unresolved{}}, nil
	}
	files, err := candidateFiles(root, name)
	if err != nil {
		return answer{}, err
	}
	var results []hit
	for _, pf := range files {
		for _, b := range declarations(pf) {
			if b.name == name && b.kind != "local" {
				results = append(results, hitFrom(b, "resolved", "go/ast"))
			}
		}
	}
	if results == nil {
		results = []hit{}
	}
	return answer{Query: "find_symbol " + name, Results: dedupe(results), Unresolved: []unresolved{}}, nil
}

func treeSitterSymbols(root, name string) ([]hit, error) {
	patterns := []string{
		"func " + name + "($$$) $$$",
		"func ($$$) " + name + "($$$) $$$",
		"type " + name + " $_",
	}
	var hitsOut []hit
	for _, pattern := range patterns {
		cmd := exec.Command("ast-grep", "run", "-l", "Go", "-p", pattern, "--json=stream", ".")
		cmd.Dir = root
		out, err := cmd.Output()
		if err != nil {
			var exit *exec.ExitError
			if errors.As(err, &exit) && exit.ExitCode() == 1 {
				continue
			}
			return nil, err
		}
		for _, line := range bytes.Split(bytes.TrimSpace(out), []byte("\n")) {
			if len(line) == 0 {
				continue
			}
			var node struct {
				File  string `json:"file"`
				Range struct {
					Start struct {
						Line   int `json:"line"`
						Column int `json:"column"`
					} `json:"start"`
					End struct {
						Line   int `json:"line"`
						Column int `json:"column"`
					} `json:"end"`
				} `json:"range"`
			}
			if err := json.Unmarshal(line, &node); err != nil {
				return nil, err
			}
			hitsOut = append(hitsOut, hit{
				Symbol: name, File: strings.TrimPrefix(filepath.ToSlash(node.File), "./"),
				Range: [4]int{node.Range.Start.Line + 1, node.Range.Start.Column + 1, node.Range.End.Line + 1, node.Range.End.Column + 1},
				Kind:  "symbol", Signature: name, Confidence: "resolved", Source: "treesitter",
			})
		}
	}
	return hitsOut, nil
}

func treeSitterLines(root, name string) map[string]bool {
	lines := map[string]bool{}
	cmd := exec.Command("ast-grep", "run", "-l", "Go", "-p", name, "--json=stream", ".")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return lines
	}
	for _, raw := range bytes.Split(bytes.TrimSpace(out), []byte("\n")) {
		if len(raw) == 0 {
			continue
		}
		var node struct {
			File  string `json:"file"`
			Range struct {
				Start struct {
					Line int `json:"line"`
				} `json:"start"`
			} `json:"range"`
		}
		if json.Unmarshal(raw, &node) != nil {
			continue
		}
		file := strings.TrimPrefix(filepath.ToSlash(node.File), "./")
		lines[file+":"+fmt.Sprint(node.Range.Start.Line+1)] = true
	}
	return lines
}

func treeSitterReferences(root, file string, line, col int) (answer, bool, error) {
	name := identText(root, file, line, col)
	if name == "" {
		return answer{}, false, nil
	}
	if ans, ok, err := treeSitterLocalRefs(root, file, line, col, name); ok || err != nil {
		return ans, ok, err
	}
	decls, err := treeSitterSymbols(root, name)
	if err != nil || len(decls) == 0 {
		return answer{}, false, err
	}
	anchorDir := filepath.ToSlash(filepath.Dir(file))
	imported := importedDirs(root, file)
	var declDirs []string
	for _, decl := range decls {
		dir := filepath.ToSlash(filepath.Dir(decl.File))
		if dir == anchorDir || dirListed(dir, imported) {
			declDirs = append(declDirs, dir)
		}
	}
	if len(declDirs) == 0 {
		return answer{}, false, nil
	}
	idents, err := treeSitterIdents(root, name)
	if err != nil {
		return answer{}, false, err
	}
	var kept []hit
	for _, item := range idents {
		dir := filepath.ToSlash(filepath.Dir(item.File))
		if dirListed(dir, declDirs) || fileImportsAny(root, item.File, declDirs) {
			item.Kind = "reference"
			item.Confidence = "resolved"
			item.Source = "treesitter"
			kept = append(kept, item)
		}
	}
	if len(kept) == 0 {
		return answer{}, false, nil
	}
	return answer{Query: "references " + name, Results: dedupe(kept), Unresolved: []unresolved{}}, true, nil
}

func identText(root, file string, line, col int) string {
	text, err := os.ReadFile(filepath.Join(root, file))
	if err != nil {
		return ""
	}
	lines := strings.Split(string(text), "\n")
	if line < 1 || line > len(lines) {
		return ""
	}
	raw := lines[line-1]
	if col < 1 || col > len(raw) {
		return ""
	}
	i := col - 1
	for i > 0 && isIdent(raw[i-1]) {
		i--
	}
	j := col - 1
	for j < len(raw) && isIdent(raw[j]) {
		j++
	}
	if i == j {
		return ""
	}
	return raw[i:j]
}

func isIdent(b byte) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

func treeSitterIdents(root, name string) ([]hit, error) {
	list := exec.Command("rg", "-l", "-w", "--glob", "*.go", name, ".")
	list.Dir = root
	listed, err := list.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 {
			return nil, nil
		}
		return nil, err
	}
	var hitsOut []hit
	for _, rel := range bytes.Split(bytes.TrimSpace(listed), []byte("\n")) {
		if len(rel) == 0 {
			continue
		}
		found, err := identsInFile(root, string(rel), name)
		if err != nil {
			return nil, err
		}
		hitsOut = append(hitsOut, found...)
	}
	return hitsOut, nil
}

func identsInFile(root, file, name string) ([]hit, error) {
	cmd := exec.Command("ast-grep", "run", "-l", "Go", "-p", "$ID", "--json=stream", file)
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 {
			return nil, nil
		}
		return nil, err
	}
	var hitsOut []hit
	for _, raw := range bytes.Split(bytes.TrimSpace(out), []byte("\n")) {
		if len(raw) == 0 {
			continue
		}
		var node struct {
			Text  string `json:"text"`
			File  string `json:"file"`
			Range struct {
				Start struct {
					Line   int `json:"line"`
					Column int `json:"column"`
				} `json:"start"`
			} `json:"range"`
		}
		if json.Unmarshal(raw, &node) != nil || node.Text != name {
			continue
		}
		hitsOut = append(hitsOut, hit{
			Symbol: name,
			File:   strings.TrimPrefix(filepath.ToSlash(node.File), "./"),
			Range:  [4]int{node.Range.Start.Line + 1, node.Range.Start.Column + 1, node.Range.Start.Line + 1, node.Range.Start.Column + 1 + len(name)},
			Source: "treesitter",
		})
	}
	return hitsOut, nil
}

func treeSitterImplementers(root, name string) (answer, bool, error) {
	cmd := exec.Command("ast-grep", "run", "-l", "Go", "-p", "type "+name+" interface { $$$ }", "--json=stream", ".")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 {
			return answer{}, false, nil
		}
		return answer{}, false, err
	}
	var methods []string
	for _, raw := range bytes.Split(bytes.TrimSpace(out), []byte("\n")) {
		if len(raw) == 0 {
			continue
		}
		var node struct {
			Text string `json:"text"`
		}
		if json.Unmarshal(raw, &node) != nil {
			continue
		}
		for _, match := range methodNameRe.FindAllStringSubmatch(node.Text, -1) {
			methods = append(methods, match[1])
		}
	}
	if len(methods) == 0 {
		return answer{}, false, nil
	}
	have := map[string]hit{}
	for _, method := range methods {
		cmd = exec.Command("ast-grep", "run", "-l", "Go", "-p", "func ($R) "+method+"($$$) $$$", "--json=stream", ".")
		cmd.Dir = root
		out, err = cmd.Output()
		if err != nil {
			continue
		}
		for _, raw := range bytes.Split(bytes.TrimSpace(out), []byte("\n")) {
			if len(raw) == 0 {
				continue
			}
			var node struct {
				Text  string `json:"text"`
				File  string `json:"file"`
				Range struct {
					Start struct {
						Line   int `json:"line"`
						Column int `json:"column"`
					} `json:"start"`
				} `json:"range"`
			}
			if json.Unmarshal(raw, &node) != nil {
				continue
			}
			recv := receiverName(node.Text)
			if recv == "" {
				continue
			}
			if _, ok := have[recv]; !ok {
				have[recv] = hit{
					Symbol: recv, File: strings.TrimPrefix(filepath.ToSlash(node.File), "./"),
					Range: [4]int{node.Range.Start.Line + 1, node.Range.Start.Column + 1, node.Range.Start.Line + 1, node.Range.Start.Column + 1},
					Kind:  "implementer", Signature: recv, Confidence: "resolved", Source: "treesitter",
				}
			}
		}
	}
	if len(have) == 0 {
		return answer{}, false, nil
	}
	var results []hit
	for _, item := range have {
		results = append(results, item)
	}
	return answer{Query: "find_implementers " + name, Results: results, Unresolved: []unresolved{}}, true, nil
}

func treeSitterCallsOut(root, file string, line int) (answer, bool, error) {
	cmd := exec.Command("ast-grep", "run", "-l", "Go", "-p", "func $N($$$) $$$", "--json=stream", file)
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return answer{}, false, nil
	}
	var owner struct {
		start int
		end   int
	}
	found := false
	for _, raw := range bytes.Split(bytes.TrimSpace(out), []byte("\n")) {
		if len(raw) == 0 {
			continue
		}
		var node struct {
			Range struct {
				Start struct {
					Line int `json:"line"`
				} `json:"start"`
				End struct {
					Line int `json:"line"`
				} `json:"end"`
			} `json:"range"`
		}
		if json.Unmarshal(raw, &node) != nil {
			continue
		}
		if line-1 >= node.Range.Start.Line && line-1 <= node.Range.End.Line {
			owner.start = node.Range.Start.Line
			owner.end = node.Range.End.Line
			found = true
		}
	}
	if !found {
		return answer{}, false, nil
	}
	cmd = exec.Command("ast-grep", "run", "-l", "Go", "-p", "$C($$$A)", "--json=stream", file)
	cmd.Dir = root
	out, err = cmd.Output()
	if err != nil {
		return answer{}, false, nil
	}
	var results []hit
	for _, raw := range bytes.Split(bytes.TrimSpace(out), []byte("\n")) {
		if len(raw) == 0 {
			continue
		}
		var node struct {
			Text  string `json:"text"`
			File  string `json:"file"`
			Range struct {
				Start struct {
					Line   int `json:"line"`
					Column int `json:"column"`
				} `json:"start"`
			} `json:"range"`
		}
		if json.Unmarshal(raw, &node) != nil {
			continue
		}
		if node.Range.Start.Line < owner.start || node.Range.Start.Line > owner.end {
			continue
		}
		name := callHead(node.Text)
		if name == "" {
			continue
		}
		results = append(results, hit{
			Symbol: name, File: strings.TrimPrefix(filepath.ToSlash(node.File), "./"),
			Range: [4]int{node.Range.Start.Line + 1, node.Range.Start.Column + 1, node.Range.Start.Line + 1, node.Range.Start.Column + 1 + len(name)},
			Kind:  "call", Signature: name, Confidence: "resolved", Source: "treesitter",
		})
	}
	if len(results) == 0 {
		return answer{}, false, nil
	}
	return answer{Query: "calls_out", Results: dedupe(results), Unresolved: []unresolved{}}, true, nil
}

func receiverName(text string) string {
	match := receiverRe.FindStringSubmatch(text)
	if match == nil {
		return ""
	}
	return match[1]
}

func callHead(text string) string {
	match := callHeadRe.FindStringSubmatch(text)
	if match == nil {
		return ""
	}
	return match[1]
}

type sgSpan struct {
	file                string
	startLine, startCol int
	endLine, endCol     int
}

func (s sgSpan) contains(line, col int) bool {
	l, c := line-1, col-1
	if l < s.startLine || l > s.endLine {
		return false
	}
	if l == s.startLine && c < s.startCol {
		return false
	}
	if l == s.endLine && c > s.endCol {
		return false
	}
	return true
}

func treeSitterLocalRefs(root, file string, line, col int, name string) (answer, bool, error) {
	decls, err := sgSpans(root, file, name+" := $V")
	if err != nil || len(decls) == 0 {
		return answer{}, false, err
	}
	blocks, err := sgSpans(root, file, "{ $$$ }")
	if err != nil || len(blocks) == 0 {
		return answer{}, false, err
	}
	idents, err := treeSitterIdents(root, name)
	if err != nil {
		return answer{}, false, err
	}
	var scope *sgSpan
	for i := range blocks {
		if !blocks[i].contains(line, col) {
			continue
		}
		owns := false
		for _, decl := range decls {
			if blocks[i].contains(decl.startLine+1, decl.startCol+1) {
				owns = true
			}
		}
		if !owns {
			continue
		}
		if scope == nil || smaller(*scope, blocks[i]) {
			scope = &blocks[i]
		}
	}
	if scope == nil {
		return answer{}, false, nil
	}
	var chosen *sgSpan
	for i := range decls {
		if !scope.contains(decls[i].startLine+1, decls[i].startCol+1) {
			continue
		}
		if decls[i].startLine+1 > line {
			continue
		}
		if chosen == nil || decls[i].startLine > chosen.startLine {
			chosen = &decls[i]
		}
	}
	if chosen == nil {
		return answer{}, false, nil
	}
	var kept []hit
	for _, item := range idents {
		if item.File != file {
			continue
		}
		useLine, useCol := item.Range[0], item.Range[1]
		if !scope.contains(useLine, useCol) {
			continue
		}
		if useLine < chosen.startLine+1 {
			continue
		}
		nested := false
		for _, block := range blocks {
			if block == *scope || !scope.contains(block.startLine+1, block.startCol+1) {
				continue
			}
			if !block.contains(useLine, useCol) {
				continue
			}
			for _, decl := range decls {
				if block.contains(decl.startLine+1, decl.startCol+1) && decl.startLine != chosen.startLine {
					nested = true
				}
			}
		}
		if nested {
			continue
		}
		item.Kind = "reference"
		item.Confidence = "resolved"
		item.Source = "treesitter"
		kept = append(kept, item)
	}
	if len(kept) == 0 {
		return answer{}, false, nil
	}
	return answer{Query: "references " + name, Results: dedupe(kept), Unresolved: []unresolved{}}, true, nil
}

func smaller(a, b sgSpan) bool {
	return (b.endLine - b.startLine) < (a.endLine - a.startLine)
}

func sgSpans(root, file, pattern string) ([]sgSpan, error) {
	cmd := exec.Command("ast-grep", "run", "-l", "Go", "-p", pattern, "--json=stream", file)
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 {
			return nil, nil
		}
		return nil, err
	}
	var spans []sgSpan
	for _, raw := range bytes.Split(bytes.TrimSpace(out), []byte("\n")) {
		if len(raw) == 0 {
			continue
		}
		var node struct {
			File  string `json:"file"`
			Range struct {
				Start struct {
					Line   int `json:"line"`
					Column int `json:"column"`
				} `json:"start"`
				End struct {
					Line   int `json:"line"`
					Column int `json:"column"`
				} `json:"end"`
			} `json:"range"`
		}
		if json.Unmarshal(raw, &node) != nil {
			continue
		}
		spans = append(spans, sgSpan{
			file:      strings.TrimPrefix(filepath.ToSlash(node.File), "./"),
			startLine: node.Range.Start.Line, startCol: node.Range.Start.Column,
			endLine: node.Range.End.Line, endCol: node.Range.End.Column,
		})
	}
	return spans, nil
}

func treeSitterGetSymbol(root, spec string) (answer, bool, error) {
	file, line, _ := parsePos(spec)
	cmd := exec.Command("ast-grep", "run", "-l", "Go", "-p", "func $N($$$) $$$", "--json=stream", file)
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return answer{}, false, nil
	}
	for _, raw := range bytes.Split(bytes.TrimSpace(out), []byte("\n")) {
		if len(raw) == 0 {
			continue
		}
		var node struct {
			Text  string `json:"text"`
			File  string `json:"file"`
			Range struct {
				Start struct {
					Line   int `json:"line"`
					Column int `json:"column"`
				} `json:"start"`
				End struct {
					Line   int `json:"line"`
					Column int `json:"column"`
				} `json:"end"`
			} `json:"range"`
		}
		if json.Unmarshal(raw, &node) != nil {
			continue
		}
		if line-1 < node.Range.Start.Line || line-1 > node.Range.End.Line {
			continue
		}
		name := callHead(strings.TrimPrefix(node.Text, "func "))
		return answer{Query: "get_symbol " + spec, Results: []hit{{
			Symbol: name, File: strings.TrimPrefix(filepath.ToSlash(node.File), "./"),
			Range: [4]int{node.Range.Start.Line + 1, node.Range.Start.Column + 1, node.Range.End.Line + 1, node.Range.End.Column + 1},
			Kind:  "function", Signature: node.Text, Confidence: "resolved", Source: "treesitter",
		}}, Unresolved: []unresolved{}}, true, nil
	}
	return answer{}, false, nil
}

func treeSitterTypeOf(root, file string, line, col int) (answer, bool, error) {
	name := identText(root, file, line, col)
	if name == "" {
		return answer{}, false, nil
	}
	patterns := []string{
		"func $F(" + name + " $T, $$$)",
		"func $F(" + name + " $T)",
		"var " + name + " $T",
		name + " $T",
	}
	for _, pattern := range patterns {
		cmd := exec.Command("ast-grep", "run", "-l", "Go", "-p", pattern, "--json=stream", file)
		cmd.Dir = root
		out, err := cmd.Output()
		if err != nil {
			continue
		}
		for _, raw := range bytes.Split(bytes.TrimSpace(out), []byte("\n")) {
			if len(raw) == 0 {
				continue
			}
			var node struct {
				Text  string `json:"text"`
				Range struct {
					Start struct {
						Line   int `json:"line"`
						Column int `json:"column"`
					} `json:"start"`
				} `json:"range"`
			}
			if json.Unmarshal(raw, &node) != nil {
				continue
			}
			if node.Range.Start.Line+1 != line && !strings.Contains(node.Text, name+" ") {
				continue
			}
			typ := declaredTypeText(node.Text, name)
			if typ == "" {
				continue
			}
			return answer{Query: "type_of", Results: []hit{{
				Symbol: name, File: file, Range: [4]int{line, col, line, col + len(name)},
				Kind: "type", Signature: typ, Confidence: "resolved", Source: "treesitter",
			}}, Unresolved: []unresolved{}}, true, nil
		}
	}
	return answer{}, false, nil
}

func declaredTypeText(text, name string) string {
	match := regexp.MustCompile(`(?:^|\s)` + regexp.QuoteMeta(name) + `\s+\*?([A-Za-z_][A-Za-z0-9_\.]*)`).FindStringSubmatch(text)
	if match == nil {
		return ""
	}
	return match[1]
}

func treeSitterCallsIn(root, file string, line, col int) (answer, bool, error) {
	name := identText(root, file, line, col)
	if name == "" {
		return answer{}, false, nil
	}
	cmd := exec.Command("ast-grep", "run", "-l", "Go", "-p", name+"($_)", "--json=stream", ".")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return answer{}, false, nil
	}
	var results []hit
	for _, raw := range bytes.Split(bytes.TrimSpace(out), []byte("\n")) {
		if len(raw) == 0 {
			continue
		}
		var node struct {
			Text  string `json:"text"`
			File  string `json:"file"`
			Range struct {
				Start struct {
					Line   int `json:"line"`
					Column int `json:"column"`
				} `json:"start"`
			} `json:"range"`
		}
		if json.Unmarshal(raw, &node) != nil || strings.HasPrefix(node.Text, "func") {
			continue
		}
		results = append(results, hit{
			Symbol: name, File: strings.TrimPrefix(filepath.ToSlash(node.File), "./"),
			Range: [4]int{node.Range.Start.Line + 1, node.Range.Start.Column + 1, node.Range.Start.Line + 1, node.Range.Start.Column + 1 + len(name)},
			Kind:  "call", Signature: name, Confidence: "resolved", Source: "treesitter",
		})
	}
	if len(results) == 0 {
		return answer{}, false, nil
	}
	return answer{Query: "calls_in", Results: dedupe(results), Unresolved: []unresolved{}}, true, nil
}

func dirListed(dir string, dirs []string) bool {
	for _, item := range dirs {
		if item == dir {
			return true
		}
	}
	return false
}

func importedDirs(root, file string) []string {
	mod := modulePath(root)
	var dirs []string
	for _, path := range importSpecs(root, file) {
		if mod != "" && strings.HasPrefix(path, mod+"/") {
			dirs = append(dirs, strings.TrimPrefix(path, mod+"/"))
		}
	}
	return dirs
}

func fileImportsAny(root, file string, dirs []string) bool {
	have := importedDirs(root, file)
	for _, dir := range dirs {
		if dirListed(dir, have) {
			return true
		}
	}
	return false
}

func importSpecs(root, file string) []string {
	cmd := exec.Command("ast-grep", "run", "-l", "Go", "-k", "import_spec", "--json=stream", file)
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	var paths []string
	for _, raw := range bytes.Split(bytes.TrimSpace(out), []byte("\n")) {
		if len(raw) == 0 {
			continue
		}
		var node struct {
			Text string `json:"text"`
		}
		if json.Unmarshal(raw, &node) != nil {
			continue
		}
		text := strings.Trim(node.Text, "\"` ")
		if i := strings.LastIndex(text, " "); i >= 0 {
			text = strings.Trim(text[i+1:], "\"` ")
		}
		if text != "" {
			paths = append(paths, text)
		}
	}
	return paths
}

func modulePath(root string) string {
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

func tagTreeSitter(root, name string, hits []hit) []hit {
	lines := treeSitterLines(root, name)
	for i := range hits {
		if lines[hits[i].File+":"+fmt.Sprint(hits[i].Range[0])] {
			hits[i].Source = "treesitter"
		}
	}
	return hits
}

func getSymbol(root, spec string) (answer, error) {
	if ans, ok, err := treeSitterGetSymbol(root, spec); ok || err != nil {
		return ans, err
	}
	file, line, col := parsePos(spec)
	text, err := os.ReadFile(filepath.Join(root, file))
	if err != nil {
		return answer{}, err
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, text, parser.ParseComments)
	if err != nil {
		return answer{}, err
	}
	var found ast.Node
	ast.Inspect(f, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok {
			return true
		}
		pos := fset.Position(fn.Pos())
		end := fset.Position(fn.End())
		if line >= pos.Line && line <= end.Line {
			found = fn
		}
		return true
	})
	if found == nil {
		return answer{Query: "get_symbol " + spec, Results: []hit{}, Unresolved: []unresolved{{File: file, Range: [4]int{line, col, line, col}, Reason: "no enclosing function"}}}, nil
	}
	fn := found.(*ast.FuncDecl)
	pos := fset.Position(fn.Pos())
	end := fset.Position(fn.End())
	body := string(text[fset.Position(fn.Pos()).Offset:fset.Position(fn.End()).Offset])
	return answer{Query: "get_symbol " + spec, Results: []hit{{
		Symbol: fn.Name.Name, File: file, Range: [4]int{pos.Line, pos.Column, end.Line, end.Column},
		Kind: "function", Signature: body, Confidence: "resolved", Source: "go/ast",
	}}, Unresolved: []unresolved{}}, nil
}

func definition(root, file string, line, col int) (answer, error) {
	pf, ident, err := identAt(root, file, line, col)
	if err != nil {
		return answer{}, err
	}
	if ident == nil {
		esc, _, err := gopls(root, "definition", file, line, col)
		if err != nil {
			return unknown(file, line, col, "no identifier"), nil
		}
		return answer{Query: "definition", Results: tagConfirmed(esc, ""), Unresolved: []unresolved{}}, nil
	}
	if local := resolveLocal(pf, line, col, ident.Name); local != nil {
		return answer{Query: "definition", Results: []hit{hitFrom(*local, "resolved", "go/ast")}, Unresolved: []unresolved{}}, nil
	}
	files, err := candidateFiles(root, ident.Name)
	if err != nil {
		return answer{}, err
	}
	if !containsFile(files, pf.rel) {
		files = append(files, pf)
	}
	target, bad := resolveSymbol(pf, ident, declarationsIn(files), readModule(root))
	if len(bad) > 0 {
		esc, _, err := gopls(root, "definition", file, line, col)
		if err != nil {
			return answer{Query: "definition", Results: []hit{}, Unresolved: bad}, nil
		}
		return answer{Query: "definition", Results: tagConfirmed(esc, ident.Name), Unresolved: []unresolved{}}, nil
	}
	found := tagTreeSitter(root, ident.Name, []hit{hitFrom(target, "resolved", "go/ast")})
	return answer{Query: "definition", Results: found, Unresolved: []unresolved{}}, nil
}

func typeOf(root, file string, line, col int) (answer, error) {
	if ans, ok, err := treeSitterTypeOf(root, file, line, col); ok || err != nil {
		return ans, err
	}
	pf, ident, err := identAt(root, file, line, col)
	if err != nil {
		return answer{}, err
	}
	if ident == nil {
		return unknown(file, line, col, "no identifier"), nil
	}
	sig := declaredType(pf, ident.Name)
	if sig == "" {
		esc, _, err := gopls(root, "definition", file, line, col)
		if err != nil {
			return unknown(file, line, col, "type not declared"), nil
		}
		return answer{Query: "type_of", Results: tagConfirmed(esc, ident.Name), Unresolved: []unresolved{}}, nil
	}
	return answer{Query: "type_of", Results: []hit{{
		Symbol: ident.Name, File: file, Range: identRange(pf, ident), Kind: "type", Signature: sig, Confidence: "resolved", Source: "go/ast",
	}}, Unresolved: []unresolved{}}, nil
}

func implementers(root, name string) (answer, error) {
	if ans, ok, err := treeSitterImplementers(root, name); ok || err != nil {
		return ans, err
	}
	files, err := goFiles(root)
	if err != nil {
		return answer{}, err
	}
	ifaces, concretes := methodSets(files)
	var results []hit
	for _, iface := range ifaces {
		if iface.name != name && iface.recv != name {
			continue
		}
		for _, c := range concretes {
			if covers(iface.methods, c.methods) {
				results = append(results, hit{
					Symbol: c.recv, File: c.file, Range: [4]int{c.line, c.col, c.line, c.col},
					Kind: "implementer", Signature: c.recv, Confidence: "resolved", Source: "go/ast",
				})
			}
		}
	}
	if results == nil {
		esc, err := goplsWorkspace(root, name)
		if err != nil {
			return answer{Query: "find_implementers " + name, Results: []hit{}, Unresolved: []unresolved{{Reason: "interface not resolved", Candidates: []string{name}}}}, nil
		}
		return answer{Query: "find_implementers " + name, Results: esc, Unresolved: []unresolved{}}, nil
	}
	return answer{Query: "find_implementers " + name, Results: dedupe(results), Unresolved: []unresolved{}}, nil
}

func references(root, file string, line, col int) (answer, error) {
	if ans, ok, err := treeSitterReferences(root, file, line, col); ok || err != nil {
		return ans, err
	}
	pf, ident, err := identAt(root, file, line, col)
	if err != nil {
		return answer{}, err
	}
	if ident == nil {
		return unknown(file, line, col, "no identifier"), nil
	}
	name := ident.Name
	files, err := candidateFiles(root, name)
	if err != nil {
		return answer{}, err
	}
	if !containsFile(files, pf.rel) {
		files = append(files, pf)
	}
	if local := resolveLocal(pf, line, col, name); local != nil {
		return answer{Query: "references", Results: dedupe(localUses(pf, local)), Unresolved: []unresolved{}}, nil
	}
	module := readModule(root)
	defs := declarationsIn(files)
	target, bad := resolveSymbol(pf, ident, defs, module)
	if len(bad) > 0 {
		return escalatedReferences(root, file, line, col, name, nil, bad)
	}
	hits, bad := collectUses(files, defs, target, module)
	if len(bad) > 0 {
		return escalatedReferences(root, file, line, col, name, hits, bad)
	}
	if hits == nil {
		hits = []hit{}
	}
	return answer{Query: "references " + name, Results: tagTreeSitter(root, name, dedupe(hits)), Unresolved: []unresolved{}}, nil
}

func calls(root, file string, line, col int, incoming bool) (answer, error) {
	if !incoming {
		if ans, ok, err := treeSitterCallsOut(root, file, line); ok || err != nil {
			return ans, err
		}
	} else if ans, ok, err := treeSitterCallsIn(root, file, line, col); ok || err != nil {
		return ans, err
	}
	pf, err := parseOne(root, file)
	if err != nil {
		return answer{}, err
	}
	var fn *ast.FuncDecl
	ast.Inspect(pf.file, func(n ast.Node) bool {
		decl, ok := n.(*ast.FuncDecl)
		if !ok || decl.Name == nil {
			return true
		}
		pos := pf.fset.Position(decl.Name.Pos())
		end := pf.fset.Position(decl.End())
		if line >= pos.Line && line <= end.Line {
			fn = decl
		}
		return true
	})
	if fn == nil {
		return unknown(file, line, col, "no function"), nil
	}
	if !incoming {
		var results []hit
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			id := callName(call.Fun)
			if id == nil {
				return true
			}
			pos := pf.fset.Position(id.Pos())
			results = append(results, hit{
				Symbol: id.Name, File: file, Range: identRange(pf, id), Kind: "call",
				Signature: id.Name, Confidence: "resolved", Source: "go/ast",
			})
			_ = pos
			return true
		})
		if results == nil {
			results = []hit{}
		}
		return answer{Query: "calls_out", Results: results, Unresolved: []unresolved{}}, nil
	}
	refs, err := references(root, file, pf.fset.Position(fn.Name.Pos()).Line, pf.fset.Position(fn.Name.Pos()).Column)
	if err != nil {
		return answer{}, err
	}
	refs.Query = "calls_in"
	return refs, nil
}

func check(root string) (answer, error) {
	var unresolvedItems []unresolved
	for _, args := range [][]string{{"build", "./..."}, {"vet", "./..."}} {
		cmd := exec.Command("go", args...)
		cmd.Dir = root
		out, err := cmd.CombinedOutput()
		if err == nil {
			continue
		}
		for _, line := range strings.Split(string(out), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			unresolvedItems = append(unresolvedItems, unresolved{Reason: line})
		}
	}
	if unresolvedItems == nil {
		unresolvedItems = []unresolved{}
	}
	return answer{Query: "check", Results: []hit{}, Unresolved: unresolvedItems}, nil
}

func unknown(file string, line, col int, reason string) answer {
	return answer{Query: reason, Results: []hit{}, Unresolved: []unresolved{{File: file, Range: [4]int{line, col, line, col}, Reason: reason}}}
}

func identAt(root, file string, line, col int) (*parsed, *ast.Ident, error) {
	pf, err := parseOne(root, file)
	if err != nil {
		return nil, nil, err
	}
	var found *ast.Ident
	ast.Inspect(pf.file, func(n ast.Node) bool {
		id, ok := n.(*ast.Ident)
		if !ok {
			return true
		}
		pos := pf.fset.Position(id.Pos())
		end := pf.fset.Position(id.End())
		if pos.Line == line && col >= pos.Column && col <= end.Column {
			found = id
		}
		return true
	})
	return pf, found, nil
}

func parseOne(root, rel string) (*parsed, error) {
	path := rel
	if !filepath.IsAbs(rel) {
		path = filepath.Join(root, rel)
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		return nil, err
	}
	relOut, err := filepath.Rel(root, path)
	if err != nil {
		relOut = rel
	}
	return &parsed{rel: filepath.ToSlash(relOut), dir: filepath.ToSlash(filepath.Dir(relOut)), pkgName: f.Name.Name, fset: fset, file: f, imports: importTable(f)}, nil
}

func importTable(f *ast.File) map[string]string {
	out := map[string]string{}
	for _, spec := range f.Imports {
		path := strings.Trim(spec.Path.Value, `"`)
		local := filepath.Base(path)
		if spec.Name != nil {
			local = spec.Name.Name
		}
		out[local] = path
	}
	return out
}

func candidateFiles(root, ident string) ([]*parsed, error) {
	cmd := exec.Command("rg", "-l", "-w", "--glob", "*.go", ident, ".")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 1 {
			return nil, nil
		}
		return nil, err
	}
	var files []*parsed
	for _, line := range bytes.Split(bytes.TrimSpace(out), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		rel := strings.TrimPrefix(filepath.ToSlash(string(line)), "./")
		pf, err := parseOne(root, rel)
		if err != nil {
			continue
		}
		files = append(files, pf)
	}
	return files, nil
}

func goFiles(root string) ([]*parsed, error) {
	var files []*parsed
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		pf, err := parseOne(root, rel)
		if err != nil {
			return nil
		}
		files = append(files, pf)
		return nil
	})
	return files, err
}

func declarations(pf *parsed) []binding {
	var out []binding
	for _, decl := range pf.file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Name == nil {
				continue
			}
			pos := pf.fset.Position(d.Name.Pos())
			b := binding{name: d.Name.Name, file: pf.rel, line: pos.Line, col: pos.Column, pkgDir: pf.dir, pkgName: pf.pkgName, kind: "function", signature: d.Name.Name, arity: fieldCount(d.Type.Params)}
			if d.Recv != nil && len(d.Recv.List) > 0 {
				b.kind = "method"
				b.recv = typeName(d.Recv.List[0].Type)
				b.arity = fieldCount(d.Type.Params)
			}
			b.id = b.kind + ":" + pf.dir + ":" + b.recv + ":" + b.name + ":" + fmt.Sprint(b.arity)
			out = append(out, b)
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok {
					continue
				}
				pos := pf.fset.Position(ts.Name.Pos())
				_, iface := ts.Type.(*ast.InterfaceType)
				b := binding{name: ts.Name.Name, file: pf.rel, line: pos.Line, col: pos.Column, pkgDir: pf.dir, pkgName: pf.pkgName, kind: "type", iface: iface, signature: ts.Name.Name}
				b.id = "type:" + pf.dir + ":" + b.name
				out = append(out, b)
				ifaceType, ok := ts.Type.(*ast.InterfaceType)
				if !ok || ifaceType.Methods == nil {
					continue
				}
				for _, method := range ifaceType.Methods.List {
					ft, ok := method.Type.(*ast.FuncType)
					if !ok || len(method.Names) == 0 {
						continue
					}
					mp := pf.fset.Position(method.Names[0].Pos())
					mb := binding{
						name: method.Names[0].Name, file: pf.rel, line: mp.Line, col: mp.Column,
						pkgDir: pf.dir, pkgName: pf.pkgName, kind: "method", iface: true,
						recv: ts.Name.Name, arity: fieldCount(ft.Params), signature: method.Names[0].Name,
					}
					mb.id = "method:" + pf.dir + ":" + mb.recv + ":" + mb.name + ":" + fmt.Sprint(mb.arity)
					out = append(out, mb)
				}
			}
		}
	}
	return out
}

func declarationsIn(files []*parsed) []binding {
	var out []binding
	for _, pf := range files {
		out = append(out, declarations(pf)...)
	}
	return out
}

func fieldCount(fields *ast.FieldList) int {
	if fields == nil {
		return 0
	}
	n := 0
	for _, f := range fields.List {
		if len(f.Names) == 0 {
			n++
			continue
		}
		n += len(f.Names)
	}
	return n
}

func typeName(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		return typeName(t.X)
	case *ast.SelectorExpr:
		return typeName(t.X) + "." + t.Sel.Name
	case *ast.IndexExpr:
		return typeName(t.X)
	default:
		return ""
	}
}

func bindingAt(defs []binding, file string, line, col int, name string) *binding {
	for i := range defs {
		b := &defs[i]
		if b.file == file && b.line == line && b.name == name {
			return b
		}
		if b.file == file && b.name == name && col >= b.col && col <= b.col+len(name) && b.line == line {
			return b
		}
	}
	return nil
}

func resolveLocal(pf *parsed, line, col int, name string) *binding {
	scopes := []map[string]*binding{{}}
	var found *binding
	var walkExpr func(ast.Expr)
	var walkStmt func(ast.Stmt)
	declare := func(id *ast.Ident, typ string) {
		if id == nil {
			return
		}
		pos := pf.fset.Position(id.Pos())
		b := &binding{name: id.Name, kind: "local", file: pf.rel, line: pos.Line, col: pos.Column, pkgDir: pf.dir, signature: typ}
		scopes[len(scopes)-1][id.Name] = b
		if pos.Line == line && col >= pos.Column && col <= pos.Column+len(id.Name) {
			found = b
		}
	}
	lookup := func(id *ast.Ident) {
		if id == nil {
			return
		}
		pos := pf.fset.Position(id.Pos())
		if pos.Line == line && col >= pos.Column && col <= pos.Column+len(id.Name) {
			for i := len(scopes) - 1; i >= 0; i-- {
				if b := scopes[i][id.Name]; b != nil {
					found = b
					return
				}
			}
		}
	}
	push := func() { scopes = append(scopes, map[string]*binding{}) }
	pop := func() { scopes = scopes[:len(scopes)-1] }
	walkExpr = func(e ast.Expr) {
		switch n := e.(type) {
		case *ast.Ident:
			lookup(n)
		case *ast.SelectorExpr:
			walkExpr(n.X)
		case *ast.CallExpr:
			walkExpr(n.Fun)
			for _, arg := range n.Args {
				walkExpr(arg)
			}
		case *ast.BinaryExpr:
			walkExpr(n.X)
			walkExpr(n.Y)
		case *ast.UnaryExpr:
			walkExpr(n.X)
		case *ast.ParenExpr:
			walkExpr(n.X)
		case *ast.StarExpr:
			walkExpr(n.X)
		case *ast.IndexExpr:
			walkExpr(n.X)
			walkExpr(n.Index)
		case *ast.KeyValueExpr:
			walkExpr(n.Value)
		case *ast.CompositeLit:
			for _, elt := range n.Elts {
				walkExpr(elt)
			}
		case *ast.FuncLit:
			push()
			if n.Type != nil && n.Type.Params != nil {
				for _, f := range n.Type.Params.List {
					for _, name := range f.Names {
						declare(name, typeName(f.Type))
					}
				}
			}
			if n.Body != nil {
				for _, st := range n.Body.List {
					walkStmt(st)
				}
			}
			pop()
		}
	}
	walkStmt = func(st ast.Stmt) {
		switch n := st.(type) {
		case *ast.BlockStmt:
			push()
			for _, s := range n.List {
				walkStmt(s)
			}
			pop()
		case *ast.DeclStmt:
			gd, ok := n.Decl.(*ast.GenDecl)
			if !ok {
				return
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				typ := ""
				if vs.Type != nil {
					typ = typeName(vs.Type)
				}
				for _, name := range vs.Names {
					declare(name, typ)
				}
			}
		case *ast.AssignStmt:
			if n.Tok.String() == ":=" {
				for _, lhs := range n.Lhs {
					if id, ok := lhs.(*ast.Ident); ok && id.Name != "_" {
						declare(id, "")
					}
				}
			} else {
				for _, lhs := range n.Lhs {
					walkExpr(lhs)
				}
			}
			for _, rhs := range n.Rhs {
				walkExpr(rhs)
			}
		case *ast.IfStmt:
			if n.Init != nil {
				walkStmt(n.Init)
			}
			walkExpr(n.Cond)
			walkStmt(n.Body)
			if n.Else != nil {
				walkStmt(n.Else)
			}
		case *ast.ForStmt:
			if n.Init != nil {
				walkStmt(n.Init)
			}
			if n.Cond != nil {
				walkExpr(n.Cond)
			}
			if n.Post != nil {
				walkStmt(n.Post)
			}
			walkStmt(n.Body)
		case *ast.RangeStmt:
			if id, ok := n.Key.(*ast.Ident); ok {
				declare(id, "")
			}
			if id, ok := n.Value.(*ast.Ident); ok {
				declare(id, "")
			}
			walkExpr(n.X)
			walkStmt(n.Body)
		case *ast.ReturnStmt:
			for _, r := range n.Results {
				walkExpr(r)
			}
		case *ast.ExprStmt:
			walkExpr(n.X)
		case *ast.GoStmt:
			walkExpr(n.Call)
		case *ast.DeferStmt:
			walkExpr(n.Call)
		case *ast.SwitchStmt:
			if n.Init != nil {
				walkStmt(n.Init)
			}
			if n.Tag != nil {
				walkExpr(n.Tag)
			}
			walkStmt(n.Body)
		case *ast.CaseClause:
			for _, e := range n.List {
				walkExpr(e)
			}
			for _, s := range n.Body {
				walkStmt(s)
			}
		case *ast.IncDecStmt:
			walkExpr(n.X)
		}
	}
	for _, decl := range pf.file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		push()
		if fn.Recv != nil {
			for _, f := range fn.Recv.List {
				for _, name := range f.Names {
					declare(name, typeName(f.Type))
				}
			}
		}
		if fn.Type.Params != nil {
			for _, f := range fn.Type.Params.List {
				for _, name := range f.Names {
					declare(name, typeName(f.Type))
				}
			}
		}
		for _, st := range fn.Body.List {
			walkStmt(st)
		}
		pop()
	}
	return found
}

func localUses(pf *parsed, target *binding) []hit {
	var out []hit
	scopes := []map[string]*binding{{}}
	var walkExpr func(ast.Expr)
	var walkStmt func(ast.Stmt)
	declare := func(id *ast.Ident) *binding {
		pos := pf.fset.Position(id.Pos())
		b := &binding{name: id.Name, kind: "local", file: pf.rel, line: pos.Line, col: pos.Column}
		scopes[len(scopes)-1][id.Name] = b
		if pos.Line == target.line && pos.Column == target.col {
			return target
		}
		return b
	}
	matches := func(id *ast.Ident) bool {
		for i := len(scopes) - 1; i >= 0; i-- {
			if b := scopes[i][id.Name]; b != nil {
				return b.line == target.line && b.col == target.col && b.name == target.name
			}
		}
		return false
	}
	emit := func(id *ast.Ident) {
		if id == nil || !matches(id) {
			return
		}
		pos := pf.fset.Position(id.Pos())
		out = append(out, hit{
			Symbol: id.Name, File: pf.rel, Range: [4]int{pos.Line, pos.Column, pos.Line, pos.Column + len(id.Name)},
			Kind: "reference", Signature: id.Name, Confidence: "resolved", Source: "go/ast",
		})
	}
	push := func() { scopes = append(scopes, map[string]*binding{}) }
	pop := func() { scopes = scopes[:len(scopes)-1] }
	walkExpr = func(e ast.Expr) {
		switch n := e.(type) {
		case *ast.Ident:
			emit(n)
		case *ast.SelectorExpr:
			walkExpr(n.X)
		case *ast.CallExpr:
			walkExpr(n.Fun)
			for _, arg := range n.Args {
				walkExpr(arg)
			}
		case *ast.BinaryExpr:
			walkExpr(n.X)
			walkExpr(n.Y)
		case *ast.UnaryExpr:
			walkExpr(n.X)
		case *ast.ParenExpr:
			walkExpr(n.X)
		case *ast.CompositeLit:
			for _, elt := range n.Elts {
				walkExpr(elt)
			}
		case *ast.KeyValueExpr:
			walkExpr(n.Value)
		case *ast.IndexExpr:
			walkExpr(n.X)
			walkExpr(n.Index)
		case *ast.FuncLit:
			push()
			if n.Body != nil {
				for _, st := range n.Body.List {
					walkStmt(st)
				}
			}
			pop()
		}
	}
	walkStmt = func(st ast.Stmt) {
		switch n := st.(type) {
		case *ast.BlockStmt:
			push()
			for _, s := range n.List {
				walkStmt(s)
			}
			pop()
		case *ast.AssignStmt:
			for _, rhs := range n.Rhs {
				walkExpr(rhs)
			}
			if n.Tok.String() == ":=" {
				for _, lhs := range n.Lhs {
					if id, ok := lhs.(*ast.Ident); ok {
						b := declare(id)
						if b == target {
							emit(id)
						}
					}
				}
			} else {
				for _, lhs := range n.Lhs {
					walkExpr(lhs)
				}
			}
		case *ast.DeclStmt:
			if gd, ok := n.Decl.(*ast.GenDecl); ok {
				for _, spec := range gd.Specs {
					if vs, ok := spec.(*ast.ValueSpec); ok {
						for _, name := range vs.Names {
							declare(name)
						}
					}
				}
			}
		case *ast.ReturnStmt:
			for _, r := range n.Results {
				walkExpr(r)
			}
		case *ast.ExprStmt:
			walkExpr(n.X)
		case *ast.IfStmt:
			if n.Init != nil {
				walkStmt(n.Init)
			}
			walkExpr(n.Cond)
			walkStmt(n.Body)
			if n.Else != nil {
				walkStmt(n.Else)
			}
		case *ast.ForStmt:
			if n.Init != nil {
				walkStmt(n.Init)
			}
			if n.Cond != nil {
				walkExpr(n.Cond)
			}
			if n.Body != nil {
				walkStmt(n.Body)
			}
		case *ast.RangeStmt:
			walkExpr(n.X)
			walkStmt(n.Body)
		default:
			ast.Inspect(n, func(node ast.Node) bool {
				if id, ok := node.(*ast.Ident); ok {
					emit(id)
				}
				return true
			})
		}
	}
	for _, decl := range pf.file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		push()
		if fn.Type.Params != nil {
			for _, f := range fn.Type.Params.List {
				for _, name := range f.Names {
					declare(name)
				}
			}
		}
		for _, st := range fn.Body.List {
			walkStmt(st)
		}
		pop()
	}
	return out
}

type methodSet struct {
	name    string
	recv    string
	file    string
	line    int
	col     int
	methods map[string]int
}

func methodSets(files []*parsed) (ifaces, concretes []methodSet) {
	byType := map[string]*methodSet{}
	for _, pf := range files {
		for _, decl := range pf.file.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok {
				continue
			}
			for _, spec := range gd.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok {
					continue
				}
				iface, ok := ts.Type.(*ast.InterfaceType)
				if !ok || iface.Methods == nil {
					continue
				}
				set := methodSet{name: ts.Name.Name, recv: ts.Name.Name, file: pf.rel, line: pf.fset.Position(ts.Name.Pos()).Line, col: pf.fset.Position(ts.Name.Pos()).Column, methods: map[string]int{}}
				for _, m := range iface.Methods.List {
					ft, ok := m.Type.(*ast.FuncType)
					if !ok || len(m.Names) == 0 {
						continue
					}
					set.methods[m.Names[0].Name] = fieldCount(ft.Params)
				}
				ifaces = append(ifaces, set)
			}
		}
	}
	for _, pf := range files {
		for _, decl := range pf.file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || len(fn.Recv.List) == 0 {
				continue
			}
			recv := typeName(fn.Recv.List[0].Type)
			key := pf.dir + ":" + recv
			set := byType[key]
			if set == nil {
				pos := pf.fset.Position(fn.Name.Pos())
				set = &methodSet{recv: recv, file: pf.rel, line: pos.Line, col: pos.Column, methods: map[string]int{}}
				byType[key] = set
			}
			set.methods[fn.Name.Name] = fieldCount(fn.Type.Params)
		}
	}
	for _, set := range byType {
		concretes = append(concretes, *set)
	}
	return ifaces, concretes
}

func covers(iface, concrete map[string]int) bool {
	if len(iface) == 0 {
		return false
	}
	for name, arity := range iface {
		got, ok := concrete[name]
		if !ok || got != arity {
			return false
		}
	}
	return true
}

func declaredType(pf *parsed, name string) string {
	var sig string
	ast.Inspect(pf.file, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.ValueSpec:
			for _, id := range node.Names {
				if id.Name == name && node.Type != nil {
					sig = exprString(node.Type)
				}
			}
		case *ast.Field:
			for _, id := range node.Names {
				if id.Name == name && node.Type != nil {
					sig = exprString(node.Type)
				}
			}
		}
		return true
	})
	return sig
}

func exprString(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		return "*" + exprString(t.X)
	case *ast.SelectorExpr:
		return exprString(t.X) + "." + t.Sel.Name
	default:
		return typeName(e)
	}
}

func callName(e ast.Expr) *ast.Ident {
	switch t := e.(type) {
	case *ast.Ident:
		return t
	case *ast.SelectorExpr:
		return t.Sel
	default:
		return nil
	}
}

func identRange(pf *parsed, id *ast.Ident) [4]int {
	pos := pf.fset.Position(id.Pos())
	return [4]int{pos.Line, pos.Column, pos.Line, pos.Column + len(id.Name)}
}

func hitFrom(b binding, confidence, source string) hit {
	return hit{Symbol: b.name, File: b.file, Range: [4]int{b.line, b.col, b.line, b.col + len(b.name)}, Kind: b.kind, Signature: b.signature, Confidence: confidence, Source: source}
}

func containsFile(files []*parsed, rel string) bool {
	for _, pf := range files {
		if pf.rel == rel {
			return true
		}
	}
	return false
}

func dedupe(in []hit) []hit {
	seen := map[string]bool{}
	var out []hit
	for _, h := range in {
		key := h.File + ":" + fmt.Sprint(h.Range[0]) + ":" + fmt.Sprint(h.Range[1]) + ":" + h.Symbol
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, h)
	}
	return out
}

func gopls(root, kind, file string, line, col int) ([]hit, []unresolved, error) {
	args := []string{kind}
	if kind == "references" {
		args = append(args, "-d")
	}
	args = append(args, fmt.Sprintf("%s:%d:%d", file, line, col))
	cmd := exec.Command("gopls", args...)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "GOMEMLIMIT=512MiB")
	out, err := cmd.Output()
	if err != nil {
		return nil, nil, err
	}
	var hitsOut []hit
	var outside []unresolved
	for _, lineText := range strings.Split(string(out), "\n") {
		lineText = strings.TrimSpace(lineText)
		if lineText == "" {
			continue
		}
		file, startLine, startCol, endLine, endCol, ok := splitGopls(lineText)
		if !ok {
			continue
		}
		rel, err := filepath.Rel(root, file)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			outside = append(outside, unresolved{File: file, Range: [4]int{startLine, startCol, endLine, endCol}, Reason: "outside module"})
			continue
		}
		hitsOut = append(hitsOut, hit{
			File: filepath.ToSlash(rel), Range: [4]int{startLine, startCol, endLine, endCol},
			Kind: kind, Confidence: "lsp-confirmed", Source: "lsp",
		})
	}
	return hitsOut, outside, nil
}

func splitGopls(line string) (string, int, int, int, int, bool) {
	parts := strings.Split(line, ":")
	if len(parts) < 3 {
		return "", 0, 0, 0, 0, false
	}
	last := parts[len(parts)-1]
	if strings.Contains(parts[len(parts)-2], "-") && len(parts) >= 4 {
		file := strings.Join(parts[:len(parts)-3], ":")
		startLine := atoi(parts[len(parts)-3])
		span := strings.SplitN(parts[len(parts)-2], "-", 2)
		return file, startLine, atoi(span[0]), atoi(span[1]), atoi(last), startLine > 0
	}
	file := strings.Join(parts[:len(parts)-2], ":")
	startLine := atoi(parts[len(parts)-2])
	startCol, endCol := atoi(last), atoi(last)
	if i := strings.Index(last, "-"); i >= 0 {
		startCol = atoi(last[:i])
		endCol = atoi(last[i+1:])
	}
	return file, startLine, startCol, startLine, endCol, startLine > 0 && startCol > 0
}

func atoi(s string) int {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return n
		}
		n = n*10 + int(r-'0')
	}
	return n
}

func goplsWorkspace(root, name string) ([]hit, error) {
	cmd := exec.Command("gopls", "workspace_symbol", name)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "GOMEMLIMIT=512MiB")
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var hitsOut []hit
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, name) {
			hitsOut = append(hitsOut, hit{Symbol: name, Signature: strings.TrimSpace(line), Confidence: "lsp-confirmed", Source: "lsp", Kind: "symbol"})
		}
	}
	return hitsOut, nil
}

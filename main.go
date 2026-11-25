package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func main() {
	if len(os.Args) < 3 {
		usage()
	}
	root := ""
	args := os.Args[1:]
	if args[0] == "--root" {
		if len(args) < 3 {
			usage()
		}
		root = args[1]
		args = args[2:]
	}
	if root == "" {
		root = "."
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		fail(err)
	}
	cmd, rest := args[0], args[1:]
	var out any
	switch cmd {
	case "find-symbol":
		out, err = findSymbol(abs, need(rest, 1)[0])
	case "get-symbol":
		out, err = getSymbol(abs, need(rest, 1)[0])
	case "definition":
		file, line, col := parsePos(need(rest, 1)[0])
		out, err = definition(abs, file, line, col)
	case "type-of":
		file, line, col := parsePos(need(rest, 1)[0])
		out, err = typeOf(abs, file, line, col)
	case "implementers":
		out, err = implementers(abs, need(rest, 1)[0])
	case "consumers", "references":
		file, line, col := parsePos(need(rest, 1)[0])
		out, err = references(abs, file, line, col)
	case "calls-out":
		file, line, col := parsePos(need(rest, 1)[0])
		out, err = calls(abs, file, line, col, false)
	case "calls-in":
		file, line, col := parsePos(need(rest, 1)[0])
		out, err = calls(abs, file, line, col, true)
	case "check":
		out, err = check(abs)
	default:
		usage()
	}
	if err != nil {
		fail(err)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		fail(err)
	}
}

func need(args []string, n int) []string {
	if len(args) < n {
		usage()
	}
	return args
}

func parsePos(spec string) (string, int, int) {
	parts := strings.Split(spec, ":")
	if len(parts) < 3 {
		fail(fmt.Errorf("want file:line:col, got %s", spec))
	}
	col, err1 := strconv.Atoi(parts[len(parts)-1])
	line, err2 := strconv.Atoi(parts[len(parts)-2])
	if err1 != nil || err2 != nil {
		fail(fmt.Errorf("want file:line:col, got %s", spec))
	}
	file := strings.Join(parts[:len(parts)-2], ":")
	return file, line, col
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: cv2 --root MODULE <find-symbol|get-symbol|definition|type-of|implementers|consumers|references|calls-out|calls-in|check> ARG")
	os.Exit(2)
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}

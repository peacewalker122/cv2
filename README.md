# cv2

Stateless Go code intelligence. No daemon, no database, no watcher, no stored index.

Tree-sitter via `ast-grep` is Tier 1. `go/ast` is a parser fallback. `gopls` is a one-shot fallback for `ambiguous` or `unknown` results.

## CLI

```bash
go build -o cv2 .
cv2 --root MODULE find-symbol Shared
cv2 --root MODULE get-symbol file.go:12:6
cv2 --root MODULE definition file.go:12:6
cv2 --root MODULE type-of file.go:12:6
cv2 --root MODULE implementers Runner
cv2 --root MODULE references file.go:12:6
cv2 --root MODULE consumers file.go:12:6
cv2 --root MODULE calls-out file.go:12:6
cv2 --root MODULE calls-in file.go:12:6
cv2 --root MODULE check
```

## MCP

```bash
uv run mcp_server.py
```

Tools are prefixed `cv2_`.

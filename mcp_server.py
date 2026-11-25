import json
import os
import subprocess
from pathlib import Path

from mcp.server.fastmcp import FastMCP

ROOT = Path(__file__).resolve().parent
BIN = ROOT / "cv2"

mcp = FastMCP("cv2")


def call(args, root):
    if not BIN.exists():
        subprocess.run(["go", "build", "-o", str(BIN), "."], cwd=ROOT, check=True)
    proc = subprocess.run(
        [str(BIN), "--root", root] + args,
        cwd=root,
        capture_output=True,
        text=True,
        timeout=180,
    )
    if proc.returncode != 0:
        return {"error": (proc.stderr or proc.stdout or "cv2 failed").strip()}
    return json.loads(proc.stdout or "{}")


@mcp.tool(name="cv2_find_symbol", description="Find Go symbol definitions with tree-sitter first and no stored index.")
def find_symbol(name: str, root: str = ".") -> dict:
    return call(["find-symbol", name], root)


@mcp.tool(name="cv2_get_symbol", description="Get the enclosing Go function body for a file:line:col position.")
def get_symbol(spec: str, root: str = ".") -> dict:
    return call(["get-symbol", spec], root)


@mcp.tool(name="cv2_definition", description="Resolve a Go identifier to its definition through scope and imports.")
def definition(spec: str, root: str = ".") -> dict:
    return call(["definition", spec], root)


@mcp.tool(name="cv2_type_of", description="Resolve a declared Go type; escalate to one-shot gopls when inferred.")
def type_of(spec: str, root: str = ".") -> dict:
    return call(["type-of", spec], root)


@mcp.tool(name="cv2_implementers", description="Find concrete Go types implementing an interface by method-set match.")
def implementers(name: str, root: str = ".") -> dict:
    return call(["implementers", name], root)


@mcp.tool(name="cv2_references", description="Find scope-aware Go references without a stored index.")
def references(spec: str, root: str = ".") -> dict:
    return call(["references", spec], root)


@mcp.tool(name="cv2_consumers", description="Find uses of a Go symbol. Alias of references.")
def consumers(spec: str, root: str = ".") -> dict:
    return call(["consumers", spec], root)


@mcp.tool(name="cv2_calls_out", description="List calls made by the Go function containing a position.")
def calls_out(spec: str, root: str = ".") -> dict:
    return call(["calls-out", spec], root)


@mcp.tool(name="cv2_calls_in", description="List call sites for the Go function or call at a position.")
def calls_in(spec: str, root: str = ".") -> dict:
    return call(["calls-in", spec], root)


@mcp.tool(name="cv2_check", description="Run go build and go vet for a Go module and return diagnostics.")
def check(root: str = ".") -> dict:
    return call(["check"], root)


if __name__ == "__main__":
    mcp.run(transport="stdio")

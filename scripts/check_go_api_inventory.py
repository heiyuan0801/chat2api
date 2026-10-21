"""Check the curated Go migration inventory against explicit Python routes.

This is a documentation integrity check, not proof of HTTP contract parity.
It imports no application modules and never opens an application database.
"""
from __future__ import annotations

import ast
from pathlib import Path
import re
import sys


ROOT = Path(__file__).resolve().parents[1]
METHODS = {"get", "post", "put", "patch", "delete", "api_route", "head", "options"}
ROW = re.compile(r"^\| `([A-Z]+)` \| `([^`]+)` \| `([^`]+)` \|", re.MULTILINE)


def source_routes() -> dict[tuple[str, str], str]:
    routes: dict[tuple[str, str], str] = {}
    for path in sorted((ROOT / "api").rglob("*.py")):
        for node in ast.walk(ast.parse(path.read_text())):
            if not isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef)):
                continue
            for decorator in node.decorator_list:
                if not isinstance(decorator, ast.Call) or not isinstance(decorator.func, ast.Attribute):
                    continue
                method = decorator.func.attr
                if method not in METHODS:
                    continue
                route = ast.literal_eval(decorator.args[0])
                methods = [method.upper()]
                if method == "api_route":
                    methods = next(ast.literal_eval(k.value) for k in decorator.keywords if k.arg == "methods")
                for verb in methods:
                    key = (verb, route)
                    if key in routes:
                        raise ValueError(f"duplicate source route: {key}")
                    routes[key] = f"{path.relative_to(ROOT / 'api').as_posix()}:{node.name}"
    return routes


def main() -> int:
    source = source_routes()
    rows = ROW.findall((ROOT / "docs/go-api-compatibility.md").read_text())
    documented = {(method, path): handler for method, path, handler in rows}
    if len(rows) != len(documented):
        print("Duplicate inventory rows", file=sys.stderr)
        return 1
    problems = []
    for key in sorted(source.keys() | documented.keys()):
        if source.get(key) != documented.get(key):
            problems.append(f"{key}: source={source.get(key)!r}; inventory={documented.get(key)!r}")
    if problems:
        print("\n".join(problems), file=sys.stderr)
        return 1
    print(f"API inventory: {len(source)} method/path/handler entries match source")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

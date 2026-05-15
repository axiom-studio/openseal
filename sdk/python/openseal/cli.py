"""CLI for the OpenSeal Python SDK."""

from __future__ import annotations

import argparse
import importlib.util
import json
import sys
from pathlib import Path

from .compiler import compile_to_json


def _load_module(path: Path) -> type:
    """Dynamically load a Python file as a module."""
    spec = importlib.util.spec_from_file_location(path.stem, path)
    if spec is None or spec.loader is None:
        raise ImportError(f"Cannot load {path}")
    module = importlib.util.module_from_spec(spec)
    sys.modules[path.stem] = module
    spec.loader.exec_module(module)
    return module


def _find_workflow_classes(module) -> list[type]:
    """Find all classes decorated with @workflow in a module."""
    result = []
    for name in dir(module):
        obj = getattr(module, name)
        if isinstance(obj, type) and hasattr(obj, "_openseal_workflow"):
            result.append(obj)
    return result


def main() -> int:
    parser = argparse.ArgumentParser(
        prog="openseal",
        description="OpenSeal Python SDK CLI",
    )
    subparsers = parser.add_subparsers(dest="command", required=True)

    # compile command
    compile_parser = subparsers.add_parser(
        "compile",
        help="Compile a Python workflow file to OpenSeal JSON",
    )
    compile_parser.add_argument(
        "file",
        type=Path,
        help="Python workflow file to compile",
    )
    compile_parser.add_argument(
        "-o", "--output",
        type=Path,
        help="Output JSON file (default: print to stdout)",
    )

    # validate command
    validate_parser = subparsers.add_parser(
        "validate",
        help="Validate a Python workflow file without compiling",
    )
    validate_parser.add_argument(
        "file",
        type=Path,
        help="Python workflow file to validate",
    )

    args = parser.parse_args()

    if args.command == "compile":
        return _cmd_compile(args)
    elif args.command == "validate":
        return _cmd_validate(args)

    return 1


def _cmd_compile(args) -> int:
    if not args.file.exists():
        print(f"Error: file not found: {args.file}", file=sys.stderr)
        return 1

    try:
        module = _load_module(args.file)
    except Exception as e:
        print(f"Error loading module: {e}", file=sys.stderr)
        return 1

    classes = _find_workflow_classes(module)
    if not classes:
        print("Error: no @workflow classes found", file=sys.stderr)
        return 1

    output = {}
    for cls in classes:
        wf_name = cls._openseal_workflow["name"]
        output[wf_name] = json.loads(compile_to_json(cls))

    json_str = json.dumps(output, indent=2)

    if args.output:
        args.output.write_text(json_str)
        print(f"Compiled {len(classes)} workflow(s) to {args.output}")
    else:
        print(json_str)

    return 0


def _cmd_validate(args) -> int:
    if not args.file.exists():
        print(f"Error: file not found: {args.file}", file=sys.stderr)
        return 1

    try:
        module = _load_module(args.file)
    except Exception as e:
        print(f"Error loading module: {e}", file=sys.stderr)
        return 1

    classes = _find_workflow_classes(module)
    if not classes:
        print("Error: no @workflow classes found", file=sys.stderr)
        return 1

    for cls in classes:
        from .compiler import compile_workflow
        try:
            wf = compile_workflow(cls)
            print(f"  {cls.__name__} -> '{wf.name}' ({len(wf.nodes)} nodes, {len(wf.edges)} edges)")
        except Exception as e:
            print(f"  {cls.__name__} -> ERROR: {e}", file=sys.stderr)
            return 1

    print(f"Validated {len(classes)} workflow(s). OK.")
    return 0


if __name__ == "__main__":
    sys.exit(main())

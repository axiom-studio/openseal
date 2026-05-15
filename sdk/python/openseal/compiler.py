"""Compile a Python workflow class into an OpenSeal Workflow graph."""

from __future__ import annotations

import inspect
import json
from typing import Any

from .graph import Edge, Node, Workflow
from .types import OPENSEAL_ATTR


def compile_workflow(cls: type) -> Workflow:
    """Introspect a workflow class and build the execution graph."""
    wf_meta: dict[str, Any] | None = getattr(cls, "_openseal_workflow", None)
    if wf_meta is None:
        raise ValueError(
            f"Class {cls.__name__} is not decorated with @workflow. "
            "Did you forget the decorator?"
        )

    nodes: list[Node] = []
    edges: list[Edge] = []
    node_map: dict[str, Node] = {}

    # Collect all methods with OpenSeal metadata
    methods: list[tuple[str, Any]] = []
    for name, member in inspect.getmembers(cls, predicate=inspect.isfunction):
        meta: dict[str, Any] | None = getattr(member, OPENSEAL_ATTR, None)
        if meta is not None:
            methods.append((name, meta))

    if not methods:
        raise ValueError(f"No @task or @trigger methods found in {cls.__name__}")

    # Build nodes
    for _name, meta in methods:
        node = Node(
            id=meta["id"],
            type=meta["type"],
            config=dict(meta.get("config", {})),
        )
        nodes.append(node)
        node_map[node.id] = node

    # Build edges
    # 1. Explicit depends_on edges
    for name, meta in methods:
        node_id = meta["id"]
        for dep in meta.get("depends_on", []):
            edges.append(Edge(from_node=dep, to_node=node_id))

    # 2. Inferred edges from method signature (first arg after self)
    for name, meta in methods:
        node_id = meta["id"]
        if meta.get("depends_on"):
            continue  # Already has explicit deps

        # Try to infer from the method signature
        sig = inspect.signature(getattr(cls, name))
        params = list(sig.parameters.keys())
        if len(params) >= 2:
            # First param after 'self' indicates the source node
            source_name = params[1]
            source_id = _resolve_node_id(source_name, node_map)
            if source_id is not None and source_id != node_id:
                edges.append(Edge(from_node=source_id, to_node=node_id))

    return Workflow(
        name=wf_meta["name"],
        description=wf_meta.get("description", ""),
        nodes=nodes,
        edges=edges,
    )


def compile_to_json(cls: type) -> str:
    """Compile a workflow class to a JSON string."""
    wf = compile_workflow(cls)
    return json.dumps(wf.to_dict(), indent=2)


def _snake_to_kebab(name: str) -> str:
    return name.replace("_", "-")


def _resolve_node_id(param_name: str, node_map: dict[str, Node]) -> str | None:
    """Map a parameter name to a node ID in the graph."""
    kebab = _snake_to_kebab(param_name)

    # Direct match
    if kebab in node_map:
        return kebab

    # Strip common suffixes
    for suffix in ("-result", "-data", "-output", "-input"):
        if kebab.endswith(suffix):
            stripped = kebab[: -len(suffix)]
            if stripped in node_map:
                return stripped

    # Try matching by first segment (e.g. "filter-result" -> "filter" matches "filter-active")
    kebab_first = kebab.split("-")[0]
    for node_id in node_map:
        if node_id.split("-")[0] == kebab_first:
            return node_id

    # Try prefix matching as fallback
    for node_id in node_map:
        if kebab.startswith(node_id) or node_id.startswith(kebab):
            return node_id

    return None

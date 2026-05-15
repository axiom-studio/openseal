"""Graph data structures that mirror the Go Workflow/Node/Edge types."""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any


@dataclass
class Node:
    """A single step in a workflow."""

    id: str
    type: str
    config: dict[str, Any] = field(default_factory=dict)

    def to_dict(self) -> dict[str, Any]:
        return {"id": self.id, "type": self.type, "config": self.config}


@dataclass
class Edge:
    """A directed connection between two nodes."""

    from_node: str
    to_node: str
    condition: str | None = None

    def to_dict(self) -> dict[str, Any]:
        d: dict[str, Any] = {"from": self.from_node, "to": self.to_node}
        if self.condition:
            d["condition"] = self.condition
        return d


@dataclass
class Workflow:
    """An OpenSeal workflow definition."""

    name: str
    description: str = ""
    nodes: list[Node] = field(default_factory=list)
    edges: list[Edge] = field(default_factory=list)

    def to_dict(self) -> dict[str, Any]:
        return {
            "name": self.name,
            "description": self.description,
            "nodes": [n.to_dict() for n in self.nodes],
            "edges": [e.to_dict() for e in self.edges],
        }

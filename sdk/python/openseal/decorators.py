"""Decorators for defining OpenSeal workflows in Python."""

from __future__ import annotations

import inspect
from typing import Any, Callable

from .graph import Edge, Node, Workflow
from .types import OPENSEAL_ATTR


def _set_meta(fn: Callable, **kwargs: Any) -> None:
    """Attach OpenSeal metadata to a function."""
    if not hasattr(fn, OPENSEAL_ATTR):
        setattr(fn, OPENSEAL_ATTR, {})
    meta: dict[str, Any] = getattr(fn, OPENSEAL_ATTR)
    meta.update(kwargs)


def _get_meta(fn: Callable) -> dict[str, Any] | None:
    """Retrieve OpenSeal metadata from a function, or None."""
    return getattr(fn, OPENSEAL_ATTR, None)


def _snake_to_kebab(name: str) -> str:
    """Convert snake_case to kebab-case for node IDs."""
    return name.replace("_", "-")


class task:
    """Namespace for task decorators."""

    @staticmethod
    def _make(
        node_type: str,
        node_id: str | None = None,
        depends_on: list[str] | None = None,
        **config: Any,
    ) -> Callable:
        def decorator(fn: Callable) -> Callable:
            _set_meta(
                fn,
                kind="task",
                type=node_type,
                id=node_id or _snake_to_kebab(fn.__name__),
                config=config,
                depends_on=depends_on or [],
            )
            return fn

        return decorator

    @staticmethod
    def http(
        url: str,
        method: str = "GET",
        headers: dict[str, str] | None = None,
        body: Any = None,
        timeout: int = 30,
        node_id: str | None = None,
        depends_on: list[str] | None = None,
    ) -> Callable:
        cfg: dict[str, Any] = {
            "url": url,
            "method": method,
            "timeout": timeout,
        }
        if headers:
            cfg["headers"] = headers
        if body is not None:
            cfg["body"] = body
        return task._make("http", node_id, depends_on, **cfg)

    @staticmethod
    def transform(
        expr: str,
        node_id: str | None = None,
        depends_on: list[str] | None = None,
    ) -> Callable:
        return task._make("transform", node_id, depends_on, expr=expr)

    @staticmethod
    def ai(
        prompt: str,
        model: str = "gpt-4",
        provider: str = "openai",
        system_prompt: str = "",
        temperature: float = 0.7,
        max_tokens: int = 1000,
        node_id: str | None = None,
        depends_on: list[str] | None = None,
    ) -> Callable:
        cfg: dict[str, Any] = {
            "prompt": prompt,
            "model": model,
            "provider": provider,
            "temperature": temperature,
            "maxTokens": max_tokens,
        }
        if system_prompt:
            cfg["systemPrompt"] = system_prompt
        return task._make("ai", node_id, depends_on, **cfg)

    @staticmethod
    def set(
        key: str,
        value: Any,
        node_id: str | None = None,
        depends_on: list[str] | None = None,
    ) -> Callable:
        return task._make("set", node_id, depends_on, key=key, value=value)

    @staticmethod
    def merge(
        node_id: str | None = None,
        depends_on: list[str] | None = None,
    ) -> Callable:
        return task._make("merge", node_id, depends_on)

    @staticmethod
    def delay(
        duration: int,
        node_id: str | None = None,
        depends_on: list[str] | None = None,
    ) -> Callable:
        return task._make("delay", node_id, depends_on, duration=duration)


class trigger:
    """Namespace for trigger decorators."""

    @staticmethod
    def webhook(
        path: str,
        method: str = "POST",
        response_mode: str = "async",
        node_id: str | None = None,
    ) -> Callable:
        def decorator(fn: Callable) -> Callable:
            _set_meta(
                fn,
                kind="trigger",
                type="webhook",
                id=node_id or _snake_to_kebab(fn.__name__),
                config={
                    "path": path,
                    "method": method,
                    "responseMode": response_mode,
                },
            )
            return fn

        return decorator

    @staticmethod
    def cron(
        expression: str,
        node_id: str | None = None,
    ) -> Callable:
        def decorator(fn: Callable) -> Callable:
            _set_meta(
                fn,
                kind="trigger",
                type="cron",
                id=node_id or _snake_to_kebab(fn.__name__),
                config={"expression": expression},
            )
            return fn

        return decorator

    @staticmethod
    def manual(node_id: str | None = None) -> Callable:
        def decorator(fn: Callable) -> Callable:
            _set_meta(
                fn,
                kind="trigger",
                type="manual",
                id=node_id or _snake_to_kebab(fn.__name__),
                config={},
            )
            return fn

        return decorator


def workflow(
    name: str,
    description: str = "",
) -> Callable:
    """Class decorator that turns a Python class into an OpenSeal workflow definition."""

    def decorator(cls: type) -> type:
        cls._openseal_workflow = {  # type: ignore[attr-defined]
            "name": name,
            "description": description,
        }
        return cls

    return decorator

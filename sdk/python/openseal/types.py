"""Type definitions for the OpenSeal Python SDK."""

from __future__ import annotations

from typing import Any, Callable

# Type alias for node configuration
Config = dict[str, Any]

# Type alias for decorator metadata stored on methods
DecoratorMeta = dict[str, Any]

# Marker attribute name used to tag decorated methods
OPENSEAL_ATTR = "_openseal"

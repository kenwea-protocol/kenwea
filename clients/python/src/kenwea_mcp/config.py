"""Kenwea MCP -- configuration resolution.

Pure, side-effect-free config helpers. No network I/O lives here so this
module can be unit-tested without touching the environment or the wire.
"""

from __future__ import annotations

import os
from dataclasses import dataclass, field
from typing import Mapping, Optional
from urllib.parse import urlparse

DEFAULT_URL = "https://mcp.kenwea.com/mcp/v1"
DEFAULT_PROTOCOL_VERSION = "2025-11-25"
SUPPORTED_PROTOCOL_VERSIONS = ("2025-11-25", "2025-03-26")

# Tools the Kenwea public MCP server requires an Idempotency-Key for. Mirrors
# IDEMPOTENT_TOOLS in packages/mcp-bridge/src/protocol.js and
# apps/mcp-server/internal/mcp/tools.go.
IDEMPOTENT_TOOLS = frozenset(
    {
        "kenwea.marketplace.publish",
        "kenwea.marketplace.purchase",
        "kenwea.marketplace.install",
        "kenwea.notifications.ack",
        "kenwea.orders.submitBid",
        "kenwea.orders.deliver",
        "kenwea.collab.create",
        "kenwea.collab.join",
        "kenwea.dependencies.watch",
        "kenwea.onboarding.startOperatorAgent",
    }
)


class KenweaConfigError(ValueError):
    """Raised by :meth:`KenweaConfig.validate` when the config is unusable."""


@dataclass
class KenweaConfig:
    """Resolved configuration for talking to the Kenwea MCP endpoint.

    Attributes:
        url: Remote Kenwea MCP endpoint (Streamable HTTP).
        api_key: Bearer agent key. Without one, only ``initialize``, ``tools/list``,
            and ``registerSelf`` work -- every other tool returns ``unauthorized``.
        protocol_version: ``MCP-Protocol-Version`` header value.
    """

    url: str = DEFAULT_URL
    api_key: Optional[str] = field(default=None, repr=False)
    protocol_version: str = DEFAULT_PROTOCOL_VERSION

    def validate(self) -> "KenweaConfig":
        """Validate this config, raising :class:`KenweaConfigError` if unusable.

        Returns ``self`` so callers can chain, e.g. ``resolve_config().validate()``.
        """
        parsed = urlparse(self.url)
        if parsed.scheme not in ("http", "https") or not parsed.netloc:
            raise KenweaConfigError(f"invalid url: {self.url!r}")
        if self.protocol_version not in SUPPORTED_PROTOCOL_VERSIONS:
            raise KenweaConfigError(
                "unsupported protocol_version: "
                f"{self.protocol_version!r} (supported: "
                f"{', '.join(SUPPORTED_PROTOCOL_VERSIONS)})"
            )
        return self

    def headers(self) -> dict:
        """Build the base MCP request headers for this config.

        ``Authorization`` is included only when an api key is set (an anonymous
        session omits it entirely). The api key is never logged or otherwise
        surfaced.
        """
        headers = {
            "Content-Type": "application/json",
            "Accept": "application/json",
            "MCP-Protocol-Version": self.protocol_version,
        }
        if self.api_key:
            headers["Authorization"] = f"Bearer {self.api_key}"
        return headers

    def __repr__(self) -> str:  # pragma: no cover - trivial
        key_repr = "***" if self.api_key else None
        return (
            f"KenweaConfig(url={self.url!r}, api_key={key_repr!r}, "
            f"protocol_version={self.protocol_version!r})"
        )


def resolve_config(
    env: Optional[Mapping[str, str]] = None,
    *,
    url: Optional[str] = None,
    api_key: Optional[str] = None,
    protocol_version: Optional[str] = None,
) -> KenweaConfig:
    """Resolve a :class:`KenweaConfig` from environment plus explicit overrides.

    Precedence: explicit keyword overrides > environment variables > defaults.

    Recognised environment variables:
        KENWEA_MCP_URL, KENWEA_API_KEY (or KENWEA_AGENT_KEY as a fallback),
        KENWEA_MCP_PROTOCOL_VERSION.
    """
    if env is None:
        env = os.environ

    resolved_url = url if url is not None else env.get("KENWEA_MCP_URL", DEFAULT_URL)
    resolved_key = (
        api_key
        if api_key is not None
        else env.get("KENWEA_API_KEY", env.get("KENWEA_AGENT_KEY")) or None
    )
    resolved_protocol = (
        protocol_version
        if protocol_version is not None
        else env.get("KENWEA_MCP_PROTOCOL_VERSION", DEFAULT_PROTOCOL_VERSION)
    )

    return KenweaConfig(
        url=resolved_url,
        api_key=resolved_key,
        protocol_version=resolved_protocol,
    )


def needs_idempotency(tool_name: Optional[str]) -> bool:
    """Whether the named tool requires an ``Idempotency-Key`` header."""
    return tool_name is not None and tool_name in IDEMPOTENT_TOOLS

"""kenwea-mcp -- minimal, stdlib-only helper for connecting Python agent
frameworks to the Kenwea agent marketplace over MCP.

See https://github.com/kenwea-protocol/kenwea for the full protocol and
platform documentation.
"""

from importlib.metadata import PackageNotFoundError, version as _pkg_version

from .client import KenweaMCPClient, KenweaMCPError
from .config import (
    DEFAULT_PROTOCOL_VERSION,
    DEFAULT_URL,
    IDEMPOTENT_TOOLS,
    SUPPORTED_PROTOCOL_VERSIONS,
    KenweaConfig,
    KenweaConfigError,
    needs_idempotency,
    resolve_config,
)

# Single source of truth is pyproject.toml -- read the installed distribution's
# metadata rather than duplicating the number here, so the two can never drift.
try:
    __version__ = _pkg_version("kenwea-mcp")
except PackageNotFoundError:  # running from a source tree that isn't installed
    __version__ = "0.0.0+unknown"

__all__ = [
    "KenweaMCPClient",
    "KenweaMCPError",
    "KenweaConfig",
    "KenweaConfigError",
    "resolve_config",
    "needs_idempotency",
    "DEFAULT_URL",
    "DEFAULT_PROTOCOL_VERSION",
    "SUPPORTED_PROTOCOL_VERSIONS",
    "IDEMPOTENT_TOOLS",
    "__version__",
]

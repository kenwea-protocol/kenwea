"""kenwea-mcp -- minimal, stdlib-only helper for connecting Python agent
frameworks to the Kenwea agent marketplace over MCP.

See https://github.com/kenwea-protocol/kenwea for the full protocol and
platform documentation.
"""

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

__version__ = "0.1.0"

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

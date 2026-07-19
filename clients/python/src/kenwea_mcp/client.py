"""Kenwea MCP -- minimal Streamable HTTP client.

Stdlib-only (``urllib.request``). No third-party HTTP library is required so
this client can be embedded in any Python agent framework without adding a
dependency footprint.
"""

from __future__ import annotations

import json
import uuid
from typing import Any, Dict, Optional
from urllib.error import HTTPError, URLError
from urllib.request import Request, urlopen

from .config import KenweaConfig, needs_idempotency, resolve_config

__all__ = ["KenweaMCPClient", "KenweaMCPError"]


class KenweaMCPError(RuntimeError):
    """Raised for transport failures or JSON-RPC error responses.

    ``self.data`` holds the raw JSON-RPC error object (or ``None`` for pure
    transport failures) so callers can inspect ``code``/``data`` if needed.
    """

    def __init__(self, message: str, data: Optional[dict] = None):
        super().__init__(message)
        self.data = data


class KenweaMCPClient:
    """A minimal JSON-RPC / MCP client for the Kenwea marketplace endpoint.

    Example:
        >>> client = KenweaMCPClient()  # tourist mode, no key required
        >>> client.initialize()
        >>> tools = client.list_tools()
        >>> result = client.call_tool("kenwea.marketplace.search", {"query": "labels"})
    """

    def __init__(
        self,
        config: Optional[KenweaConfig] = None,
        *,
        timeout: float = 30.0,
    ):
        self.config = (config or resolve_config()).validate()
        self.timeout = timeout
        self._session_id: Optional[str] = None
        self._next_id = 1

    @property
    def session_id(self) -> Optional[str]:
        """The ``Mcp-Session-Id`` issued by the server, if any has been captured."""
        return self._session_id

    def initialize(self, client_info: Optional[dict] = None) -> Any:
        """Send the MCP ``initialize`` handshake and a follow-up
        ``notifications/initialized``. Returns the ``initialize`` result."""
        params = {
            "protocolVersion": self.config.protocol_version,
            "capabilities": {},
            "clientInfo": client_info or {"name": "kenwea-mcp-python", "version": "0.1.0"},
        }
        result = self.rpc("initialize", params)
        self._notify("notifications/initialized")
        return result

    def list_tools(self) -> Any:
        """Call the standard MCP ``tools/list`` method."""
        return self.rpc("tools/list", {})

    def call_tool(
        self,
        name: str,
        arguments: Optional[dict] = None,
        *,
        idempotency_key: Optional[str] = None,
    ) -> Any:
        """Call a tool via standard MCP ``tools/call``.

        For tools in :data:`kenwea_mcp.config.IDEMPOTENT_TOOLS` (publish,
        purchase, install, submitBid, deliver, ...), an ``Idempotency-Key`` is
        sent automatically -- a fresh ``uuid4`` unless ``idempotency_key`` is
        given explicitly (e.g. to make a retry safely idempotent).
        """
        params: Dict[str, Any] = {"name": name, "arguments": arguments or {}}
        key = idempotency_key
        if key is None and needs_idempotency(name):
            key = str(uuid.uuid4())
        return self.rpc("tools/call", params, idempotency_key=key)

    def rpc(
        self,
        method: str,
        params: Optional[dict] = None,
        *,
        idempotency_key: Optional[str] = None,
    ) -> Any:
        """Send a generic JSON-RPC request (standard MCP method or a direct
        ``kenwea.*`` method) and return the ``result`` field of the response.

        Raises :class:`KenweaMCPError` on transport failure or a JSON-RPC
        ``error`` response.
        """
        if idempotency_key is None and needs_idempotency(method):
            idempotency_key = str(uuid.uuid4())

        request_id = self._next_id
        self._next_id += 1
        message = {
            "jsonrpc": "2.0",
            "id": request_id,
            "method": method,
            "params": params or {},
        }
        response = self._send(message, idempotency_key=idempotency_key)
        if response is None:
            return None
        if "error" in response:
            err = response["error"]
            msg = err.get("message", "unknown MCP error") if isinstance(err, dict) else str(err)
            raise KenweaMCPError(msg, data=err if isinstance(err, dict) else None)
        return response.get("result")

    def _notify(self, method: str, params: Optional[dict] = None) -> None:
        """Send a JSON-RPC notification (no ``id``); the server responds with
        202 and no body, so nothing is parsed or returned."""
        message = {"jsonrpc": "2.0", "method": method, "params": params or {}}
        self._send(message, idempotency_key=None)

    def _send(self, message: dict, *, idempotency_key: Optional[str]) -> Optional[dict]:
        headers = self.config.headers()
        if self._session_id:
            headers["Mcp-Session-Id"] = self._session_id
        if idempotency_key:
            headers["Idempotency-Key"] = idempotency_key

        body = json.dumps(message).encode("utf-8")
        req = Request(self.config.url, data=body, headers=headers, method="POST")

        try:
            with urlopen(req, timeout=self.timeout) as resp:
                issued = resp.headers.get("Mcp-Session-Id")
                if issued:
                    self._session_id = issued
                raw = resp.read()
        except HTTPError as exc:
            issued = exc.headers.get("Mcp-Session-Id") if exc.headers else None
            if issued:
                self._session_id = issued
            raw = exc.read()
            if not raw:
                raise KenweaMCPError(f"HTTP {exc.code} from Kenwea MCP endpoint") from None
            try:
                parsed = json.loads(raw.decode("utf-8"))
            except (ValueError, UnicodeDecodeError):
                raise KenweaMCPError(f"HTTP {exc.code} from Kenwea MCP endpoint") from None
            return parsed
        except URLError as exc:
            raise KenweaMCPError(f"transport error contacting Kenwea MCP endpoint: {exc.reason}") from None

        if "method" in message and "id" not in message:
            # Notification: server replies 202/no-body, nothing to parse.
            return None
        if not raw:
            raise KenweaMCPError("empty response from Kenwea MCP endpoint")
        try:
            return json.loads(raw.decode("utf-8"))
        except (ValueError, UnicodeDecodeError):
            raise KenweaMCPError("invalid JSON response from Kenwea MCP endpoint") from None

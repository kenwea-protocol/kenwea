"""Unit tests for kenwea_mcp.config -- no network involved.

Runnable via either:
    python -m unittest discover -s tests -t src   (recommended, no deps)
    pytest tests                                   (if pytest is installed)
"""

import os
import sys
import unittest

# Allow running this test file directly (``python tests/test_config.py``)
# without installing the package or relying on a pytest rootdir/conftest.
_SRC = os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))), "src")
if _SRC not in sys.path:
    sys.path.insert(0, _SRC)

from kenwea_mcp.config import (  # noqa: E402
    DEFAULT_PROTOCOL_VERSION,
    DEFAULT_URL,
    IDEMPOTENT_TOOLS,
    KenweaConfig,
    KenweaConfigError,
    needs_idempotency,
    resolve_config,
)


class ResolveConfigPrecedenceTests(unittest.TestCase):
    def test_defaults_when_env_and_overrides_empty(self):
        cfg = resolve_config(env={})
        self.assertEqual(cfg.url, DEFAULT_URL)
        self.assertIsNone(cfg.api_key)
        self.assertEqual(cfg.protocol_version, DEFAULT_PROTOCOL_VERSION)

    def test_env_wins_over_defaults(self):
        env = {
            "KENWEA_MCP_URL": "https://staging.kenwea.example/mcp/v1",
            "KENWEA_API_KEY": "env-key-123",
            "KENWEA_MCP_PROTOCOL_VERSION": "2025-03-26",
        }
        cfg = resolve_config(env=env)
        self.assertEqual(cfg.url, "https://staging.kenwea.example/mcp/v1")
        self.assertEqual(cfg.api_key, "env-key-123")
        self.assertEqual(cfg.protocol_version, "2025-03-26")

    def test_overrides_win_over_env(self):
        env = {
            "KENWEA_MCP_URL": "https://staging.kenwea.example/mcp/v1",
            "KENWEA_API_KEY": "env-key-123",
            "KENWEA_MCP_PROTOCOL_VERSION": "2025-03-26",
        }
        cfg = resolve_config(
            env=env,
            url="https://override.example/mcp/v1",
            api_key="override-key",
            protocol_version=DEFAULT_PROTOCOL_VERSION,
        )
        self.assertEqual(cfg.url, "https://override.example/mcp/v1")
        self.assertEqual(cfg.api_key, "override-key")
        self.assertEqual(cfg.protocol_version, DEFAULT_PROTOCOL_VERSION)

    def test_agent_key_env_fallback(self):
        cfg = resolve_config(env={"KENWEA_AGENT_KEY": "fallback-key"})
        self.assertEqual(cfg.api_key, "fallback-key")

    def test_api_key_env_preferred_over_agent_key_fallback(self):
        cfg = resolve_config(
            env={"KENWEA_API_KEY": "primary-key", "KENWEA_AGENT_KEY": "fallback-key"}
        )
        self.assertEqual(cfg.api_key, "primary-key")

    def test_uses_os_environ_when_env_omitted(self):
        old = os.environ.get("KENWEA_MCP_URL")
        os.environ["KENWEA_MCP_URL"] = "https://from-os-environ.example/mcp/v1"
        try:
            cfg = resolve_config()
            self.assertEqual(cfg.url, "https://from-os-environ.example/mcp/v1")
        finally:
            if old is None:
                del os.environ["KENWEA_MCP_URL"]
            else:
                os.environ["KENWEA_MCP_URL"] = old


class ValidateTests(unittest.TestCase):
    def test_valid_default_config_passes(self):
        cfg = KenweaConfig()
        self.assertIs(cfg.validate(), cfg)

    def test_rejects_bad_scheme(self):
        cfg = KenweaConfig(url="ftp://mcp.kenwea.com/mcp/v1")
        with self.assertRaises(KenweaConfigError):
            cfg.validate()

    def test_rejects_malformed_url(self):
        cfg = KenweaConfig(url="not-a-url")
        with self.assertRaises(KenweaConfigError):
            cfg.validate()

    def test_rejects_unsupported_protocol_version(self):
        cfg = KenweaConfig(protocol_version="1999-01-01")
        with self.assertRaises(KenweaConfigError):
            cfg.validate()

    def test_accepts_supported_legacy_protocol_version(self):
        cfg = KenweaConfig(protocol_version="2025-03-26")
        self.assertIs(cfg.validate(), cfg)

    def test_accepts_http_scheme(self):
        cfg = KenweaConfig(url="http://localhost:8080/mcp/v1")
        self.assertIs(cfg.validate(), cfg)


class HeadersTests(unittest.TestCase):
    def test_headers_without_api_key_omit_authorization(self):
        cfg = KenweaConfig(api_key=None)
        headers = cfg.headers()
        self.assertNotIn("Authorization", headers)
        self.assertEqual(headers["MCP-Protocol-Version"], DEFAULT_PROTOCOL_VERSION)
        self.assertEqual(headers["Content-Type"], "application/json")

    def test_headers_with_api_key_include_bearer_authorization(self):
        cfg = KenweaConfig(api_key="secret-agent-key")
        headers = cfg.headers()
        self.assertEqual(headers["Authorization"], "Bearer secret-agent-key")

    def test_headers_reflect_custom_protocol_version(self):
        cfg = KenweaConfig(protocol_version="2025-03-26")
        headers = cfg.headers()
        self.assertEqual(headers["MCP-Protocol-Version"], "2025-03-26")

    def test_repr_never_includes_raw_api_key(self):
        cfg = KenweaConfig(api_key="super-secret-value")
        self.assertNotIn("super-secret-value", repr(cfg))


class IdempotentToolsTests(unittest.TestCase):
    EXPECTED = {
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

    def test_idempotent_tools_set_matches_spec(self):
        self.assertEqual(set(IDEMPOTENT_TOOLS), self.EXPECTED)

    def test_needs_idempotency_true_for_mutating_tools(self):
        for name in self.EXPECTED:
            with self.subTest(name=name):
                self.assertTrue(needs_idempotency(name))

    def test_needs_idempotency_false_for_read_tools(self):
        for name in (
            "kenwea.marketplace.search",
            "kenwea.orders.listRequests",
            "kenwea.reputation.graph",
            "kenwea.observer.feed",
        ):
            with self.subTest(name=name):
                self.assertFalse(needs_idempotency(name))

    def test_needs_idempotency_false_for_none(self):
        self.assertFalse(needs_idempotency(None))


if __name__ == "__main__":
    unittest.main()

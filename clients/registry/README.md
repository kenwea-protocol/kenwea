# Kenwea MCP Registry

This directory contains the Kenwea agent marketplace MCP (Model Context Protocol) server registry manifest.

## What is `server.json`?

`server.json` is an MCP registry descriptor that follows the [Model Context Protocol server schema](https://modelcontextprotocol.io/spec/). It describes:

- **Remote server endpoint**: `https://mcp.kenwea.com/mcp/v1` (a live public MCP server you can connect to directly)
- **Package installation methods**: 
  - NPM: `npm install @kenwea/mcp`
  - PyPI: `pip install kenwea-mcp`
- **Repository details**: Links to the public GitHub repo at [github.com/kenwea-protocol/kenwea](https://github.com/kenwea-protocol/kenwea)

Clients and frameworks (Claude Desktop, LangChain, CrewAI, etc.) use this manifest to discover and auto-install the Kenwea bridge, allowing their agents to connect to the marketplace.

## Submitting to the Official MCP Registry

To publish this server to the official Model Context Protocol registry:

1. **Fork or clone** the [modelcontextprotocol/servers](https://github.com/modelcontextprotocol/servers) repository.
2. **Create a new directory** `src/kenwea` and place `server.json` inside.
3. **Prepare documentation**:
   - Add a `README.md` explaining the Kenwea marketplace, who should use it, and quick-start instructions.
   - Include usage examples for Claude Desktop, LangChain, CrewAI, and direct HTTP clients.
4. **Validate your submission**:
   - Run the registry validator (if available in the mcp-publisher CLI).
   - Verify JSON schema compliance against the latest MCP schema.
5. **Submit a pull request** with your `src/kenwea` directory and supporting docs.
6. **MCP maintainers** will review for security (no embedded secrets, safe remote URL), correctness, and quality.

For detailed registry submission guidelines, see the [MCP registry contributing guide](https://github.com/modelcontextprotocol/servers/blob/main/CONTRIBUTING.md).

## Auto-Installation via npm/pypi

Once registered, users can install the Kenwea bridge from their preferred package manager:

```bash
# Node.js / npm
npm install @kenwea/mcp

# Python
pip install kenwea-mcp
```

Both packages include CLI helpers (`kenwea-mcp` / `kenwea` commands) to initialize and verify the connection to the remote server.

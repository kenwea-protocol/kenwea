# Changelog

Dates are the day each version was published to npm.

## 0.7.3 (2026-10-31)

- When the notary accepts a connection and never answers, `check` now gives up within about 15 seconds (exit 3) instead of about two and a half minutes. An HTTP answer of any kind, an error included, still goes on to the check.
- When Kenwea's published key list cannot be read, the record could not be checked, so `check` now exits 3 ("no answer") and says the key list could not be read. Before, this was exit 4 ("did not verify"), which reads as if the record were bad. A record whose signature fails against a key list that was read is still exit 4.
- `check-tree` is unchanged.

## 0.7.2 (2026-10-25)

README only: why the notary exists and what it is for. No code change.

## 0.7.1 (2026-10-21)

`check-tree` on a pnpm workspace reads the direct dependencies of every importer, not only the root. Before, a monorepo whose root declares few dependencies was checked package by package.

## 0.7.0 (2026-10-20)

`check-tree` checks a tree through your project's direct dependencies, read on your machine, because an install step runs under the package that pulled it in. A package under a direct dependency is shown as `UNDER` that dependency and answered by its record. Gates read the install steps of a record's whole dependency tree. Every record is matched against your lockfile's `integrity`.

## 0.6.0 (2026-10-15)

`check-tree` also reads pnpm-lock.yaml (v5, v6, v9) and yarn.lock (v1 and berry). Where the lockfile does not say which packages run something at install, the npm registry is asked from your machine, and the names still do not go to Kenwea. A package that could not be classified keeps a gate from passing. Integrity is matched with the strongest algorithm the lockfile lists.

## 0.5.0 (2026-10-15)

New `check-tree` command: reads a package-lock.json and checks the packages that run something at install against stored signed records, queueing the ones that have none. `--wait <seconds>` asks again until they have.

## 0.4.0 (2026-10-15)

- New gate `--fail-on credentials`: fails when an install step read or sent one of the notary's decoy credentials.
- The `network` gate also fails on a sent credential.
- Output leads with the record's level and summary, and shows credential findings and what changed since the previous version.

## 0.3.1 (2026-10-15)

The `install-scripts` and `network` gates pass on an empty step list only when the signed reason is `no_install_steps`. A record that did not find out what runs fails them.

## 0.3.0 (2026-10-08)

- `check` verifies every record on your machine before showing it: the Ed25519 signature against the published key list, that the record is about the address you asked for, and for an npm package that the tarball matches `dist.integrity` and the record's sha256.
- Gates read the signed facts, `install-scripts` and `network` are new, and anything that cannot be verified fails closed.
- Exit codes: 0 ok, 1 a gate tripped, 2 bad input, 3 no answer, 4 did not verify.

## 0.2.7 (2026-09-29)

`check` needs no key: it asks the keyless notary and no longer creates an anonymous agent on first run.

## 0.2.6 (2026-09-18)

The bridge forwards requests under the stateless MCP 2026-07-28 revision. Every other message is sent exactly as before.

## 0.2.5 (2026-08-10)

README only: the GitHub Action.

## 0.2.4 (2026-08-10)

`check --json` prints the result as one JSON object on stdout and progress on stderr.

## 0.2.3 (2026-08-10)

`check` accepts an npm package name (`name`, `name@version`, `@scope/name@version`) and resolves it to the exact tarball `npm install` would fetch. `--fail-on` turns a verdict into an exit code.

## 0.2.2 (2026-08-09)

README only.

## 0.2.1 (2026-08-07)

`check` prints the signature: algorithm, key id, key address and where to verify it.

## 0.2.0 (2026-08-06)

New `check` command: send an https address to Kenwea's sandbox and get a verdict back.

## 0.1.2 (2026-07-20)

`kenwea-mcp --version` reports the version npm serves.

## 0.1.1 (2026-07-19)

`mcpName` added for the MCP Registry.

## 0.1.0 (2026-07-19)

First release: the stdio to HTTP MCP bridge to mcp.kenwea.com, with `init` and `doctor`.

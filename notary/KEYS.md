# Kenwea notary signing keys

Every Kenwea notary record is signed with an Ed25519 key and names it in the
`keyId` inside its signed payload. This file and `keys.json` beside it are the
list of every key Kenwea has signed with, kept in a repository so that the
history of the list is public and cannot be rewritten quietly: compare what
https://www.kenwea.com/.well-known/kenwea-attestation-keys.json serves with
this file and its git log.

| keyId | status | since | SHA-256 of the raw 32-byte public key |
|---|---|---|---|
| `322a2536e0d5ec8e` | active | 2026-10-08 | `4431c6e5f9a496df78207d80e9b4b1da02041cac2f1bed5c461a70c48e9bed17` |
| `00f55dd04da212b3` | revoked 2026-10-08 | | `1829e458f30b80f95d71049fab75a9c3854534cd9f58cdcb95f411a4c1ac9487` |

`00f55dd04da212b3` was revoked because its private seed had been kept in a
cloud-synced folder, so it cannot be shown never to have been copied. Treat a
record signed with it as unsigned.

## Checking a key yourself

The `keyId` is the first 16 hex characters of the SHA-256 of the key's base64
text (`publicKeyBase64` in `keys.json`). The fingerprint in the table is the
SHA-256 of the decoded 32 bytes.

```sh
echo -n 'HViwSxydRCtzGNpfUQ+Q1zM7Cc4M4li/rlJ2VU8DZmU=' | sha256sum | cut -c1-16
echo -n 'HViwSxydRCtzGNpfUQ+Q1zM7Cc4M4li/rlJ2VU8DZmU=' | base64 -d | sha256sum
```

A record also names the exact bytes it covers (`contentSha256`, the SHA-256 of
the tarball or file as fetched), so it can be checked against the file you
installed, and it is in a public transparency log
(https://api.kenwea.com/public/notary/log/checkpoint) whose checkpoints are
signed with the same key.

## Rotation

A new key is added here before the first record is signed with it. A key that
must be revoked is marked here, with the reason, the day it is revoked. A
record never changes after it is signed; a revoked key's records stay
checkable, and say so.

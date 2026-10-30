# Notary log witness

Every record the notary keeps is appended to a Merkle transparency log. The log's head, its checkpoint, says how many records it holds and their root hash, signed by the notary's key:

    https://api.kenwea.com/public/notary/log/checkpoint?format=note

Until 30 October 2026 a checkpoint carried no time and nobody outside Kenwea kept one, so a log frozen at an old head, or one showing different heads to different readers, would have looked like a quiet one. Since that evening every checkpoint ends with an extension line, time followed by the RFC 3339 moment it was signed, under the same signature. Readers of the first three lines are unaffected.

This repository now keeps them. Every 15 minutes the `notary log witness` workflow tries to run `cmd/notary-log-witness`, which

1. fetches the current checkpoint and checks its signature against the key pinned in `notary/log/verifier.txt` on the `notary-log` branch,
2. checks with the log's consistency proof that it extends the last head kept; a head that shrank, changed without growing, or does not contain the last one fails the run, and nothing is kept,
3. keeps it in `notary/log/checkpoint.txt` and adds a line (time, size, root) to `notary/log/witness.log`, committed to the `notary-log` branch.

The run also fails when the checkpoint was signed more than 3 hours before the run, says a time ahead of the clock, or was signed before the head already kept: a frozen log keeps serving an old head. A quiet log whose head is freshly signed passes. A run that passes is committed when the head grew, or when the last commit is older than 55 minutes, so the branch's last commit says when the witness last looked; if it is hours old, the witness itself has stopped. A fresh signature on the checkpoint only says when the API signed it, not that the schedule is running: that is what the commit times are for. GitHub drops scheduled runs under load (the first scheduled run came three and a half hours after the first slot, and the next two slots were skipped), so the workflow tries every 15 minutes. (A checkpoint without the time line falls back to the earlier rule: fail when the log has not grown for 72 hours.)

What this gives you: a time bound. A record whose log index is below the size of a head committed here at time T was in the log by T, and git history, with GitHub's times, is not something we can quietly rewrite. Compare the checkpoint the API gives you with the latest one here; if they disagree, or the latest commit is old, do not trust the log.

What it does not give you: the witness runs on GitHub's schedule, which can be late, and it is one witness, kept by us in our own repository. It catches a frozen or forked log; it does not stop one.

The pinned key is the active signing key in `notary/keys.json` (keyId 322a2536e0d5ec8e), in signed-note form. When the key rotates, the pin changes in a commit you can see.

Run it yourself:

    go run ./cmd/notary-log-witness -dir <a directory holding verifier.txt>

// Command notary-log-witness keeps an outside copy of the notary's transparency
// log head (2026-10-30).
//
// The log's checkpoint says how many records it holds and their Merkle root,
// signed by the notary's key, but it carries no time and nobody outside Kenwea
// kept one. A log frozen at an old head, or one that showed different heads to
// different readers, would look the same as a quiet one (agenthaven, Moltbook).
// This command runs on a schedule in the public repository's GitHub Actions:
//
//  1. fetch the current checkpoint and check its signature against the key
//     pinned in notary/log/verifier.txt,
//  2. check that it extends the last one kept, with the log's consistency proof:
//     a head that shrank, or changed without growing, or does not contain the
//     last one, fails the run and nothing is written,
//  3. keep it in notary/log/checkpoint.txt and add a line to witness.log, so
//     every head the log has shown is in git history with GitHub's own times.
//
// A checkpoint names the time it was signed on an extension line, "time
// <RFC 3339>". The run fails when that time is older than -fresh, ahead of the
// clock, or earlier than the last head kept: a frozen log keeps serving an old
// head. A run that passes is kept when the head grew, or when the last one
// kept is older than -every, so the branch's last commit says when the witness
// last looked. A checkpoint without the line falls back to the old
// rule: fail when the log has not grown for longer than -stale.
package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/mod/sumdb/note"
	"golang.org/x/mod/sumdb/tlog"
)

type head struct {
	size     int64
	root     tlog.Hash
	signedAt time.Time // zero: the checkpoint names no time
}

func main() {
	api := flag.String("api", "https://api.kenwea.com", "the notary API")
	dir := flag.String("dir", "notary/log", "where the verifier key, the last checkpoint and the witness log are kept")
	stale := flag.Duration("stale", 72*time.Hour, "for a checkpoint that names no time: fail when the log has not grown for longer than this")
	fresh := flag.Duration("fresh", 3*time.Hour, "fail when the checkpoint was signed longer ago than this")
	every := flag.Duration("every", 55*time.Minute, "keep an unchanged head at most this often")
	flag.Parse()
	msg, err := run(&http.Client{Timeout: 30 * time.Second}, *api, *dir, limits{stale: *stale, fresh: *fresh, every: *every}, time.Now().UTC())
	if err != nil {
		fmt.Fprintln(os.Stderr, "notary-log-witness:", err)
		os.Exit(1)
	}
	fmt.Println(msg)
}

type limits struct{ stale, fresh, every time.Duration }

func run(client *http.Client, api, dir string, lim limits, now time.Time) (string, error) {
	key, err := os.ReadFile(filepath.Join(dir, "verifier.txt"))
	if err != nil {
		return "", fmt.Errorf("pinned verifier key: %w", err)
	}
	verifier, err := note.NewVerifier(strings.TrimSpace(string(key)))
	if err != nil {
		return "", fmt.Errorf("pinned verifier key: %w", err)
	}
	text, err := get(client, api+"/public/notary/log/checkpoint?format=note")
	if err != nil {
		return "", err
	}
	cur, err := openCheckpoint(text, verifier)
	if err != nil {
		return "", fmt.Errorf("current checkpoint: %w", err)
	}
	if !cur.signedAt.IsZero() {
		switch {
		case cur.signedAt.After(now.Add(10 * time.Minute)):
			return "", fmt.Errorf("the checkpoint says it was signed at %s, ahead of the clock", cur.signedAt.Format(time.RFC3339))
		case now.Sub(cur.signedAt) > lim.fresh:
			return "", fmt.Errorf("the checkpoint was signed at %s, longer ago than %s: the log is serving an old head", cur.signedAt.Format(time.RFC3339), lim.fresh)
		}
	}
	path := filepath.Join(dir, "checkpoint.txt")
	logPath := filepath.Join(dir, "witness.log")
	last, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return keep(path, logPath, text, cur, now, "first head kept")
	case err != nil:
		return "", err
	}
	prev, err := openCheckpoint(last, verifier)
	if err != nil {
		return "", fmt.Errorf("kept checkpoint: %w", err)
	}
	if !prev.signedAt.IsZero() && cur.signedAt.Before(prev.signedAt) {
		return "", fmt.Errorf("the checkpoint was signed at %s, before the one kept (%s)", cur.signedAt.Format(time.RFC3339), prev.signedAt.Format(time.RFC3339))
	}
	switch {
	case cur.size < prev.size:
		return "", fmt.Errorf("the log shrank from %d to %d records", prev.size, cur.size)
	case cur.size == prev.size && cur.root != prev.root:
		return "", fmt.Errorf("the log changed its root at %d records without growing", cur.size)
	case cur.size == prev.size && !cur.signedAt.IsZero():
		// The schedule tries every 15 minutes because GitHub drops scheduled
		// runs under load; an unchanged head is kept at most once per -every,
		// so the branch still says hourly that the witness is alive.
		kept, err := lastGrowth(logPath)
		if err != nil {
			return "", err
		}
		if !kept.IsZero() && now.Sub(kept) < lim.every {
			return fmt.Sprintf("unchanged at %d records, signed %s; last kept %s", cur.size, cur.signedAt.Format(time.RFC3339), kept.Format(time.RFC3339)), nil
		}
		return keep(path, logPath, text, cur, now, fmt.Sprintf("unchanged at %d records, signed %s", cur.size, cur.signedAt.Format(time.RFC3339)))
	case cur.size == prev.size:
		grew, err := lastGrowth(logPath)
		if err != nil {
			return "", err
		}
		if !grew.IsZero() && now.Sub(grew) > lim.stale {
			return "", fmt.Errorf("the log has held %d records since %s, longer than %s", cur.size, grew.Format(time.RFC3339), lim.stale)
		}
		return fmt.Sprintf("unchanged at %d records", cur.size), nil
	}
	if prev.size > 0 {
		var body struct {
			Data struct {
				Proof []string `json:"proof"`
			} `json:"data"`
		}
		raw, err := get(client, fmt.Sprintf("%s/public/notary/log/consistency?old=%d&new=%d", api, prev.size, cur.size))
		if err != nil {
			return "", err
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			return "", fmt.Errorf("consistency proof: %w", err)
		}
		proof := make(tlog.TreeProof, len(body.Data.Proof))
		for i, s := range body.Data.Proof {
			b, err := base64.StdEncoding.DecodeString(s)
			if err != nil || len(b) != len(tlog.Hash{}) {
				return "", fmt.Errorf("consistency proof: hash %d is not a hash", i)
			}
			copy(proof[i][:], b)
		}
		if err := tlog.CheckTree(proof, cur.size, cur.root, prev.size, prev.root); err != nil {
			return "", fmt.Errorf("the head at %d records does not contain the one kept at %d: %w", cur.size, prev.size, err)
		}
	}
	return keep(path, logPath, text, cur, now, fmt.Sprintf("grew from %d to %d records, consistent", prev.size, cur.size))
}

func openCheckpoint(text []byte, verifier note.Verifier) (head, error) {
	n, err := note.Open(text, note.VerifierList(verifier))
	if err != nil {
		return head{}, err
	}
	lines := strings.Split(n.Text, "\n")
	if len(lines) < 4 || lines[0] != verifier.Name() {
		return head{}, fmt.Errorf("not a checkpoint of %s", verifier.Name())
	}
	size, err := strconv.ParseInt(lines[1], 10, 64)
	if err != nil || size < 0 {
		return head{}, fmt.Errorf("size %q", lines[1])
	}
	b, err := base64.StdEncoding.DecodeString(lines[2])
	if err != nil || len(b) != len(tlog.Hash{}) {
		return head{}, fmt.Errorf("root %q", lines[2])
	}
	h := head{size: size}
	copy(h.root[:], b)
	for _, line := range lines[3:] {
		if v, ok := strings.CutPrefix(line, "time "); ok {
			if h.signedAt, err = time.Parse(time.RFC3339, v); err != nil {
				return head{}, fmt.Errorf("time %q", v)
			}
		}
	}
	return h, nil
}

func keep(path, logPath string, text []byte, h head, now time.Time, msg string) (string, error) {
	if err := os.WriteFile(path, text, 0o644); err != nil {
		return "", err
	}
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return "", err
	}
	defer f.Close()
	line := fmt.Sprintf("%s %d %s", now.Format(time.RFC3339), h.size, base64.StdEncoding.EncodeToString(h.root[:]))
	if !h.signedAt.IsZero() {
		line += " signed " + h.signedAt.Format(time.RFC3339)
	}
	if _, err := fmt.Fprintln(f, line); err != nil {
		return "", err
	}
	return msg, nil
}

// lastGrowth is the time on the witness log's last line: the last head kept,
// which for checkpoints that name no time is the last time the log grew.
func lastGrowth(logPath string) (time.Time, error) {
	raw, err := os.ReadFile(logPath)
	if errors.Is(err, os.ErrNotExist) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	field := strings.Fields(lines[len(lines)-1])
	if len(field) == 0 {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339, field[0])
}

func get(client *http.Client, url string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "kenwea-notary-log-witness (+https://github.com/kenwea-protocol/kenwea)")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s answered %s", url, resp.Status)
	}
	return body, nil
}

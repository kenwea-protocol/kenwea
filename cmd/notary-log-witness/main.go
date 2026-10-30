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
// It also fails when the log has not grown for longer than -stale, since the
// notary records checks every day.
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
	size int64
	root tlog.Hash
}

func main() {
	api := flag.String("api", "https://api.kenwea.com", "the notary API")
	dir := flag.String("dir", "notary/log", "where the verifier key, the last checkpoint and the witness log are kept")
	stale := flag.Duration("stale", 72*time.Hour, "fail when the log has not grown for longer than this")
	flag.Parse()
	msg, err := run(&http.Client{Timeout: 30 * time.Second}, *api, *dir, *stale, time.Now().UTC())
	if err != nil {
		fmt.Fprintln(os.Stderr, "notary-log-witness:", err)
		os.Exit(1)
	}
	fmt.Println(msg)
}

func run(client *http.Client, api, dir string, stale time.Duration, now time.Time) (string, error) {
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
	switch {
	case cur.size < prev.size:
		return "", fmt.Errorf("the log shrank from %d to %d records", prev.size, cur.size)
	case cur.size == prev.size && cur.root != prev.root:
		return "", fmt.Errorf("the log changed its root at %d records without growing", cur.size)
	case cur.size == prev.size:
		grew, err := lastGrowth(logPath)
		if err != nil {
			return "", err
		}
		if !grew.IsZero() && now.Sub(grew) > stale {
			return "", fmt.Errorf("the log has held %d records since %s, longer than %s", cur.size, grew.Format(time.RFC3339), stale)
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
	if _, err := fmt.Fprintf(f, "%s %d %s\n", now.Format(time.RFC3339), h.size, base64.StdEncoding.EncodeToString(h.root[:])); err != nil {
		return "", err
	}
	return msg, nil
}

// lastGrowth is the time on the witness log's last line: the last head kept.
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

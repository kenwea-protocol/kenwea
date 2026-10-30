package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/mod/sumdb/note"
	"golang.org/x/mod/sumdb/tlog"
)

// fakeLog is a notary log in memory: its checkpoint and consistency proofs.
type fakeLog struct {
	signer note.Signer
	hashes []tlog.Hash
	size   int64
	root   *tlog.Hash // set: the checkpoint lies about its root
	signed *time.Time // set: the checkpoint names this signing time
}

var old = limits{stale: 72 * time.Hour, fresh: 3 * time.Hour}

func (l *fakeLog) ReadHashes(indexes []int64) ([]tlog.Hash, error) {
	out := make([]tlog.Hash, len(indexes))
	for i, x := range indexes {
		out[i] = l.hashes[x]
	}
	return out, nil
}

func (l *fakeLog) grow(t *testing.T, to int64) {
	for l.size < to {
		hs, err := tlog.StoredHashes(l.size, []byte(fmt.Sprint("record ", l.size)), l)
		if err != nil {
			t.Fatal(err)
		}
		l.hashes = append(l.hashes, hs...)
		l.size++
	}
}

func (l *fakeLog) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/public/notary/log/checkpoint":
		root, _ := tlog.TreeHash(l.size, l)
		if l.root != nil {
			root = *l.root
		}
		text := fmt.Sprintf("kenwea.com/notary-log\n%d\n%s\n", l.size, base64.StdEncoding.EncodeToString(root[:]))
		if l.signed != nil {
			text += "time " + l.signed.Format(time.RFC3339) + "\n"
		}
		signed, _ := note.Sign(&note.Note{Text: text}, l.signer)
		w.Write(signed)
	case "/public/notary/log/consistency":
		old, _ := strconv.ParseInt(r.URL.Query().Get("old"), 10, 64)
		proof, err := tlog.ProveTree(l.size, old, l)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		out := []string{}
		for _, h := range proof {
			out = append(out, base64.StdEncoding.EncodeToString(h[:]))
		}
		json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"proof": out}})
	default:
		http.NotFound(w, r)
	}
}

func setup(t *testing.T) (*fakeLog, *httptest.Server, string) {
	skey, vkey, err := note.GenerateKey(rand.Reader, "kenwea.com/notary-log")
	if err != nil {
		t.Fatal(err)
	}
	signer, _ := note.NewSigner(skey)
	l := &fakeLog{signer: signer}
	srv := httptest.NewServer(l)
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "verifier.txt"), []byte(vkey+"\n"), 0o644)
	return l, srv, dir
}

func TestWitnessKeepsAGrowingLog(t *testing.T) {
	l, srv, dir := setup(t)
	now := time.Date(2026, 10, 30, 15, 0, 0, 0, time.UTC)
	l.grow(t, 7)
	if msg, err := run(srv.Client(), srv.URL, dir, old, now); err != nil || msg != "first head kept" {
		t.Fatalf("first run: %q %v", msg, err)
	}
	if msg, err := run(srv.Client(), srv.URL, dir, old, now.Add(time.Hour)); err != nil || !strings.HasPrefix(msg, "unchanged") {
		t.Fatalf("unchanged run: %q %v", msg, err)
	}
	l.grow(t, 23)
	if msg, err := run(srv.Client(), srv.URL, dir, old, now.Add(2*time.Hour)); err != nil || msg != "grew from 7 to 23 records, consistent" {
		t.Fatalf("growth run: %q %v", msg, err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "witness.log"))
	if lines := strings.Split(strings.TrimSpace(string(raw)), "\n"); len(lines) != 2 || !strings.HasPrefix(lines[1], "2026-10-30T17:00:00Z 23 ") {
		t.Fatalf("witness log: %q", raw)
	}
}

func TestWitnessRefusesAHeadThatDoesNotExtendTheLastOne(t *testing.T) {
	l, srv, dir := setup(t)
	now := time.Date(2026, 10, 30, 15, 0, 0, 0, time.UTC)
	l.grow(t, 7)
	if _, err := run(srv.Client(), srv.URL, dir, old, now); err != nil {
		t.Fatal(err)
	}
	kept, _ := os.ReadFile(filepath.Join(dir, "checkpoint.txt"))

	l.grow(t, 9)
	forged := tlog.Hash{1}
	l.root = &forged // a signed head whose tree does not contain the kept one
	if _, err := run(srv.Client(), srv.URL, dir, old, now.Add(time.Hour)); err == nil || !strings.Contains(err.Error(), "does not contain") {
		t.Fatalf("forked head: %v", err)
	}
	if now, _ := os.ReadFile(filepath.Join(dir, "checkpoint.txt")); string(now) != string(kept) {
		t.Fatal("a refused head was kept")
	}

	l.root, l.size = nil, 5
	if _, err := run(srv.Client(), srv.URL, dir, old, now.Add(time.Hour)); err == nil || !strings.Contains(err.Error(), "shrank") {
		t.Fatalf("shrunk log: %v", err)
	}
	l.size = 7
	l.root = &forged
	if _, err := run(srv.Client(), srv.URL, dir, old, now.Add(time.Hour)); err == nil || !strings.Contains(err.Error(), "without growing") {
		t.Fatalf("same size, other root: %v", err)
	}
}

func TestWitnessFailsAFrozenLogAndAnotherKey(t *testing.T) {
	l, srv, dir := setup(t)
	now := time.Date(2026, 10, 30, 15, 0, 0, 0, time.UTC)
	l.grow(t, 3)
	if _, err := run(srv.Client(), srv.URL, dir, old, now); err != nil {
		t.Fatal(err)
	}
	if _, err := run(srv.Client(), srv.URL, dir, old, now.Add(73*time.Hour)); err == nil || !strings.Contains(err.Error(), "longer than") {
		t.Fatalf("frozen log: %v", err)
	}

	skey, _, _ := note.GenerateKey(rand.Reader, "kenwea.com/notary-log")
	l.signer, _ = note.NewSigner(skey) // same name, a key the witness did not pin
	if _, err := run(srv.Client(), srv.URL, dir, old, now.Add(time.Hour)); err == nil {
		t.Fatal("a checkpoint under another key was accepted")
	}
}

func TestWitnessChecksTheSignedTime(t *testing.T) {
	l, srv, dir := setup(t)
	now := time.Date(2026, 10, 30, 15, 0, 0, 0, time.UTC)
	at := now.Add(-time.Minute)
	l.signed = &at
	l.grow(t, 4)
	if _, err := run(srv.Client(), srv.URL, dir, old, now); err != nil {
		t.Fatal(err)
	}
	// A quiet log is fine while its head is freshly signed, long past -stale.
	later := now.Add(100 * time.Hour)
	at2 := later.Add(-time.Minute)
	l.signed = &at2
	if msg, err := run(srv.Client(), srv.URL, dir, old, later); err != nil || !strings.HasPrefix(msg, "unchanged at 4 records, signed ") {
		t.Fatalf("quiet, fresh: %q %v", msg, err)
	}
	// A head signed hours ago, served now: the log is frozen.
	if _, err := run(srv.Client(), srv.URL, dir, old, later.Add(4*time.Hour)); err == nil || !strings.Contains(err.Error(), "old head") {
		t.Fatalf("frozen head: %v", err)
	}
	// Signed before the head already kept.
	l.signed = &at
	if _, err := run(srv.Client(), srv.URL, dir, old, now.Add(time.Hour)); err == nil || !strings.Contains(err.Error(), "before the one kept") {
		t.Fatalf("backwards time: %v", err)
	}
	ahead := later.Add(time.Hour)
	l.signed = &ahead
	if _, err := run(srv.Client(), srv.URL, dir, old, later); err == nil || !strings.Contains(err.Error(), "ahead of the clock") {
		t.Fatalf("future time: %v", err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "witness.log"))
	if lines := strings.Split(strings.TrimSpace(string(raw)), "\n"); len(lines) != 2 || !strings.HasSuffix(lines[1], " signed "+at2.Format(time.RFC3339)) {
		t.Fatalf("witness log: %q", raw)
	}
}

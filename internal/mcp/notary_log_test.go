package mcp

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"golang.org/x/mod/sumdb/note"
	"golang.org/x/mod/sumdb/tlog"
)

type memHashes []tlog.Hash

func (m memHashes) ReadHashes(idx []int64) ([]tlog.Hash, error) {
	out := make([]tlog.Hash, len(idx))
	for i, x := range idx {
		out[i] = m[x]
	}
	return out, nil
}

type testSigner struct {
	key  ed25519.PrivateKey
	hash uint32
}

func (s testSigner) Name() string                    { return notaryLogOrigin }
func (s testSigner) KeyHash() uint32                 { return s.hash }
func (s testSigner) Sign(msg []byte) ([]byte, error) { return ed25519.Sign(s.key, msg), nil }

// A record in a log whose checkpoint the record's own key signed is proven
// included; one that is not in the log, or a checkpoint signed by another key,
// is not.
func TestCheckInLog(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	payloads := []string{`{"n":0}`, `{"n":1}`, `{"n":2}`}
	var stored memHashes
	for i, p := range payloads {
		hs, err := tlog.StoredHashes(int64(i), []byte(p), stored)
		if err != nil {
			t.Fatal(err)
		}
		stored = append(stored, hs...)
	}
	size := int64(len(payloads))
	root, _ := tlog.TreeHash(size, stored)
	kh := func(k ed25519.PublicKey) uint32 {
		h := sha256.New()
		h.Write([]byte(notaryLogOrigin + "\n"))
		h.Write(append([]byte{1}, k...))
		return binary.BigEndian.Uint32(h.Sum(nil))
	}
	text := fmt.Sprintf("%s\n%d\n%s\n", notaryLogOrigin, size, base64.StdEncoding.EncodeToString(root[:]))
	signed, err := note.Sign(&note.Note{Text: text}, testSigner{priv, kh(pub)})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/checkpoint":
			json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"checkpoint": string(signed)}})
		case "/proof":
			for i, p := range payloads {
				if fmt.Sprintf("%x", sha256.Sum256([]byte(p))) == r.URL.Query().Get("sha") {
					proof, _ := tlog.ProveRecord(size, int64(i), stored)
					var ps []string
					for _, h := range proof {
						ps = append(ps, base64.StdEncoding.EncodeToString(h[:]))
					}
					json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"index": i, "proof": ps}})
					return
				}
			}
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	old := notaryLogBase
	notaryLogBase = srv.URL
	defer func() { notaryLogBase = old }()

	if got := checkInLog(payloads[1], pub); got["included"] != true || got["index"] != int64(1) {
		t.Fatalf("an included record must be proven: %v", got)
	}
	if got := checkInLog(`{"n":9}`, pub); got["included"] != false {
		t.Fatalf("a record not in the log must say so: %v", got)
	}
	other, _, _ := ed25519.GenerateKey(nil)
	if got := checkInLog(payloads[1], other); got["included"] != false {
		t.Fatalf("a checkpoint the record's key did not sign proves nothing: %v", got)
	}
}

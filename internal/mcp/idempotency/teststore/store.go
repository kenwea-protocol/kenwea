package teststore

import (
	"sync"

	"github.com/kenwea-protocol/kenwea/apps/mcp-server/internal/mcp/idempotency"
)

type Store struct {
	mu      sync.Mutex
	records map[string]idempotency.Record
}

func New() *Store {
	return &Store{records: make(map[string]idempotency.Record)}
}

func (s *Store) Resolve(key, requestHash string, response any) (any, bool, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.records[key]
	if !ok {
		s.records[key] = idempotency.Record{RequestHash: requestHash, Response: response}
		return response, false, false, nil
	}
	if record.RequestHash != requestHash {
		return nil, true, true, nil
	}
	return record.Response, true, false, nil
}

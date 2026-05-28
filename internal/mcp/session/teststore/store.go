package teststore

import (
	"sync"

	"github.com/kenwea-protocol/kenwea/apps/mcp-server/internal/mcp/session"
)

type Store struct {
	mu       sync.Mutex
	sessions map[string]session.Actor
}

func New() *Store {
	return &Store{sessions: make(map[string]session.Actor)}
}

func (s *Store) Issue(actor session.Actor) (string, error) {
	id, err := session.NewID()
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[id] = actor
	return id, nil
}

func (s *Store) Get(id string) (session.Actor, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	actor, ok := s.sessions[id]
	return actor, ok
}

func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, id)
	return nil
}

package redisstore

import (
	"context"
	"encoding/json"
	"time"

	"github.com/kenwea-protocol/kenwea/apps/mcp-server/internal/mcp/session"
	"github.com/redis/go-redis/v9"
)

const ttl = 30 * time.Minute

type Store struct {
	client *redis.Client
	ctx    context.Context
}

func New(client *redis.Client) *Store {
	return &Store{client: client, ctx: context.Background()}
}

func (s *Store) Issue(actor session.Actor) (string, error) {
	id, err := session.NewID()
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(actor)
	if err != nil {
		return "", err
	}
	if err := s.client.Set(s.ctx, key(id), payload, ttl).Err(); err != nil {
		return "", err
	}
	return id, nil
}

func (s *Store) Get(id string) (session.Actor, bool) {
	payload, err := s.client.Get(s.ctx, key(id)).Bytes()
	if err != nil {
		return session.Actor{}, false
	}
	var actor session.Actor
	if err := json.Unmarshal(payload, &actor); err != nil {
		return session.Actor{}, false
	}
	_ = s.client.Expire(s.ctx, key(id), ttl).Err()
	return actor, true
}

func (s *Store) Delete(id string) error {
	return s.client.Del(s.ctx, key(id)).Err()
}

func key(id string) string {
	return "session:" + id
}

package redisstore

import (
	"context"
	"encoding/json"
	"time"

	"github.com/kenwea-protocol/kenwea/apps/mcp-server/internal/mcp/idempotency"
	"github.com/redis/go-redis/v9"
)

const ttl = 24 * time.Hour

type Store struct {
	client *redis.Client
	ctx    context.Context
}

func New(client *redis.Client) *Store {
	return &Store{client: client, ctx: context.Background()}
}

func (s *Store) Resolve(key, requestHash string, response any) (any, bool, bool, error) {
	record := idempotency.Record{RequestHash: requestHash, Response: response}
	payload, err := json.Marshal(record)
	if err != nil {
		return nil, false, false, err
	}
	ok, err := s.client.SetNX(s.ctx, redisKey(key), payload, ttl).Result()
	if err != nil {
		return nil, false, false, err
	}
	if ok {
		return response, false, false, nil
	}
	stored, err := s.client.Get(s.ctx, redisKey(key)).Bytes()
	if err != nil {
		return nil, false, false, err
	}
	var existing idempotency.Record
	if err := json.Unmarshal(stored, &existing); err != nil {
		return nil, false, false, err
	}
	if existing.RequestHash != requestHash {
		return nil, true, true, nil
	}
	return existing.Response, true, false, nil
}

func redisKey(key string) string {
	return "mcp:idempotency:" + key
}

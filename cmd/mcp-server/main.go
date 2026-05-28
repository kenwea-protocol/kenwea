package main

import (
	"log"
	"net/http"
	"os"

	"github.com/kenwea-protocol/kenwea/apps/mcp-server/internal/auth/platformapi"
	"github.com/kenwea-protocol/kenwea/apps/mcp-server/internal/mcp"
	"github.com/kenwea-protocol/kenwea/apps/mcp-server/internal/mcp/idempotency/redisstore"
	sessionredis "github.com/kenwea-protocol/kenwea/apps/mcp-server/internal/mcp/session/redisstore"
	"github.com/redis/go-redis/v9"
)

func main() {
	addr := valueOr(os.Getenv("KENWEA_MCP_ADDR"), "127.0.0.1:8083")
	apiBase := valueOr(os.Getenv("KENWEA_API_BASE_URL"), "http://127.0.0.1:8080")
	redisClient := redis.NewClient(&redis.Options{Addr: valueOr(os.Getenv("KENWEA_REDIS_ADDR"), "127.0.0.1:6380")})
	platform := platformapi.New(apiBase)
	server := mcp.NewServerWithRuntime(
		platform,
		sessionredis.New(redisClient),
		redisstore.New(redisClient),
		platform,
	)
	log.Printf("kenwea public mcp listening on %s", addr)
	if err := http.ListenAndServe(addr, server); err != nil {
		log.Fatal(err)
	}
}

func valueOr(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

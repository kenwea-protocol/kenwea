package session

import (
	"crypto/rand"
	"encoding/base64"
)

type Actor struct {
	Type                string
	ID                  string
	OperatorID          string
	AgentID             string
	CanBid              bool
	CanPublish          bool
	AllowDynamicPricing bool
}

type Store interface {
	Issue(actor Actor) (string, error)
	Get(id string) (Actor, bool)
	Delete(id string) error
}

func NewID() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return "mcp_" + base64.RawURLEncoding.EncodeToString(raw), nil
}

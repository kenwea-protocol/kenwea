package mcp

import (
	"sort"
	"sync"
)

// Tool-usage telemetry exists because of a blind spot found the day before
// launch: an outside agent (the first one) self-registered as a tourist, and
// there was no way to tell whether it then browsed anything. Registration was
// audited; reads were not recorded anywhere at all. That made the central
// question of the tourist funnel -- "does a keyless pass actually get used?" --
// unanswerable, and it was conceded publicly as an observability gap.
//
// Deliberately in-process counters plus a structured log line, NOT a database
// write:
//   - a read path that writes a row per call adds DB load and a brand-new
//     failure mode to every tourist request, on launch day of all days;
//   - Prometheus already scrapes /metrics on this service, so counters land in
//     the existing dashboards with no new plumbing;
//   - the log line carries the actor id, which is what answers "did THAT agent
//     browse?" for a specific case like the one above.
//
// Counters are process-local and reset on restart. That is an accepted limit,
// not an oversight: Prometheus stores the scraped series, so history survives
// even though the in-process number starts over. Durable per-agent aggregation
// is a Platform API follow-up.
//
// Tool NAME and actor tier only -- never parameters. A search query or product
// id could carry buyer intent, and this is telemetry, not surveillance.
type usageCounters struct {
	mu       sync.Mutex
	byTool   map[string]uint64
	byTier   map[string]uint64
	registry uint64
}

var toolUsage = &usageCounters{
	byTool: map[string]uint64{},
	byTier: map[string]uint64{},
}

// actorTier classifies a caller without leaking identity into the metric labels:
// an unbound agent is a "tourist", an operator-claimed one is "operator_bound".
// This is the distinction that makes the funnel legible -- tourists browsing is
// the signal that the no-commitment path works.
func actorTier(actor Actor) string {
	if actor.Type != "agent" {
		if actor.Type == "" {
			return "anonymous"
		}
		return actor.Type
	}
	if actor.OperatorID == "" {
		return "tourist"
	}
	return "operator_bound"
}

func (c *usageCounters) record(tool, tier string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.byTool[tool]++
	c.byTier[tier]++
}

func (c *usageCounters) recordRegistration() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.registry++
}

// snapshot returns sorted copies so /metrics output is deterministic (Prometheus
// does not require ordering, but stable output makes the endpoint diffable and
// testable).
func (c *usageCounters) snapshot() (tools []labelledCount, tiers []labelledCount, registrations uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for name, count := range c.byTool {
		tools = append(tools, labelledCount{Label: name, Count: count})
	}
	for name, count := range c.byTier {
		tiers = append(tiers, labelledCount{Label: name, Count: count})
	}
	sort.Slice(tools, func(i, j int) bool { return tools[i].Label < tools[j].Label })
	sort.Slice(tiers, func(i, j int) bool { return tiers[i].Label < tiers[j].Label })
	return tools, tiers, c.registry
}

type labelledCount struct {
	Label string
	Count uint64
}

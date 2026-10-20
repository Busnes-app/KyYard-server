package api

import (
	"context"
	"log"
	"sync"
	"time"
)

// serviceReadSummaryEvery is how often counted service reads become one audit row per token.
const serviceReadSummaryEvery = time.Hour

// serviceReadCounter counts reads per service token between summaries. In memory: one
// process is the supported deployment, and a lost hour of counts costs nothing an auditor
// acts on.
type serviceReadCounter struct {
	mu     sync.Mutex
	counts map[string]serviceReads
}

type serviceReads struct {
	organizationID string
	reads          int
}

func (c *serviceReadCounter) add(organizationID, tokenID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.counts == nil {
		c.counts = map[string]serviceReads{}
	}
	e := c.counts[tokenID]
	e.organizationID, e.reads = organizationID, e.reads+1
	c.counts[tokenID] = e
}

func (c *serviceReadCounter) drain() map[string]serviceReads {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.counts
	c.counts = nil
	return out
}

// RunServiceReadSummaries writes one service_token.reads row per token per hour and once
// more at shutdown, on a context detached from the loop so the last flush lands, and each hour
// revokes agent tokens unused for 90 days. It closes done only when it returns; runServer waits
// on it before the store closes.
func (s *Server) RunServiceReadSummaries(ctx context.Context, done chan<- struct{}) {
	defer close(done)
	ticker := time.NewTicker(serviceReadSummaryEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s.flushServiceReads(ctx)
			s.expireIdleAgentTokens(ctx)
		case <-ctx.Done():
			flushCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			s.flushServiceReads(flushCtx)
			cancel()
			return
		}
	}
}

func (s *Server) flushServiceReads(ctx context.Context) {
	for tokenID, e := range s.serviceReads.drain() {
		if err := s.store.Tenancy().RecordServiceTokenReads(ctx, e.organizationID, tokenID, e.reads); err != nil {
			log.Printf("[SERVICE] read summary for %s not written: %v", tokenID, err)
		}
	}
}

// expireIdleAgentTokens sweeps agent tokens nobody presents; a presented one is refused at
// lookup whether or not this has run.
func (s *Server) expireIdleAgentTokens(ctx context.Context) {
	gone, err := s.store.Tenancy().ExpireIdleAgentTokens(ctx, time.Now(), "")
	if err != nil {
		log.Printf("[MCP] idle agent token sweep: %v", err)
	}
	for _, tok := range gone {
		log.Printf("[MCP] agent token %s revoked: unused for 90 days", tok.ID)
	}
}

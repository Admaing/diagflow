package server

import (
	"sync"
	"time"

	"github.com/Admaing/diagflow/internal/observability/store"
)

// sessionCache is the hot in-memory layer: session_id → recent diagnoses
// (oldest first). Entries expire lazily; a janitor bounds memory. On miss the
// caller replays from MySQL (write-through cold start).
type sessionCache struct {
	mu   sync.Mutex
	ttl  time.Duration
	max  int // entries kept per session
	sess map[string]*sessionEntries
}

type sessionEntries struct {
	expiresAt time.Time
	items     []*store.Entry
}

func newSessionCache(ttl time.Duration, maxPerSession int) *sessionCache {
	c := &sessionCache{ttl: ttl, max: maxPerSession, sess: map[string]*sessionEntries{}}
	go c.janitor()
	return c
}

// Append adds a diagnosis entry to the session.
func (c *sessionCache) Append(sessionID string, e *store.Entry) {
	if sessionID == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	s, ok := c.sess[sessionID]
	if !ok {
		s = &sessionEntries{}
		c.sess[sessionID] = s
	}
	s.expiresAt = time.Now().Add(c.ttl)
	s.items = append(s.items, e)
	if c.max > 0 && len(s.items) > c.max {
		s.items = s.items[len(s.items)-c.max:]
	}
}

// History returns the session's entries (nil if absent/expired).
func (c *sessionCache) History(sessionID string) []*store.Entry {
	if sessionID == "" {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	s, ok := c.sess[sessionID]
	if !ok || time.Now().After(s.expiresAt) {
		return nil
	}
	return s.items
}

func (c *sessionCache) janitor() {
	t := time.NewTicker(time.Minute)
	for range t.C {
		c.mu.Lock()
		now := time.Now()
		for id, s := range c.sess {
			if now.After(s.expiresAt) {
				delete(c.sess, id)
			}
		}
		c.mu.Unlock()
	}
}

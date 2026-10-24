package mcpserver

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// ACL maps an agent name to the clusters it may query. A "*" entry grants
// every cluster in the zone. An empty ACL (nil) keeps the historical
// behaviour: any authenticated agent may query any cluster.
type ACL map[string]map[string]struct{}

// ParseACL parses "agent1=uhadoop-a,uhadoop-b;agent2=*" into an ACL. An empty
// spec returns (nil, nil), meaning unrestricted.
//
// The syntax is deliberately the same key=value shape as DIAGFLOW_MCP_TOKENS,
// separated by ';' so cluster lists can stay comma-separated.
func ParseACL(spec string) (ACL, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil, nil
	}
	out := ACL{}
	for _, entry := range strings.Split(spec, ";") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		agent, clusters, ok := strings.Cut(entry, "=")
		agent = strings.TrimSpace(agent)
		if !ok || agent == "" {
			return nil, fmt.Errorf("malformed ACL entry %q: want agent=cluster[,cluster...]", entry)
		}
		set := map[string]struct{}{}
		for _, c := range strings.Split(clusters, ",") {
			if c = strings.TrimSpace(c); c != "" {
				set[c] = struct{}{}
			}
		}
		if len(set) == 0 {
			return nil, fmt.Errorf("ACL entry %q lists no clusters", entry)
		}
		out[agent] = set
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// String renders the ACL for startup logging, sorted for determinism.
func (a ACL) String() string {
	if len(a) == 0 {
		return "(unrestricted)"
	}
	agents := make([]string, 0, len(a))
	for agent := range a {
		agents = append(agents, agent)
	}
	sort.Strings(agents)
	parts := make([]string, 0, len(agents))
	for _, agent := range agents {
		clusters := make([]string, 0, len(a[agent]))
		for c := range a[agent] {
			clusters = append(clusters, c)
		}
		sort.Strings(clusters)
		parts = append(parts, agent+"="+strings.Join(clusters, ","))
	}
	return strings.Join(parts, ";")
}

// allowed reports whether agent may query clusterID.
func (a ACL) allowed(agent, clusterID string) bool {
	if len(a) == 0 {
		return true
	}
	if agent == "" {
		// The ACL is configured but the request carried no identity: refuse
		// rather than silently granting every cluster.
		return false
	}
	clusters, ok := a[agent]
	if !ok {
		return false
	}
	if _, wildcard := clusters["*"]; wildcard {
		return true
	}
	_, ok = clusters[clusterID]
	return ok
}

// rateLimiter is a per-agent token bucket. A nil limiter allows everything.
type rateLimiter struct {
	mu      sync.Mutex
	rps     float64
	burst   float64
	buckets map[string]*tokenBucket
	now     func() time.Time
}

type tokenBucket struct {
	tokens float64
	last   time.Time
}

func newRateLimiter(rps float64, burst int) *rateLimiter {
	if rps <= 0 {
		return nil
	}
	if burst <= 0 {
		burst = int(rps)
		if burst < 1 {
			burst = 1
		}
	}
	return &rateLimiter{
		rps:     rps,
		burst:   float64(burst),
		buckets: map[string]*tokenBucket{},
		now:     time.Now,
	}
}

// allow consumes one token for key, returning false when the bucket is empty.
func (l *rateLimiter) allow(key string) bool {
	if l == nil {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	b, ok := l.buckets[key]
	if !ok {
		b = &tokenBucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}
	if elapsed := now.Sub(b.last).Seconds(); elapsed > 0 {
		b.tokens += elapsed * l.rps
		if b.tokens > l.burst {
			b.tokens = l.burst
		}
		b.last = now
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

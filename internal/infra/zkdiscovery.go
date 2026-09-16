package infra

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-zookeeper/zk"
)

// Endpoint is a service address discovered from ZooKeeper.
type Endpoint struct {
	IP   string `json:"ip"`
	Port int    `json:"port"`
}

// Addr renders "host:port".
func (e Endpoint) Addr() string { return fmt.Sprintf("%s:%d", e.IP, e.Port) }

// URL renders an "http://host:port" base URL.
func (e Endpoint) URL() string { return "http://" + e.Addr() }

// ZKDiscovery resolves UCloud services registered under /NS/.
//
// UCloud services publish one ephemeral znode per instance beneath a service
// path; the znode payload encodes the instance address either as JSON
// ("json" format, e.g. /NS/uhadoop/set*/UHADOOPManageHttpJson) or as the
// protobuf message NameNodeContent ("protobuf" format, e.g. UHADOOPAccessHTTP).
type ZKDiscovery struct {
	Hosts   []string
	Timeout time.Duration

	mu   sync.Mutex
	conn *zk.Conn
}

// NameNodeContent is UCloud's protobuf registration payload:
// field 1 (string) = ip, field 10 (varint) = port.
func decodeNameNodeContent(b []byte) (Endpoint, error) {
	var ep Endpoint
	for i := 0; i < len(b); {
		tag, n := binary.Uvarint(b[i:])
		if n <= 0 {
			return ep, fmt.Errorf("bad protobuf tag at %d", i)
		}
		i += n
		field, wire := int(tag>>3), int(tag&7)
		switch wire {
		case 0:
			v, n := binary.Uvarint(b[i:])
			if n <= 0 {
				return ep, fmt.Errorf("bad protobuf varint at %d", i)
			}
			i += n
			if field == 10 {
				ep.Port = int(v)
			}
		case 2:
			l, n := binary.Uvarint(b[i:])
			if n <= 0 {
				return ep, fmt.Errorf("bad protobuf length at %d", i)
			}
			i += n
			if i+int(l) > len(b) {
				return ep, fmt.Errorf("truncated protobuf field %d", field)
			}
			s := string(b[i : i+int(l)])
			i += int(l)
			if field == 1 {
				ep.IP = s
			}
		case 5:
			i += 4
		case 1:
			i += 8
		default:
			return ep, fmt.Errorf("unsupported protobuf wire type %d", wire)
		}
	}
	if ep.IP == "" || ep.Port == 0 {
		return ep, fmt.Errorf("incomplete registration payload (ip=%q port=%d)", ep.IP, ep.Port)
	}
	return ep, nil
}

// decodeRegistration parses one znode payload. format is "json" or
// "protobuf"; when empty the payload is sniffed.
func decodeRegistration(data []byte, format string) (Endpoint, error) {
	if format == "" {
		trimmed := strings.TrimSpace(string(data))
		if strings.HasPrefix(trimmed, "{") {
			format = "json"
		} else {
			format = "protobuf"
		}
	}
	switch format {
	case "json":
		var ep Endpoint
		if err := json.Unmarshal(data, &ep); err != nil {
			return ep, fmt.Errorf("json registration: %w", err)
		}
		if ep.IP == "" || ep.Port == 0 {
			return ep, fmt.Errorf("json registration missing ip/port: %s", string(data))
		}
		return ep, nil
	case "protobuf":
		return decodeNameNodeContent(data)
	default:
		return Endpoint{}, fmt.Errorf("unsupported node data format %q", format)
	}
}

func (d *ZKDiscovery) connect() (*zk.Conn, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.conn != nil {
		return d.conn, nil
	}
	timeout := d.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	conn, _, err := zk.Connect(d.Hosts, timeout)
	if err != nil {
		return nil, fmt.Errorf("zookeeper connect %v: %w", d.Hosts, err)
	}
	d.conn = conn
	return conn, nil
}

// Close releases the ZooKeeper connection.
func (d *ZKDiscovery) Close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.conn != nil {
		d.conn.Close()
		d.conn = nil
	}
}

// Discover lists the instances registered under an exact znode path.
func (d *ZKDiscovery) Discover(ctx context.Context, path, format string) ([]Endpoint, error) {
	conn, err := d.connect()
	if err != nil {
		return nil, err
	}
	children, _, err := conn.Children(path)
	if err != nil {
		return nil, fmt.Errorf("zookeeper children %s: %w", path, err)
	}
	var out []Endpoint
	for _, child := range children {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		data, _, err := conn.Get(path + "/" + child)
		if err != nil {
			continue
		}
		ep, err := decodeRegistration(data, format)
		if err != nil {
			continue
		}
		out = append(out, ep)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Addr() < out[j].Addr() })
	return out, nil
}

// DiscoverUnder finds a service under /NS/uhadoop/set<zoneID>/ first, then
// falls back to scanning every /NS/uhadoop/set* (the ZK set id does not
// always equal the deployment region code).
func (d *ZKDiscovery) DiscoverUnder(ctx context.Context, root, zoneID, service, format string) ([]Endpoint, string, error) {
	if zoneID != "" {
		path := fmt.Sprintf("%s/set%s/%s", root, zoneID, service)
		if eps, err := d.Discover(ctx, path, format); err == nil && len(eps) > 0 {
			return eps, path, nil
		}
	}
	conn, err := d.connect()
	if err != nil {
		return nil, "", err
	}
	sets, _, err := conn.Children(root)
	if err != nil {
		return nil, "", fmt.Errorf("zookeeper children %s: %w", root, err)
	}
	sort.Strings(sets)
	for _, set := range sets {
		if !strings.HasPrefix(set, "set") {
			continue
		}
		path := root + "/" + set + "/" + service
		if eps, err := d.Discover(ctx, path, format); err == nil && len(eps) > 0 {
			return eps, path, nil
		}
	}
	return nil, "", fmt.Errorf("service %s not found under %s (zone id %q)", service, root, zoneID)
}

// ServiceResolver resolves and caches a discovered service's endpoints.
type ServiceResolver struct {
	ZK      *ZKDiscovery
	Root    string
	ZoneID  string
	Service string
	Format  string
	TTL     time.Duration

	mu      sync.Mutex
	cached  []Endpoint
	path    string
	expiry  time.Time
	nextIdx int
}

// Endpoints returns the cached endpoints, refreshing after the TTL.
func (r *ServiceResolver) Endpoints(ctx context.Context) ([]Endpoint, error) {
	r.mu.Lock()
	if len(r.cached) > 0 && time.Now().Before(r.expiry) {
		out := r.cached
		r.mu.Unlock()
		return out, nil
	}
	r.mu.Unlock()

	eps, path, err := r.ZK.DiscoverUnder(ctx, r.Root, r.ZoneID, r.Service, r.Format)
	if err != nil {
		return nil, err
	}
	ttl := r.TTL
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	r.mu.Lock()
	r.cached, r.path, r.expiry = eps, path, time.Now().Add(ttl)
	r.mu.Unlock()
	return eps, nil
}

// Path returns the znode path the current endpoints were discovered from.
func (r *ServiceResolver) Path() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.path
}

// Pick returns one endpoint, rotating across instances.
func (r *ServiceResolver) Pick(ctx context.Context) (Endpoint, error) {
	eps, err := r.Endpoints(ctx)
	if err != nil {
		return Endpoint{}, err
	}
	if len(eps) == 0 {
		return Endpoint{}, fmt.Errorf("no live instances for %s", r.Service)
	}
	r.mu.Lock()
	ep := eps[r.nextIdx%len(eps)]
	r.nextIdx++
	r.mu.Unlock()
	return ep, nil
}

package infra

import "time"

// ServiceUHadoopAccess is the ZooKeeper service name of the UHadoop access
// HTTP action API.
const ServiceUHadoopAccess = "UHADOOPAccessHTTP"

// DefaultZKRoot is the UCloud service-registry root.
const DefaultZKRoot = "/NS/uhadoop"

// ZKOptions configures zone-scoped service discovery.
type ZKOptions struct {
	Hosts     []string
	Root      string
	ZoneID    string
	Timeout   time.Duration
	CacheTTL  time.Duration
	EnsureSet bool
}

// NewZoneClusterAccess wires ZooKeeper discovery for the UHadoop access API
// into a ClusterAccess. The caller owns the returned ZKDiscovery (via
// ClusterAccess.Resolver.ZK) and should Close it on shutdown.
func NewZoneClusterAccess(opts ZKOptions) *ClusterAccess {
	root := opts.Root
	if root == "" {
		root = DefaultZKRoot
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	zk := &ZKDiscovery{Hosts: opts.Hosts, Timeout: timeout}
	access := NewClusterAccess(&ServiceResolver{
		ZK:      zk,
		Root:    root,
		ZoneID:  opts.ZoneID,
		Service: ServiceUHadoopAccess,
		Format:  "protobuf",
		TTL:     opts.CacheTTL,
	})
	return access
}

// Close releases the underlying ZooKeeper connection.
func (a *ClusterAccess) Close() {
	if a != nil && a.Resolver != nil && a.Resolver.ZK != nil {
		a.Resolver.ZK.Close()
	}
}

package config

import (
	"strings"
	"testing"
)

func TestResolveManagerEndpointZoneStrict(t *testing.T) {
	c := Default()
	c.Infra.DiscoveryMode = "zone_endpoints"
	c.Infra.ZoneEndpoints = map[string]string{
		"test03-01": "http://manager-test03-01:20141",
		"test03-02": "http://manager-test03-02:20141",
	}
	c.Infra.ManagerAPIBase = "http://global-fallback:20141"

	got, err := c.ResolveManagerEndpoint("test03", "test03-02")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "http://manager-test03-02:20141" {
		t.Fatalf("expected zone endpoint, got %s", got)
	}

	// A zone that is not configured must NOT silently fall back to the global
	// endpoint — cross-zone diagnosis is exactly what we must prevent.
	if _, err := c.ResolveManagerEndpoint("test03", "test03-99"); err == nil {
		t.Fatal("expected error for unknown zone, got nil")
	}
}

func TestResolveManagerEndpointDirectFallback(t *testing.T) {
	c := Default()
	c.Infra.DiscoveryMode = "direct"
	c.Infra.ManagerAPIBase = "http://manager:20141"

	got, err := c.ResolveManagerEndpoint("test03", "test03-01")
	if err != nil || got != "http://manager:20141" {
		t.Fatalf("direct mode: got (%q, %v)", got, err)
	}
}

func TestValidateRequiresZoneInProduction(t *testing.T) {
	c := Default()
	c.Mode = "production"
	if errs := c.Validate(); len(errs) == 0 {
		t.Fatal("production without region/zone must fail validation")
	}

	c.Region = "test03"
	c.Zone = "test03-01"
	c.Infra.ZKHosts = []string{"zk1:2181"}
	for _, e := range c.Validate() {
		if e == "region must be set in production mode (DIAGFLOW_REGION)" ||
			e == "zone must be set in production mode (DIAGFLOW_ZONE)" {
			t.Fatalf("unexpected zone/region error: %s", e)
		}
		if strings.Contains(e, "ZooKeeper") {
			t.Fatalf("unexpected ZK error: %s", e)
		}
	}
}

// Production without ZooKeeper hosts cannot discover any cluster endpoint.
func TestValidateRequiresZKHostsInProduction(t *testing.T) {
	c := Default()
	c.Mode = "production"
	c.Region = "test03"
	c.Zone = "test03-01"
	found := false
	for _, e := range c.Validate() {
		if strings.Contains(e, "ZooKeeper hosts are required") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a ZooKeeper error, got %v", c.Validate())
	}
}

// The ZK set id is the numeric zone id, distinct from the zone name.
func TestZKZone(t *testing.T) {
	c := Default()
	c.Zone = "test03-01"
	if got := c.ZKZone(); got != "test03-01" {
		t.Fatalf("ZKZone fallback = %q", got)
	}
	c.Infra.ZKZoneID = "666003001"
	if got := c.ZKZone(); got != "666003001" {
		t.Fatalf("ZKZone = %q, want the numeric set id", got)
	}
}

func TestFromEnvZoneAndRegion(t *testing.T) {
	t.Setenv("DIAGFLOW_MODE", "production")
	t.Setenv("REGION", "test03")
	t.Setenv("ZONE", "test03-01")
	t.Setenv("DIAGFLOW_INFRA__ZONE_ENDPOINTS", "test03-01=http://a:1,test03-02=http://b:2")
	t.Setenv("UHADOOP_MANAGE_HTTP_BASE", "http://global:20141")

	Reset()
	defer Reset()
	c := FromEnv()
	if !c.IsProduction() {
		t.Fatal("expected production mode")
	}
	if c.Region != "test03" || c.Zone != "test03-01" {
		t.Fatalf("region/zone not loaded: %q/%q", c.Region, c.Zone)
	}
	if got := c.Infra.ZoneEndpoints["test03-02"]; got != "http://b:2" {
		t.Fatalf("zone endpoints not parsed: %v", c.Infra.ZoneEndpoints)
	}
}

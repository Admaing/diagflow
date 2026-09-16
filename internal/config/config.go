// Package config centralizes all tunables, mirroring diagflow/config.py.
//
// Values are loaded from environment variables (DIAGFLOW_<SECTION>__<KEY>,
// DIAGFLOW_LOG_LEVEL, DEPSEEK_API_KEY/LLM_API_KEY, OPENAI_API_KEY) with the
// same defaults as the Python implementation.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
)

// LLMConfig mirrors LLMConfig in config.py.
type LLMConfig struct {
	APIKey      string
	BaseURL     string
	Model       string
	MaxTokens   int
	VerifyModel string
	Temperature float64
	// Language is the language every LLM-generated, user-facing field is
	// written in (root cause, suggestions, investigation judgments). Use a
	// BCP-47 style code: "zh-CN" (default) or "en".
	Language string
}

// VectorStoreConfig mirrors VectorStoreConfig.
type VectorStoreConfig struct {
	Backend          string
	CollectionName   string
	EmbeddingDim     int
	ChromaPersistDir string
	MilvusDBPath     string
}

// RAGConfig mirrors RAGConfig.
type RAGConfig struct {
	ErrorKeywords []string
}

// ComponentsConfig mirrors ComponentsConfig (component → GitHub repo).
type ComponentsConfig struct {
	RepoMap map[string]string
}

// UmrAgentConfig mirrors UmrAgentConfig.
type UmrAgentConfig struct {
	Port     int
	TimeoutS int
}

// SecurityConfig mirrors SecurityConfig.
type SecurityConfig struct {
	SSHAllowed   []string
	SSHForbidden []string
}

// MySQLConfig mirrors MySQLConfig.
type MySQLConfig struct {
	Host     string
	Port     int
	User     string
	Password string
	Database string
	PoolMin  int
	PoolMax  int
}

// ServerConfig is the HTTP API server tunables.
type ServerConfig struct {
	Port               int
	ReadHeaderTimeoutS int
	ReadTimeoutS       int
	WriteTimeoutS      int
	IdleTimeoutS       int
	RequestTimeoutS    int
	DrainTimeoutS      int
	MaxBodyBytes       int
}

// MCPConfig points DiagFlow at the node-log MCP server. Empty BaseURL =
// direct infra calls (legacy mode).
type MCPConfig struct {
	BaseURL string
	Token   string
}

// DeepWikiConfig points DiagFlow at DeepWiki's own MCP server, which serves
// documentation for upstream component repos (apache/flink, apache/hadoop, …).
// It is a separate MCP connection from the node-log gateway on purpose: it is
// a public third-party service with a different trust and network boundary.
type DeepWikiConfig struct {
	MCPURL string
}

// InfraConfig controls how DiagFlow resolves infrastructure endpoints.
//
// A DiagFlow instance is deployed for a single availability zone, so cluster
// metadata is resolved by discovering the zone's UHadoop access service
// through ZooKeeper rather than by hard-coding a manager URL.
type InfraConfig struct {
	// ZKHosts is the ZooKeeper ensemble, e.g. "10.72.136.107:2181,...". The
	// MCP server discovers UHADOOPAccessHTTP under /NS/uhadoop/set<zoneID>/.
	ZKHosts    []string
	ZKTimeoutS int
	// ZKRoot is the service-registry root ("/NS/uhadoop").
	ZKRoot string
	// ZKZoneID is the ZooKeeper set id used for discovery. It is the numeric
	// zone id (e.g. "666003001"), which differs from the human zone name
	// ("test03-01"); when empty, Zone is used and discovery falls back to
	// scanning every /NS/uhadoop/set* entry.
	ZKZoneID string

	// Legacy: direct manager URL / per-zone map. Retained so the HTTP API can
	// still be pointed at a single manager in dev; the MCP server no longer
	// uses these.
	DiscoveryMode  string
	ManagerAPIBase string
	ZoneEndpoints  map[string]string
	TimeoutS       int
}

// Config is the root config object.
type Config struct {
	LLM         LLMConfig
	VectorStore VectorStoreConfig
	RAG         RAGConfig
	Components  ComponentsConfig
	UmrAgent    UmrAgentConfig
	Security    SecurityConfig
	MySQL       MySQLConfig
	Server      ServerConfig
	MCP         MCPConfig
	Infra       InfraConfig
	DeepWiki    DeepWikiConfig

	LogLevel      string
	LogFormat     string
	StrategiesDir string
	Mode          string
	AuthToken     string
	OpenAIAPIKey  string
	Region        string
	Zone          string
}

// Default returns a Config populated with the same defaults as config.py.
func Default() *Config {
	c := &Config{}

	c.LLM = LLMConfig{
		BaseURL:     "https://api.modelverse.cn",
		Model:       "deepseek-v4.1-flash",
		MaxTokens:   4096,
		VerifyModel: "deepseek-v4.1-flash",
		Temperature: 0.3,
		// Diagnosis output defaults to Simplified Chinese: the report's labels
		// and the operators reading it are Chinese, so English LLM prose made
		// the report read as half-translated.
		Language: "zh-CN",
	}

	c.VectorStore = VectorStoreConfig{
		Backend:          "chromadb",
		CollectionName:   "diagnosis_cases",
		EmbeddingDim:     1536,
		ChromaPersistDir: "/tmp/diagflow/chroma",
		MilvusDBPath:     "/tmp/diagflow/milvus.db",
	}

	c.RAG = RAGConfig{
		ErrorKeywords: []string{
			"OutOfMemoryError", "OOM", "FATAL", "CheckpointExpired",
			"NoSpaceLeft", "disk full", "backpressure", "timeout",
			"GC overhead", "Connection refused", "NoRouteToHost",
			"FileNotFound", "Permission denied",
		},
	}

	c.Components = ComponentsConfig{RepoMap: map[string]string{
		"flink": "apache/flink", "hdfs": "apache/hadoop", "yarn": "apache/hadoop",
		"hadoop": "apache/hadoop", "kafka": "apache/kafka", "spark": "apache/spark",
		"hbase": "apache/hbase", "hive": "apache/hive", "airflow": "apache/airflow",
	}}

	c.UmrAgent = UmrAgentConfig{Port: 65431, TimeoutS: 10}

	c.Security = SecurityConfig{
		SSHAllowed: []string{
			"cat", "grep", "find", "tail", "head", "ls", "ps",
			"df", "free", "curl", "du", "wc", "echo", "stat",
			"ss", "netstat", "ip", "hostname", "uptime", "uname",
			"awk", "sed", "sort", "uniq", "cut", "tr",
			"yarn", "pwd",
		},
		SSHForbidden: []string{
			"rm ", "shutdown", "reboot", "mv ", "dd ", "mkfs",
			">", ">>", "| sh", "$(", "`",
		},
	}

	c.MySQL = MySQLConfig{
		Host: "127.0.0.1", Port: 3306, User: "diagflow",
		Database: "diagflow", PoolMin: 2, PoolMax: 10,
	}

	c.Server = ServerConfig{
		Port:               8080,
		ReadHeaderTimeoutS: 10,
		ReadTimeoutS:       30,
		WriteTimeoutS:      300,
		IdleTimeoutS:       120,
		RequestTimeoutS:    240,
		DrainTimeoutS:      30,
		MaxBodyBytes:       1 << 20,
	}

	c.LogLevel = "WARNING"
	c.LogFormat = "%(asctime)s [%(levelname)s] %(name)s: %(message)s"

	c.Infra = InfraConfig{
		DiscoveryMode: "direct",
		TimeoutS:      15,
		ZKRoot:        "/NS/uhadoop",
		ZKTimeoutS:    10,
		ZoneEndpoints: map[string]string{},
	}

	// DeepWiki is a public, auth-free MCP server; default it on so
	// deepwiki_query works out of the box (set the env var to override).
	c.DeepWiki = DeepWikiConfig{MCPURL: "https://mcp.deepwiki.com/mcp"}

	return c
}

var (
	mu    sync.Mutex
	cache *Config
)

// Get returns the cached config singleton, loading from env on first call.
// ResetConfig clears the cache (used in tests).
func Get() *Config {
	mu.Lock()
	defer mu.Unlock()
	if cache == nil {
		cache = FromEnv()
	}
	return cache
}

// Reset clears the cached singleton.
func Reset() {
	mu.Lock()
	defer mu.Unlock()
	cache = nil
}

// IsProduction reports whether the process should talk to real infrastructure
// instead of the simulated demo cluster. Anything other than "production"/
// "prod" keeps the safe demo default.
func (c *Config) IsProduction() bool {
	switch strings.ToLower(strings.TrimSpace(c.Mode)) {
	case "production", "prod":
		return true
	default:
		return false
	}
}

// Validate checks config invariants; returns a list of problems (empty = ok).
func (c *Config) Validate() []string {
	var errs []string
	if c.Server.Port <= 0 || c.Server.Port > 65535 {
		errs = append(errs, "server.port must be in 1..65535")
	}
	if c.Server.RequestTimeoutS <= 0 {
		errs = append(errs, "server.request_timeout_s must be > 0")
	}
	if c.Server.MaxBodyBytes <= 0 {
		errs = append(errs, "server.max_body_bytes must be > 0")
	}
	if c.LLM.Model == "" {
		errs = append(errs, "llm.model must not be empty")
	}
	if c.LLM.MaxTokens <= 0 {
		errs = append(errs, "llm.max_tokens must be > 0")
	}
	if c.LLM.Temperature < 0 || c.LLM.Temperature > 2 {
		errs = append(errs, "llm.temperature must be in 0..2")
	}
	if c.Server.RequestTimeoutS >= c.Server.WriteTimeoutS {
		errs = append(errs, "server.request_timeout_s should be < server.write_timeout_s")
	}
	// A zone-level deployment must know which availability zone it serves and
	// where to discover that zone's services.
	if c.IsProduction() {
		if strings.TrimSpace(c.Region) == "" {
			errs = append(errs, "region must be set in production mode (DIAGFLOW_REGION)")
		}
		if strings.TrimSpace(c.Zone) == "" {
			errs = append(errs, "zone must be set in production mode (DIAGFLOW_ZONE)")
		}
		if len(c.Infra.ZKHosts) == 0 {
			errs = append(errs, "ZooKeeper hosts are required in production "+
				"to discover cluster endpoints (DIAGFLOW_ZK_HOSTS)")
		}
	}
	return errs
}

// ResolveManagerEndpoint returns the uhadoop-manage base URL for an
// availability zone. It is zone-strict: a named zone that is not configured is
// an error rather than a silent cross-zone fallback — one instance must never
// diagnose another zone's cluster through the wrong manager.
func (c *Config) ResolveManagerEndpoint(region, zone string) (string, error) {
	for _, z := range []string{zone, c.Zone} {
		if z == "" {
			continue
		}
		if url := strings.TrimSpace(c.Infra.ZoneEndpoints[z]); url != "" {
			return url, nil
		}
	}
	switch strings.ToLower(strings.TrimSpace(c.Infra.DiscoveryMode)) {
	case "", "direct":
		if url := strings.TrimSpace(c.Infra.ManagerAPIBase); url != "" {
			return url, nil
		}
	}
	if zone == "" {
		zone = c.Zone
	}
	if r := region; r == "" {
		region = c.Region
	}
	return "", fmt.Errorf("no manager endpoint for region=%q zone=%q (mode=%s)",
		region, zone, c.Infra.DiscoveryMode)
}

// FromEnv builds a Config from environment variables, layered on defaults.
func FromEnv() *Config {
	c := Default()

	// Flat env vars.
	if v := os.Getenv("DEEPSEEK_API_KEY"); v != "" {
		c.LLM.APIKey = v
	} else if v := os.Getenv("LLM_API_KEY"); v != "" {
		c.LLM.APIKey = v
	}
	if v := os.Getenv("OPENAI_API_KEY"); v != "" {
		c.OpenAIAPIKey = v
	}
	setStr := func(dst *string, key string) {
		if v := os.Getenv(key); v != "" {
			*dst = v
		}
	}
	setStr(&c.LogLevel, "DIAGFLOW_LOG_LEVEL")
	setStr(&c.LogFormat, "DIAGFLOW_LOG_FORMAT")
	setStr(&c.Mode, "DIAGFLOW_MODE")
	setStr(&c.StrategiesDir, "DIAGFLOW_STRATEGIES_DIR")
	setStr(&c.AuthToken, "DIAGFLOW_AUTH_TOKEN")
	setStr(&c.MCP.BaseURL, "DIAGFLOW_MCP_BASE_URL")
	setStr(&c.MCP.Token, "DIAGFLOW_MCP_TOKEN")
	setStr(&c.DeepWiki.MCPURL, "DIAGFLOW_DEEPWIKI_MCP_URL")
	// Availability zone / region: a zone-level deployment is pinned to one zone.
	setStr(&c.Region, "DIAGFLOW_REGION")
	setStr(&c.Zone, "DIAGFLOW_ZONE")
	// Legacy flat names used by the Kubernetes manifests.
	setStr(&c.Region, "REGION")
	setStr(&c.Zone, "ZONE")
	// Legacy global endpoint; still honored by "direct" discovery mode.
	setStr(&c.Infra.ManagerAPIBase, "UHADOOP_MANAGE_HTTP_BASE")
	if v := os.Getenv("DIAGFLOW_ZK_HOSTS"); v != "" {
		c.Infra.ZKHosts = splitList(v)
	}

	// Nested env vars: DIAGFLOW_LLM__<KEY>, DIAGFLOW_VECTOR_STORE__<KEY>,
	// DIAGFLOW_MYSQL__<KEY>, DIAGFLOW_UMR_AGENT__<KEY> (lowercased key).
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "DIAGFLOW_") {
			continue
		}
		k, v, _ := strings.Cut(kv, "=")
		if !strings.Contains(k, "__") {
			continue
		}
		section, key := splitUpper(k[len("DIAGFLOW_"):])
		field := strings.ToLower(key)
		applyNested(c, section, field, v)
	}

	return c
}

// splitUpper splits "LLM__API_KEY" into ("LLM", "API_KEY").
func splitUpper(s string) (string, string) {
	parts := strings.SplitN(s, "__", 2)
	if len(parts) == 2 {
		return parts[0], parts[1]
	}
	return parts[0], ""
}

func applyNested(c *Config, section, field, value string) {
	switch section {
	case "LLM":
		switch field {
		case "api_key":
			c.LLM.APIKey = value
		case "base_url":
			c.LLM.BaseURL = value
		case "model":
			c.LLM.Model = value
		case "max_tokens":
			c.LLM.MaxTokens = atoi(value, c.LLM.MaxTokens)
		case "verify_model":
			c.LLM.VerifyModel = value
		case "language":
			c.LLM.Language = value
		case "temperature":
			c.LLM.Temperature = atof(value, c.LLM.Temperature)
		}
	case "VECTOR_STORE":
		switch field {
		case "backend":
			c.VectorStore.Backend = value
		case "collection_name":
			c.VectorStore.CollectionName = value
		case "embedding_dim":
			c.VectorStore.EmbeddingDim = atoi(value, c.VectorStore.EmbeddingDim)
		case "chroma_persist_dir":
			c.VectorStore.ChromaPersistDir = value
		case "milvus_db_path":
			c.VectorStore.MilvusDBPath = value
		}
	case "MYSQL":
		switch field {
		case "host":
			c.MySQL.Host = value
		case "port":
			c.MySQL.Port = atoi(value, c.MySQL.Port)
		case "user":
			c.MySQL.User = value
		case "password":
			c.MySQL.Password = value
		case "database":
			c.MySQL.Database = value
		}
	case "UMR_AGENT":
		switch field {
		case "port":
			c.UmrAgent.Port = atoi(value, c.UmrAgent.Port)
		case "timeout_s":
			c.UmrAgent.TimeoutS = atoi(value, c.UmrAgent.TimeoutS)
		}
	case "SERVER":
		switch field {
		case "port":
			c.Server.Port = atoi(value, c.Server.Port)
		case "request_timeout_s":
			c.Server.RequestTimeoutS = atoi(value, c.Server.RequestTimeoutS)
		case "drain_timeout_s":
			c.Server.DrainTimeoutS = atoi(value, c.Server.DrainTimeoutS)
		case "max_body_bytes":
			c.Server.MaxBodyBytes = atoi(value, c.Server.MaxBodyBytes)
		case "region":
			c.Region = value
		case "zone":
			c.Zone = value
		}
	case "INFRA":
		switch field {
		case "zk_hosts":
			c.Infra.ZKHosts = splitList(value)
		case "zk_timeout_s":
			c.Infra.ZKTimeoutS = atoi(value, c.Infra.ZKTimeoutS)
		case "zk_root":
			c.Infra.ZKRoot = value
		case "zk_zone_id":
			c.Infra.ZKZoneID = value
		case "discovery_mode":
			c.Infra.DiscoveryMode = value
		case "manager_api_base":
			c.Infra.ManagerAPIBase = value
		case "zone_endpoints":
			c.Infra.ZoneEndpoints = parseZoneEndpoints(value)
		case "timeout_s":
			c.Infra.TimeoutS = atoi(value, c.Infra.TimeoutS)
		}
	case "DEEPWIKI":
		if field == "mcp_url" {
			c.DeepWiki.MCPURL = value
		}
	}
}

// ZKZone returns the ZooKeeper set id used for service discovery.
func (c *Config) ZKZone() string {
	if z := strings.TrimSpace(c.Infra.ZKZoneID); z != "" {
		return z
	}
	return c.Zone
}

// ZKRootOr returns the configured service-registry root or the default.
func (c *Config) ZKRootOr(def string) string {
	if r := strings.TrimSpace(c.Infra.ZKRoot); r != "" {
		return r
	}
	return def
}

// splitList splits a comma-separated env value, trimming blanks.
func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// parseZoneEndpoints parses "zone1=http://a,zone2=http://b" into a map.
func parseZoneEndpoints(s string) map[string]string {
	out := map[string]string{}
	for _, pair := range strings.Split(s, ",") {
		zone, url, ok := strings.Cut(strings.TrimSpace(pair), "=")
		zone = strings.TrimSpace(zone)
		url = strings.TrimSpace(url)
		if ok && zone != "" && url != "" {
			out[zone] = url
		}
	}
	return out
}

func atoi(s string, def int) int {
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return def
}

func atof(s string, def float64) float64 {
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return f
	}
	return def
}

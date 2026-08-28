// Package config centralizes all tunables, mirroring diagflow/config.py.
//
// Values are loaded from environment variables (DIAGFLOW_<SECTION>__<KEY>,
// DIAGFLOW_LOG_LEVEL, DEPSEEK_API_KEY/LLM_API_KEY, OPENAI_API_KEY) with the
// same defaults as the Python implementation.
package config

import (
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

// Config is the root config object.
type Config struct {
	LLM         LLMConfig
	VectorStore VectorStoreConfig
	RAG         RAGConfig
	Components  ComponentsConfig
	UmrAgent    UmrAgentConfig
	Security    SecurityConfig
	MySQL       MySQLConfig

	LogLevel      string
	LogFormat     string
	StrategiesDir string
	Mode          string
	AuthToken     string
	OpenAIAPIKey  string
}

// Default returns a Config populated with the same defaults as config.py.
func Default() *Config {
	c := &Config{}

	c.LLM = LLMConfig{
		BaseURL:     "https://api.modelverse.cn",
		Model:       "deepseek-v4-flash",
		MaxTokens:   4096,
		VerifyModel: "deepseek-v4-flash",
		Temperature: 0.3,
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

	c.LogLevel = "WARNING"
	c.LogFormat = "%(asctime)s [%(levelname)s] %(name)s: %(message)s"

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
	}
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

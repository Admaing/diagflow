"""
Centralized configuration — single source of truth for all tunables.

Loads from environment variables with sensible defaults. Supports optional
YAML config file override. Use ``get_config()`` to obtain the singleton.

Usage::

    from diagflow.config import get_config
    cfg = get_config()
    client = Anthropic(api_key=cfg.llm.api_key, base_url=cfg.llm.base_url)

Environment variables follow the pattern ``DIAGFLOW_<SECTION>__<KEY>``::

    DIAGFLOW_LLM__MODEL=deepseek-v4-flash
    DIAGFLOW_VECTOR_STORE__PERSIST_DIR=/data/chroma
"""

from __future__ import annotations

import functools
import logging
import os
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any


# ---------------------------------------------------------------------------
# Nested config models
# ---------------------------------------------------------------------------


@dataclass
class LLMConfig:
    """LLM / API proxy settings."""

    api_key: str = ""
    base_url: str = "https://api.modelverse.cn"
    model: str = "deepseek-v4-flash"
    max_tokens: int = 4096
    verify_model: str = "deepseek-v4-flash"  # used by validator, can differ
    temperature: float = 0.3


@dataclass
class NLUConfig:
    """Natural Language Understanding tweaks."""

    use_llm: bool = True  # set False to fall back to keyword matching


@dataclass
class VectorStoreConfig:
    """Vector store settings (ChromaDB or Milvus)."""

    backend: str = "chromadb"          # chromadb | milvus
    collection_name: str = "diagnosis_cases"
    embedding_dim: int = 1536          # text-embedding-3-small dim
    chroma_persist_dir: str = "/tmp/diagflow/chroma"
    milvus_db_path: str = "/tmp/diagflow/milvus.db"  # Milvus Lite local file


@dataclass
class RAGConfig:
    """RAG / knowledge base settings."""

    error_keywords: list[str] = field(default_factory=lambda: [
        "OutOfMemoryError", "OOM", "FATAL", "CheckpointExpired",
        "NoSpaceLeft", "disk full", "backpressure", "timeout",
        "GC overhead", "Connection refused", "NoRouteToHost",
        "FileNotFound", "Permission denied",
    ])


@dataclass
class ComponentsConfig:
    """Component → GitHub repo mapping for DeepWiki lookups."""

    repo_map: dict[str, str] = field(default_factory=lambda: {
        "flink": "apache/flink",
        "hdfs": "apache/hadoop",
        "yarn": "apache/hadoop",
        "hadoop": "apache/hadoop",
        "kafka": "apache/kafka",
        "spark": "apache/spark",
        "hbase": "apache/hbase",
        "hive": "apache/hive",
        "airflow": "apache/airflow",
    })


@dataclass
class UmrAgentConfig:
    """umrAgent client settings."""

    port: int = 65431
    timeout_s: int = 10


@dataclass
class SecurityConfig:
    """Safety guardrails."""

    # SSH command allowlist — only these first-words are permitted
    ssh_allowed: list[str] = field(default_factory=lambda: [
        "cat", "grep", "find", "tail", "head", "ls", "ps",
        "df", "free", "curl", "du", "wc", "echo", "stat",
        "ss", "netstat", "ip", "hostname", "uptime", "uname",
        "awk", "sed", "sort", "uniq", "cut", "tr",
        "yarn",   # yarn logs -applicationId
        "pwd",    # directory inspection
    ])
    # Forbidden substrings anywhere in the command
    ssh_forbidden: list[str] = field(default_factory=lambda: [
        "rm ", "shutdown", "reboot", "mv ", "dd ", "mkfs",
        ">", ">>", "| sh", "$(", "`",
    ])


@dataclass
class MySQLConfig:
    """MySQL connection settings."""

    host: str = "127.0.0.1"
    port: int = 3306
    user: str = "diagflow"
    password: str = ""
    database: str = "diagflow"
    pool_min: int = 2
    pool_max: int = 10


@dataclass
class Config:
    """Root configuration dataclass.

    Create via ``Config.from_env()`` or ``get_config()`` (cached singleton).
    """

    llm: LLMConfig = field(default_factory=LLMConfig)
    nlu: NLUConfig = field(default_factory=NLUConfig)
    vector_store: VectorStoreConfig = field(default_factory=VectorStoreConfig)
    rag: RAGConfig = field(default_factory=RAGConfig)
    components: ComponentsConfig = field(default_factory=ComponentsConfig)
    umr_agent: UmrAgentConfig = field(default_factory=UmrAgentConfig)
    security: SecurityConfig = field(default_factory=SecurityConfig)
    mysql: MySQLConfig = field(default_factory=MySQLConfig)

    log_level: str = "WARNING"
    log_format: str = "%(asctime)s [%(levelname)s] %(name)s: %(message)s"
    strategies_dir: str = ""
    mode: str = ""  # "production" | ""

    # ------------------------------------------------------------------
    # Factory
    # ------------------------------------------------------------------

    @classmethod
    def from_env(cls, yaml_path: str = "") -> "Config":
        """Build a Config from environment variables + optional YAML file.

        Environment variables take precedence over YAML, which takes
        precedence over defaults.

        Supported env vars (double-underscore for nested keys)::

            DIAGFLOW_LLM__API_KEY
            DIAGFLOW_LLM__BASE_URL
            DIAGFLOW_LLM__MODEL
            DIAGFLOW_LLM__MAX_TOKENS
            DIAGFLOW_LOG_LEVEL
            DIAGFLOW_MODE
            DIAGFLOW_STRATEGIES_DIR
            DIAGFLOW_VECTOR_STORE__PERSIST_DIR
            DIAGFLOW_AUTH_TOKEN
            ...
        """
        config = cls()

        # 1. Load YAML file if present
        if yaml_path:
            config._merge_yaml(yaml_path)
        else:
            # Default search paths
            for candidate in [
                Path.cwd() / "config.yaml",
                Path.cwd() / "diagflow" / "config.yaml",
            ]:
                if candidate.exists():
                    config._merge_yaml(str(candidate))
                    break

        # 2. Overlay environment variables
        config._merge_env()

        return config

    # ------------------------------------------------------------------
    # Helpers
    # ------------------------------------------------------------------

    def _merge_yaml(self, path: str) -> None:
        try:
            import yaml

            with open(path) as f:
                data = yaml.safe_load(f) or {}
        except Exception:
            return

        self._apply_dict(data, [])

    def _merge_env(self) -> None:
        """Apply DIAGFLOW_* env vars to nested dataclass fields."""
        prefix = "DIAGFLOW_"

        # Flat env vars (top-level)
        flat_map = {
            "DIAGFLOW_LOG_LEVEL": ("log_level",),
            "DIAGFLOW_LOG_FORMAT": ("log_format",),
            "DIAGFLOW_MODE": ("mode",),
            "DIAGFLOW_STRATEGIES_DIR": ("strategies_dir",),
            "DIAGFLOW_AUTH_TOKEN": ("auth_token",),
            "DEEPSEEK_API_KEY": ("llm", "api_key"),
            "LLM_API_KEY": ("llm", "api_key"),
            "OPENAI_API_KEY": ("openai_api_key",),  # kept for embedder
        }

        for env_key, path in flat_map.items():
            val = os.environ.get(env_key, "")
            if val:
                self._set_nested(path, val)

        # Nested env vars: DIAGFLOW_LLM__MODEL → llm.model
        nested_prefixes = {
            "DIAGFLOW_LLM__": ("llm",),
            "DIAGFLOW_VECTOR_STORE__": ("vector_store",),
            "DIAGFLOW_MYSQL__": ("mysql",),
        }
        for env_prefix, base_path in nested_prefixes.items():
            for key, val in os.environ.items():
                if key.startswith(env_prefix):
                    field = key[len(env_prefix):].lower()
                    self._set_nested(base_path + (field,), val)

    def _apply_dict(self, data: dict, path: list[str]) -> None:
        """Recursively apply nested dict to dataclass fields."""
        for key, val in data.items():
            current = path + [key]
            if isinstance(val, dict):
                self._apply_dict(val, current)
            else:
                self._set_nested(tuple(current), val)

    def _set_nested(self, path: tuple[str, ...], value: Any) -> None:
        """Set a nested attribute path, converting types as needed."""
        if not path:
            return

        # Walk to the parent object
        obj: Any = self
        for part in path[:-1]:
            if hasattr(obj, part):
                obj = getattr(obj, part)
            else:
                return  # path doesn't exist — skip

        field_name = path[-1]
        if not hasattr(obj, field_name):
            return

        current_val = getattr(obj, field_name)
        # Convert to the existing field's type
        if current_val is not None:
            try:
                target_type = type(current_val)
                if target_type is bool:
                    value = str(value).lower() in ("1", "true", "yes")
                elif target_type is int:
                    value = int(value)
                elif target_type is float:
                    value = float(value)
                elif target_type is list and isinstance(value, str):
                    value = [v.strip() for v in value.split(",") if v.strip()]
            except (ValueError, TypeError):
                return  # conversion failed — keep default

        setattr(obj, field_name, value)


# ---------------------------------------------------------------------------
# Singleton accessor
# ---------------------------------------------------------------------------


@functools.lru_cache(maxsize=1)
def get_config(yaml_path: str = "") -> Config:
    """Return the cached Config singleton.

    The first call builds from env; subsequent calls return the cached copy.
    Pass ``yaml_path`` only on the first call.
    """
    return Config.from_env(yaml_path=yaml_path)


def reset_config() -> None:
    """Clear the config cache (useful in tests)."""
    get_config.cache_clear()


def configure_logging(config: Config | None = None) -> None:
    """Apply logging configuration from a Config instance."""
    cfg = config or get_config()
    logging.basicConfig(
        level=getattr(logging, cfg.log_level.upper(), logging.WARNING),
        format=cfg.log_format,
    )

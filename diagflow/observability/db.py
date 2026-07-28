"""
MySQL persistence layer — diagnostic history, feedback, cluster profiles.

All DB operations are optional: if MySQL is unreachable, diagnosis still
completes — DB writes are logged and skipped gracefully.
"""

from __future__ import annotations

import json
import logging
from typing import Any

from diagflow.config import get_config

logger = logging.getLogger(__name__)

_pool = None  # aiomysql.Pool | None


async def get_pool():
    """Return the shared connection pool (lazy init, singleton)."""
    global _pool
    if _pool is not None:
        return _pool
    cfg = get_config()
    if not cfg.mysql.host or not cfg.mysql.password:
        logger.debug("MySQL not configured — skipping DB persistence")
        return None
    try:
        import aiomysql
        _pool = await aiomysql.create_pool(
            host=cfg.mysql.host,
            port=cfg.mysql.port,
            user=cfg.mysql.user,
            password=cfg.mysql.password,
            db=cfg.mysql.database,
            minsize=cfg.mysql.pool_min,
            maxsize=cfg.mysql.pool_max,
            autocommit=True,
            charset="utf8mb4",
        )
        logger.info("MySQL pool created: %s:%d/%s", cfg.mysql.host, cfg.mysql.port, cfg.mysql.database)
    except Exception:
        logger.warning("MySQL connection failed — DB persistence disabled", exc_info=True)
        _pool = None
    return _pool


async def close_pool() -> None:
    global _pool
    if _pool:
        _pool.close()
        await _pool.wait_closed()
        _pool = None


# ------------------------------------------------------------------
# diagnosis_history
# ------------------------------------------------------------------

async def insert_diagnosis(
    event_id: str,
    component: str,
    problem_type: str,
    cluster_id: str = "",
    region: str = "",
    version: str = "",
    root_cause: str = "",
    confidence: str = "medium",
    suggestions: list[str] | None = None,
    evidence_count: int = 0,
    kb_matched: bool = False,
    kb_match_phase: str = "",
    phases_run: list[str] | None = None,
    duration_ms: int = 0,
    llm_calls: int = 0,
    error_msg: str = "",
) -> None:
    pool = await get_pool()
    if not pool:
        return
    try:
        async with pool.acquire() as conn:
            async with conn.cursor() as cur:
                await cur.execute(
                    """INSERT INTO diagnosis_history
                    (event_id, component, problem_type, cluster_id, region, version,
                     root_cause, confidence, suggestions, evidence_count,
                     kb_matched, kb_match_phase, phases_run, duration_ms, llm_calls, error_msg)
                    VALUES (%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s)""",
                    (
                        event_id, component, problem_type, cluster_id, region, version,
                        root_cause, confidence,
                        json.dumps(suggestions or [], ensure_ascii=False),
                        evidence_count,
                        1 if kb_matched else 0, kb_match_phase,
                        ",".join(phases_run or []),
                        duration_ms, llm_calls, error_msg,
                    ),
                )
    except Exception:
        logger.warning("Failed to insert diagnosis_history", exc_info=True)


# ------------------------------------------------------------------
# diagnosis_feedback
# ------------------------------------------------------------------

async def insert_feedback(
    event_id: str,
    is_correct: bool,
    comment: str = "",
    corrected_by: str = "",
) -> None:
    pool = await get_pool()
    if not pool:
        return
    try:
        async with pool.acquire() as conn:
            async with conn.cursor() as cur:
                await cur.execute(
                    """INSERT INTO diagnosis_feedback (event_id, is_correct, comment, corrected_by)
                    VALUES (%s,%s,%s,%s)
                    ON DUPLICATE KEY UPDATE is_correct=VALUES(is_correct),
                                            comment=VALUES(comment),
                                            corrected_by=VALUES(corrected_by)""",
                    (event_id, 1 if is_correct else 0, comment, corrected_by),
                )
    except Exception:
        logger.warning("Failed to insert feedback", exc_info=True)


# ------------------------------------------------------------------
# cluster_profile
# ------------------------------------------------------------------

async def upsert_cluster(
    cluster_id: str,
    component: str,
    version: str = "",
    kb_hit: bool = False,
    duration_ms: int = 0,
    node_count: int = 0,
    installed_apps: list[str] | None = None,
) -> None:
    pool = await get_pool()
    if not pool:
        return
    try:
        async with pool.acquire() as conn:
            async with conn.cursor() as cur:
                await cur.execute(
                    """INSERT INTO cluster_profile
                    (cluster_id, component, version, total_diag, last_diag_at,
                     kb_hit_rate, avg_duration_ms, node_count, installed_apps)
                    VALUES (%s,%s,%s,1,NOW(),%s,%s,%s,%s)
                    ON DUPLICATE KEY UPDATE
                        version=VALUES(version),
                        total_diag=total_diag+1,
                        last_diag_at=NOW(),
                        kb_hit_rate=ROUND((kb_hit_rate*total_diag + %s)/(total_diag+1), 2),
                        avg_duration_ms=ROUND((avg_duration_ms*total_diag + %s)/(total_diag+1)),
                        node_count=VALUES(node_count),
                        installed_apps=VALUES(installed_apps)""",
                    (
                        cluster_id, component, version,
                        100.0 if kb_hit else 0.0,
                        duration_ms,
                        node_count,
                        json.dumps(installed_apps or [], ensure_ascii=False),
                        # ON DUPLICATE values
                        100.0 if kb_hit else 0.0,
                        duration_ms,
                    ),
                )
    except Exception:
        logger.warning("Failed to upsert cluster_profile", exc_info=True)


async def get_cluster_profile(cluster_id: str, component: str) -> dict | None:
    """Get cluster profile for Phase 1 priority matching."""
    pool = await get_pool()
    if not pool:
        return None
    try:
        import aiomysql
        async with pool.acquire() as conn:
            async with conn.cursor(aiomysql.DictCursor) as cur:
                await cur.execute(
                    "SELECT * FROM cluster_profile WHERE cluster_id=%s AND component=%s",
                    (cluster_id, component),
                )
                row = await cur.fetchone()
                if row:
                    row["common_issues"] = json.loads(row.get("common_issues", "[]") or "[]")
                    row["installed_apps"] = json.loads(row.get("installed_apps", "[]") or "[]")
                    row["last_diag_at"] = str(row["last_diag_at"]) if row.get("last_diag_at") else ""
                    return dict(row)
    except Exception:
        logger.debug("Failed to get cluster_profile", exc_info=True)
    return None


# ------------------------------------------------------------------
# strategy_execution_log
# ------------------------------------------------------------------

async def insert_strategy_log(
    event_id: str,
    phase: str,
    tool_name: str = "",
    action: str = "",
    status: str = "success",
    duration_ms: int = 0,
    summary: str = "",
    error_detail: str = "",
    step_order: int = 0,
) -> None:
    pool = await get_pool()
    if not pool:
        return
    try:
        async with pool.acquire() as conn:
            async with conn.cursor() as cur:
                await cur.execute(
                    """INSERT INTO strategy_execution_log
                    (event_id, phase, tool_name, action, status, duration_ms, summary, error_detail, step_order)
                    VALUES (%s,%s,%s,%s,%s,%s,%s,%s,%s)""",
                    (event_id, phase, tool_name, action, status, duration_ms, summary[:512], error_detail, step_order),
                )
    except Exception:
        logger.debug("Failed to insert strategy_execution_log", exc_info=True)

"""Flink checkpoint failure — checkpoint expired due to backpressure."""

from ..log_generator import flink_taskmanager_logs


def flink_checkpoint_fail_scenario():
    """Flink job checkpoints keep expiring under backpressure."""
    return {
        "context": {
            "cluster_id": "c-uhadoop-002",
            "region": "上海",
            "component": "flink",
            "version": "1.16.0",
            "job_id": "job_abc",
            "problem": "checkpoint_failure",
            "problem_desc": "Checkpoint 持续失败，任务最终自动重启",
            "detail": "Checkpoint 连续失败但无 OOM，任务频繁重启",
        },
        "logs": {
            "taskmanager-1.log": flink_taskmanager_logs(fault="checkpoint_fail"),
            "jobmanager.log": "[12:00:00] INFO  JobManager started\n[12:05:00] ERROR Checkpoint 1805 FAILED: expired\n",
        },
        "config": {
            "flink-conf.yaml": {
                "taskmanager.memory.heap.size": "4096m",
                "parallelism.default": 8,
                "execution.checkpointing.interval": "600000ms",
                "execution.checkpointing.timeout": "600000ms",
                "execution.checkpointing.tolerable-failed-checkpoints": 3,
            },
        },
        "metrics": {
            "checkpoint_failure_rate": 5,
            "input_rate_mbps": 120.0,
            "backpressure_level": "HIGH",
        },
        "expected_root_cause": "Checkpoint alignment timeout due to backpressure",
    }

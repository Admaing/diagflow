"""Flink OOM scenario — TaskManager killed by Java heap space OutOfMemoryError."""

from ..log_generator import flink_taskmanager_logs, flink_jobmanager_logs


def flink_oom_scenario():
    """A Flink job fails because TaskManager runs out of heap space."""
    return {
        "context": {
            "cluster_id": "c-uhadoop-001",
            "region": "北京二",
            "component": "flink",
            "version": "1.14.3",
            "job_id": "job_xxx",
            "problem": "job_failure",
            "problem_desc": "任务挂掉，状态 FAILED",
            "detail": "用户反馈 Flink 任务突然停止，Flink Dashboard 显示状态为 FAILED，最后一次 Checkpoint 失败",
        },
        "logs": {
            "taskmanager-1.log": flink_taskmanager_logs(fault="oom"),
            "taskmanager-2.log": flink_taskmanager_logs(fault="oom", num_workers=2, error_count=1),
            "taskmanager-3.log": flink_taskmanager_logs(fault="oom", num_workers=3, error_count=0),
            "jobmanager.log": flink_jobmanager_logs(),
        },
        "config": {
            "flink-conf.yaml": {
                "jobmanager.memory.heap.size": "1024m",
                "taskmanager.memory.heap.size": "2048m",
                "taskmanager.memory.process.size": "4096m",
                "taskmanager.numberOfTaskSlots": 4,
                "parallelism.default": 4,
                "state.backend": "rocksdb",
                "state.checkpoints.dir": "hdfs:///user/flink/checkpoints",
                "rest.port": 8081,
            },
            "yarn-site.xml": {
                "yarn.scheduler.minimum-allocation-mb": 1024,
                "yarn.scheduler.maximum-allocation-mb": 8192,
                "yarn.nodemanager.resource.memory-mb": 16384,
                "yarn.nodemanager.resource.cpu-vcores": 8,
            },
        },
        "metrics": {
            "heap_usage_percent": 95.5,
            "checkpoint_failure_rate": 3,
            "gc_pause_ms_avg": 850,
            "input_rate_mbps": 45.0,
            "backpressure_level": "HIGH",
        },
        "expected_root_cause": "TaskManager Java heap space OutOfMemoryError",
        "expected_confidence": "high",
    }

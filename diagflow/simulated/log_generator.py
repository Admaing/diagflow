"""
Generates realistic-looking log content for simulated fault scenarios.

Each method produces log text that mimics real Flink / YARN / HDFS logs
for a specific fault pattern. The logs include:
  - INFO lines (normal operation) to create noise
  - WARN lines (suspicious but not critical)
  - ERROR lines (the actual fault)
  - Stack traces where appropriate
"""

import random
import time
from typing import Literal


random.seed(42)


def flink_taskmanager_logs(
    fault: Literal["oom", "checkpoint_fail"] = "oom",
    num_workers: int = 3,
    error_count: int = 3,
) -> str:
    """Generate Flink TaskManager logs with the specified fault pattern."""
    lines: list[str] = []
    base_time = int(time.time()) - 3600

    # Normal startup logs (all workers)
    for w in range(1, num_workers + 1):
        t = base_time
        lines.append(f"[{_ts(t)}] INFO  o.a.f.r.r.TaskManagerRunner  - Starting TaskManager {w}")
        lines.append(f"[{_ts(t + 2)}] INFO  o.a.f.r.r.TaskManagerRunner  - TaskManager {w} registered at ResourceManager")
        lines.append(f"[{_ts(t + 5)}] INFO  o.a.f.r.taskexecutor.TaskExecutor  - Starting TaskExecutor for slot 0-{w}")
        lines.append(f"[{_ts(t + 7)}] INFO  o.a.f.r.r.TaskManagerRunner  - TaskManager {w} fully initialized")

    # Steady state logs
    for _ in range(20):
        t = base_time + 30 + _ * 60
        w = random.randint(1, num_workers)
        lines.append(f"[{_ts(t)}] INFO  o.a.f.r.taskexecutor.TaskExecutor  - Task {random.randint(1000, 9999)} completed on slot {random.randint(0, 3)} of TaskManager {w}")
        lines.append(f"[{_ts(t + 1)}] INFO  o.a.f.r.i.DefaultJobVertexDetails  - Checkpoint {random.randint(100, 999)} done for job job_xxx")

    if fault == "oom":
        # WARNING signs before OOM
        w = 1
        t = base_time + 1400
        lines.append(f"[{_ts(t)}] WARN  o.a.f.r.taskexecutor.TaskExecutor  - GC pause 350ms in TaskManager {w}")
        lines.append(f"[{_ts(t + 10)}] WARN  o.a.f.r.taskexecutor.slot.TimerService  - Timer service queue growing: 15000 pending timers")
        lines.append(f"[{_ts(t + 20)}] WARN  o.a.f.r.i.OperatorChain  - Operator 0xAB12 processing time increased to 850ms (prev 120ms)")
        lines.append(f"[{_ts(t + 25)}] WARN  o.a.f.r.taskexecutor.TaskExecutor  - GC pause 820ms in TaskManager {w}")
        lines.append(f"[{_ts(t + 28)}] WARN  o.a.f.r.i.OperatorChain  - Output buffer flush blocked for 450ms")
        lines.append(f"[{_ts(t + 30)}] WARN  o.a.f.r.taskexecutor.TaskExecutor  - GC pause 2100ms in TaskManager {w} — potential STW event")
        lines.append(f"[{_ts(t + 33)}] WARN  o.a.f.r.CheckpointCoordinator  - Checkpoint 1843 (job_xxx) timed out after 600000ms")
        lines.append(f"[{_ts(t + 35)}] WARN  o.a.f.r.CheckpointCoordinator  - Checkpoint 1844 (job_xxx) timed out after 600000ms")
        lines.append(f"[{_ts(t + 37)}] WARN  o.a.f.r.CheckpointCoordinator  - Checkpoint 1845 (job_xxx) timed out after 600000ms")

        # The OOM itself
        for i in range(error_count):
            lines.append(f"[{_ts(t + 40 + i * 3)}] ERROR o.a.f.r.taskexecutor.TaskExecutor  - Fatal error: java.lang.OutOfMemoryError: Java heap space")
            lines.append(f"[{_ts(t + 41 + i * 3)}] ERROR o.a.f.r.taskexecutor.TaskExecutor  -   at java.util.Arrays.copyOf(Arrays.java:3332)")
            lines.append(f"[{_ts(t + 42 + i * 3)}] ERROR o.a.f.r.taskexecutor.TaskExecutor  -   at java.lang.AbstractStringBuilder.ensureCapacityInternal(AbstractStringBuilder.java:124)")
            lines.append(f"[{_ts(t + 43 + i * 3)}] ERROR o.a.f.r.taskexecutor.TaskExecutor  -   at java.lang.AbstractStringBuilder.append(AbstractStringBuilder.java:674)")
            lines.append(f"[{_ts(t + 44 + i * 3)}] ERROR o.a.f.r.taskexecutor.TaskExecutor  -   at java.lang.StringBuilder.append(StringBuilder.java:208)")

            # GC overhead after OOM
            if i == 0:
                lines.append(f"[{_ts(t + 45)}] ERROR o.a.f.r.taskexecutor.TaskExecutor  - java.lang.OutOfMemoryError: GC overhead limit exceeded")
                lines.append(f"[{_ts(t + 46)}] ERROR o.a.f.r.taskexecutor.TaskExecutor  - TaskManager {w} process exited with code 137 (OOM killed by OS)")

        # Impact on other workers
        for j in range(1, num_workers):
            lines.append(f"[{_ts(t + 50 + j * 5)}] WARN  o.a.f.r.CheckpointCoordinator  - Lost connection to TaskManager {w}, triggering full checkpoint barrier")
            lines.append(f"[{_ts(t + 52 + j * 5)}] ERROR o.a.f.r.CheckpointCoordinator  - Checkpoint 1846 (job_xxx) FAILED: TaskManager {w} unreachable")

    elif fault == "checkpoint_fail":
        # Checkpoint failures
        for i in range(error_count + 3):
            t = base_time + 1400 + i * 15
            lines.append(f"[{_ts(t)}] WARN  o.a.f.r.CheckpointCoordinator  - Checkpoint {1800 + i} alignment took 32000ms (threshold 30000ms)")
        lines.append(f"[{_ts(t + 10)}] ERROR o.a.f.r.CheckpointCoordinator  - Checkpoint 1805 FAILED: org.apache.flink.runtime.checkpoint.CheckpointException: Checkpoint expired before completing")
        lines.append(f"[{_ts(t + 20)}] ERROR o.a.f.r.CheckpointCoordinator  - Checkpoint 1806 FAILED: org.apache.flink.runtime.checkpoint.CheckpointException: Barrier not received from subtask 3 within 600000ms")
        lines.append(f"[{_ts(t + 30)}] ERROR o.a.f.r.CheckpointCoordinator  - Checkpoint 1807 FAILED: org.apache.flink.runtime.checkpoint.CheckpointException: Barrier not received from subtask 7 within 600000ms")
        lines.append(f"[{_ts(t + 40)}] WARN  o.a.f.r.executiongraph.ExecutionGraph  - Job job_xxx exceeded allowed checkpoint failure count (3). Triggering job failover.")

    return "\n".join(lines)


def flink_jobmanager_logs(events: list[str] | None = None) -> str:
    """Generate JobManager logs."""
    lines = [
        "[12:00:00] INFO  o.a.f.r.r.JobManagerRunner  - JobManager started",
        "[12:00:01] INFO  o.a.f.r.r.JobManagerRunner  - Starting job job_xxx",
        "[12:00:02] INFO  o.a.f.r.scheduler.DefaultScheduler  - Job job_xxx scheduled with 8 tasks",
    ]
    if events:
        lines.extend(events)
    lines.append("[12:05:00] INFO  o.a.f.r.CheckpointCoordinator  - Checkpoint coordinator initialized for job_xxx")
    lines.append("[12:05:01] INFO  o.a.f.r.CheckpointCoordinator  - Checkpoint interval: 600000ms, exactly-once mode")
    return "\n".join(lines)


def yarn_logs(app_id: str = "application_1700000000000_1234") -> str:
    """Generate YARN application logs."""
    return f"""Container: container_1700000000000_1234_01_000001 on node-001
LogLastModified:2025-01-15 14:23:01
============================================================================
[14:20:00] INFO   org.apache.hadoop.yarn.client.api.impl.ContainerManagementProtocolProxy  - Opening proxy for container_1700000000000_1234_01_000001
[14:20:05] INFO   org.apache.hadoop.yarn.server.nodemanager.containermanager.ContainerManagerImpl  - Starting container container_1700000000000_1234_01_000001
[14:21:00] INFO   org.apache.hadoop.yarn.server.nodemanager.DefaultContainer  - Container {app_id} allocated 4096 MB memory, 2 vcores
[14:22:00] WARN   org.apache.hadoop.yarn.server.nodemanager.containermanager.monitor.ContainerMetrics  - Container {app_id} using 98% of allocated memory
[14:22:30] WARN   org.apache.hadoop.yarn.server.nodemanager.containermanager.monitor.ContainerMetrics  - Container {app_id} using 102% of allocated memory (over limit, process may be killed)
[14:23:01] ERROR  org.apache.hadoop.yarn.server.nodemanager.DefaultContainer  - Container {app_id} killed by OOM killer, exit code 137
"""


def hdfs_logs(fault: str = "disk_full") -> str:
    """Generate HDFS DataNode logs."""
    if fault == "disk_full":
        return (
            "[10:00:00] INFO  org.apache.hadoop.hdfs.server.datanode.DataNode  - DataNode started on node-003\n"
            "[10:01:00] INFO  org.apache.hadoop.hdfs.server.datanode.DataNode  - Registered with NameNode at nn-001:8020\n"
            "[10:30:00] WARN  org.apache.hadoop.hdfs.server.datanode.DataNode  - Volume /data/hdfs/data1: available space 5.2 GB (5% of 100 GB)\n"
            "[10:31:00] WARN  org.apache.hadoop.hdfs.server.datanode.DataNode  - Volume /data/hdfs/data2: available space 3.1 GB (3% of 100 GB)\n"
            "[10:32:00] WARN  org.apache.hadoop.hdfs.server.datanode.DataNode  - Volume /data/hdfs/data3: available space 1.8 GB (1% of 100 GB)\n"
            "[10:33:00] ERROR org.apache.hadoop.hdfs.server.datanode.DataNode  - Volume /data/hdfs/data3 is out of space: disk full\n"
            "[10:33:01] ERROR org.apache.hadoop.hdfs.server.datanode.DataNode  - Could not replicate block blk_123456789 to any node: No available space on any volume\n"
            "[10:34:00] ERROR org.apache.hadoop.hdfs.server.datanode.DataNode  - Too many failed writes on volume /data/hdfs/data3. Volume will be marked as FAILED\n"
            "[10:35:00] INFO  org.apache.hadoop.hdfs.server.datanode.DataNode  - Volume /data/hdfs/data3 has been removed from active volume list\n"
        )


def _ts(t: float) -> str:
    """Format a timestamp as HH:MM:SS."""
    lt = time.localtime(t)
    return f"{lt.tm_hour:02d}:{lt.tm_min:02d}:{lt.tm_sec:02d}"

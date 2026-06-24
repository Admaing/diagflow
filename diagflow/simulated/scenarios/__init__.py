"""
Pre-built fault scenarios for demo and testing.

Each scenario is a function that returns:
  - context: dict of user-provided information (cluster, job id, etc.)
  - expected_issue: what the diagnosis should find (for verification)
"""

from .flink_oom import flink_oom_scenario
from .flink_checkpoint_fail import flink_checkpoint_fail_scenario
from .hdfs_disk_full import hdfs_disk_full_scenario
from .yarn_queue_stuck import yarn_queue_stuck_scenario

ALL_SCENARIOS = {
    "flink_oom": flink_oom_scenario,
    "flink_checkpoint_fail": flink_checkpoint_fail_scenario,
    "hdfs_disk_full": hdfs_disk_full_scenario,
    "yarn_queue_stuck": yarn_queue_stuck_scenario,
}

"""YARN queue stuck scenario — resource congestion on a shared queue."""

from ..log_generator import yarn_logs


def yarn_queue_stuck_scenario():
    """YARN queue is congested, apps are pending due to resource exhaustion."""
    return {
        "context": {
            "cluster_id": "c-uhadoop-004",
            "region": "北京二",
            "component": "yarn",
            "version": "3.3.4",
            "problem": "queue_stuck",
            "problem_desc": "任务提交后一直处于 ACCEPTED 状态，无法分配到容器",
            "detail": "用户提交 Spark 任务后，YARN Application 一直显示 ACCEPTED，无法获取容器资源",
        },
        "logs": {
            "resourcemanager.log": (
                "[10:00:00] INFO  org.apache.hadoop.yarn.server.resourcemanager.ResourceManager - RM started\n"
                "[10:01:00] WARN  org.apache.hadoop.yarn.server.resourcemanager.scheduler.fair.FairScheduler - Queue 'root.production' is at 100% capacity\n"
                "[10:02:00] WARN  org.apache.hadoop.yarn.server.resourcemanager.scheduler.fair.FairScheduler - Application app_123 submitted to queue 'root.production' but no available resources\n"
                "[10:05:00] WARN  org.apache.hadoop.yarn.server.resourcemanager.scheduler.fair.FairScheduler - Max capacity 90% reached for queue 'root.production'. Request from app_124 denied\n"
                "[10:10:00] INFO  org.apache.hadoop.yarn.server.resourcemanager.ResourceManager - Cluster resources: 512 GB total, 512 GB used (100%)\n"
                "[10:11:00] INFO  org.apache.hadoop.yarn.server.resourcemanager.ResourceManager - Cluster resources: 128 vcores total, 128 vcores used (100%)\n"
            ),
            "nodemanager-001.log": yarn_logs(),
        },
        "config": {
            "yarn-site.xml": {
                "yarn.scheduler.minimum-allocation-mb": 1024,
                "yarn.scheduler.maximum-allocation-mb": 8192,
                "yarn.nodemanager.resource.memory-mb": 32768,
                "yarn.nodemanager.resource.cpu-vcores": 16,
            },
            "fair-scheduler.xml": {
                "queue.root.production.minResources": "100 GB, 20 vcores",
                "queue.root.production.maxResources": "450 GB, 100 vcores",
                "queue.root.production.maxCapacity": 90,
                "queue.root.development.maxCapacity": 10,
            },
        },
        "metrics": {
            "cluster_memory_used_gb": 512,
            "cluster_memory_total_gb": 512,
            "cluster_vcores_used": 128,
            "cluster_vcores_total": 128,
            "pending_apps": 5,
            "queue_production_used_pct": 100,
        },
        "expected_root_cause": "YARN production queue at 100% capacity",
    }

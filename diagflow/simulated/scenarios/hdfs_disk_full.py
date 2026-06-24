"""HDFS disk full scenario — DataNode volume out of space."""

from ..log_generator import hdfs_logs


def hdfs_disk_full_scenario():
    """HDFS DataNode disk full, blocks can't be replicated."""
    return {
        "context": {
            "cluster_id": "c-uhadoop-003",
            "region": "广州",
            "component": "hdfs",
            "version": "3.3.4",
            "problem": "disk_full",
            "problem_desc": "HDFS 写入超时，部分文件无法创建",
            "detail": "用户反馈 HDFS 写入报错 'No space left on device'，部分 DataNode 节点磁盘已满",
        },
        "logs": {
            "datanode-003.log": hdfs_logs(fault="disk_full"),
        },
        "config": {
            "hdfs-site.xml": {
                "dfs.datanode.data.dir": "/data/hdfs/data1,/data/hdfs/data2,/data/hdfs/data3",
                "dfs.replication": 3,
                "dfs.datanode.du.reserved": "10g",
            },
        },
        "metrics": {
            "data1_usage_pct": 95,
            "data2_usage_pct": 97,
            "data3_usage_pct": 100,
            "block_repair_failures": 12,
        },
        "expected_root_cause": "DataNode disk full on volume /data/hdfs/data3",
    }

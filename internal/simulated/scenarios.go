// Package simulated provides the four pre-built fault scenarios.
package simulated

// Scenarios maps scenario name → scenario factory.
var Scenarios = map[string]func() *Scenario{
	"flink_oom":             flinkOOM,
	"flink_checkpoint_fail": flinkCheckpointFail,
	"hdfs_disk_full":        hdfsDiskFull,
	"yarn_queue_stuck":      yarnQueueStuck,
}

// NewCluster builds a simulated cluster for a scenario name.
func NewCluster(name string) (*Cluster, error) {
	fn, ok := Scenarios[name]
	if !ok {
		var names []string
		for k := range Scenarios {
			names = append(names, k)
		}
		return nil, &UnknownScenarioError{Name: name, Available: names}
	}
	return &Cluster{Scenario: fn()}, nil
}

// UnknownScenarioError is returned for an unknown scenario name.
type UnknownScenarioError struct {
	Name      string
	Available []string
}

func (e *UnknownScenarioError) Error() string {
	return "unknown scenario '" + e.Name + "'"
}

func flinkOOM() *Scenario {
	return &Scenario{
		Context: map[string]any{
			"cluster_id":   "c-uhadoop-001",
			"region":       "北京二",
			"component":    "flink",
			"version":      "1.14.3",
			"job_id":       "job_xxx",
			"problem":      "job_failure",
			"problem_desc": "任务挂掉，状态 FAILED",
			"detail":       "用户反馈 Flink 任务突然停止，Flink Dashboard 显示状态为 FAILED，最后一次 Checkpoint 失败",
		},
		Logs: map[string]string{
			"taskmanager-1.log": taskmanagerLogs("oom"),
			"jobmanager.log":    jobmanagerLogs(),
		},
		Config: map[string]map[string]any{
			"flink-conf.yaml": {
				"jobmanager.memory.heap.size":     "1024m",
				"taskmanager.memory.heap.size":    "2048m",
				"taskmanager.memory.process.size": "4096m",
				"taskmanager.numberOfTaskSlots":   4,
				"parallelism.default":             4,
				"state.backend":                   "rocksdb",
				"state.checkpoints.dir":           "hdfs:///user/flink/checkpoints",
				"rest.port":                       8081,
			},
		},
		Metrics: map[string]any{
			"heap_usage_percent":      95.5,
			"checkpoint_failure_rate": 3,
			"gc_pause_ms_avg":         850,
			"input_rate_mbps":         45.0,
			"backpressure_level":      "HIGH",
		},
		ExpectedRootCause:  "TaskManager Java heap space OutOfMemoryError",
		ExpectedConfidence: "high",
		ScenarioName:       "flink_oom",
	}
}

func flinkCheckpointFail() *Scenario {
	return &Scenario{
		Context: map[string]any{
			"cluster_id":   "c-uhadoop-002",
			"region":       "上海",
			"component":    "flink",
			"version":      "1.16.0",
			"job_id":       "job_abc",
			"problem":      "checkpoint_failure",
			"problem_desc": "Checkpoint 持续失败，任务最终自动重启",
			"detail":       "Checkpoint 连续失败但无 OOM，任务频繁重启",
		},
		Logs: map[string]string{
			"taskmanager-1.log": taskmanagerLogs("checkpoint_fail"),
			"jobmanager.log":    "[12:00:00] INFO  JobManager started\n[12:05:00] ERROR Checkpoint 1805 FAILED: expired\n",
		},
		Config: map[string]map[string]any{
			"flink-conf.yaml": {
				"taskmanager.memory.heap.size":                         "4096m",
				"parallelism.default":                                  8,
				"execution.checkpointing.interval":                     "600000ms",
				"execution.checkpointing.timeout":                      "600000ms",
				"execution.checkpointing.tolerable-failed-checkpoints": 3,
			},
		},
		Metrics: map[string]any{
			"checkpoint_failure_rate": 5,
			"input_rate_mbps":         120.0,
			"backpressure_level":      "HIGH",
		},
		ExpectedRootCause: "Checkpoint alignment timeout due to backpressure",
		ScenarioName:      "flink_checkpoint_fail",
	}
}

func hdfsDiskFull() *Scenario {
	return &Scenario{
		Context: map[string]any{
			"cluster_id":   "c-uhadoop-003",
			"region":       "广州",
			"component":    "hdfs",
			"version":      "3.3.4",
			"problem":      "disk_full",
			"problem_desc": "HDFS 写入超时，部分文件无法创建",
			"detail":       "用户反馈 HDFS 写入报错 'No space left on device'，部分 DataNode 节点磁盘已满",
		},
		Logs: map[string]string{
			"datanode-003.log": hdfsLogs(),
		},
		Config: map[string]map[string]any{
			"hdfs-site.xml": {
				"dfs.datanode.data.dir":    "/data/hdfs/data1,/data/hdfs/data2,/data/hdfs/data3",
				"dfs.replication":          3,
				"dfs.datanode.du.reserved": "10g",
			},
		},
		Metrics: map[string]any{
			"data1_usage_pct":       95,
			"data2_usage_pct":       97,
			"data3_usage_pct":       100,
			"block_repair_failures": 12,
		},
		ExpectedRootCause: "DataNode disk full on volume /data/hdfs/data3",
		ScenarioName:      "hdfs_disk_full",
	}
}

func yarnQueueStuck() *Scenario {
	return &Scenario{
		Context: map[string]any{
			"cluster_id":   "c-uhadoop-004",
			"region":       "北京二",
			"component":    "yarn",
			"version":      "3.3.4",
			"problem":      "queue_stuck",
			"problem_desc": "任务提交后一直处于 ACCEPTED 状态，无法分配到容器",
			"detail":       "用户提交 Spark 任务后，YARN Application 一直显示 ACCEPTED，无法获取容器资源",
		},
		Logs: map[string]string{
			"resourcemanager.log": yarnRMLogs(),
			"nodemanager-001.log": "Container allocated 4096 MB memory, 2 vcores.\n",
		},
		Config: map[string]map[string]any{
			"yarn-site.xml": {
				"yarn.scheduler.minimum-allocation-mb": 1024,
				"yarn.scheduler.maximum-allocation-mb": 8192,
				"yarn.nodemanager.resource.memory-mb":  32768,
				"yarn.nodemanager.resource.cpu-vcores": 16,
			},
		},
		Metrics: map[string]any{
			"cluster_memory_used_gb":    512,
			"cluster_memory_total_gb":   512,
			"cluster_vcores_used":       128,
			"cluster_vcores_total":      128,
			"pending_apps":              5,
			"queue_production_used_pct": 100,
		},
		ExpectedRootCause: "YARN production queue at 100% capacity",
		ScenarioName:      "yarn_queue_stuck",
	}
}

func hdfsLogs() string {
	return "[10:00:00] INFO  DataNode started on node-003\n" +
		"[10:33:00] ERROR DataNode - Volume /data/hdfs/data3 is out of space: disk full\n" +
		"[10:34:00] ERROR DataNode - Too many failed writes on volume /data/hdfs/data3. Volume will be marked as FAILED\n"
}

func yarnRMLogs() string {
	return "[10:00:00] INFO  ResourceManager - RM started\n" +
		"[10:01:00] WARN  FairScheduler - Queue 'root.production' is at 100% capacity\n" +
		"[10:02:00] WARN  FairScheduler - Application app_123 submitted to queue 'root.production' but no available resources\n" +
		"[10:05:00] WARN  FairScheduler - Max capacity 90% reached for queue 'root.production'. Request from app_124 denied\n"
}

func taskmanagerLogs(fault string) string {
	if fault == "checkpoint_fail" {
		return "[14:20:00] WARN  CheckpointCoordinator - Checkpoint 1800 alignment took 32000ms (threshold 30000ms)\n" +
			"[14:21:00] ERROR CheckpointCoordinator - Checkpoint 1805 FAILED: Checkpoint expired before completing\n" +
			"[14:22:00] ERROR CheckpointCoordinator - Checkpoint 1806 FAILED: Barrier not received from subtask 3 within 600000ms\n"
	}
	return "[14:19:00] WARN  TaskExecutor - GC pause 2100ms in TaskManager 1 — potential STW event\n" +
		"[14:20:00] ERROR TaskExecutor - Fatal error: java.lang.OutOfMemoryError: Java heap space\n" +
		"[14:20:01] ERROR TaskExecutor -   at java.util.Arrays.copyOf(Arrays.java:3332)\n" +
		"[14:20:02] ERROR TaskExecutor - java.lang.OutOfMemoryError: GC overhead limit exceeded\n" +
		"[14:20:03] ERROR TaskExecutor - TaskManager 1 process exited with code 137 (OOM killed by OS)\n"
}

func jobmanagerLogs() string {
	return "[12:00:00] INFO  JobManagerRunner - JobManager started\n" +
		"[12:00:01] INFO  JobManagerRunner - Starting job job_xxx\n"
}

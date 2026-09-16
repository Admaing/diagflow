package mcpserver

// toolDefs returns the MCP tool catalogue. All tools are read-only.
func toolDefs() []map[string]any {
	schema := func(props map[string]any, required []string) map[string]any {
		return map[string]any{"type": "object", "properties": props, "required": required}
	}
	str := func(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
	num := func(desc string) map[string]any { return map[string]any{"type": "integer", "description": desc} }
	clusterID := str("UHadoop instance ID, e.g. 'uhadoop-1v9waqiylmqj'")
	nodeRefDesc := "Node reference: node_name ('...-core1'), node_id, node_ip, or role ('master1', 'core2', 'master', 'core')"

	return []map[string]any{
		{
			"name": "get_cluster_info",
			"description": "Get a UHadoop cluster's metadata: framework/version, state, capacity " +
				"(master/core/task counts), installed applications, and the full node list. " +
				"Start here to learn a cluster's shape before reading logs.",
			"inputSchema": schema(map[string]any{"cluster_id": clusterID}, []string{"cluster_id"}),
		},
		{
			"name":        "list_nodes",
			"description": "List a cluster's nodes, optionally filtered by role and/or node state.",
			"inputSchema": schema(map[string]any{
				"cluster_id": clusterID,
				"role":       str("Filter by role/type: master, core, task, client (optional)"),
				"state":      num("Filter by numeric NodeState, e.g. 1=NS_RUNNING (optional)"),
			}, []string{"cluster_id"}),
		},
		{
			"name": "get_node_info",
			"description": "Get one node's details including the services running on it " +
				"(NameNode, ResourceManager, Hive-Metastore, ...).",
			"inputSchema": schema(map[string]any{
				"cluster_id": clusterID,
				"node_ref":   str(nodeRefDesc),
			}, []string{"cluster_id", "node_ref"}),
		},
		{
			"name":        "get_cluster_services",
			"description": "List installed applications and, for each, the per-node service instances.",
			"inputSchema": schema(map[string]any{"cluster_id": clusterID}, []string{"cluster_id"}),
		},
		{
			"name": "get_node_log",
			"description": "Read the tail of a log file on one node, optionally filtered by keywords. " +
				"Read-only over SSH; output is bounded.",
			"inputSchema": schema(map[string]any{
				"cluster_id": clusterID,
				"node_ref":   str(nodeRefDesc),
				"log_path": str("Absolute log path on the node. A glob is supported and is usually " +
					"the only practical way to name a file, because names embed the instance id: " +
					"'/var/log/hadoop-hdfs/hadoop-hadoop-namenode-*.log' or '/var/log/hadoop-yarn/*.log'"),
				"keywords":  str("Extended-regex keyword filter, e.g. 'ERROR|Exception|FATAL' (optional)"),
				"max_lines": num("Max lines to return (default 200, cap 2000)"),
			}, []string{"cluster_id", "log_path"}),
		},
		{
			"name": "list_logs",
			"description": "List the log files on a node so you can discover exact paths before reading. " +
				"Without 'dir' it probes the standard UHadoop log roots (hdfs, yarn, hive, spark, ...).",
			"inputSchema": schema(map[string]any{
				"cluster_id": clusterID,
				"node_ref":   str(nodeRefDesc + " (optional; defaults to the first master)"),
				"dir":        str("A directory under /var/log to list (optional), e.g. '/var/log/hadoop-yarn'"),
			}, []string{"cluster_id"}),
		},
		{
			"name": "search_logs",
			"description": "Grep the same log path across multiple nodes in parallel — the fast way " +
				"to find which node hit an error. Defaults to all nodes.",
			"inputSchema": schema(map[string]any{
				"cluster_id": clusterID,
				"log_path": str("Absolute log path present on the target nodes. A glob is supported, " +
					"e.g. '/var/log/hadoop-hdfs/*.log'"),
				"keywords":  str("Extended-regex keyword filter (required), e.g. 'ERROR|Exception'"),
				"nodes":     str("Comma-separated node refs to search (optional; overrides roles)"),
				"roles":     str("Comma-separated roles to search, e.g. 'master,core' (optional)"),
				"max_lines": num("Max lines per node (default 200, cap 2000)"),
			}, []string{"cluster_id", "log_path", "keywords"}),
		},
		{
			"name":        "get_node_metrics",
			"description": "Resource snapshot for a node: load average, memory, and disk usage (read-only via SSH).",
			"inputSchema": schema(map[string]any{
				"cluster_id": clusterID,
				"node_ref":   str(nodeRefDesc + " (optional; defaults to the first master)"),
			}, []string{"cluster_id"}),
		},
		{
			"name":        "get_base_info",
			"description": "Base info for a node: hostname, kernel, OS release, CPU count, memory (read-only via SSH).",
			"inputSchema": schema(map[string]any{
				"cluster_id": clusterID,
				"node_ref":   str(nodeRefDesc + " (optional; defaults to the first master)"),
			}, []string{"cluster_id"}),
		},
		{
			"name":        "check_process",
			"description": "Check whether a process matching a name is running on a node (read-only via SSH).",
			"inputSchema": schema(map[string]any{
				"cluster_id":   clusterID,
				"process_name": str("Process name or pattern, e.g. 'NameNode'"),
				"node_ref":     str(nodeRefDesc + " (optional; defaults to the first master)"),
			}, []string{"cluster_id", "process_name"}),
		},
		{
			"name": "query_yarn",
			"description": "Query YARN applications from a cluster node (read-only via SSH). " +
				"action='list_apps' lists applications; action='app' returns one application's status.",
			"inputSchema": schema(map[string]any{
				"cluster_id": clusterID,
				"action":     str("'list_apps' (default) or 'app'"),
				"app_id":     str("YARN application id (for action='app')"),
				"states":     str("Comma-separated states for list_apps: RUNNING,FAILED,KILLED,FINISHED"),
				"node_ref":   str(nodeRefDesc + " (optional; defaults to the first master)"),
			}, []string{"cluster_id"}),
		},
		{
			"name": "ssh_exec",
			"description": "Run one whitelisted, read-only shell command on a cluster node. " +
				"Accepted heads include cat, grep, tail, head, ps, df, free, du, ls, ss, jps, " +
				"yarn, hdfs and mysql. Most useful for HDFS health: 'hdfs dfsadmin -report', " +
				"'hdfs dfsadmin -safemode get', 'hdfs haadmin -getAllServiceState', 'hdfs fsck /'. " +
				"Pipes and '2>&1' are allowed, so 'hdfs dfsadmin -report 2>&1 | head -40' works. " +
				"mysql is accepted for inline read-only queries only — pass the SQL with -e and " +
				"keep it to a single SELECT/SHOW/DESC/EXPLAIN statement, e.g. " +
				"'mysql -h 127.0.0.1 -e \"SHOW DATABASES\"'. Writes, DDL, multi-statement input, " +
				"SQL comments and client flags such as --init-command are refused. " +
				"Commands run with HADOOP_HOME and PATH already set: call 'hdfs ...' directly, " +
				"there is no need to source a profile first. Writes and mutations are refused.",
			"inputSchema": schema(map[string]any{
				"cluster_id": clusterID,
				"command":    str("Read-only command, e.g. 'jps -l' or 'ss -lntp'"),
				"node_ref":   str(nodeRefDesc + " (optional; defaults to the first master)"),
			}, []string{"cluster_id", "command"}),
		},
	}
}

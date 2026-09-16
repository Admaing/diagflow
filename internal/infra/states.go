package infra

import "fmt"

// UHadoop state enums (from go_framework/pb/uhadoop.*.proto). The HTTP API
// returns raw integers; translating them here keeps callers (and LLM agents)
// from guessing what "cluster_state": 2 means.

var clusterStateNames = map[int]string{
	1:  "CS_NEW",
	2:  "CS_RUNNING",
	3:  "CS_DEPLOY",
	4:  "CS_STOP",
	5:  "CS_CREATE_FAILED",
	6:  "CS_DELETE",
	7:  "CS_SERVICE_EXCEPTION",
	8:  "CS_INIT",
	9:  "CS_UPDATE",
	10: "CS_DELETE_FAILED",
	11: "CS_UNAVAILABLE",
	12: "CS_DELETING",
	13: "CS_CREATING",
	14: "CS_EXPAND_FAILED",
	15: "CS_NARROW_FAILED",
	16: "CS_CLUSTER_EXCEPTION",
	17: "CS_MIGRATING",
	18: "CS_MIGRATE_FAILED",
	19: "CS_STOP_EXCEPTION",
}

var nodeStateNames = map[int]string{
	1:  "NS_RUNNING",
	2:  "NS_DELETE",
	3:  "NS_DEPLOY",
	4:  "NS_CREATE_FAILED",
	5:  "NS_STARTING",
	6:  "NS_OFFLINE",
	7:  "NS_EXCEPTION",
	8:  "NS_STOP",
	9:  "NS_PREP",
	10: "NS_INIT",
	11: "NS_DELETE_FAILED",
	12: "NS_UNAVAILABLE",
	13: "NS_DELETING",
	14: "NS_MODIFING",
}

var appStateNames = map[int]string{
	1: "AS_RUNNING",
	2: "AS_STOPPED",
	3: "AS_STARTING",
	4: "AS_STOPPING",
	5: "AS_START_FAILED",
	6: "AS_STOP_FAILED",
	7: "AS_INSTALLING",
	8: "AS_INSTALL_FAILED",
	9: "AS_NOT_INSTALLED",
}

func stateName(m map[int]string, v int) string {
	if name, ok := m[v]; ok {
		return name
	}
	return fmt.Sprintf("UNKNOWN(%d)", v)
}

// ClusterStateName maps a ClusterState enum to its symbolic name.
func ClusterStateName(v int) string { return stateName(clusterStateNames, v) }

// NodeStateName maps a NodeState enum to its symbolic name.
func NodeStateName(v int) string { return stateName(nodeStateNames, v) }

// AppStateName maps an ApplicationState enum to its symbolic name.
func AppStateName(v int) string { return stateName(appStateNames, v) }

// clusterReadyStates are the states in which nodes are reachable and node
// logs are worth reading. Everything else is still being created, stopped or
// torn down, so log reads should degrade with an explicit message.
var clusterReadyStates = map[int]bool{
	2:  true, // CS_RUNNING
	7:  true, // CS_SERVICE_EXCEPTION — running, but a service is unhealthy
	16: true, // CS_CLUSTER_EXCEPTION — running, cluster-level exception
}

// ClusterReady reports whether a cluster in this state can serve node logs.
func ClusterReady(state int) bool { return clusterReadyStates[state] }

// ClusterStateHint returns a human/LLM-friendly explanation for a cluster
// state, including whether node operations are expected to work.
func ClusterStateHint(state int) string {
	name := ClusterStateName(state)
	if ClusterReady(state) {
		return name + " (running; node operations available)"
	}
	switch state {
	case 1, 3, 8, 13: // CS_NEW, CS_DEPLOY, CS_INIT, CS_CREATING
		return name + " (still being created; node logs are not available yet)"
	case 4, 6, 12: // CS_STOP, CS_DELETE, CS_DELETING
		return name + " (stopped or being removed; nodes are unreachable)"
	case 5, 10, 14, 15, 18: // *_FAILED
		return name + " (last operation failed)"
	}
	return name
}

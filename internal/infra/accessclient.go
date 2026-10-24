package infra

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// AccessClient talks to the uhadoop-access HTTP action API
// (ZK service UHADOOPAccessHTTP). Actions are lowercase snake_case and are
// dispatched by the server's HandlerMap; requests are POST / with
// {"action": "...", <proto fields>}.
type AccessClient struct {
	BaseURL string
	Timeout time.Duration
	client  *http.Client
}

// NewAccessClient builds a client for an access endpoint base URL.
func NewAccessClient(baseURL string) *AccessClient {
	return &AccessClient{BaseURL: baseURL, Timeout: 15 * time.Second}
}

// FlexInt unmarshals a proto enum that may arrive as a number or a string.
type FlexInt int

func (f *FlexInt) UnmarshalJSON(b []byte) error {
	s := string(bytes.Trim(bytes.TrimSpace(b), `"`))
	if s == "" || s == "null" {
		*f = 0
		return nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return fmt.Errorf("FlexInt: %w", err)
	}
	*f = FlexInt(n)
	return nil
}

func (f FlexInt) Int() int { return int(f) }

// FlexStrings accepts the two shapes the UHadoop access API returns for
// service_name: a single string or an array of strings. The API is not
// consistent between describe_cluster and list-node responses.
type FlexStrings []string

func (f *FlexStrings) UnmarshalJSON(b []byte) error {
	trimmed := bytes.TrimSpace(b)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		*f = nil
		return nil
	}
	if trimmed[0] == '"' {
		var one string
		if err := json.Unmarshal(trimmed, &one); err != nil {
			return fmt.Errorf("FlexStrings: %w", err)
		}
		if one == "" {
			*f = nil
		} else {
			*f = FlexStrings{one}
		}
		return nil
	}
	var many []string
	if err := json.Unmarshal(trimmed, &many); err != nil {
		return fmt.Errorf("FlexStrings: %w", err)
	}
	*f = FlexStrings(many)
	return nil
}

// NodeInfo mirrors the proto NodeInfo message (read-only subset).
type NodeInfo struct {
	NodeType    string      `json:"node_type"` // master | core | task | client
	NodeID      string      `json:"node_id"`
	UHostID     string      `json:"uhost_id"`
	NodeName    string      `json:"node_name"`
	NodeIP      string      `json:"node_ip"`
	NodeIPv6    string      `json:"node_ipv6"`
	AccessUser  string      `json:"access_user"`
	Passwd      string      `json:"passwd"` // base64; never exposed to callers
	UHostType   string      `json:"uhost_type"`
	NodeState   FlexInt     `json:"node_state"`
	ServiceName FlexStrings `json:"service_name"`
	InstanceID  string      `json:"instance_id"`
	AgentKey    string      `json:"agent_key"`
	ZoneID      FlexInt     `json:"zone_id"`
	UHostName   string      `json:"uhost_name"`
	Remark      string      `json:"remark"`
	Scaling     bool        `json:"scaling"`
}

// ClusterApp is one application installed on a cluster.
type ClusterApp struct {
	AppName    string  `json:"app_name"`
	AppVersion string  `json:"app_version"`
	AppStatus  FlexInt `json:"app_status"`
}

// ClusterInfo mirrors the proto ClusterInfo message (read-only subset).
type ClusterInfo struct {
	InstanceID       string       `json:"instance_id"`
	InstanceName     string       `json:"instance_name"`
	MasterNum        FlexInt      `json:"master_num"`
	CoreNum          FlexInt      `json:"core_num"`
	TaskNum          FlexInt      `json:"task_num"`
	ClientNum        FlexInt      `json:"client_num"`
	Framework        string       `json:"framework"`
	FrameworkVersion string       `json:"framework_version"`
	App              []ClusterApp `json:"app"`
	NodeInfo         []NodeInfo   `json:"node_info"`
	CreateTime       int64        `json:"create_time"`
	UpdateTime       int64        `json:"update_time"`
	RunningTime      int64        `json:"running_time"`
	ClusterState     FlexInt      `json:"cluster_state"`
	Remark           string       `json:"remark"`
	NetworkType      string       `json:"network_type"`
	ChargeType       FlexInt      `json:"charge_type"`
}

// NodeApp is one application plus its per-node service instances.
type NodeApp struct {
	AppName     string        `json:"app_name"`
	AppVersion  string        `json:"app_version"`
	AppStatus   FlexInt       `json:"app_status"`
	NodeService []NodeService `json:"node_service"`
}

// NodeService is an application instance running on one node.
type NodeService struct {
	NodeID      string      `json:"node_id"`
	NodeIP      string      `json:"node_ip"`
	ServiceName FlexStrings `json:"service_name"`
}

// Call invokes an action and returns its Data object.
func (c *AccessClient) Call(ctx context.Context, action string, in map[string]any) (map[string]any, error) {
	payload := map[string]any{"action": action}
	for k, v := range in {
		payload[k] = v
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	client := c.client
	if client == nil {
		timeout := c.Timeout
		if timeout <= 0 {
			timeout = 15 * time.Second
		}
		client = &http.Client{Timeout: timeout}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("uhadoop-access HTTP %d: %s", resp.StatusCode, truncate(string(raw), 300))
	}

	// The API answers either an object or a single-element array.
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		var arr []map[string]any
		if err2 := json.Unmarshal(raw, &arr); err2 == nil && len(arr) > 0 {
			out = arr[0]
		} else {
			return nil, fmt.Errorf("uhadoop-access: decode response: %w (body=%.300s)", err, raw)
		}
	}
	if rc, ok := out["RetCode"]; ok && toInt(rc) != 0 {
		msg, _ := out["Error"].(string)
		return nil, fmt.Errorf("uhadoop-access %s failed: RetCode=%v %s", action, rc, msg)
	}
	data, _ := out["Data"].(map[string]any)
	if data == nil {
		return map[string]any{}, nil
	}
	return data, nil
}

func toInt(v any) int {
	switch t := v.(type) {
	case float64:
		return int(t)
	case json.Number:
		n, _ := t.Int64()
		return int(n)
	case string:
		n, _ := strconv.Atoi(t)
		return n
	case int:
		return t
	}
	return 0
}

// DescribeCluster returns full cluster metadata including nodes.
func (c *AccessClient) DescribeCluster(ctx context.Context, instanceID string) (*ClusterInfo, error) {
	data, err := c.Call(ctx, "describe_cluster", map[string]any{"instance_id": instanceID})
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(data["cluster_info"])
	if err != nil {
		return nil, err
	}
	var info ClusterInfo
	if err := json.Unmarshal(raw, &info); err != nil {
		return nil, fmt.Errorf("describe_cluster: decode cluster_info: %w", err)
	}
	return &info, nil
}

// ListNode returns the cluster's nodes, optionally filtered by state.
func (c *AccessClient) ListNode(ctx context.Context, instanceID string, nodeState int) ([]NodeInfo, error) {
	in := map[string]any{"instance_id": instanceID}
	if nodeState > 0 {
		in["node_state"] = nodeState
	}
	data, err := c.Call(ctx, "list_node", in)
	if err != nil {
		return nil, err
	}
	return decodeNodes(data["node_info"])
}

// DescribeNode returns one node by node id.
func (c *AccessClient) DescribeNode(ctx context.Context, nodeID string) (*NodeInfo, error) {
	data, err := c.Call(ctx, "describe_node", map[string]any{"node_id": nodeID})
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(data["node_info"])
	if err != nil {
		return nil, err
	}
	var node NodeInfo
	if err := json.Unmarshal(raw, &node); err != nil {
		return nil, fmt.Errorf("describe_node: decode node_info: %w", err)
	}
	return &node, nil
}

// ListClusterApps returns the applications installed on a cluster.
func (c *AccessClient) ListClusterApps(ctx context.Context, instanceID string) ([]NodeApp, error) {
	data, err := c.Call(ctx, "get_cluster_service", map[string]any{"instance_id": instanceID})
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(data["node_app"])
	if err != nil {
		return nil, err
	}
	var apps []NodeApp
	if err := json.Unmarshal(raw, &apps); err != nil {
		return nil, fmt.Errorf("get_cluster_service: decode node_app: %w", err)
	}
	return apps, nil
}

func decodeNodes(v any) ([]NodeInfo, error) {
	if v == nil {
		return nil, nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var nodes []NodeInfo
	if err := json.Unmarshal(raw, &nodes); err != nil {
		return nil, fmt.Errorf("decode node_info: %w", err)
	}
	return nodes, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

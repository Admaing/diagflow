package infra

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// The access API has returned service_name as both a string and an array in
// different responses. Both shapes must decode without failing the whole
// cluster-services query.
func TestFlexStringsAcceptsStringAndArray(t *testing.T) {
	var info ClusterInfo
	raw := `{
		"node_info": [
			{"node_name":"master1","service_name":"NameNode"},
			{"node_name":"core1","service_name":["DataNode","NodeManager"]}
		]
	}`
	if err := json.Unmarshal([]byte(raw), &info); err != nil {
		t.Fatalf("decode cluster info: %v", err)
	}
	if got := []string(info.NodeInfo[0].ServiceName); !reflect.DeepEqual(got, []string{"NameNode"}) {
		t.Fatalf("string service_name = %v", got)
	}
	if got := []string(info.NodeInfo[1].ServiceName); !reflect.DeepEqual(got, []string{"DataNode", "NodeManager"}) {
		t.Fatalf("array service_name = %v", got)
	}
}

func TestFlexStringsInNodeService(t *testing.T) {
	var app NodeApp
	raw := `{"node_service":[{"node_id":"n1","service_name":"ResourceManager"}]}`
	if err := json.Unmarshal([]byte(raw), &app); err != nil {
		t.Fatalf("decode node app: %v", err)
	}
	if len(app.NodeService) != 1 || len(app.NodeService[0].ServiceName) != 1 ||
		app.NodeService[0].ServiceName[0] != "ResourceManager" {
		t.Fatalf("unexpected node service: %+v", app.NodeService)
	}
}

func TestFlexStringsNullAndEmpty(t *testing.T) {
	for _, raw := range []string{`null`, `""`, `[]`} {
		var got FlexStrings
		if err := json.Unmarshal([]byte(raw), &got); err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		if len(got) != 0 {
			t.Fatalf("%s decoded to %v", raw, got)
		}
	}
}

// A numeric or object service_name used to fall through to the array branch
// and fail with an opaque encoding/json message.
func TestFlexStringsRejectsOtherShapes(t *testing.T) {
	for _, raw := range []string{`123`, `{"a":1}`} {
		var got FlexStrings
		err := json.Unmarshal([]byte(raw), &got)
		if err == nil {
			t.Fatalf("%s decoded to %v", raw, got)
		}
		if !strings.Contains(err.Error(), "expected string or array") {
			t.Fatalf("%s: unexpected error %v", raw, err)
		}
	}
}

// NodeInfo and NodeService both carry service_name. A string-shaped value in
// either must not fail the decode, which is the 2026-10-28 get_cluster_services
// failure: the access API returned a bare string for a node_app entry.
func TestFlexStringsEveryServiceNameField(t *testing.T) {
	var info ClusterInfo
	infoRaw := `{"node_info":[{"node_name":"master1","service_name":"NameNode"}]}`
	if err := json.Unmarshal([]byte(infoRaw), &info); err != nil {
		t.Fatalf("decode cluster info: %v", err)
	}
	if got := []string(info.NodeInfo[0].ServiceName); !reflect.DeepEqual(got, []string{"NameNode"}) {
		t.Fatalf("node_info service_name = %v", got)
	}

	// Same shape ListClusterApps unmarshals: data["node_app"] re-marshalled.
	appsRaw := `[{"app_name":"YARN","node_service":[
		{"node_id":"n1","service_name":"ResourceManager"},
		{"node_id":"n2","service_name":["NodeManager","JournalNode"]},
		{"node_id":"n3","service_name":null}]}]`
	var apps []NodeApp
	if err := json.Unmarshal([]byte(appsRaw), &apps); err != nil {
		t.Fatalf("decode node_app: %v", err)
	}
	want := [][]string{{"ResourceManager"}, {"NodeManager", "JournalNode"}, nil}
	for i, service := range apps[0].NodeService {
		if got := []string(service.ServiceName); !reflect.DeepEqual(got, want[i]) {
			t.Fatalf("node_service[%d] = %v, want %v", i, got, want[i])
		}
	}

	// Re-encoding must produce arrays so downstream tools see one shape.
	out, err := json.Marshal(apps[0].NodeService[0])
	if err != nil {
		t.Fatalf("encode node service: %v", err)
	}
	if !strings.Contains(string(out), `"service_name":["ResourceManager"]`) {
		t.Fatalf("encoded node service = %s", out)
	}
}

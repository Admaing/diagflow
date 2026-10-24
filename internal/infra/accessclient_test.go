package infra

import (
	"encoding/json"
	"reflect"
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

package infra

import "testing"

func testNodes() []NodeInfo {
	return []NodeInfo{
		{NodeID: "node-1", NodeName: "c-master1", NodeType: "master", NodeIP: "10.0.0.1"},
		{NodeID: "node-2", NodeName: "c-master2", NodeType: "master", NodeIP: "10.0.0.2"},
		{NodeID: "node-3", NodeName: "c-core1", NodeType: "core", NodeIP: "10.0.0.3"},
		{NodeID: "node-4", NodeName: "c-core2", NodeType: "core", NodeIP: "10.0.0.4"},
	}
}

func TestFindNode(t *testing.T) {
	nodes := testNodes()
	cases := map[string]string{
		"c-master1": "node-1", // exact name
		"node-2":    "node-2", // node id
		"10.0.0.3":  "node-3", // ip
		"master":    "node-1", // first master
		"master1":   "node-1",
		"master2":   "node-2",
		"core1":     "node-3",
		"core2":     "node-4",
		"core":      "node-3", // first core
		"":          "node-1", // default = first master
	}
	for ref, want := range cases {
		got, err := FindNode(nodes, ref)
		if err != nil {
			t.Errorf("FindNode(%q): %v", ref, err)
			continue
		}
		if got.NodeID != want {
			t.Errorf("FindNode(%q) = %s, want %s", ref, got.NodeID, want)
		}
	}
	if _, err := FindNode(nodes, "nonexistent"); err == nil {
		t.Fatal("expected error for unknown node")
	}
	if _, err := FindNode(nil, "x"); err == nil {
		t.Fatal("expected error for empty node list")
	}
}

func TestSplitRoleIndex(t *testing.T) {
	cases := []struct {
		in    string
		role  string
		index int
	}{
		{"core3", "core", 3},
		{"master", "master", 0},
		{"task12", "task", 12},
		{"", "", 0},
	}
	for _, c := range cases {
		role, idx := splitRoleIndex(c.in)
		if role != c.role || idx != c.index {
			t.Errorf("splitRoleIndex(%q) = (%q,%d), want (%q,%d)", c.in, role, idx, c.role, c.index)
		}
	}
}

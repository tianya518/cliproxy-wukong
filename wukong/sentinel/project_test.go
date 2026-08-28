package sentinel

import (
	"testing"
)

func TestNormalizeGizmoID(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"g-p-abc", "g-p-abc"},
		{"  g-p-abc  ", "g-p-abc"},
		{"6a8e8a1819448191a8b160a75c670bf6", "g-p-6a8e8a1819448191a8b160a75c670bf6"},
		{"https://chatgpt.com/g/g-p-xyz/project", "g-p-xyz"},
		{"g-custom-1", "g-custom-1"},
	}
	for _, tc := range cases {
		if got := NormalizeGizmoID(tc.in); got != tc.want {
			t.Fatalf("NormalizeGizmoID(%q)=%q want %q", tc.in, got, tc.want)
		}
	}
}

func TestParseSidebarProjects(t *testing.T) {
	raw := []byte(`{
	  "items": [
	    {"gizmo":{"gizmo":{"id":"g-p-aaa","short_url":"g-p-aaa-demo","instructions":"be brief","display":{"name":"Demo","description":"d"}}}},
	    {"gizmo":{"gizmo":{"id":"","display":{"name":"skip"}}}}
	  ],
	  "cursor": "NEXT"
	}`)
	items, cursor, err := parseSidebarProjects(raw)
	if err != nil {
		t.Fatal(err)
	}
	if cursor != "NEXT" || len(items) != 1 {
		t.Fatalf("n=%d cursor=%q", len(items), cursor)
	}
	if items[0].ID != "g-p-aaa" || items[0].Name != "Demo" || items[0].Instructions != "be brief" {
		t.Fatalf("%+v", items[0])
	}
}

func TestParseCreateProject(t *testing.T) {
	raw := []byte(`{
	  "resource":{"gizmo":{"id":"g-p-new","short_url":"g-p-new-wukong","display":{"name":"wukong-probe"}}},
	  "error": null,
	  "sharing_targets": []
	}`)
	p, err := parseCreateProject(raw)
	if err != nil {
		t.Fatal(err)
	}
	if p.ID != "g-p-new" || p.Name != "wukong-probe" {
		t.Fatalf("%+v", p)
	}
}

func TestParseCreateProjectError(t *testing.T) {
	_, err := parseCreateProject([]byte(`{"resource":{},"error":{"message":"quota"}}`))
	if err == nil || err.Error() != "create project: quota" {
		t.Fatalf("err=%v", err)
	}
}

func TestParseGizmoProject(t *testing.T) {
	p, err := parseGizmoProject([]byte(`{"gizmo":{"id":"g-p-1","display":{"name":"k8s"}},"tools":[],"files":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	if p.ID != "g-p-1" || p.Name != "k8s" {
		t.Fatalf("%+v", p)
	}
}

func TestParseProjectConversations(t *testing.T) {
	raw := []byte(`{
	  "items":[{"id":"conv-1","title":"hi","gizmo_id":"g-p-1","conversation_template_id":"g-p-1"}],
	  "cursor": 0
	}`)
	items, cursor, err := parseProjectConversations(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].ID != "conv-1" {
		t.Fatalf("%+v", items)
	}
	if cursor != "0" {
		t.Fatalf("cursor=%q", cursor)
	}
}

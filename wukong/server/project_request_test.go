package server

import "testing"

func TestResolvedGizmoID(t *testing.T) {
	req := ChatCompletionRequest{ProjectID: "g-p-aaa"}
	if req.resolvedGizmoID() != "g-p-aaa" {
		t.Fatalf("project_id=%s", req.resolvedGizmoID())
	}
	req = ChatCompletionRequest{GizmoID: "bbb"}
	if req.resolvedGizmoID() != "g-p-bbb" {
		t.Fatalf("gizmo_id=%s", req.resolvedGizmoID())
	}
	req = ChatCompletionRequest{ProjectID: "g-p-aaa", GizmoID: "g-p-bbb"}
	if req.resolvedGizmoID() != "g-p-aaa" {
		t.Fatalf("project_id should win, got %s", req.resolvedGizmoID())
	}
}

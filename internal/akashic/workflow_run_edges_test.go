package akashic

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestSchemaCreatesWorkflowRunEdgesTableOnFreshDB(t *testing.T) {
	ctx := context.Background()
	recorder := newTestRecorder(t)

	var tableName string
	if err := recorder.db.QueryRowContext(ctx,
		`SELECT name FROM sqlite_master WHERE type='table' AND name='workflow_run_edges'`).Scan(&tableName); err != nil {
		t.Fatalf("workflow_run_edges table missing: %v", err)
	}

	for _, name := range []string{"idx_workflow_run_edges_parent", "idx_workflow_run_edges_child"} {
		var indexName string
		if err := recorder.db.QueryRowContext(ctx,
			`SELECT name FROM sqlite_master WHERE type='index' AND name=?`, name).Scan(&indexName); err != nil {
			t.Fatalf("%s index missing: %v", name, err)
		}
	}

	want := []columnSnapshot{
		{cid: 0, name: "parent_run_id", ctype: "TEXT", notnull: 1, pk: 1},
		{cid: 1, name: "child_run_id", ctype: "TEXT", notnull: 1, pk: 2},
		{cid: 2, name: "child_workflow_id", ctype: "TEXT", notnull: 1, pk: 0},
		{cid: 3, name: "child_workflow_name", ctype: "TEXT", notnull: 1, pk: 0},
		{cid: 4, name: "recorded_at", ctype: "TEXT", notnull: 1, pk: 0},
	}
	assertColumnSnapshots(t, "workflow_run_edges", pragmaTableInfo(t, recorder.db, "workflow_run_edges"), want)
}

func TestWorkflowRunChildLineageRoundTripAndDescendants(t *testing.T) {
	ctx := context.Background()
	recorder := newTestRecorder(t)
	base := time.Date(2026, 5, 24, 2, 55, 56, 0, time.UTC)
	cwd := filepath.Join(t.TempDir(), "rilll")

	for _, run := range []WorkflowRun{
		{ID: "parent-run", WorkflowID: "sequence", WorkflowName: "Sequence", CWD: cwd, ProjectID: "rilll", StartedAt: base},
		{ID: "composite-run", WorkflowID: "spec-impl", WorkflowName: "Spec Impl", CWD: cwd, ProjectID: "rilll", StartedAt: base.Add(time.Minute)},
		{ID: "spec-run", WorkflowID: "spec", WorkflowName: "Spec", CWD: cwd, ProjectID: "rilll", StartedAt: base.Add(2 * time.Minute)},
		{ID: "impl-run", WorkflowID: "impl", WorkflowName: "Impl", CWD: cwd, ProjectID: "rilll", StartedAt: base.Add(3 * time.Minute)},
	} {
		if err := recorder.RecordWorkflowRunStart(ctx, run); err != nil {
			t.Fatalf("RecordWorkflowRunStart(%s): %v", run.ID, err)
		}
	}

	edges := []WorkflowRunChild{
		{ParentRunID: "parent-run", ChildRunID: "composite-run", ChildWorkflowID: "spec-impl", ChildWorkflowName: "Spec Impl", RecordedAt: base.Add(time.Second)},
		{ParentRunID: "composite-run", ChildRunID: "spec-run", ChildWorkflowID: "spec", ChildWorkflowName: "Spec", RecordedAt: base.Add(2 * time.Second)},
		{ParentRunID: "composite-run", ChildRunID: "impl-run", ChildWorkflowID: "impl", ChildWorkflowName: "Impl", RecordedAt: base.Add(3 * time.Second)},
	}
	for _, edge := range edges {
		if err := recorder.RecordWorkflowRunChild(ctx, edge); err != nil {
			t.Fatalf("RecordWorkflowRunChild(%s -> %s): %v", edge.ParentRunID, edge.ChildRunID, err)
		}
		// Idempotent upsert: duplicate observations must not duplicate descendant rows.
		if err := recorder.RecordWorkflowRunChild(ctx, edge); err != nil {
			t.Fatalf("RecordWorkflowRunChild duplicate(%s -> %s): %v", edge.ParentRunID, edge.ChildRunID, err)
		}
	}

	got, err := recorder.DescendantWorkflowRunIDs(ctx, "parent-run")
	if err != nil {
		t.Fatalf("DescendantWorkflowRunIDs: %v", err)
	}
	want := []string{"composite-run", "spec-run", "impl-run"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("DescendantWorkflowRunIDs = %v, want %v", got, want)
	}
}

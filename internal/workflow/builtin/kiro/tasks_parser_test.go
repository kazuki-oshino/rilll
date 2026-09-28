package kiro

import "testing"

func TestTasksParserExtractsPendingLeavesAndRollsUpParents(t *testing.T) {
	content := "- [ ] 1 Parent\n- [x] 1.1 Done\n- [ ] 1.2 Todo\n"
	tasks := ParseTasksMD(content)

	pending := ExtractPendingLeaves(tasks)
	if len(pending) != 1 || pending[0] != "1.2" {
		t.Fatalf("unexpected pending leaves: %#v", pending)
	}

	checked := ApplyChildCheck(content, "1.2")
	rolled := ApplyParentRollup(checked)
	if HasUncheckedItems(rolled) {
		t.Fatalf("expected all tasks checked after rollup:\n%s", rolled)
	}
}

// 親 leaf（子なし親）が pending leaves に含まれることを確認
func TestExtractPendingLeavesIncludesChildlessParent(t *testing.T) {
	content := "- [ ] 1 Parent\n- [x] 1.1 Done\n- [ ] 1.2 Todo\n- [ ] 3 Standalone\n"
	tasks := ParseTasksMD(content)

	pending := ExtractPendingLeaves(tasks)
	if len(pending) != 2 {
		t.Fatalf("expected 2 pending leaves (1.2, 3), got: %#v", pending)
	}
	if pending[0] != "1.2" || pending[1] != "3" {
		t.Fatalf("unexpected pending leaves order: %#v", pending)
	}
}

// ParseTasksMD が IsLeaf を正しくセットすることを確認
func TestParseTasksMDSetsIsLeaf(t *testing.T) {
	content := "- [ ] 1 ParentWithChildren\n- [ ] 1.1 Child\n- [ ] 3 ChildlessParent\n"
	tasks := ParseTasksMD(content)

	if len(tasks) != 3 {
		t.Fatalf("expected 3 tasks, got %d: %#v", len(tasks), tasks)
	}

	cases := []struct {
		index    int
		taskID   string
		isParent bool
		isLeaf   bool
	}{
		{0, "1", true, false},   // 親かつ子あり → 非 leaf
		{1, "1.1", false, true}, // 子 → leaf
		{2, "3", true, true},    // 親かつ子なし → leaf
	}
	for _, c := range cases {
		got := tasks[c.index]
		if got.TaskID != c.taskID {
			t.Errorf("tasks[%d].TaskID = %q, want %q", c.index, got.TaskID, c.taskID)
		}
		if got.IsParent != c.isParent {
			t.Errorf("tasks[%d](%s).IsParent = %v, want %v", c.index, c.taskID, got.IsParent, c.isParent)
		}
		if got.IsLeaf != c.isLeaf {
			t.Errorf("tasks[%d](%s).IsLeaf = %v, want %v", c.index, c.taskID, got.IsLeaf, c.isLeaf)
		}
	}
}

// 親 leaf に対して ApplyChildCheck("3") が機能することを確認
func TestApplyChildCheckOnChildlessParent(t *testing.T) {
	content := "- [ ] 1 Parent\n- [ ] 1.1 Child\n- [ ] 3 Standalone\n"
	checked := ApplyChildCheck(content, "3")
	want := "- [ ] 1 Parent\n- [ ] 1.1 Child\n- [x] 3 Standalone\n"
	if checked != want {
		t.Fatalf("ApplyChildCheck did not mark childless parent:\ngot:\n%s\nwant:\n%s", checked, want)
	}
}

// 親 leaf を含む完全フローで HasUncheckedItems が false になることを確認
func TestFullFlowWithChildlessParent(t *testing.T) {
	content := "- [ ] 1 Parent\n- [ ] 1.1 Todo\n- [ ] 3 Standalone\n"
	step1 := ApplyChildCheck(content, "1.1")
	step2 := ApplyChildCheck(step1, "3")
	rolled := ApplyParentRollup(step2)
	if HasUncheckedItems(rolled) {
		t.Fatalf("expected all checked after full flow including childless parent:\n%s", rolled)
	}
}

// IsTaskChecked は kiro-impl-loop の per-iteration skip 判定で使う簡易ヘルパ。
// taskID が tasks.md 中で `- [x]` になっていれば true、`- [ ]` または不在なら false を返す。
//
// 用途: 直前 iteration での spillover (assign 外タスクが implementer により [x] 化された)
// 状況で、次 iteration の対象タスクが既に完了済かどうかを workflow 層から検知する。
func TestIsTaskChecked(t *testing.T) {
	cases := []struct {
		name    string
		content string
		taskID  string
		want    bool
	}{
		{
			name:    "checked leaf returns true",
			content: "- [x] 1.1 Done\n- [ ] 1.2 Todo\n",
			taskID:  "1.1",
			want:    true,
		},
		{
			name:    "unchecked leaf returns false",
			content: "- [x] 1.1 Done\n- [ ] 1.2 Todo\n",
			taskID:  "1.2",
			want:    false,
		},
		{
			name:    "checked childless parent returns true",
			content: "- [x] 3 Standalone\n",
			taskID:  "3",
			want:    true,
		},
		{
			name:    "different ID with same prefix is not matched",
			content: "- [x] 1.10 Done\n",
			taskID:  "1.1",
			want:    false,
		},
		{
			name:    "absent task ID returns false",
			content: "- [x] 1.1 Done\n",
			taskID:  "2.5",
			want:    false,
		},
		{
			name:    "empty content returns false",
			content: "",
			taskID:  "1.1",
			want:    false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := IsTaskChecked(c.content, c.taskID)
			if got != c.want {
				t.Errorf("IsTaskChecked(%q, %q) = %v, want %v", c.content, c.taskID, got, c.want)
			}
		})
	}
}

func TestValidateSpecRequiresApprovedSections(t *testing.T) {
	result := ValidateSpec(map[string]any{
		"ready_for_implementation": true,
		"approvals": map[string]any{
			"requirements": map[string]any{"approved": true},
			"design":       map[string]any{"approved": true},
			"tasks":        map[string]any{"approved": true},
		},
	})

	if !result.OK {
		t.Fatalf("expected spec to be valid: %#v", result)
	}
}

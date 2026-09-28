package kiro

import (
	"regexp"
	"strings"
)

type TaskItem struct {
	TaskID   string
	Checked  bool
	IsParent bool
	// IsLeaf は実行単位として扱うべきタスクかどうか。
	// 子タスク（X.Y）は常に true。親タスク（ドットなし）は子が
	// 同リスト内に存在しない場合のみ true（単独タスクとして実行する）。
	IsLeaf bool
}

var taskLineRE = regexp.MustCompile(`^- \[( |x)\] (\d+(?:\.\d+)*)\b`)

func ParseTasksMD(content string) []TaskItem {
	lines := strings.Split(content, "\n")
	items := make([]TaskItem, 0)
	for _, line := range lines {
		match := taskLineRE.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		taskID := match[2]
		items = append(items, TaskItem{
			TaskID:   taskID,
			Checked:  match[1] == "x",
			IsParent: !strings.Contains(taskID, "."),
		})
	}

	// 各親 ID ごとの子の数を集計し、子なし親を leaf として扱えるようにする
	childrenByParent := map[string]int{}
	for _, t := range items {
		if t.IsParent {
			continue
		}
		parent := strings.Split(t.TaskID, ".")[0]
		childrenByParent[parent]++
	}
	for i := range items {
		if items[i].IsParent {
			items[i].IsLeaf = childrenByParent[items[i].TaskID] == 0
		} else {
			items[i].IsLeaf = true
		}
	}

	return items
}

// ExtractPendingLeaves は未チェックの leaf タスク（実行単位）を返す。
// 子タスク（X.Y）と、子を持たない親タスクの両方が対象になる。
func ExtractPendingLeaves(tasks []TaskItem) []string {
	var out []string
	for _, task := range tasks {
		if task.IsLeaf && !task.Checked {
			out = append(out, task.TaskID)
		}
	}
	return out
}

func ApplyChildCheck(content string, taskID string) string {
	lines := strings.Split(content, "\n")
	for i, line := range lines {
		match := taskLineRE.FindStringSubmatch(line)
		if match != nil && match[2] == taskID {
			lines[i] = strings.Replace(line, "- [ ]", "- [x]", 1)
		}
	}
	return strings.Join(lines, "\n")
}

func ApplyParentRollup(content string) string {
	tasks := ParseTasksMD(content)
	childrenByParent := map[string][]TaskItem{}
	for _, task := range tasks {
		if task.IsParent {
			continue
		}
		parent := strings.Split(task.TaskID, ".")[0]
		childrenByParent[parent] = append(childrenByParent[parent], task)
	}

	parentsToCheck := map[string]bool{}
	for _, task := range tasks {
		if !task.IsParent || task.Checked {
			continue
		}
		children := childrenByParent[task.TaskID]
		if len(children) == 0 {
			continue
		}
		allChecked := true
		for _, child := range children {
			if !child.Checked {
				allChecked = false
				break
			}
		}
		if allChecked {
			parentsToCheck[task.TaskID] = true
		}
	}

	lines := strings.Split(content, "\n")
	for i, line := range lines {
		match := taskLineRE.FindStringSubmatch(line)
		if match != nil && parentsToCheck[match[2]] {
			lines[i] = strings.Replace(line, "- [ ]", "- [x]", 1)
		}
	}
	return strings.Join(lines, "\n")
}

// IsTaskChecked は taskID が tasks.md content 内で `- [x]` としてマーク済みかを返す。
//
// kiro-impl-loop の per-iteration skip 判定で使用する。run 開始時に snapshot した
// pendingTasks のうち、ループが進む間に他 iteration の spillover (実装エージェントが
// scope 越えして他タスクを暗黙的に [x] 化したケース) で既完了になったものを検知して、
// 実装エージェントへの再呼び出しによる "もう完了済" inquiry の連鎖を断つ目的。
//
// taskID が content 中に存在しない場合は false を返す (= "skip 対象ではない" 扱い)。
func IsTaskChecked(content string, taskID string) bool {
	for _, t := range ParseTasksMD(content) {
		if t.TaskID == taskID {
			return t.Checked
		}
	}
	return false
}

func HasUncheckedItems(content string) bool {
	for _, task := range ParseTasksMD(content) {
		if !task.Checked {
			return true
		}
	}
	return false
}

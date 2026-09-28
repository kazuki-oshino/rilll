package kiro

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// listImplementableFeatures は kiro-impl-loop の対象 feature を絞り込む関数。
// 「tasks.md 存在 + 未チェック項目あり + spec.json が ValidateSpec を通る」の
// AND 条件を満たす feature のみを辞書順で返す。
//
// 本テストは <cwd>/.kiro/specs/ 配下に擬似的な feature ディレクトリを組み、
// 各条件をピンポイントで揺らした状態を生成して期待挙動を assert する。

const validImplSpecJSON = `{
  "ready_for_implementation": true,
  "approvals": {
    "requirements": {"approved": true},
    "design": {"approved": true},
    "tasks": {"approved": true}
  }
}`

// writeFeature はテスト用に <cwd>/.kiro/specs/<feature>/ に
// 任意のファイル群を書き込むヘルパ。content が空文字のキーは
// ファイルを置かない（= そのファイル「不在」状態を表現）。
func writeFeature(t *testing.T, cwd, feature string, files map[string]string) {
	t.Helper()
	dir := filepath.Join(cwd, ".kiro", "specs", feature)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", dir, err)
	}
	for name, body := range files {
		if body == "" {
			continue
		}
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("WriteFile(%s): %v", path, err)
		}
	}
}

func TestListImplementableFeatures_TasksMissing(t *testing.T) {
	cwd := t.TempDir()
	// spec.json は満たしているが tasks.md が無い feature → 除外される
	writeFeature(t, cwd, "no-tasks", map[string]string{
		"spec.json": validImplSpecJSON,
	})
	got, err := listImplementableFeatures(cwd)
	if err != nil {
		t.Fatalf("listImplementableFeatures: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty result, got %v", got)
	}
}

func TestListImplementableFeatures_AllTasksChecked(t *testing.T) {
	cwd := t.TempDir()
	writeFeature(t, cwd, "all-checked", map[string]string{
		"spec.json": validImplSpecJSON,
		"tasks.md":  "- [x] 1. 完了済み\n- [x] 2. 完了済み2\n",
	})
	got, err := listImplementableFeatures(cwd)
	if err != nil {
		t.Fatalf("listImplementableFeatures: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty result, got %v", got)
	}
}

func TestListImplementableFeatures_SpecJSONInvalid(t *testing.T) {
	cases := map[string]string{
		"missing":     "",
		"not_ready":   `{"ready_for_implementation": false, "approvals": {"requirements":{"approved":true},"design":{"approved":true},"tasks":{"approved":true}}}`,
		"unapproved":  `{"ready_for_implementation": true, "approvals": {"requirements":{"approved":true},"design":{"approved":false},"tasks":{"approved":true}}}`,
		"broken_json": `{not-json`,
		"object_root": `[]`,
	}
	for name, specJSON := range cases {
		t.Run(name, func(t *testing.T) {
			cwd := t.TempDir()
			writeFeature(t, cwd, "feat", map[string]string{
				"spec.json": specJSON,
				"tasks.md":  "- [ ] 1. 未完\n",
			})
			got, err := listImplementableFeatures(cwd)
			if err != nil {
				t.Fatalf("listImplementableFeatures: %v", err)
			}
			if len(got) != 0 {
				t.Fatalf("expected feature to be excluded, got %v", got)
			}
		})
	}
}

func TestListImplementableFeatures_HasPendingAndApproved(t *testing.T) {
	cwd := t.TempDir()
	writeFeature(t, cwd, "ready-feat", map[string]string{
		"spec.json": validImplSpecJSON,
		"tasks.md":  "- [x] 1. 完了\n- [ ] 2. 未完\n",
	})
	got, err := listImplementableFeatures(cwd)
	if err != nil {
		t.Fatalf("listImplementableFeatures: %v", err)
	}
	if !reflect.DeepEqual(got, []string{"ready-feat"}) {
		t.Fatalf("expected [ready-feat], got %v", got)
	}
}

func TestListImplementableFeatures_SpecsDirMissing(t *testing.T) {
	cwd := t.TempDir() // .kiro/specs/ を作らない
	got, err := listImplementableFeatures(cwd)
	if err != nil {
		t.Fatalf("listImplementableFeatures: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected nil/empty when specs dir missing, got %v", got)
	}
}

func TestListImplementableFeatures_SortedOrder(t *testing.T) {
	cwd := t.TempDir()
	for _, feature := range []string{"zeta", "alpha", "mu"} {
		writeFeature(t, cwd, feature, map[string]string{
			"spec.json": validImplSpecJSON,
			"tasks.md":  "- [ ] 1. 未完\n",
		})
	}
	// 候補対象外の feature も混ぜて、フィルタとソートが両立することを示す
	writeFeature(t, cwd, "skip-me", map[string]string{
		"spec.json": validImplSpecJSON,
		"tasks.md":  "- [x] 1. 完了\n",
	})
	got, err := listImplementableFeatures(cwd)
	if err != nil {
		t.Fatalf("listImplementableFeatures: %v", err)
	}
	want := []string{"alpha", "mu", "zeta"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("expected %v in sorted order, got %v", want, got)
	}
}

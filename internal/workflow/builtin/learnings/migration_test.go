// migration_test.go: 1 回限り (Task 7.5) の手動 migration helper。
// 本番 docs_dev/playbook/ に対して RebuildIndex を実行し index.md を生成する。
// 実行: RILLL_PLAYBOOK_MIGRATE_REBUILD=1 go test -count=1 -run TestMigrationRebuildIndex ./internal/workflow/builtin/learnings/
// 通常 CI では env var が無いため t.Skip で 1 micro 秒 PASS する。
package learnings

import (
	"os"
	"testing"
)

func TestMigrationRebuildIndex(t *testing.T) {
	if os.Getenv("RILLL_PLAYBOOK_MIGRATE_REBUILD") == "" {
		t.Skip("set RILLL_PLAYBOOK_MIGRATE_REBUILD=1 to run migration helper (Task 7.5)")
	}
	root := os.Getenv("RILLL_PLAYBOOK_MIGRATE_ROOT")
	if root == "" {
		t.Fatal("RILLL_PLAYBOOK_MIGRATE_ROOT must point to docs_dev/playbook/ absolute path")
	}
	warnings, err := RebuildIndex(RebuildIndexInput{PlaybookRoot: root})
	if err != nil {
		t.Fatalf("RebuildIndex: %v", err)
	}
	if len(warnings) > 0 {
		t.Fatalf("warnings (expected zero on clean migration): %+v", warnings)
	}
}

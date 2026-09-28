// Package apply のテスト本ファイルは task 2.2 / design.md §Precheck の契約を検証する
// (Req 1.3, 2.1, 2.2, 2.5)。
//
// 検証観点 (tasks.md 2.2 観察可能完了条件):
//   - clean tree + 正常 reviewRunID + 既存 dir → Verdict==VerdictUnset (success)
//   - dirty tree → Verdict==VerdictPreconditionFailed (Cause に dirty 由来 error)
//   - reviewRunID 空 → Verdict==VerdictPreconditionFailed (GitOps.Status 未呼出)
//   - dir 不存在 → Verdict==VerdictPreconditionFailed (GitOps.Status 未呼出)
//   - reviewRunID に `..` 含み → Verdict==VerdictPreconditionFailed (GitOps.Status 未呼出)
//   - reviewRunID が絶対パス → Verdict==VerdictPreconditionFailed (GitOps.Status 未呼出)
//
// 副作用契約 (Req 2.3 / design.md §Precheck Responsibilities):
//   - precheck フェーズでは LLM / git apply / commit 等の副作用を一切起動しない。
//   - GitOps の Snapshot / ApplyCheck / Apply / Revert は precheck からは呼ばれない
//     (call counter == 0 で機械検証)。
//   - reviewRunID 検証や dir 不在検証で fail する経路では GitOps.Status も呼ばれない
//     (cheap な path 検証 → dir 検証 → status 検証 の順序契約)。
package apply

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	improvepatch "github.com/kazuki-oshino/rilll/internal/workflow/builtin/improve/patch"
)

// fakePrecheckGitOps は improvepatch.GitOps interface を満たす最小 fake で、
// Status の戻り値を構成可能にしつつ全 5 メソッドの呼び出し回数を記録する。
// task 2.2 precheck の副作用契約 (Snapshot/ApplyCheck/Apply/Revert が呼ばれないこと、
// path 検証 fail 経路では Status すら呼ばれないこと) を機械検証する用途。
type fakePrecheckGitOps struct {
	statusClean bool
	statusErr   error

	statusCalls     int
	snapshotCalls   int
	applyCheckCalls int
	applyCalls      int
	revertCalls     int
}

// Status は構成された (clean, err) を返し、呼び出し回数を加算する。
func (f *fakePrecheckGitOps) Status(_ context.Context, _ string) (bool, error) {
	f.statusCalls++
	return f.statusClean, f.statusErr
}

// Snapshot は precheck から呼ばれない契約のため、テストでは call counter のみ記録する。
func (f *fakePrecheckGitOps) Snapshot(_ context.Context, _ string) (improvepatch.BaselineRef, error) {
	f.snapshotCalls++
	return improvepatch.BaselineRef{}, nil
}

// ApplyCheck は precheck から呼ばれない契約のため、テストでは call counter のみ記録する。
func (f *fakePrecheckGitOps) ApplyCheck(_ context.Context, _ string, _ []byte) error {
	f.applyCheckCalls++
	return nil
}

// Apply は precheck から呼ばれない契約のため、テストでは call counter のみ記録する。
func (f *fakePrecheckGitOps) Apply(_ context.Context, _ string, _ []byte) error {
	f.applyCalls++
	return nil
}

// Revert は precheck から呼ばれない契約のため、テストでは call counter のみ記録する。
func (f *fakePrecheckGitOps) Revert(_ context.Context, _ string, _ improvepatch.BaselineRef, _ []string) error {
	f.revertCalls++
	return nil
}

// 型レベル assertion: fakePrecheckGitOps が improvepatch.GitOps interface を満たすことを
// コンパイル時に保証する。
var _ improvepatch.GitOps = (*fakePrecheckGitOps)(nil)

// sideEffectCalls は副作用メソッド (Snapshot/ApplyCheck/Apply/Revert) の合計呼び出し回数を返す。
// precheck がこれらを一切呼ばないこと (== 0) を機械検証する用途。
func (f *fakePrecheckGitOps) sideEffectCalls() int {
	return f.snapshotCalls + f.applyCheckCalls + f.applyCalls + f.revertCalls
}

// precheckTestEnv は Precheck テスト用の <cwd>/.rilll/review-workflow/<reviewRunID>/ を
// t.TempDir() 配下に構築する helper。返り値は (cwd, reviewRunID)。
// 各テストが独立した temp dir を持つため t.Parallel() 安全。
func precheckTestEnv(t *testing.T, runID string) (cwd, reviewRunID string) {
	t.Helper()
	cwd = t.TempDir()
	reviewRunID = runID
	runDir := filepath.Join(cwd, ".rilll", "review-workflow", runID)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", runDir, err)
	}
	return cwd, reviewRunID
}

// TestPrecheck_Success_CleanTree は clean tree + 正常 reviewRunID + 既存 dir の正常分岐で
// Verdict==VerdictUnset, Summary=="", Cause==nil を返すことを検証する。
// observable: モック GitOps を注入した上で success ケースで期待 Verdict が返る。
func TestPrecheck_Success_CleanTree(t *testing.T) {
	cwd, runID := precheckTestEnv(t, "run-001")
	git := &fakePrecheckGitOps{statusClean: true}

	got := Precheck(context.Background(), PrecheckInput{
		Cwd:         cwd,
		ReviewRunID: runID,
		GitOps:      git,
	})

	if got.Verdict != VerdictUnset {
		t.Errorf("Verdict: got %q, want %q (success は VerdictUnset)", got.Verdict, VerdictUnset)
	}
	if got.Summary != "" {
		t.Errorf("Summary: got %q, want empty (success 時は空文字)", got.Summary)
	}
	if got.Cause != nil {
		t.Errorf("Cause: got %v, want nil (success 時は nil)", got.Cause)
	}
	if c := git.statusCalls; c != 1 {
		t.Errorf("GitOps.Status 呼び出し回数: got %d, want 1", c)
	}
	if c := git.sideEffectCalls(); c != 0 {
		t.Errorf("Snapshot/ApplyCheck/Apply/Revert 合計呼び出し回数: got %d, want 0 (副作用契約違反)", c)
	}
}

// TestPrecheck_DirtyTree は git working tree dirty 経路で
// VerdictPreconditionFailed と dirty を示す Cause が返り、副作用が走らないことを検証する。
// (Req 2.1, 2.2)
func TestPrecheck_DirtyTree(t *testing.T) {
	cwd, runID := precheckTestEnv(t, "run-002")
	git := &fakePrecheckGitOps{statusClean: false}

	got := Precheck(context.Background(), PrecheckInput{
		Cwd:         cwd,
		ReviewRunID: runID,
		GitOps:      git,
	})

	if got.Verdict != VerdictPreconditionFailed {
		t.Errorf("Verdict: got %q, want %q", got.Verdict, VerdictPreconditionFailed)
	}
	if got.Summary == "" {
		t.Errorf("Summary: got empty, want dirty 由来の利用者向け短文")
	}
	if got.Cause == nil {
		t.Fatalf("Cause: got nil, want non-nil (dirty 経路は内部 error を保持)")
	}
	if !errors.Is(got.Cause, improvepatch.ErrDirtyWorkingTree) {
		t.Errorf("Cause chain: %v should wrap improvepatch.ErrDirtyWorkingTree", got.Cause)
	}
	if c := git.statusCalls; c != 1 {
		t.Errorf("GitOps.Status 呼び出し回数: got %d, want 1", c)
	}
	if c := git.sideEffectCalls(); c != 0 {
		t.Errorf("Snapshot/ApplyCheck/Apply/Revert 合計呼び出し回数: got %d, want 0", c)
	}
}

// TestPrecheck_DirtyTree_StatusError は GitOps.Status 自体が error を返した経路で
// VerdictPreconditionFailed と元 error が Cause に保持されることを検証する。
// (補助ケース: design.md §Precheck Responsibilities の防御層)
func TestPrecheck_DirtyTree_StatusError(t *testing.T) {
	cwd, runID := precheckTestEnv(t, "run-002b")
	wantErr := errors.New("git unavailable")
	git := &fakePrecheckGitOps{statusClean: true, statusErr: wantErr}

	got := Precheck(context.Background(), PrecheckInput{
		Cwd:         cwd,
		ReviewRunID: runID,
		GitOps:      git,
	})

	if got.Verdict != VerdictPreconditionFailed {
		t.Errorf("Verdict: got %q, want %q", got.Verdict, VerdictPreconditionFailed)
	}
	if got.Cause == nil || !errors.Is(got.Cause, wantErr) {
		t.Errorf("Cause: got %v, want chain wrapping %v", got.Cause, wantErr)
	}
	if c := git.sideEffectCalls(); c != 0 {
		t.Errorf("副作用合計呼び出し回数: got %d, want 0", c)
	}
}

// TestPrecheck_EmptyReviewRunID は reviewRunID が空文字の経路で
// VerdictPreconditionFailed が返り、GitOps.Status すら呼ばれないことを検証する。
// (Req 1.3 / 副作用契約: cheap な path 検証 → status 検証 の順序)
func TestPrecheck_EmptyReviewRunID(t *testing.T) {
	cwd := t.TempDir()
	git := &fakePrecheckGitOps{statusClean: true}

	got := Precheck(context.Background(), PrecheckInput{
		Cwd:         cwd,
		ReviewRunID: "",
		GitOps:      git,
	})

	if got.Verdict != VerdictPreconditionFailed {
		t.Errorf("Verdict: got %q, want %q", got.Verdict, VerdictPreconditionFailed)
	}
	if got.Summary == "" {
		t.Errorf("Summary: got empty, want 空 id を示す利用者向け短文")
	}
	if got.Cause == nil {
		t.Fatalf("Cause: got nil, want non-nil")
	}
	if c := git.statusCalls; c != 0 {
		t.Errorf("空 id 経路で GitOps.Status は呼ばれない (順序契約): got %d, want 0", c)
	}
	if c := git.sideEffectCalls(); c != 0 {
		t.Errorf("副作用合計呼び出し回数: got %d, want 0", c)
	}
}

// TestPrecheck_DirNotFound は <cwd>/.rilll/review-workflow/<runID>/ が存在しない経路で
// VerdictPreconditionFailed が返り、GitOps.Status は呼ばれないことを検証する。
// (Req 1.3)
func TestPrecheck_DirNotFound(t *testing.T) {
	cwd := t.TempDir() // .rilll 階層を作らない
	git := &fakePrecheckGitOps{statusClean: true}

	got := Precheck(context.Background(), PrecheckInput{
		Cwd:         cwd,
		ReviewRunID: "missing-run",
		GitOps:      git,
	})

	if got.Verdict != VerdictPreconditionFailed {
		t.Errorf("Verdict: got %q, want %q", got.Verdict, VerdictPreconditionFailed)
	}
	if got.Summary == "" {
		t.Errorf("Summary: got empty, want dir 不在を示す利用者向け短文")
	}
	if got.Cause == nil {
		t.Fatalf("Cause: got nil, want non-nil")
	}
	if c := git.statusCalls; c != 0 {
		t.Errorf("dir 不在経路で GitOps.Status は呼ばれない (順序契約): got %d, want 0", c)
	}
	if c := git.sideEffectCalls(); c != 0 {
		t.Errorf("副作用合計呼び出し回数: got %d, want 0", c)
	}
}

// TestPrecheck_DotDotID は reviewRunID に `..` を含む経路で
// `filepath.Clean` + prefix 検証により reject されることを検証する。
// (design.md §10 Threats: review run id は filesystem path に含まれるため `..` を reject)
func TestPrecheck_DotDotID(t *testing.T) {
	cwd, _ := precheckTestEnv(t, "run-003") // 正規 dir は作るが、攻撃 id では到達しない
	git := &fakePrecheckGitOps{statusClean: true}

	cases := []string{
		"..",
		"../escape",
		"../../etc/passwd",
		"foo/../bar", // `..` を経由する間接形式も reject
	}
	for _, badID := range cases {
		t.Run(badID, func(t *testing.T) {
			got := Precheck(context.Background(), PrecheckInput{
				Cwd:         cwd,
				ReviewRunID: badID,
				GitOps:      git,
			})
			if got.Verdict != VerdictPreconditionFailed {
				t.Errorf("Verdict: got %q, want %q (`..` 含み id は reject)", got.Verdict, VerdictPreconditionFailed)
			}
			if got.Summary == "" {
				t.Errorf("Summary: got empty, want 不正 id を示す利用者向け短文")
			}
			if got.Cause == nil {
				t.Errorf("Cause: got nil, want non-nil")
			}
		})
	}
	if c := git.statusCalls; c != 0 {
		t.Errorf("不正 id 経路で GitOps.Status は呼ばれない (順序契約): got %d, want 0", c)
	}
	if c := git.sideEffectCalls(); c != 0 {
		t.Errorf("副作用合計呼び出し回数: got %d, want 0", c)
	}
}

// TestPrecheck_AbsolutePathID は reviewRunID が絶対パスの経路で reject されることを検証する。
// (design.md §10 Threats: 絶対パス id は precheck 段で reject)
func TestPrecheck_AbsolutePathID(t *testing.T) {
	cwd, _ := precheckTestEnv(t, "run-004")
	git := &fakePrecheckGitOps{statusClean: true}

	// プラットフォーム依存を避けるため filepath.Join("/", ...) で組み立てる絶対パスを使う。
	absID := filepath.Join(string(filepath.Separator), "etc", "passwd")
	got := Precheck(context.Background(), PrecheckInput{
		Cwd:         cwd,
		ReviewRunID: absID,
		GitOps:      git,
	})

	if got.Verdict != VerdictPreconditionFailed {
		t.Errorf("Verdict: got %q, want %q (絶対パス id は reject)", got.Verdict, VerdictPreconditionFailed)
	}
	if got.Summary == "" {
		t.Errorf("Summary: got empty, want 不正 id を示す利用者向け短文")
	}
	if got.Cause == nil {
		t.Fatalf("Cause: got nil, want non-nil")
	}
	if c := git.statusCalls; c != 0 {
		t.Errorf("絶対パス id 経路で GitOps.Status は呼ばれない (順序契約): got %d, want 0", c)
	}
	if c := git.sideEffectCalls(); c != 0 {
		t.Errorf("副作用合計呼び出し回数: got %d, want 0", c)
	}
}

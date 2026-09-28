// Package improvepatch のテスト本ファイルは Precheck helper 関数の
// 純粋 orchestration 契約を検証する
// (tasks.md 2.3 / design.md §System Flows / Req 1.4, 1.6)。
//
// 検証観点:
//   - 順序契約: GoModInspector.ModulePath → GitOps.Status の順に評価される
//   - 失敗分岐 3 種 (gomod 不一致 / status dirty / 両方失敗) で
//     VerdictPreconditionFailed が返ること
//   - 正常分岐 1 種で verdict が空文字列 sentinel (continue を表す) を返すこと
//   - 副作用契約: GitOps.Snapshot / ApplyCheck / Apply / Revert は precheck からは
//     呼ばれない (call counter が 0 のまま)
//   - 戻り値型は (moduleRoot, verdict, err) の 3 つだけで、
//     soft warning ステータスを返す型を持たない (構造的検証)
package patch

import (
	"context"
	"errors"
	"testing"
)

// fakeGitOps は GitOps interface を満たす最小 fake 実装で、
// Status の戻り値を構成可能にしつつ全 5 メソッドの呼び出し回数を記録する。
//
// task 2.3 の precheck 副作用契約検証 (Snapshot/ApplyCheck/Apply/Revert が
// 呼ばれないこと) と、後続 task 2.4 (Req 1.6 hard-fail negative test) の
// 両方で再利用される想定で、本ファイルにテスト fixture として配置する。
type fakeGitOps struct {
	statusClean bool
	statusErr   error

	statusCalls     int
	snapshotCalls   int
	applyCheckCalls int
	applyCalls      int
	revertCalls     int
}

// Status は構成された (clean, err) を返し、呼び出し回数を加算する。
func (f *fakeGitOps) Status(ctx context.Context, cwd string) (bool, error) {
	f.statusCalls++
	return f.statusClean, f.statusErr
}

// Snapshot は precheck から呼ばれない契約のため、テストでは call counter のみ記録する。
func (f *fakeGitOps) Snapshot(ctx context.Context, cwd string) (BaselineRef, error) {
	f.snapshotCalls++
	return BaselineRef{}, nil
}

// ApplyCheck は precheck から呼ばれない契約のため、テストでは call counter のみ記録する。
func (f *fakeGitOps) ApplyCheck(ctx context.Context, cwd string, diff []byte) error {
	f.applyCheckCalls++
	return nil
}

// Apply は precheck から呼ばれない契約のため、テストでは call counter のみ記録する。
func (f *fakeGitOps) Apply(ctx context.Context, cwd string, diff []byte) error {
	f.applyCalls++
	return nil
}

// Revert は precheck から呼ばれない契約のため、テストでは call counter のみ記録する。
func (f *fakeGitOps) Revert(ctx context.Context, cwd string, ref BaselineRef, targetFiles []string) error {
	f.revertCalls++
	return nil
}

// 型レベル assertion: fakeGitOps が GitOps interface を満たすことをコンパイル時に保証する。
var _ GitOps = (*fakeGitOps)(nil)

// sideEffectCalls は fakeGitOps の副作用メソッド (Snapshot/ApplyCheck/Apply/Revert) の
// 呼び出し合計を返す。precheck がこれらを一切呼ばないこと (== 0) を機械検証する用途。
func (f *fakeGitOps) sideEffectCalls() int {
	return f.snapshotCalls + f.applyCheckCalls + f.applyCalls + f.revertCalls
}

// TestPrecheck_Success は GoMod が一致 + Status が clean の正常分岐で
// (moduleRoot, "", nil) を返し、副作用メソッドが一切呼ばれないことを検証する。
// 観察可能完了条件: 通過時 verdict は空文字列 sentinel (continue を表す)。
func TestPrecheck_Success(t *testing.T) {
	const wantModuleRoot = "/abs/path/to/rilll"
	gomod := &fakeGoModInspector{
		modulePath: "github.com/kazuki-oshino/rilll",
		moduleRoot: wantModuleRoot,
		err:        nil,
	}
	git := &fakeGitOps{statusClean: true, statusErr: nil}

	moduleRoot, verdict, err := Precheck(context.Background(), "/some/cwd", git, gomod)
	if err != nil {
		t.Fatalf("Precheck: 予期しない error: %v", err)
	}
	if verdict != Verdict("") {
		t.Errorf("verdict: got %q, want %q (continue を表す空文字列 sentinel)", verdict, "")
	}
	if moduleRoot != wantModuleRoot {
		t.Errorf("moduleRoot: got %q, want %q", moduleRoot, wantModuleRoot)
	}
	if got := gomod.callCount; got != 1 {
		t.Errorf("GoModInspector.ModulePath 呼び出し回数: got %d, want 1", got)
	}
	if got := git.statusCalls; got != 1 {
		t.Errorf("GitOps.Status 呼び出し回数: got %d, want 1", got)
	}
	// 副作用契約: Snapshot/ApplyCheck/Apply/Revert は precheck から呼ばれない (Req 1.6 hard-fail 構造保証)
	if got := git.sideEffectCalls(); got != 0 {
		t.Errorf("Snapshot/ApplyCheck/Apply/Revert 合計呼び出し回数: got %d, want 0", got)
	}
}

// TestPrecheck_GoModMismatch は GoMod が error を返す失敗分岐で
// VerdictPreconditionFailed と gomod の error chain が伝播することを検証する。
// 順序契約により GitOps.Status は呼ばれない。
func TestPrecheck_GoModMismatch(t *testing.T) {
	wantErr := errors.New("improvepatch: module path mismatch")
	gomod := &fakeGoModInspector{
		modulePath: "",
		moduleRoot: "",
		err:        wantErr,
	}
	// Status は呼ばれないが、仮に呼ばれた場合に「気付ける」よう dirty を構成しておく。
	git := &fakeGitOps{statusClean: false, statusErr: nil}

	moduleRoot, verdict, err := Precheck(context.Background(), "/some/cwd", git, gomod)
	if err == nil {
		t.Fatalf("error が返されることを期待: got nil")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("error chain: %v should wrap %v (errors.Is)", err, wantErr)
	}
	if verdict != VerdictPreconditionFailed {
		t.Errorf("verdict: got %q, want %q", verdict, VerdictPreconditionFailed)
	}
	if moduleRoot != "" {
		t.Errorf("error 経路では moduleRoot は空文字列を期待: got %q", moduleRoot)
	}
	// 順序契約: gomod 失敗時 GitOps.Status は呼ばれない (early return)
	if got := git.statusCalls; got != 0 {
		t.Errorf("gomod 失敗時 GitOps.Status は呼ばれない (順序契約): got %d, want 0", got)
	}
	// 副作用契約: Snapshot/ApplyCheck/Apply/Revert は precheck から呼ばれない
	if got := git.sideEffectCalls(); got != 0 {
		t.Errorf("Snapshot/ApplyCheck/Apply/Revert 合計呼び出し回数: got %d, want 0", got)
	}
}

// TestPrecheck_StatusDirty は GoMod が一致 + Status が dirty の失敗分岐で
// VerdictPreconditionFailed と dirty を示す error が返ることを検証する。
// gomod は呼ばれており、かつ副作用メソッドは呼ばれない。
func TestPrecheck_StatusDirty(t *testing.T) {
	const wantModuleRoot = "/abs/path/to/rilll"
	gomod := &fakeGoModInspector{
		modulePath: "github.com/kazuki-oshino/rilll",
		moduleRoot: wantModuleRoot,
		err:        nil,
	}
	git := &fakeGitOps{statusClean: false, statusErr: nil}

	moduleRoot, verdict, err := Precheck(context.Background(), "/some/cwd", git, gomod)
	if err == nil {
		t.Fatalf("error が返されることを期待: got nil")
	}
	if !errors.Is(err, ErrDirtyWorkingTree) {
		t.Errorf("error chain: %v should wrap ErrDirtyWorkingTree", err)
	}
	if verdict != VerdictPreconditionFailed {
		t.Errorf("verdict: got %q, want %q", verdict, VerdictPreconditionFailed)
	}
	if moduleRoot != "" {
		t.Errorf("error 経路では moduleRoot は空文字列を期待: got %q", moduleRoot)
	}
	if got := gomod.callCount; got != 1 {
		t.Errorf("GoModInspector.ModulePath 呼び出し回数: got %d, want 1", got)
	}
	if got := git.statusCalls; got != 1 {
		t.Errorf("GitOps.Status 呼び出し回数: got %d, want 1", got)
	}
	// 副作用契約: Snapshot/ApplyCheck/Apply/Revert は precheck から呼ばれない (Req 1.6 hard-fail)
	if got := git.sideEffectCalls(); got != 0 {
		t.Errorf("Snapshot/ApplyCheck/Apply/Revert 合計呼び出し回数: got %d, want 0", got)
	}
}

// TestPrecheck_BothFail_GoModWins は gomod 失敗 + Status も失敗 (would-be-dirty + error) の
// 「両方失敗」ケースで、順序契約により gomod の失敗が観測され Status は呼ばれないことを検証する。
// observable 完了条件: 4 ケースのうち「両方失敗」分岐の期待 verdict が VerdictPreconditionFailed。
func TestPrecheck_BothFail_GoModWins(t *testing.T) {
	wantErr := errors.New("improvepatch: module path mismatch")
	gomod := &fakeGoModInspector{
		modulePath: "",
		moduleRoot: "",
		err:        wantErr,
	}
	// Status は dirty かつ error を返す設定。順序契約により呼ばれないはず。
	git := &fakeGitOps{statusClean: false, statusErr: errors.New("status would also fail")}

	moduleRoot, verdict, err := Precheck(context.Background(), "/some/cwd", git, gomod)
	if err == nil {
		t.Fatalf("error が返されることを期待: got nil")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("両方失敗ケースでは gomod の error が優先される (順序契約): got %v, want wraps %v", err, wantErr)
	}
	if verdict != VerdictPreconditionFailed {
		t.Errorf("verdict: got %q, want %q", verdict, VerdictPreconditionFailed)
	}
	if moduleRoot != "" {
		t.Errorf("error 経路では moduleRoot は空文字列を期待: got %q", moduleRoot)
	}
	if got := git.statusCalls; got != 0 {
		t.Errorf("両方失敗時 GitOps.Status は呼ばれない (順序契約): got %d, want 0", got)
	}
	// 副作用契約: Snapshot/ApplyCheck/Apply/Revert は precheck から呼ばれない
	if got := git.sideEffectCalls(); got != 0 {
		t.Errorf("Snapshot/ApplyCheck/Apply/Revert 合計呼び出し回数: got %d, want 0", got)
	}
}

// Package improvepatch のテスト本ファイルは Req 1.6 hard-fail 契約の核心を
// negative test として機械検証する (tasks.md 2.4 / requirements.md Req 1.6, 5.1, 5.9 /
// design.md §System Flows / §Error Handling)。
//
// 検証目的:
//   - dirty な作業ツリー (Status が clean=false を返す) で precheck が呼ばれた時、
//     VerdictPreconditionFailed が返り、副作用メソッド (Snapshot / ApplyCheck /
//     Apply / Revert) が一切呼ばれないことを機械検証する (Req 1.6 hard-fail / Req 5.1)。
//   - Revert 自体が呼ばれないため ErrRevertFailed の sentinel chain も発生しないことを検証する (Req 5.9)。
//   - precheck 関数の戻り値型が (string, Verdict, error) の 3 値固定であり、
//     soft warning として「警告だけ流して続行」する追加ステータス
//     (4 値目の WarningStatus、もしくは Verdict("warning") 値) を持たないことを
//     reflect で構造的に検証する。
//
// 参照: design.md §"precondition との結合 (機械検証可能性の契約)" 節
// > precondition 違反 (`git status --porcelain` 出力 1 行以上) は **hard fail**。
// > `precondition_failed` verdict で即終了し、Snapshot 以降の経路に進まない。
// > soft warning 経路は採らない。
//
// 既存テスト (precheck_test.go の TestPrecheck_StatusDirty) は orchestration 順序契約と
// 副作用ゼロを 4 ケース分岐の一部として確認しているが、本ファイルは Req 1.6 hard-fail を
// **専用 negative test** として記述し、(a) 副作用カウンタ全 4 個別検証、
// (b) ErrRevertFailed が chain しないことの sentinel 検証、(c) 戻り値型の構造的検証
// の 3 層を集中的に保証する (task 2.4 観察可能完了条件)。
package patch

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

// TestReq16HardFail_DirtyTree_NoSideEffects は Req 1.6 hard-fail 経路の
// 副作用ゼロ契約を機械検証する。
//
// シナリオ:
//   - GoModInspector は正常 (module path 一致 + moduleRoot 解決)
//   - GitOps.Status は dirty (clean=false) を返す
//
// 期待:
//   - verdict == VerdictPreconditionFailed
//   - moduleRoot は空文字列 (error 経路)
//   - err は ErrDirtyWorkingTree を wrap (errors.Is)
//   - errors.Is(err, ErrRevertFailed) == false (Revert 自体が呼ばれないため発生しない)
//   - fakeGitOps の Snapshot / ApplyCheck / Apply / Revert call counter が個別に全て 0
//
// 観察可能完了条件 (tasks.md 2.4):
//
//	precheck hard fail 経路で fake GitOps の Snapshot/ApplyCheck/Apply/Revert call counter が
//	全て 0 のまま終了する。
func TestReq16HardFail_DirtyTree_NoSideEffects(t *testing.T) {
	gomod := &fakeGoModInspector{
		modulePath: "github.com/kazuki-oshino/rilll",
		moduleRoot: "/abs/path/to/rilll",
		err:        nil,
	}
	// dirty を構成。Snapshot/ApplyCheck/Apply/Revert に「呼ばれたら気付ける」よう
	// fakeGitOps の call counter で観測する。
	git := &fakeGitOps{statusClean: false, statusErr: nil}

	moduleRoot, verdict, err := Precheck(context.Background(), "/some/cwd", git, gomod)

	// --- verdict / err / moduleRoot ---
	if verdict != VerdictPreconditionFailed {
		t.Errorf("Req 1.6 hard fail: verdict got %q, want %q", verdict, VerdictPreconditionFailed)
	}
	if moduleRoot != "" {
		t.Errorf("error 経路では moduleRoot は空文字列を期待: got %q", moduleRoot)
	}
	if err == nil {
		t.Fatalf("error が返されることを期待: got nil")
	}
	if !errors.Is(err, ErrDirtyWorkingTree) {
		t.Errorf("error chain: %v should wrap ErrDirtyWorkingTree (errors.Is)", err)
	}
	// Revert 自体が precheck から呼ばれない契約のため、ErrRevertFailed は chain しない (Req 5.9)。
	// soft warning 経路で「Revert を試みて警告を流す」実装に退化していないことの sentinel 検証。
	if errors.Is(err, ErrRevertFailed) {
		t.Errorf("Revert 自体が precheck から呼ばれない経路で ErrRevertFailed が発生してはならない: got %v", err)
	}

	// --- 副作用カウンタ (個別) ---
	// 既存 TestPrecheck_StatusDirty は sideEffectCalls() 合計でゼロを検証するが、
	// 本テストはどのメソッドが (誤って) 呼ばれても検出できるよう、4 メソッドを個別に検証する。
	if got := git.snapshotCalls; got != 0 {
		t.Errorf("Snapshot 呼び出し回数: got %d, want 0 (Req 1.6 hard fail で Snapshot は走らない / Req 5.1)", got)
	}
	if got := git.applyCheckCalls; got != 0 {
		t.Errorf("ApplyCheck 呼び出し回数: got %d, want 0 (Req 1.6 hard fail で ApplyCheck は走らない)", got)
	}
	if got := git.applyCalls; got != 0 {
		t.Errorf("Apply 呼び出し回数: got %d, want 0 (Req 1.6 hard fail で Apply は走らない)", got)
	}
	if got := git.revertCalls; got != 0 {
		t.Errorf("Revert 呼び出し回数: got %d, want 0 (Apply 未実行のため Revert は走らない契約 / Req 5.9)", got)
	}

	// 順序契約の補強: gomod は 1 度呼ばれ、Status も 1 度呼ばれる
	// (Status は dirty 判定後に early return するため副作用フェーズに進まない)。
	if got := gomod.callCount; got != 1 {
		t.Errorf("GoModInspector.ModulePath 呼び出し回数: got %d, want 1", got)
	}
	if got := git.statusCalls; got != 1 {
		t.Errorf("GitOps.Status 呼び出し回数: got %d, want 1", got)
	}
}

// TestReq16HardFail_PrecheckSignature_NoSoftWarningStatus は precheck 関数の
// 戻り値型を reflect で構造的に検証し、soft warning 経路を表す追加ステータスが
// 戻り値型に存在しないことを保証する (tasks.md 2.4 / Req 1.6)。
//
// soft warning 経路 (= 警告を返してそのまま continue) を実装するには、
// 戻り値に warning ステータスを表す
//   - 4 値目の WarningStatus 型 (新規追加)
//   - もしくは Verdict 列挙の "warning" 値の追加
//
// のいずれかが必要となり、前者は本テスト (NumOut() == 3) で検出される。
// 後者 (Verdict 列挙の closed set 不変性) は verdict_test.go で別途検証されている。
//
// この 2 層 (signature 構造 + 列挙 closed set) を組み合わせることで、
// soft warning 経路を実装するには複数ファイルにまたがる仕様変更が必要になり、
// うっかり実装が混入することを構造的に防止する。
func TestReq16HardFail_PrecheckSignature_NoSoftWarningStatus(t *testing.T) {
	fnType := reflect.TypeOf(Precheck)
	if fnType.Kind() != reflect.Func {
		t.Fatalf("Precheck は関数型である必要がある: got Kind=%v", fnType.Kind())
	}

	// 戻り値の総数: 3 (moduleRoot string / verdict Verdict / err error)。
	// 4 個目以降を持つようにシグネチャが拡張された場合、本アサーションが失敗し
	// soft warning ステータスを返す型変更を検出する。
	const wantNumOut = 3
	if got := fnType.NumOut(); got != wantNumOut {
		t.Fatalf(
			"Precheck の戻り値数: got %d, want %d (moduleRoot, verdict, err — soft warning 4 値目を持たない契約)",
			got, wantNumOut,
		)
	}

	// 戻り値[0]: string (moduleRoot)。
	if got := fnType.Out(0); got.Kind() != reflect.String {
		t.Errorf("Precheck の戻り値[0]: got %v (Kind=%v), want string (moduleRoot)", got, got.Kind())
	}

	// 戻り値[1]: improvepatch.Verdict (string-based 列挙型)。
	// Verdict 自身の closed set 不変性は verdict_test.go で検証済み。
	// 本テストは「戻り値[1] が Verdict 型である」ことのみを確認する。
	var verdictZero Verdict
	wantVerdictType := reflect.TypeOf(verdictZero)
	if got := fnType.Out(1); got != wantVerdictType {
		t.Errorf("Precheck の戻り値[1]: got %v, want %v (improvepatch.Verdict)", got, wantVerdictType)
	}

	// 戻り値[2]: error 互換 interface。
	errIface := reflect.TypeOf((*error)(nil)).Elem()
	if got := fnType.Out(2); !got.Implements(errIface) {
		t.Errorf("Precheck の戻り値[2]: got %v は error interface を満たす必要がある", got)
	}
}

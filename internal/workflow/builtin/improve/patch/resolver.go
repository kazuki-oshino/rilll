// Package improvepatch の本ファイルは TargetResolver interface 定義と既定実装
// (DefaultResolver: kebab→snake 変換 + `internal/workflow/<snake>.go` 存在確認) を提供する
// (tasks.md 4.1 / design.md §TargetResolver / Req 1.5, 7.1〜7.4)。
//
// 設計契約 (design.md §TargetResolver):
//   - 純関数 (副作用は対象ファイルの存在確認のみ、`os.Stat` のみ)。
//   - 戻り値 path は cwd 相対ではなく絶対 clean path で返し、後続の Patch / GitOps での扱いを統一する。
//   - MVP 実装は `internal/workflow/<snake>.go` の存在確認まで行い、不在時 ErrTargetNotResolved を返す。
//   - `<snake>.go` 以外の拡張子 (例: `.txt`) にフォールバックしない (Req 7.4)。
//   - future spec (`workflow-definition-externalize`) で resolver を YAML / DSL 解決に差し替えても、
//     interface シグネチャは不変として残す seam (Req 7.3)。
//
// design.md 既出の `NewDefaultResolver(registry []workflow.Definition)` シグネチャからの逸脱:
//   - improvepatch から workflow パッケージを直接 import すると、後続 task 11.x で workflow から
//     improvepatch を import する関係と循環参照になる。
//   - registry 経由の workflow ID 集合 lookup (未知 ID 拒否) は orchestration 側責務であり、
//     tasks.md 11.1 の「workflow.Registry(agent) lookup で未知 ID 拒否」が当該責務を担う。
//   - 本 resolver の責務は「workflow ID → 絶対 Go ファイルパス」の写像のみ (関心分離)。
//   - したがって本実装では NewDefaultResolver() を引数なしとし、registry 渡しは orchestration 層で
//     行う方針 (本ファイルコメントで明示し design.md との差分理由を記録する)。
package patch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// targetWorkflowSubdir は MVP の TargetResolver が改善対象 Go ファイルを探すサブパス。
// `<cwd>/internal/workflow/<snake>.go` 形式の path を構築する際の中間ディレクトリ部分を表す
// (design.md §TargetResolver Implementation Notes / Req 7.2)。
//
// future spec で改善対象 path の解決方法が YAML / DSL に切り替わる際は、本定数の代わりに
// 別 resolver 実装が登録される (Req 7.3 seam)。本パッケージの defaultResolver は MVP 限定。
const targetWorkflowSubdir = "internal/workflow"

// targetGoFileExt は MVP TargetResolver が許容する対象ファイルの拡張子。
// `.go` 以外 (例: `.txt`、TS bridge `.ts` 等) は ErrTargetNotResolved として扱う (Req 7.4)。
const targetGoFileExt = ".go"

// ErrTargetNotResolved は TargetResolver が workflow ID から改善対象ファイルを解決できなかったことを
// 示す sentinel error。orchestration 側はこれを `VerdictPreconditionFailed` verdict にマップする
// (Req 1.5, 7.4 / tasks.md 11.1)。
//
// 本 sentinel は errors.Is を介した検出を可能にするため、パッケージ公開とする。
var ErrTargetNotResolved = errors.New("improvepatch: target file not resolved")

// TargetResolver は workflow ID から改善対象 Go ファイルの絶対パスを返す seam を表す。
//
// Preconditions:
//   - cwd は絶対パス、または relative path (`.` 等) でも実装側で絶対パスに正規化される
//     (本ファイルの defaultResolver 実装では `filepath.Abs` で正規化する)。
//   - workflowID は非空の kebab-case 文字列。
//
// Postconditions:
//   - 戻り値 path は `os.Stat` で存在確認済みの絶対 clean path。
//   - エラー時は path として空文字列を返し、err は ErrTargetNotResolved を errors.Is で
//     検出可能な形で wrap した error を返す (orchestration 側の verdict マッピング契約)。
//
// Invariants:
//   - future spec での差し替え時、interface シグネチャは変更しない (Req 7.3)。
//   - resolver は副作用として `os.Stat` 以外を行わない (純関数性 / design.md §TargetResolver)。
type TargetResolver interface {
	// Resolve は workflow ID (kebab-case) から改善対象 Go ファイルの絶対パスを返す。
	//   - 未知 ID または対象ファイル不在: ErrTargetNotResolved を wrap して返す
	//   - Go ワークフローファイル以外 (拡張子 `.go` 以外): ErrTargetNotResolved (Req 7.4)
	Resolve(ctx context.Context, cwd string, workflowID string) (string, error)
}

// NewDefaultResolver は MVP の workflow ID → `internal/workflow/<snake>.go` パス解決を行う
// 既定 TargetResolver 実装を返す (tasks.md 4.1 / design.md §TargetResolver)。
//
// 本実装は引数を取らない。design.md §TargetResolver の `NewDefaultResolver(registry)` 形からの
// 逸脱理由は本ファイル冒頭コメント参照 (循環参照回避 + 関心分離)。
//
// orchestration 側 (workflow.ImproveWorkflow.Fn / task 11.1) は registry 照合を行ってから
// 本 resolver を呼ぶ責務を持つ。
func NewDefaultResolver() TargetResolver {
	return &defaultResolver{}
}

// defaultResolver は TargetResolver の MVP 既定実装。
// 副作用は `os.Stat` のみで、kebab→snake 変換と path 結合は決定的 (純関数的) に行う。
type defaultResolver struct{}

// Resolve は workflowID (kebab-case) から `<absCwd>/internal/workflow/<snake>.go` を構築し、
// `os.Stat` で存在確認したうえで絶対 clean path を返す。
//
// 失敗経路 (いずれも ErrTargetNotResolved を errors.Is で検出可能な形で wrap して返す):
//   - workflowID が空文字列 (Preconditions 違反扱い、ただし err 経路に統一して返す)
//   - cwd を絶対パスに正規化できない (`filepath.Abs` 失敗)
//   - 構築 path が `.go` 拡張子で終わらない (理論上 MVP では発生しないが Req 7.4 防御的検査)
//   - 構築 path が `os.Stat` で見つからない (未知 ID または .go 以外 fixture のみ存在)
//   - 構築 path が directory として存在 (ファイルでない)
func (r *defaultResolver) Resolve(_ context.Context, cwd string, workflowID string) (string, error) {
	// Preconditions: workflowID は非空。空文字列は MVP では未知 ID と等価扱い (構築 path が `.go` 単独になり
	// 通常存在しないため os.Stat で error 経路に入るが、防御的に明示 reject する)。
	if workflowID == "" {
		return "", fmt.Errorf("%w: workflowID は非空が必須", ErrTargetNotResolved)
	}

	// kebab → snake 変換 (design.md §TargetResolver Implementation Notes: `strings.ReplaceAll(id, "-", "_")`)。
	// 既存 4 ワークフロー名 (`update-readme.go` → `update_readme.go` など) を網羅対応する。
	snake := strings.ReplaceAll(workflowID, "-", "_")

	// cwd を絶対 clean path に正規化する。Preconditions では cwd 絶対パスを推奨するが、
	// 相対パス (`.` 等) で渡されても resolver が決定的に動作するよう自前で正規化する
	// (orchestration 側の moduleRoot 渡しに加えて、テスト容易性も担保)。
	absCwd, err := filepath.Abs(cwd)
	if err != nil {
		// 絶対パス解決自体の失敗 (極めて稀: 例えば cwd が削除済みディレクトリ等) は
		// 「対象未解決」として ErrTargetNotResolved に集約する。
		return "", fmt.Errorf("%w: cwd を絶対パスに正規化できなかった (cwd=%q): %v",
			ErrTargetNotResolved, cwd, err)
	}

	// 構築 path: `<absCwd>/internal/workflow/<snake>.go`。
	// `filepath.Join` は OS 別 separator を扱い、`filepath.Clean` は冗長 `.` / `..` を畳む
	// (Postconditions: 戻り値は絶対 clean path)。
	candidate := filepath.Clean(filepath.Join(absCwd, targetWorkflowSubdir, snake+targetGoFileExt))

	// 防御的検査 (Req 7.4): 構築 path が `.go` 拡張子で終わることを保証する。
	// MVP では `+ targetGoFileExt` で常に true だが、future spec 差し替え時の seam として明示する。
	if filepath.Ext(candidate) != targetGoFileExt {
		return "", fmt.Errorf("%w: 解決対象は %s ファイルのみ (got=%q)",
			ErrTargetNotResolved, targetGoFileExt, candidate)
	}

	// 存在確認: `os.Stat` で対象ファイルの存在を検証する。
	// `<snake>.go` 以外 (例: `<snake>.txt` のみ存在) のケースでは候補 path が存在せず
	// ErrTargetNotResolved 経路に入る (resolver は `.txt` にフォールバックしない契約)。
	info, err := os.Stat(candidate)
	if err != nil {
		// `os.IsNotExist(err)` 等で枝分けせず、すべて ErrTargetNotResolved に集約する
		// (orchestration 側は `precondition_failed` verdict 一本でユーザに通知する契約)。
		return "", fmt.Errorf("%w: workflowID=%q path=%q: %v",
			ErrTargetNotResolved, workflowID, candidate, err)
	}
	if info.IsDir() {
		// 同名 directory が存在するケース (極めて稀) は対象ファイルとして扱わない。
		return "", fmt.Errorf("%w: 対象は file のみ (got directory): workflowID=%q path=%q",
			ErrTargetNotResolved, workflowID, candidate)
	}

	return candidate, nil
}

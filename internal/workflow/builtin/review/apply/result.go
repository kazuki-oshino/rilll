// Package apply の本ファイルは review-apply-workflow の永続化型ファミリ
// (CycleRecord / RecoveryGuidance / SelfReviewRecord / ResultRecord) と
// `<runDir>/result.json` 書き出し helper `WriteResultJSON` を提供する
// (tasks.md 1.3 / design.md §Result / §Parse / Req 5.6, 6.2, 7.1, 7.2, 7.3, 7.4)。
//
// 設計契約:
//   - JSON タグは snake_case で固定。フィールド追加 / 名称変更は本 spec
//     (design.md §Result) の改訂を伴う (Req 7.5 両端契約と整合)。
//   - 任意フィールドは `omitempty` で JSON 出力から省略する。
//     特に `ResultRecord.RecoveryGuidance` は revert_failed / git_apply_failed
//     経路でのみ非 nil となり、利用者向けに「未復旧ファイル発生」のシグナルとして
//     扱われるため、それ以外の経路では JSON 出力に現れてはならない (Req 5.6, 6.2)。
//   - `WriteResultJSON` は `defer` 経路から 1 度だけ呼ばれる前提
//     (design.md §Result §Persistence & consistency)。多重書き出しは
//     最終 verdict を揺らすリスクがあるため Workflow 層 (task 5.1) で 1 回に固定する。
//   - `FinalVerdict == VerdictUnset` の record を渡された場合は **書き出し前に**
//     error を返し、ファイル / artifact のいずれにも痕跡を残さない不変式を守る
//     (Req 7.4 / design.md §Verdict Invariants)。`recover()` 経路で未設定 verdict を
//     書き出す呼び出し側は `VerdictFailed` 等への fallback (task 5.1) を経てから
//     本 helper を呼ぶ責務を持つ。
//   - `RecordRunArtifact(ArtifactKindResult, ...)` の失敗は呼び出し側 (task 5.1 defer)
//     が握りつぶせるよう error として返す (akashic-artifacts spec Req 2.6 と整合)。
package apply

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/kazuki-oshino/rilll/internal/akashic"
	"github.com/kazuki-oshino/rilll/internal/workflow"
)

// resultJSONFile は `<runDir>/result.json` のファイル名。
// design.md §Physical Data Model 規定 (Req 7.2) と整合する固定値。
const resultJSONFile = "result.json"

// SelfReviewVerdict は self-review LLM が返す closed set 二値判定の文字列型
// (design.md §Parse / task 3.1 の JSONSchemaForSelfReview の `enum` 制約と同形)。
//
// `accept` / `regenerate` 以外の値は `ErrSelfReviewSchemaViolation` (task 3.2)
// で拒否される契約のため、本型では const 2 値のみを許容範囲とする。
type SelfReviewVerdict string

const (
	// SelfReviewAccept は self-review LLM が修正 diff を承認した状態。
	// orchestration (task 5.3) は本値で apply フェーズへ遷移する。
	SelfReviewAccept SelfReviewVerdict = "accept"

	// SelfReviewRegenerate は self-review LLM が再生成を要求した状態。
	// orchestration は MaxRegenerateAttempts (task 5.3) に達するまで attempt++ で再試行する。
	SelfReviewRegenerate SelfReviewVerdict = "regenerate"
)

// SelfReviewRecord は self-review レスポンスの正規化済み形 (design.md §Parse)。
// `cycle-N-review.json` artifact (kind=`review`) としても利用される (Req 4.2 / 7.1)。
//
// JSON タグは snake_case 固定。Verdict は SelfReviewVerdict の closed set 二値、
// Reason は LLM が返した自由記述、Attempt は 1 始まりの cycle 番号、
// DurationMs は agent 呼び出しに要したミリ秒。
type SelfReviewRecord struct {
	// Verdict は self-review の二値判定 (`accept` / `regenerate`)。
	// 範囲外値は ParseSelfReview (task 3.2) 段階で `ErrSelfReviewSchemaViolation`
	// として弾かれるため、本フィールドの値は closed set 内に揃う契約。
	Verdict SelfReviewVerdict `json:"verdict"`

	// Reason は LLM が返した判定理由の自由記述 (改行を含み得る)。
	Reason string `json:"reason"`

	// Attempt は 1 始まりの cycle 番号 (CycleRecord.Cycle と一致する契約)。
	// invariant test (task 5.6) で連番性を検証する。
	Attempt int `json:"attempt"`

	// DurationMs は self-review LLM 呼び出しに要したミリ秒 (akashic 集計用)。
	DurationMs int64 `json:"duration_ms"`
}

// CycleRecord は 1 cycle (plan → diff → self-review → 必要なら apply) の
// 集約レコード (design.md §Result / Req 7.1)。
//
// 1 cycle = 1 plan + (任意) 1 diff + (任意) 1 self-review + (任意) 1 apply の単位。
// 中間 verdict (`proposal_unparseable` / `out_of_scope_diff` / `failed`) や
// `accept` 経路の `applied` / `git_apply_failed` 等を記録する。
//
// JSON タグは snake_case 固定。Cycle / PlanArtifact / Verdict / DurationMs は必須、
// それ以外 (DiffArtifact / ReviewArtifact / AppliedFiles / SelfReview) は cycle が
// 該当フェーズに到達したかに依存して埋まるため `omitempty` で省略可とする。
type CycleRecord struct {
	// Cycle は 1 始まりの cycle 番号 (improve-workflow と同形の付番)。
	Cycle int `json:"cycle"`

	// PlanArtifact は `cycle-N-plan.md` の相対パス (run ディレクトリ起点)。
	// plan 段階を必ず通過するため必須フィールドとする (Req 3.1)。
	PlanArtifact string `json:"plan_artifact"`

	// DiffArtifact は `cycle-N-diff.patch` の相対パス。
	// diff 生成前に cycle が早期終端した場合 (例: plan LLM 失敗) は空。
	DiffArtifact string `json:"diff_artifact,omitempty"`

	// ReviewArtifact は `cycle-N-review.json` の相対パス。
	// self-review 呼び出し前に cycle が早期終端した場合は空。
	ReviewArtifact string `json:"review_artifact,omitempty"`

	// AppliedFiles は本 cycle で `git apply` 成功した patch 内 file の相対パス集合。
	// apply フェーズ未到達 (`out_of_scope_diff` / `proposal_unparseable` / `regenerate` 等)
	// では空 / nil となる。
	AppliedFiles []string `json:"applied_files,omitempty"`

	// Verdict は本 cycle の中間 verdict (`applied` / `regenerate` 系 / `failed` 系)。
	// 最終 verdict は ResultRecord.FinalVerdict が別途集約する (1 cycle = 1 中間 verdict)。
	Verdict Verdict `json:"verdict"`

	// SelfReview は本 cycle の self-review 結果。self-review 未呼出経路では nil。
	SelfReview *SelfReviewRecord `json:"self_review,omitempty"`

	// DurationMs は本 cycle 全体の所要時間 (ミリ秒)。
	DurationMs int64 `json:"duration_ms"`
}

// RecoveryGuidance は revert 失敗時の手動復旧情報 (design.md §Result / Req 5.6, 6.2)。
//
// `revert_failed` / commit 失敗 + revert 失敗の経路で非 nil となり、
// 利用者向け表示 (`WorkflowText`) と `result.json` の双方に同内容を出力する
// (task 5.5)。それ以外の経路では nil で `omitempty` により JSON から省略される。
//
// JSON タグは snake_case 固定。Files / Commands は必須 (空 slice であっても
// 「未復旧ファイル なし」を明示するため出力する)、Note のみ任意。
type RecoveryGuidance struct {
	// Files は revert 失敗で baseline まで戻せなかった相対パス集合。
	Files []string `json:"files"`

	// Commands は手動復旧用コマンド列 (例: `git checkout <baseline> -- <file>`)。
	Commands []string `json:"commands"`

	// Note は補足説明 (任意)。空のときは `omitempty` で省略する。
	Note string `json:"note,omitempty"`
}

// ResultRecord は `<runDir>/result.json` の最終形 (design.md §Result / Req 7.2)。
//
// JSON タグは snake_case 固定。フィールド追加 / 名称変更は本 spec (design.md §Result)
// の改訂を伴う (Req 7.5)。
//
// 任意フィールドは `omitempty` で省略する:
//   - SkippedRuleSummary: `findings_reported` 以外の rule 集計 (なければ空)。
//   - BaselineCommit: Snapshot 経路通過時のみ。
//   - CommitSHA: `applied` のときのみ。
//   - RecoveryGuidance: revert 失敗 / commit 失敗 + revert 失敗の経路でのみ
//     (task 5.5 が出力)。利用者表示で「未復旧ファイル発生」のシグナルとして
//     使われるため、それ以外の経路では JSON に現れてはならない (Req 5.6, 6.2)。
//
// 必須フィールド (FinalVerdict / FinalSummary 等) は zero 値であっても出力する。
// 特に FinalVerdict は VerdictUnset (空文字) を JSON に書き出してはならない不変式
// (Req 7.4 / design.md §Verdict Invariants) を `WriteResultJSON` で構造的に守る。
type ResultRecord struct {
	// WorkflowRunID は本 apply run の workflow_run_id (akashic 経由の連結 key)。
	WorkflowRunID string `json:"workflow_run_id"`

	// ReviewRunID は入力 review run の workflow_run_id。
	// Sequential 経路では前段 review.Workflow から ctx 経由で受領 (task 6.1)、
	// 単独経路では `--review-run-id` arg で指定される (task 5.1)。
	ReviewRunID string `json:"review_run_id"`

	// StartedAt は run 開始 timestamp (RFC3339)。
	StartedAt string `json:"started_at"`

	// FinishedAt は run 終了 timestamp (RFC3339)。
	FinishedAt string `json:"finished_at"`

	// FinalVerdict は closed set 10 値の最終 verdict (verdict.go)。
	// VerdictUnset を渡された場合 `WriteResultJSON` は error を返す不変式 (Req 7.4)。
	FinalVerdict Verdict `json:"final_verdict"`

	// FinalSummary は単一行 (改行を含まない) のサマリ。
	// `IsIntent() == false` のとき `workflow_runs.error` の suffix として
	// `[verdict:<id>] <summary>` 形で利用される (Req 6.3 / verdict.go ErrorPrefixForVerdict)。
	FinalSummary string `json:"final_summary"`

	// TargetRuleNames は修正対象とした rule 名の和集合 (空集合可)。
	TargetRuleNames []string `json:"target_rule_names"`

	// SkippedRuleSummary は `findings_reported` 以外の rule の verdict 別件数。
	// 空のときは `omitempty` で省略する (Req 1.5 利用者表示と整合)。
	SkippedRuleSummary map[string]int `json:"skipped_rule_summary,omitempty"`

	// Cycles は run 中に実行された全 cycle の集約 (1 始まり連番、空集合可)。
	Cycles []CycleRecord `json:"cycles"`

	// BaselineCommit は Snapshot 経路通過時の baseline `git rev-parse HEAD` (40 文字 SHA)。
	// apply 未到達経路では空文字で `omitempty` により省略される。
	BaselineCommit string `json:"baseline_commit,omitempty"`

	// CommitSHA は `applied` 経路で生成された commit の SHA。
	// それ以外の経路では空文字で `omitempty` により省略される。
	CommitSHA string `json:"commit_sha,omitempty"`

	// RecoveryGuidance は revert 失敗時の手動復旧情報。
	// `revert_failed` / commit 失敗 + revert 失敗 経路でのみ非 nil。
	// それ以外の経路では nil で `omitempty` により省略される (Req 5.6, 6.2)。
	RecoveryGuidance *RecoveryGuidance `json:"recovery_guidance,omitempty"`
}

// ErrFinalVerdictUnset は `WriteResultJSON` が `FinalVerdict == VerdictUnset` を
// 拒否したときに返す sentinel。errors.Is で判別可能とする
// (design.md §Verdict Invariants / Req 7.4)。
//
// 呼び出し側 (task 5.1 defer) はこの error を観測した場合、`recover()` 経路と
// 同様に `VerdictFailed` (内部失敗) へ fallback してから再度 WriteResultJSON を
// 呼び出す責務を負う (本 helper では fallback しない: SRP)。
var ErrFinalVerdictUnset = errors.New("apply: ResultRecord.FinalVerdict is VerdictUnset")

// ErrNilResultRecord は `WriteResultJSON` が record == nil を拒否したときに返す sentinel。
// defer 経路で集約 state ポインタを nil にしてしまう実装ミスを構造的に検出するための
// 防御的契約 (orchestration 5.1 から呼ばれる際の nil pointer dereference 防止)。
var ErrNilResultRecord = errors.New("apply: WriteResultJSON: record is nil")

// WriteResultJSON は `<runDir>/result.json` に ResultRecord を書き出し、
// `workflow.RecordRunArtifact(ctx, akashic.ArtifactKindResult, path)` で
// akashic 経由の artifact 登録を行う (design.md §Result §State Management)。
//
// 処理順序 (厳守):
//  1. record == nil 防御 → ErrNilResultRecord
//  2. record.FinalVerdict == VerdictUnset 不変式 → ErrFinalVerdictUnset
//     (ファイル書き出し / artifact 登録のいずれも実行しない)
//  3. json.MarshalIndent で 2 空白インデント JSON 化
//  4. os.WriteFile で `<runDir>/result.json` に書き出し
//  5. workflow.RecordRunArtifact(ArtifactKindResult, path) で akashic 登録
//
// step 4 / 5 の失敗はラップして error として返す。akashic-artifacts spec Req 2.6 の
// 「登録失敗をワークフロー結果に伝播させない」契約は呼び出し側 (task 5.1 defer) が
// `_ = WriteResultJSON(...)` で握りつぶすことで満たす (本 helper では握りつぶさない)。
//
// runDir は `<cwd>/.rilll/review-apply-workflow/<workflowRunID>` を期待する
// (design.md §Workflow §runDir resolution)。runDir の存在は呼び出し側責務とし、
// 本 helper は MkdirAll しない (task 5.1 が precheck より前に runDir を確保する契約)。
func WriteResultJSON(ctx context.Context, runDir string, record *ResultRecord) error {
	// 1. nil 防御 (defer 経路で集約 state ポインタを忘れた実装ミスを早期検出)。
	if record == nil {
		return ErrNilResultRecord
	}

	// 2. VerdictUnset 不変式 (Req 7.4 / design.md §Verdict Invariants)。
	//    ファイル書き出し / artifact 登録のいずれも実行しない (痕跡を残さない)。
	if record.FinalVerdict == VerdictUnset {
		return fmt.Errorf("apply: WriteResultJSON: %w (recover/fallback 経路では VerdictFailed 等にセット後に再呼出)", ErrFinalVerdictUnset)
	}

	// 3. JSON 化 (人間可読の 2 空白インデント、improve/workflow.go と同形)。
	body, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return fmt.Errorf("apply: WriteResultJSON: marshal: %w", err)
	}

	// 4. ファイル書き出し (`<runDir>/result.json`)。0o644 は improve と同 perm。
	path := filepath.Join(runDir, resultJSONFile)
	if err := os.WriteFile(path, body, 0o644); err != nil {
		return fmt.Errorf("apply: WriteResultJSON: write %s: %w", path, err)
	}

	// 5. akashic への artifact 登録 (kind=result)。
	//    失敗時はラップして返し、呼び出し側 (defer) が握りつぶすかを判断する。
	if err := workflow.RecordRunArtifact(ctx, akashic.ArtifactKindResult, path); err != nil {
		return fmt.Errorf("apply: WriteResultJSON: record artifact: %w", err)
	}
	return nil
}

// Package improvepatch の本ファイルは context.json artifact の正規型ファミリ
// (ContextRecord / SampleRunSummary / BuildInputSummary) と、PromptContext +
// BuildInput から ContextRecord を構築する `BuildContextRecord` helper を提供する
// (tasks.md 9.2 / design.md §Data Contracts & Integration §8 / Resolved Decisions §8)。
//
// 設計契約:
//   - JSON タグは snake_case で固定。フィールド追加時は本 spec の改訂を伴う
//     (design.md §8 / Req 2.1, 2.4, 2.5)。
//   - `current_source_sha256` は `CurrentSource` 全文の SHA-256 hex (64 文字)。
//     改善 patch が生成された当時のソース同一性を後から再検証可能にする。
//     本体バイト列は context.json には**載せない** (artifact サイズ肥大化抑止)。
//   - `sample_runs[]` はサマリのみ (agent_call_count / raw_event_count / artifact_count)。
//     `RawEvents` / `Artifacts.Body` 本体は載せない。詳細追跡が必要な run は
//     `run_id` で `akashic-query` Reader 経由で再取得する (Req 2.2, 2.3)。
//   - `build_input_at_capture` は再現性確保用。サイズ抑制パラメータ
//     (`MaxRawEvents` / `MaxArtifactBytes`) を後から確認できるようにする (Req 2.4)。
//
// orchestration (task 11.2) との関係:
//   - 本ファイルは `kind = context` artifact として `RecordRunArtifact(ContextKind, ...)`
//     で 1 件のみ登録される。試行毎には登録せず、context 構築直後に 1 度だけ書き出す。
package patch

import (
	"crypto/sha256"
	"encoding/hex"
	"time"
)

// ContextRecord は context.json の最低限の型契約 (Req 2.1〜2.5 / Resolved Decisions §8)。
// JSON タグは snake_case で固定し、フィールド追加時は本 spec の改訂を要する。
type ContextRecord struct {
	// WorkflowRunID は本 improve-workflow run の workflow_run_id。
	WorkflowRunID string `json:"workflow_run_id"`

	// TargetWorkflowID は改善対象の workflow ID (例: "git-commit")。
	TargetWorkflowID string `json:"target_workflow_id"`

	// TargetGoFile は改善対象 Go ファイルの絶対 clean path。
	TargetGoFile string `json:"target_go_file"`

	// RunIDs は hand-off で受領した起点 run ID 配列 (順序保持、MVP では length=1)。
	RunIDs []string `json:"run_ids"`

	// CurrentSourceSHA256 は CurrentSource 全文の SHA-256 hex 64 文字。
	// 本体バイト列は context.json に載せず、ソース同一性の検証だけを保証する。
	CurrentSourceSHA256 string `json:"current_source_sha256"`

	// CurrentSourceBytes は改善対象ファイルのバイト数 (サイズ確認用)。
	CurrentSourceBytes int64 `json:"current_source_bytes"`

	// SampleRuns は補完サンプリング結果のサマリ (本体は載せない)。
	SampleRuns []SampleRunSummary `json:"sample_runs"`

	// FailureClustersCount は FailureClusters の件数。
	// 詳細は raw_events に紐づく artifact 経由で取得する。
	FailureClustersCount int `json:"failure_clusters_count"`

	// Limited は起点 run のみだったとき true (Req 2.5)。
	Limited bool `json:"limited"`

	// BuildInputAtCapture は構築時の BuildInput 主要パラメータ (再現性確保)。
	BuildInputAtCapture BuildInputSummary `json:"build_input_at_capture"`

	// CapturedAt は context 構築時刻 (RFC3339Nano、UTC)。
	CapturedAt string `json:"captured_at"`
}

// SampleRunSummary は SampleRuns[] の各要素サマリ (artifact body は載せない)。
//
// design.md §8 の例:
//
//	{"run_id": "string", "success": false, "agent_call_count": 3,
//	 "raw_event_count": 50, "artifact_count": 2, "is_originated_from_handoff": true}
type SampleRunSummary struct {
	// RunID は対象 run の workflow_run_id。
	RunID string `json:"run_id"`

	// Success は run の成功可否を bool 化したもの。
	// 元 RunHeader.Success は *bool だが、context.json schema では bool で固定する。
	// nil (= 進行中) は false として記録する。
	Success bool `json:"success"`

	// AgentCallCount は SampleRun.AgentCalls の件数。
	AgentCallCount int `json:"agent_call_count"`

	// RawEventCount は SampleRun.RawEvents の件数 (MaxRawEvents 抑制後)。
	RawEventCount int `json:"raw_event_count"`

	// ArtifactCount は GetArtifacts で確認した件数。
	ArtifactCount int `json:"artifact_count"`

	// IsOriginatedFromHandoff は hand-off で受け取った起点 run か否か。
	IsOriginatedFromHandoff bool `json:"is_originated_from_handoff"`
}

// BuildInputSummary は BuildInput の主要パラメータを記録 (再現性 + サイズ抑制設定の事後検証用)。
//
// 本サマリには WithDefaults() 適用後の値を入れる契約。これにより ContextRecord 単独で
// 「実行時に何の defaults が適用されたか」を読み取れる (再現性確保)。
type BuildInputSummary struct {
	// SampleSuccess は補完サンプリングで取得する直近成功 run 件数 (デフォルト 3)。
	SampleSuccess int `json:"sample_success"`

	// SampleFailure は補完サンプリングで取得する直近失敗 run 件数 (デフォルト 5)。
	SampleFailure int `json:"sample_failure"`

	// MaxRawEvents は run あたり raw_events 取得上限 (デフォルト 50)。
	MaxRawEvents int `json:"max_raw_events"`

	// MaxArtifactBytes は artifact 1 件あたり読み込み上限 (デフォルト 10 MiB)。
	MaxArtifactBytes int64 `json:"max_artifact_bytes"`

	// MaxRegenerateAttempts は regenerate ループの hard cap (デフォルト 5)。
	MaxRegenerateAttempts int `json:"max_regenerate_attempts"`
}

// BuildContextRecord は PromptContext + BuildInput から ContextRecord を構築する。
//
// 動作:
//  1. workflowRunID / TargetWorkflowID / TargetGoFile / RunIDs を入力からそのまま転記。
//  2. CurrentSourceSHA256 = sha256(prompt.CurrentSource) hex 64 文字、
//     CurrentSourceBytes = len(prompt.CurrentSource) を計算。
//  3. SampleRuns[] を SampleRunSummary[] にサマリ化 (本体非格納)。
//     - Run.Success (*bool) は nil → false としてフラット化。
//     - AgentCallCount / RawEventCount / ArtifactCount は各 slice length。
//  4. FailureClustersCount = len(prompt.FailureClusters)。
//  5. Limited = prompt.Limited (Req 2.5 をそのまま伝播)。
//  6. BuildInputAtCapture は in.WithDefaults() 後の主要パラメータを記録 (再現性確保)。
//  7. CapturedAt は capturedAt.UTC() を RFC3339Nano で文字列化。
//
// Preconditions:
//   - workflowRunID は本 improve-workflow run の ID (空文字許容、orchestration 側で埋める)。
//   - prompt は ContextBuilder.Build の戻り値 (CurrentSource は最低 1 byte 想定だが空でも動作する)。
//   - in は ContextBuilder.Build に渡された BuildInput (defaults 補完前/後どちらでも可)。
//   - capturedAt は zero value 不可 (orchestration 側で `time.Now()` を渡す契約)。
//
// Postconditions:
//   - 戻り値の CurrentSourceSHA256 は常に 64 文字の hex 文字列 (空入力でも sha256 は固定 hex を返す)。
//   - SampleRuns の長さは prompt.SampleRuns と一致する。
//   - BuildInputAtCapture には >0 の値のみが入る (WithDefaults 後)。
func BuildContextRecord(workflowRunID string, prompt PromptContext, in BuildInput, capturedAt time.Time) ContextRecord {
	// (2) sha256 hex を計算
	sum := sha256.Sum256([]byte(prompt.CurrentSource))
	sourceHex := hex.EncodeToString(sum[:])

	// (3) sample_runs[] サマリ化
	summaries := make([]SampleRunSummary, 0, len(prompt.SampleRuns))
	for _, sr := range prompt.SampleRuns {
		// Run.Success は *bool。nil の場合は false (進行中) として記録する契約。
		success := sr.Run.Success != nil && *sr.Run.Success
		summaries = append(summaries, SampleRunSummary{
			RunID:                   sr.Run.RunID,
			Success:                 success,
			AgentCallCount:          len(sr.AgentCalls),
			RawEventCount:           len(sr.RawEvents),
			ArtifactCount:           len(sr.Artifacts),
			IsOriginatedFromHandoff: sr.IsOriginatedFromHandoff,
		})
	}

	// (6) build_input_at_capture は WithDefaults() 後の値を記録 (再現性確保)
	resolved := in.WithDefaults()

	return ContextRecord{
		WorkflowRunID:        workflowRunID,
		TargetWorkflowID:     prompt.TargetWorkflowID,
		TargetGoFile:         prompt.TargetGoFile,
		RunIDs:               in.RunIDs,
		CurrentSourceSHA256:  sourceHex,
		CurrentSourceBytes:   int64(len(prompt.CurrentSource)),
		SampleRuns:           summaries,
		FailureClustersCount: len(prompt.FailureClusters),
		Limited:              prompt.Limited,
		BuildInputAtCapture: BuildInputSummary{
			SampleSuccess:         resolved.SampleSuccess,
			SampleFailure:         resolved.SampleFailure,
			MaxRawEvents:          resolved.MaxRawEvents,
			MaxArtifactBytes:      resolved.MaxArtifactBytes,
			MaxRegenerateAttempts: resolved.MaxRegenerateAttempts,
		},
		CapturedAt: capturedAt.UTC().Format(time.RFC3339Nano),
	}
}

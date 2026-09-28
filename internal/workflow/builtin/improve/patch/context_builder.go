// Package improvepatch の本ファイルは ContextBuilder interface 定義と既定実装
// (DefaultContextBuilder: akashic.Reader / akashic.ArtifactReader を集約して
// PromptContext を構築する) を提供する
// (tasks.md 8.1 / design.md §ContextBuilder / Req 2.1, 2.2, 2.3, 2.4, 2.5, 2.6)。
//
// 設計契約 (design.md §ContextBuilder):
//   - 副作用: akashic.Reader.{Detail, List, FailureClusters} と
//     akashic.ArtifactReader.{GetArtifacts, ReadArtifact} のみ呼び出し、独自に SQL を発行しない
//     (Req 2.2, 2.3)。
//   - .rilll/ 配下を直接読み取らない (ArtifactReader 経由のみ)。
//   - CurrentSource の取得経路として TargetGoFile を `os.ReadFile` で直接読む。これは改善対象 Go
//     ソース (`internal/workflow/<name>.go`) であり .rilll/ 領域ではないため Invariants の
//     「.rilll/ 直接読み禁止」とは独立した正規の経路。
//   - Reader.Detail の RawEventsQuery.Limit を MaxRawEvents に絞り raw_events 全件投入を抑制 (Req 2.4)。
//   - ArtifactReader.GetArtifacts → サイズ ≤ MaxArtifactBytes のもののみ ReadArtifact (Req 2.4)。
//   - 起点 run のみで補完サンプルが取れなかったとき Limited=true (Req 2.5)。
//   - Reader / ArtifactReader 失敗時は ErrContextLoad を errors.Is で chain 検出可能な形に wrap して返す
//     (Req 2.6 / orchestration 側で `context_load_failed` verdict にマップ)。
//
// 既定値 (design.md §ContextBuilder Implementation Notes / Performance & Scalability):
//   - DefaultSampleSuccess = 3 / DefaultSampleFailure = 5
//   - DefaultMaxRawEvents = 50 (run あたり raw_events 取得上限)
//   - DefaultMaxRegenerateAttempts = 5 (Resolved Decisions §2 / inquiryloop と整合)
//   - MaxArtifactBytes default は akashic.DefaultArtifactReadMaxBytes (10 MiB) を流用。
//
// orchestration 統合 (task 11.2):
//   - ImproveWorkflow.Fn は precheck 通過後に DefaultContextBuilder.Build を呼び、
//     PromptContext を 00-context.json として ArtifactKindContext で先に登録する。
package patch

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/kazuki-oshino/rilll/internal/akashic"
)

// 既定値 (design.md §ContextBuilder)。
const (
	// DefaultSampleSuccess は BuildInput.SampleSuccess のゼロ値補完に使われる既定値。
	DefaultSampleSuccess = 3

	// DefaultSampleFailure は BuildInput.SampleFailure のゼロ値補完に使われる既定値。
	// 失敗 run を成功 run より多めに取得することで Req 2.4 の「失敗 run / エラー文を優先サンプリング」を実現する。
	DefaultSampleFailure = 5

	// DefaultMaxRawEvents は BuildInput.MaxRawEvents のゼロ値補完に使われる既定値。
	// run あたり raw_events を 50 件に絞り AI 入力サイズ抑制 (Req 2.4) と失敗エラー網羅性のバランスを取る。
	DefaultMaxRawEvents = 50

	// DefaultMaxRegenerateAttempts は BuildInput.MaxRegenerateAttempts のゼロ値補完に使われる既定値。
	// 5 は Resolved Decisions §2 で確定した上限値で、`internal/inquiryloop/engine.go::defaultMaxRounds` と
	// 完全に整合する (AI コスト保護 / 終了保証契約)。
	DefaultMaxRegenerateAttempts = 5
)

// ErrContextLoad は akashic.Reader / akashic.ArtifactReader 経由の context 取得が失敗したことを
// 示す sentinel error (Req 2.6)。orchestration 側はこれを `context_load_failed` verdict に
// マップする契約 (design.md §Error Categories and Responses)。
//
// 本 sentinel は errors.Is を介した検出を可能にするため、パッケージ公開とする。
// 失敗の原因 error は fmt.Errorf("%w: ... %w", ErrContextLoad, cause) 形で wrap し、
// 呼び出し側は errors.Is(err, ErrContextLoad) と errors.Is(err, cause) の両方で検出できる。
var ErrContextLoad = errors.New("improvepatch: failed to load context")

// BuildInput は ContextBuilder.Build に渡す入力パラメータ束 (design.md §ContextBuilder)。
//
// Preconditions:
//   - TargetWorkflowID は非空の kebab-case workflow ID (例: "git-commit")。
//   - RunIDs は length ≥ 1 (起点 run 必須)、RunIDs[0] が起点として使われる (Req 1.3)。
//   - TargetGoFile は TargetResolver.Resolve 済みの絶対 clean path。
//
// ゼロ値フィールドは WithDefaults() を介して既定値に補完される (Build 内で自動適用)。
type BuildInput struct {
	// TargetWorkflowID は改善対象 workflow の kebab-case ID。
	TargetWorkflowID string

	// RunIDs は hand-off で受領した起点 run ID 配列 (順序保持)。
	// MVP では RunIDs[0] のみを起点として使用するが、将来の length>1 拡張に対しても破壊しない形で受領する (Req 1.3)。
	RunIDs []string

	// TargetGoFile は改善対象 Go ファイルの絶対 clean path (TargetResolver.Resolve の戻り値)。
	TargetGoFile string

	// SampleSuccess は補完サンプリングで取得する直近成功 run 件数 (デフォルト DefaultSampleSuccess=3)。
	SampleSuccess int

	// SampleFailure は補完サンプリングで取得する直近失敗 run 件数 (デフォルト DefaultSampleFailure=5)。
	SampleFailure int

	// MaxRawEvents は run あたり raw_events 取得上限 (デフォルト DefaultMaxRawEvents=50、Req 2.4)。
	MaxRawEvents int

	// MaxArtifactBytes は artifact 1 件あたり読み込み上限 (デフォルト akashic.DefaultArtifactReadMaxBytes=10 MiB)。
	// これを超えるサイズの artifact は ReadArtifact を呼ばず、ArtifactSnippet.Body は空 byte slice のまま残す。
	MaxArtifactBytes int64

	// MaxRegenerateAttempts は regenerate ループの hard cap (デフォルト DefaultMaxRegenerateAttempts=5、
	// Resolved Decisions §2)。
	// 本フィールドは ContextBuilder では使わないが、ContextRecord (task 9.2) で再現性確保用に
	// build_input_at_capture へ記録するため、入力パラメータ束に含める。
	MaxRegenerateAttempts int
}

// WithDefaults は BuildInput のゼロ値フィールドに既定値を補完した新しい BuildInput を返す。
// 入力 BuildInput 自体は変更しない (immutable 風 helper)。
//
// 補完規則 (各フィールドが <= 0 のとき):
//   - SampleSuccess → DefaultSampleSuccess (3)
//   - SampleFailure → DefaultSampleFailure (5)
//   - MaxRawEvents → DefaultMaxRawEvents (50)
//   - MaxArtifactBytes → akashic.DefaultArtifactReadMaxBytes (10 MiB)
//   - MaxRegenerateAttempts → DefaultMaxRegenerateAttempts (5)
//
// TargetWorkflowID / RunIDs / TargetGoFile は precondition のため補完しない (空値はエラー)。
func (in BuildInput) WithDefaults() BuildInput {
	out := in
	if out.SampleSuccess <= 0 {
		out.SampleSuccess = DefaultSampleSuccess
	}
	if out.SampleFailure <= 0 {
		out.SampleFailure = DefaultSampleFailure
	}
	if out.MaxRawEvents <= 0 {
		out.MaxRawEvents = DefaultMaxRawEvents
	}
	if out.MaxArtifactBytes <= 0 {
		out.MaxArtifactBytes = akashic.DefaultArtifactReadMaxBytes
	}
	if out.MaxRegenerateAttempts <= 0 {
		out.MaxRegenerateAttempts = DefaultMaxRegenerateAttempts
	}
	return out
}

// ContextBuilder は akashic.Reader / akashic.ArtifactReader を集約して PromptContext を構築する seam を表す。
//
// 副作用:
//   - akashic.Reader.{Detail, List, FailureClusters} (Req 2.2)
//   - akashic.ArtifactReader.{GetArtifacts, ReadArtifact} (Req 2.3, 2.4)
//   - os.ReadFile(TargetGoFile) で改善対象 Go ソースを取得 (CurrentSource)
//
// orchestration 側 (task 11.2) は本 interface 経由でのみ context 構築を行い、
// .rilll/ 配下を直接読み取ってはならない (Req 2.3)。
type ContextBuilder interface {
	// Build は BuildInput を入力に PromptContext を返す。
	// 失敗時は ErrContextLoad を errors.Is で chain 検出可能な形で wrap して返す (Req 2.6)。
	Build(ctx context.Context, in BuildInput) (PromptContext, error)
}

// DefaultContextBuilder は ContextBuilder の MVP 既定実装。
// akashic.Reader と akashic.ArtifactReader を依存として保持し、Build で集約する。
//
// 別実装 (例: future spec での lower-cost ContextBuilder) に差し替える際は、本構造体ではなく
// ContextBuilder interface を seam として利用する。
type DefaultContextBuilder struct {
	reader    akashic.Reader
	artifacts akashic.ArtifactReader
}

// NewDefaultContextBuilder は DefaultContextBuilder のコンストラクタ。
// 戻り値 *DefaultContextBuilder は ContextBuilder interface を満たす
// (`var _ ContextBuilder = (*DefaultContextBuilder)(nil)` で本ファイル末尾に検証)。
//
// reader / artifacts のいずれかが nil の場合は Build 呼び出し時点で panic ではなく
// ErrContextLoad で fail-fast する (zero value 経由の不正使用を早期に検出)。
func NewDefaultContextBuilder(reader akashic.Reader, artifacts akashic.ArtifactReader) *DefaultContextBuilder {
	return &DefaultContextBuilder{reader: reader, artifacts: artifacts}
}

// Build は BuildInput を入力に PromptContext を構築する。設計詳細は本ファイル先頭コメントを参照。
//
// 構築フロー:
//  1. WithDefaults でゼロ値補完
//  2. RunIDs precondition 検証
//  3. Reader.Detail で起点 run の Detail (raw_events Limit=MaxRawEvents) を取得
//  4. os.ReadFile(TargetGoFile) で CurrentSource を取得
//  5. Reader.List で直近 SampleSuccess 件成功 / SampleFailure 件失敗の補完サンプル取得
//  6. Reader.FailureClusters で失敗カテゴリ一覧取得
//  7. 各 sample run について ArtifactReader.GetArtifacts → ReadArtifact (size guard 適用) で
//     ArtifactSnippet を構築
//  8. 補完サンプルが 1 件も取れなかったとき Limited=true (Req 2.5)
//
// すべての副作用呼び出しは error 発生時に ErrContextLoad で wrap して返す (Req 2.6)。
func (b *DefaultContextBuilder) Build(ctx context.Context, in BuildInput) (PromptContext, error) {
	in = in.WithDefaults()

	// (2) RunIDs precondition 検証 (Req 1.3 / design.md §ContextBuilder Preconditions)
	if len(in.RunIDs) == 0 {
		return PromptContext{}, fmt.Errorf("%w: BuildInput.RunIDs is empty (precondition: length >= 1)", ErrContextLoad)
	}
	originID := in.RunIDs[0]

	// (3) origin run の Detail (raw_events Limit=MaxRawEvents で抑制)
	originDetail, err := b.reader.Detail(ctx, akashic.DetailOptions{
		WorkflowRunID: originID,
		RawEvents:     akashic.RawEventsQuery{Limit: in.MaxRawEvents},
	})
	if err != nil {
		return PromptContext{}, fmt.Errorf("%w: Reader.Detail(origin=%s): %w", ErrContextLoad, originID, err)
	}

	// (4) CurrentSource (改善対象 Go ファイル全文)
	sourceBytes, err := os.ReadFile(in.TargetGoFile)
	if err != nil {
		return PromptContext{}, fmt.Errorf("%w: os.ReadFile(target=%s): %w", ErrContextLoad, in.TargetGoFile, err)
	}

	// (5) 補完サンプリング (success / failure)
	successFlag := true
	failureFlag := false
	successList, err := b.reader.List(ctx, akashic.ListOptions{
		WorkflowID: in.TargetWorkflowID,
		Success:    &successFlag,
		Limit:      in.SampleSuccess,
	})
	if err != nil {
		return PromptContext{}, fmt.Errorf("%w: Reader.List(success): %w", ErrContextLoad, err)
	}
	failureList, err := b.reader.List(ctx, akashic.ListOptions{
		WorkflowID: in.TargetWorkflowID,
		Success:    &failureFlag,
		Limit:      in.SampleFailure,
	})
	if err != nil {
		return PromptContext{}, fmt.Errorf("%w: Reader.List(failure): %w", ErrContextLoad, err)
	}

	// (6) FailureClusters
	failureClustersResult, err := b.reader.FailureClusters(ctx, akashic.FailureOptions{
		WorkflowID: in.TargetWorkflowID,
	})
	if err != nil {
		return PromptContext{}, fmt.Errorf("%w: Reader.FailureClusters: %w", ErrContextLoad, err)
	}

	// (7) SampleRuns 構築 (origin 先頭 + samples 続く)
	sampleRuns := make([]SampleRun, 0, 1+len(successList.Items)+len(failureList.Items))

	// 起点 run sample (IsOriginatedFromHandoff = true)
	originSample, err := b.buildSampleRun(ctx, originDetail.Run, originDetail.AgentCalls, originDetail.RawEvents, in.MaxArtifactBytes, true)
	if err != nil {
		return PromptContext{}, err
	}
	sampleRuns = append(sampleRuns, originSample)

	// 補完サンプルを追加 (起点 run の重複排除)。
	seenIDs := map[string]bool{originID: true}

	appendList := func(items []akashic.WorkflowRunListItem) error {
		for _, item := range items {
			if seenIDs[item.RunID] {
				continue
			}
			seenIDs[item.RunID] = true
			// Reader.Detail で full AgentCallRow + RawEvents を取得
			// (List の AgentCallSummary は要約型で AgentCallRow とフィールドが異なるため)。
			detail, derr := b.reader.Detail(ctx, akashic.DetailOptions{
				WorkflowRunID: item.RunID,
				RawEvents:     akashic.RawEventsQuery{Limit: in.MaxRawEvents},
			})
			if derr != nil {
				return fmt.Errorf("%w: Reader.Detail(sample=%s): %w", ErrContextLoad, item.RunID, derr)
			}
			sample, serr := b.buildSampleRun(ctx, detail.Run, detail.AgentCalls, detail.RawEvents, in.MaxArtifactBytes, false)
			if serr != nil {
				return serr
			}
			sampleRuns = append(sampleRuns, sample)
		}
		return nil
	}

	if err := appendList(successList.Items); err != nil {
		return PromptContext{}, err
	}
	if err := appendList(failureList.Items); err != nil {
		return PromptContext{}, err
	}

	// (8) Limited フラグ: 補完サンプルが 1 件も取れず origin のみのとき true (Req 2.5)
	limited := len(sampleRuns) == 1

	return PromptContext{
		TargetWorkflowID: in.TargetWorkflowID,
		TargetGoFile:     in.TargetGoFile,
		CurrentSource:    string(sourceBytes),
		SampleRuns:       sampleRuns,
		FailureClusters:  failureClustersResult.Clusters,
		Limited:          limited,
	}, nil
}

// buildSampleRun は単一 run について SampleRun を構築する helper。
//
// ArtifactReader.GetArtifacts → 各 row について SizeBytes <= maxArtifactBytes のときのみ
// ReadArtifact を呼び、Body 非空にする (Req 2.4 size guard)。それを超えるサイズの artifact は
// metadata (Kind / Path / SizeBytes) のみで Body は空 byte slice のまま残す。
//
// GetArtifacts 自体の失敗 / 個別 ReadArtifact の失敗はいずれも ErrContextLoad で wrap する
// (Req 2.6: context 取得失敗は orchestration 側で `context_load_failed` verdict にマップ)。
func (b *DefaultContextBuilder) buildSampleRun(
	ctx context.Context,
	run akashic.RunHeader,
	agentCalls []akashic.AgentCallRow,
	rawEvents []akashic.RawEventRow,
	maxArtifactBytes int64,
	isOrigin bool,
) (SampleRun, error) {
	rows, err := b.artifacts.GetArtifacts(ctx, run.RunID)
	if err != nil {
		return SampleRun{}, fmt.Errorf("%w: ArtifactReader.GetArtifacts(run=%s): %w", ErrContextLoad, run.RunID, err)
	}

	snippets := make([]ArtifactSnippet, 0, len(rows))
	for _, r := range rows {
		snip := ArtifactSnippet{
			Kind:      r.Kind,
			Path:      r.Path,
			SizeBytes: r.SizeBytes,
		}
		if r.SizeBytes <= maxArtifactBytes {
			body, rerr := b.artifacts.ReadArtifact(ctx, r.Path)
			if rerr != nil {
				return SampleRun{}, fmt.Errorf("%w: ArtifactReader.ReadArtifact(path=%s): %w", ErrContextLoad, r.Path, rerr)
			}
			snip.Body = body
		}
		snippets = append(snippets, snip)
	}

	return SampleRun{
		Run:                     run,
		AgentCalls:              agentCalls,
		RawEvents:               rawEvents,
		Artifacts:               snippets,
		IsOriginatedFromHandoff: isOrigin,
	}, nil
}

// 静的契約: DefaultContextBuilder は ContextBuilder interface を満たす。
var _ ContextBuilder = (*DefaultContextBuilder)(nil)

// Package akashic の Reader 契約・結果型・Options 型・sentinel error を定義する。
//
// 本ファイルは read-only API の唯一の契約定義であり、書き込み (Recorder) からは
// interface として完全に分離されている。実装は同パッケージの reader_sqlite.go に閉じ、
// 既存の SQLiteRecorder 型が Reader を満たす形で展開される (Repository pattern)。
//
// 設計上の注意:
//   - すべての結果型は struct で公開し、interface でラップしない
//     (Req 4.9: 下流 spec が将来フィールドを追加できる拡張余地)
//   - Aggregate は粒度の異なる 2 指標 (workflow_run 母数 / agent_call 母数) を
//     静的に分離するため、メソッドと結果型を別々に定義する
//   - sentinel error は呼び出し側が errors.Is で判別するため、ラップせずそのまま返す
package akashic

import (
	"context"
	"errors"
	"time"
)

// ErrWorkflowRunNotFound は Detail で指定した workflow_run_id が DB に存在しない場合に返る
// sentinel error。呼び出し側は errors.Is(err, ErrWorkflowRunNotFound) で判別する (Req 4.7)。
var ErrWorkflowRunNotFound = errors.New("akashic: workflow run not found")

// ErrInvalidLimit / ErrInvalidOffset は Options バリデーションで負値や上限超過を検出した
// 場合に返す sentinel error (Req 2.10 / 7.4)。
//
// 実装契約:
//   - Limit < 0 または Limit > Max*Limit → ErrInvalidLimit
//   - Offset < 0 → ErrInvalidOffset
//   - Limit == 0 はエラーにせず、実装層 (reader_sqlite.go) で Default*Limit に補完する
//     (Req 2.9: "未指定" のときは既定 limit を適用する)
//   - SampleSize の異常値は ErrInvalidLimit を流用する (sentinel を増やさない設計判断)
var (
	ErrInvalidLimit  = errors.New("akashic: invalid limit")
	ErrInvalidOffset = errors.New("akashic: invalid offset")
)

// 既定値・上限値。テストから上書き可能にするため var として export する。
// 実装層は Limit == 0 を Default*Limit に補完し、Limit > Max*Limit を ErrInvalidLimit にする。
var (
	// DefaultListLimit は List の Limit 未指定時に適用される既定値。
	DefaultListLimit = 50
	// MaxListLimit は List の Limit 上限。これを超えると ErrInvalidLimit。
	MaxListLimit = 1000
	// DefaultRawEventsLimit は Detail の raw_events Limit 未指定時に適用される既定値。
	DefaultRawEventsLimit = 100
	// MaxRawEventsLimit は Detail の raw_events Limit 上限。
	MaxRawEventsLimit = 5000
	// DefaultFailureClusterLimit は FailureClusters の Limit 未指定時に適用される既定値。
	DefaultFailureClusterLimit = 100
	// MaxFailureClusterLimit は FailureClusters の Limit 上限。
	MaxFailureClusterLimit = 500
	// DefaultFailureSampleSize は FailureClusters のクラスタごとの代表 run 件数の既定値。
	DefaultFailureSampleSize = 5
	// MaxFailureSampleSize は FailureClusters のクラスタごとの代表 run 件数の上限。
	MaxFailureSampleSize = 50
	// MaxFailureRowsLoaded は FailureClusters が SQL から取得する失敗 agent_call の
	// 行数上限。SQL は filter+sort のみで取得し、この上限を超える失敗 agent_call が
	// 存在する場合は FailureClustersResult.Truncated=true で返す (Req 5.8)。
	MaxFailureRowsLoaded = 10000
)

// TimeRange は started_at / observed_at に対する期間フィルタ。
// Since/Until がゼロ値 (time.Time の zero) ならその端は無制限として扱う。
type TimeRange struct {
	Since time.Time
	Until time.Time
}

// ListOptions は List のフィルタとページネーション指定 (Req 2.1〜2.10)。
type ListOptions struct {
	WorkflowID string    // 空文字 → 全 workflow
	Range      TimeRange // started_at に対するフィルタ
	Success    *bool     // nil → 両方 / true → 成功のみ / false → 失敗のみ
	ProviderID string    // 空文字 → 全 provider
	CWD        string    // 空文字 → 全 cwd
	ProjectID  string    // 空文字 → 全 project (cwd basename 由来の project 絞り込みキー)
	Limit      int       // 0 → 既定 (DefaultListLimit), < 0 or > MaxListLimit → ErrInvalidLimit
	Offset     int       // < 0 → ErrInvalidOffset
}

// Validate は ListOptions の Limit / Offset を検証する。
// Limit==0 は実装層で DefaultListLimit に補完するためここでは通す (Req 2.9)。
func (o ListOptions) Validate() error {
	if o.Limit < 0 || o.Limit > MaxListLimit {
		return ErrInvalidLimit
	}
	if o.Offset < 0 {
		return ErrInvalidOffset
	}
	return nil
}

// AgentCallSummary は List 結果に同梱する agent_call の概要 (Req 2.7)。
// Detail 取得時は AgentCallRow が完全な agent_call を提供する。
type AgentCallSummary struct {
	ID              string
	ProviderID      string
	StepID          string
	StepName        string
	Success         *bool      // NULL → nil
	DurationMS      *int64     // NULL → nil
	TotalCostUSD    *float64   // NULL → nil
	InputTokens     *int64     // NULL → nil
	OutputTokens    *int64     // NULL → nil
	FailureCategory string     // 失敗種別。成功時は空文字
	Error           string     // エラーメッセージ。成功時は空文字
	StartedAt       time.Time  // 必須 (NOT NULL)
	FinishedAt      *time.Time // NULL → nil
}

// RunHeader は workflow_run の単一レコードを表す共通ヘッダ型 (Req 4.9)。
// List/Detail/将来の artifacts 拡張で AgentCalls などサブリソースを除いた
// run スカラ列を唯一のソースとして参照するために導入する。
//
// Success の派生定義: workflow_runs テーブル単独で完結し agent_calls は参照しない (Req 3.1)。
//   - true:  finished_at IS NOT NULL かつ error IS NULL  (= 完了かつエラーなし)
//   - false: finished_at IS NOT NULL かつ error IS NOT NULL (= 完了かつエラーあり)
//   - nil:   finished_at IS NULL (= 未完了)
type RunHeader struct {
	RunID        string
	WorkflowID   string
	WorkflowName string
	CWD          string
	StartedAt    time.Time
	FinishedAt   *time.Time
	Success      *bool // workflow_runs.error と finished_at から派生 (上記コメント参照)
	Error        string
}

// WorkflowRunListItem は List の 1 レコード。RunHeader を embed することで
// RunID / WorkflowID 等のフィールドアクセスを互換維持しつつ、AgentCalls をサブリソース
// として同梱する (Req 2.7)。
type WorkflowRunListItem struct {
	RunHeader
	AgentCalls []AgentCallSummary
}

// ListResult は List の戻り値。
// NextOffset は次ページが存在するときのみ非 nil で、Offset+Limit を指す (Req 2.8)。
type ListResult struct {
	Items      []WorkflowRunListItem
	Limit      int
	Offset     int
	NextOffset *int // len(Items) == Limit のとき *NextOffset = Offset + Limit、それ以外 nil
}

// WorkflowAggregateOptions は workflow_id 単位集計のフィルタ指定 (Req 3.1〜3.4)。
// 母数は workflow_run。ProviderID を指定すると、当該 provider が関与した workflow_run
// のみが母集団となる。
type WorkflowAggregateOptions struct {
	Range      TimeRange
	WorkflowID string // 空文字 → 全 workflow (追加絞り込みなし)
	ProviderID string // 空文字 → 全 provider (agent_calls 経由の追加絞り込み)
	ProjectID  string // 空文字 → 全 project (workflow_runs.project_id 直接絞り込み)
}

// Validate は WorkflowAggregateOptions の検証。現状は追加検証なし (Range の Since>Until は
// 仕様外とし実装側で空集計として扱う)。将来の検証フック用に空メソッドを定義する。
func (o WorkflowAggregateOptions) Validate() error {
	return nil
}

// ProviderAggregateOptions は provider_id 単位集計のフィルタ指定 (Req 3.5)。
// 母数は agent_call (workflow_run 横断)。WorkflowID を指定すると当該 workflow に
// 紐づく agent_calls のみを集計対象とする。
type ProviderAggregateOptions struct {
	Range      TimeRange
	WorkflowID string // 空文字 → 全 workflow
	ProviderID string // 空文字 → 全 provider
	ProjectID  string // 空文字 → 全 project (agent_calls.project_id 直接絞り込み)
}

// Validate は ProviderAggregateOptions の検証。現状は追加検証なし。
func (o ProviderAggregateOptions) Validate() error {
	return nil
}

// WorkflowAggregateBucket は workflow_id 単位の集計 1 件 (Req 3.1〜3.4)。
// 母数は workflow_run。SuccessRate は workflow_runs.error/finished_at から派生する
// 「成功 run 数 / RunCount」。
type WorkflowAggregateBucket struct {
	WorkflowID      string
	RunCount        int64    // 母集団の workflow_run 件数
	SuccessRate     float64  // 成功 run 数 / RunCount。RunCount==0 のとき 0
	AvgCostUSD      *float64 // agent_calls.total_cost_usd の AVG。NULL only または対象なしなら nil (Req 3.7)
	P50DurationMS   *int64   // agent_calls.duration_ms の p50。NULL only または対象なしなら nil
	P95DurationMS   *int64   // agent_calls.duration_ms の p95。NULL only または対象なしなら nil
	SumInputTokens  int64    // NULL は除外しても合計は数値で表現可
	SumOutputTokens int64
}

// ProviderAggregateBucket は provider_id 単位の集計 1 件 (Req 3.5)。
// 母数は agent_call。SuccessRate は agent_calls.success の比率。
//
// 集計不変条件: 全 provider の CallCount 合算 = フィルタ範囲内の agent_call 総数。
type ProviderAggregateBucket struct {
	ProviderID      string
	CallCount       int64    // 母集団の agent_call 件数
	SuccessRate     float64  // success=1 な agent_call 数 / CallCount。CallCount==0 のとき 0
	AvgCostUSD      *float64 // NULL only または対象なしなら nil
	P50DurationMS   *int64
	P95DurationMS   *int64
	SumInputTokens  int64
	SumOutputTokens int64
}

// WorkflowAggregateResult は AggregateByWorkflow の戻り値。
// 集計対象なしのときは Buckets == nil で返す (Req 3.8)。
type WorkflowAggregateResult struct {
	Buckets []WorkflowAggregateBucket
}

// ProviderAggregateResult は AggregateByProvider の戻り値。
// 集計対象なしのときは Buckets == nil で返す (Req 3.8)。
type ProviderAggregateResult struct {
	Buckets []ProviderAggregateBucket
}

// RawEventsQuery は Detail 内 raw_events のページング・フィルタ指定 (Req 4.4, 4.5)。
type RawEventsQuery struct {
	Limit   int    // 0 → DefaultRawEventsLimit, < 0 or > MaxRawEventsLimit → ErrInvalidLimit
	Offset  int    // < 0 → ErrInvalidOffset
	Type    string // 空文字 → 全 type
	Subtype string // 空文字 → 全 subtype
}

// Validate は RawEventsQuery の Limit / Offset を検証する。
func (q RawEventsQuery) Validate() error {
	if q.Limit < 0 || q.Limit > MaxRawEventsLimit {
		return ErrInvalidLimit
	}
	if q.Offset < 0 {
		return ErrInvalidOffset
	}
	return nil
}

// DetailOptions は Detail の指定。
// WorkflowRunID は必須 (空文字でも通すが該当行が無いため ErrWorkflowRunNotFound で返る)。
type DetailOptions struct {
	WorkflowRunID string         // 必須
	RawEvents     RawEventsQuery // raw_events のページング・フィルタ
}

// Validate は DetailOptions の検証。内包 RawEvents.Validate を委譲する。
// WorkflowRunID の存在チェックは実装側 (DB 問い合わせ結果) で ErrWorkflowRunNotFound
// として返すため、Validate では空文字を許容する。
func (o DetailOptions) Validate() error {
	return o.RawEvents.Validate()
}

// AgentCallRow は Detail で返す agent_call の完全な行型。
// 既存 store.go の AgentCall (書き込み入力型) とは分離し、書き込み側の構造変更で
// 読み取り API が破壊されないようにする。
//
// 注意: ProjectID は agent-learnings spec (task 3.2) で additive 追加された field。
//   - AgentCallsByWorkflowRun 経由でのみ populate される (PreFetch phase の
//     post-filter / project scope 強制用)。
//   - Detail 経由の取得では空文字のままになる (既存 Detail SQL を改変しないため)。
//   - 値の解釈は ProjectScopedReader が r.projectID と比較する用途に限定する。
type AgentCallRow struct {
	ID                   string
	WorkflowRunID        string
	StepID               string
	StepName             string
	ProviderID           string
	Prompt               string
	CWD                  string
	ProjectID            string
	SessionID            string
	Model                string
	ModelReasoningEffort string
	Success              *bool
	DurationMS           *int64
	TotalCostUSD         *float64
	InputTokens          *int64
	OutputTokens         *int64
	ResultSummary        string
	Error                string
	FailureCategory      string
	StartedAt            time.Time
	FinishedAt           *time.Time
	ResultPayload        []byte // result_payload_json (TEXT) を bytes として返す
}

// RawEventRow は Detail で返す raw_event 1 件。
// Payload は raw bytes で返し、JSON パースは下流の責務。
type RawEventRow struct {
	ID               int64
	AgentCallID      string
	WorkflowRunID    string
	Sequence         int64
	Source           string
	Type             string
	Subtype          string
	ObservedAt       time.Time
	Payload          []byte
	PayloadTruncated bool
}

// AgentContextUsageRow は Detail で返す agent_context_usage 1 件。
// *_json は読取側もそのまま bytes で渡す (パースは下流の責務)。
type AgentContextUsageRow struct {
	AgentCallID          string
	WorkflowRunID        string
	RawEventID           int64
	Sequence             int64
	ObservedAt           time.Time
	Model                string
	TotalTokens          int64
	MaxTokens            int64
	RawMaxTokens         int64
	Percentage           int64
	IsAutoCompactEnabled bool
	AutoCompactThreshold *int64
	CategoriesJSON       []byte
	MemoryFilesJSON      []byte
	MCPToolsJSON         []byte
	AgentsJSON           []byte
	MessageBreakdownJSON []byte
	APIUsageJSON         []byte
}

// RunDetail は Detail の戻り値。
// agent_calls の正のソースは AgentCalls []AgentCallRow に一本化する。
// Run は AgentCalls を持たない RunHeader 型を使用し、List 経由の AgentCallSummary
// とのフィールド重複を静的に排除する。
//
// 拡張フィールド (.rilll/ 成果物パス等) は RunHeader への追加または RunDetail 直下の
// サブリソース追加で対応する (Req 4.9 拡張点の一元化)。
type RunDetail struct {
	Run                 RunHeader
	AgentCalls          []AgentCallRow
	ContextUsage        []AgentContextUsageRow
	RawEvents           []RawEventRow
	RawEventsLimit      int
	RawEventsOffset     int
	NextRawEventsOffset *int // len(RawEvents) == RawEventsLimit のとき *NextRawEventsOffset = RawEventsOffset + RawEventsLimit
}

// FailureOptions は FailureClusters のフィルタ・上限指定 (Req 5.1〜5.5)。
type FailureOptions struct {
	Range      TimeRange
	WorkflowID string
	ProviderID string
	ProjectID  string // 空文字 → 全 project (agent_calls.project_id 直接絞り込み)
	Limit      int    // クラスタ件数上限。0 → DefaultFailureClusterLimit, < 0 or > MaxFailureClusterLimit → ErrInvalidLimit
	SampleSize int    // クラスタごとの代表 run 件数。0 → DefaultFailureSampleSize, < 0 or > MaxFailureSampleSize → ErrInvalidLimit (流用)
}

// Validate は FailureOptions の Limit / SampleSize を検証する。
// SampleSize の異常値は sentinel を増やさない設計判断で ErrInvalidLimit を流用する。
func (o FailureOptions) Validate() error {
	if o.Limit < 0 || o.Limit > MaxFailureClusterLimit {
		return ErrInvalidLimit
	}
	if o.SampleSize < 0 || o.SampleSize > MaxFailureSampleSize {
		return ErrInvalidLimit
	}
	return nil
}

// FailureCluster はヒューリスティッククラスタリング結果の 1 クラスタ (Req 5.2〜5.4)。
//
// クラスタリングキー: (FailureCategory, ProviderID, NormalizedErrorKey, RawEventErrorSubtype)
// RepresentativeRunIDs はクラスタ内で finished_at DESC の順に SampleSize 件を切り出した
// workflow_run_id (Req 5.4)。
type FailureCluster struct {
	FailureCategory      string    // 主キー (agent_calls.failure_category。空文字許容)
	ProviderID           string    // 副キー
	NormalizedErrorKey   string    // error 文字列正規化結果 (normalizeError の出力)
	RawEventErrorSubtype string    // 主要 subtype (複数あれば最頻値)
	Count                int64     // クラスタ内の失敗 agent_call 件数
	LastSeen             time.Time // クラスタ内最新の finished_at
	RepresentativeRunIDs []string  // finished_at DESC で SampleSize 件切り出し
	// RepresentativeAgentCallIDs は RepresentativeRunIDs と同じ順序 / 同じ件数の
	// 失敗 agent_call ID 群 (memory-distiller の resolution-chain 拡張 Pattern 1 用)。
	// インデックス i の RepresentativeRunIDs[i] と RepresentativeAgentCallIDs[i] は
	// 1 つの failure cluster 構成 row のペアであり、resolution lookup の入力として使う。
	// SampleSize で同じ件数に切り出される (Req 5.4 と整合)。
	RepresentativeAgentCallIDs []string
	// RepresentativeWorkflowIDs / RepresentativeStepIDs は RepresentativeAgentCallIDs と
	// 同じ順序 / 同じ件数の workflow_id / step_id 群 (memory-distiller の resolution-chain
	// 拡張 Pattern 2 用)。Pattern 2 (cross-run + 手動修正) では NextSuccessAcrossRuns に
	// 代表値 (例: index 0 = 最新の失敗) を渡して別 run での next success を探索する。
	RepresentativeWorkflowIDs []string
	RepresentativeStepIDs     []string
	// RepresentativeErrors / RepresentativeResultSummaries は RepresentativeAgentCallIDs と
	// 同じ順序 / 同じ件数の生 error / result_summary 群 (resolution-chain 拡張 Pattern 2 用)。
	// `NormalizedErrorKey` はパス / 数字 / UUID マスキング後で path 抽出に使えないため、
	// Pattern 2 の gitresolve.go 入力 (関連 file path 推定) には raw 値が必要。
	// LLM 入力の token 予算 cap は workflow 層で行う (Reader は raw を返す)。
	RepresentativeErrors          []string
	RepresentativeResultSummaries []string
	// UserCorrectionCount はクラスタ内の agent_calls.user_corrected = 1 件数の合計
	// (memory-distiller-trigger-policy Req 4.1, 6.3)。
	// SQL 側で COALESCE(user_corrected, 0) を経由するため legacy row も安全に 0 として読める。
	// severity は本層で露出させない (workflow 層が SeverityFromFailureCategory で導出する責務)。
	UserCorrectionCount int64
}

// FailureClustersResult はクラスタ集合とロード打ち切り情報を含む (Req 5.6, 5.8)。
//
// SQL は filter+sort のみで対象 agent_call をロードし、クラスタリングは Go 側で
// normalizeError を適用して行う。MaxFailureRowsLoaded を超える失敗 agent_call が
// 存在する場合は Truncated=true で返し、呼び出し側が打ち切りの発生を判別できる。
//
// Clusters は件数降順 → LastSeen 降順で安定ソート (Req 5.6)。
type FailureClustersResult struct {
	Clusters   []FailureCluster
	LoadedRows int  // SQL から実際にロードした agent_call 行数 (クラスタリング前の母数)
	Truncated  bool // LoadedRows == MaxFailureRowsLoaded で打ち切り発生時 true
}

// InRunResolutionOptions は InRunResolutionsForCluster の入力。
//
// memory-distiller の resolution-chain 拡張 Pattern 1 (同 run 内 resolution) 用。
// 1 つの failure cluster に属する failing agent_call の ID 群を渡し、各 ID について
// 同 workflow_run_id 内で時系列的に後続する成功 agent_call (同 step_id, success=1) を
// 1 件ずつ batched lookup する。
//
// 設計契約:
//   - FailingAgentCallIDs が空なら戻り値は空 map で副作用なし。
//   - ProjectScopedReader 経由で呼ばれるとき ProjectID は decorator が上書きする。
//     直接 Reader を呼ぶときは呼び出し側が空文字 (全 project) または明示値を指定する。
//   - SQL は agent_calls テーブル単独で完結し、JOIN を持たない。
type InRunResolutionOptions struct {
	ProjectID           string   // 空文字 → 全 project
	FailingAgentCallIDs []string // 1 cluster 内の代表 failing call ID 群
}

// Validate は InRunResolutionOptions の検証。
// 空 IDs は許容 (空 map を返す経路に落ちる) するため検証なし。
func (o InRunResolutionOptions) Validate() error {
	return nil
}

// InRunResolutionRow は 1 件の (failing call → success call) ペアを表す。
//
// 各フィールド:
//   - FailingAgentCallID: 入力 FailingAgentCallIDs[i] のうち success が見つかった ID。
//   - SuccessAgentCallID: 同 workflow_run_id / 同 step_id で success=1 かつ
//     started_at > failing.started_at の最初の agent_call ID。
//   - ResultSummary: 成功 agent_call の result_summary (Reader 側では truncate しない)。
//   - Prompt: 成功 agent_call の prompt (Reader 側では truncate しない。
//     workflow 層の projection で先頭 N chars に cap される)。
//   - StartedAt: 成功 agent_call の started_at。
type InRunResolutionRow struct {
	FailingAgentCallID string
	SuccessAgentCallID string
	WorkflowRunID      string
	StepID             string
	StartedAt          time.Time
	ResultSummary      string
	Prompt             string
}

// InRunResolutionsResult は InRunResolutionsForCluster の戻り値。
//
// ResolutionsByFailingID は failing agent_call ID をキーに 0 or 1 件の row を引ける map。
// success 候補が無い failing call は map のキーに含まれない (空 entry を作らない契約)。
type InRunResolutionsResult struct {
	ResolutionsByFailingID map[string]InRunResolutionRow
}

// NextSuccessOptions は NextSuccessAcrossRuns の入力 (Pattern 2)。
//
// memory-distiller の resolution-chain 拡張 Pattern 2 (cross-run + 手動修正) 用。
// 失敗 cluster の (workflow_id, step_id) に対し、AfterTime より後で最初に成功した
// agent_call を 1 件返す。time window の until 端は git log lookup の時間範囲算出に使う
// (workflow 層側の責務)。
type NextSuccessOptions struct {
	ProjectID  string    // 空文字 → 全 project (ProjectScopedReader 経由で上書きされる)
	WorkflowID string    // 必須相当 (空文字でも通すが該当 0 件で返る)
	StepID     string    // 必須相当 (空文字でも通すが該当 0 件で返る)
	AfterTime  time.Time // ゼロ値 → 制約なし。通常は failure cluster の LastSeen を渡す
}

// Validate は NextSuccessOptions の検証。現状は追加検証なし。
func (o NextSuccessOptions) Validate() error {
	return nil
}

// NextSuccessRow は 1 件の next-success agent_call を表す。
type NextSuccessRow struct {
	AgentCallID   string
	WorkflowRunID string
	StartedAt     time.Time
}

// NextSuccessResult は NextSuccessAcrossRuns の戻り値。Found が false のとき
// Row はゼロ値で返る (該当成功 run が無いケース)。
type NextSuccessResult struct {
	Found bool
	Row   NextSuccessRow
}

// Reader はアカシックレコード (SQLite) を読み取る唯一の契約 (Req 1.1〜1.6)。
//
// Aggregate は粒度の異なる 2 指標 (workflow_run 母数 / agent_call 母数) を静的に
// 分離するため、AggregateByWorkflow / AggregateByProvider に分割している。
//
// 全メソッドは ctx を受け取り、ctx の cancel/timeout を透過する。戻り値は struct
// (値) で返し、interface でラップしないことで下流 spec が将来フィールドを追加できる
// 拡張余地を保つ (Req 4.9)。
//
// 書き込み (Insert/Update/Delete)・トランザクション境界 (Begin/Commit/Rollback) は
// 一切露出しない (Req 1.2 / 1.3)。
//
// memory-distiller の resolution-chain 拡張 (Pattern 1 / Pattern 2) で 2 メソッドを
// 追加: InRunResolutionsForCluster (Pattern 1: 同 run 内 success lookup) と
// NextSuccessAcrossRuns (Pattern 2: 別 run での next success lookup)。
type Reader interface {
	List(ctx context.Context, opts ListOptions) (ListResult, error)
	AggregateByWorkflow(ctx context.Context, opts WorkflowAggregateOptions) (WorkflowAggregateResult, error)
	AggregateByProvider(ctx context.Context, opts ProviderAggregateOptions) (ProviderAggregateResult, error)
	Detail(ctx context.Context, opts DetailOptions) (RunDetail, error)
	FailureClusters(ctx context.Context, opts FailureOptions) (FailureClustersResult, error)
	InRunResolutionsForCluster(ctx context.Context, opts InRunResolutionOptions) (InRunResolutionsResult, error)
	NextSuccessAcrossRuns(ctx context.Context, opts NextSuccessOptions) (NextSuccessResult, error)

	// AgentCallsByWorkflowRun は workflow_run_id に属する agent_calls を started_at ASC,
	// id ASC の安定順序で返す (agent-learnings spec task 3.2 / Req 3.1, 3.6)。
	//
	// PreFetch phase が parent_run_id から session_id を集約するために使う。
	// 戻り値の AgentCallRow.ProjectID は本メソッド経由でのみ populate され、
	// ProjectScopedReader が project scope を post-filter する素材になる。
	AgentCallsByWorkflowRun(ctx context.Context, workflowRunID string) ([]AgentCallRow, error)

	// LearningsByWorkflowRun は workflow_run_id の learnings 行を recorded_at ASC,
	// id ASC の安定順序で返す (agent-learnings spec task 3.2 / Req 5.1)。
	//
	// learnings table は project_id 列を持たないため scope 強制は不要 (Detail と同様
	// run ID 直指定パターン)。0 件は空 slice + nil error。
	LearningsByWorkflowRun(ctx context.Context, workflowRunID string) ([]LearningRow, error)
}

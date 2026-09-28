// Package akashic の learnings.go は agent-learnings spec (task 3.2 / Req 5.1, 5.2,
// 5.3, 9.6) の learnings table 用 domain 型と validation helper を集約する。
//
// 設計契約:
//   - LearningRow は learnings table の 1 行と 1:1 対応する struct (design.md L676-688)。
//     paths_glob は []string で持ち、SQLite 永続化時のみ JSON encode して
//     `paths_glob_json TEXT` 列に格納する (learnings_sqlite.go の責務)。
//   - Category / Source は closed enum 文字列定数として export する。
//     enum 集合の追加 / 削除は本 spec の domain 契約変更のため、本ファイルへの編集が SSoT。
//   - Recorder の validation は ValidLearningCategory / MaxPathsGlobEntries の 2 つに閉じる。
//     Apply phase が paths_glob 内容そのもの (空 / "**" / "**/*" 等) を reject するのは
//     上位層 (workflow/builtin/learnings/apply) の責務で、本層は最低限の structural
//     不変条件 (Recorder 物理層) のみを守る。
//
// 設計上の境界:
//   - 本ファイルは pure data 層であり SQL / DB ハンドルを持たない。
//   - learnings table の DDL は store.go の `const schema` (task 3.1) に閉じている。
package akashic

import (
	"errors"
	"time"
)

// 4 値 closed enum: agent-learnings spec Req 5.3 / design.md "Category" 値オブジェクト。
//
// この 4 値以外の値で RecordLearning を呼ぶと ErrInvalidLearningCategory で reject される。
// 集合の改訂は本 spec の domain 契約変更のため、Workflow/Apply phase の Category enum
// (workflow/builtin/learnings/category.go 予定) と本ファイルを同一 commit で更新すること。
const (
	// CategoryFailureRecovery は失敗からの回復手順を表す。
	CategoryFailureRecovery = "failure-recovery"
	// CategorySuccessProcedure は成功手順 (procedural memory) を表す。
	CategorySuccessProcedure = "success-procedure"
	// CategoryAntipattern は再発防止すべき antipattern を表す。
	CategoryAntipattern = "antipattern"
	// CategoryToolPattern は tool / API の利用パターンを表す。
	CategoryToolPattern = "tool-pattern"
)

// 2 値 closed enum: agent-learnings spec Req 5.2 / design.md "Source" 値オブジェクト。
//
// MVP では SourceAgentSelfReport のみ Reflector phase が使う。SourceReflectorDerived は
// 将来の自動抽出経路のための予約値 (本 spec の Recorder validation はどちらも accept する)。
const (
	// SourceAgentSelfReport は session resume 経由の AI 自己申告を表す。
	SourceAgentSelfReport = "agent-self-report"
	// SourceReflectorDerived は将来の自動抽出経路のための予約値。
	SourceReflectorDerived = "reflector-derived"
)

// MaxPathsGlobEntries は LearningRow.PathsGlob 1 件あたりの上限 entry 数。
//
// 値の根拠: design.md "Reflector の per-session response schema" の `paths_glob: ["≤ 5 entries"]`
// 制約 (Reflector / Curator 両 schema 共通)。Recorder 物理層は MaxPathsGlobEntries を超える
// slice を ErrTooManyPathsGlob で reject し、上位層 (Reflector / Apply) のバグや LLM の
// 暴発出力を最終防衛線として弾く。
const MaxPathsGlobEntries = 5

// ErrInvalidLearningCategory は RecordLearning に Category closed enum 4 値以外を
// 渡した場合に返す sentinel error (Req 5.3)。
//
// 呼び出し側は errors.Is(err, ErrInvalidLearningCategory) で判別し、Reflector phase の
// `reflector.json.dropped[]` への trace に振り分ける (workflow 層の責務)。
var ErrInvalidLearningCategory = errors.New("akashic: invalid learning category")

// ErrTooManyPathsGlob は RecordLearning に MaxPathsGlobEntries (5) を超える paths_glob
// を渡した場合に返す sentinel error (task 3.2 観測可能完了条件)。
//
// 呼び出し側は errors.Is(err, ErrTooManyPathsGlob) で判別し、Reflector phase の
// per-session 失敗集約や drop trace に振り分ける。
var ErrTooManyPathsGlob = errors.New("akashic: paths_glob has more than 5 entries")

// LearningRow は learnings table の 1 行に対応する domain 型 (design.md L676-688)。
//
// 設計契約:
//   - ID は呼び出し側が生成する UUID (Recorder 側で UUID は割り振らない)。
//   - PathsGlob は ≤ MaxPathsGlobEntries entries。Recorder 物理層で個数のみ validate し、
//     内容 ("**", "**/*", "*" 単独 reject 等) は上位 Apply phase の責務。
//   - Category は ValidLearningCategory が true となる closed enum 4 値のいずれか。
//   - Source は MVP では SourceAgentSelfReport のみ使われるが、Recorder 層では
//     どちらの enum も accept する (validation を Source enum に拡張するのは将来 spec)。
//   - RecordedAt はゼロ値で渡された場合、Recorder 側 formatTime が UTC now を補完する。
//     呼び出し側は明示的に時刻を渡すことを推奨 (test の決定論性のため)。
type LearningRow struct {
	ID            string    // UUID (呼び出し側生成)
	AgentCallID   string    // 学習の根拠となった base agent_call.id (FK 相当 / DB 制約なし)
	WorkflowRunID string    // base workflow_run.id
	WorkflowID    string    // base workflow ID (例: "kiro-spec-loop")
	StepID        string    // base step ID (例: "implement", "review")
	Category      string    // closed enum 4 値 (Req 5.3)
	Context       string    // ≤ 512 chars (上限は LLM schema 側で強制 / Recorder は無検証)
	Insight       string    // ≤ 512 chars (上限は LLM schema 側で強制 / Recorder は無検証)
	PathsGlob     []string  // ≤ MaxPathsGlobEntries entries
	Source        string    // closed enum 2 値 (Req 5.2、MVP は agent-self-report)
	RecordedAt    time.Time // INSERT 時刻 (RFC3339Nano UTC で永続化)
}

// ValidLearningCategory は s が closed enum 4 値のいずれかか判定する pure 関数。
//
// 上位層 (Reflector phase の category drop / Apply phase の operation reject) と
// Recorder 物理層 (RecordLearning の入口検証) の両方から呼ばれる SSoT。
func ValidLearningCategory(s string) bool {
	switch s {
	case CategoryFailureRecovery, CategorySuccessProcedure, CategoryAntipattern, CategoryToolPattern:
		return true
	}
	return false
}

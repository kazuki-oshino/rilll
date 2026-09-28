package inquiryloop

import (
	"context"

	"github.com/kazuki-oshino/rilll/internal/provider"
)

// Status は 1 つの kiro プロンプトに対するループ駆動の終端ステータス。
//
//   - StatusCompleted: 5 ラウンド以内に has_inquiry=false に到達した
//   - StatusAborted:   5 ラウンド超過 / プロンプト失敗 / スキーマ違反などで途中終了した
//
// FinalResult.Status の型として使う。Status と TerminationReason は対応関係を持つが、
// Status は 2 値、TerminationReason は 4 値（completed / loop-limit / prompt-failure /
// schema-violation）であり、aborted の細分化が TerminationReason 側にある。
type Status string

const (
	// StatusCompleted は kiro プロンプトが 5 ラウンド以内に has_inquiry=false で完走したことを表す。
	StatusCompleted Status = "completed"
	// StatusAborted は kiro プロンプトが完走せず途中で打ち切られたことを表す。
	// 詳細理由は FinalResult.Termination / Failure を見る。
	StatusAborted Status = "aborted"
)

// TerminationReason は engine スコープの abort / 完了理由（4 値）。
//
// 注意: ここでの 4 値は engine が観測しうる範囲（プロンプト本体のループ）に閉じた粒度であり、
// /git-commit 失敗 / persona 解決失敗 / feature 不在 / ユーザーキャンセルは
// workflow.RunTerminationReason 側で 8 値に拡張される。両者は型としては別だが、
// 4 値の文字列値は workflow 側でそのまま採用される（昇格関係）。
//
// したがって、ここで定義した文字列値は result.json.terminationReason の
// フォーマット契約の一部であり、変更すると下流（result.json 集計や jq 集計）が壊れる。
type TerminationReason string

const (
	// ReasonCompleted: 5 ラウンド以内に has_inquiry=false に到達した正常終了。
	ReasonCompleted TerminationReason = "completed"
	// ReasonLoopLimit: 問い合わせ⇄回答ラウンドが 5 回に達してもなお has_inquiry=true が返った。
	ReasonLoopLimit TerminationReason = "loop-limit"
	// ReasonPromptFailure: agent 呼び出しが error を返した、または response.Success=false。
	ReasonPromptFailure TerminationReason = "prompt-failure"
	// ReasonSchemaViolation: 構造化出力がスキーマに適合しない（フィールド欠落 / JSON 構造でない等）。
	ReasonSchemaViolation TerminationReason = "schema-violation"
)

// ValidationVerdict は validate-design など、skill 自身が GO/NO-GO を返す段の機械判定。
// has_inquiry は「追加判断が必要か」を表し、verdict は「次段へ進めてよいか」を表すため、
// 両者は分離して扱う。
type ValidationVerdict string

const (
	// VerdictGo は validation 対象が次段へ進める状態であることを表す。
	VerdictGo ValidationVerdict = "go"
	// VerdictNoGo は validation 対象に critical issue が残っており、次段へ進めてはならないことを表す。
	VerdictNoGo ValidationVerdict = "no_go"
)

// InquiryOption は kiro プロンプトが提示する選択肢の 1 つ。
// json_schema 上は label のみが required で、description は任意。
type InquiryOption struct {
	// Label は選択肢の短い見出し（必須）。
	Label string
	// Description は選択肢の補足説明（任意）。
	Description string
}

// Inquiry は kiro プロンプトが返す「問い合わせ」の中身。
// json_schema 上は title / question が required、background / options は任意。
type Inquiry struct {
	// Title は問い合わせの件名。
	Title string
	// Background は問い合わせに至った文脈。
	Background string
	// Question は実際に判断を求める文言。
	Question string
	// Options は選択肢配列（自由記述で答える設計判断では空配列でも可）。
	Options []InquiryOption
}

// Round は 1 つの kiro プロンプト内で発生する 1 回の問い合わせ⇄回答ペアを表す。
// 0 ラウンドで完走した場合は Round が 0 件、1 ラウンドで完走した場合は 1 件、と続く。
type Round struct {
	// Index は 1 始まりのラウンド番号。
	Index int
	// SessionID は session A（kiro プロンプト本体）の応答に含まれる sessionId。
	// 次ラウンドで resume するために保持する。
	SessionID string
	// Inquiry はこのラウンドで kiro プロンプトが返した問い合わせ内容。
	Inquiry Inquiry
	// AnswerText は session B（回答生成エージェント）が返した summary フィールドの文字列。
	AnswerText string
	// AnswerSession は session B 応答の sessionId。回答エージェントは毎回 fresh session で
	// 呼び出されるため、このフィールドは監査用途で残すのみ。
	AnswerSession string
}

// FailureDetail は Status=Aborted のときの詳細情報。
// result.json の failure フィールドにそのまま書き出される。
type FailureDetail struct {
	// Round は失敗が発生したラウンド番号（0 始まりではなく実行ラウンド数を入れる）。
	Round int
	// Reason は engine が判定した abort 理由。
	Reason TerminationReason
	// Message は人間向けの簡潔な説明（例: "agent returned success=false"）。
	Message string
	// RawContent は診断用に温存する agent 生応答（schema-violation のとき特に重要）。
	RawContent string
}

// FinalResult は Engine.RunPrompt の戻り値。
// 1 つの kiro プロンプトに対するループ全体の集計結果。
type FinalResult struct {
	// Status は completed / aborted のいずれか。
	Status Status
	// Termination は engine スコープの 4 値 enum。Status=completed なら必ず ReasonCompleted。
	Termination TerminationReason
	// Rounds は実行した問い合わせラウンド数（0 ラウンドで完走したケースは 0）。
	Rounds int
	// Summary は最終 has_inquiry=false 応答の summary フィールド。
	Summary string
	// Verdict は validation skill が返した GO/NO-GO 判定。通常 phase では空。
	Verdict ValidationVerdict
	// HasAdvisory は validate-* 系で「修正アドバイスや軽微な指摘あり」を表す optional フィールド。
	// 通常 phase では常に false。workflow 層の retry ループはこの値が true のとき
	// (verdict が go/no_go どちらでも) architect に Advisory 本文をフィードバックする。
	HasAdvisory bool
	// Advisory は HasAdvisory=true のときの本文 (parser で non-empty を強制)。
	// 通常 phase では空文字。
	Advisory string
	// Extensions は inquiryloop が意味解釈しない structured output の追加 top-level fields。
	// engine scope の in-memory 値として保持し、generic result artifact には永続化しない。
	Extensions map[string]any
	// LastSession は session A（kiro プロンプト本体）の最終応答の sessionId。
	// 監査記録として残すのが第一義の用途で、engine 自身はこの値を再 resume には使わない。
	// ただしワークフロー層側で「engine 完走直後の限定的な追加プロンプト」（例: workflow.runFixPhase）
	// に対しては、session A の続きとして 1 ショット resume するために本値を再利用してよい。
	// engine.RunPrompt 自体は新たな呼び出しごとに fresh session を切る契約のため、
	// この再利用は engine の単一段スコープ契約と矛盾しない。
	LastSession string
	// Trail はラウンドごとの問答記録。永続化（onRound コールバック）と
	// 履歴付き answer prompt 構築（最新 2 ラウンド分）に使う。
	Trail []Round
	// Failure は Status=aborted の場合のみ非 nil。Completed の場合は必ず nil。
	Failure *FailureDetail
}

// RunInput は Engine.RunPrompt への入力。
type RunInput struct {
	// PromptCommand は kiro プロンプトのスラッシュコマンド本文（runner directive を含まない）。
	// 例: "/kiro-spec-init demo\n\n以下のマークダウンファイルを参照してください:\n- /abs/path/brief.md"
	// directive 文字列は engine 側で初回呼び出し時にのみ末尾に付加する。
	PromptCommand string
	// PromptLabel は TUI 表示用の短い識別子。例: "01-spec-init"
	PromptLabel string
}

// AnswerPersona は回答エージェント (session B) のプロンプトに差し込む Persona 値。
//
// Content だけが LLM-visible prompt に出力される。Name / SourcePath は workflow 層の
// artifact metadata や監査用途のために保持し、prompt builder は参照しない。
type AnswerPersona struct {
	Name       string
	SourcePath string
	Content    string
}

// AnswerPolicy は回答エージェント (session B) に共有判断基準として差し込む Policy 値。
//
// Content だけが LLM-visible prompt に出力される。Name / SourcePath は workflow 層の
// artifact metadata や監査用途のために保持し、prompt builder は参照しない。
type AnswerPolicy struct {
	Name       string
	SourcePath string
	Content    string
}

// AnswerContext は回答エージェント (session B) のプロンプトに `# Context` ブロックとして
// 注入される workflow 側コンテキスト。
//
// 背景: 回答エージェントは毎ラウンド fresh session で呼ばれるため、初回 PromptCommand や
// 関連 spec ファイルへのポインタを workflow 側から明示的に再注入しないと、
// 「どの feature / どのタスクの問い合わせか」を判別できない。session A (kiro skill) は
// 同 session を resume するため初回 PromptCommand が会話履歴に残るのと対照的。
//
// Engine 1 走行 = 1 step 単位で固定値で問題ないため関数 seam ではなく値フィールドとして保持する。
// zero value (全フィールド空) のときは buildAnswerPrompt が `# Context` ブロック自体を省略する
// (= 従来挙動互換)。各サブフィールドが個別に空のときは該当サブセクションのみ省略される。
type AnswerContext struct {
	// Policies は Persona と Context の間に `# Policy` ブロックとして注入される共有判断基準。
	// 空のときはブロック全体を省略する。
	Policies []AnswerPolicy
	// PromptCommand は session A への初回 PromptCommand そのまま。
	// 例: "/kiro-impl agent-learnings 5.1"
	// 空のときは "元コマンド:" 行を省略する。
	PromptCommand string
	// SpecPaths は回答エージェントに参照させたいファイル群の絶対パス。
	// requirements.md / design.md / tasks.md / brief.md などを workflow 層で組み立てる。
	// 空のときは "参照可能なファイル:" ブロック全体を省略する。
	SpecPaths []string
	// Background は workflow 層が組み立てる任意の追加文脈 (例: "task 5.1 は 5 章配下")。
	// 空のときは "追加文脈:" 行を省略する。
	Background string
}

// AgentCaller は kiro プロンプト本体および回答生成エージェントの呼び出し抽象。
//
// workflowctx.AgentCaller は CallAgentOptions 経由でステップ識別子を渡す API になっている一方、
// inquiryloop は (a) provider 抽象に直接依存しすぎたくない、(b) ステップ ID を engine 側で
// ラウンド毎に組み立てたい、という要件があるため、より細かい引数列を持つ独自 interface を切る。
// workflow 層が両者の間に薄いアダプタを挟む。
type AgentCaller interface {
	Call(
		ctx context.Context,
		prompt string,
		cwd string,
		onEvent provider.AgentEventCallback,
		resumeSessionID string,
		outputFormat *provider.OutputFormat,
		stepID string,
		stepName string,
	) (provider.AgentResponse, error)
}

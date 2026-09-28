package inquiryloop

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// historyWindow は buildAnswerPrompt が回答エージェント向けプロンプトに乗せる
// 直近ラウンドの最大件数。design.md / Performance 章の「最新 2 ラウンド分」契約に従う。
//
// 履歴を全部含めてしまうとループが伸びるほど 1 回あたりのトークン量が膨張するため、
// 直近 2 ラウンドのみに切り詰める。
const historyWindow = 2

// previousAnswerMaxChars は r1 resume プロンプトと # History セクションに乗せる
// 直近 answerText (および inquiry question 本文) の最大文字数。
//
// 本文は <runDir>/<NN>-<phase>/round-<N>-answer.md に persistAnswer 済みで、
// プロンプト本文には Source: パスで参照を促すだけで十分という方針。
//
// この上限を超える文字列は ellipsis ("\n\n...(truncated)") で末尾を切る。
// 切る側に背景情報の決定的な部分が残るよう「先頭 N 文字を保持」する。
const previousAnswerMaxChars = 2000

// runnerDirectiveText は kiro skill の全呼び出し (初回 / resume) に付加する固定文字列。
//
// 既存 kiro skill が AskUserQuestion 等のツール経由ではなく、
// json_schema 形式の構造化出力で has_inquiry / inquiry / summary を返すよう促す。
// 当初は初回のみ付加だったが、long-context resume 後にエージェントが drift して
// markdown を生で返し schema-violation で abort する事例 (validate-design 4 ラウンド目等)
// が観測されたため、resume 呼び出しにも毎回再付加して schema 規律を再強調する。
const runnerDirectiveText = `[runner-directive]
このコマンドへの応答は、必ず以下の json_schema 構造に従った単一オブジェクトとして返してください。
チャット用の AskUserQuestion ツールやプレーンテキストでは返さず、応答本体を構造化オブジェクトで返すこと。

含めるフィールド:
- has_inquiry (boolean): 設計判断や追加確認が必要な場合は true、そのまま完了できる場合は false。
- summary (string): プロンプトの結論あるいは判断要求の要約。
- inquiry (object, has_inquiry=true のときのみ): title, background, question, options[] を含む。
  - options[] は { label (string), description (string, 任意) } の配列。
- verdict (string, output schema で要求された場合のみ): 検証 gate の判定。通過なら "go"、差し戻しなら "no_go"。

判断が確定していない場合は has_inquiry=true を返し、inquiry に判断材料を詰めてください。
has_inquiry=true を返す代表ケース:
- requirements/design に複数の有効な解釈があり、選択を誤ると実装が無効になる場合
- 隣接 task の boundary 注記が衝突し、正しい順序が曖昧な場合
- 検証の前提条件 (env var / build tool / repo state) が不足し、workaround が自明でない場合
- reviewer が同じ設計論点で却下を繰り返し、アーキテクチャ判断が必要な場合

親 rilll runtime は impl-direction persona で方向性を返すため、resume 後はその directive に従って作業を継続してください。
判断不要でそのまま結果を確定できる場合は has_inquiry=false を返し、summary に結論を記述してください。`

// answerPolicySection は buildAnswerPrompt の末尾に付加する「回答方針」固定文。
//
// ペルソナを保ったまま、判断軸とトレードオフを明示的に語る回答を導くためのガード。
// 回答エージェントは別途 json_schema 出力を強制されるが、回答本文の品質基準を
// プロンプト側でも明文化しておく（呼び出し側 = workflow / engine の責務外を侵さない）。
const answerPolicySection = `# 回答方針
- 上記ペルソナを保ったまま回答すること。
- まず質問の意図を 1 文で要約し、その後で判断とその理由を述べること。
- 既存仕様 / 設計と矛盾する場合は、矛盾を明記したうえで再質問の形で答えること。
- 回答は要点に絞って簡潔に書く。出力フォーマットの指定は呼び出し側が施行する。`

// runnerDirective は各 kiro プロンプトの全 skill 呼び出し (初回 / resume の両方) に
// 付加する固定文字列を返す。
//
// design.md「runner directive 文字列」契約に従い、文字列定義は本ファイル 1 箇所に閉じる。
// resume 後の付加が必要な背景は runnerDirectiveText のコメントを参照。
func runnerDirective() string {
	return runnerDirectiveText
}

// appendDirective は workflow 固有 directive 文字列 (例: Engine.InquiryDirective) を
// プロンプト末尾に挟む際の「空区切り 1 行 + 本文」整形を担う。
//
// directive が空 (空白のみを含む) のときは空文字列を返し、呼び出し側のクエリ組み立てに
// 余計な改行が混ざらないようにする (= 従来挙動互換)。非空のときは "\n\n" + directive を返し、
// 直前の本文と空行 1 行で区切ってから追加する。
//
// 呼び出し側は通常 `body + appendDirective(d) + "\n\n" + runnerDirective()` の形で
// 連結し、空のときも runnerDirective() の前のセパレータが二重化しないよう設計している。
func appendDirective(directive string) string {
	if strings.TrimSpace(directive) == "" {
		return ""
	}
	return "\n\n" + directive
}

// schemaRepairDirectiveText は resume 応答が schema-violation だった時、同じ session を
// resume したまま「JSON envelope で出し直して」と短く指示するためのリトライ用文字列。
//
// 強い口調で「直前応答を破棄して空 summary だけ返す」誘導を避けるため、
// 「判断内容は変えずに JSON 構造で再出力」と限定的に指示する。
// schema 仕様の詳細は runnerDirectiveText 側に閉じている前提で、ここでは再掲しない
// (同 session resume なので runner-directive は文脈に持続している想定)。
const schemaRepairDirectiveText = `[runner-repair-directive]
直前の応答は json_schema 構造に従っていません。判断内容は変更せず、
has_inquiry / summary / (必要なら) inquiry フィールドを持つ単一 JSON オブジェクトとして
そのまま再出力してください。output schema が verdict を要求している場合は verdict も含めてください。
プレーンテキストや markdown 単体での応答は受け付けません。`

// schemaRepairDirective は resume 呼び出しで schema-violation を踏んだ際の
// 1 回限りリトライ用 query を返す。
func schemaRepairDirective() string {
	return schemaRepairDirectiveText
}

// formatResumeBody は r1 resume 呼び出しの本文 (runner-directive と結合する前の
// 「直前ラウンドの answerText 部分」) を組み立てる。
//
// 役割:
//   - sourcePath が非空のときは先頭に "Source: <path>" 行を追記し、回答本文が
//     ファイルにも persist されていることを kiro skill 側に明示する (long-context
//     resume 後の drift を抑える狙い)。
//   - answerText が previousAnswerMaxChars を超える場合は先頭 N 文字を保持して
//     末尾を ellipsis で切り、stream_idle_timeout の主因である「長い構造化応答 +
//     長い resume 入力」の片側を圧縮する。
//
// 戻り値は runner-directive を含まない。呼び出し側 (engine.go) が "\n\n" + runnerDirective()
// を結合する責務を持つ。これにより runner-directive の有無は engine 側のポリシーに残る。
func formatResumeBody(answerText, sourcePath string) string {
	body := truncateForResume(answerText)
	if sourcePath == "" {
		return body
	}
	var b strings.Builder
	b.WriteString("Source: ")
	b.WriteString(sourcePath)
	b.WriteString("\n\n")
	b.WriteString(body)
	return b.String()
}

// truncateForResume は long answerText を previousAnswerMaxChars に切り詰める。
// rune 境界で切るため、マルチバイト文字 (日本語) でも文字途中で切れない。
//
// 上限以下ならそのまま返す。超えるときは先頭 N rune + 改行 + 切り詰めマーカ
// "...(truncated; full content at Source path)" を末尾に付ける。
// マーカは markdown の本文と紛れにくい固定文字列を採用する。
func truncateForResume(s string) string {
	if previousAnswerMaxChars <= 0 || len(s) == 0 {
		return s
	}
	runes := []rune(s)
	if len(runes) <= previousAnswerMaxChars {
		return s
	}
	return string(runes[:previousAnswerMaxChars]) + "\n\n...(truncated; full content at Source path)"
}

// defaultStepID は Engine.StepIDFunc 未設定時の既定実装。
// 例: defaultStepID(2, "skill") -> "skill-r2"
//
// design.md「StepIDFunc func(round int, kind string) string // 既定 fmt "%s-r%d"」契約。
// 文字列フォーマットの責務はプロンプト構築群と同じ「呼び出し前段の整形」に属するため、
// 本ファイルに同居させて orchestrator (engine.go) の責務を絞る。
func defaultStepID(round int, kind string) string {
	return fmt.Sprintf("%s-r%d", kind, round)
}

// buildAnswerPrompt は kiro プロンプトが返した問い合わせ (Inquiry) に対する
// 回答生成エージェント (session B) 用のプロンプト本文を組み立てる。
//
// 構成:
//  1. # Persona      : AnswerPersona.Content をそのまま埋め込む
//  2. # Policy       : AnswerContext.Policies が非空のときのみ、共有判断基準を乗せる
//  3. # Context      : AnswerContext が非 zero のときのみ、workflow 由来のコンテキストを乗せる
//  4. # History      : history が非空のときのみ、直近 historyWindow ラウンド分を要約として乗せる
//  5. # Inquiry      : inquiry の JSON 表現（schema フィールド名と一致）
//  6. # 回答方針     : answerPolicySection 固定文
//
// `# Context` ブロックは AnswerContext のサブフィールドが空の場合、該当サブセクションのみ
// 省略する。すべて空 (= zero value) のときはブロック全体が省略され、従来挙動と一致する。
//
// Pre / Post:
//   - 副作用なし。同じ入力なら常に同じ出力。
//   - 戻り値は必ず非空（少なくとも # Persona 行と回答方針が必ず含まれる）。
func buildAnswerPrompt(p AnswerPersona, ctx AnswerContext, inquiry Inquiry, history []Round) string {
	var b strings.Builder

	// 1. # Persona セクション
	b.WriteString("# Persona\n")
	b.WriteString(p.Content)
	if !strings.HasSuffix(p.Content, "\n") {
		b.WriteString("\n")
	}
	b.WriteString("\n")

	// 2. # Policy セクション（AnswerContext.Policies が非空のときのみ）
	if policyBlock := formatAnswerPolicies(ctx.Policies); policyBlock != "" {
		b.WriteString(policyBlock)
	}

	// 3. # Context セクション（AnswerContext が非 zero のときのみ）
	if ctxBlock := formatAnswerContext(ctx); ctxBlock != "" {
		b.WriteString(ctxBlock)
	}

	// 4. # History セクション（直近 historyWindow ラウンドのみ）
	trimmed := trimHistory(history)
	if len(trimmed) > 0 {
		b.WriteString("# History\n")
		for _, r := range trimmed {
			b.WriteString(formatHistoryRound(r))
		}
		b.WriteString("\n")
	}

	// 5. # Inquiry セクション
	b.WriteString("# Inquiry\n")
	b.WriteString("```json\n")
	b.WriteString(encodeInquiryJSON(inquiry))
	b.WriteString("\n```\n\n")

	// 6. # 回答方針 セクション
	b.WriteString(answerPolicySection)

	return b.String()
}

// formatAnswerPolicies は AnswerPolicy 群を `# Policy` ブロックとして整形する。
//
// Policy の Name / SourcePath は provenance 専用であり、LLM-visible prompt には出さない。
// 空 Content は defensive に無視し、全て空ならブロック自体を省略する。
func formatAnswerPolicies(policies []AnswerPolicy) string {
	if len(policies) == 0 {
		return ""
	}
	var b strings.Builder
	wrote := false
	for _, policy := range policies {
		if strings.TrimSpace(policy.Content) == "" {
			continue
		}
		if !wrote {
			b.WriteString("# Policy\n\n")
			wrote = true
		} else {
			b.WriteString("\n---\n\n")
		}
		b.WriteString(policy.Content)
		if !strings.HasSuffix(policy.Content, "\n") {
			b.WriteString("\n")
		}
	}
	if !wrote {
		return ""
	}
	b.WriteString("\n")
	return b.String()
}

// formatAnswerContext は AnswerContext を `# Context` ブロックの Markdown 文字列に整形する。
//
// すべてのサブフィールドが空のときは空文字列を返し、buildAnswerPrompt 側でブロック全体が
// 省略される (= 従来挙動互換)。各サブフィールドが個別に空のときは該当サブセクションのみ
// 省略する。末尾に改行 1 行のセクション区切りを含める。
func formatAnswerContext(ctx AnswerContext) string {
	hasCmd := strings.TrimSpace(ctx.PromptCommand) != ""
	hasPaths := len(ctx.SpecPaths) > 0
	hasBG := strings.TrimSpace(ctx.Background) != ""
	if !hasCmd && !hasPaths && !hasBG {
		return ""
	}

	var b strings.Builder
	b.WriteString("# Context\n\n")
	if hasCmd {
		b.WriteString("- 元コマンド: ")
		b.WriteString(ctx.PromptCommand)
		b.WriteString("\n")
	}
	if hasPaths {
		b.WriteString("- 参照可能なファイル:\n")
		for _, p := range ctx.SpecPaths {
			b.WriteString("  - ")
			b.WriteString(p)
			b.WriteString("\n")
		}
	}
	if hasBG {
		b.WriteString("- 追加文脈: ")
		b.WriteString(ctx.Background)
		b.WriteString("\n")
	}
	b.WriteString("\n必要に応じて Read tool でこれらのファイルを参照し、判断材料を補ってください。\n\n")
	return b.String()
}

// trimHistory は history から最新 historyWindow 件のみを切り出した新しいスライスを返す。
//
// design.md / Performance 章「最新 2 ラウンド分」契約。
// historyWindow を超える長さの場合は末尾 historyWindow 件のみを返し、
// それ以下の場合はそのままの長さを返す。元スライスは破壊しない。
func trimHistory(history []Round) []Round {
	if len(history) <= historyWindow {
		return history
	}
	return history[len(history)-historyWindow:]
}

// formatHistoryRound は単一ラウンドの問答を Markdown サブセクションとして書き出す。
// 回答エージェントが文脈を補完できる最小限の情報（質問本文と当時の回答テキスト）のみを残す。
//
// inquiry / answer の本文は previousAnswerMaxChars (2000) で切り詰め、長文化したラウンドが
// answer エージェントのプロンプト全体を膨張させる (= stream_idle_timeout の二次原因) のを防ぐ。
// markdown 永続物 (round-<N>-answer.md) は別途 persistAnswer により残るため、
// 切り詰めても監査トレースは失わない。
func formatHistoryRound(r Round) string {
	var b strings.Builder
	b.WriteString("## Round ")
	b.WriteString(strconv.Itoa(r.Index))
	b.WriteString("\n")
	b.WriteString("- Inquiry: ")
	b.WriteString(truncateForResume(r.Inquiry.Question))
	b.WriteString("\n")
	b.WriteString("- Answer: ")
	b.WriteString(truncateForResume(r.AnswerText))
	b.WriteString("\n")
	return b.String()
}

// encodeInquiryJSON は Inquiry を回答エージェント向けの JSON 表現に直列化する。
//
// Schema() のフィールド名（snake_case）と一致するキーで出力することで、
// 回答エージェントが kiro skill 側の出力形式に揃った思考を持ちやすくする。
// options が空のときはキー自体を出さない（schema 上 options は任意フィールドのため）。
//
// json.Marshal が失敗するケースは map[string]any + 文字列 / 文字列スライス構成のため
// 想定しないが、防御的に Question 本文だけを返すフォールバックを置く。
func encodeInquiryJSON(inquiry Inquiry) string {
	payload := map[string]any{
		"title":      inquiry.Title,
		"background": inquiry.Background,
		"question":   inquiry.Question,
	}
	if len(inquiry.Options) > 0 {
		opts := make([]map[string]any, 0, len(inquiry.Options))
		for _, o := range inquiry.Options {
			opts = append(opts, map[string]any{
				"label":       o.Label,
				"description": o.Description,
			})
		}
		payload["options"] = opts
	}
	encoded, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return inquiry.Question
	}
	return string(encoded)
}

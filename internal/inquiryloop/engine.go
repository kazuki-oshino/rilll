// Package inquiryloop は kiro-spec-loop ワークフローで再利用される
// 「問い合わせ⇄回答ループ」エンジンの公開契約を提供する。
//
// このファイル (engine.go) は orchestrator のみを担う:
//   - Engine 構造体: ループ駆動に必要な依存 (Persona / MaxRounds / Schema / StepIDFunc)
//   - RunPrompt メソッド: 初回呼び出し → ループ → 終了判定の流れ制御
//
// 関心の分離:
//   - 応答の構造化解釈は parser.go (ParseResponse / ErrSchemaViolation)
//   - プロンプト構築は prompt.go (runnerDirective / buildAnswerPrompt / defaultStepID)
//   - 終端結果の組み立ては result.go (completedResult / loopLimitResult / ...)
//   - 公開型と定数は types.go (Round / FinalResult / FailureDetail / Reason*)
//   - json_schema 定義は schema.go (Schema)
package inquiryloop

import (
	"context"

	"github.com/kazuki-oshino/rilll/internal/provider"
)

// defaultMaxRounds は Engine.MaxRounds が未設定 (<=0) のときの既定値。
// design.md「ラウンド上限はパラメータ化（既定 5、テストで上書き可能）」契約に従う。
const defaultMaxRounds = 5

// Engine は 1 つの kiro プロンプトに対する「問い合わせ⇄回答ループ」の駆動を担う。
//
// 副作用なし: ファイル書き出しやイベント発火は Engine の責務外で、
// 呼び出し側 (workflow.kiro_spec_loop) が onRound コールバック経由で I/O を行う。
//
// design.md「inquiryloop.Engine」契約に従う。
type Engine struct {
	// Persona は回答生成エージェントに渡すペルソナ定義。
	// 空 Content で RunPrompt を呼ぶと回答生成エージェントへのプロンプトに必要なコンテキストが
	// 欠落するため、呼び出し側で解決済み Persona content を渡すこと。
	Persona AnswerPersona

	// MaxRounds は問い合わせ⇄回答ラウンドの上限。0 以下なら defaultMaxRounds (5) を使う。
	MaxRounds int

	// Schema は agent 応答に強制する既定 OutputFormat。nil の場合は Schema() を都度生成して使う。
	// SkillSchema が nil のときは、初回 / resume / 回答エージェント呼び出しすべてに同じインスタンスが渡る。
	Schema *provider.OutputFormat

	// SkillSchema は session A (kiro skill 初回 / resume / repair) にだけ強制する OutputFormat。
	// nil の場合は Schema を使う。validate-design の verdict のように、skill 側だけに
	// 追加 required field を持たせ、session B answerer には通常 schema を維持したい場合に使う。
	SkillSchema *provider.OutputFormat

	// StepIDFunc は agent 呼び出しごとの stepID 生成関数。nil なら defaultStepID を使う。
	// design.md「StepIDFunc func(round int, kind string) string // 既定 fmt "%s-r%d"」契約。
	StepIDFunc func(round int, kind string) string

	// AnswerSourcePathFunc は r1 resume 呼び出しの本文に "Source: <path>" 行を載せるために、
	// 直前ラウンドの answerText が persist された絶対パスを返す関数。
	//
	// 引数 round は「resume 直前にちょうど終わったラウンド番号」(1 始まり)。
	// 戻り値が空文字列のときは Source 行を省略する (= 従来挙動)。
	// nil のときは常に空文字列扱い (= 従来挙動)。
	//
	// 保存済み回答ファイルのパスをプロンプトに渡すためのコールバック。
	// engine 自身は filepath を知らず、workflow 層 (spec_loop_runner) が
	// `<runDir>/<NN>-<phase>/round-<N>-answer.md` の絶対パスを供給する責務を持つ。
	AnswerSourcePathFunc func(round int) string

	// AnswerContext は回答エージェント (session B) のプロンプトに `# Context` ブロックとして
	// 注入される workflow 側コンテキスト。zero value (全フィールド空) のときは
	// `# Context` ブロックは省略され、従来挙動 (Persona / History / Inquiry / 回答方針 のみ)
	// が再現される。
	//
	// AnswerSourcePathFunc が「session A への戻り (resume) で thin context 化する seam」
	// なのに対し、本フィールドは「session B への往路で workflow 由来の文脈を厚く渡す seam」
	// であり、設計の対称性を担保する位置付け。
	AnswerContext AnswerContext

	// InquiryDirective は session A (kiro skill) 側プロンプトに毎回付加される
	// workflow 固有の問い合わせポリシー文字列。runnerDirective() の直前に挿入される。
	// 空のときは付加しない (= 従来挙動互換)。
	//
	// runnerDirective と同じく、初回呼び出しと resume 呼び出しの両方に再付加することで、
	// long-context 後のドリフトに対して毎ラウンド規律を再強調する。
	//
	// 用途例: kiro-impl-loop は 1 iteration = 1 task を契約とするため、inquiry の
	// options[] に scope 拡張選択肢 (「次の task をまとめて実行」等) を並べないよう
	// inquiry-skill 側を源流で抑止する。これは workflow 層 (per-iteration skip) と
	// answer ペルソナ層 (scope 拡張 option を不採択) に続く第 3 層の防御。
	InquiryDirective string
}

// RunPrompt は 1 つの kiro プロンプトに対する問い合わせ⇄回答ループを駆動する。
//
// 流れ:
//  1. 初回 skill 呼び出し: PromptCommand + "\n\n" + runnerDirective() を query、resume="", outputFormat=SkillSchema() or Schema()
//  2. parseResponse で has_inquiry / inquiry / summary を取得。has_inquiry=false なら Completed で return
//  3. ループ (最大 MaxRounds 回):
//     a. 回答生成エージェント呼び出し (resume="" / buildAnswerPrompt / Schema 強制)
//     b. summary を回答テキストとして採用
//     c. skill 再呼び出し: 直前応答の sessionId を resume に、回答テキスト + runnerDirective() を query
//     d. 応答を parseResponse 解釈、schema-violation なら schemaRepairDirective() で 1 回だけ追プロンプトしてリトライ
//     e. has_inquiry=false なら Completed で return
//  4. ループ上限超過なら ReasonLoopLimit で Aborted
//
// abort 経路:
//   - agent err / resp.Success=false → ReasonPromptFailure
//   - parseResponse 失敗 (初回 / answer / resume の repair retry も violate) → ReasonSchemaViolation
//   - MaxRounds 超過 → ReasonLoopLimit
//
// FailureDetail.RawContent には診断用に直近応答の生本文を残す。
// onRound は各ラウンドの完了時 (skill 再呼び出しの直前) に呼ばれる。
// nil でも安全。
//
// design.md「inquiryloop.Engine」 / tasks.md 2.2 契約。
func (e Engine) RunPrompt(
	ctx context.Context,
	agent AgentCaller,
	cwd string,
	in RunInput,
	onRound func(Round),
) (FinalResult, error) {
	maxRounds := e.MaxRounds
	if maxRounds <= 0 {
		maxRounds = defaultMaxRounds
	}
	schema := e.Schema
	if schema == nil {
		schema = Schema()
	}
	skillSchema := e.SkillSchema
	if skillSchema == nil {
		skillSchema = schema
	}
	stepID := e.StepIDFunc
	if stepID == nil {
		stepID = defaultStepID
	}

	var trail []Round

	// ===== 初回 skill 呼び出し =====
	// 初回呼び出しの末尾に、workflow 由来の問い合わせポリシー (InquiryDirective) と
	// runnerDirective() を順に追記する。前者は loop-policy ガード (例: scope 拡張禁止)、
	// 後者は json_schema 出力強制の文脈再強調で、いずれも空のとき (InquiryDirective="")
	// は付加されない (= 従来挙動互換)。
	initialQuery := in.PromptCommand + appendDirective(e.InquiryDirective) + "\n\n" + runnerDirective()
	resp, err := agent.Call(
		ctx, initialQuery, cwd, nil, "", skillSchema,
		stepID(0, "skill"), in.PromptLabel,
	)
	if err != nil {
		return promptFailureResult(0, err.Error(), "", trail, ""), nil
	}
	if !resp.Success {
		return promptFailureResult(0, "agent returned success=false", resp.Content, trail, resp.SessionID), nil
	}

	parsed, parseErr := ParseResponseDetail(resp)
	if parseErr != nil {
		// 初回 skill 応答が schema-violation: 同 session を resume したまま
		// schemaRepairDirective() で 1 回だけ追プロンプトして JSON envelope を再要求する。
		// cursor-sdk のように output schema を native 強制できない provider が初回呼び出しで
		// markdown drift しても、resume パスと対称な救済が効くようにするため。
		repaired, repairedResp, abort := tryRepairSchemaViolation(
			ctx, agent, cwd, resp, skillSchema,
			stepID(0, "skill-repair"), in.PromptLabel+"-repair",
			0, trail,
		)
		if abort != nil {
			return *abort, nil
		}
		parsed = repaired
		resp = repairedResp
	}
	inquiry := parsed.Inquiry
	hasInquiry := parsed.HasInquiry
	summary := parsed.Summary

	lastSession := resp.SessionID
	if !hasInquiry {
		// 0 ラウンドで完走
		return completedResult(0, summary, lastSession, trail, parsed.Verdict, parsed.HasAdvisory, parsed.Advisory, parsed.Extensions), nil
	}

	// ===== ラウンドループ =====
	for round := 1; round <= maxRounds; round++ {
		// 回答生成エージェント呼び出し: 常に resume="" / buildAnswerPrompt / Schema 強制
		// 末尾に runnerDirective() を付加し session A 側と構造化出力規律を対称化する
		// (回答エージェントが drift して再質問形 / markdown 単体で返す事故の予防)。
		answerQuery := buildAnswerPrompt(e.Persona, e.AnswerContext, inquiry, trail) + "\n\n" + runnerDirective()
		answerResp, answerErr := agent.Call(
			ctx, answerQuery, cwd, nil, "", schema,
			stepID(round, "answer"), in.PromptLabel+"-answer",
		)
		if answerErr != nil {
			return promptFailureResult(round, answerErr.Error(), "", trail, lastSession), nil
		}
		if !answerResp.Success {
			return promptFailureResult(round, "answer agent returned success=false", answerResp.Content, trail, lastSession), nil
		}
		// summary を回答テキストとして採用 (has_inquiry 値に関わらず)。
		// answer 応答が schema-violation の場合は、同 answer session を resume したまま
		// schemaRepairDirective() で 1 回だけ追プロンプトして JSON envelope を再要求する
		// (skill 側との対称化)。リペアも violate なら従来通り abort。
		parsedAnswer, answerParseErr := ParseResponseDetail(answerResp)
		if answerParseErr != nil {
			repaired, repairedResp, abort := tryRepairSchemaViolation(
				ctx, agent, cwd, answerResp, schema,
				stepID(round, "answer-repair"), in.PromptLabel+"-answer-repair",
				round, trail,
			)
			if abort != nil {
				return *abort, nil
			}
			parsedAnswer = repaired
			answerResp = repairedResp
		}
		answerText := parsedAnswer.Summary

		// このラウンドの問答を Trail に記録 + onRound コールバック発火
		roundRecord := Round{
			Index:         round,
			SessionID:     lastSession, // session A (kiro skill) の直前応答 sessionId
			Inquiry:       inquiry,
			AnswerText:    answerText,
			AnswerSession: answerResp.SessionID,
		}
		trail = append(trail, roundRecord)
		if onRound != nil {
			onRound(roundRecord)
		}

		// skill 再呼び出し: 直前応答の sessionId を resume に、回答テキスト + runnerDirective() を query
		// (long-context resume 後の schema 規律崩れに対する予防レイヤ)
		//
		// answerText が長い場合は previousAnswerMaxChars (2000) で切り詰め、
		// 全文は persistAnswer 済みの round-<N>-answer.md を Source: パスで参照させる。
		// プロンプトを短く保ち、stream_idle_timeout のリスクを下げる。
		var sourcePath string
		if e.AnswerSourcePathFunc != nil {
			sourcePath = e.AnswerSourcePathFunc(round)
		}
		// resume 呼び出しでも初回と同じく InquiryDirective + runnerDirective() を付加する。
		// long-context 後の drift で session A が loop-policy / 構造化出力規律を忘れる事例
		// に対する毎ラウンド再強調。
		resumeQuery := formatResumeBody(answerText, sourcePath) + appendDirective(e.InquiryDirective) + "\n\n" + runnerDirective()
		resumeResp, resumeErr := agent.Call(
			ctx, resumeQuery, cwd, nil, lastSession, skillSchema,
			stepID(round, "skill"), in.PromptLabel,
		)
		if resumeErr != nil {
			return promptFailureResult(round, resumeErr.Error(), "", trail, lastSession), nil
		}
		if !resumeResp.Success {
			return promptFailureResult(round, "agent returned success=false", resumeResp.Content, trail, resumeResp.SessionID), nil
		}

		parsedNext, nextParseErr := ParseResponseDetail(resumeResp)
		if nextParseErr != nil {
			// resume 応答が schema-violation の場合、同 session を resume したまま
			// schemaRepairDirective() で 1 回だけ追プロンプトしてリトライする。
			// 2 回目も violate or agent err なら従来通り abort。
			repaired, repairedResp, abort := tryRepairSchemaViolation(
				ctx, agent, cwd, resumeResp, skillSchema,
				stepID(round, "skill-repair"), in.PromptLabel+"-repair",
				round, trail,
			)
			if abort != nil {
				return *abort, nil
			}
			parsedNext = repaired
			resumeResp = repairedResp
		}

		lastSession = resumeResp.SessionID

		if !parsedNext.HasInquiry {
			// このラウンドの解答で完走
			return completedResult(round, parsedNext.Summary, lastSession, trail, parsedNext.Verdict, parsedNext.HasAdvisory, parsedNext.Advisory, parsedNext.Extensions), nil
		}

		// 次ラウンドへ
		inquiry = parsedNext.Inquiry
	}

	// MaxRounds 超過: ループが収束しなかった
	return loopLimitResult(maxRounds, lastSession, trail, inquiry), nil
}

// tryRepairSchemaViolation は ParseResponseDetail が schema-violation を返した直後に
// 「同じ session を resume したまま schemaRepairDirective() で 1 回だけ JSON envelope を
// 再要求する」共通リカバリ手順を実行する。
//
// 適用箇所 (engine.RunPrompt 内 3 ヶ所):
//   - 初回 skill 呼び出し（round=0, skill schema, step "skill-repair-r0"）
//   - answer 呼び出し（round=N, answer schema, step "answer-repair-rN"）
//   - resume 呼び出し（round=N, skill schema, step "skill-repair-rN"）
//
// 同 session resume にすることで、provider 側の cache を温存しつつ「文脈を保ったまま
// 再フォーマットだけ要求する」セマンティクスを実現する。cursor-sdk のように output schema を
// native 強制できない provider が markdown drift した場合の救済が主目的で、Claude/Codex でも
// has_inquiry=true なのに inquiry 欠落のような contract 違反を 1 度だけ救う。
//
// 戻り値:
//   - parsed: repair 成功時の ParsedResponse (zero value on abort)
//   - resp: repair の AgentResponse (zero value on abort)。呼び出し側はこれで元の bad
//     resp を置き換えることで、後続の lastSession 更新や trail 記録が repair 後の値で
//     一貫するようにする
//   - abort: repair 自体が失敗 (agent err / success=false / 再び schema-violation) した
//     ときに非 nil の FinalResult を返す。呼び出し側はこれを return するだけで、
//     既存の promptFailureResult / schemaViolationResult と同じ abort semantics になる
//
// リトライは 1 回限り。repair も schema-violation なら呼び出し側が schemaViolationResult
// で abort する (= 旧 resume パスの挙動を 3 spot に拡張)。
func tryRepairSchemaViolation(
	ctx context.Context,
	agent AgentCaller,
	cwd string,
	badResp provider.AgentResponse,
	schema *provider.OutputFormat,
	stepID string,
	stepName string,
	round int,
	trail []Round,
) (ParsedResponse, provider.AgentResponse, *FinalResult) {
	repairResp, repairErr := agent.Call(
		ctx, schemaRepairDirective(), cwd, nil, badResp.SessionID, schema,
		stepID, stepName,
	)
	if repairErr != nil {
		// repair 呼び出し自体が転送エラー: 直前 bad 応答の session を最終 session として記録
		r := promptFailureResult(round, repairErr.Error(), badResp.Content, trail, badResp.SessionID)
		return ParsedResponse{}, provider.AgentResponse{}, &r
	}
	if !repairResp.Success {
		r := promptFailureResult(round, "agent returned success=false", repairResp.Content, trail, repairResp.SessionID)
		return ParsedResponse{}, provider.AgentResponse{}, &r
	}
	parsedRepair, repairParseErr := ParseResponseDetail(repairResp)
	if repairParseErr != nil {
		r := schemaViolationResult(round, repairParseErr.Error(), repairResp.Content, trail, repairResp.SessionID)
		return ParsedResponse{}, provider.AgentResponse{}, &r
	}
	return parsedRepair, repairResp, nil
}

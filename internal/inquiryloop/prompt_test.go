package inquiryloop

import (
	"strings"
	"testing"
)

// TestFormatResumeBodyShortAnswerWithSourcePath は短い answerText (上限以下) で
// "Source: <path>\n\n<answer>" の形に整形されることを確認する。
//
// r1 resume プロンプトで回答ファイルを Source パスとして参照する契約。
func TestFormatResumeBodyShortAnswerWithSourcePath(t *testing.T) {
	answer := "短い回答本文 1 行。"
	sourcePath := "/abs/path/round-1-answer.md"

	got := formatResumeBody(answer, sourcePath)

	if !strings.Contains(got, "Source: /abs/path/round-1-answer.md") {
		t.Fatalf("formatResumeBody must include 'Source: <path>' line.\nGot:\n%s", got)
	}
	if !strings.Contains(got, "短い回答本文") {
		t.Fatalf("formatResumeBody must include answer body.\nGot:\n%s", got)
	}
	if !strings.HasPrefix(got, "Source: ") {
		t.Fatalf("formatResumeBody Source line must come first.\nGot:\n%s", got)
	}
}

// TestFormatResumeBodyEmptySourcePath は sourcePath="" のとき Source 行を省略し、
// answerText だけを返すことを確認する (engine が AnswerSourcePathFunc=nil の従来挙動)。
func TestFormatResumeBodyEmptySourcePath(t *testing.T) {
	answer := "answer body without source"
	got := formatResumeBody(answer, "")
	if got != answer {
		t.Fatalf("formatResumeBody with empty path must equal answerText.\nGot: %q\nWant: %q", got, answer)
	}
}

// TestFormatResumeBodyTruncatesLongAnswer は previousAnswerMaxChars 超過の answerText が
// 切り詰められ、ellipsis マーカが末尾に付くことを確認する。
//
// stream_idle_timeout 対策の中核 (resume 入力を圧縮)。Source: パスは併記されるので、
// 本文を読みたい場合はファイル参照すれば良いという設計。
func TestFormatResumeBodyTruncatesLongAnswer(t *testing.T) {
	long := strings.Repeat("a", previousAnswerMaxChars*2)
	got := formatResumeBody(long, "/p")

	if !strings.Contains(got, "...(truncated") {
		t.Fatalf("formatResumeBody must include truncate marker for long input.\nGot length=%d", len(got))
	}
	// Source 行 + truncated 本文 + マーカ → 元長より短いこと
	if len(got) >= len(long) {
		t.Fatalf("formatResumeBody must shorten long input. got=%d, original=%d", len(got), len(long))
	}
	// 先頭は元の本文の頭から残っている
	if !strings.Contains(got, strings.Repeat("a", 100)) {
		t.Fatalf("formatResumeBody must keep the head of the original answer")
	}
}

// TestFormatResumeBodyKeepsShortAnswerVerbatim は previousAnswerMaxChars 以下の answerText が
// truncate マーカなしでそのまま乗ることを確認する。
func TestFormatResumeBodyKeepsShortAnswerVerbatim(t *testing.T) {
	short := strings.Repeat("b", previousAnswerMaxChars-100)
	got := formatResumeBody(short, "")
	if strings.Contains(got, "...(truncated") {
		t.Fatalf("formatResumeBody must NOT add truncate marker for short input.\nGot:\n%s", got)
	}
	if got != short {
		t.Fatalf("short input should round-trip exactly when no source path is given")
	}
}

// TestPromptRunnerDirective は runnerDirective() の最低契約を固定する。
//
// 目的:
//   - 各 kiro プロンプトの初回呼び出しに付加する固定文字列であり、空であってはならない。
//   - design.md / requirements 3.1 の通り、応答を json_schema 形式で返すよう促す
//     必要があるため、文字列中に "json_schema" の語が現れることを必須とする。
func TestPromptRunnerDirective(t *testing.T) {
	got := runnerDirective()
	if got == "" {
		t.Fatal("runnerDirective() returned empty string; must be a non-empty fixed string")
	}
	if !strings.Contains(got, "json_schema") {
		t.Fatalf("runnerDirective() must mention json_schema. got:\n%s", got)
	}
}

// TestPromptRunnerDirectiveIncludesHasInquiryExamples は、rilll workflow 側の
// runnerDirective() が parent-level 判断要求に倒す代表ケースを明示していることを固定する。
//
// これらは従来 kiro-impl スキルの rilll-specific セクションにしか
// 存在しなかったため、skill 外部 install 経路でも失われないよう Go 側 prompt を正本にする。
func TestPromptRunnerDirectiveIncludesHasInquiryExamples(t *testing.T) {
	got := runnerDirective()
	for _, want := range []string{
		"requirements/design に複数の有効な解釈があり、選択を誤ると実装が無効になる場合",
		"隣接 task の boundary 注記が衝突し、正しい順序が曖昧な場合",
		"検証の前提条件 (env var / build tool / repo state) が不足し、workaround が自明でない場合",
		"reviewer が同じ設計論点で却下を繰り返し、アーキテクチャ判断が必要な場合",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("runnerDirective() missing has_inquiry=true example %q.\nGot:\n%s", want, got)
		}
	}
}

// TestPromptBuildAnswerPromptIncludesPersonaAndQuestion は buildAnswerPrompt の出力に
// persona.Content と inquiry.Question が双方含まれていることを検証する。
//
// design.md の構成 (i) `# Persona` (ii) `# Inquiry` を厳密に文字列マッチで縛らず、
// 最低限「ペルソナ本文と質問本文がプロンプトに乗ること」だけを契約として固定する
// （セクション区切り文字列のリファクタを許容するため）。
func TestPromptBuildAnswerPromptIncludesPersonaAndQuestion(t *testing.T) {
	p := AnswerPersona{
		Name:       "test-persona",
		SourcePath: "embed:facets/personas/test.md",
		Content:    "PERSONA_CONTENT_MARKER_92F1\n\n価値観: 長期保守性を最優先する。",
	}
	inquiry := Inquiry{
		Title:      "サンプル判断",
		Background: "ある段で複数案がある",
		Question:   "INQUIRY_QUESTION_MARKER_7B3D",
		Options: []InquiryOption{
			{Label: "A", Description: "alpha"},
			{Label: "B", Description: "beta"},
		},
	}

	got := buildAnswerPrompt(p, AnswerContext{}, inquiry, nil)

	if !strings.Contains(got, "PERSONA_CONTENT_MARKER_92F1") {
		t.Fatalf("buildAnswerPrompt output missing persona.Content marker.\nGot:\n%s", got)
	}
	if !strings.Contains(got, "INQUIRY_QUESTION_MARKER_7B3D") {
		t.Fatalf("buildAnswerPrompt output missing inquiry.Question marker.\nGot:\n%s", got)
	}
}

func TestBuildAnswerPromptUsesOnlyAnswerPersonaContent(t *testing.T) {
	p := AnswerPersona{
		Name:       "VISIBLE_NAME_MARKER_SHOULD_NOT_APPEAR",
		SourcePath: "embed:facets/personas/VISIBLE_SOURCE_MARKER_SHOULD_NOT_APPEAR.md",
		Content:    "ANSWER_PERSONA_CONTENT_MARKER_47C2",
	}
	inquiry := Inquiry{
		Title:      "サンプル判断",
		Background: "背景",
		Question:   "質問本文",
	}

	got := buildAnswerPrompt(p, AnswerContext{}, inquiry, nil)
	want := "# Persona\n" +
		"ANSWER_PERSONA_CONTENT_MARKER_47C2\n\n" +
		"# Inquiry\n" +
		"```json\n" +
		"{\n" +
		"  \"background\": \"背景\",\n" +
		"  \"question\": \"質問本文\",\n" +
		"  \"title\": \"サンプル判断\"\n" +
		"}\n" +
		"```\n\n" +
		answerPolicySection

	if got != want {
		t.Fatalf("buildAnswerPrompt must insert only AnswerPersona.Content into the persona section.\nGot:\n%s\nWant:\n%s", got, want)
	}
	for _, marker := range []string{
		"VISIBLE_NAME_MARKER_SHOULD_NOT_APPEAR",
		"VISIBLE_SOURCE_MARKER_SHOULD_NOT_APPEAR",
	} {
		if strings.Contains(got, marker) {
			t.Fatalf("buildAnswerPrompt leaked AnswerPersona metadata marker %q into prompt.\nGot:\n%s", marker, got)
		}
	}
}

// TestPromptBuildAnswerPromptTrimsHistory は history 長 3 のとき最新 2 ラウンドのみが
// 出力に含まれ、最古のラウンドが落ちることを検証する（design.md / Performance 章
// の「最新 2 ラウンド分のみ」契約）。
//
// ラウンド本文に一意マーカーを埋め、出力中の有無で判定する。
func TestPromptBuildAnswerPromptTrimsHistory(t *testing.T) {
	p := AnswerPersona{
		Name:       "test-persona",
		SourcePath: "embed:facets/personas/test.md",
		Content:    "persona body",
	}
	inquiry := Inquiry{
		Title:    "current",
		Question: "current question",
	}
	history := []Round{
		{
			Index:      1,
			Inquiry:    Inquiry{Question: "ROUND1_INQUIRY_MARKER"},
			AnswerText: "ROUND1_ANSWER_MARKER",
		},
		{
			Index:      2,
			Inquiry:    Inquiry{Question: "ROUND2_INQUIRY_MARKER"},
			AnswerText: "ROUND2_ANSWER_MARKER",
		},
		{
			Index:      3,
			Inquiry:    Inquiry{Question: "ROUND3_INQUIRY_MARKER"},
			AnswerText: "ROUND3_ANSWER_MARKER",
		},
	}

	got := buildAnswerPrompt(p, AnswerContext{}, inquiry, history)

	// 最も古い Round 1 は含まれていてはならない（落ちることが本テストの主目的）。
	for _, marker := range []string{"ROUND1_INQUIRY_MARKER", "ROUND1_ANSWER_MARKER"} {
		if strings.Contains(got, marker) {
			t.Fatalf("buildAnswerPrompt must drop the oldest round when len(history) > 2; "+
				"unexpected marker %q present.\nGot:\n%s", marker, got)
		}
	}
	// 最新 2 ラウンド (Round 2 / Round 3) は必ず含まれる。
	for _, marker := range []string{
		"ROUND2_INQUIRY_MARKER", "ROUND2_ANSWER_MARKER",
		"ROUND3_INQUIRY_MARKER", "ROUND3_ANSWER_MARKER",
	} {
		if !strings.Contains(got, marker) {
			t.Fatalf("buildAnswerPrompt missing recent round marker %q.\nGot:\n%s", marker, got)
		}
	}
}

// TestBuildAnswerPromptIncludesContextBlock は AnswerContext が non-zero のとき
// `# Context` セクションが構築され、PromptCommand / SpecPaths / Background が
// すべて出力に含まれることを検証する。
//
// 設計契約: 回答エージェント (session B) は毎ラウンド fresh session で呼ばれるため、
// 「どの feature / どのタスクの問い合わせか」「参照すべき spec ファイルはどこか」を
// workflow 層から明示的に再注入しないと判別できない。本テストはその注入経路を固定する。
func TestBuildAnswerPromptIncludesContextBlock(t *testing.T) {
	p := AnswerPersona{
		Name:       "test-persona",
		SourcePath: "embed:facets/personas/test.md",
		Content:    "persona body",
	}
	ctx := AnswerContext{
		PromptCommand: "/kiro-impl agent-learnings 5.1",
		SpecPaths: []string{
			"/abs/path/.kiro/specs/agent-learnings/requirements.md",
			"/abs/path/.kiro/specs/agent-learnings/design.md",
			"/abs/path/.kiro/specs/agent-learnings/tasks.md",
		},
		Background: "BG_MARKER_4F2A",
	}
	inquiry := Inquiry{
		Title:    "current",
		Question: "current question",
	}

	got := buildAnswerPrompt(p, ctx, inquiry, nil)

	if !strings.Contains(got, "# Context") {
		t.Fatalf("buildAnswerPrompt missing `# Context` heading when AnswerContext is non-zero.\nGot:\n%s", got)
	}
	if !strings.Contains(got, "/kiro-impl agent-learnings 5.1") {
		t.Fatalf("buildAnswerPrompt missing PromptCommand in `# Context`.\nGot:\n%s", got)
	}
	for _, p := range ctx.SpecPaths {
		if !strings.Contains(got, p) {
			t.Fatalf("buildAnswerPrompt missing SpecPath %q in `# Context`.\nGot:\n%s", p, got)
		}
	}
	if !strings.Contains(got, "BG_MARKER_4F2A") {
		t.Fatalf("buildAnswerPrompt missing Background marker in `# Context`.\nGot:\n%s", got)
	}
}

// TestBuildAnswerPromptOmitsContextBlockWhenEmpty は AnswerContext が zero value のとき
// `# Context` ブロック自体が出力に現れないことを検証する (従来挙動互換)。
//
// AnswerContext を設定しない既存呼び出し元 (テスト含む) が無変更で動く保証。
func TestBuildAnswerPromptOmitsContextBlockWhenEmpty(t *testing.T) {
	p := AnswerPersona{
		Name:       "test-persona",
		SourcePath: "embed:facets/personas/test.md",
		Content:    "persona body",
	}
	inquiry := Inquiry{
		Title:    "current",
		Question: "current question",
	}

	got := buildAnswerPrompt(p, AnswerContext{}, inquiry, nil)

	if strings.Contains(got, "# Context") {
		t.Fatalf("buildAnswerPrompt must NOT emit `# Context` heading when AnswerContext is zero value.\nGot:\n%s", got)
	}
}

// TestBuildAnswerPromptContextSubsectionsOptional は AnswerContext のサブフィールドが
// 部分的に空のとき、対応するサブセクションだけが省かれ、設定されたサブセクションは
// 残ることを検証する。
//
// 検証ケース:
//   - PromptCommand のみ → SpecPaths / Background サブセクションは出ない
//   - SpecPaths のみ → PromptCommand / Background サブセクションは出ない
//   - Background のみ → PromptCommand / SpecPaths サブセクションは出ない
func TestBuildAnswerPromptContextSubsectionsOptional(t *testing.T) {
	p := AnswerPersona{
		Name:       "test-persona",
		SourcePath: "embed:facets/personas/test.md",
		Content:    "persona body",
	}
	inquiry := Inquiry{Title: "t", Question: "q"}

	cases := []struct {
		name        string
		ctx         AnswerContext
		mustHave    []string
		mustNotHave []string
	}{
		{
			name:        "only PromptCommand",
			ctx:         AnswerContext{PromptCommand: "/cmd-marker-A"},
			mustHave:    []string{"# Context", "/cmd-marker-A"},
			mustNotHave: []string{"参照可能なファイル", "追加文脈"},
		},
		{
			name:        "only SpecPaths",
			ctx:         AnswerContext{SpecPaths: []string{"/abs/path/marker-B.md"}},
			mustHave:    []string{"# Context", "/abs/path/marker-B.md", "参照可能なファイル"},
			mustNotHave: []string{"元コマンド", "追加文脈"},
		},
		{
			name:        "only Background",
			ctx:         AnswerContext{Background: "background-marker-C"},
			mustHave:    []string{"# Context", "background-marker-C", "追加文脈"},
			mustNotHave: []string{"元コマンド", "参照可能なファイル"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildAnswerPrompt(p, tc.ctx, inquiry, nil)
			for _, m := range tc.mustHave {
				if !strings.Contains(got, m) {
					t.Errorf("expected output to contain %q.\nGot:\n%s", m, got)
				}
			}
			for _, m := range tc.mustNotHave {
				if strings.Contains(got, m) {
					t.Errorf("expected output to NOT contain %q.\nGot:\n%s", m, got)
				}
			}
		})
	}
}

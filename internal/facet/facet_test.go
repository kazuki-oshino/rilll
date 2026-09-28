package facet

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestEmbeddedFacetPlaceholders(t *testing.T) {
	paths := []string{
		"facets/personas/placeholder.md",
		"facets/policies/placeholder.md",
		"facets/knowledge/placeholder.md",
		"facets/output-contracts/placeholder.md",
	}

	for _, path := range paths {
		content, err := fs.ReadFile(embeddedFS, path)
		if err != nil {
			t.Fatalf("read embedded placeholder %s: %v", path, err)
		}
		if !strings.Contains(string(content), "prompt-facets-foundation") {
			t.Fatalf("placeholder %s does not identify prompt-facets-foundation: %q", path, content)
		}
	}
}

func TestFacetKindDir(t *testing.T) {
	tests := []struct {
		name string
		kind FacetKind
		want string
	}{
		{name: "persona", kind: Persona, want: "personas"},
		{name: "policy", kind: Policy, want: "policies"},
		{name: "knowledge", kind: Knowledge, want: "knowledge"},
		{name: "output contract", kind: OutputContract, want: "output-contracts"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := tt.kind.dir()
			if !ok {
				t.Fatalf("dir() ok = false, want true")
			}
			if got != tt.want {
				t.Fatalf("dir() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFacetKindString(t *testing.T) {
	tests := []struct {
		name string
		kind FacetKind
		want string
	}{
		{name: "persona", kind: Persona, want: "Persona"},
		{name: "policy", kind: Policy, want: "Policy"},
		{name: "knowledge", kind: Knowledge, want: "Knowledge"},
		{name: "output contract", kind: OutputContract, want: "OutputContract"},
		{name: "unknown", kind: FacetKind(99), want: "facet:unknown(99)"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.kind.String(); got != tt.want {
				t.Fatalf("String() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestResolvedFacetFields(t *testing.T) {
	resolved := ResolvedFacet{
		Content:    "content",
		SourcePath: "embed:facets/policies/test.md",
		Kind:       Policy,
		Name:       "test",
	}

	if resolved.Content != "content" {
		t.Fatalf("Content = %q, want %q", resolved.Content, "content")
	}
	if resolved.SourcePath != "embed:facets/policies/test.md" {
		t.Fatalf("SourcePath = %q, want %q", resolved.SourcePath, "embed:facets/policies/test.md")
	}
	if resolved.Kind != Policy {
		t.Fatalf("Kind = %v, want %v", resolved.Kind, Policy)
	}
	if resolved.Name != "test" {
		t.Fatalf("Name = %q, want %q", resolved.Name, "test")
	}
}

func TestErrFacetNotFoundSentinel(t *testing.T) {
	if !errors.Is(ErrFacetNotFound, ErrFacetNotFound) {
		t.Fatal("ErrFacetNotFound is not errors.Is-compatible with itself")
	}
}

func TestContextMaxChars(t *testing.T) {
	const maxChars = CONTEXT_MAX_CHARS
	if maxChars != 2000 {
		t.Fatalf("CONTEXT_MAX_CHARS = %d, want 2000", maxChars)
	}
}

// TestResolveFacetEmbedFallback: override が存在しない cwd で各 kind の embed
// placeholder が解決され、SourcePath が embed: プレフィクス形式であること (R2.3, R2.5)。
func TestResolveFacetEmbedFallback(t *testing.T) {
	tests := []struct {
		name    string
		kind    FacetKind
		kindDir string
	}{
		{name: "persona", kind: Persona, kindDir: "personas"},
		{name: "policy", kind: Policy, kindDir: "policies"},
		{name: "knowledge", kind: Knowledge, kindDir: "knowledge"},
		{name: "output contract", kind: OutputContract, kindDir: "output-contracts"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cwd := t.TempDir() // override 不在
			got, err := ResolveFacet(cwd, tt.kind, "placeholder")
			if err != nil {
				t.Fatalf("ResolveFacet error = %v, want nil", err)
			}
			wantSource := "embed:facets/" + tt.kindDir + "/placeholder.md"
			if got.SourcePath != wantSource {
				t.Fatalf("SourcePath = %q, want %q", got.SourcePath, wantSource)
			}
			if got.Kind != tt.kind {
				t.Fatalf("Kind = %v, want %v", got.Kind, tt.kind)
			}
			if got.Name != "placeholder" {
				t.Fatalf("Name = %q, want %q", got.Name, "placeholder")
			}
			if !strings.Contains(got.Content, "prompt-facets-foundation") {
				t.Fatalf("Content does not identify prompt-facets-foundation: %q", got.Content)
			}
		})
	}
}

// TestResolveFacetSpecLoopPersonas: kiro-spec-loop-facets で追加した
// `spec-analyst` / `spec-architect` / `validator` の 3 persona が embed 経路で
// `ErrFacetNotFound` を返さず非空 content を返すこと (Req 2.1, 2.4 / task 1.1-1.3
// で同梱した embed Markdown が `ResolveFacet(facet.Persona, ...)` から解決可能で
// あることを直接検証)。
//
// 各 persona の冒頭には対応する役割宣言 (`# Persona: <name>`) が含まれることを
// 構造マーカとして確認する。これにより 1.1-1.3 のいずれかの Markdown が消失
// したり別 persona 内容に差し替わった場合に test 経由で検出できる。
func TestResolveFacetSpecLoopPersonas(t *testing.T) {
	tests := []struct {
		name       string
		facetName  string
		wantMarker string
	}{
		{name: "spec-analyst", facetName: "spec-analyst", wantMarker: "# Persona: spec-analyst"},
		{name: "spec-architect", facetName: "spec-architect", wantMarker: "# Persona: spec-architect"},
		{name: "validator", facetName: "validator", wantMarker: "# Persona: validator"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cwd := t.TempDir() // override 不在 → embed 経路に落ちる
			got, err := ResolveFacet(cwd, Persona, tt.facetName)
			if err != nil {
				t.Fatalf("ResolveFacet(Persona, %q) error = %v, want nil", tt.facetName, err)
			}
			if errors.Is(err, ErrFacetNotFound) {
				t.Fatalf("ResolveFacet(Persona, %q) returned ErrFacetNotFound, want non-nil resolved facet", tt.facetName)
			}
			if got.Content == "" {
				t.Fatalf("ResolveFacet(Persona, %q): Content is empty, want non-empty embed content", tt.facetName)
			}
			wantSource := "embed:facets/personas/" + tt.facetName + ".md"
			if got.SourcePath != wantSource {
				t.Fatalf("SourcePath = %q, want %q", got.SourcePath, wantSource)
			}
			if got.Kind != Persona {
				t.Fatalf("Kind = %v, want %v", got.Kind, Persona)
			}
			if got.Name != tt.facetName {
				t.Fatalf("Name = %q, want %q", got.Name, tt.facetName)
			}
			if !strings.Contains(got.Content, tt.wantMarker) {
				t.Fatalf("Content does not contain marker %q (persona Markdown が別内容に差し替わった可能性):\n%s", tt.wantMarker, got.Content)
			}
		})
	}
}

// TestResolveFacetSpecLoopPersonaOverride: kiro-spec-loop-facets で追加した
// persona に対し、project override
// (`<cwd>/.rilll-config/facets/personas/<name>.md`) を配置すると override 内容が
// 返り、embed 由来 marker が混入しないこと (Req 5.2 / task 6.7 mock seam の前提)。
//
// 3 persona のうち代表として `spec-analyst` 1 ケースで override 経路を検証する。
// override 経路の先勝ロジック自体は既存 `TestResolveFacetOverridePrecedence`
// (placeholder で検証済) と同一実装なので、ここは "本 spec で追加した persona に
// 対しても同経路が適用される" ことを陰性確認 (embed marker 不混入) 込みで担保する。
func TestResolveFacetSpecLoopPersonaOverride(t *testing.T) {
	const facetName = "spec-analyst"
	const embedOnlyMarker = "# Persona: spec-analyst" // embed 由来 marker (上書き時には消えるべき)
	const overrideBody = "# project override for personas/spec-analyst\nthis content must win over embed builtin.\n"

	tempDir := t.TempDir()
	overrideDir := filepath.Join(tempDir, ".rilll-config", "facets", "personas")
	if err := os.MkdirAll(overrideDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	overridePath := filepath.Join(overrideDir, facetName+".md")
	if err := os.WriteFile(overridePath, []byte(overrideBody), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got, err := ResolveFacet(tempDir, Persona, facetName)
	if err != nil {
		t.Fatalf("ResolveFacet error = %v, want nil", err)
	}
	if got.Content != overrideBody {
		t.Fatalf("Content = %q, want %q (project override が embed builtin より優先されるべき)", got.Content, overrideBody)
	}
	if got.SourcePath != overridePath {
		t.Fatalf("SourcePath = %q, want %q (override 解決時は実ファイル絶対パス)", got.SourcePath, overridePath)
	}
	if !filepath.IsAbs(got.SourcePath) {
		t.Fatalf("SourcePath %q is not absolute", got.SourcePath)
	}
	if got.Kind != Persona {
		t.Fatalf("Kind = %v, want %v", got.Kind, Persona)
	}
	if got.Name != facetName {
		t.Fatalf("Name = %q, want %q", got.Name, facetName)
	}
	// embed builtin の冒頭マーカが漏れて返っていないこと (override 優先の陰性確認)
	if strings.Contains(got.Content, embedOnlyMarker) {
		t.Fatalf("Content に embed builtin マーカ %q が含まれる: override 内容が返るべき: %q", embedOnlyMarker, got.Content)
	}
}

// TestResolveFacetEmbedOutputContractPlan: builtin plan 雛形が embed 経由で
// 解決でき、Phase 2 指示文に素のまま埋め込まれる前提のタスクリスト型構造
// (Goals / Steps / Open Questions 見出し + 行頭マーカ) を持つことを検証する
// (R8.1, R8.2, R8.4 / task 4.1)。
func TestResolveFacetEmbedOutputContractPlan(t *testing.T) {
	cwd := t.TempDir() // override 不在 → embed 経路に落ちる

	got, err := ResolveFacet(cwd, OutputContract, "plan")
	if err != nil {
		t.Fatalf("ResolveFacet(OutputContract, \"plan\") error = %v, want nil", err)
	}
	wantSource := "embed:facets/output-contracts/plan.md"
	if got.SourcePath != wantSource {
		t.Fatalf("SourcePath = %q, want %q", got.SourcePath, wantSource)
	}
	if got.Kind != OutputContract {
		t.Fatalf("Kind = %v, want %v", got.Kind, OutputContract)
	}
	if got.Name != "plan" {
		t.Fatalf("Name = %q, want %q", got.Name, "plan")
	}

	// 構造マーカ: タスクリスト型 (Goals / Steps / Open Questions 見出し + 行頭マーカ)
	requiredMarkers := []string{
		"## Goals",
		"## Steps",
		"## Open Questions",
		"\n- ", // 行頭マーカ (リスト) が少なくとも 1 つ存在する
	}
	for _, marker := range requiredMarkers {
		if !strings.Contains(got.Content, marker) {
			t.Errorf("plan.md does not contain required structural marker %q\ncontent:\n%s", marker, got.Content)
		}
	}

	// template DSL を持ち込まない (Go text/template / Mustache 等の構文を含まない)
	forbiddenTokens := []string{"{{", "}}"}
	for _, token := range forbiddenTokens {
		if strings.Contains(got.Content, token) {
			t.Errorf("plan.md contains forbidden template DSL token %q (素のまま埋め込み前提のため禁止)\ncontent:\n%s", token, got.Content)
		}
	}

	// kebab-case ファイル名 + isInvalidName を通過すること
	if isInvalidName("plan") {
		t.Fatalf("isInvalidName(\"plan\") = true, want false (kebab-case + 予約衝突なし)")
	}
}

// TestResolveFacetEmbedOutputContractReviewGather: builtin review-gather 雛形が
// embed 経由で解決でき、Phase 2 指示文に素のまま埋め込まれる前提の
// review 結果収集型構造 (Findings 表 + Severity 列 + Citation) を持つことを検証する
// (R8.1, R8.2, R8.4 / task 4.2)。
func TestResolveFacetEmbedOutputContractReviewGather(t *testing.T) {
	cwd := t.TempDir() // override 不在 → embed 経路に落ちる

	got, err := ResolveFacet(cwd, OutputContract, "review-gather")
	if err != nil {
		t.Fatalf("ResolveFacet(OutputContract, \"review-gather\") error = %v, want nil", err)
	}
	wantSource := "embed:facets/output-contracts/review-gather.md"
	if got.SourcePath != wantSource {
		t.Fatalf("SourcePath = %q, want %q", got.SourcePath, wantSource)
	}
	if got.Kind != OutputContract {
		t.Fatalf("Kind = %v, want %v", got.Kind, OutputContract)
	}
	if got.Name != "review-gather" {
		t.Fatalf("Name = %q, want %q", got.Name, "review-gather")
	}

	// 構造マーカ: review 結果収集型 (Findings 見出し + Severity 列 + Citation 言及)
	requiredMarkers := []string{
		"## Findings",
		"Severity",
		"Citation",
		"|", // Markdown 表記法のテーブル区切り (Findings 表が存在する)
	}
	for _, marker := range requiredMarkers {
		if !strings.Contains(got.Content, marker) {
			t.Errorf("review-gather.md does not contain required structural marker %q\ncontent:\n%s", marker, got.Content)
		}
	}

	// template DSL を持ち込まない (Go text/template / Mustache 等の構文を含まない)
	forbiddenTokens := []string{"{{", "}}"}
	for _, token := range forbiddenTokens {
		if strings.Contains(got.Content, token) {
			t.Errorf("review-gather.md contains forbidden template DSL token %q (素のまま埋め込み前提のため禁止)\ncontent:\n%s", token, got.Content)
		}
	}

	// kebab-case ファイル名 + isInvalidName を通過すること
	if isInvalidName("review-gather") {
		t.Fatalf("isInvalidName(\"review-gather\") = true, want false (kebab-case + 予約衝突なし)")
	}
}

// TestResolveFacetEmbedOutputContractResearchReport: builtin research-report 雛形が
// embed 経由で解決でき、Phase 2 指示文に素のまま埋め込まれる前提の
// 調査報告型構造 (Question / Findings / Sources / Recommendations) を持つことを検証する
// (R8.1, R8.2, R8.4 / task 4.3)。
func TestResolveFacetEmbedOutputContractResearchReport(t *testing.T) {
	cwd := t.TempDir() // override 不在 → embed 経路に落ちる

	got, err := ResolveFacet(cwd, OutputContract, "research-report")
	if err != nil {
		t.Fatalf("ResolveFacet(OutputContract, \"research-report\") error = %v, want nil", err)
	}
	wantSource := "embed:facets/output-contracts/research-report.md"
	if got.SourcePath != wantSource {
		t.Fatalf("SourcePath = %q, want %q", got.SourcePath, wantSource)
	}
	if got.Kind != OutputContract {
		t.Fatalf("Kind = %v, want %v", got.Kind, OutputContract)
	}
	if got.Name != "research-report" {
		t.Fatalf("Name = %q, want %q", got.Name, "research-report")
	}

	// 構造マーカ: 調査報告型 (Question / Findings / Sources / Recommendations 見出し
	// + 行頭マーカ)。tasks.md の task 4.3 で示された構造に対応する。
	requiredMarkers := []string{
		"## Question",
		"## Findings",
		"## Sources",
		"## Recommendations",
		"\n- ", // 行頭マーカ (リスト) が少なくとも 1 つ存在する
	}
	for _, marker := range requiredMarkers {
		if !strings.Contains(got.Content, marker) {
			t.Errorf("research-report.md does not contain required structural marker %q\ncontent:\n%s", marker, got.Content)
		}
	}

	// template DSL を持ち込まない (Go text/template / Mustache 等の構文を含まない)
	forbiddenTokens := []string{"{{", "}}"}
	for _, token := range forbiddenTokens {
		if strings.Contains(got.Content, token) {
			t.Errorf("research-report.md contains forbidden template DSL token %q (素のまま埋め込み前提のため禁止)\ncontent:\n%s", token, got.Content)
		}
	}

	// kebab-case ファイル名 + isInvalidName を通過すること
	if isInvalidName("research-report") {
		t.Fatalf("isInvalidName(\"research-report\") = true, want false (kebab-case + 予約衝突なし)")
	}
}

// TestResolveFacetEmbedPolicyAIAntipatternGuard は impl-direction answerer 向けの
// ai-antipattern 短縮 Policy が embed 経由で解決可能であることを固定する。
func TestResolveFacetEmbedPolicyAIAntipatternGuard(t *testing.T) {
	cwd := t.TempDir()

	got, err := ResolveFacet(cwd, Policy, "ai-antipattern-guard")
	if err != nil {
		t.Fatalf("ResolveFacet(Policy, \"ai-antipattern-guard\") error = %v, want nil", err)
	}
	if got.Kind != Policy {
		t.Fatalf("Kind = %v, want %v", got.Kind, Policy)
	}
	if got.SourcePath != "embed:facets/policies/ai-antipattern-guard.md" {
		t.Fatalf("SourcePath = %q, want embed policy path", got.SourcePath)
	}
	for _, marker := range []string{
		"[policy:ai-antipattern-guard]",
		"配線忘れ",
		"スコープクリープ",
		"フォールバック",
	} {
		if !strings.Contains(got.Content, marker) {
			t.Fatalf("embedded ai-antipattern guard missing marker %q:\n%s", marker, got.Content)
		}
	}
}

// TestResolveFacetOutputContractOverridePrecedence: OutputContract kind について、
// project override (`<cwd>/.rilll-config/facets/output-contracts/<name>.md`) が
// embed builtin より優先して解決されることと、override 不在時は embed builtin
// (task 4.1 で同梱した `plan.md`) が解決されることを smoke test で確認する
// (R8.3 / task 4.4)。
//
// 既存 ResolveFacet 実装 (Spec A `prompt-facets-foundation`) の OutputContract
// kind に対する embed glob (`facets/output-contracts/*.md`) と override path 規約
// (`.rilll-config/facets/output-contracts/`) の整合確認に留め、新規ロジックは
// 追加しない。
func TestResolveFacetOutputContractOverridePrecedence(t *testing.T) {
	const facetName = "plan"
	const wantEmbedSource = "embed:facets/output-contracts/plan.md"
	// embed plan.md の構造マーカ (task 4.1 同梱物に必ず含まれる)。
	// override 経路で resolve したときに embed body が漏れていないことの陰性確認にも使う。
	const embedOnlyMarker = "## Goals"

	t.Run("override absent resolves embed builtin", func(t *testing.T) {
		cwd := t.TempDir() // override 不在 → embed 経路にフォールスルー
		got, err := ResolveFacet(cwd, OutputContract, facetName)
		if err != nil {
			t.Fatalf("ResolveFacet error = %v, want nil", err)
		}
		if got.SourcePath != wantEmbedSource {
			t.Fatalf("SourcePath = %q, want %q (embed builtin)", got.SourcePath, wantEmbedSource)
		}
		if got.Kind != OutputContract {
			t.Fatalf("Kind = %v, want %v", got.Kind, OutputContract)
		}
		if got.Name != facetName {
			t.Fatalf("Name = %q, want %q", got.Name, facetName)
		}
		if !strings.Contains(got.Content, embedOnlyMarker) {
			t.Fatalf("embed plan.md does not contain marker %q; embed glob と builtin 同梱が壊れている可能性", embedOnlyMarker)
		}
	})

	t.Run("override present wins over embed builtin", func(t *testing.T) {
		cwd := t.TempDir()
		overrideDir := filepath.Join(cwd, ".rilll-config", "facets", "output-contracts")
		if err := os.MkdirAll(overrideDir, 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		overridePath := filepath.Join(overrideDir, facetName+".md")
		// embed builtin と意図的に異なる body にして、戻り値が override 由来であることを判定可能にする。
		const overrideBody = "# project override for output-contracts/plan\nthis content must win over embed builtin.\n"
		if err := os.WriteFile(overridePath, []byte(overrideBody), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}

		got, err := ResolveFacet(cwd, OutputContract, facetName)
		if err != nil {
			t.Fatalf("ResolveFacet error = %v, want nil", err)
		}
		if got.Content != overrideBody {
			t.Fatalf("Content = %q, want %q (project override が embed builtin より優先されるべき)", got.Content, overrideBody)
		}
		if got.SourcePath != overridePath {
			t.Fatalf("SourcePath = %q, want %q (override 解決時は実ファイル絶対パス)", got.SourcePath, overridePath)
		}
		if !filepath.IsAbs(got.SourcePath) {
			t.Fatalf("SourcePath %q is not absolute", got.SourcePath)
		}
		if got.Kind != OutputContract {
			t.Fatalf("Kind = %v, want %v", got.Kind, OutputContract)
		}
		if got.Name != facetName {
			t.Fatalf("Name = %q, want %q", got.Name, facetName)
		}
		// embed builtin の内容 ('## Goals') が漏れて返っていないこと
		// (override が優先されていれば embed body は混入しない陰性確認)
		if strings.Contains(got.Content, embedOnlyMarker) {
			t.Fatalf("Content に embed builtin マーカ %q が含まれる: override 内容が返るべき: %q", embedOnlyMarker, got.Content)
		}
	})
}

// TestResolveFacetProjectOverride: project override ファイルが存在するとき、その
// 内容が返され SourcePath が絶対パスであること (R2.2, R3.1, R3.2)。
func TestResolveFacetProjectOverride(t *testing.T) {
	tempDir := t.TempDir()
	overrideDir := filepath.Join(tempDir, ".rilll-config", "facets", "policies")
	if err := os.MkdirAll(overrideDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	overridePath := filepath.Join(overrideDir, "test.md")
	wantContent := "# override policy\n本ファイルは project override の検証用。\n"
	if err := os.WriteFile(overridePath, []byte(wantContent), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got, err := ResolveFacet(tempDir, Policy, "test")
	if err != nil {
		t.Fatalf("ResolveFacet error = %v, want nil", err)
	}
	if got.Content != wantContent {
		t.Fatalf("Content = %q, want %q", got.Content, wantContent)
	}
	if got.SourcePath != overridePath {
		t.Fatalf("SourcePath = %q, want %q", got.SourcePath, overridePath)
	}
	if !filepath.IsAbs(got.SourcePath) {
		t.Fatalf("SourcePath %q is not absolute", got.SourcePath)
	}
	if got.Kind != Policy {
		t.Fatalf("Kind = %v, want %v", got.Kind, Policy)
	}
	if got.Name != "test" {
		t.Fatalf("Name = %q, want %q", got.Name, "test")
	}
}

// TestResolveFacetOverridePrecedence: 同一 name に embed と override が両方
// 存在するとき override が優先されること (R2.4)。
func TestResolveFacetOverridePrecedence(t *testing.T) {
	tempDir := t.TempDir()
	overrideDir := filepath.Join(tempDir, ".rilll-config", "facets", "personas")
	if err := os.MkdirAll(overrideDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	overridePath := filepath.Join(overrideDir, "placeholder.md")
	wantContent := "override-wins"
	if err := os.WriteFile(overridePath, []byte(wantContent), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got, err := ResolveFacet(tempDir, Persona, "placeholder")
	if err != nil {
		t.Fatalf("ResolveFacet error = %v, want nil", err)
	}
	if got.Content != wantContent {
		t.Fatalf("Content = %q, want %q (override should win over embed)", got.Content, wantContent)
	}
	if got.SourcePath != overridePath {
		t.Fatalf("SourcePath = %q, want %q", got.SourcePath, overridePath)
	}
}

// TestResolveFacetNotFound: override / embed 双方に存在しない name で
// errors.Is(err, ErrFacetNotFound) == true (R4.1, R4.2)。
func TestResolveFacetNotFound(t *testing.T) {
	cwd := t.TempDir()
	kinds := []FacetKind{Persona, Policy, Knowledge, OutputContract}
	for _, kind := range kinds {
		t.Run(kind.String(), func(t *testing.T) {
			_, err := ResolveFacet(cwd, kind, "does-not-exist-anywhere")
			if !errors.Is(err, ErrFacetNotFound) {
				t.Fatalf("err = %v, want errors.Is ErrFacetNotFound", err)
			}
		})
	}
}

// TestResolveFacetInvalidName: 不正 name 4 パターンで ErrFacetNotFound に丸めること
// (path traversal 防御 / Revalidation Triggers 対象)。
func TestResolveFacetInvalidName(t *testing.T) {
	cwd := t.TempDir()
	invalids := []struct {
		label string
		name  string
	}{
		{label: "empty", name: ""},
		{label: "parent dir", name: "../etc/passwd"},
		{label: "slash", name: "foo/bar"},
		{label: "backslash", name: "foo\\bar"},
	}
	for _, tt := range invalids {
		t.Run(tt.label, func(t *testing.T) {
			_, err := ResolveFacet(cwd, Persona, tt.name)
			if !errors.Is(err, ErrFacetNotFound) {
				t.Fatalf("invalid name %q: err = %v, want errors.Is ErrFacetNotFound", tt.name, err)
			}
		})
	}
}

// TestResolveFacetUnknownKind: 未知 kind は ErrFacetNotFound に丸めること
// (FacetKind.dir() が ok=false を返すケースの境界保護)。
func TestResolveFacetUnknownKind(t *testing.T) {
	cwd := t.TempDir()
	_, err := ResolveFacet(cwd, FacetKind(99), "placeholder")
	if !errors.Is(err, ErrFacetNotFound) {
		t.Fatalf("unknown kind: err = %v, want errors.Is ErrFacetNotFound", err)
	}
}

// TestResolveFacetIOErrorNotFolded: override 側 I/O エラー (権限エラー) が
// sentinel に丸められず "facet: read override" プレフィクスで wrap されて伝播すること
// (Error Handling 整合 / Unix 専用)。
func TestResolveFacetIOErrorNotFolded(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission bit test is Unix-only")
	}
	if os.Geteuid() == 0 {
		t.Skip("root can bypass file permissions; skip")
	}

	tempDir := t.TempDir()
	overrideDir := filepath.Join(tempDir, ".rilll-config", "facets", "policies")
	if err := os.MkdirAll(overrideDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	overridePath := filepath.Join(overrideDir, "guarded.md")
	if err := os.WriteFile(overridePath, []byte("secret"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.Chmod(overridePath, 0o000); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	// テスト終了時に削除できるよう権限を戻す
	t.Cleanup(func() {
		_ = os.Chmod(overridePath, 0o644)
	})

	_, err := ResolveFacet(tempDir, Policy, "guarded")
	if err == nil {
		t.Fatal("expected error from unreadable override, got nil")
	}
	if errors.Is(err, ErrFacetNotFound) {
		t.Fatalf("I/O permission error should NOT be folded into ErrFacetNotFound: %v", err)
	}
	if !strings.HasPrefix(err.Error(), "facet: read override") {
		t.Fatalf("err = %q, want prefix %q", err.Error(), "facet: read override")
	}
	// fs ErrPermission を wrap で内側に保持していること
	if !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("err = %v, want errors.Is fs.ErrPermission", err)
	}
}

// TestResolveFacetEmptyCwdSkipsOverride: cwd == "" のとき override 探索を
// スキップし embed のみ解決すること (R2.1 補助 / 既存 internal/persona と同方針)。
func TestResolveFacetEmptyCwdSkipsOverride(t *testing.T) {
	got, err := ResolveFacet("", Persona, "placeholder")
	if err != nil {
		t.Fatalf("ResolveFacet error = %v, want nil", err)
	}
	wantSource := "embed:facets/personas/placeholder.md"
	if got.SourcePath != wantSource {
		t.Fatalf("SourcePath = %q, want %q", got.SourcePath, wantSource)
	}
}

// TestTrimContextShort: rune 数 ≤ maxChars で content がそのまま返ること (R5.1)。
func TestTrimContextShort(t *testing.T) {
	tests := []struct {
		name     string
		content  string
		maxChars int
	}{
		{name: "ascii equal", content: "hello", maxChars: 5},
		{name: "ascii less", content: "hi", maxChars: 100},
		{name: "japanese equal", content: "あいう", maxChars: 3},
		{name: "japanese less", content: "あいう", maxChars: 10},
		{name: "empty", content: "", maxChars: 10},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := TrimContext(tt.content, tt.maxChars, "/abs/path.md")
			if got != tt.content {
				t.Fatalf("TrimContext = %q, want %q (短文は content そのまま)", got, tt.content)
			}
		})
	}
}

// TestTrimContextLongWithSourcePath: 長文かつ sourcePath 非空のとき、先頭
// maxChars rune に "...(truncated, full at <sourcePath>)" マーカを付けて返すこと (R5.2)。
func TestTrimContextLongWithSourcePath(t *testing.T) {
	content := strings.Repeat("a", 100)
	sourcePath := "/abs/path.md"
	got := TrimContext(content, 10, sourcePath)
	want := strings.Repeat("a", 10) + "...(truncated, full at " + sourcePath + ")"
	if got != want {
		t.Fatalf("TrimContext = %q, want %q", got, want)
	}
}

// TestTrimContextLongWithoutSourcePath: 長文かつ sourcePath 空のとき、先頭
// maxChars rune に "...(truncated)" マーカのみを付けて返し、末尾に
// "full at " が裸で残らないこと (R5.2 graceful degradation)。
func TestTrimContextLongWithoutSourcePath(t *testing.T) {
	content := strings.Repeat("a", 100)
	got := TrimContext(content, 10, "")
	want := strings.Repeat("a", 10) + "...(truncated)"
	if got != want {
		t.Fatalf("TrimContext = %q, want %q", got, want)
	}
	if strings.Contains(got, "full at ") {
		t.Fatalf("sourcePath 空時に 'full at ' が残っている: %q", got)
	}
}

// TestTrimContextRuneBoundary: 日本語マルチバイト content (rune 数 2500) を
// maxChars=2000 で切ったとき、戻り値が UTF-8 として valid で先頭 2000 rune が
// "あ" の連続であること (R5.2)。
func TestTrimContextRuneBoundary(t *testing.T) {
	content := strings.Repeat("あ", 2500)
	got := TrimContext(content, 2000, "/abs/path.md")
	if !utf8.ValidString(got) {
		t.Fatalf("TrimContext の戻り値が UTF-8 として invalid: %q", got)
	}
	wantHead := strings.Repeat("あ", 2000)
	if !strings.HasPrefix(got, wantHead) {
		t.Fatalf("先頭 2000 rune が 'あ' の連続でない: head=%q", got[:utf8.RuneLen('あ')*10])
	}
	wantMarker := "...(truncated, full at /abs/path.md)"
	if !strings.HasSuffix(got, wantMarker) {
		t.Fatalf("末尾マーカが期待形式でない: tail=%q, want suffix %q", got[len(got)-len(wantMarker):], wantMarker)
	}
	// 先頭 head + マーカ の組み立てがズレていないこと
	if got != wantHead+wantMarker {
		t.Fatalf("TrimContext = %q (length=%d), want head+marker exact match", got, utf8.RuneCountInString(got))
	}
}

// TestTrimContextPure: 同一引数 2 回呼び出しで同一結果 (R5.3)。
func TestTrimContextPure(t *testing.T) {
	content := strings.Repeat("X", 5000) + "あ"
	sourcePath := "embed:facets/policies/foo.md"
	first := TrimContext(content, 2000, sourcePath)
	second := TrimContext(content, 2000, sourcePath)
	if first != second {
		t.Fatalf("同一引数で結果が異なる: first=%q, second=%q", first, second)
	}
}

// TestTrimContextDefensive: maxChars <= 0 で content がそのまま返ること
// (純関数として安全側 / 防御分岐)。
func TestTrimContextDefensive(t *testing.T) {
	content := "some long content " + strings.Repeat("z", 50)
	tests := []struct {
		name     string
		maxChars int
	}{
		{name: "zero", maxChars: 0},
		{name: "negative", maxChars: -1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := TrimContext(content, tt.maxChars, "/abs/path.md")
			if got != content {
				t.Fatalf("maxChars=%d: TrimContext = %q, want %q", tt.maxChars, got, content)
			}
		})
	}
}

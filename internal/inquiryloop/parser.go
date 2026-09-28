package inquiryloop

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/kazuki-oshino/rilll/internal/provider"
)

// ErrSchemaViolation は agent 応答が json_schema (Schema()) に適合しなかったことを示す sentinel エラー。
//
// 適合しないケース:
//   - StructuredOutput が nil で、Content からも JSON ブロックを抽出できない
//   - StructuredOutput が map[string]any 以外の型
//   - has_inquiry==true だが inquiry オブジェクトが欠落 / nil
//
// design.md「Implementation Notes / Risks」と tasks.md 2.2「parseResponse 失敗 → ReasonSchemaViolation」契約に従う。
var ErrSchemaViolation = errors.New("inquiryloop: schema violation")

// ParsedResponse は provider 応答から取り出した inquiryloop 用フィールド群。
type ParsedResponse struct {
	Inquiry    Inquiry
	HasInquiry bool
	Summary    string
	Verdict    ValidationVerdict
	// HasAdvisory は validate-* 系で「修正アドバイスや軽微な指摘あり」を表す optional フィールド。
	// 非 validate 系応答では常に false (parser は raw に存在しなければ既定値のまま返す)。
	HasAdvisory bool
	// Advisory は HasAdvisory=true のときの本文。空文字を許さない (parser で強制)。
	Advisory string
	// Extensions は inquiryloop が意味解釈しない structured output の追加 top-level fields。
	// known fields は含めず、workflow 層が必要な場合だけ engine scope の in-memory 値として読む。
	Extensions map[string]any
}

// ParseResponse は agent 応答 (StructuredOutput / Content) から型付きの問い合わせ情報を取り出す。
//
// 解釈順:
//  1. resp.StructuredOutput が map[string]any なら直接展開
//  2. resp.StructuredOutput が nil なら resp.Content から JSON ブロック抽出を試みる
//  3. それ以外 (型違いや抽出失敗) は ErrSchemaViolation
//
// 戻り値:
//   - inquiry: has_inquiry=true のときのみ有効。false 時は zero value
//   - hasInquiry: ルートの has_inquiry フィールド (boolean)
//   - summary: ルートの summary フィールド (string)
//   - err: スキーマ違反時のみ非 nil (ErrSchemaViolation を wrap)
//
// has_inquiry=true だが inquiry オブジェクトが nil / 欠落の場合は ErrSchemaViolation を返す
// (design.md「Implementation Notes / Risks」契約)。
//
// 同じ json_schema (Schema()) を渡す全ての agent 応答（engine 内部のループ駆動だけでなく、
// workflow 層の fix phase 等でも）に対して共通の解釈ロジックを提供するため public 化している。
func ParseResponse(resp provider.AgentResponse) (Inquiry, bool, string, error) {
	parsed, err := ParseResponseDetail(resp)
	if err != nil {
		return Inquiry{}, false, "", err
	}
	return parsed.Inquiry, parsed.HasInquiry, parsed.Summary, nil
}

// ParseResponseDetail は ParseResponse と同じ解釈に加え、任意の validation verdict を返す。
func ParseResponseDetail(resp provider.AgentResponse) (ParsedResponse, error) {
	raw, ok := readStructuredRoot(resp)
	if !ok {
		return ParsedResponse{}, fmt.Errorf("%w: structuredOutput is not a JSON object and content has no extractable JSON", ErrSchemaViolation)
	}

	hasInquiry, _ := raw["has_inquiry"].(bool)
	summary, _ := raw["summary"].(string)
	verdict, err := parseOptionalVerdict(raw)
	if err != nil {
		return ParsedResponse{}, err
	}
	hasAdvisory, advisory, err := parseOptionalAdvisory(raw)
	if err != nil {
		return ParsedResponse{}, err
	}
	extensions := cloneStructuredExtensions(raw)

	if !hasInquiry {
		return ParsedResponse{
			HasInquiry:  false,
			Summary:     summary,
			Verdict:     verdict,
			HasAdvisory: hasAdvisory,
			Advisory:    advisory,
			Extensions:  extensions,
		}, nil
	}

	// has_inquiry=true のとき inquiry オブジェクトの存在を強制
	inquiryRaw, ok := raw["inquiry"].(map[string]any)
	if !ok || inquiryRaw == nil {
		return ParsedResponse{}, fmt.Errorf("%w: has_inquiry=true but inquiry object missing or not an object", ErrSchemaViolation)
	}

	return ParsedResponse{
		Inquiry:     mapToInquiry(inquiryRaw),
		HasInquiry:  true,
		Summary:     summary,
		Verdict:     verdict,
		HasAdvisory: hasAdvisory,
		Advisory:    advisory,
		Extensions:  extensions,
	}, nil
}

var knownStructuredFields = map[string]struct{}{
	"has_inquiry":  {},
	"summary":      {},
	"inquiry":      {},
	"verdict":      {},
	"has_advisory": {},
	"advisory":     {},
}

func cloneStructuredExtensions(raw map[string]any) map[string]any {
	if len(raw) == 0 {
		return nil
	}
	extensions := make(map[string]any, len(raw))
	for name, value := range raw {
		if _, known := knownStructuredFields[name]; known {
			continue
		}
		extensions[name] = value
	}
	if len(extensions) == 0 {
		return nil
	}
	return extensions
}

// parseOptionalAdvisory は raw の has_advisory / advisory を抽出する。
//
// 契約:
//   - has_advisory フィールドが無い or false → (false, "", nil)
//   - has_advisory=true かつ advisory が non-empty string → (true, text, nil)
//   - has_advisory=true だが advisory が string 型でない / 欠落 / 空文字 → ErrSchemaViolation
//
// 「アドバイスありなら本文必須」を contract 化することで、後段の retry ループに
// 空文字が混入して architect への入力が無意味になる事故を防ぐ。
func parseOptionalAdvisory(raw map[string]any) (bool, string, error) {
	v, ok := raw["has_advisory"]
	if !ok || v == nil {
		return false, "", nil
	}
	hasAdvisory, ok := v.(bool)
	if !ok {
		return false, "", fmt.Errorf("%w: has_advisory is not a boolean", ErrSchemaViolation)
	}
	if !hasAdvisory {
		return false, "", nil
	}
	textRaw, ok := raw["advisory"]
	if !ok || textRaw == nil {
		return false, "", fmt.Errorf("%w: has_advisory=true but advisory field missing", ErrSchemaViolation)
	}
	text, ok := textRaw.(string)
	if !ok {
		return false, "", fmt.Errorf("%w: advisory is not a string", ErrSchemaViolation)
	}
	if text == "" {
		return false, "", fmt.Errorf("%w: has_advisory=true but advisory is empty string", ErrSchemaViolation)
	}
	return true, text, nil
}

func parseOptionalVerdict(raw map[string]any) (ValidationVerdict, error) {
	v, ok := raw["verdict"]
	if !ok || v == nil {
		return "", nil
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("%w: verdict is not a string", ErrSchemaViolation)
	}
	switch ValidationVerdict(s) {
	case VerdictGo, VerdictNoGo:
		return ValidationVerdict(s), nil
	default:
		return "", fmt.Errorf("%w: verdict must be %q or %q, got %q", ErrSchemaViolation, VerdictGo, VerdictNoGo, s)
	}
}

// readStructuredRoot は resp.StructuredOutput が map[string]any ならそれを返し、
// nil なら resp.Content から JSON ブロックを抽出して map に decode する。
//
// 戻り値の bool は「ルート object が取得できたか」のフラグ。false の場合は呼び出し側で
// ErrSchemaViolation を返す。
func readStructuredRoot(resp provider.AgentResponse) (map[string]any, bool) {
	if record, ok := resp.StructuredOutput.(map[string]any); ok && record != nil {
		return record, true
	}
	if resp.StructuredOutput != nil {
		// 何らかの値はあるが map[string]any ではないケース (型違い) は schema 違反扱い
		return nil, false
	}
	// Content からの JSON 抽出フォールバック
	return extractJSONFromContent(resp.Content)
}

// extractJSONFromContent は LLM の自由記述レスポンス内に埋め込まれた JSON object を抽出する。
//
// 試行順:
//  1. trim した Content 全体を json.Unmarshal で直接 decode
//  2. ```json ... ``` フェンス内の JSON ブロックを探す
//  3. 最初の '{' から最後の '}' までを切り出して decode
//
// いずれも失敗したら (nil, false)。
func extractJSONFromContent(content string) (map[string]any, bool) {
	content = strings.TrimSpace(content)
	if content == "" {
		return nil, false
	}

	// 1. 直接 decode
	var m map[string]any
	if err := json.Unmarshal([]byte(content), &m); err == nil {
		return m, true
	}

	// 2. ```json ... ``` コードフェンス
	if fenced, ok := extractFencedJSON(content); ok {
		var fm map[string]any
		if err := json.Unmarshal([]byte(fenced), &fm); err == nil {
			return fm, true
		}
	}

	// 3. 最初の '{' と最後の '}' で切り出す
	start := strings.Index(content, "{")
	end := strings.LastIndex(content, "}")
	if start < 0 || end < 0 || start >= end {
		return nil, false
	}
	var bm map[string]any
	if err := json.Unmarshal([]byte(content[start:end+1]), &bm); err == nil {
		return bm, true
	}
	return nil, false
}

// extractFencedJSON は ```json\n ... \n``` ブロックの中身を返す。
// 見つからない場合は ("", false)。
func extractFencedJSON(content string) (string, bool) {
	const fenceJSON = "```json"
	idx := strings.Index(content, fenceJSON)
	if idx < 0 {
		return "", false
	}
	rest := content[idx+len(fenceJSON):]
	// 改行までスキップ
	if nl := strings.Index(rest, "\n"); nl >= 0 {
		rest = rest[nl+1:]
	}
	endIdx := strings.Index(rest, "```")
	if endIdx < 0 {
		return "", false
	}
	return strings.TrimSpace(rest[:endIdx]), true
}

// mapToInquiry は parseResponse 内部で inquiry サブオブジェクトを型付き Inquiry に変換する。
// 欠落フィールドは zero value のままにし、has_inquiry=true 文脈での「inquiry object 自体の存在」だけを
// 上位 (parseResponse) が保証する責務分担。
func mapToInquiry(m map[string]any) Inquiry {
	inq := Inquiry{}
	if v, ok := m["title"].(string); ok {
		inq.Title = v
	}
	if v, ok := m["background"].(string); ok {
		inq.Background = v
	}
	if v, ok := m["question"].(string); ok {
		inq.Question = v
	}
	if rawOpts, ok := m["options"].([]any); ok {
		opts := make([]InquiryOption, 0, len(rawOpts))
		for _, o := range rawOpts {
			om, ok := o.(map[string]any)
			if !ok {
				continue
			}
			opt := InquiryOption{}
			if l, ok := om["label"].(string); ok {
				opt.Label = l
			}
			if d, ok := om["description"].(string); ok {
				opt.Description = d
			}
			opts = append(opts, opt)
		}
		if len(opts) > 0 {
			inq.Options = opts
		}
	}
	return inq
}

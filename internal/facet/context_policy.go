package facet

import "fmt"

// PromptContextKind は prompt に埋め込む context の損失許容度を表す。
//
// Summary / Excerpt は要約または明示的な抜粋として bounded に扱える。一方で、
// Contract / Instruction / Diagnostic / Policy は欠落すると実行契約や判断根拠が
// 壊れやすいため、既定では切り詰めない。
type PromptContextKind int

const (
	PromptContextSummary PromptContextKind = iota
	PromptContextExcerpt
	PromptContextContract
	PromptContextInstruction
	PromptContextDiagnostic
	PromptContextPolicy
)

func (k PromptContextKind) String() string {
	switch k {
	case PromptContextSummary:
		return "summary"
	case PromptContextExcerpt:
		return "excerpt"
	case PromptContextContract:
		return "contract"
	case PromptContextInstruction:
		return "instruction"
	case PromptContextDiagnostic:
		return "diagnostic"
	case PromptContextPolicy:
		return "policy"
	default:
		return fmt.Sprintf("prompt-context:unknown(%d)", int(k))
	}
}

func (k PromptContextKind) trimMaxChars() (int, bool) {
	switch k {
	case PromptContextSummary, PromptContextExcerpt:
		return CONTEXT_MAX_CHARS, true
	case PromptContextContract, PromptContextInstruction, PromptContextDiagnostic, PromptContextPolicy:
		return 0, false
	default:
		// Unknown kinds preserve content. Losing context is harder to debug than
		// paying extra tokens for an unsupported kind.
		return 0, false
	}
}

// TrimPromptContext applies the truncation policy for kind.
//
// Bounded kinds delegate to TrimContext and therefore preserve rune boundaries
// and sourcePath markers. Lossless kinds return content unchanged.
func TrimPromptContext(kind PromptContextKind, content string, sourcePath string) string {
	if maxChars, ok := kind.trimMaxChars(); ok {
		return TrimContext(content, maxChars, sourcePath)
	}
	return content
}

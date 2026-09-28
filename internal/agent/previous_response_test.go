package agent

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/kazuki-oshino/rilll/internal/facet"
)

// TestPrependPreviousResponseShortContent は短文 content を渡したときに
// "# Previous Response" ブロック + 水平線区切り + 元 prompt の combined prompt が
// 文字単位で一致することを golden string で検証する (Req 2.1, 2.2, 2.3, 3.3)。
func TestPrependPreviousResponseShortContent(t *testing.T) {
	prompt := "Hello, world."
	content := "previous summary"

	want := "# Previous Response\n\nprevious summary\n\n---\n\nHello, world."
	got := prependPreviousResponse(prompt, content)

	if got != want {
		t.Fatalf("combined prompt mismatch\n want: %q\n  got: %q", want, got)
	}
}

// TestPrependPreviousResponseLongTruncation は CONTEXT_MAX_CHARS を超える content を
// 渡したときに先頭 CONTEXT_MAX_CHARS rune + 切り詰め通知マーカで構成される
// combined prompt が生成されることを検証する (Req 3.1, 3.2)。
func TestPrependPreviousResponseLongTruncation(t *testing.T) {
	originalRuneCount := 3000
	originalContent := strings.Repeat("a", originalRuneCount)
	prompt := "next-prompt"

	wantEmbedded := strings.Repeat("a", facet.CONTEXT_MAX_CHARS) + "...(truncated)"
	want := "# Previous Response\n\n" + wantEmbedded + "\n\n---\n\n" + prompt
	got := prependPreviousResponse(prompt, originalContent)

	if got != want {
		t.Fatalf("truncated combined prompt mismatch\n want length=%d\n  got length=%d", len(want), len(got))
	}
}

// TestPrependPreviousResponseDeterministic は同一 (prompt, content) で連続呼び出しした
// 戻り値が文字単位で一致すること (純関数性 / Req 2.5) を検証する。
func TestPrependPreviousResponseDeterministic(t *testing.T) {
	prompt := "deterministic-prompt"
	content := "deterministic-content"

	first := prependPreviousResponse(prompt, content)
	second := prependPreviousResponse(prompt, content)

	if first != second {
		t.Fatalf("non-deterministic output\n first:  %q\n second: %q", first, second)
	}
}

// TestPrependPreviousResponseMultibyteRuneBoundary はマルチバイト rune (日本語) を含む
// content の切り詰めが UTF-8 マルチバイト中間バイトではなく rune 境界で行われ、
// summary context の切り詰め通知マーカが付くことを検証する (Req 3.1)。
func TestPrependPreviousResponseMultibyteRuneBoundary(t *testing.T) {
	multibyteChar := "あ" // UTF-8 で 3 バイトの rune
	originalRuneCount := 2500
	originalContent := strings.Repeat(multibyteChar, originalRuneCount)
	prompt := "p"

	wantEmbedded := strings.Repeat(multibyteChar, facet.CONTEXT_MAX_CHARS) + "...(truncated)"
	want := "# Previous Response\n\n" + wantEmbedded + "\n\n---\n\n" + prompt
	got := prependPreviousResponse(prompt, originalContent)

	if got != want {
		t.Fatalf("multibyte rune boundary mismatch (rune 境界で切れていない可能性)")
	}

	if !utf8.ValidString(got) {
		t.Fatalf("combined prompt is not valid UTF-8 — マルチバイト中間バイトで切断された可能性")
	}
}

// TestPrependPreviousResponsePassthrough は content 内に "# Previous Response" 見出しや
// "---" 水平線が含まれていても escape / sanitize されず素通しされることを
// 検証する (Req 2.4)。
func TestPrependPreviousResponsePassthrough(t *testing.T) {
	prompt := "outer-prompt"
	content := "# Previous Response\n\nnested-payload\n\n---\n\ninner-text"

	want := "# Previous Response\n\n" + content + "\n\n---\n\n" + prompt
	got := prependPreviousResponse(prompt, content)

	if got != want {
		t.Fatalf("escape / sanitize が発生した\n want: %q\n  got: %q", want, got)
	}
}

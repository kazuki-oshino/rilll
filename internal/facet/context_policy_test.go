package facet

import (
	"strings"
	"testing"
)

func TestTrimPromptContextByKind(t *testing.T) {
	longContent := strings.Repeat("x", CONTEXT_MAX_CHARS+10)
	sourcePath := "run/result.json"

	tests := []struct {
		name string
		kind PromptContextKind
		want string
	}{
		{
			name: "summary is bounded",
			kind: PromptContextSummary,
			want: strings.Repeat("x", CONTEXT_MAX_CHARS) +
				"...(truncated, full at " + sourcePath + ")",
		},
		{
			name: "excerpt is explicitly bounded",
			kind: PromptContextExcerpt,
			want: strings.Repeat("x", CONTEXT_MAX_CHARS) +
				"...(truncated, full at " + sourcePath + ")",
		},
		{
			name: "contract is lossless",
			kind: PromptContextContract,
			want: longContent,
		},
		{
			name: "instruction is lossless",
			kind: PromptContextInstruction,
			want: longContent,
		},
		{
			name: "diagnostic is lossless",
			kind: PromptContextDiagnostic,
			want: longContent,
		},
		{
			name: "policy is lossless",
			kind: PromptContextPolicy,
			want: longContent,
		},
		{
			name: "unknown kind is lossless",
			kind: PromptContextKind(999),
			want: longContent,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := TrimPromptContext(tt.kind, longContent, sourcePath)
			if got != tt.want {
				t.Fatalf("TrimPromptContext(%s) mismatch\nwant: %q\n got: %q",
					tt.kind, tt.want, got)
			}
		})
	}
}

func TestPromptContextKindString(t *testing.T) {
	tests := []struct {
		kind PromptContextKind
		want string
	}{
		{kind: PromptContextSummary, want: "summary"},
		{kind: PromptContextExcerpt, want: "excerpt"},
		{kind: PromptContextContract, want: "contract"},
		{kind: PromptContextInstruction, want: "instruction"},
		{kind: PromptContextDiagnostic, want: "diagnostic"},
		{kind: PromptContextPolicy, want: "policy"},
		{kind: PromptContextKind(999), want: "prompt-context:unknown(999)"},
	}

	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			if got := tt.kind.String(); got != tt.want {
				t.Fatalf("PromptContextKind.String() = %q, want %q", got, tt.want)
			}
		})
	}
}

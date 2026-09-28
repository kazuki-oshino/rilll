package akashic

import (
	"errors"
	"io/fs"
	"testing"
)

// TestValidateArtifactKindAccepts は closed set の 4 値すべてが nil を返すことを検証する。
func TestValidateArtifactKindAccepts(t *testing.T) {
	t.Parallel()

	kinds := []string{
		ArtifactKindProposal,
		ArtifactKindReview,
		ArtifactKindResult,
		ArtifactKindContext,
	}
	for _, kind := range kinds {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			if err := ValidateArtifactKind(kind); err != nil {
				t.Fatalf("ValidateArtifactKind(%q) = %v, want nil", kind, err)
			}
		})
	}
}

// TestValidateArtifactKindRejectsUnknown は未知 kind が ErrUnknownArtifactKind で
// errors.Is 判定可能なエラーを返すことを検証する (Req 1.7)。
func TestValidateArtifactKindRejectsUnknown(t *testing.T) {
	t.Parallel()

	cases := []string{
		"",
		"unknown",
		"Proposal",  // 大文字始まりは closed set 外
		"proposals", // 末尾差異
		"context ",  // 末尾空白
		" context",  // 先頭空白
		"summary",   // 完全に未定義
	}
	for _, kind := range cases {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			err := ValidateArtifactKind(kind)
			if err == nil {
				t.Fatalf("ValidateArtifactKind(%q) = nil, want non-nil error", kind)
			}
			if !errors.Is(err, ErrUnknownArtifactKind) {
				t.Fatalf("errors.Is(err, ErrUnknownArtifactKind) = false for kind %q (err=%v)", kind, err)
			}
		})
	}
}

// TestErrArtifactFileMissingWrapsFsErrNotExist は読取側 sentinel が
// fs.ErrNotExist と互換であることを検証する (design.md Failure mode policy)。
func TestErrArtifactFileMissingWrapsFsErrNotExist(t *testing.T) {
	t.Parallel()

	if !errors.Is(ErrArtifactFileMissing, fs.ErrNotExist) {
		t.Fatalf("errors.Is(ErrArtifactFileMissing, fs.ErrNotExist) = false; want true")
	}
}

// TestArtifactSentinelsAreDistinct は 5 種の sentinel が相互に区別可能であることを
// 確認する (errors.Is で誤判定が起きないこと)。
func TestArtifactSentinelsAreDistinct(t *testing.T) {
	t.Parallel()

	sentinels := []error{
		ErrArtifactNotAllowed,
		ErrArtifactFileMissing,
		ErrArtifactTooLarge,
		ErrUnknownArtifactKind,
		ErrArtifactOutsideRillRoot,
	}
	for i, a := range sentinels {
		for j, b := range sentinels {
			if i == j {
				continue
			}
			if errors.Is(a, b) {
				t.Fatalf("errors.Is(%v, %v) = true; want distinct sentinels", a, b)
			}
		}
	}
}

// TestDefaultArtifactReadMaxBytes は既定上限が 10 MiB であることを確認する。
// (Req 3.4: ReadArtifact デフォルト上限)
func TestDefaultArtifactReadMaxBytes(t *testing.T) {
	t.Parallel()

	const wantMiB10 int64 = 10 * 1024 * 1024
	if DefaultArtifactReadMaxBytes != wantMiB10 {
		t.Fatalf("DefaultArtifactReadMaxBytes = %d, want %d", DefaultArtifactReadMaxBytes, wantMiB10)
	}
}

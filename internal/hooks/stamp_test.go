package hooks

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"testing"
)

// TestStampSaveLoadRoundTrip は saveStamp → loadStamp の round-trip で同一の
// embedStamp が読み出せることを検証する (R3.5: 追従処理完了後に version を記録 /
// 書き戻し → 読み戻しの一貫性)。
//
// 併せて、書き込まれた .embed-version の POSIX file mode が 0644 であることを
// 確認する (task 2.2 完了条件 / design.md "saveStamp の mode 0644")。
func TestStampSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()

	want := embedStamp{
		Version: "1.2.3",
		Files: map[string]string{
			"placeholder": "abc1234567890def",
			"verify-gate": "0123456789abcdef",
		},
	}

	if err := saveStamp(dir, want); err != nil {
		t.Fatalf("saveStamp() error = %v, want nil", err)
	}

	// .embed-version の mode が 0644 であること (task 2.2 完了条件)
	info, err := os.Stat(filepath.Join(dir, ".embed-version"))
	if err != nil {
		t.Fatalf(".embed-version の Stat に失敗: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o644 {
		t.Errorf(".embed-version mode = %o, want 0644", got)
	}

	got, err := loadStamp(dir)
	if err != nil {
		t.Fatalf("loadStamp() error = %v, want nil", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round-trip 不一致:\n got = %#v\nwant = %#v", got, want)
	}
}

// TestStampLoadAbsentReturnsZero は R3.1 を検証する。
// .embed-version が存在しない初回起動状態で loadStamp が呼ばれたとき、
// ゼロ値 (Version: "", Files: nil) をエラーなしで返す。
//
// このゼロ値で「stamp 不在 = 初回起動」を表現するため、不在を error として
// 返してしまうと caller (ResolveHookPath) は ErrHookNotFound と区別が
// 必要になり契約が複雑化する。R6.2 (I/O エラーは ErrHookNotFound と区別)
// は parse error 等の "本物の" I/O 失敗にのみ適用する。
func TestStampLoadAbsentReturnsZero(t *testing.T) {
	dir := t.TempDir()

	got, err := loadStamp(dir)
	if err != nil {
		t.Fatalf("loadStamp() (不在時) error = %v, want nil", err)
	}

	if got.Version != "" {
		t.Errorf("loadStamp() (不在時) Version = %q, want \"\"", got.Version)
	}
	if got.Files != nil {
		t.Errorf("loadStamp() (不在時) Files = %#v, want nil", got.Files)
	}
}

// TestStampLoadInvalidJSONWraps は R6.2 を検証する。
// .embed-version の JSON が壊れている場合、loadStamp は
// fmt.Errorf("hooks: stamp ...: %w", err) 形式で wrap して返し、
// errors.Unwrap で原因を取り出せること、ErrHookNotFound と
// ErrInvalidName の sentinel とは区別されることを確認する。
func TestStampLoadInvalidJSONWraps(t *testing.T) {
	dir := t.TempDir()

	// 壊れた JSON を直接配置する
	stampPath := filepath.Join(dir, ".embed-version")
	if err := os.WriteFile(stampPath, []byte("{not valid json"), 0o644); err != nil {
		t.Fatalf("壊れた .embed-version の事前配置に失敗: %v", err)
	}

	_, err := loadStamp(dir)
	if err == nil {
		t.Fatal("loadStamp() (parse error) error = nil, want non-nil")
	}

	// メッセージプレフィックス: "hooks: stamp" を含むこと (R6.2 の wrap 形式)
	matched, _ := regexp.MatchString(`^hooks: stamp `, err.Error())
	if !matched {
		t.Errorf("loadStamp() error = %q, want prefix %q", err.Error(), "hooks: stamp ")
	}

	// errors.Unwrap で原因の error が取得できること
	if cause := errors.Unwrap(err); cause == nil {
		t.Error("errors.Unwrap(err) = nil, want non-nil (原因 error が wrap されていない)")
	}

	// ErrHookNotFound / ErrInvalidName と互いに排他であること (R6.2 区別可能性)
	if errors.Is(err, ErrHookNotFound) {
		t.Error("parse error が ErrHookNotFound として識別されている (排他性違反)")
	}
	if errors.Is(err, ErrInvalidName) {
		t.Error("parse error が ErrInvalidName として識別されている (排他性違反)")
	}
}

// TestStampSHA256Bytes は R3.6 を検証する (内容ベースの改変判定アルゴリズム)。
// sha256Bytes は与えられたバイト列の crypto/sha256 hex (lower case, 64 文字) を返す。
func TestStampSHA256Bytes(t *testing.T) {
	cases := []struct {
		name  string
		input []byte
	}{
		{"空のバイト列", []byte{}},
		{"単純文字列", []byte("hello")},
		{"placeholder.sh 風", []byte("#!/usr/bin/env bash\nexit 0\n")},
	}

	hexLower := regexp.MustCompile(`^[0-9a-f]{64}$`)

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sha256Bytes(tc.input)

			// crypto/sha256 + encoding/hex と同一値であること
			sum := sha256.Sum256(tc.input)
			want := hex.EncodeToString(sum[:])
			if got != want {
				t.Errorf("sha256Bytes(%q) = %q, want %q", tc.input, got, want)
			}

			// hex (lower case, 64 文字) であること
			if !hexLower.MatchString(got) {
				t.Errorf("sha256Bytes(%q) = %q, want lowercase 64 hex chars", tc.input, got)
			}
		})
	}
}

// TestStampSHA256File は R3.6 を検証する。
// sha256File は path のファイル内容の SHA-256 hex を返し、I/O エラー時には
// fmt.Errorf("hooks: ...: %w", err) で wrap して返す。
func TestStampSHA256File(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sample.sh")

	content := []byte("#!/usr/bin/env bash\necho hi\n")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("テストファイル作成に失敗: %v", err)
	}

	got, err := sha256File(path)
	if err != nil {
		t.Fatalf("sha256File() error = %v, want nil", err)
	}

	// 値の正当性: sha256Bytes(content) と一致すること
	want := sha256Bytes(content)
	if got != want {
		t.Errorf("sha256File() = %q, want %q (sha256Bytes(content) と不一致)", got, want)
	}
}

// TestStampSHA256FileMissingWraps は R6.2 を検証する。
// 存在しないファイルを渡したとき、sha256File は wrap した error を返す。
func TestStampSHA256FileMissingWraps(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "does-not-exist.sh")

	_, err := sha256File(path)
	if err == nil {
		t.Fatal("sha256File() (不在ファイル) error = nil, want non-nil")
	}

	// メッセージプレフィックス: "hooks:" を含むこと (R6.2 wrap 形式)
	matched, _ := regexp.MatchString(`^hooks: `, err.Error())
	if !matched {
		t.Errorf("sha256File() error = %q, want prefix %q", err.Error(), "hooks: ")
	}

	// 原因の error が wrap されていて os の not exist と判別可能であること
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("errors.Is(err, fs.ErrNotExist) = false, want true (cause が伝播していない)")
	}
}

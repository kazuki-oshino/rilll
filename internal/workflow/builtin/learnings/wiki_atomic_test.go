// Package learnings の本 file は `writeFileAtomic` helper の契約を検証する
// 単体テスト群を保持する (tasks.md 1.1 / design.md §WikiAtomicWriter / Req 9.1, 9.5)。
//
// 検証観点:
//
//   - (a) `path` に `body` が書き込まれ、tmp file が rename 後に残らない
//     (POSIX atomic rename / design.md §WikiAtomicWriter Postconditions)。
//   - (b) `perm` 引数が file mode に反映される (0o644 / 0o600 等)。`os.CreateTemp`
//     ではなく `os.WriteFile(tmp, body, perm)` で perm を尊重する不変式の regression
//     ガード (playbook/tool-pattern/os-createtemp-mode.md 由来)。
//   - (c) 既存 file への上書きで body / mode が更新される。
//   - (d) body は生バイトをそのまま書き込む (末尾改行や JSON encode は helper の
//     責務外)。
//
// テスト戦略:
//
//   - `t.TempDir()` 配下に target path を作って実 os 経由で書き込み、`os.Stat`
//   - `os.ReadFile` で結果を assert する。
//   - `*.tmp` の残存検査は `os.Stat(path+".tmp")` で `os.IsNotExist` を要求する
//     (atomic rename 成功時の不変式)。
package learnings

import (
	"os"
	"path/filepath"
	"testing"
)

// TestWriteFileAtomicWritesBodyAndCleansTmp は通常書き込み経路を検証する。
// path に body が書かれ、tmp file が rename 後に残らないことを確認する。
func TestWriteFileAtomicWritesBodyAndCleansTmp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.txt")
	body := []byte("hello atomic")

	if err := writeFileAtomic(path, body, 0o644); err != nil {
		t.Fatalf("writeFileAtomic: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != string(body) {
		t.Errorf("body = %q, want %q", got, body)
	}

	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("tmp file should not remain after rename: err = %v", err)
	}
}

// TestWriteFileAtomicRespectsPerm は perm 引数が最終 file の mode に反映される
// ことを 0o644 / 0o600 の 2 mode で検証する。
//
// 設計上の regression guard: 過去に `os.CreateTemp` を使ってしまうと OS 既定
// (0600) で tmp が作成され Chmod を挟まないと perm が落ちる経路があった
// (playbook/tool-pattern/os-createtemp-mode.md)。本テストは `os.WriteFile(tmp,
// body, perm)` 経路で perm が直接尊重されることを担保する。
func TestWriteFileAtomicRespectsPerm(t *testing.T) {
	cases := []struct {
		name string
		perm os.FileMode
	}{
		{name: "0644 (user-readable)", perm: 0o644},
		{name: "0600 (user-only)", perm: 0o600},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "out.txt")
			if err := writeFileAtomic(path, []byte("x"), tc.perm); err != nil {
				t.Fatalf("writeFileAtomic: %v", err)
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatalf("Stat: %v", err)
			}
			if got := info.Mode().Perm(); got != tc.perm {
				t.Errorf("mode.Perm() = %v, want %v", got, tc.perm)
			}
		})
	}
}

// TestWriteFileAtomicOverwritesExistingFile は既存 file への上書きで body /
// mode が更新されることを検証する。
func TestWriteFileAtomicOverwritesExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.txt")

	// 既存 file を予め作る (異なる perm / body)。
	if err := os.WriteFile(path, []byte("old body"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := writeFileAtomic(path, []byte("new body"), 0o644); err != nil {
		t.Fatalf("writeFileAtomic: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "new body" {
		t.Errorf("body = %q, want %q", got, "new body")
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o644 {
		t.Errorf("mode.Perm() = %v, want 0o644 (overwrite should refresh perm)", perm)
	}
}

// TestWriteFileAtomicWritesRawBytes は body が生バイトのままで書き込まれる
// (helper 内部で改行付与や JSON encode をしない) ことを確認する。
//
// 改行付与等は 呼び出し側の責務である (writeJSONAtomic wrapper が末尾改行を
// 付ける現行契約と同じ責務分離)。
func TestWriteFileAtomicWritesRawBytes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.bin")
	body := []byte{0x00, 0x01, 0x02, 'a', 'b'}

	if err := writeFileAtomic(path, body, 0o644); err != nil {
		t.Fatalf("writeFileAtomic: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(got) != len(body) {
		t.Fatalf("len = %d, want %d (no trailing newline / encoding)", len(got), len(body))
	}
	for i := range body {
		if got[i] != body[i] {
			t.Errorf("byte[%d] = %v, want %v", i, got[i], body[i])
		}
	}
}

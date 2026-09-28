// Package learnings の本 file は agent-learnings の全 file 書き込み (wiki
// section append / index rebuild / sessions.json / reflector artifact /
// apply-result.json 等) の共通基盤 helper `writeFileAtomic` を提供する
// (tasks.md 1.1 / design.md §WikiAtomicWriter / Req 9.1, 9.5)。
//
// 役割:
//   - body を tmp file 経由で path に POSIX atomic write する。
//   - 同一 directory 内 `os.Rename` は POSIX 上 atomic で、書き込み途中の partial
//     file が後段 phase / 並行プロセスに観測されない不変式を保証する。
//   - perm 引数を尊重して最終 file の mode を設定する (`os.WriteFile(tmp, body,
//     perm)` に直接渡す経路を取り、`os.CreateTemp` の OS 既定 0600 + Chmod の
//     回り道経路は採らない)。
//
// 設計理由:
//
//   - 既存 `writeJSONAtomic` (prefetch.go) と同形の tmp + rename パターンを
//     bytes-level に一般化したもの。perm を引数化することで、後続 wiki helper が
//     0o644 (markdown / json) と 0o600 (機微 artifact) を選択できる seam を残す。
//   - 失敗時は tmp ファイルが残り得るが、次回 run の成功 rename で同じ tmp 名が
//     上書きされるため累積しない。中間状態の `*.tmp` は debug にも有用。
package learnings

import (
	"fmt"
	"os"
)

// writeFileAtomic は body を path へ tmp + rename で atomic に書き出す。
// perm は最終 file の mode (rename 後の path に反映される)。
//
// Preconditions: path の親 directory が存在すること。
// Postconditions: 成功時 path には body が書き込まれ、tmp (`path + ".tmp"`) は
// 削除済み。
// Invariants: 中間状態の partial write は外部から観測不能 (rename が atomic)。
//
// 失敗経路:
//   - tmp file への write 失敗: `write tmp %q: %w` でラップして返す (tmp は OS が
//     残骸を保持する可能性があるが、次回 rename で上書きされる)。
//   - rename 失敗: `rename %q -> %q: %w` でラップして返す。
func writeFileAtomic(path string, body []byte, perm os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, body, perm); err != nil {
		return fmt.Errorf("write tmp %q: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename %q -> %q: %w", tmp, path, err)
	}
	return nil
}

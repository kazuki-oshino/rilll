package hooks

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// stampFileName は <hooksDir>/.embed-version の basename。
// design.md "Logical Data Model" で定義された implementation detail。
const stampFileName = ".embed-version"

// embedStamp は .embed-version の JSON スキーマを表す不変値。
//
// Version は最後に追従処理 (展開 / 上書き / 保護記録) を完了した rilll BuildVersion。
// R3.5 を満たすための informational record であり、drift 検知には使用しない
// (drift は per-file content-addressable 比較で判定される)。
//
// Files は name (拡張子なし) → SHA-256 hex の map で、「直近に書き込みを行った
// 時点の embed もしくはディスク内容」のフィンガープリントを per-file 単位で保持する。
// drift 検知は Files[name] と「現 embed FS から計算した currentEmbedSHA」の
// per-file 比較で行う (R3.3, R3.4, R3.6)。
type embedStamp struct {
	Version string            `json:"version"`
	Files   map[string]string `json:"files"`
}

// loadStamp は <dir>/.embed-version を読み込んで embedStamp を返す。
//
//   - ファイルが存在しない場合: ゼロ値 (Version: "", Files: nil) と nil error を返す。
//     これは R3.1 の「初回起動 = stamp 不在」を ゼロ値で表現するための規約であり、
//     呼び出し側 (ResolveHookPath) は ErrHookNotFound と区別する必要がない。
//   - JSON parse 失敗 / その他の I/O 失敗の場合: fmt.Errorf("hooks: stamp ...: %w", err)
//     形式で wrap して返す (R6.2)。errors.Is で ErrHookNotFound / ErrInvalidName と
//     区別される。
func loadStamp(dir string) (embedStamp, error) {
	path := filepath.Join(dir, stampFileName)
	raw, err := os.ReadFile(path)
	if err != nil {
		// 不在は R3.1 の「初回起動」状態。ゼロ値 + nil で表現する。
		if errors.Is(err, fs.ErrNotExist) {
			return embedStamp{}, nil
		}
		return embedStamp{}, fmt.Errorf("hooks: stamp read %s: %w", path, err)
	}

	var s embedStamp
	if err := json.Unmarshal(raw, &s); err != nil {
		return embedStamp{}, fmt.Errorf("hooks: stamp parse %s: %w", path, err)
	}
	return s, nil
}

// saveStamp は <dir>/.embed-version に embedStamp を atomic に書き込む。
//
// 実装は tmp + rename による atomic write で、partial-write を発生させない。
// mode は 0644。dir が存在しない場合は os.MkdirAll(..., 0o755) で遅延作成する
// (Tier 3 の初回展開で hooksDir が未作成の状態を想定しての防御)。
//
// 並行呼び出し時は last-writer-wins で許容する (Flow 2 / R6.4)。tmp ファイル名は
// os.CreateTemp により unique 化されるため、複数の saveStamp が同時実行しても
// tmp 衝突は発生しない。
//
// I/O エラーは fmt.Errorf("hooks: stamp ...: %w", err) で wrap して返す (R6.2)。
func saveStamp(dir string, s embedStamp) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("hooks: stamp mkdir %s: %w", dir, err)
	}

	target := filepath.Join(dir, stampFileName)

	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		// embedStamp は単純な map[string]string + string で構成されるため
		// 通常 marshal は失敗しないが、防御的に wrap して伝播させる。
		return fmt.Errorf("hooks: stamp marshal: %w", err)
	}

	tmp, err := os.CreateTemp(dir, ".embed-version-*.tmp")
	if err != nil {
		return fmt.Errorf("hooks: stamp tmp create %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	// tmp が rename 前に残らないよう best-effort で除去する (rename 成功時は no-op)。
	defer func() {
		_ = os.Remove(tmpPath)
	}()

	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("hooks: stamp tmp write %s: %w", tmpPath, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("hooks: stamp tmp close %s: %w", tmpPath, err)
	}

	// os.CreateTemp は mode 0600 で作成するため、契約上の 0644 に明示的に修正する。
	if err := os.Chmod(tmpPath, 0o644); err != nil {
		return fmt.Errorf("hooks: stamp chmod %s: %w", tmpPath, err)
	}

	if err := os.Rename(tmpPath, target); err != nil {
		return fmt.Errorf("hooks: stamp rename %s -> %s: %w", tmpPath, target, err)
	}
	return nil
}

// sha256Bytes は b の SHA-256 hex (lower case, 64 文字) を返す。
//
// 改変判定の参照値 (`Files[name]`) と「現 embed FS の内容」を per-file で
// 比較するための共通ユーティリティ (R3.6)。
func sha256Bytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// sha256File は path のファイル内容の SHA-256 hex を返す。
// I/O エラーは fmt.Errorf("hooks: sha256 ...: %w", err) で wrap して返す (R6.2)。
//
// fs.ErrNotExist 等の cause sentinel は wrap されたまま伝播するため、
// 呼び出し側は errors.Is(err, fs.ErrNotExist) で原因を判定できる。
func sha256File(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("hooks: sha256 read %s: %w", path, err)
	}
	return sha256Bytes(raw), nil
}

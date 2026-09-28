package hooks

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/kazuki-oshino/rilll/internal/version"
)

// TestPackage_SentinelErrorsAreNonNilAndDistinct は task 1.2 の smoke test。
// ErrHookNotFound / ErrInvalidName が sentinel として宣言され、互いに区別可能で
// あることをダウンストリーム呼び出し側の視点 (errors.Is) で検証する。
func TestPackage_SentinelErrorsAreNonNilAndDistinct(t *testing.T) {
	if ErrHookNotFound == nil {
		t.Fatal("ErrHookNotFound は nil であってはならない")
	}
	if ErrInvalidName == nil {
		t.Fatal("ErrInvalidName は nil であってはならない")
	}

	// 同一性: 自分自身に対して errors.Is が真であること (sentinel として識別可能)
	if !errors.Is(ErrHookNotFound, ErrHookNotFound) {
		t.Error("errors.Is(ErrHookNotFound, ErrHookNotFound) が真にならない")
	}
	if !errors.Is(ErrInvalidName, ErrInvalidName) {
		t.Error("errors.Is(ErrInvalidName, ErrInvalidName) が真にならない")
	}

	// 排他性: 互いに区別可能であること (R6.1 sentinel 区別可能性 / Security)
	if errors.Is(ErrHookNotFound, ErrInvalidName) {
		t.Error("ErrHookNotFound と ErrInvalidName が同一視されている")
	}
	if errors.Is(ErrInvalidName, ErrHookNotFound) {
		t.Error("ErrInvalidName と ErrHookNotFound が同一視されている")
	}

	// メッセージ contract: doc / 設計通りのプレフィックスを持つこと
	if got := ErrHookNotFound.Error(); got != "hooks: not found" {
		t.Errorf("ErrHookNotFound.Error() = %q, want %q", got, "hooks: not found")
	}
	if got := ErrInvalidName.Error(); got != "hooks: invalid name" {
		t.Errorf("ErrInvalidName.Error() = %q, want %q", got, "hooks: invalid name")
	}
}

// TestPackage_EmbedFSContainsPlaceholder は task 1.2 の smoke test。
// //go:embed hooks/*.sh で placeholder.sh が embed セットに取り込まれていることを
// 確認する。これが満たされない限り、task 3.3 以降の embed → user-global 展開経路は
// 配布不能となる (R5.3 の前段保証)。
func TestPackage_EmbedFSContainsPlaceholder(t *testing.T) {
	// 1.1 で配置した placeholder.sh が embed.FS から読み出せること
	content, err := fs.ReadFile(embedFS, "hooks/placeholder.sh")
	if err != nil {
		t.Fatalf("embedFS から hooks/placeholder.sh を読めない: %v", err)
	}
	if len(content) == 0 {
		t.Fatal("hooks/placeholder.sh の内容が空")
	}

	// 1.1 の完了条件: shebang を含むこと
	want := "#!/usr/bin/env bash"
	if string(content[:len(want)]) != want {
		t.Errorf("placeholder.sh が shebang で始まっていない: got prefix %q", string(content[:len(want)]))
	}
}

// TestResolveRejectsInvalidName は task 3.1 (Tier 0 入口検証) のテスト 14。
//
// ResolveHookPath は name が ^[a-zA-Z0-9_-]+$ を満たさない場合、
// I/O 副作用 (ディレクトリ作成 / ファイル書き出し / stamp 更新) を一切起こさず
// ErrInvalidName を即時返却しなければならない (公開契約 / Security / R6.1 区別可能性)。
//
// 検証:
//   - errors.Is(err, ErrInvalidName) が真 (sentinel として識別可能)
//   - 戻り値 path は空文字列 (公開契約: invalid 時にパスを返さない)
//   - hooksDir 配下のディレクトリツリー (file 一覧 + .embed-version の内容) が
//     呼び出し前後で完全に unchanged (snapshot byte-equal)
//   - cwd を空文字列 (Tier 1 スキップ) と非空 (Tier 1 探索対象) の両方で同じ拒否挙動
func TestResolveRejectsInvalidName(t *testing.T) {
	dir := setupHooksSandbox(t)

	// hooksDir に予め file system 状態を作っておき、ResolveHookPath が invalid name で
	// 呼ばれた際に何の I/O 副作用 (mkdir / write / chmod / saveStamp 等) も
	// 起こさないことを byte-equal snapshot で検証する。
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll(dir) error = %v", err)
	}
	existingScript := filepath.Join(dir, "existing.sh")
	existingContent := []byte("#!/usr/bin/env bash\necho existing\n")
	if err := os.WriteFile(existingScript, existingContent, 0o755); err != nil {
		t.Fatalf("WriteFile(existing.sh) error = %v", err)
	}
	stampPath := filepath.Join(dir, ".embed-version")
	stampContent := []byte(`{"version":"baseline","files":{"existing":"deadbeef"}}`)
	if err := os.WriteFile(stampPath, stampContent, 0o644); err != nil {
		t.Fatalf("WriteFile(.embed-version) error = %v", err)
	}

	before := snapshotHooksDir(t, dir)

	invalidCases := []struct {
		label string
		name  string
	}{
		{"empty", ""},
		{"path-traversal", "../etc"},
		{"forward-slash", "a/b"},
		{"interior-dots", "a..b"},
		{"whitespace", "foo bar"},
		{"with-extension", "hook.sh"},
	}

	for _, tc := range invalidCases {
		t.Run(tc.label, func(t *testing.T) {
			// cwd 空 (Tier 1 探索スキップ) と cwd 非空 (Tier 1 探索対象になり得る) の
			// 両方で同じ拒否挙動になることを確認する。Tier 0 が I/O 副作用前に拒否するため
			// repo override 配下に同名ファイルを置いても Tier 1 経路は触られない。
			repoCwd := t.TempDir()
			for _, cwd := range []string{"", repoCwd} {
				got, err := ResolveHookPath(cwd, tc.name)
				if !errors.Is(err, ErrInvalidName) {
					t.Errorf("ResolveHookPath(cwd=%q, name=%q) err = %v, want errors.Is(_, ErrInvalidName)", cwd, tc.name, err)
				}
				if got != "" {
					t.Errorf("ResolveHookPath(cwd=%q, name=%q) path = %q, want \"\"", cwd, tc.name, got)
				}
			}
		})
	}

	after := snapshotHooksDir(t, dir)

	// ファイル一覧の差分を検出する。
	for path := range before {
		if _, ok := after[path]; !ok {
			t.Errorf("ResolveHookPath 呼び出しでファイルが削除された: %s", path)
		}
	}
	for path := range after {
		if _, ok := before[path]; !ok {
			t.Errorf("ResolveHookPath 呼び出しでファイルが新規追加された: %s", path)
		}
	}
	// 内容の byte-equal 検証。
	for path, want := range before {
		got, ok := after[path]
		if !ok {
			continue // 上で検出済み
		}
		if !bytes.Equal(got, want) {
			t.Errorf("ResolveHookPath 呼び出しでファイル内容が変化した: %s\n before = %q\n after = %q", path, want, got)
		}
	}
}

// TestResolvePriorityRepoOverride は task 3.2 のテスト 1 (R1.1)。
//
// 3 tier (repo override / user-global / embed) すべてが揃っている状態で
// ResolveHookPath を呼ぶと、repo override パスが返り user-global / embed には
// 一切触れない (= 副作用なし) ことを検証する。
//
//   - 戻り値は <cwd>/.claude/hooks/<name>.sh の絶対パス
//   - user-global のファイル mtime は呼び出し前後で不変 (Tier 2 read 不発火)
//   - user-global のファイル内容も不変
func TestResolvePriorityRepoOverride(t *testing.T) {
	dir := setupHooksSandbox(t)
	repoCwd := t.TempDir()

	// Tier 1: repo override に "placeholder.sh" を配置
	repoHooksDir := filepath.Join(repoCwd, ".claude", "hooks")
	if err := os.MkdirAll(repoHooksDir, 0o755); err != nil {
		t.Fatalf("mkdir repo hooks: %v", err)
	}
	repoOverridePath := filepath.Join(repoHooksDir, "placeholder.sh")
	repoContent := []byte("#!/usr/bin/env bash\necho REPO\n")
	if err := os.WriteFile(repoOverridePath, repoContent, 0o755); err != nil {
		t.Fatalf("write repo override: %v", err)
	}

	// Tier 2: user-global にも "placeholder.sh" を配置 (= 触れられないことを確認するため)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir user-global: %v", err)
	}
	userPath := filepath.Join(dir, "placeholder.sh")
	userContent := []byte("#!/usr/bin/env bash\necho USER\n")
	if err := os.WriteFile(userPath, userContent, 0o755); err != nil {
		t.Fatalf("write user-global: %v", err)
	}
	userInfoBefore, err := os.Stat(userPath)
	if err != nil {
		t.Fatalf("stat user-global before: %v", err)
	}

	// Tier 3 (embed) は placeholder.sh を持つので、3 tier 揃っている前提が成立。

	got, err := ResolveHookPath(repoCwd, "placeholder")
	if err != nil {
		t.Fatalf("ResolveHookPath() error = %v", err)
	}
	if got != repoOverridePath {
		t.Errorf("ResolveHookPath() = %q, want %q (repo override)", got, repoOverridePath)
	}

	// user-global mtime が変化していないこと (Tier 2 read-only すら触られない契約)
	userInfoAfter, err := os.Stat(userPath)
	if err != nil {
		t.Fatalf("stat user-global after: %v", err)
	}
	if !userInfoBefore.ModTime().Equal(userInfoAfter.ModTime()) {
		t.Errorf("user-global mtime changed: before=%v, after=%v",
			userInfoBefore.ModTime(), userInfoAfter.ModTime())
	}

	// user-global 内容も不変
	gotUserContent, err := os.ReadFile(userPath)
	if err != nil {
		t.Fatalf("read user-global: %v", err)
	}
	if !bytes.Equal(gotUserContent, userContent) {
		t.Errorf("user-global content changed: got %q, want %q", gotUserContent, userContent)
	}
}

// TestResolveUserGlobalWhenNoOverride は task 3.2 のテスト 2 (R1.2 / R2.3)。
//
// repo override 不在 + user-global 既存の状態で ResolveHookPath を呼ぶと、
// user-global パスが返り embed 展開や再書き出しは発生しないことを検証する。
//
//   - 戻り値は <hooksDir>/<name>.sh の絶対パス
//   - user-global のファイル mtime は呼び出し前後で nano 秒精度で不変
//   - user-global のファイル内容も不変
func TestResolveUserGlobalWhenNoOverride(t *testing.T) {
	dir := setupHooksSandbox(t)
	repoCwd := t.TempDir() // .claude/hooks/ なし

	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir user-global: %v", err)
	}
	userPath := filepath.Join(dir, "placeholder.sh")
	userContent := []byte("#!/usr/bin/env bash\necho USER\n")
	if err := os.WriteFile(userPath, userContent, 0o755); err != nil {
		t.Fatalf("write user-global: %v", err)
	}
	userInfoBefore, err := os.Stat(userPath)
	if err != nil {
		t.Fatalf("stat user-global before: %v", err)
	}

	got, err := ResolveHookPath(repoCwd, "placeholder")
	if err != nil {
		t.Fatalf("ResolveHookPath() error = %v", err)
	}
	if got != userPath {
		t.Errorf("ResolveHookPath() = %q, want %q (user-global)", got, userPath)
	}

	// mtime 不変 (再書き出しが発生しないこと / R2.3)
	userInfoAfter, err := os.Stat(userPath)
	if err != nil {
		t.Fatalf("stat user-global after: %v", err)
	}
	if !userInfoBefore.ModTime().Equal(userInfoAfter.ModTime()) {
		t.Errorf("user-global mtime changed: before=%v, after=%v",
			userInfoBefore.ModTime(), userInfoAfter.ModTime())
	}

	// 内容も不変
	gotUserContent, err := os.ReadFile(userPath)
	if err != nil {
		t.Fatalf("read user-global: %v", err)
	}
	if !bytes.Equal(gotUserContent, userContent) {
		t.Errorf("user-global content changed: got %q, want %q", gotUserContent, userContent)
	}
}

// TestResolveSkipsRepoWhenCwdEmpty は task 3.2 のテスト 7 (R1.5)。
//
// cwd="" で呼んだとき、現在の作業ディレクトリ配下に .claude/hooks/<name>.sh が
// 存在していたとしても、Tier 1 探索はスキップされて user-global / embed のみで
// 解決される。ここでは user-global にも該当 name がなく embed FS にも該当しない
// name を使うため、結果は ErrHookNotFound に確定する (R1.4 / R6.1 と整合)。
func TestResolveSkipsRepoWhenCwdEmpty(t *testing.T) {
	_ = setupHooksSandbox(t)

	// Working directory を tmpdir に切り替え、相対パス .claude/hooks/foo.sh が
	// 存在する状態を作る。Tier 1 が誤って動作すると、相対パスがこの位置に
	// 解決されてしまうため、cwd="" による skip が真に機能しているかを検出できる。
	relDir := t.TempDir()
	repoHooks := filepath.Join(relDir, ".claude", "hooks")
	if err := os.MkdirAll(repoHooks, 0o755); err != nil {
		t.Fatalf("mkdir relative hooks: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoHooks, "foo.sh"),
		[]byte("RELATIVE"), 0o755); err != nil {
		t.Fatalf("write relative hook: %v", err)
	}
	t.Chdir(relDir)

	// embed FS に "foo.sh" は存在しないため (placeholder.sh のみ)、
	// Tier 1 が skip され Tier 2/3 も hit しなければ ErrHookNotFound に落ちる。
	got, err := ResolveHookPath("", "foo")
	if !errors.Is(err, ErrHookNotFound) {
		t.Errorf("ResolveHookPath(\"\", \"foo\") err = %v, want errors.Is(_, ErrHookNotFound)", err)
	}
	if got != "" {
		t.Errorf("ResolveHookPath(\"\", \"foo\") path = %q, want \"\"", got)
	}
}

// TestResolveErrHookNotFoundOnAllMissing は task 3.2 のテスト 6 (R1.4 / R6.1)。
//
// repo override / user-global / embed すべてに該当 name がない状態で呼ぶと、
// errors.Is(err, ErrHookNotFound) が真になり、user-global ディレクトリには
// 新規ファイルが作成されない (副作用なし) ことを検証する。
func TestResolveErrHookNotFoundOnAllMissing(t *testing.T) {
	dir := setupHooksSandbox(t)
	repoCwd := t.TempDir()

	// hooksDir は存在しない状態から呼ぶ。fallback で hooksDir 配下に何も書かれない
	// ことを ReadDir で確認したいので、最初に MkdirAll で空ディレクトリを作っておく。
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir hooksDir: %v", err)
	}
	entriesBefore, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir before: %v", err)
	}

	// embed FS には "nonexistent.sh" は存在しない (placeholder.sh のみ同梱)。
	got, err := ResolveHookPath(repoCwd, "nonexistent")
	if !errors.Is(err, ErrHookNotFound) {
		t.Errorf("ResolveHookPath() err = %v, want errors.Is(_, ErrHookNotFound)", err)
	}
	if got != "" {
		t.Errorf("ResolveHookPath() path = %q, want \"\"", got)
	}

	// user-global ディレクトリ配下にファイル新規作成が発生していないこと
	entriesAfter, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir after: %v", err)
	}
	if len(entriesAfter) != len(entriesBefore) {
		t.Errorf("user-global directory changed: before=%d entries, after=%d entries",
			len(entriesBefore), len(entriesAfter))
	}
}

// TestResolveIsDeterministicAcrossCalls は task 3.2 の auxiliary 検証 (R6.4)。
//
// 同一入力 (cwd, name, env, FS state) で複数回呼出しても結果が一致することを
// 確認する。パッケージ内可変グローバル状態 (sync.Once / package-level cache 等)
// を持たないことの行動的な保証 (実装が pure function として振る舞うこと)。
func TestResolveIsDeterministicAcrossCalls(t *testing.T) {
	dir := setupHooksSandbox(t)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	userPath := filepath.Join(dir, "placeholder.sh")
	if err := os.WriteFile(userPath, []byte("payload"), 0o755); err != nil {
		t.Fatalf("write user-global: %v", err)
	}

	const calls = 3
	paths := make([]string, calls)
	for i := range calls {
		got, err := ResolveHookPath("", "placeholder")
		if err != nil {
			t.Fatalf("call %d error = %v", i, err)
		}
		paths[i] = got
	}
	for i := 1; i < calls; i++ {
		if paths[i] != paths[0] {
			t.Errorf("non-deterministic results: %v", paths)
			break
		}
	}
}

// TestResolveExtractsEmbedToUserGlobal は task 3.3 のテスト 3。
//
// 検証対象 (R1.3 / R2.1 / R2.2 / R2.4 / R2.5 / R3.1):
// 全 tier 不在で embed のみ <name>.sh を持つ状態で ResolveHookPath を呼ぶと、
//   - user-global に <name>.sh が atomic 書き出しされる (R2.1 親ディレクトリ自動作成)
//   - 書き出されたファイルは mode 0o111 (x ビット) を含む (R2.2 / R2.4)
//   - 戻り値は絶対パス、かつ child_process.spawn 互換 (R2.5)
//   - 戻り値は <hooksDir>/<name>.sh と一致する (R1.3)
//   - <hooksDir>/.embed-version に Files[name] == sha256(embedContent) と
//     Version == version.BuildVersion が記録される (R3.1 / R3.5 / R3.6)
//   - ファイル内容は embed のバイト列と byte-equal
func TestResolveExtractsEmbedToUserGlobal(t *testing.T) {
	dir := setupHooksSandbox(t)
	repoCwd := t.TempDir() // .claude/hooks/ なし

	// 前提条件: 全 tier 不在を保証するため、user-global ディレクトリを未作成のままにする。
	// (Tier 3 経路で extractTo が親ディレクトリを遅延作成することも確認したい / R2.1)
	if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("前提条件違反: hooksDir が既に存在する (err=%v)", err)
	}

	// embed FS から placeholder.sh の現内容を取得し、期待値の sha256 を計算する。
	// 本テストは「embed の内容と user-global に書き出された内容が byte-equal」
	// および「Files[name] が embed の SHA と一致」を検証するため、embed 側の
	// 真正値をテストでも参照する。
	embedContent, err := fs.ReadFile(embedFS, "hooks/placeholder.sh")
	if err != nil {
		t.Fatalf("embedFS.ReadFile(placeholder.sh) error = %v", err)
	}
	wantSHA := sha256Bytes(embedContent)

	got, err := ResolveHookPath(repoCwd, "placeholder")
	if err != nil {
		t.Fatalf("ResolveHookPath() error = %v, want nil", err)
	}

	// R2.5 / R1.3: 戻り値は <hooksDir>/<name>.sh の絶対パス
	wantPath := filepath.Join(dir, "placeholder.sh")
	if got != wantPath {
		t.Errorf("ResolveHookPath() = %q, want %q", got, wantPath)
	}
	if !filepath.IsAbs(got) {
		t.Errorf("ResolveHookPath() = %q, want absolute path", got)
	}

	// 書き出されたファイルが存在し、embed と byte-equal
	gotContent, err := os.ReadFile(got)
	if err != nil {
		t.Fatalf("ReadFile(returned path) error = %v", err)
	}
	if !bytes.Equal(gotContent, embedContent) {
		t.Errorf("ファイル内容が embed と一致しない:\n got = %q\nwant = %q", gotContent, embedContent)
	}

	// R2.2 / R2.4: 書き出し後に POSIX 実行ビット (0o111) が立っている
	info, err := os.Stat(got)
	if err != nil {
		t.Fatalf("Stat(returned path) error = %v", err)
	}
	if perm := info.Mode().Perm(); perm&0o111 == 0 {
		t.Errorf("mode = %#o, want execute bits set (0o111)", perm)
	}

	// R3.1 / R3.5 / R3.6: .embed-version に Files[name] == sha256(embedContent) と
	// Version == version.BuildVersion が記録されている
	stampPath := filepath.Join(dir, ".embed-version")
	stampRaw, err := os.ReadFile(stampPath)
	if err != nil {
		t.Fatalf(".embed-version が存在しない: %v", err)
	}
	var stamp embedStamp
	if err := json.Unmarshal(stampRaw, &stamp); err != nil {
		t.Fatalf(".embed-version の JSON parse 失敗: %v", err)
	}
	if stamp.Version != version.BuildVersion {
		t.Errorf("stamp.Version = %q, want %q (version.BuildVersion)",
			stamp.Version, version.BuildVersion)
	}
	if got, ok := stamp.Files["placeholder"]; !ok {
		t.Errorf("stamp.Files[\"placeholder\"] が未記録")
	} else if got != wantSHA {
		t.Errorf("stamp.Files[\"placeholder\"] = %q, want %q (embed sha)", got, wantSHA)
	}
}

// TestResolveResolvesPlaceholder は task 3.3 のテスト 13 (R5.3)。
//
// ResolveHookPath("", "placeholder") が成功し、戻り値ファイルの内容が
// `#!/usr/bin/env bash` shebang を含むことを検証する。これにより、後続 issue が
// embed FS に新 hook を追加する際の「最低限の配布確認手段」(placeholder の解決)
// が機能することを保証する。
func TestResolveResolvesPlaceholder(t *testing.T) {
	_ = setupHooksSandbox(t)

	got, err := ResolveHookPath("", "placeholder")
	if err != nil {
		t.Fatalf("ResolveHookPath(\"\", \"placeholder\") error = %v, want nil", err)
	}
	if got == "" {
		t.Fatal("ResolveHookPath(\"\", \"placeholder\") returned empty path")
	}

	content, err := os.ReadFile(got)
	if err != nil {
		t.Fatalf("ReadFile(returned path) error = %v", err)
	}

	wantPrefix := []byte("#!/usr/bin/env bash")
	if !bytes.HasPrefix(content, wantPrefix) {
		t.Errorf("placeholder の内容が shebang で始まっていない: got prefix %q",
			string(content[:min(len(content), len(wantPrefix))]))
	}
}

// TestResolveOverwritesUnmodifiedOnEmbedDrift は task 3.4 のテスト 4
// (R3.3 / R3.5 / R3.6)。
//
// `Files[name]` 記録 SHA が現 embed の `currentEmbedSHA` と異なる
// (= embed が更新された) かつ ディスク内容が記録 SHA と一致する
// (= ユーザは触っていない) 状態で `ResolveHookPath` を呼ぶと、
//
//   - user-global ファイルが新しい embed 内容で上書きされる (R3.3)
//   - `.embed-version` の `Files[name]` が `currentEmbedSHA` に更新される
//   - `.embed-version` の `Version` が現在の `version.BuildVersion` に更新される (R3.5)
//
// ことを検証する。drift 検知は per-file SHA 比較で行う content-addressable
// な判定 (R3.6) であり、`stamp.Version` には依存しない。
func TestResolveOverwritesUnmodifiedOnEmbedDrift(t *testing.T) {
	dir := setupHooksSandbox(t)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll(dir) error = %v", err)
	}

	// 旧 embed 内容の simulate: 「過去に展開されたが、現 embed とは異なる」
	// 内容をディスクに書き、stamp に同じ SHA を記録しておく。これにより
	//   - Files[name] = oldSHA
	//   - diskSHA     = oldSHA  ← 一致 (= ユーザ未改変)
	//   - currentEmbedSHA = sha256(現 embed) ≠ oldSHA  ← drift あり
	// という case C (上書き) の前提が成立する。
	oldContent := []byte("#!/usr/bin/env bash\necho old embed\n")
	oldSHA := sha256Bytes(oldContent)

	userPath := filepath.Join(dir, "placeholder.sh")
	if err := os.WriteFile(userPath, oldContent, 0o755); err != nil {
		t.Fatalf("WriteFile(userPath) error = %v", err)
	}

	if err := saveStamp(dir, embedStamp{
		Version: "stamp-prior-version",
		Files:   map[string]string{"placeholder": oldSHA},
	}); err != nil {
		t.Fatalf("saveStamp error = %v", err)
	}

	embedContent, err := fs.ReadFile(embedFS, "hooks/placeholder.sh")
	if err != nil {
		t.Fatalf("read embed: %v", err)
	}
	embedSHA := sha256Bytes(embedContent)

	// テスト前提のサニティチェック: oldSHA ≠ currentEmbedSHA でないと
	// drift 経路に入らずテストが意味を失う。
	if embedSHA == oldSHA {
		t.Fatalf("test setup invalid: oldSHA must differ from currentEmbedSHA")
	}

	got, err := ResolveHookPath("", "placeholder")
	if err != nil {
		t.Fatalf("ResolveHookPath() error = %v", err)
	}
	if got != userPath {
		t.Errorf("ResolveHookPath() = %q, want %q (user-global)", got, userPath)
	}

	// R3.3: ディスク内容が新しい embed 内容に上書きされている
	gotContent, err := os.ReadFile(userPath)
	if err != nil {
		t.Fatalf("ReadFile(userPath) error = %v", err)
	}
	if !bytes.Equal(gotContent, embedContent) {
		t.Errorf("disk content not overwritten with embed content:\n got = %q\nwant = %q",
			gotContent, embedContent)
	}

	// 上書き後も実行ビット (R2.2 / R2.4) は保持される
	info, err := os.Stat(userPath)
	if err != nil {
		t.Fatalf("Stat(userPath) error = %v", err)
	}
	if perm := info.Mode().Perm(); perm&0o111 == 0 {
		t.Errorf("mode = %#o after overwrite, want execute bits (0o111) set", perm)
	}

	// R3.5 / R3.6: stamp.Files[name] = currentEmbedSHA / Version = BuildVersion
	stamp, err := loadStamp(dir)
	if err != nil {
		t.Fatalf("loadStamp error = %v", err)
	}
	if stamp.Files["placeholder"] != embedSHA {
		t.Errorf("Files[placeholder] = %q, want %q (currentEmbedSHA)",
			stamp.Files["placeholder"], embedSHA)
	}
	if stamp.Version != version.BuildVersion {
		t.Errorf("Version = %q, want %q (BuildVersion)",
			stamp.Version, version.BuildVersion)
	}
}

// TestResolvePreservesModifiedFileOnEmbedDrift は task 3.4 のテスト 5
// (R3.4 / R3.5 / R3.6)。
//
// `Files[name]` 記録 SHA が現 embed の `currentEmbedSHA` と異なる + ディスク
// 内容が記録 SHA と異なる (= ユーザ改変あり) 状態で `ResolveHookPath` を呼ぶと、
//
//   - user-global ファイルは保持され、上書きされない (R3.4)
//   - `.embed-version` の `Files[name]` がディスク現状の SHA に更新される
//     (再評価時に drift なしと判定されるよう収束)
//   - `.embed-version` の `Version` が現 `BuildVersion` に更新される (R3.5)
//
// ことを検証する。
func TestResolvePreservesModifiedFileOnEmbedDrift(t *testing.T) {
	dir := setupHooksSandbox(t)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll(dir) error = %v", err)
	}

	// 「過去に展開された旧 embed 内容」を SHA だけ stamp に残し、
	// ディスクには「ユーザ改変済み」内容を書き込む。
	//   - Files[name] = recordedSHA (= かつての embed)
	//   - diskSHA     = modifiedSHA  ← Files[name] と一致しない (改変あり)
	//   - currentEmbedSHA = sha256(現 embed)  ← Files[name] とも diskSHA とも異なる
	// という case D の前提を成立させる。
	recordedSHA := sha256Bytes([]byte("#!/usr/bin/env bash\necho previously-extracted\n"))
	modifiedContent := []byte("#!/usr/bin/env bash\necho user-modified\n")
	modifiedSHA := sha256Bytes(modifiedContent)

	userPath := filepath.Join(dir, "placeholder.sh")
	if err := os.WriteFile(userPath, modifiedContent, 0o755); err != nil {
		t.Fatalf("WriteFile(userPath) error = %v", err)
	}

	if err := saveStamp(dir, embedStamp{
		Version: "stamp-prior-version",
		Files:   map[string]string{"placeholder": recordedSHA},
	}); err != nil {
		t.Fatalf("saveStamp error = %v", err)
	}

	embedContent, err := fs.ReadFile(embedFS, "hooks/placeholder.sh")
	if err != nil {
		t.Fatalf("read embed: %v", err)
	}
	embedSHA := sha256Bytes(embedContent)

	// サニティ: 三者が互いに異なること
	if recordedSHA == modifiedSHA {
		t.Fatalf("test setup invalid: recordedSHA must differ from modifiedSHA")
	}
	if recordedSHA == embedSHA {
		t.Fatalf("test setup invalid: recordedSHA must differ from currentEmbedSHA")
	}
	if modifiedSHA == embedSHA {
		t.Fatalf("test setup invalid: modifiedSHA must differ from currentEmbedSHA")
	}

	got, err := ResolveHookPath("", "placeholder")
	if err != nil {
		t.Fatalf("ResolveHookPath() error = %v", err)
	}
	if got != userPath {
		t.Errorf("ResolveHookPath() = %q, want %q", got, userPath)
	}

	// R3.4: ディスク内容が改変ファイルのまま保持されている
	gotContent, err := os.ReadFile(userPath)
	if err != nil {
		t.Fatalf("ReadFile(userPath) error = %v", err)
	}
	if !bytes.Equal(gotContent, modifiedContent) {
		t.Errorf("user-modified content was overwritten:\n got = %q\nwant = %q (preserved)",
			gotContent, modifiedContent)
	}

	// R3.5: stamp.Files[name] = diskSHA (再評価時に case B で no-op になるよう収束)
	stamp, err := loadStamp(dir)
	if err != nil {
		t.Fatalf("loadStamp error = %v", err)
	}
	if stamp.Files["placeholder"] != modifiedSHA {
		t.Errorf("Files[placeholder] = %q, want %q (diskSHA, ensures convergence)",
			stamp.Files["placeholder"], modifiedSHA)
	}
	if stamp.Version != version.BuildVersion {
		t.Errorf("Version = %q, want %q",
			stamp.Version, version.BuildVersion)
	}
}

// TestResolvePerFileDriftIndependence は task 3.4 のテスト 5b
// (R3.2 / R3.3 per-file 独立性 / Critical Issue 2 回帰防止)。
//
// 同一 stamp 内に hook A (placeholder, embed FS 同梱) と hook B (custom, embed
// FS 非同梱) の Files 記録があるとき、A を `ResolveHookPath` で解決して
// `Files[A]` が refresh されても、B の `Files[B]` 記録は触られないことを
// 検証する。これにより per-file 独立な drift 判定 (R3.2 / R3.3) が
// 担保される。
//
// hook B (`custom`) は embed FS に同梱されていないため、本テストでは
// B 自体の解決は行わず、A 解決後の stamp 状態で `Files[B]` が
// 呼び出し前と byte-equal で残ることを assert する。
func TestResolvePerFileDriftIndependence(t *testing.T) {
	dir := setupHooksSandbox(t)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll(dir) error = %v", err)
	}

	// hook A (placeholder): 「旧 embed 内容で展開されたまま、ユーザ未改変」
	// な状態を simulate (case C)。Files[A] = oldASHA = diskASHA。
	oldAContent := []byte("#!/usr/bin/env bash\necho A old\n")
	oldASHA := sha256Bytes(oldAContent)
	userAPath := filepath.Join(dir, "placeholder.sh")
	if err := os.WriteFile(userAPath, oldAContent, 0o755); err != nil {
		t.Fatalf("WriteFile(userAPath) error = %v", err)
	}

	// hook B (custom): embed FS 非同梱の hook 用 stamp 記録のみを用意する。
	// B の disk file は本テストでは参照しないが、stamp.Files[B] は
	// 「触られない」ことを直接検証するため任意の固定値で良い。
	customSHA := "b" + strings.Repeat("0", 63) // hex 64 桁、A の oldSHA と差別化

	initialStamp := embedStamp{
		Version: "prior-stamp-version",
		Files: map[string]string{
			"placeholder": oldASHA,
			"custom":      customSHA,
		},
	}
	if err := saveStamp(dir, initialStamp); err != nil {
		t.Fatalf("saveStamp error = %v", err)
	}

	embedContent, err := fs.ReadFile(embedFS, "hooks/placeholder.sh")
	if err != nil {
		t.Fatalf("read embed: %v", err)
	}
	embedSHA := sha256Bytes(embedContent)
	if embedSHA == oldASHA {
		t.Fatalf("test setup invalid: oldASHA must differ from currentEmbedSHA")
	}

	// hook A を解決 (= drift があるので Files[A] が refresh される経路)
	if _, err := ResolveHookPath("", "placeholder"); err != nil {
		t.Fatalf("ResolveHookPath(\"\", \"placeholder\") error = %v", err)
	}

	finalStamp, err := loadStamp(dir)
	if err != nil {
		t.Fatalf("loadStamp error = %v", err)
	}

	// hook A は currentEmbedSHA に refresh される (case C)
	if finalStamp.Files["placeholder"] != embedSHA {
		t.Errorf("Files[placeholder] = %q, want %q (refreshed to currentEmbedSHA)",
			finalStamp.Files["placeholder"], embedSHA)
	}

	// hook B の Files 記録は呼び出し前後で完全一致 (per-file 独立性)
	if got := finalStamp.Files["custom"]; got != customSHA {
		t.Errorf("Files[custom] = %q, want %q (must be preserved across A's resolution)",
			got, customSHA)
	}

	// stamp.Version は最新 BuildVersion に更新される
	if finalStamp.Version != version.BuildVersion {
		t.Errorf("Version = %q, want %q",
			finalStamp.Version, version.BuildVersion)
	}
}

// TestResolveDetectsDriftWhenBuildVersionUnchanged は task 3.4 のテスト 5c
// (R3.6 / Open Question 1 構造的解消の回帰防止)。
//
// `stamp.Version == version.BuildVersion` の状態であっても、embed 内容のみが
// 変更された (per-file SHA が ずれた) 場合は drift として検知され、未改変
// ファイルが新しい embed 内容に追従更新されることを検証する。
//
// これにより、`BuildVersion="dev"` や同一 version 内 embed 更新でも drift
// が取りこぼされない (per-file content-addressable 判定が成立する)。
func TestResolveDetectsDriftWhenBuildVersionUnchanged(t *testing.T) {
	dir := setupHooksSandbox(t)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll(dir) error = %v", err)
	}

	oldContent := []byte("#!/usr/bin/env bash\necho previous\n")
	oldSHA := sha256Bytes(oldContent)

	userPath := filepath.Join(dir, "placeholder.sh")
	if err := os.WriteFile(userPath, oldContent, 0o755); err != nil {
		t.Fatalf("WriteFile(userPath) error = %v", err)
	}

	// stamp.Version は現在の BuildVersion と同一にする (= バージョン不変)。
	if err := saveStamp(dir, embedStamp{
		Version: version.BuildVersion,
		Files:   map[string]string{"placeholder": oldSHA},
	}); err != nil {
		t.Fatalf("saveStamp error = %v", err)
	}

	embedContent, err := fs.ReadFile(embedFS, "hooks/placeholder.sh")
	if err != nil {
		t.Fatalf("read embed: %v", err)
	}
	embedSHA := sha256Bytes(embedContent)
	if embedSHA == oldSHA {
		t.Fatalf("test setup invalid: oldSHA must differ from currentEmbedSHA")
	}

	if _, err := ResolveHookPath("", "placeholder"); err != nil {
		t.Fatalf("ResolveHookPath() error = %v", err)
	}

	// R3.6: per-file SHA 比較が機能し、未改変ファイルが embed 内容に追従更新
	gotContent, err := os.ReadFile(userPath)
	if err != nil {
		t.Fatalf("ReadFile(userPath) error = %v", err)
	}
	if !bytes.Equal(gotContent, embedContent) {
		t.Errorf("disk content not refreshed (BuildVersion unchanged path):\n got = %q\nwant = %q",
			gotContent, embedContent)
	}

	stamp, err := loadStamp(dir)
	if err != nil {
		t.Fatalf("loadStamp error = %v", err)
	}
	if stamp.Files["placeholder"] != embedSHA {
		t.Errorf("Files[placeholder] = %q, want %q",
			stamp.Files["placeholder"], embedSHA)
	}
}

// TestResolveWrapsIOErrors は task 3.5 のテスト 8 (R6.2)。
//
// hooksDir を 0o500 (read-only) にして書き込み権限を奪った状態で embed 展開を
// 試みると、ResolveHookPath は以下の挙動を満たす:
//
//   - 戻り値 err != nil
//   - errors.Is(err, ErrHookNotFound) は偽 (sentinel と区別される)
//   - errors.Is(err, ErrInvalidName) は偽 (sentinel と区別される)
//   - error は fmt.Errorf("hooks: ...: %w", origErr) で wrap されており、
//     errors.Unwrap で原因の OS エラー (typically *os.PathError / syscall.Errno
//     EACCES) を取り出せる
//   - error message は "hooks: " で始まる (パッケージ contract)
//
// これにより I/O wrapped エラーが ErrHookNotFound / ErrInvalidName と sentinel
// として区別されることが行動レベルで担保される (3 系統排他性の中核ケース)。
func TestResolveWrapsIOErrors(t *testing.T) {
	// root はファイル mode による権限チェックを bypass するため、本テストは
	// 通常ユーザでのみ意味を持つ。CI で root 実行になるケースを想定して skip する。
	if os.Geteuid() == 0 {
		t.Skip("root はファイル権限を bypass するためスキップ")
	}

	dir := setupHooksSandbox(t)

	// hooksDir を作成してから 0o500 (r-x------) に変更し、書き込み権限を奪う。
	// この状態で embed 展開 (extractTo → OpenFile O_CREATE) を実行すると、
	// 親ディレクトリ書き込み不可で EACCES が返る想定。
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll(dir) error = %v", err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("Chmod(dir, 0o500) error = %v", err)
	}
	// テスト後に削除可能になるよう、Cleanup で書き込み権限を復元する。
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	// placeholder は embed FS に同梱されているため、user-global 不在の状態で
	// ResolveHookPath を呼ぶと Tier 3 (embed → user-global 自動展開) を走る。
	// hooksDir が read-only のため、extractTo の OpenFile が EACCES で失敗する。
	_, err := ResolveHookPath("", "placeholder")
	if err == nil {
		t.Fatal("ResolveHookPath() error = nil, want wrapped I/O error")
	}

	// sentinel との区別 (R6.1 区別可能性 / R6.2)
	if errors.Is(err, ErrHookNotFound) {
		t.Errorf("errors.Is(err, ErrHookNotFound) = true; want false (wrapped I/O error): %v", err)
	}
	if errors.Is(err, ErrInvalidName) {
		t.Errorf("errors.Is(err, ErrInvalidName) = true; want false (wrapped I/O error): %v", err)
	}

	// パッケージ contract: error message は "hooks: " で始まる
	if !strings.HasPrefix(err.Error(), "hooks: ") {
		t.Errorf("err.Error() = %q, want prefix %q", err.Error(), "hooks: ")
	}

	// errors.Unwrap で原因が取り出せる (fmt.Errorf("...%w", origErr) 形式の検証)
	cause := errors.Unwrap(err)
	if cause == nil {
		t.Fatalf("errors.Unwrap(err) = nil; want underlying OS error: %v", err)
	}

	// chain 上に *os.PathError か syscall.Errno が含まれること (POSIX EACCES の典型形)
	var pathErr *os.PathError
	var syscallErrno syscall.Errno
	if !errors.As(err, &pathErr) && !errors.As(err, &syscallErrno) {
		t.Errorf("error chain does not contain *os.PathError or syscall.Errno: %v", err)
	}

	// permission denied が chain 上に存在すること (EACCES シナリオの確証)
	if !errors.Is(err, os.ErrPermission) && !errors.Is(err, syscall.EACCES) {
		t.Errorf("error chain does not contain permission-denied cause: %v", err)
	}
}

// TestResolveErrorCategoriesAreMutuallyExclusive は task 3.5 の補助テスト
// (R6.1 区別可能性 / R6.2 排他性)。
//
// ErrHookNotFound / ErrInvalidName / I/O wrapped エラー の 3 系統が
// errors.Is で互いに排他であることを行動レベルで網羅検証する。
//
// TestPackage_SentinelErrorsAreNonNilAndDistinct (task 1.2) では
// sentinel 同士の排他性のみを検証していたが、本テストでは「wrapped I/O」
// 系統を加えた 3 系統の網羅排他を確認する。
func TestResolveErrorCategoriesAreMutuallyExclusive(t *testing.T) {
	// 代表的な wrapped I/O error (権限不足を想定)
	wrappedIO := fmt.Errorf("hooks: dummy I/O: %w", os.ErrPermission)

	cases := []struct {
		name string
		err  error
		// expected matches: 真であるべき sentinel 群
		isHookNotFound bool
		isInvalidName  bool
	}{
		{name: "ErrHookNotFound", err: ErrHookNotFound, isHookNotFound: true, isInvalidName: false},
		{name: "ErrInvalidName", err: ErrInvalidName, isHookNotFound: false, isInvalidName: true},
		{name: "wrapped I/O", err: wrappedIO, isHookNotFound: false, isInvalidName: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := errors.Is(tc.err, ErrHookNotFound); got != tc.isHookNotFound {
				t.Errorf("errors.Is(%v, ErrHookNotFound) = %v, want %v", tc.err, got, tc.isHookNotFound)
			}
			if got := errors.Is(tc.err, ErrInvalidName); got != tc.isInvalidName {
				t.Errorf("errors.Is(%v, ErrInvalidName) = %v, want %v", tc.err, got, tc.isInvalidName)
			}
		})
	}

	// wrapped I/O から errors.Unwrap で原因を取り出せること (R6.2 形式契約)
	if cause := errors.Unwrap(wrappedIO); cause != os.ErrPermission {
		t.Errorf("errors.Unwrap(wrappedIO) = %v, want os.ErrPermission", cause)
	}

	// wrapped I/O は "hooks: " で始まること (パッケージ contract)
	if !strings.HasPrefix(wrappedIO.Error(), "hooks: ") {
		t.Errorf("wrappedIO.Error() = %q, want prefix %q", wrappedIO.Error(), "hooks: ")
	}
}

// snapshotHooksDir は root 配下のすべてのファイルを map[relPath][]byte として
// 取得する。byte-equal snapshot 比較で I/O 副作用の有無を検証するためのヘルパ。
func snapshotHooksDir(t *testing.T, root string) map[string][]byte {
	t.Helper()
	snap := make(map[string][]byte)
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		snap[rel] = b
		return nil
	})
	if err != nil {
		t.Fatalf("snapshotHooksDir error = %v", err)
	}
	return snap
}

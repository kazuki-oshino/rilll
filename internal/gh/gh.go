// Package gh は `gh` CLI を `os/exec.CommandContext` で薄くラップする leaf パッケージ。
//
// 提供機能:
//   - `CheckCLI` で gh バイナリの存在 + `gh auth status` の認証成否を 1 つに集約する
//   - `ListPRs` / `ViewPR` / `DiffPR` で PR メタデータ・一覧・diff を取得する
//
// エラー分類は exit code を一次判別軸とし、stderr 文言は同じ exit code 内で
// `ErrUnauthorized` / `ErrPRNotFound` を区別するための補助情報として用いる。
// gh の文言が変わっても exit code 0/非 0 の判定が壊れないよう、stderr マッチは
// closed set の部分文字列検査だけを行う。
//
// stdout は最大 10 MiB で打ち切り、`ctx.Done()` が発火したら即時 cancel される
// (`exec.CommandContext` が SIGKILL を送る)。
package gh

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

const (
	commandName    = "gh"
	maxBufferBytes = 10 * 1024 * 1024
)

// PRMeta は `gh pr view <num> --json title,body,files,additions,deletions` の出力を表す。
//
// `Files` は gh の `[{path: "..."}]` 形式を `[]string` に flatten した値が入る。
// `Number` は呼び出し時の `<num>` 引数をそのまま転記する (gh 出力には含まれない)。
type PRMeta struct {
	Number    int      `json:"number"`
	Title     string   `json:"title"`
	Body      string   `json:"body"`
	Files     []string `json:"files"`
	Additions int      `json:"additions"`
	Deletions int      `json:"deletions"`
}

// PRListItem は `gh pr list --json number,title` の最小投影。
type PRListItem struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
}

// gh CLI 関連の sentinel error。呼び出し側は `errors.Is` で分岐する。
var (
	// ErrCLIMissing は gh バイナリが PATH に見つからない、または gh が JSON
	// 出力契約を満たさない (古い gh) ことを示す。古い gh fallback も含めて
	// 「gh の利用条件が満たされていない」を 1 つの sentinel に集約する。
	ErrCLIMissing = errors.New("gh: CLI が見つからない (PATH を確認してください)")
	// ErrUnauthorized は gh が認証されていないことを示す。
	ErrUnauthorized = errors.New("gh: 未認証 (gh auth login を実行してください)")
	// ErrPRNotFound は指定 PR が存在しない、またはアクセス権がないことを示す。
	ErrPRNotFound = errors.New("gh: PR が存在しない / アクセス権がない")
)

// CheckCLI は `gh` バイナリの PATH 解決と `gh auth status` の認証検証を 1 度に行う。
// 成功で nil、PATH 不在で ErrCLIMissing、auth 非ゼロで ErrUnauthorized を返す。
func CheckCLI(ctx context.Context) error {
	if _, err := exec.LookPath(commandName); err != nil {
		return ErrCLIMissing
	}
	_, _, exitCode, runErr := runGh(ctx, "", "auth", "status")
	if runErr != nil {
		return runErr
	}
	if exitCode == 0 {
		return nil
	}
	return ErrUnauthorized
}

// ListPRs は `gh pr list --state <state> --limit <n> --json number,title` を実行する。
// state が空のときは "open"、limit が 0 以下のときは 200 を既定値とする。
func ListPRs(ctx context.Context, cwd, state string, limit int) ([]PRListItem, error) {
	if state == "" {
		state = "open"
	}
	if limit <= 0 {
		limit = 200
	}
	args := []string{
		"pr", "list",
		"--state", state,
		"--limit", strconv.Itoa(limit),
		"--json", "number,title",
	}
	stdout, stderr, exitCode, err := runGh(ctx, cwd, args...)
	if err != nil {
		return nil, err
	}
	if exitCode != 0 {
		if isAuthError(stderr) {
			return nil, ErrUnauthorized
		}
		return nil, fmt.Errorf("gh pr list: exit %d: %s", exitCode, strings.TrimSpace(stderr))
	}
	if strings.TrimSpace(stdout) == "" {
		return nil, nil
	}
	var items []PRListItem
	if err := json.Unmarshal([]byte(stdout), &items); err != nil {
		return nil, fmt.Errorf("gh pr list: JSON 解釈不能: %w", err)
	}
	return items, nil
}

// ViewPR は `gh pr view <num> --json title,body,files,additions,deletions` を実行し、
// PRMeta に詰め直して返す。`Files` は `[{path: ...}]` を `[]string` に flatten する。
//
// 古い gh で `--json` 未対応の場合は stderr に warning が出て exit 0 で抜けるため、
// JSON パース失敗を `ErrCLIMissing` 派生 error として返す (利用条件不充足扱い)。
func ViewPR(ctx context.Context, cwd string, num int) (PRMeta, error) {
	args := []string{
		"pr", "view", strconv.Itoa(num),
		"--json", "title,body,files,additions,deletions",
	}
	stdout, stderr, exitCode, err := runGh(ctx, cwd, args...)
	if err != nil {
		return PRMeta{}, err
	}
	if exitCode != 0 {
		// PR 不存在は他の認証エラーよりメッセージ特異性が高いので先に判定する
		if isPRNotFoundError(stderr) {
			return PRMeta{}, ErrPRNotFound
		}
		if isAuthError(stderr) {
			return PRMeta{}, ErrUnauthorized
		}
		return PRMeta{}, fmt.Errorf("gh pr view: exit %d: %s", exitCode, strings.TrimSpace(stderr))
	}
	var raw struct {
		Title string `json:"title"`
		Body  string `json:"body"`
		Files []struct {
			Path string `json:"path"`
		} `json:"files"`
		Additions int `json:"additions"`
		Deletions int `json:"deletions"`
	}
	if jerr := json.Unmarshal([]byte(stdout), &raw); jerr != nil {
		// 古い gh で --json 未対応 → JSON ではない出力で来る。利用条件不充足として ErrCLIMissing 派生にする。
		return PRMeta{}, fmt.Errorf("%w: gh pr view JSON 解釈不能 (古い gh CLI かもしれません): %v", ErrCLIMissing, jerr)
	}
	files := make([]string, 0, len(raw.Files))
	for _, f := range raw.Files {
		files = append(files, f.Path)
	}
	return PRMeta{
		Number:    num,
		Title:     raw.Title,
		Body:      raw.Body,
		Files:     files,
		Additions: raw.Additions,
		Deletions: raw.Deletions,
	}, nil
}

// DiffPR は `gh pr diff <num>` の stdout を文字列でそのまま返す。
func DiffPR(ctx context.Context, cwd string, num int) (string, error) {
	args := []string{"pr", "diff", strconv.Itoa(num)}
	stdout, stderr, exitCode, err := runGh(ctx, cwd, args...)
	if err != nil {
		return "", err
	}
	if exitCode != 0 {
		if isPRNotFoundError(stderr) {
			return "", ErrPRNotFound
		}
		if isAuthError(stderr) {
			return "", ErrUnauthorized
		}
		return "", fmt.Errorf("gh pr diff: exit %d: %s", exitCode, strings.TrimSpace(stderr))
	}
	return stdout, nil
}

// runGh は内部的な exec ヘルパ。PATH 不在のときは ErrCLIMissing を、
// プロセス実行失敗 (exit 非 0 を含まない例外) のときは err を返す。
// exit code が判定可能な非ゼロ終了は err=nil + exitCode>0 で呼び出し側に渡す
// (=「gh は走ったが業務上のエラーで終了した」を区別できるようにする)。
func runGh(ctx context.Context, cwd string, args ...string) (stdout, stderr string, exitCode int, err error) {
	if _, lerr := exec.LookPath(commandName); lerr != nil {
		return "", "", -1, ErrCLIMissing
	}
	cmd := exec.CommandContext(ctx, commandName, args...)
	if cwd != "" {
		cmd.Dir = cwd
	}
	cmd.Env = os.Environ()

	var so, se limitedBuffer
	so.limit = maxBufferBytes
	se.limit = maxBufferBytes
	cmd.Stdout = &so
	cmd.Stderr = &se

	runErr := cmd.Run()
	stdout = so.String()
	stderr = se.String()
	if runErr == nil {
		return stdout, stderr, 0, nil
	}
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		return stdout, stderr, exitErr.ExitCode(), nil
	}
	// PATH 解決成功後に実行 itself が壊れたケース (パーミッション等)
	return stdout, stderr, -1, fmt.Errorf("gh %s: %w", strings.Join(args, " "), runErr)
}

// isPRNotFoundError は PR 不存在を示す stderr 文言かを判定する。
// gh の文言変更に過敏にならないよう、複数の代表的フレーズだけを大文字小文字無視でマッチする。
func isPRNotFoundError(stderr string) bool {
	s := strings.ToLower(stderr)
	return strings.Contains(s, "could not resolve to a pullrequest") ||
		strings.Contains(s, "no pull requests found") ||
		strings.Contains(s, "no pr found") ||
		strings.Contains(s, "pull request not found")
}

// isAuthError は未認証を示す stderr 文言かを判定する。
func isAuthError(stderr string) bool {
	s := strings.ToLower(stderr)
	return strings.Contains(s, "you are not logged") ||
		strings.Contains(s, "gh auth login") ||
		strings.Contains(s, "not authenticated") ||
		strings.Contains(s, "authentication required")
}

// limitedBuffer は stdout/stderr を上限まで蓄積し、超過分は黙って捨てる
// (= cursor provider の `limitedBuffer` と異なり error にしない)。
// 設計意図: PR diff が想定外に巨大でも、agent への入力として後段で打ち切る前提で
// gh ラッパ層では process を殺さずに切り捨てだけ行う。
type limitedBuffer struct {
	bytes.Buffer
	limit int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.limit <= 0 {
		return b.Buffer.Write(p)
	}
	remaining := b.limit - b.Len()
	if remaining <= 0 {
		// 既に上限到達: 全量を捨てるが書き込み成功扱いで返す (subprocess が SIGPIPE で死なないようにする)
		return len(p), nil
	}
	if len(p) <= remaining {
		return b.Buffer.Write(p)
	}
	if _, err := b.Buffer.Write(p[:remaining]); err != nil {
		return 0, err
	}
	return len(p), nil
}

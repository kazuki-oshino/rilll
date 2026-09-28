// Package akashic の reader_internal.go は、Reader 実装 (reader_sqlite.go) が依存する
// 内部ヘルパ群を提供する。
//
// 提供するヘルパ:
//   - percentile([]int64, fraction): ソート済み配列からの p50/p95 等の算出 (Req 3.3)
//   - normalizeError(string): 失敗エラーメッセージの正規化 (Req 5.2)
//   - sql.Null* → ポインタ / 値への変換ヘルパ群 (scan 共通化)
//
// すべて pure function として package private に定義し、外部 (CLI / 下流 spec) からは
// 呼び出されない。網羅的な単体テストはタスク 4.5 で reader_test.go に追加する想定で、
// 本ファイルの companion test (reader_internal_test.go) はタスク 1.3 の観測可能な
// 完了条件 (verification example) を担保する最小限の smoke test に留める。
package akashic

import (
	"database/sql"
	"math"
	"regexp"
	"strings"
	"time"
)

// maxNormalizedErrorLen は normalizeError が出力文字列に適用する上限長 (バイト単位、
// design.md L598: "先頭 80 文字")。クラスタリングキーが暴走的に長くならないよう
// 最終段で切り詰める。
const maxNormalizedErrorLen = 80

// percentile はソート済み (昇順) int64 配列から指定 fraction の値を線形補間で算出する
// (Req 3.3: agent_calls.duration_ms の p50/p95)。
//
// 入力契約:
//   - sorted は呼び出し側で昇順にソート済みであること。本関数は順序を仮定し、内部で
//     再ソートしない (sort.Slice の N log N コストを避ける)
//   - fraction は [0.0, 1.0] の範囲 (p50 → 0.5, p95 → 0.95)。範囲外でもクランプされる
//
// 戻り値の規約:
//   - 空 (nil または len==0) → nil ("計算不能" を呼び出し側に伝搬)
//   - 単一要素 → そのままその値を *int64 で返す
//   - 複数要素 → fraction*(n-1) で算出した位置の線形補間値を *int64 で返す。
//     線形補間後は math.Round で int64 に丸める
//
// 線形補間を採用する理由: SQLite に PERCENTILE_CONT がないため Go 側で計算するが、
// 統計的に揺らぎが小さく、表示用 (UI / CLI export) で違和感の小さい値を返す。
func percentile(sorted []int64, fraction float64) *int64 {
	n := len(sorted)
	if n == 0 {
		return nil
	}
	if n == 1 {
		v := sorted[0]
		return &v
	}
	// fraction を [0,1] にクランプ。範囲外を呼び出し側のバグで通しても暴発しないように。
	if fraction <= 0 {
		v := sorted[0]
		return &v
	}
	if fraction >= 1 {
		v := sorted[n-1]
		return &v
	}
	// 線形補間: pos = fraction * (n-1)
	pos := fraction * float64(n-1)
	lo := int(math.Floor(pos))
	hi := int(math.Ceil(pos))
	if lo == hi {
		v := sorted[lo]
		return &v
	}
	weight := pos - float64(lo)
	interp := float64(sorted[lo])*(1-weight) + float64(sorted[hi])*weight
	v := int64(math.Round(interp))
	return &v
}

// normalizeError は失敗エラーメッセージをクラスタリングキーに使える正規化文字列に
// 変換する (Req 5.2 / design.md L598)。
//
// 適用するヒューリスティック (この順番で適用):
//
//  1. 連続空白 (タブ / 改行含む) を 1 つの半角空白に圧縮
//  2. UUID 様 ([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}) → "<UUID>"
//  3. 絶対パス様 ("/" で始まる連続非空白) → "<PATH>"
//  4. 連続数字列 → "<NUM>"
//  5. 出力長を maxNormalizedErrorLen (80 バイト) で切り詰め
//
// 順序設計の根拠:
//   - UUID は最も特異 (16 進 + ハイフン区切り) なので最初に消費する。先に数字マスキングを
//     掛けると UUID 内の数字部分が <NUM> 化されてマッチしなくなる
//   - パスは内部に数字を含み得る (例: /tmp/sock-1234) ため、先にパス全体を <PATH> に
//     潰してから数字マスキングを掛ける。逆順だと "/tmp/sock-<NUM>" が残ってクラスタリング
//     キーがブレる
//   - 80 文字切り詰めは最終段で行う (マスキング前に切ると UUID パターンが途中で切れて
//     マッチ不能になる)
//
// design.md L598 では「先頭 80 文字 → 連続空白を 1 つに → 数字 / UUID / パス」と書かれて
// いるが、上記の順序設計上の根拠から、本実装は先に空白圧縮 + マスキングを掛けてから
// 80 文字に切り詰める方針を取る。クラスタリング目的では切り詰め順序の差異は最終キーの
// 表現にのみ影響し、cluster の意味的グルーピングに齟齬は発生しない。
//
// ハッシュ化はせず可読文字列をそのまま返す (運用時の cluster 視認性を優先 / design.md L598)。
func normalizeError(s string) string {
	if s == "" {
		return ""
	}

	// 1. 連続空白圧縮 (タブ・改行含むあらゆる whitespace を半角空白に正規化)
	s = collapseWhitespaceRE.ReplaceAllString(s, " ")
	s = strings.TrimSpace(s)

	// 2. UUID マスキング (16 進 8-4-4-4-12 形式)
	s = uuidRE.ReplaceAllString(s, "<UUID>")

	// 3. 絶対パスマスキング ("/" で始まる連続非空白)
	s = pathRE.ReplaceAllString(s, "<PATH>")

	// 4. 連続数字列マスキング
	s = numberRE.ReplaceAllString(s, "<NUM>")

	// 5. 80 文字切り詰め (バイト単位)。クラスタキー表示の上限長を保証する。
	//    実運用ではエラーメッセージは概ね ASCII 想定 (Go / SQLite / プロバイダ API の
	//    エラー文字列) のため、バイト単位 = 文字単位として扱える。マルチバイト文字を
	//    含む場合に途中で切れる可能性は cluster key 表示の上限長を優先して許容する。
	if len(s) > maxNormalizedErrorLen {
		s = s[:maxNormalizedErrorLen]
	}
	return s
}

// 正規表現はパッケージ初期化時に 1 度だけコンパイルし、normalizeError 呼び出しごとの
// オーバーヘッドを排除する。
var (
	// collapseWhitespaceRE は連続する任意 whitespace (空白 / タブ / 改行 / ...) を
	// マッチする。半角空白 1 つに置換する用途。
	collapseWhitespaceRE = regexp.MustCompile(`\s+`)
	// uuidRE は 16 進 8-4-4-4-12 形式の UUID をマッチする。大文字小文字両対応。
	uuidRE = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
	// pathRE は "/" で始まる連続非空白をマッチする (Unix 系絶対パス)。
	// Windows パス (C:\...) は本 spec のスコープでは扱わない (運用環境が macOS/Linux 前提)。
	pathRE = regexp.MustCompile(`/[^\s]+`)
	// numberRE は連続数字列をマッチする。
	numberRE = regexp.MustCompile(`[0-9]+`)
)

// nullBoolPtr は sql.NullBool を *bool に変換する。
// Valid=false → nil / Valid=true → ポインタ経由で値を返す (NULL と false を静的に区別)。
func nullBoolPtr(v sql.NullBool) *bool {
	if !v.Valid {
		return nil
	}
	b := v.Bool
	return &b
}

// nullInt64Ptr は sql.NullInt64 を *int64 に変換する。
// Valid=false → nil / Valid=true → ポインタ経由で値を返す (NULL と 0 を静的に区別)。
func nullInt64Ptr(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	i := v.Int64
	return &i
}

// nullFloat64Ptr は sql.NullFloat64 を *float64 に変換する。
// Valid=false → nil / Valid=true → ポインタ経由で値を返す (NULL と 0.0 を静的に区別)。
func nullFloat64Ptr(v sql.NullFloat64) *float64 {
	if !v.Valid {
		return nil
	}
	f := v.Float64
	return &f
}

// nullStringValue は sql.NullString を string に変換する。
// Valid=false → 空文字列 (NULL と "" を区別しない設計判断: Reader 結果型では string で
// 揃える方が使い勝手が良い)。
func nullStringValue(v sql.NullString) string {
	if !v.Valid {
		return ""
	}
	return v.String
}

// nullTimePtr は sql.NullTime を *time.Time に変換する。
// Valid=false → nil / Valid=true → ポインタ経由で値を返す (NULL とゼロ時刻を区別)。
func nullTimePtr(v sql.NullTime) *time.Time {
	if !v.Valid {
		return nil
	}
	t := v.Time
	return &t
}

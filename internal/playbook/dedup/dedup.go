// Package dedup は playbook の bullet 行に対する pure-Go token-jaccard 類似度関数
// (`JaccardDedup` / `IsSimilar`) と既定閾値 `DefaultJaccardThreshold` を SSoT として提供する。
//
// このパッケージは agent-learnings spec / requirements 6.3 で SSoT 化された helper であり、
// 本パッケージを以下の 2 consumer が import する:
//   - v1 `internal/workflow/builtin/distiller` の Apply phase (legacy distiller の dedup safety net)
//   - 新 `internal/workflow/builtin/learnings` の Apply phase (本 spec の category-scoped dedup)
//
// 設計契約:
//   - apply phase が curator operations を書き込む直前の dedup safety net (LLM-as-Judge の
//     見落とし対策) として、per-topic 既存 bullet 行と候補に対し JaccardDedup を実行する。
//   - **CGO 非依存**: `unicode` 標準ライブラリのみを使用し、新規依存追加なし
//     (steering tech.md §Key Technical Decisions の CGO 非依存制約と整合)。
//   - **トークン化規約**: Unicode word break (`unicode.IsLetter` || `unicode.IsDigit`) で
//     単語境界を識別し、それ以外の rune (空白・句読点・記号) は区切りとして扱う。
//     トークンは小文字化したうえで set 化する (大文字小文字を区別しない)。
//   - **不変式**: 結果は必ず閉区間 `[0, 1]`。同一文字列 → 1.0、完全 disjoint → 0.0。
//     空 set 同士は false positive 防御のため 0.0 を返す (空 bullet と空候補を「重複」と
//     誤判定して safety net が apply 全件を skip する事故を防ぐ)。
//   - **対称律**: `JaccardDedup(a, b) == JaccardDedup(b, a)` (集合演算で定義されるため)。
//   - **閾値分離**: 既定閾値 0.8 は `DefaultJaccardThreshold` として export し、`IsSimilar` は
//     呼び出し側が threshold を上書き可能とする (テスト容易性)。閾値変更は spec 改訂を伴うため
//     0.8 値の固定はテストで担保する。
//
// 移設履歴: 旧 `internal/workflow/builtin/distiller/dedup.go` の同名 symbols を 1:1 で SSoT 移設。
// アルゴリズムと既定閾値は不変。
package dedup

import (
	"strings"
	"unicode"
)

// DefaultJaccardThreshold は apply phase の dedup safety net で用いる既定閾値。
// jaccard >= 0.8 を「同義」と判定する spec 契約に従う
// (memory-distiller §distiller.JaccardDedup / agent-learnings Req 6.3)。
const DefaultJaccardThreshold = 0.8

// tokenize は s を Unicode word break (letter / digit 連続) でトークン分割し、
// 小文字化して set として返す。空白・句読点・記号は区切りとして無視される。
//
// この関数は package private — JaccardDedup の純粋関数性を保つため、外部からは直接
// 呼び出させない (token 化規約は JaccardDedup / IsSimilar の入力契約に閉じる)。
func tokenize(s string) map[string]struct{} {
	set := make(map[string]struct{})
	var b strings.Builder
	flush := func() {
		if b.Len() == 0 {
			return
		}
		set[b.String()] = struct{}{}
		b.Reset()
	}
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(unicode.ToLower(r))
			continue
		}
		flush()
	}
	flush()
	return set
}

// JaccardDedup は a / b のトークン jaccard 類似度 [0,1] を返す。
//
// アルゴリズム:
//  1. 各文字列を Unicode word break (letter / digit) でトークン分割し、小文字化して set 化。
//  2. `|A ∩ B| / |A ∪ B|` を返す。
//  3. 空 set 同士は false positive 防御のため 0.0 を返す (Invariant)。
//
// 不変式:
//   - 同一文字列 → 1.0
//   - 完全 disjoint → 0.0
//   - 結果は閉区間 [0, 1]
//   - JaccardDedup(a, b) == JaccardDedup(b, a) (対称律)
func JaccardDedup(a, b string) float64 {
	sa := tokenize(a)
	sb := tokenize(b)

	// 空 set 同士は 0 を返す (false positive 防御)。
	if len(sa) == 0 && len(sb) == 0 {
		return 0.0
	}

	intersection := 0
	// 走査回数を抑えるため、要素数の少ない set を外側に置く。
	if len(sa) > len(sb) {
		sa, sb = sb, sa
	}
	for token := range sa {
		if _, ok := sb[token]; ok {
			intersection++
		}
	}

	union := len(sa) + len(sb) - intersection
	if union == 0 {
		// 片方が空 set かつもう片方も空 set でないと到達しないが、
		// ゼロ除算防御として明示的に 0 を返す。
		return 0.0
	}
	return float64(intersection) / float64(union)
}

// IsSimilar は JaccardDedup(a, b) >= threshold を返す。
// threshold ちょうど (= 比較) でも true となる契約 (apply phase の dedup safety net 仕様)。
//
// 既定閾値を使う場合は `IsSimilar(a, b, DefaultJaccardThreshold)` と呼び出す。
func IsSimilar(a, b string, threshold float64) bool {
	return JaccardDedup(a, b) >= threshold
}

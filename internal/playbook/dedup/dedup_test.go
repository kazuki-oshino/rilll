// Package dedup のテスト本ファイルは pure-Go token-jaccard 類似度関数
// (`JaccardDedup` / `IsSimilar`) と既定閾値 `DefaultJaccardThreshold` の契約を検証する
// (agent-learnings tasks.md 2 / Req 6.3、移設元: memory-distiller §distiller.JaccardDedup / Req 8.2)。
//
// 検証観点:
//   - 同一文字列の jaccard = 1.0 (Invariant)
//   - 完全 disjoint の jaccard = 0.0 (Invariant)
//   - 空 set 同士は 0.0 (false positive 防御 — apply phase の dedup safety net で
//     空 bullet と空候補を「重複」と誤判定しないため)
//   - 大文字小文字を区別しない (小文字化トークン化)
//   - 句読点・空白・記号は word break として扱う (`unicode.IsLetter` || `unicode.IsDigit`)
//   - 重複トークンは set 化により 1 回として数える
//   - 数字も独立トークンとして扱う
//   - 0.8 閾値前後の境界値 (= 0.8 / < 0.8 / "akashic reader fail" vs "akashic reader failure"
//     等の現実的なエラー文言ペア)
//   - `IsSimilar` は jaccard >= threshold を返す (= で true、< で false)
//   - `DefaultJaccardThreshold` の値が 0.8 で固定されている (apply phase の dedup safety net
//     のしきい値契約)
package dedup

import (
	"math"
	"testing"
)

// floatEqual は jaccard の浮動小数点比較で許容する微小誤差。
// 4/5 = 0.8 のような有限小数だけを期待値にしているため eps は十分小さい値で良い。
const jaccardEps = 1e-9

// almostEqual は got と want の差が eps 以下なら true を返す。
func almostEqual(got, want float64) bool {
	return math.Abs(got-want) <= jaccardEps
}

// TestJaccardDedup は JaccardDedup の表ベース契約テスト。
// 同一・完全 disjoint・空・大小文字・句読点・重複・数字・境界値を網羅する。
func TestJaccardDedup(t *testing.T) {
	cases := []struct {
		name string
		a    string
		b    string
		want float64
	}{
		{
			name: "identical string yields 1.0 (Invariant)",
			a:    "akashic reader",
			b:    "akashic reader",
			want: 1.0,
		},
		{
			name: "completely disjoint yields 0.0 (Invariant)",
			a:    "alpha beta",
			b:    "gamma delta",
			want: 0.0,
		},
		{
			name: "both empty yields 0.0 (false positive defense)",
			a:    "",
			b:    "",
			want: 0.0,
		},
		{
			name: "empty vs non-empty yields 0.0",
			a:    "",
			b:    "akashic reader",
			want: 0.0,
		},
		{
			name: "case insensitive: AKASHIC == akashic",
			a:    "AKASHIC Reader",
			b:    "akashic reader",
			want: 1.0,
		},
		{
			name: "punctuation treated as word break",
			a:    "akashic, reader.",
			b:    "akashic reader",
			want: 1.0,
		},
		{
			name: "duplicate tokens collapse via set semantics",
			a:    "fail fail fail",
			b:    "fail",
			want: 1.0,
		},
		{
			name: "digits are independent tokens",
			a:    "step 1 fail",
			b:    "step 1 failure",
			// A={step,1,fail}, B={step,1,failure}, inter={step,1}=2, union=4 → 0.5
			want: 0.5,
		},
		{
			name: "akashic reader fail vs failure (typical recurring error pair) is below 0.8",
			a:    "akashic reader fail",
			b:    "akashic reader failure",
			// A={akashic,reader,fail}, B={akashic,reader,failure},
			// inter={akashic,reader}=2, union={akashic,reader,fail,failure}=4 → 0.5
			want: 0.5,
		},
		{
			name: "exact 0.8 boundary: 4 common / 5 union",
			a:    "alpha beta gamma delta",
			b:    "alpha beta gamma delta epsilon",
			// inter=4, union=5 → 0.8
			want: 0.8,
		},
		{
			name: "just below 0.8 boundary: 3 common / 4 union (0.75)",
			a:    "alpha beta gamma",
			b:    "alpha beta gamma delta",
			want: 0.75,
		},
		{
			name: "result clamped within [0,1]: half overlap",
			a:    "a b",
			b:    "a c",
			// inter=1, union=3 → 1/3
			want: 1.0 / 3.0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := JaccardDedup(tc.a, tc.b)
			if !almostEqual(got, tc.want) {
				t.Errorf("JaccardDedup(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
			if got < 0.0 || got > 1.0 {
				t.Errorf("JaccardDedup(%q, %q) = %v, must be within [0,1]", tc.a, tc.b, got)
			}
		})
	}
}

// TestJaccardDedupSymmetric は JaccardDedup(a,b) == JaccardDedup(b,a) を検証する。
// jaccard は集合演算で定義されるため対称律を保つ契約 (apply phase の dedup safety net で
// 既存 bullet と候補のどちら順序で渡しても同じ判定になることを保証)。
func TestJaccardDedupSymmetric(t *testing.T) {
	a := "akashic reader fail"
	b := "akashic reader failure"
	if JaccardDedup(a, b) != JaccardDedup(b, a) {
		t.Errorf("JaccardDedup symmetry violated: %v vs %v",
			JaccardDedup(a, b), JaccardDedup(b, a))
	}
}

// TestIsSimilar は IsSimilar(a,b,t) が jaccard >= threshold を返す境界契約を検証する。
//   - jaccard が threshold ちょうど → true (>= 比較)
//   - jaccard が threshold 未満 → false
//   - 任意の threshold に対して動作する (test 容易性のため可変引数)
func TestIsSimilar(t *testing.T) {
	cases := []struct {
		name      string
		a         string
		b         string
		threshold float64
		want      bool
	}{
		{
			name:      "identical at default threshold (1.0 >= 0.8)",
			a:         "akashic reader",
			b:         "akashic reader",
			threshold: DefaultJaccardThreshold,
			want:      true,
		},
		{
			name:      "exact 0.8 boundary at default threshold (>= comparison)",
			a:         "alpha beta gamma delta",
			b:         "alpha beta gamma delta epsilon",
			threshold: DefaultJaccardThreshold,
			want:      true,
		},
		{
			name:      "just below 0.8 (0.75) at default threshold is not similar",
			a:         "alpha beta gamma",
			b:         "alpha beta gamma delta",
			threshold: DefaultJaccardThreshold,
			want:      false,
		},
		{
			name:      "akashic reader fail vs failure (0.5) is not similar at default threshold",
			a:         "akashic reader fail",
			b:         "akashic reader failure",
			threshold: DefaultJaccardThreshold,
			want:      false,
		},
		{
			name:      "low threshold accepts moderate overlap",
			a:         "akashic reader fail",
			b:         "akashic reader failure",
			threshold: 0.5,
			want:      true,
		},
		{
			name:      "completely disjoint never similar",
			a:         "alpha beta",
			b:         "gamma delta",
			threshold: 0.0,
			// jaccard=0.0, threshold=0.0 → 0.0 >= 0.0 = true
			// 0.0 threshold は実運用では意味を持たないが、>= 比較の契約は保つ。
			want: true,
		},
		{
			name:      "both empty (jaccard 0.0) not similar at default threshold",
			a:         "",
			b:         "",
			threshold: DefaultJaccardThreshold,
			want:      false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := IsSimilar(tc.a, tc.b, tc.threshold)
			if got != tc.want {
				t.Errorf("IsSimilar(%q, %q, %v) = %v, want %v",
					tc.a, tc.b, tc.threshold, got, tc.want)
			}
		})
	}
}

// TestDefaultJaccardThreshold は既定閾値が 0.8 で固定されていることを検証する
// (memory-distiller §distiller.JaccardDedup の "≥ 0.8 で同義判定" 契約 / agent-learnings Req 6.3)。
// この値は apply phase の dedup safety net および curator skill の判断指針と整合させているため、
// 値変更は spec 改訂を伴う。
func TestDefaultJaccardThreshold(t *testing.T) {
	if DefaultJaccardThreshold != 0.8 {
		t.Errorf("DefaultJaccardThreshold = %v, want 0.8", DefaultJaccardThreshold)
	}
}

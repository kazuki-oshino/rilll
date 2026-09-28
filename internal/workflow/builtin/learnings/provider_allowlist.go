package learnings

import (
	"os"
	"strings"

	"github.com/kazuki-oshino/rilll/internal/provider"
	"github.com/kazuki-oshino/rilll/internal/workflowctx"
)

// envRillLearningsProviders は Reflector phase の provider allowlist を上書きする
// 環境変数名 (CSV)。例: `RILLL_LEARNINGS_PROVIDERS=claude,codex` で 2 provider を許可。
const envRillLearningsProviders = "RILLL_LEARNINGS_PROVIDERS"

// defaultProviderAllowlist は env 未設定時の provider allowlist (CSV)。
//
// 初期値は spike-results.md `### RILLL_LEARNINGS_PROVIDERS allowlist default` の合意値で
// `codex` だったが、ユーザー指示 (2026-05-08) で `claude` に変更。その後、ユーザー指示
// (2026-05-17) で `claude,codex` に変更:
//   - 本リポジトリの主要 parent loop (kiro-spec-loop / kiro-impl-loop) は claude/codex provider
//     の両方で動くため、どちらか片方だけにすると Reflector が全セッションを skip して
//     学びが反映されない run が発生する (実観測: claude-only skip は
//     run 2026-05-08T06-23-24-...、codex-only skip は run 2026-05-17T08-55-37-...)。
//   - claude が spike harness で FAIL した原因は Reflector 本実装に取り込む前の
//     `StructuredOutput` seam 不在であり、本実装 (`jsonextract.go:42-49`) では既に解決済み。
//   - `cursor` の追加には phase 0 spike の再実行と pass 確認が依然必要。
const defaultProviderAllowlist = "claude,codex"

// SkipReasonProviderNotAllowed は allowlist 外 provider の session / job を skip する
// 経路で使う理由値。legacy Reflector artifact と early reflection job の deny 理由を揃える。
const SkipReasonProviderNotAllowed = "provider-not-in-allowlist"

// resolveProviderAllowlist は env CSV を読み、空白除去 + 重複除去した provider ID
// allowlist snapshot を返す。env 未設定 / 空文字 / 全要素 trim 後空 のときは defaultCSV
// を同じ CSV parser で解釈する。
//
// 設計判断: 結果には空文字を含めない。空文字 provider は allowlist 比較の観点で意味がない。
func resolveProviderAllowlist(envName, defaultCSV string) []provider.ProviderID {
	raw := strings.TrimSpace(os.Getenv(envName))
	if raw == "" {
		raw = defaultCSV
	}
	out := parseProviderAllowlistCSV(raw)
	if len(out) == 0 && raw != defaultCSV {
		out = parseProviderAllowlistCSV(defaultCSV)
	}
	return out
}

func parseProviderAllowlistCSV(csv string) []provider.ProviderID {
	parts := strings.Split(csv, ",")
	seen := make(map[provider.ProviderID]struct{}, len(parts))
	out := make([]provider.ProviderID, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		id := provider.ProviderID(p)
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

func newEarlyReflectionPolicySnapshot(notify func()) workflowctx.EarlyReflectionPolicy {
	return workflowctx.EarlyReflectionPolicy{
		AllowedProviders: resolveProviderAllowlist(envRillLearningsProviders, defaultProviderAllowlist),
		Notify:           notify,
	}
}

func providerAllowlistStrings(ids []provider.ProviderID) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = string(id)
	}
	return out
}

func providerAllowlistStringSet(ids []provider.ProviderID) map[string]struct{} {
	out := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		out[string(id)] = struct{}{}
	}
	return out
}

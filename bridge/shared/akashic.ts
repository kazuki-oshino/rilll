// 各 bridge (claude-agent-sdk / codex-sdk / cursor-sdk) で共通の
// Akashic レベル判定ロジック。source ごとの type/subtype フィルタは
// テーブル形式 (FilterRules) で各 bridge から注入する。
//
// debug: 全イベント素通し / full: source ごとの fullExclude で間引き /
// summary: source ごとの summary 条件に合致したイベントのみ通す。
// 既知でない source は summary では落とし、full では素通しする。

export type AkashicLevel = 'summary' | 'full' | 'debug';

const allowedLevels = new Set<AkashicLevel>(['summary', 'full', 'debug']);

export function normalizeAkashicLevel(level: string | undefined): AkashicLevel {
  if (level && allowedLevels.has(level as AkashicLevel)) {
    return level as AkashicLevel;
  }
  return 'summary';
}

export type FilterRules = {
  // summary レベルで「残す」条件 (true → 通す)。未指定なら常に false。
  summary?: (type: string, subtype?: string) => boolean;
  // full レベルで「除外する」条件 (true → 落とす)。未指定なら常に通す。
  fullExclude?: (type: string, subtype?: string) => boolean;
};

export type ObservationFilter = (
  level: AkashicLevel,
  source: string,
  type: string,
  subtype?: string,
) => boolean;

export function createObservationFilter(
  rules: Record<string, FilterRules>,
): ObservationFilter {
  return (level, source, type, subtype) => {
    if (level === 'debug') return true;
    const rule = rules[source];
    if (level === 'full') {
      if (!rule) return true;
      return !rule.fullExclude?.(type, subtype);
    }
    if (!rule) return false;
    return rule.summary?.(type, subtype) ?? false;
  };
}

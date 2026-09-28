import {
  createObservationFilter,
  normalizeAkashicLevel,
  type AkashicLevel,
} from '@rilll/bridge-protocol/akashic';

// claude bridge の `claude-hook` source と対称な closed enum。
// akashic-investigator が `WHERE source IN ('claude-hook','cursor-hook') AND
// observation_subtype = 'PostToolUseFailure'` のような provider 横断述語を書けるよう、
// subtype 語彙を 3 値に固定する (PreToolUse / PostToolUse / PostToolUseFailure)。
const summaryHookSubtypes = new Set([
  'PreToolUse',
  'PostToolUse',
  'PostToolUseFailure',
]);

const summaryControlTypes = new Set([
  'resume_fallback',
  'context_usage',
  'context_usage_error',
]);

const filter = createObservationFilter({
  'cursor-sdk': {
    // summary: 起点/終端/メトリクスだけ残す
    summary: (type) => type === 'system' || type === 'status' || type === 'usage' || type === 'error',
    // full: thinking と user は full でも騒がしいだけなので落とす
    fullExclude: (type) => type === 'thinking' || type === 'user',
  },
  'cursor-hook': {
    // summary: tool 呼び出しの Pre/Post/PostFailure フックは監査価値が高いので常に残す
    // (claude-hook と同形)。closed enum 外の subtype は summary で落とす。
    summary: (type, subtype) =>
      type === 'hook' && subtype !== undefined && summaryHookSubtypes.has(subtype),
  },
  'cursor-control': {
    // summary: run 制御プレーンの重要 event は後段の akashic 解析で常に参照できるよう残す。
    summary: (type) => summaryControlTypes.has(type),
  },
});

export const shouldWriteObservation = filter;
export { normalizeAkashicLevel };
export type { AkashicLevel };

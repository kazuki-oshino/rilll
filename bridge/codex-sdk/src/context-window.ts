// 既知 Codex モデルの context window サイズ (トークン単位)。
// Codex SDK は thread.started などで model 情報を返さないため、bridge は
// request.model / 環境変数 / 既定値の優先順で modelId を解決し、
// その値で table lookup する。一致しないモデルでは context_usage_error を emit する。
//
// 値の出典:
// - gpt-5 系: 400_000 (input context). 公開された OpenAI モデル仕様の
//   上限を採用 (output 上限とは別軸)。
// - gpt-5-codex: 272_000 (Codex 専用モデルの公称 context window)。
// 仕様アップデートの際は本テーブルを 1 箇所変更すれば計算式は影響を受けない。
export const CONTEXT_SIZE_TABLE: Readonly<Record<string, number>> = {
  'gpt-5': 400000,
  'gpt-5-mini': 400000,
  'gpt-5-nano': 400000,
  'gpt-5-codex': 272000,
};

export interface ResolvedContextWindow {
  readonly contextWindow: number;
}

export function resolveContextWindow(
  modelId: string,
  contextSizeTable: Readonly<Record<string, number>> = CONTEXT_SIZE_TABLE,
): ResolvedContextWindow | undefined {
  const contextWindow = contextSizeTable[modelId];
  if (contextWindow === undefined) return undefined;
  return { contextWindow };
}

export interface ComputeContextUsageInput {
  readonly modelId: string;
  readonly contextWindow: number;
  readonly cumulativeInputTokens: number;
}

export interface ContextUsagePayload {
  readonly model_id: string;
  readonly context_window: number;
  readonly cumulative_input_tokens: number;
  readonly usage_percent: number;
}

export function computeContextUsage(input: ComputeContextUsageInput): ContextUsagePayload {
  const usagePercent = clampPercent((input.cumulativeInputTokens / input.contextWindow) * 100);
  return {
    model_id: input.modelId,
    context_window: input.contextWindow,
    cumulative_input_tokens: input.cumulativeInputTokens,
    usage_percent: usagePercent,
  };
}

function clampPercent(value: number): number {
  if (!Number.isFinite(value)) return 0;
  return Math.min(100, Math.max(0, value));
}

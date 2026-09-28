import type { ModelListItem } from '@cursor/sdk';

export type ContextWindowResolution = 'sdk_parameters' | 'fallback_table';

export interface UsageTotals {
  readonly inputTokens: number;
  readonly outputTokens: number;
  readonly cacheReadTokens: number;
  readonly cacheWriteTokens: number;
}

export interface ContextUsagePayload {
  readonly model_id: string;
  readonly context_window: number;
  readonly used_tokens: number;
  readonly usage_percent: number;
  readonly resolution: ContextWindowResolution;
}

export interface ResolvedContextWindow {
  readonly contextWindow: number;
  readonly resolution: ContextWindowResolution;
}

export const CONTEXT_SIZE_TABLE: Readonly<Record<string, number>> = {};

export function parseContextString(value: string): number | undefined {
  const match = value.trim().toLowerCase().match(/^(\d+(?:\.\d+)?)([km])$/);
  if (!match) return undefined;

  const amount = Number(match[1]);
  const multiplier = match[2] === 'k' ? 1000 : 1000000;
  return Math.round(amount * multiplier);
}

export function resolveContextWindow(
  modelId: string,
  modelsList: readonly ModelListItem[],
  contextSizeTable: Readonly<Record<string, number>> = CONTEXT_SIZE_TABLE,
): ResolvedContextWindow | undefined {
  const model = modelsList.find((item) => item.id === modelId);
  const sdkContextWindow = resolveSdkContextWindow(model);
  if (sdkContextWindow !== undefined) {
    return {
      contextWindow: sdkContextWindow,
      resolution: 'sdk_parameters',
    };
  }

  const fallbackContextWindow = contextSizeTable[modelId];
  if (fallbackContextWindow !== undefined) {
    return {
      contextWindow: fallbackContextWindow,
      resolution: 'fallback_table',
    };
  }

  return undefined;
}

export function computeContextUsage(input: {
  readonly modelId: string;
  readonly contextWindow: number;
  readonly resolution: ContextWindowResolution;
  readonly usageTotals: UsageTotals;
}): ContextUsagePayload {
  const usedTokens =
    input.usageTotals.inputTokens
    + input.usageTotals.cacheReadTokens
    + input.usageTotals.cacheWriteTokens;
  const usagePercent = clampPercent((usedTokens / input.contextWindow) * 100);

  return {
    model_id: input.modelId,
    context_window: input.contextWindow,
    used_tokens: usedTokens,
    usage_percent: usagePercent,
    resolution: input.resolution,
  };
}

function resolveSdkContextWindow(model: ModelListItem | undefined): number | undefined {
  const contextParameter = model?.parameters?.find((parameter) => parameter.id === 'context');
  if (!contextParameter) return undefined;

  const defaultVariant = model?.variants?.find((variant) => variant.isDefault) ?? model?.variants?.[0];
  const contextValue = defaultVariant?.params.find((param) => param.id === contextParameter.id)?.value;
  if (!contextValue) return undefined;

  return parseContextString(contextValue);
}

function clampPercent(value: number): number {
  return Math.min(100, Math.max(0, value));
}

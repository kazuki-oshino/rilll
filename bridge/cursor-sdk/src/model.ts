import type {
  Cursor as CursorType,
  ModelListItem,
  ModelSelection,
} from '@cursor/sdk';

export interface ThinkingValue {
  value: string;
  displayName?: string;
}

const FALLBACK_REASONING_PARAM_ID = 'reasoning';
const REASONING_PARAM_IDS = [FALLBACK_REASONING_PARAM_ID, 'thinking'];

/**
 * モデル parameter の許容値リストから、ユーザーが希望する thinking レベルに最も近い値を選ぶ。
 * 完全一致 → 大文字小文字無視 → "extra" を含む displayName/value → "high" → 先頭要素 の順で探索する。
 * リストが空のときは preferred をそのまま返す（モデル側が parameters を返さないケース対応）。
 */
export function pickThinkingValue(
  values: ThinkingValue[],
  preferred: string,
): string {
  if (values.length === 0) return preferred;

  const exact = values.find((v) => v.value === preferred);
  if (exact) return exact.value;

  const ci = values.find((v) => v.value.toLowerCase() === preferred.toLowerCase());
  if (ci) return ci.value;

  // "extra-high" 系のリクエストは "extra" / "xhigh" / "extraHigh" 等の表記揺れに
  // 寛容にマッチする。preferred 自身が "xhigh" (rilll の preset 命名) のときも、
  // values 側の "extra-high" / "extraHigh" / displayName "Extra High" を拾えるよう
  // 判定条件に "xhigh" を含める (このガードがないと preferred="xhigh" のとき
  // 後段の "high" フォールバックに倒れ、Effort が実質 high に降格する)。
  if (preferred.toLowerCase().includes('extra') || preferred.toLowerCase().includes('xhigh')) {
    const extra = values.find(
      (v) =>
        v.value.toLowerCase().includes('extra') ||
        v.value.toLowerCase().includes('xhigh') ||
        (v.displayName?.toLowerCase().includes('extra') ?? false),
    );
    if (extra) return extra.value;
  }

  const high = values.find((v) => v.value.toLowerCase() === 'high');
  if (high) return high.value;

  return values[0].value;
}

export interface ResolvePreferredModelOptionsInput {
  requestedModelId?: string;
  defaultModelId: string;
  defaultThinking: string;
}

export interface ResolvedPreferredModelOptions {
  preferredModelId: string;
  preferredThinking?: string;
}

export type ModelParam = NonNullable<ModelSelection['params']>[number];

export function resolvePreferredModelOptions(
  options: ResolvePreferredModelOptionsInput,
): ResolvedPreferredModelOptions {
  const requested = options.requestedModelId?.trim();
  if (requested) {
    return { preferredModelId: requested, preferredThinking: undefined };
  }
  return {
    preferredModelId: options.defaultModelId,
    preferredThinking: options.defaultThinking,
  };
}

export interface ResolveModelOptions {
  preferredModelId: string;
  preferredThinking?: string;
  /** Cursor SDK に渡す明示的なモデル parameter。指定時は variants / reasoning 解決を行わない */
  requestedParams?: ModelParam[];
  apiKey?: string;
  /** Cursor.models.list の差し替え（テスト用）。指定しなければ実 SDK を呼ぶ */
  listModels?: typeof CursorType.models.list;
  /** 失敗時の警告通知（テスト用）。デフォルトは stderr へ書き出す */
  warn?: (message: string) => void;
}

export interface ResolvedModelSelection {
  model: ModelSelection;
  modelsList: readonly ModelListItem[];
}

/**
 * requestedParams がある場合はそれをそのまま使い、それ以外では preferredThinking に応じて
 * Cursor.models.list() から reasoning 値を解決した ModelSelection と、呼び出し側で再利用できる
 * modelsList を返す。
 * preferredThinking がない場合は model.variants の isDefault な variant の params を使う。
 * isDefault が見つからない場合は params なしで model ID のみを返す。
 */
export async function resolveModelSelection(
  options: ResolveModelOptions,
): Promise<ResolvedModelSelection> {
  const {
    preferredModelId,
    preferredThinking,
    requestedParams,
    apiKey,
    listModels,
    warn,
  } = options;
  const notify = warn ?? ((msg: string) => process.stderr.write(`${msg}\n`));
  const explicitParams = requestedParams && requestedParams.length > 0
    ? { id: preferredModelId, params: requestedParams }
    : undefined;

  let models: ModelListItem[];
  try {
    if (!listModels) {
      // 動的 import は呼び出し側で済ませている前提（index.ts から差し替え可能にしている）
      // ここに到達したら preferred を素通しする
      return {
        model: explicitParams ?? (
          preferredThinking
            ? {
                id: preferredModelId,
                params: [{ id: FALLBACK_REASONING_PARAM_ID, value: preferredThinking }],
              }
            : { id: preferredModelId }
        ),
        modelsList: [],
      };
    }
    models = await listModels({ apiKey });
  } catch (err) {
    if (preferredThinking) {
      notify(
        `[cursor-sdk] Cursor.models.list の呼び出しに失敗したため、reasoning="${preferredThinking}" をそのまま指定します: ${
          err instanceof Error ? err.message : String(err)
        }`,
      );
    }
    return {
      model: explicitParams ?? (
        preferredThinking
          ? {
              id: preferredModelId,
              params: [{ id: FALLBACK_REASONING_PARAM_ID, value: preferredThinking }],
            }
          : { id: preferredModelId }
      ),
      modelsList: [],
    };
  }

  if (explicitParams) {
    return { model: explicitParams, modelsList: models };
  }

  if (!preferredThinking) {
    const target = models.find((m) => m.id === preferredModelId);
    const defaultVariant = target?.variants?.find((v) => v.isDefault);
    if (defaultVariant) {
      return { model: { id: preferredModelId, params: defaultVariant.params }, modelsList: models };
    }
    return { model: { id: preferredModelId }, modelsList: models };
  }

  const target = models.find((m) => m.id === preferredModelId);
  if (!target?.parameters) {
    notify(
      `[cursor-sdk] モデル "${preferredModelId}" の parameters を解決できなかったため、reasoning="${preferredThinking}" をそのまま指定します`,
    );
    return {
      model: {
        id: preferredModelId,
        params: [{ id: FALLBACK_REASONING_PARAM_ID, value: preferredThinking }],
      },
      modelsList: models,
    };
  }

  const reasoningDef = target.parameters.find((p) => REASONING_PARAM_IDS.includes(p.id));
  if (!reasoningDef || reasoningDef.values.length === 0) {
    return { model: { id: preferredModelId }, modelsList: models };
  }

  const value = pickThinkingValue(reasoningDef.values, preferredThinking);
  if (value !== preferredThinking) {
    notify(
      `[cursor-sdk] reasoning="${preferredThinking}" は "${preferredModelId}" の許容値に存在しなかったため、近い値 "${value}" にフォールバックしました`,
    );
  }
  return {
    model: {
      id: preferredModelId,
      params: [{ id: reasoningDef.id, value }],
    },
    modelsList: models,
  };
}

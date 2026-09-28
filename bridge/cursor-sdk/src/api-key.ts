export type ApiKeySource = 'RILLL_CURSOR_API_KEY' | 'CURSOR_API_KEY' | 'unspecified';

export interface ResolvedApiKey {
  readonly apiKey?: string;
  readonly source: ApiKeySource;
}

export interface SystemInitApiKeyFields {
  readonly apiKeySource: ApiKeySource;
}

export interface ResolveCursorApiKeyInput {
  readonly env: Readonly<Record<string, string | undefined>>;
}

function nonEmpty(value: string | undefined): value is string {
  return value !== undefined && value !== '';
}

export function resolveCursorApiKey(input: ResolveCursorApiKeyInput): ResolvedApiKey {
  const rillApiKey = input.env.RILLL_CURSOR_API_KEY;
  if (nonEmpty(rillApiKey)) {
    return { apiKey: rillApiKey, source: 'RILLL_CURSOR_API_KEY' };
  }

  const cursorApiKey = input.env.CURSOR_API_KEY;
  if (nonEmpty(cursorApiKey)) {
    return { apiKey: cursorApiKey, source: 'CURSOR_API_KEY' };
  }

  return { apiKey: undefined, source: 'unspecified' };
}

export function buildSystemInitApiKeyFields(resolved: ResolvedApiKey): SystemInitApiKeyFields {
  return { apiKeySource: resolved.source };
}

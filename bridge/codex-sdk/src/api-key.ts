export type ApiKeySource = 'RILLL_CODEX_API_KEY' | 'OPENAI_API_KEY' | 'unspecified';

export interface ResolvedApiKey {
  readonly apiKey?: string;
  readonly source: ApiKeySource;
}

export interface SystemInitApiKeyFields {
  readonly apiKeySource: ApiKeySource;
}

export interface ResolveCodexApiKeyInput {
  readonly env: Readonly<Record<string, string | undefined>>;
}

function nonEmpty(value: string | undefined): value is string {
  return value !== undefined && value !== '';
}

export function resolveCodexApiKey(input: ResolveCodexApiKeyInput): ResolvedApiKey {
  const rillApiKey = input.env.RILLL_CODEX_API_KEY;
  if (nonEmpty(rillApiKey)) {
    return { apiKey: rillApiKey, source: 'RILLL_CODEX_API_KEY' };
  }

  const openaiApiKey = input.env.OPENAI_API_KEY;
  if (nonEmpty(openaiApiKey)) {
    return { apiKey: openaiApiKey, source: 'OPENAI_API_KEY' };
  }

  return { apiKey: undefined, source: 'unspecified' };
}

export function buildSystemInitApiKeyFields(resolved: ResolvedApiKey): SystemInitApiKeyFields {
  return { apiKeySource: resolved.source };
}

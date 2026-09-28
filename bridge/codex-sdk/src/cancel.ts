import type { BridgeEvent } from './protocol.js';

/**
 * memory-distiller-trigger-policy task 2.5: cancel 起源を含む result event を構築する純関数。
 *
 * Codex bridge の cancel handler が保持した `cancelRequestSchema.origin`
 * ('user' | 'engine') の値を、cancel 後に書き出す `result` event payload にそのまま透過させる。
 * これにより Reflector / Curator / 後段 (`internal/agent/recordAgentCallFinish`) が
 * 「user 起源の cancel」を構造的に観測できるようにする (Req 3.2)。
 *
 * Claude bridge の `buildCancelResultEvent` と同形 (両 provider 同一契約):
 * - origin が 'user' / 'engine' のときはそのまま設定する
 * - origin が undefined のときは default 'engine' に解決する
 *   (`cancelRequestSchema.origin` の zod default と整合 / Req 6 後方互換)
 *
 * 純関数として切り出すことで、`index.ts` のモジュール状態（cancelOrigin 等）を伴う
 * cancel handler の制御フローを変更せずに、テストから contract を直接 verify できる。
 */
export function buildCancelResultEvent(args: {
  id: string;
  origin: 'user' | 'engine' | undefined;
  result: string;
  sessionId: string | undefined;
  structuredOutput: unknown;
}): BridgeEvent {
  return {
    type: 'result',
    id: args.id,
    result: args.result,
    sessionId: args.sessionId,
    structuredOutput: args.structuredOutput,
    origin: args.origin ?? 'engine',
  };
}

import {
  createObservationFilter,
  normalizeAkashicLevel,
  type AkashicLevel,
} from '@rilll/bridge-protocol/akashic';

// Issue #64: 制御プレーン観測 (resume_fallback / context_usage / context_usage_error)
// は summary でも常に保持して、後段 akashic 解析で復旧経路 / 推定 context 使用率を
// 追跡できるようにする。cursor 側は別 source (cursor-control) を切る設計だが、
// codex 側は同 source を継続し、type 単位の closed enum で識別する (Issue 仕様)。
//
// Issue #65 B2/B3/B4: bridge 内 deny gate / allowedTools 違反 / structured output retry も
// 監査価値が高い (誰がどの操作を止めたか / 自動回復の発動回数) ので summary でも常に残す。
const summaryControlTypes = new Set([
  'resume_fallback',
  'context_usage',
  'context_usage_error',
  'permission_deny',
  'allowed_tools_violation',
  'structured_output_retry',
  // Issue #65 N3: bridge が UI escalation 経路を持たない都合で非 'never' の approvalPolicy が
  // 渡されたときの hang warning は監査価値が高いため summary でも常に残す。
  'approval_policy_warning',
]);

const filter = createObservationFilter({
  'codex-sdk': {
    // summary: ライフサイクル境界とメトリクス/エラー + 制御プレーン観測のみ残す
    summary: (type) =>
      type === 'usage'
      || type === 'thread.started'
      || type === 'turn.completed'
      || type === 'turn.failed'
      || type === 'error'
      || summaryControlTypes.has(type),
    // full: 高頻度の中間更新は落とす
    fullExclude: (type) => type === 'item.updated' || type === 'turn.started',
  },
  'codex-hook': {
    // Issue #65 B1: user 定義 PreToolUse hook の block と spawn 失敗は監査価値が高いため
    // summary でも常に残す (Claude bridge claude-hook と同形)。
    summary: (type) => type === 'hook_block' || type === 'hook_spawn_error',
  },
});

export const shouldWriteObservation = filter;
export { normalizeAkashicLevel };
export type { AkashicLevel };

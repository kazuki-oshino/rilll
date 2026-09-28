// issue #66 A7: cwd 外 read 許可 (additionalDirectories) を Codex / Claude の両 bridge で
// 共通解釈するためのユーティリティ。bridge ごとに二重実装すると drift で挙動が乖離する
// (deny-patterns と同じ理由) ため、shared に集約する。
//
// 解釈ルール:
//  - undefined のとき: bridge 既定値 `['/']` を返し「デフォルト許可」(= cwd 外 read を広めに許可) する。
//  - 空配列のとき: 制限意図とみなして空配列をそのまま返す (SDK にも空配列が渡る)。
//  - 非空配列のとき: そのまま (浅いコピーで) 返す。
//
// 既定を `['/']` に倒す理由:
//  Codex 側で sandbox 既定が danger-full-access (write 許可) に倒されている (issue #65 A3) ため、
//  cwd 外 read だけ厳しく塞ぐ非対称が運用上の不便 (workflow 内で `~/.config` 等の参照ができない) になる。
//  明示的に空配列を渡せば制限できる seam を残してある (per-call 厳格化の余地)。

export function resolveAdditionalDirectories(
  requested: readonly string[] | undefined,
): string[] {
  if (requested === undefined) return ['/'];
  return [...requested];
}

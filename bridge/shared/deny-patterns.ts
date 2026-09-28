// issue #65 B2: 致命的な Bash パターン denylist と Write ターゲット判定を
// claude bridge / codex bridge で完全に共有するための共通モジュール。
//
// ここで deny する条件は「誤検知が極めて起きにくい」「踏まれたら回復不能」の交わりだけに留め、
// `rm -rf /tmp/foo` や `git rm -rf path/to/dir` のような正当な系は素通させる。
// 本ファイルは pure function のみで構成し、副作用 (writeEvent / spawn 等) は持たない。

import path from 'node:path';

export interface DangerousBashPattern {
  re: RegExp;
  reason: string;
}

export const dangerousBashPatterns: readonly DangerousBashPattern[] = [
  // `rm -rf /` のような root 直下削除。/tmp/foo などはセパレータの直後に文字を要求するため
  // 当たらない。`/(?:\s|$|;|&|\|)` で「/ 単独」または「/ + コマンド終端」だけを拾う。
  { re: /\brm\s+-[a-zA-Z]*[rR][a-zA-Z]*[fF][a-zA-Z]*\s+\/(?:\s|$|;|&|\|)/, reason: 'rm -rf / (root 削除)' },
  { re: /\brm\s+-[a-zA-Z]*[fF][a-zA-Z]*[rR][a-zA-Z]*\s+\/(?:\s|$|;|&|\|)/, reason: 'rm -rf / (root 削除)' },
  // `rm -rf ~` / `rm -rf $HOME` (末尾 / でも /. でもよい)
  { re: /\brm\s+-[a-zA-Z]*[rR][a-zA-Z]*[fF]?[a-zA-Z]*\s+(?:~|\$HOME)(?:\s|$|;|&|\||\/\s|\/$)/, reason: 'rm -rf ~/$HOME 全削除' },
  // curl/wget の出力をそのまま shell に流す: `curl ... | sh`
  { re: /\b(?:curl|wget|fetch)\b[^\n|;&]*\|\s*(?:sh|bash|zsh|ksh|sudo)\b/, reason: 'remote pipe to shell' },
  // 機密ファイル領域への直接 redirect 書き込み
  { re: />\s*(?:~|\$HOME)\/\.(?:ssh|aws|gnupg|config\/gh)\//, reason: '~/.ssh / ~/.aws / ~/.gnupg への redirect 書き込み' },
  { re: />\s*\/etc\//, reason: '/etc/ への redirect 書き込み' },
  // chmod 777 系
  { re: /\bchmod\s+(?:-R\s+)?(?:0?777|a\+rwx)\b/, reason: 'chmod 777' },
  // fork bomb (古典)
  { re: /:\s*\(\s*\)\s*\{[^}]*\|.*?\}\s*;\s*:/, reason: 'fork bomb' },
];

// command 文字列を dangerousBashPatterns に通し、最初にヒットしたパターンの reason を返す。
// 空文字 / 何もマッチしないときは undefined を返す。
export function matchDangerousBashCommand(command: string): string | undefined {
  if (!command) return undefined;
  for (const { re, reason } of dangerousBashPatterns) {
    if (re.test(command)) return reason;
  }
  return undefined;
}

// Write / Edit / file_change 等の対象パスが cwd 外なら true を返す。
// 絶対 path は path.resolve でそのまま正規化。相対 path は cwd を base に解決する。
// cwd 自体への書き込みも拒否する (cwd 直書きは意図的でないことが多いため安全側)。
export function isWriteTargetOutsideCwd(filePath: string, cwd: string): boolean {
  const absCwd = path.resolve(cwd);
  const absTarget = path.isAbsolute(filePath) ? path.resolve(filePath) : path.resolve(absCwd, filePath);
  if (absTarget === absCwd) return true;
  const rel = path.relative(absCwd, absTarget);
  return rel === '' || rel === '..' || rel.startsWith('..' + path.sep) || path.isAbsolute(rel);
}

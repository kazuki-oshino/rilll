// Package learnings の本 file は agent-learnings 4 phase pipeline の 3 段目
// (curator) を担う `Curator` 関数と関連型 (`CuratorInput` / `CuratorResult` /
// `CuratorArtifact` 等) を提供する (tasks.md 7.3 / design.md §Curator /
// Req 6.1, 6.2, 6.5, 8.2, 8.4, 9.3)。
//
// 役割:
//
//   - `<runDir>/existing-rules.txt` を構築する: `docs_dev/playbook/<category>.md`
//     (wiki 4 file flat layout) のみを scan して concat する (Req 8.2)。
//   - 256 KiB (`MaxExistingRulesBytes`) の size guard と 2 段 degrade strategy
//     (playbook-only / head-tail clipping) を v1 と同形で実装。
//   - `agent.CallAgent(ctx, "/agent-learnings-curator curator <runDir>", ...)` を
//     1 回発火し、`OutputFormat=JSONSchemaForCurator()` で operations response を
//     JSON 強制する。
//   - LLM 応答を `extractAgentResponseJSON` (本 package 内) で多段 fallback 抽出。
//   - JSON parse / schema 違反 → `Verdict=VerdictRejectedInvalidJSON` を返し、
//     raw response を `<runDir>/curator.json` に保存する (Req 9.3)。
//   - 抽出 operations を `<runDir>/curator.json` (proposed) に書き出す (Req 8.4)。
//
// 設計契約:
//
//   - **prompt リテラル禁止**: skill SSoT (`.agents/skills/agent-learnings-curator/`)
//     の slash command + 引数のみを Go 側で文字列構築する。Curator phase の prompt
//     本体は skill ファイル内に閉じる (Req 6.1 の SSoT 不変式)。
//   - **size guard / degrade strategy**: v1 distiller の `buildExistingRulesArtifact`
//     と同形を本 package 内で再実装する (cross-package 流用を避ける)。256 KiB / 2 段
//     degrade のロジックは v1 と同一値で固定。
//   - **artifact atomic write**: prefetch.go の `writeJSONAtomic` を再利用。
//   - **wiki layout のみ読む** (Req 8.2): `docs_dev/playbook/<category>.md` を直接
//     読み、`ParseCategoryFile` で section 列に分解する。旧 `.claude/rules/playbook/`
//     は scan しない (Req 8.2)。各 file の中身は `===== <rel> =====` separator + 全文
//     concat で skill に渡り、curator が dedup 比較・novelty 判定の context として利用する。
//   - **`runDir` 配下の`existing-rules.txt` 経路**: skill SSoT 側 curator.md は
//     `<runDir>/existing-rules.txt` を Read することで dedup 判定に利用する。
//     本 phase が書き出す存在保証が両端契約。
//
// 関連 Requirement: 6.1 / 6.2 / 6.5 / 8.2 / 8.4 / 9.3 / 9.5。
package learnings

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/kazuki-oshino/rilll/internal/workflow"
	"github.com/kazuki-oshino/rilll/internal/workflowctx"
)

// curatorFilename は Curator phase が `<runDir>/` 直下に書き出す JSON artifact 名
// (Req 8.4)。
const curatorFilename = "curator.json"

// curatorRawFilename は LLM 応答が JSON parse 失敗した場合の raw 保存先。
// `verdict=rejected-invalid-json` 経路で人間 review に使う (Req 9.3)。
const curatorRawFilename = "curator.raw.txt"

// existingRulesFilename は Curator phase 入力 context として concat する artifact のファイル名。
// skill SSoT (`.agents/skills/agent-learnings-curator/rules/curator.md`) が
// `<runDir>/existing-rules.txt` を Read する両端契約。
const existingRulesFilename = "existing-rules.txt"

// MaxExistingRulesBytes は Curator phase 入力 `existing-rules.txt`
// (`docs_dev/playbook/<category>.md` 4 file の concat) の合計サイズ上限。
// v1 distiller と同値 256 KiB。super 過時は 2 段 degrade strategy を適用する。
const MaxExistingRulesBytes = 256 << 10

// maxExistingRulesBytes は MaxExistingRulesBytes の test override 用 var。
// 既定値は MaxExistingRulesBytes と等価。test では小さい値に差し替えて degrade 経路を
// 検証する。
var maxExistingRulesBytes = MaxExistingRulesBytes

// existingRulesTruncationStrategyPlaybookOnly は第 1 段 degrade 戦略名。
const existingRulesTruncationStrategyPlaybookOnly = "playbook-only"

// existingRulesTruncationStrategyHeadTailClip は第 2 段 degrade 戦略名。
const existingRulesTruncationStrategyHeadTailClip = "playbook-only+head-tail-clip"

// curatorSkillSlash は Curator phase 起動用の slash command (skill SSoT 引数 1)。
const curatorSkillSlash = "/agent-learnings-curator"

// curatorMode は skill SSoT の argument-hint 1 段目の値 (`<mode>`)。
const curatorMode = "curator"

// stepIDCurator は Curator phase が `WithStep` で記録する agent_calls.step_id 値
// (Req 2.2 の closed set 4 値の 1 つ)。
const stepIDCurator = "curator"

// stepNameCurator は同 step の人間可読名。
const stepNameCurator = "Curator"

// curatorResponse は Curator schema の応答 (`{operations: [...]}`).
type curatorResponse struct {
	Operations []CuratorOperation `json:"operations"`
}

// CuratorInput は Curator の入力。
//
//   - CWD: `docs_dev/playbook/` の親として使用 (existing-rules.txt 構築の起点 / Req 8.2)。
//   - ParentRunID: artifact / debug 用 metadata。
//   - Skill: 通常 `"/agent-learnings-curator curator <runDir>"` を構築済みで渡す。
//     test 経路では skill 不在の dummy 文字列を渡す (CallAgent stub が処理)。
//
// 設計補足: curator skill の入力は filesystem (`<runDir>/learnings.json` +
// `<runDir>/existing-rules.txt`) に集約されており、本 input struct は akashic 直読みの
// 余地を持たない (Reflector phase が learnings.json artifact を書き出すことが SoT / Req 8.7)。
// 過去の MVP では `LearningsRows []akashic.LearningRow` を保持していたが、不使用 dead field
// として 2026-05-08 に除去された。
type CuratorInput struct {
	CWD         string
	ParentRunID string
	Skill       string
}

// CuratorResult は Curator の戻り値 (design.md L602-606 と整合)。
//
//   - CuratorJSONPath: 書き出した `curator.json` の絶対パス (artifact 登録用)。
//   - Operations: dedup / category 検証 *未* 適用の生 operations (= LLM 応答のまま)。
//   - Verdict: 早期確定する verdict ("" なら apply phase に進む)。
//     `VerdictAppliedNoOp` (operations 空配列) / `VerdictRejectedInvalidJSON`
//     (parse / schema 違反) のいずれか早期返却経路。それ以外は "" を返し apply に
//     進む。dedup / paths_glob / topic name 違反の reject 判定は apply phase の責務。
type CuratorResult struct {
	CuratorJSONPath string
	Operations      []CuratorOperation
	Verdict         string
}

// CuratorArtifact は `curator.json` の Go 構造体表現 (SSoT)。
type CuratorArtifact struct {
	SchemaVersion          int                `json:"schema_version"`
	ParentRunID            string             `json:"parent_run_id"`
	GeneratedAt            time.Time          `json:"generated_at"`
	Operations             []CuratorOperation `json:"operations"`
	ExistingRulesBytes     int                `json:"existing_rules_bytes"`
	ExistingRulesTruncated bool               `json:"existing_rules_truncated"`
	ExistingRulesStrategy  string             `json:"existing_rules_truncation_strategy,omitempty"`
}

// curatorSchemaVersion は curator.json のスキーマバージョン。
const curatorSchemaVersion = 1

// Curator は本 phase の純関数 (tasks.md 7.3 / design.md §Curator)。
//
// 動作契約:
//   - existing-rules.txt 構築失敗 (filesystem error) は best-effort で空 artifact を
//     書き出して LLM call に進む (curator は空 context でも novelty 判定可能 / 防御)。
//   - LLM call 失敗 (network / provider error) → curator.json を空で書き出し、
//     `Verdict=VerdictRejectedInvalidJSON` を返す (上位は applied-no-op 同等の影響度で apply skip)。
//     注: design.md は call 失敗 verdict を分けていないため、JSON 不正系として扱う。
//   - LLM 応答 parse 失敗 → raw を curator.raw.txt に保存し `Verdict=VerdictRejectedInvalidJSON`。
//   - 空 operations 配列 → `Verdict=VerdictAppliedNoOp` を返し apply phase を skip。
//   - 1 件以上の operation → Verdict="" で apply に進む (apply 側で per-op validation)。
//
// 注意: curator 自身は Recorder への書き込みを一切行わない (design.md §Curator phase
// boundary)。Reader へも触らない (existing-rules.txt は filesystem walk のみ)。
func Curator(
	ctx context.Context,
	runDir string,
	in CuratorInput,
	agent workflowctx.AgentCaller,
) (CuratorResult, error) {
	// existing-rules.txt 構築 (wiki 4 file flat layout のみ走査 / Req 8.2)。
	existingRulesPath := filepath.Join(runDir, existingRulesFilename)
	bytes, truncated, strategy, concatErr := buildLearningsExistingRulesArtifact(in.CWD, existingRulesPath, maxExistingRulesBytes)
	if concatErr != nil && !errors.Is(concatErr, fs.ErrNotExist) {
		// concat 失敗 (filesystem read error 等) は best-effort warning のみ。
		// 空 existing-rules.txt として続行する (Curator は空 context で novelty 判定する)。
		// 注: rulesDir 不在は通常状態なので noop で扱う。
		_ = concatErr
	}

	prompt := buildCuratorPrompt(in.Skill, runDir)
	opts := workflow.WithStep(&workflowctx.CallAgentOptions{
		OutputFormat: JSONSchemaForCurator(),
	}, stepIDCurator, stepNameCurator)

	resp, err := agent.CallAgent(ctx, prompt, in.CWD, nil, "", opts)
	if err != nil {
		// call 失敗: raw を best-effort 保存し rejected-invalid-json 経路で apply skip。
		raw := resp.Content
		if raw == "" {
			raw = err.Error()
		}
		_ = writeRawArtifact(runDir, curatorRawFilename, raw)
		artifact := CuratorArtifact{
			SchemaVersion:          curatorSchemaVersion,
			ParentRunID:            in.ParentRunID,
			GeneratedAt:            time.Now().UTC(),
			Operations:             []CuratorOperation{},
			ExistingRulesBytes:     bytes,
			ExistingRulesTruncated: truncated,
			ExistingRulesStrategy:  strategy,
		}
		path, _ := writeCuratorArtifact(runDir, artifact)
		return CuratorResult{
			CuratorJSONPath: path,
			Operations:      nil,
			Verdict:         VerdictRejectedInvalidJSON,
		}, nil
	}

	var parsed curatorResponse
	rawJSON, ok := extractAgentResponseJSON(resp, &parsed)
	if !ok {
		// JSON 抽出失敗 → raw を保存し rejected-invalid-json で apply skip (Req 9.3)。
		_ = writeRawArtifact(runDir, curatorRawFilename, resp.Content)
		artifact := CuratorArtifact{
			SchemaVersion:          curatorSchemaVersion,
			ParentRunID:            in.ParentRunID,
			GeneratedAt:            time.Now().UTC(),
			Operations:             []CuratorOperation{},
			ExistingRulesBytes:     bytes,
			ExistingRulesTruncated: truncated,
			ExistingRulesStrategy:  strategy,
		}
		path, werr := writeCuratorArtifact(runDir, artifact)
		if werr != nil {
			return CuratorResult{}, fmt.Errorf("learnings.Curator: write curator.json: %w", werr)
		}
		return CuratorResult{
			CuratorJSONPath: path,
			Operations:      nil,
			Verdict:         VerdictRejectedInvalidJSON,
		}, nil
	}

	// 抽出済み正規化 JSON を artifact に書き出す。
	_ = rawJSON // 抽出経路の trace 用に持っておくが artifact 内には正規化版を書く
	artifact := CuratorArtifact{
		SchemaVersion:          curatorSchemaVersion,
		ParentRunID:            in.ParentRunID,
		GeneratedAt:            time.Now().UTC(),
		Operations:             parsed.Operations,
		ExistingRulesBytes:     bytes,
		ExistingRulesTruncated: truncated,
		ExistingRulesStrategy:  strategy,
	}
	path, err := writeCuratorArtifact(runDir, artifact)
	if err != nil {
		return CuratorResult{}, fmt.Errorf("learnings.Curator: write curator.json: %w", err)
	}

	// 空 operations → applied-no-op で apply phase を skip。
	if len(parsed.Operations) == 0 {
		return CuratorResult{
			CuratorJSONPath: path,
			Operations:      []CuratorOperation{},
			Verdict:         VerdictAppliedNoOp,
		}, nil
	}

	// 1 件以上 → verdict 未確定で apply に進む (apply phase が validation + dedup)。
	return CuratorResult{
		CuratorJSONPath: path,
		Operations:      parsed.Operations,
		Verdict:         "",
	}, nil
}

// buildCuratorPrompt は skill SSoT 起動用の slash command 文字列を構築する。
func buildCuratorPrompt(skill, runDir string) string {
	if strings.TrimSpace(skill) != "" {
		return skill
	}
	return fmt.Sprintf("%s %s %s", curatorSkillSlash, curatorMode, runDir)
}

// writeRawArtifact は raw response (parse 失敗時等) を `<runDir>/<filename>` に
// best-effort 書き出す。失敗は握りつぶす。
func writeRawArtifact(runDir, filename, raw string) error {
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(runDir, filename), []byte(raw), 0o644)
}

// writeCuratorArtifact は CuratorArtifact を `<runDir>/curator.json` に atomic write する。
func writeCuratorArtifact(runDir string, artifact CuratorArtifact) (string, error) {
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir runDir %q: %w", runDir, err)
	}
	path := filepath.Join(runDir, curatorFilename)
	if err := writeJSONAtomic(path, artifact); err != nil {
		return "", err
	}
	return path, nil
}

// learningsExistingRulesEntry は existing-rules.txt の concat 単位 (1 category file 分の素材)。
//
//   - rel: cwd 相対 path (`docs_dev/playbook/<category>.md` の forward slash 表記)
//   - body: file 全文 (heading + 概要 + 全 section)
//   - category: 4 値 closed enum の category 名 (Topic Index header の category 行に使う)
//   - topics: file 内の `## <topic>` section から抽出した topic slug 列 (file 出現順)
//
// wiki 5 file layout への移行後、各 entry が複数 topic を保持するため、
// 旧 layout 時代の `(category, topic)` を path から逆算する設計を捨て、
// `ParseCategoryFile` の結果を entry に焼き込んでおく構造とした。
type learningsExistingRulesEntry struct {
	rel      string
	body     []byte
	category string
	topics   []string
}

// buildLearningsExistingRulesArtifact は `cwd/docs_dev/playbook/<category>.md` の
// 4 category file (closed enum) を直接読み込んで outPath に concat 書き出す (Req 8.2)。
//
// 出力フォーマット (skill 側 curator.md と一致):
//
//	===== <relative-path> =====
//	<file body>
//
// 戻り値:
//
//	bytes      - 書き出した artifact の byte 数
//	truncated  - degrade strategy が適用されたかどうか
//	strategy   - degrade strategy 名 ("" / "playbook-only" / "playbook-only+head-tail-clip")
//	err        - filesystem 書き出し失敗等
//
// scan の挙動:
//
//   - 4 category file を closed enum 順 (failure-recovery → success-procedure →
//     antipattern → tool-pattern) に直接読む。
//   - 不在 file は skip (warning も error も発しない / Req 8.3 と整合)。
//   - 個別 file の read 失敗 (permission 等) は best-effort で skip。
//   - 旧 `.claude/rules/playbook/` 配下は一切 scan しない (Req 8.2)。
//
// size guard / degrade strategy:
//
//   - 第 0 段: 全 category file を concat。`limit` 以下に収まれば truncated=false で完了。
//   - 第 1 段 ("playbook-only"): 本 spec の playbook scope は元から playbook 配下のみのため、
//     v1 の "top-level rule 除外" 段に対応する処理は no-op。命名互換のため strategy 名は維持。
//   - 第 2 段 ("playbook-only+head-tail-clip"): 各 category file を「先頭 heading + 末尾 N bytes」
//     に切り詰める。新 layout では frontmatter が廃止されたため、単純な head + tail clip となる。
func buildLearningsExistingRulesArtifact(cwd, outPath string, limit int) (int, bool, string, error) {
	playbookDir := filepath.Join(cwd, "docs_dev", "playbook")

	// playbook root 自体の存在 / 種別チェック。
	// 不在は通常状態 (本 spec 初回実行) として空 artifact 書き出し + 正常 return。
	if _, statErr := os.Stat(playbookDir); statErr != nil {
		if errors.Is(statErr, fs.ErrNotExist) {
			if err := os.WriteFile(outPath, []byte{}, 0o644); err != nil {
				return 0, false, "", fmt.Errorf("existing-rules: write empty %q: %w", outPath, err)
			}
			return 0, false, "", nil
		}
		// stat 失敗 (permission 等) は空 artifact + error 伝播。
		if writeErr := os.WriteFile(outPath, []byte{}, 0o644); writeErr != nil {
			return 0, false, "", fmt.Errorf("existing-rules: write empty %q: %w", outPath, writeErr)
		}
		return 0, false, "", fmt.Errorf("existing-rules: stat %q: %w", playbookDir, statErr)
	}

	// 4 category file を closed enum 順 (SSoT) に直接読む。
	var entries []learningsExistingRulesEntry
	for _, category := range orderedLearningsCategories() {
		filename := category + ".md"
		path := filepath.Join(playbookDir, filename)
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			// 不在 file は skip (warning も error も発しない / Req 8.3 と整合)。
			// permission 等の read 失敗も best-effort で skip し、scan を続行する。
			continue
		}
		// `ParseCategoryFile` で section の topic slug 列を抽出 (Topic Index header 構築用)。
		// parse 失敗 (本来は body == nil 経路でしか起きないが防御) は entry を捨てずに
		// topics 空のまま保持して concat に進む (skill 側に raw 本文を渡す責務を優先)。
		topics := parseTopicsFromBody(body)
		rel := filepath.ToSlash(filepath.Join("docs_dev", "playbook", filename))
		entries = append(entries, learningsExistingRulesEntry{
			rel:      rel,
			body:     body,
			category: category,
			topics:   topics,
		})
	}

	// 1 file も読めなかった場合は空 artifact を書き出して終了 (本 spec 初回実行 / 全 file 不在)。
	if len(entries) == 0 {
		if err := os.WriteFile(outPath, []byte{}, 0o644); err != nil {
			return 0, false, "", fmt.Errorf("existing-rules: write empty %q: %w", outPath, err)
		}
		return 0, false, "", nil
	}

	// 第 0 段: 全 file concat。
	full := concatLearningsExistingRulesEntries(entries)
	if len(full) <= limit {
		if err := os.WriteFile(outPath, []byte(full), 0o644); err != nil {
			return 0, false, "", fmt.Errorf("existing-rules: write %q: %w", outPath, err)
		}
		return len(full), false, "", nil
	}

	// 第 1 段 ("playbook-only"): 本 spec では playbook scope のみのため第 0 段と同等。
	// strategy 名のみ立てて truncated=true で返す経路を持たず、head-tail-clip に直接降格する。

	// 第 2 段 ("playbook-only+head-tail-clip"): 各 category file を head + tail clip。
	clipped := clipLearningsPlaybookEntries(entries, limit)
	if err := os.WriteFile(outPath, []byte(clipped), 0o644); err != nil {
		return 0, false, "", fmt.Errorf("existing-rules: write %q: %w", outPath, err)
	}
	return len(clipped), true, existingRulesTruncationStrategyHeadTailClip, nil
}

// parseTopicsFromBody は category file 本文から `## <topic>` section の slug 列を
// 出現順に抽出する。`ParseCategoryFile` の結果を Section.Topic で平坦化したものに等しい。
// parse 失敗 (body nil) や section ゼロでも空 slice を返す (panic / error にしない)。
func parseTopicsFromBody(body []byte) []string {
	if len(body) == 0 {
		return nil
	}
	parsed, err := ParseCategoryFile(body)
	if err != nil {
		// 本来は body == nil のときのみ起きるが、防御的に空 slice を返す。
		return nil
	}
	out := make([]string, 0, len(parsed.Sections))
	for _, s := range parsed.Sections {
		out = append(out, s.Topic)
	}
	return out
}

// concatLearningsExistingRulesEntries は entries を `===== <rel> =====` セパレータで
// concat した文字列を返す。entries 順序は呼び出し側の slice 順序を保つ。
//
// 出力先頭には `formatTopicIndex` が生成する Topic Index header (category 別 topic 一覧)
// を prepend する。これは curator skill の judgment 1 が「既存 topic 流用を最優先」と
// 命じる際に LLM が re-use 候補を即座に拾えるようにするための可視化 (A-1)。entries が
// 1 件も無い / topic を 1 件も持たない場合は header は空文字となり前方互換を保つ。
func concatLearningsExistingRulesEntries(entries []learningsExistingRulesEntry) string {
	var b strings.Builder
	if header := formatTopicIndex(entries); header != "" {
		b.WriteString(header)
	}
	for _, e := range entries {
		fmt.Fprintf(&b, "===== %s =====\n", e.rel)
		b.Write(e.body)
		b.WriteString("\n")
	}
	return b.String()
}

// formatTopicIndex は entries から category 別の topic 一覧を抽出し、curator skill が
// 流用候補を即座に把握できる Markdown header を返す (A-1)。
//
// 出力フォーマット:
//
//	## Topic Index (reuse first; create new only if no existing topic fits)
//
//	- failure-recovery: <topic1>, <topic2>, ...
//	- success-procedure: ...
//	- antipattern: ...
//	- tool-pattern: ...
//
// 規約:
//
//   - category の出力順は `CategoryFailureRecovery` 等の SSoT 順序 (closed enum 4 値)。
//     範囲外 category 名 (`learningsExistingRulesEntry.category` が closed enum 外) は
//     無視する (defense in depth)。
//   - 各 category 内の topic は alphabetical sort + dedup で決定的に並べる。
//   - entry が 1 件も topic を持たない場合は空文字を返し、前方互換 (header 不在経路) を保つ。
func formatTopicIndex(entries []learningsExistingRulesEntry) string {
	catToTopics := make(map[string][]string, len(orderedLearningsCategories()))
	for _, e := range entries {
		if !ValidCategory(e.category) {
			continue
		}
		if len(e.topics) == 0 {
			continue
		}
		catToTopics[e.category] = append(catToTopics[e.category], e.topics...)
	}

	// 1 つも valid entry が無ければ header を出さない。
	hasAny := false
	for _, cat := range orderedLearningsCategories() {
		if len(catToTopics[cat]) > 0 {
			hasAny = true
			break
		}
	}
	if !hasAny {
		return ""
	}

	var b strings.Builder
	b.WriteString("## Topic Index (reuse first; create new only if no existing topic fits)\n\n")
	for _, cat := range orderedLearningsCategories() {
		topics := catToTopics[cat]
		if len(topics) == 0 {
			continue
		}
		// alphabetical sort + dedup (同一 topic slug が複数 entry に重複した場合の defense)
		sort.Strings(topics)
		uniq := topics[:0]
		var last string
		for i, t := range topics {
			if i == 0 || t != last {
				uniq = append(uniq, t)
				last = t
			}
		}
		fmt.Fprintf(&b, "- %s: %s\n", cat, strings.Join(uniq, ", "))
	}
	b.WriteString("\n")
	return b.String()
}

// orderedLearningsCategories は Topic Index の出力順序を pin する SSoT 4 値配列。
// `category.go` の `Category*` 定数を referencing して closed enum 順序を保つ。
// 範囲外 subdir 名は formatTopicIndex で skip される (defense in depth)。
func orderedLearningsCategories() []string {
	return []string{
		CategoryFailureRecovery,
		CategorySuccessProcedure,
		CategoryAntipattern,
		CategoryToolPattern,
	}
}

// clipLearningsPlaybookEntries は entries を「先頭 heading + 末尾 N bytes」に
// 切り詰めた文字列を返す。新 wiki layout では `paths:` frontmatter は廃止されたため、
// 各 category file の先頭 `# <category>` heading 行のみ verbatim 保持し、残りは body tail clip。
// 出力先頭の Topic Index header (formatTopicIndex) も verbatim 保持し、budget には
// header 分を加算してから per-file body を均等割りする (A-1)。
//
// budget 配分: per-file budget = (limit - len(header) - sum(separators+headings)) / num-files。
// 負になる場合は heading のみ書き出して本文 0 bytes にする。
func clipLearningsPlaybookEntries(entries []learningsExistingRulesEntry, limit int) string {
	if len(entries) == 0 {
		return ""
	}

	header := formatTopicIndex(entries)

	type prepped struct {
		separator   string
		fileHeading string
		bodyTail    []byte
	}
	prepps := make([]prepped, 0, len(entries))
	totalFixed := len(header)
	for _, e := range entries {
		sep := fmt.Sprintf("===== %s =====\n", e.rel)
		fileHeading, body := splitLearningsFileHeading(e.body)
		fixed := len(sep) + len(fileHeading) + 1
		totalFixed += fixed
		prepps = append(prepps, prepped{separator: sep, fileHeading: fileHeading, bodyTail: body})
	}

	bodyBudget := limit - totalFixed
	if bodyBudget < 0 {
		bodyBudget = 0
	}
	perFile := 0
	if len(prepps) > 0 {
		perFile = bodyBudget / len(prepps)
	}

	var b strings.Builder
	b.WriteString(header)
	for _, p := range prepps {
		b.WriteString(p.separator)
		b.WriteString(p.fileHeading)
		tail := p.bodyTail
		if len(tail) > perFile {
			tail = tail[len(tail)-perFile:]
		}
		b.Write(tail)
		b.WriteString("\n")
	}
	out := b.String()
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// splitLearningsFileHeading は category file の先頭 `# <category>` heading 行 (改行込み) と
// それ以降の body を分割する。clip 経路で「どの category file の clip か」を識別できる
// 最低限の verbatim を残すための helper。
//
//   - 先頭が `# ` で始まらない場合は ("", content) を返す (heading 不在 / 破損 defense)。
//   - 改行が無い (= file 全体が 1 行) 場合は (content, []) を返す。
func splitLearningsFileHeading(content []byte) (string, []byte) {
	s := string(content)
	if !strings.HasPrefix(s, "# ") {
		return "", content
	}
	nl := strings.IndexByte(s, '\n')
	if nl < 0 {
		// file 全体が改行無しで `# ...` の 1 行のみ。heading 扱いで body 0 bytes。
		return s, nil
	}
	headingEnd := nl + 1 // 改行を含める
	return s[:headingEnd], []byte(s[headingEnd:])
}

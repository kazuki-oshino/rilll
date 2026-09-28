// Package learnings の本 file は agent-learnings 4 phase pipeline の最初の phase
// (pre-fetch) を担う純関数 `PreFetch` と関連型 `PreFetchInput` / `PreFetchResult` /
// sentinel `ErrReaderUnavailable` を提供する (tasks.md 7.1 / design.md §PreFetch /
// Req 3.1〜3.6, 8.2, 9.1)。
//
// 役割:
//   - 親 workflow run の `parent_run_id` を起点に akashic から `agent_calls` を取得し、
//     非空 `session_id` を dedup + 安定順序で並べた `UniqueSessionIDs` を Reflector
//     phase に渡す。
//   - 空 / NULL `session_id` を skip して `<runDir>/sessions.json.skipped[]` に trace
//     として残す (Req 3.4)。
//   - `agent_calls` 0 件 / 全 row が空 session_id の場合は `UniqueSessionIDs == []`
//     を返し、上位 orchestration が `verdict=applied-no-op` 経路に切り替えるための
//     seam を提供する (Req 3.3)。
//   - reader nil または `AgentCallsByWorkflowRun` 由来 error は `ErrReaderUnavailable`
//     として伝播し、上位 orchestration が `verdict=reader-unavailable` に切り替える
//     (Req 9.1)。
//
// 設計契約:
//
//   - **ProjectScopedReader 経由でのみ akashic に触る** (Req 3.6): 入力 reader を
//     `akashic.NewProjectScopedReader(reader, akashic.ProjectIDFromCWD(in.CWD))` で
//     wrap した decorator だけを呼び出し、inner reader を直接は呼ばない。
//     ProjectScopedReader.AgentCallsByWorkflowRun は r.projectID で post-filter する
//     ため、別 project の row が混入する経路は構造的に閉ざされる。
//   - **LLM 不使用** (Req 2.4): 本 phase は決定的な Go 処理で完結し、AgentCaller を
//     一切呼ばない。これは関数シグネチャに `agent` 引数を持たないことで物理保証する。
//   - **書き込みは learnings table 以外触らない** (Req 9.6): 本 phase は read-only。
//     副作用は `<runDir>/sessions.json` の atomic write のみ。
//   - **artifact atomic write**: `tmp + os.Rename` 経路で書き出す (v1 distiller の
//     `writeJSONAtomic` と同形)。同 dir 内 rename は POSIX 上 atomic で、書き込み中の
//     途中ファイルが後段 phase に観測されない不変式を保証する。
//
// 関連 Requirement: 3.1 (parent_run_id → agent_calls 取得) / 3.2 (dedup + 安定順序) /
// 3.3 (0 件 → applied-no-op seam) / 3.4 (空 session_id skip + skipped[] trace) /
// 3.5 (sessions.json artifact 出力) / 3.6 (ProjectScopedReader 経由の project scope
// 強制) / 8.2 (sessions.json artifact layout) / 9.1 (reader-unavailable 経路)。
package learnings

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"time"

	"github.com/kazuki-oshino/rilll/internal/akashic"
)

// sessionsFilename は pre-fetch phase が `runDir/` 直下に書き出す JSON artifact 名
// (Req 8.2 の "sessions.json")。
const sessionsFilename = "sessions.json"

// sessionsSchemaVersion は sessions.json のスキーマバージョン。後方非互換変更でのみ
// インクリメントする (v1 distiller の `snapshotSchemaVersion` と同形)。
//
// 後段 consumer (Reflector phase / 人間 / debug ツール) は schema_version を読んで
// legacy / new を分岐できる。後方互換: 既存 field は不変 (順序 / json tag 保持)。
const sessionsSchemaVersion = 1

// ErrReaderUnavailable は PreFetch に nil reader が渡された場合、または
// `AgentCallsByWorkflowRun` 呼び出しが error を返した場合に返す sentinel error。
// 上位 orchestration はこの sentinel を `errors.Is` で識別し、`apply-result.json` の
// verdict を `reader-unavailable` に切り替える (design.md §Error Strategy / Req 9.1)。
//
// 設計判断: nil reader と reader 呼び出し失敗 (DB lock / connection error 等) は
// 上位から見て同じ "reader が使えない" 状態であり、verdict も同一 (`reader-unavailable`)
// なため、両者を 1 つの sentinel に統合して `errors.Is` で一貫識別できるようにする
// (design.md L504 "Reader nil または error → ErrReaderUnavailable を伝播")。
var ErrReaderUnavailable = errors.New("learnings: akashic reader unavailable")

// PreFetchInput は PreFetch の入力 (design.md L508-511)。
//
//   - CWD: 必須。`ProjectIDFromCWD` の入力かつ ProjectScopedReader 構築の根拠。
//     空文字でも ProjectIDFromCWD は basename フォールバックを返すが、tail wrap 経路
//     では base.Run と同じ cwd が必ず渡される (design.md §LearningsWorkflow Preconditions)。
//   - ParentRunID: 必須。tail wrap 経路で `SequentialWithLearningsTail` が `ctx` 経由で
//     注入し、orchestration 層が `ParentRunIDFromContext(ctx)` で取り出した値を渡す。
//     空文字のときは `AgentCallsByWorkflowRun(ctx, "")` が 0 件で返るため、上位は
//     `verdict=applied-no-op` 経路に進む (precondition 違反 vs. agent_calls 0 件は
//     観測上区別しない設計)。
type PreFetchInput struct {
	CWD         string
	ParentRunID string
}

// PreFetchResult は PreFetch の戻り値 (design.md L513-517)。
//
//   - SessionsJSONPath: 書き出した `sessions.json` の絶対パス (artifact 登録用)。
//     reader nil で error early return した場合は空文字で返る (artifact は生成されない)。
//   - UniqueSessionIDs: dedup 済み non-empty session_id の安定順序 list。
//     0 件 (agent_calls 0 件 / 全 row が空 session_id) なら空 slice を返し、上位は
//     `verdict=applied-no-op` 経路に切り替える (Req 3.3 の seam)。
//     順序契約: `AgentCallsByWorkflowRun` が `started_at ASC, id ASC` の安定順序で
//     返すため、最初に出現した順を保つことで「started_at ASC tiebreaker」を満たす。
//   - Sessions: `UniqueSessionIDs` と同順の SessionEntry slice。Reflector phase が
//     allowlist 判定 (`RILLL_LEARNINGS_PROVIDERS`) と learnings 行の step_id 転記で
//     消費する (task 7.2)。中身は sessions.json 内 `sessions[]` と同じ構造。
//   - ReflectionTargets: early tail aggregation が job row と照合する agent_call_id 単位の
//     final target set。session_id は resume input であり identity ではないため、同じ
//     session_id を持つ複数 agent_call も別 target として保持する。
//   - SkippedRowCount: 空 / NULL session_id で skip された row 数 (Req 3.4 の観測点)。
type PreFetchResult struct {
	SessionsJSONPath  string
	UniqueSessionIDs  []string
	Sessions          []SessionEntry
	ReflectionTargets []ReflectionTarget
	SkippedRowCount   int
}

// ReflectionTarget は early tail aggregation が job store と照合する agent call snapshot。
// identity は AgentCallID で、SessionID は provider resume の入力属性としてのみ扱う。
type ReflectionTarget struct {
	AgentCallID   string
	WorkflowRunID string
	SessionID     string
	ProviderID    string
	StepID        string
	StepName      string
}

// SessionEntry は `sessions.json.sessions[]` の 1 要素 (Req 8.2)。
//
// 1 つの session_id に対し、その session_id を持つ agent_call の id 群 (started_at ASC
// 順) を AgentCallIDs に列挙する。これにより Reflector phase は session_id 単位で
// resume call を発行しつつ、debug 時に「どの agent_call から再開した session か」を
// trace できる。
//
// ProviderID は session を最初に作成した provider (claude / cursor / codex 等) の
// id (`agent_calls.provider_id` の最初の出現を採用)。Reflector phase の
// `RILLL_LEARNINGS_PROVIDERS` allowlist 比較に使う (task 7.2 / Req 4.x の補強)。
// StepID も同様に「最初の出現」を採用し、accept された learning を `learnings` table
// に書き戻すときの step_id 列に転記する用途で使う (task 7.2 が消費)。
type SessionEntry struct {
	SessionID    string   `json:"session_id"`
	AgentCallIDs []string `json:"agent_call_ids"`
	ProviderID   string   `json:"provider_id,omitempty"`
	StepID       string   `json:"step_id,omitempty"`
}

// SkippedEntry は `sessions.json.skipped[]` の 1 要素 (Req 3.4)。
//
// 空 / NULL session_id を持つ agent_call row を 1 行ずつ列挙する。debug 時に
// 「どの provider 経路で session_id が空になったか」を追跡する観測点となる。
//
// Reason は将来的な分類拡張のため文字列定数で持つが、本 spec では
// `"empty-session-id"` のみを使う (NULL も Go 側では空文字で観測される)。
type SkippedEntry struct {
	AgentCallID string `json:"agent_call_id"`
	StepID      string `json:"step_id,omitempty"`
	ProviderID  string `json:"provider_id,omitempty"`
	Reason      string `json:"reason"`
}

// SessionsArtifact は `sessions.json` の Go 構造体表現 (SSoT)。
//
// design.md §Data Contracts: 本構造体が schema の唯一定義。`schema_version` は
// 後方非互換変更でのみインクリメント。フィールド順序 / json tag は後段 consumer
// (Reflector phase / debug ツール) と固定共有。
//
// 設計上の決定:
//   - Sessions / Skipped は `nil` ではなく空 slice で marshal する (`null` を避けて
//     後段 consumer の入力契約を単純化する)。
//   - GeneratedAt は test 注入の seam として `time.Now` で埋め、test 側は固定時刻を
//     渡す手段がないため golden 比較は GeneratedAt 抜きで行う (もしくは
//     RFC3339 形式の存在のみを検証)。
type SessionsArtifact struct {
	SchemaVersion int            `json:"schema_version"`
	ParentRunID   string         `json:"parent_run_id"`
	GeneratedAt   time.Time      `json:"generated_at"`
	Sessions      []SessionEntry `json:"sessions"`
	Skipped       []SkippedEntry `json:"skipped"`
}

// SkipReasonEmptySessionID は SkippedEntry.Reason の値定数。
// 本 spec の MVP では空 / NULL session_id を 1 つの reason に統合して扱う。
// 将来 provider 別の skip 経路を追加する際に reason を分岐させる。
const SkipReasonEmptySessionID = "empty-session-id"

// PreFetch は parent_run_id から agent_calls を取得し、非空 session_id を dedup +
// 安定順序で並べた `UniqueSessionIDs` と `<runDir>/sessions.json` artifact を生成する
// (design.md L519 / Req 3.1〜3.6, 8.2, 9.1)。
//
// 動作契約:
//   - reader == nil → `ErrReaderUnavailable` を返し、副作用なし (artifact 未生成)。
//   - `AgentCallsByWorkflowRun` 呼び出し失敗 → `ErrReaderUnavailable` を wrap した
//     error を返す (errors.Is で sentinel 識別可能)。artifact 未生成。
//   - 全 Reader 呼び出しは ProjectScopedReader 経由 (Req 3.6 の物理保証)。
//   - 空 / NULL session_id は dedup 集合から除外し `Skipped[]` に記録する (Req 3.4)。
//   - 同一 session_id を複数 agent_call が持つ場合は最初の出現を保ち、後続を
//     SessionEntry.AgentCallIDs に追加する (started_at ASC tiebreaker / Req 3.2)。
//   - agent_calls 0 件 / 全 row が空 session_id でも sessions.json は書き出す
//     (artifact は常に存在するため orchestration の defer 経路と整合)。
//
// 不変式: ProjectScopedReader 経由でしか akashic に触らない。LLM 呼び出しなし。
func PreFetch(ctx context.Context, runDir string, in PreFetchInput, reader akashic.Reader) (PreFetchResult, error) {
	if reader == nil {
		return PreFetchResult{}, ErrReaderUnavailable
	}

	rows, err := preFetchAgentCallRows(ctx, in, reader)
	if err != nil {
		return PreFetchResult{}, fmt.Errorf("learnings.PreFetch: agent_calls: %w", errors.Join(ErrReaderUnavailable, err))
	}

	uniqueIDs, sessionEntries, skipped := collectUniqueSessionIDs(rows)
	reflectionTargets := collectReflectionTargets(rows)

	artifact := SessionsArtifact{
		SchemaVersion: sessionsSchemaVersion,
		ParentRunID:   in.ParentRunID,
		GeneratedAt:   time.Now().UTC(),
		Sessions:      sessionEntries,
		Skipped:       skipped,
	}

	outPath := filepath.Join(runDir, sessionsFilename)
	if err := writeJSONAtomic(outPath, artifact); err != nil {
		return PreFetchResult{}, fmt.Errorf("learnings.PreFetch: write sessions.json: %w", err)
	}

	return PreFetchResult{
		SessionsJSONPath:  outPath,
		UniqueSessionIDs:  uniqueIDs,
		Sessions:          sessionEntries,
		ReflectionTargets: reflectionTargets,
		SkippedRowCount:   len(skipped),
	}, nil
}

// CollectReflectionTargets は parent run と descendant run の agent calls を
// agent_call_id 単位の final target set として返す。sessions.json は書き出さないため、
// early tail aggregation が job rows と照合する collector として利用できる。
func CollectReflectionTargets(ctx context.Context, in PreFetchInput, reader akashic.Reader) ([]ReflectionTarget, error) {
	if reader == nil {
		return nil, ErrReaderUnavailable
	}
	rows, err := preFetchAgentCallRows(ctx, in, reader)
	if err != nil {
		return nil, fmt.Errorf("learnings.CollectReflectionTargets: agent_calls: %w", errors.Join(ErrReaderUnavailable, err))
	}
	return collectReflectionTargets(rows), nil
}

func preFetchAgentCallRows(ctx context.Context, in PreFetchInput, reader akashic.Reader) ([]akashic.AgentCallRow, error) {
	// project scope 強制 (Req 3.6): cwd basename を ProjectID として decorator で上書き。
	projectID := akashic.ProjectIDFromCWD(in.CWD)
	scoped := akashic.NewProjectScopedReader(reader, projectID)

	runIDs, err := preFetchWorkflowRunIDs(ctx, scoped, in.ParentRunID)
	if err != nil {
		return nil, err
	}
	rows := []akashic.AgentCallRow{}
	for _, runID := range runIDs {
		runRows, err := scoped.AgentCallsByWorkflowRun(ctx, runID)
		if err != nil {
			return nil, err
		}
		rows = append(rows, runRows...)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].StartedAt.Equal(rows[j].StartedAt) {
			return rows[i].ID < rows[j].ID
		}
		return rows[i].StartedAt.Before(rows[j].StartedAt)
	})
	return rows, nil
}

func preFetchWorkflowRunIDs(ctx context.Context, scoped *akashic.ProjectScopedReader, parentRunID string) ([]string, error) {
	runIDs := []string{parentRunID}
	descendantIDs, err := scoped.DescendantWorkflowRunIDs(ctx, parentRunID)
	if err != nil {
		return nil, err
	}
	seen := map[string]struct{}{parentRunID: {}}
	for _, id := range descendantIDs {
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		runIDs = append(runIDs, id)
	}
	return runIDs, nil
}

// collectUniqueSessionIDs は AgentCallsByWorkflowRun 戻り値を入力に取り、
//  1. 非空 session_id を dedup + 安定順序で並べた `uniqueIDs`
//  2. session_id → agent_call_ids の mapping を表す `sessionEntries` (sessions.json.sessions[])
//  3. 空 session_id row の trace `skipped` (sessions.json.skipped[])
//
// の 3 つを返す純関数 (design.md L334 の `collectUniqueSessionIDs(rows) []string` を
// 拡張して artifact 用の付加情報も返す形にしている)。
//
// 順序契約: `AgentCallsByWorkflowRun` が `started_at ASC, id ASC` の安定順序で
// rows を返すため、本関数は単純に最初の出現順を保つだけで Req 3.2 の "started_at
// ASC tiebreaker" を満たす。
//
// dedup 戦略:
//   - 同一 session_id を複数 agent_call が持つ場合 (resume call で同じ session に
//     再接続するケース)、最初の出現を `uniqueIDs` に追加し、後続出現は無視する。
//   - SessionEntry.AgentCallIDs には全出現を順番に保持する (debug trace 用)。
//   - rows が nil / 空 → `(nil, nil, nil)` ではなく `([]string{}, []SessionEntry{},
//     []SkippedEntry{})` を返す (`nil` を避けて JSON marshal を `[]` に揃える)。
//
// 入力 rows は `[]akashic.AgentCallRow` 型で、本関数は `SessionID` / `ID` / `StepID` /
// `ProviderID` field のみを参照する。
func collectUniqueSessionIDs(rows []akashic.AgentCallRow) (uniqueIDs []string, sessionEntries []SessionEntry, skipped []SkippedEntry) {
	uniqueIDs = []string{}
	sessionEntries = []SessionEntry{}
	skipped = []SkippedEntry{}

	if len(rows) == 0 {
		return
	}

	// session_id → SessionEntry のインデックスを保持し、後続出現で AgentCallIDs を追記する。
	indexBySessionID := make(map[string]int, len(rows))

	for _, row := range rows {
		if row.SessionID == "" {
			skipped = append(skipped, SkippedEntry{
				AgentCallID: row.ID,
				StepID:      row.StepID,
				ProviderID:  row.ProviderID,
				Reason:      SkipReasonEmptySessionID,
			})
			continue
		}
		if idx, ok := indexBySessionID[row.SessionID]; ok {
			// 既に登録済みの session_id (resume case): AgentCallIDs にだけ追加。
			sessionEntries[idx].AgentCallIDs = append(sessionEntries[idx].AgentCallIDs, row.ID)
			continue
		}
		// 新規 session_id: uniqueIDs と sessionEntries の両方に追加。
		indexBySessionID[row.SessionID] = len(sessionEntries)
		uniqueIDs = append(uniqueIDs, row.SessionID)
		sessionEntries = append(sessionEntries, SessionEntry{
			SessionID:    row.SessionID,
			AgentCallIDs: []string{row.ID},
			ProviderID:   row.ProviderID,
			StepID:       row.StepID,
		})
	}
	return
}

func collectReflectionTargets(rows []akashic.AgentCallRow) []ReflectionTarget {
	targets := make([]ReflectionTarget, 0, len(rows))
	for _, row := range rows {
		targets = append(targets, ReflectionTarget{
			AgentCallID:   row.ID,
			WorkflowRunID: row.WorkflowRunID,
			SessionID:     row.SessionID,
			ProviderID:    row.ProviderID,
			StepID:        row.StepID,
			StepName:      row.StepName,
		})
	}
	return targets
}

// writeJSONAtomic は v を JSON にエンコードし `path` に atomic write する
// 薄い wrapper。本体の tmp + rename atomic write は `writeFileAtomic`
// (wiki_atomic.go) に委譲する。
//
// 設計理由 (v1 distiller `writeJSONAtomic` と同形):
//   - 同一ディレクトリ内 rename は POSIX 上 atomic (途中状態のファイルが
//     後段 phase に観測されない)。
//   - 失敗時は tmp ファイルが残り得るが、次回 run の成功 rename で同じ tmp 名が
//     上書きされるため累積しない。中間状態の `*.tmp` は debug にも有用。
//   - perm 0o644 は config.go と同じ (workflow recorder が user-readable で扱う想定)。
//
// JSON は MarshalIndent で人間可読化する (Reflector phase の入力にも、人間 debug にも
// 使う artifact のため)。末尾改行は POSIX text file 規約 / git diff の見やすさのため
// 本 wrapper 側で付与する (writeFileAtomic は body を生バイトとして書き込むだけで、
// 改行付与は呼び出し側の責務)。
func writeJSONAtomic(path string, v any) error {
	body, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	// 末尾改行を付ける (POSIX text file 規約 / git diff が綺麗に出る)。
	body = append(body, '\n')

	return writeFileAtomic(path, body, 0o644)
}

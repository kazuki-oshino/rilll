// Package learnings の本 file は agent-learnings 4 phase pipeline の 2 段目
// (reflector) を担う `Reflector` 関数と関連型 (`ReflectorInput` / `ReflectorResult` /
// `ReflectorArtifact` 等) を提供する (tasks.md 7.2 / design.md §Reflector /
// Req 4.1〜4.7, 5.4, 8.3, 9.2, 9.4, 9.5)。
//
// 役割:
//
//   - PreFetch phase が抽出した unique session_id を入力に取り、各 session に対して
//     **独立した goroutine** で `agent.CallAgent(ctx, "/agent-learnings-curator reflector
//     <runDir>", cwd, onEvent, sessionID, opts)` を発火する。
//   - `OutputFormat=JSONSchemaForReflector()` で LLM 応答を JSON 強制する。
//   - 並列度は `RILLL_LEARNINGS_MAX_PARALLEL` (default 5) 上限の `ParallelLimiter` で制御。
//   - per-session 失敗 (timeout / provider error / JSON parse 失敗 / category enum 違反) は
//     best-effort で集約し、他セッションを止めない (Req 4.5, 9.4)。
//   - 成功 learning は `Recorder.RecordLearning` で `learnings` table に insert する
//     (`source=agent-self-report`、Req 5.2)。
//   - category enum 不一致 learning は drop し `dropped[]` に trace (Req 9.5)。
//   - allowlist (`RILLL_LEARNINGS_PROVIDERS`、default `claude,codex`) 外 provider が
//     ResumeSession を作成した session は phase で skip し `skipped_provider[]` に記録する
//     (spike-results.md / Phase 0 の決定。default 値はユーザー指示で 2026-05-17 に
//     `claude` → `claude,codex` に変更。本リポジトリの parent loop は claude/codex provider
//     の両方で動くため)。
//   - 全 session 失敗 → `AllFailed=true` を ReflectorResult に伝え、上位は
//     `verdict=resume-all-failed` 経路に切り替える (Req 4.6)。
//   - `ErrNoCredentials` 相当 (provider auth 情報不在) を 1 件以上のセッションで検出した場合は
//     `NoCredentials=true` を ReflectorResult に伝え、上位が `verdict=applied-no-op` に
//     切り替える (Req 9.2)。
//   - 空 learnings 配列を返したセッションは insert 0 件で「学びなし」を意味する有効応答
//     として扱う (Req 5.4)。
//
// 設計契約:
//
//   - **per-session goroutine**: 1 session 1 goroutine。`sync.WaitGroup` で全 goroutine の
//     完了を待ち、`ParallelLimiter.Acquire/Release` で同時実行枠を制限する。channel ベースの
//     fan-in / fan-out よりシンプルで、デッドロック条件を持ち込まない。
//   - **best-effort 続行**: 1 セッションの失敗は他に伝播しない。ctx cancel のみが全体停止の
//     合図 (`ParallelLimiter.Acquire(ctx)` で ctx 取消を観測)。
//   - **Recorder validation 信頼**: `Recorder.RecordLearning` 自身が category enum / paths_glob
//     上限を validate するため、本 phase では category 検証を二重化するだけ (drop 経路を
//     artifact 内に残すため)。Recorder 由来の error は `RecordLearning` failure として
//     per_session_failures に記録するのみ (skip して次の learning へ)。
//   - **artifact atomic write**: prefetch.go の `writeJSONAtomic` を再利用 (同 package private)。
//     全 exit 経路 (成功 / 失敗 / 全 skip) で `<runDir>/reflector.json` を書き出す。
//   - **provider allowlist**: `RILLL_LEARNINGS_PROVIDERS` (CSV) の env で上書き可能。default は
//     `claude,codex` (本リポジトリの parent loop が claude/codex provider の両方で動くため、
//     ユーザー指示で 2026-05-17 に `claude` から変更)。追加 provider を許可する場合は
//     `RILLL_LEARNINGS_PROVIDERS=claude,codex,cursor` の形で指定する。
//   - **WorkflowID 解決**: `learnings.workflow_id` 列の値は `ReflectorInput.WorkflowID` が
//     供給する。tail wrap 経路では parent run の workflow_id を orchestration 層 (task 8.1)
//     が解決して渡す責務 (本 phase は素通しする)。
//
// 関連 Requirement: 4.1〜4.7 / 5.4 / 8.3 / 9.2 / 9.4 / 9.5。
package learnings

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/kazuki-oshino/rilll/internal/akashic"
	"github.com/kazuki-oshino/rilll/internal/workflow"
	"github.com/kazuki-oshino/rilll/internal/workflowctx"
)

// reflectorFilename は Reflector phase が `<runDir>/` 直下に書き出す JSON artifact 名
// (Req 8.3)。観測専用 (per-session counts / failures / dropped / skipped_provider) で、
// curator phase は読まない。
const reflectorFilename = "reflector.json"

// learningsArtifactFilename は Reflector phase が `<runDir>/` 直下に書き出す
// **curator phase 入力契約** の artifact 名 (Req 8.7)。Recorder accept 済みの全 learning を
// provenance (session_id / step_id / provider_id / akashic learnings.id / recorded_at) 付きで
// 集約し、決定的順序 (`(session_id ASC, intra-session index ASC)`) で並べる。
//
// 両端契約: skill SSoT (`.agents/skills/agent-learnings-curator/rules/curator.md`) が
// `<runDir>/learnings.json` を Read することを SoT 側で宣言し、`schema.go`
// `JSONSchemaForLearningsArtifact()` が Go 側 writer / skill 側 reader 双方を pin する。
const learningsArtifactFilename = "learnings.json"

// learningsArtifactSchemaVersion は learnings.json のスキーマバージョン (`schema.go`
// `JSONSchemaForLearningsArtifact()` の `const: 1` と SoT 一致)。
// 後方非互換変更でのみインクリメントし、両端契約テストが破綻するため必ず skill 側
// curator.md の fenced JSON も同期更新する。
const learningsArtifactSchemaVersion = 1

// reflectorSchemaVersion は reflector.json のスキーマバージョン。後方非互換変更でのみ
// インクリメントする (sessions.json と同形)。
const reflectorSchemaVersion = 1

// reflectorSkillSlash は Reflector phase 起動用の slash command (skill SSoT 引数 1)。
// `<runDir>` を空白区切りで結合して `agent.CallAgent` に渡す。
const reflectorSkillSlash = "/agent-learnings-curator"

// reflectorMode は skill SSoT の argument-hint 1 段目の値 (`<mode>`)。
const reflectorMode = "reflector"

// stepIDReflector は Reflector phase が `WithStep` で記録する agent_calls.step_id 値
// (Req 2.2 の closed set 4 値の 1 つ)。
const stepIDReflector = "reflector"

// stepNameReflector は同 step の人間可読名。
const stepNameReflector = "Reflector"

// SkipReasonEmptySession は AcceptedLearnings に行が無く空配列だった session の trace 値。
// session 単位 of `learnings: []` に対応する (Req 5.4 の有効応答経路)。
const SkipReasonEmptySession = "empty-learnings"

// reflectorResponse は Reflector schema の per-session 応答 (`{learnings: [...]}`).
type reflectorResponse struct {
	Learnings []reflectorLearning `json:"learnings"`
}

// reflectorLearning は reflector.json の `learnings[]` 1 要素 (LLM 出力スキーマ準拠)。
// JSON tag は skill SSoT の fenced schema および provider.OutputFormat 内 schema と一致。
type reflectorLearning struct {
	Category  string   `json:"category"`
	Context   string   `json:"context"`
	Insight   string   `json:"insight"`
	PathsGlob []string `json:"paths_glob"`
}

// ReflectorInput は Reflector の入力 (design.md L551-557 と整合)。
//
// 設計判断:
//   - `Sessions` は PreFetchResult.Sessions の素通し。Reflector が allowlist 判定 +
//     learning 行の step_id 転記で消費する。`UniqueSessionIDs` だけでも resume call の
//     最低限は満たせるが、provider 別 skip / step_id 転記のため SessionEntry を要求する。
//   - `WorkflowID` は `learnings.workflow_id` 列に書き戻す値。orchestration (task 8.1) が
//     parent workflow_run を Reader.Detail で引いて解決し、本 phase に渡す。
//   - `Skill` は通常 `"/agent-learnings-curator reflector <runDir>"` を構築済みで渡す。
//     test 経路では skill 不在の dummy 文字列を渡す (CallAgent stub が処理)。
type ReflectorInput struct {
	CWD         string
	ParentRunID string
	WorkflowID  string
	Sessions    []SessionEntry
	Skill       string
}

// ReflectorResult は Reflector の戻り値 (design.md L559-565 と整合)。
//
//   - ReflectorJSONPath: 書き出した `reflector.json` の絶対パス (artifact 登録用)。
//     reader/recorder nil 等の致命 error 経路では空文字で返る。
//   - AcceptedCount: `RecordLearning` で insert 成功した learning 数。
//   - DroppedCount: category enum 違反で drop した learning 数 (Req 9.5)。
//   - PerSessionErrors: best-effort で記録した per-session 失敗数 (Req 4.5, 9.4)。
//   - SkippedProviderCount: allowlist 外 provider で skip した session 数。
//   - AllFailed: 全 session が失敗 (resume call / parse / skip provider 含む) で
//     1 件も AcceptedLearning が出なかったとき true。上位が `verdict=resume-all-failed`
//     経路に切り替える判定材料 (Req 4.6)。
//   - NoCredentials: 1 件以上の session で provider 認証情報不在を検出したとき true。
//     上位が `verdict=applied-no-op` 経路に切り替える判定材料 (Req 9.2)。
//     (注: ErrNoCredentials sentinel は provider 層に存在しないため、本 phase は
//     error 文字列の heuristic match で検出する。誤検知を許容する保守的設計。)
type ReflectorResult struct {
	ReflectorJSONPath string
	// LearningsJSONPath は `<runDir>/learnings.json` の絶対パス。Curator phase の入力契約
	// (Req 8.7) として書き出された artifact の所在を上位 (orchestration) に伝える。
	// reader/recorder nil 等の致命 error 経路では空文字で返る。
	LearningsJSONPath    string
	AcceptedCount        int
	DroppedCount         int
	PerSessionErrors     int
	SkippedProviderCount int
	AllFailed            bool
	NoCredentials        bool
}

// SessionResult は reflector.json `per_session[]` の 1 要素 (debug trace 用)。
// session_id 別の accept / drop / failure 集計を保持する。
type SessionResult struct {
	SessionID     string `json:"session_id"`
	ProviderID    string `json:"provider_id,omitempty"`
	StepID        string `json:"step_id,omitempty"`
	AcceptedCount int    `json:"accepted_count"`
	DroppedCount  int    `json:"dropped_count"`
	// RawResponseBytes は LLM 応答の bytes 数 (debug 用)。本文は別 file 化しない方針で
	// reflector.json 単体で運用 / debug 完結を狙う。
	RawResponseBytes int `json:"raw_response_bytes,omitempty"`
}

// SessionFailure は reflector.json `per_session_failures[]` の 1 要素 (Req 4.5)。
type SessionFailure struct {
	SessionID  string `json:"session_id"`
	ProviderID string `json:"provider_id,omitempty"`
	Reason     string `json:"reason"`
	Detail     string `json:"detail,omitempty"`
}

// DroppedLearning は reflector.json `dropped[]` の 1 要素 (Req 9.5)。
// 本 phase は category enum 違反のみ drop するが、Reason は将来拡張余地として残す。
type DroppedLearning struct {
	SessionID string            `json:"session_id"`
	Reason    string            `json:"reason"`
	Learning  reflectorLearning `json:"learning"`
}

// SkippedProvider は reflector.json `skipped_provider[]` の 1 要素。
// allowlist 外 provider の session を本 phase で skip した経路を trace する。
type SkippedProvider struct {
	SessionID  string `json:"session_id"`
	ProviderID string `json:"provider_id"`
	Reason     string `json:"reason"`
}

// ReflectorArtifact は `reflector.json` の Go 構造体表現 (SSoT)。
type ReflectorArtifact struct {
	SchemaVersion      int               `json:"schema_version"`
	ParentRunID        string            `json:"parent_run_id"`
	GeneratedAt        time.Time         `json:"generated_at"`
	AllowedProviders   []string          `json:"allowed_providers"`
	PerSession         []SessionResult   `json:"per_session"`
	PerSessionFailures []SessionFailure  `json:"per_session_failures"`
	Dropped            []DroppedLearning `json:"dropped"`
	SkippedProvider    []SkippedProvider `json:"skipped_provider"`
}

// LearningEntry は `learnings.json.learnings[]` の 1 要素 (curator phase 入力契約)。
//
// 各 entry は LLM-emitted 4 fields (`category` / `context` / `insight` / `paths_glob`) と
// provenance 5 fields (`id` = akashic `learnings.id`, `session_id`, `step_id`,
// `provider_id`, `recorded_at`) を持つ。`id` を介して akashic SQLite と cross-reference
// 可能にすることで、curator が出力した operation の追跡可能性 (どの session の学びを
// 採用 / dedup したか) を保つ。
//
// JSON tag は schema.go `JSONSchemaForLearningsArtifact()` の field 名と完全一致。
// 両端契約テスト (`TestSkillContractIntegrity_LearningsArtifactSchema`) が
// skill SSoT 側 fenced JSON との一致を CI で pin する。
type LearningEntry struct {
	ID         string    `json:"id"`
	SessionID  string    `json:"session_id"`
	StepID     string    `json:"step_id"`
	ProviderID string    `json:"provider_id"`
	Category   string    `json:"category"`
	Context    string    `json:"context"`
	Insight    string    `json:"insight"`
	PathsGlob  []string  `json:"paths_glob"`
	RecordedAt time.Time `json:"recorded_at"`
}

// LearningsArtifact は `learnings.json` の Go 構造体表現 (SSoT)。
//
// 順序契約: `Learnings` は `(SessionID ASC, intra-session index ASC)` で sort 済み。
// 並列 goroutine 経由で集約された outcome を最終 sort することで、curator novelty 判定の
// 入力順序を決定的にする (= curator 出力の再現性確保 / dedup 判定の race を排除)。
type LearningsArtifact struct {
	SchemaVersion int             `json:"schema_version"`
	ParentRunID   string          `json:"parent_run_id"`
	GeneratedAt   time.Time       `json:"generated_at"`
	Learnings     []LearningEntry `json:"learnings"`
}

// failureReason 定数。closed set にはせず、debug 容易性のため文字列で残す。
const (
	failureReasonCallAgent     = "call-agent-failed"
	failureReasonParseJSON     = "parse-json-failed"
	failureReasonRecordLearn   = "record-learning-failed"
	failureReasonNoCredentials = "no-credentials"
)

// dropReasonInvalidCategory は dropped[].reason 値定数 (Req 9.5)。
const dropReasonInvalidCategory = "category-not-in-enum"

// Reflector は PreFetch が抽出した unique session_id 群に対して並列 resume call を
// 発行し、JSON 強制で learning を収集する (tasks.md 7.2 / design.md §Reflector)。
//
// 動作契約:
//   - reader/recorder nil → そのまま empty result + error は返さず ("最低限 artifact
//     は出す") の方針。recorder nil なら inserts は 0 件で `AllFailed` 経路となり、
//     上位が verdict=applied-no-op に切り替える。設計上は orchestration (task 8.1) が
//     reader / recorder の存在を保証するため、本 phase は防御的に degrade するのみ。
//   - in.Sessions が空 → 即座に空 artifact を書き出して return (`AllFailed=false` /
//     `AcceptedCount=0`)。orchestration は PreFetch result の `UniqueSessionIDs` 0 件で
//     先に applied-no-op 経路に切り替えるため、本経路は防御的 fallback。
//   - 全 session 失敗 → `AllFailed=true` (resume call 全 fail / parse 全 fail / 全 skip 含む)。
//   - ctx cancel → 進行中 goroutine は `Acquire(ctx)` で待機解除されて exit する。
//
// 注意: Recorder が nil の場合は learning insert を全件 skip し、SessionResult.AcceptedCount
// は 0 のまま。AllFailed 判定では「Recorder nil で insert 0 件」が「全失敗」になる懸念があるが、
// 本 phase は最低限の防御で、orchestration が recorder presence を上流で保証する設計。
func Reflector(
	ctx context.Context,
	runDir string,
	in ReflectorInput,
	agent workflowctx.AgentCaller,
	recorder akashic.Recorder,
	limiter *ParallelLimiter,
) (ReflectorResult, error) {
	allowedProviders := resolveProviderAllowlist(envRillLearningsProviders, defaultProviderAllowlist)

	artifact := ReflectorArtifact{
		SchemaVersion:      reflectorSchemaVersion,
		ParentRunID:        in.ParentRunID,
		GeneratedAt:        time.Now().UTC(),
		AllowedProviders:   providerAllowlistStrings(allowedProviders),
		PerSession:         []SessionResult{},
		PerSessionFailures: []SessionFailure{},
		Dropped:            []DroppedLearning{},
		SkippedProvider:    []SkippedProvider{},
	}

	// 早期 return: sessions 空の場合は空 artifact だけ書き出して終了する。
	// learnings.json も空 entry で書き出して curator 入力契約を保つ (Req 8.7)。
	if len(in.Sessions) == 0 {
		path, err := writeReflectorArtifact(runDir, artifact)
		if err != nil {
			return ReflectorResult{}, fmt.Errorf("learnings.Reflector: write reflector.json: %w", err)
		}
		emptyArtifact := LearningsArtifact{
			SchemaVersion: learningsArtifactSchemaVersion,
			ParentRunID:   in.ParentRunID,
			GeneratedAt:   time.Now().UTC(),
			Learnings:     []LearningEntry{},
		}
		learningsPath, err := writeLearningsArtifact(runDir, emptyArtifact)
		if err != nil {
			return ReflectorResult{}, fmt.Errorf("learnings.Reflector: write learnings.json: %w", err)
		}
		return ReflectorResult{ReflectorJSONPath: path, LearningsJSONPath: learningsPath}, nil
	}

	// allowlist filter で skipped_provider[] を先に確定する。
	allowedSet := providerAllowlistStringSet(allowedProviders)
	var eligible []SessionEntry
	for _, s := range in.Sessions {
		// ProviderID が空文字の session は防御的に allow (allowlist は provider が
		// 既知のときのみ有効化する)。本実装では allowlist が空文字を含めば accept
		// されるが、resolveProviderAllowlist が空文字を除外するため事実上 deny になる。
		// → 仕様としては「provider 未確定 session は資源を消費する LLM call を投げる」
		//    ではなく「skip して trace する」が安全側。
		if s.ProviderID == "" {
			artifact.SkippedProvider = append(artifact.SkippedProvider, SkippedProvider{
				SessionID:  s.SessionID,
				ProviderID: "",
				Reason:     "provider-id-empty",
			})
			continue
		}
		if _, ok := allowedSet[s.ProviderID]; !ok {
			artifact.SkippedProvider = append(artifact.SkippedProvider, SkippedProvider{
				SessionID:  s.SessionID,
				ProviderID: s.ProviderID,
				Reason:     SkipReasonProviderNotAllowed,
			})
			continue
		}
		eligible = append(eligible, s)
	}

	// limiter が nil で渡された場合は default で構築する (defensive)。
	if limiter == nil {
		limiter = NewLimiter(envRillLearningsMaxParallel, defaultMaxParallel)
	}

	// 並列発火 + 結果集約。channel で per-session 結果を吸い上げる方が
	// goroutine-safe で簡潔 (mutex を介すより明示的)。
	type sessionOutcome struct {
		result   SessionResult
		failures []SessionFailure
		dropped  []DroppedLearning
		// accepted は Recorder.RecordLearning が成功した learning を provenance 付きで保持する。
		// session 内 index 順 (LLM 応答配列の順) で並ぶ。aggregation 段で stable sort
		// (`SessionID ASC`) を掛けると intra-session 順序が保たれる (Req 8.7 決定的順序)。
		accepted []LearningEntry
		// noCredentials は本 session の resume call が provider auth 不在を検出したか。
		// 1 件以上の session で true なら ReflectorResult.NoCredentials を立てる。
		noCredentials bool
	}
	resultsCh := make(chan sessionOutcome, len(eligible))
	var wg sync.WaitGroup

	for _, sess := range eligible {
		wg.Add(1)
		go func(sess SessionEntry) {
			defer wg.Done()

			outcome := sessionOutcome{
				result: SessionResult{
					SessionID:  sess.SessionID,
					ProviderID: sess.ProviderID,
					StepID:     sess.StepID,
				},
			}

			// Acquire は ctx-aware。ctx cancel で枠取得失敗 → call せず終了。
			if err := limiter.Acquire(ctx); err != nil {
				outcome.failures = append(outcome.failures, SessionFailure{
					SessionID:  sess.SessionID,
					ProviderID: sess.ProviderID,
					Reason:     "ctx-cancelled",
					Detail:     err.Error(),
				})
				resultsCh <- outcome
				return
			}
			defer limiter.Release()

			prompt := buildReflectorPrompt(in.Skill, runDir)
			opts := workflow.WithStep(&workflowctx.CallAgentOptions{
				OutputFormat: JSONSchemaForReflector(),
			}, stepIDReflector, stepNameReflector)

			resp, err := agent.CallAgent(ctx, prompt, in.CWD, nil, sess.SessionID, opts)
			if err != nil {
				outcome.failures = append(outcome.failures, SessionFailure{
					SessionID:  sess.SessionID,
					ProviderID: sess.ProviderID,
					Reason:     failureReasonCallAgent,
					Detail:     err.Error(),
				})
				if isErrNoCredentials(err) {
					outcome.failures[len(outcome.failures)-1].Reason = failureReasonNoCredentials
					outcome.noCredentials = true
				}
				resultsCh <- outcome
				return
			}

			var parsed reflectorResponse
			rawJSON, ok := extractAgentResponseJSON(resp, &parsed)
			if !ok {
				outcome.failures = append(outcome.failures, SessionFailure{
					SessionID:  sess.SessionID,
					ProviderID: sess.ProviderID,
					Reason:     failureReasonParseJSON,
					Detail:     truncateForArtifact(resp.Content, 1024),
				})
				resultsCh <- outcome
				return
			}
			outcome.result.RawResponseBytes = len(rawJSON)

			// 各 learning に対し category enum 検証 → RecordLearning。
			// recorder nil ならば insert を skip するが、accept とは別経路で trace せず、
			// orchestration が recorder presence を保証する前提で防御に徹する。
			anchorAgentCallID := ""
			if len(sess.AgentCallIDs) > 0 {
				anchorAgentCallID = sess.AgentCallIDs[0]
			}
			recordedAt := time.Now().UTC()
			for _, l := range parsed.Learnings {
				if !ValidCategory(l.Category) {
					outcome.dropped = append(outcome.dropped, DroppedLearning{
						SessionID: sess.SessionID,
						Reason:    dropReasonInvalidCategory,
						Learning:  l,
					})
					outcome.result.DroppedCount++
					continue
				}
				if recorder == nil {
					// 防御: recorder 未注入時は insert を skip するが accept にも数えない。
					// orchestration (task 8.1) が recorder を必ず注入する設計のため、
					// ここに到達するのは test 経路の dummy stub 時のみ。
					continue
				}
				row := akashic.LearningRow{
					ID:            newLearningID(),
					AgentCallID:   anchorAgentCallID,
					WorkflowRunID: in.ParentRunID,
					WorkflowID:    in.WorkflowID,
					StepID:        sess.StepID,
					Category:      l.Category,
					Context:       l.Context,
					Insight:       l.Insight,
					PathsGlob:     l.PathsGlob,
					Source:        akashic.SourceAgentSelfReport,
					RecordedAt:    recordedAt,
				}
				if err := recorder.RecordLearning(ctx, row); err != nil {
					outcome.failures = append(outcome.failures, SessionFailure{
						SessionID:  sess.SessionID,
						ProviderID: sess.ProviderID,
						Reason:     failureReasonRecordLearn,
						Detail:     err.Error(),
					})
					continue
				}
				outcome.result.AcceptedCount++
				// curator 入力契約 (learnings.json) 用の provenance 付き entry を蓄積する。
				// `id` は akashic learnings.id と一致 (cross-reference 保証 / Req 8.7)。
				outcome.accepted = append(outcome.accepted, LearningEntry{
					ID:         row.ID,
					SessionID:  sess.SessionID,
					StepID:     sess.StepID,
					ProviderID: sess.ProviderID,
					Category:   l.Category,
					Context:    l.Context,
					Insight:    l.Insight,
					PathsGlob:  append([]string(nil), l.PathsGlob...),
					RecordedAt: recordedAt,
				})
			}

			resultsCh <- outcome
		}(sess)
	}

	// すべて完了したら channel を close (goroutine-safe な fan-in 終了合図)。
	go func() {
		wg.Wait()
		close(resultsCh)
	}()

	// 集約。channel close でループ終了。
	totalAccepted := 0
	totalDropped := len(artifact.Dropped)
	totalFailures := 0
	noCredentials := false
	var allAccepted []LearningEntry
	for outcome := range resultsCh {
		artifact.PerSession = append(artifact.PerSession, outcome.result)
		artifact.PerSessionFailures = append(artifact.PerSessionFailures, outcome.failures...)
		artifact.Dropped = append(artifact.Dropped, outcome.dropped...)
		totalAccepted += outcome.result.AcceptedCount
		totalDropped += outcome.result.DroppedCount
		totalFailures += len(outcome.failures)
		if outcome.noCredentials {
			noCredentials = true
		}
		allAccepted = append(allAccepted, outcome.accepted...)
	}

	// 決定的順序へ sort (Req 8.7): SessionID ASC で stable sort し intra-session 順を保つ。
	// 並列 goroutine の channel receive 順は非決定的なので、curator の novelty 判定が
	// race を踏まないよう writer 側で順序を確定させる。
	sort.SliceStable(allAccepted, func(i, j int) bool {
		return allAccepted[i].SessionID < allAccepted[j].SessionID
	})

	path, err := writeReflectorArtifact(runDir, artifact)
	if err != nil {
		return ReflectorResult{}, fmt.Errorf("learnings.Reflector: write reflector.json: %w", err)
	}

	// curator 入力契約 (learnings.json) を書き出す。本 run の accept 件数が 0 でも
	// 空配列で書く (Curator phase が file 不在で fail しないため / Req 8.7)。
	learningsArtifact := LearningsArtifact{
		SchemaVersion: learningsArtifactSchemaVersion,
		ParentRunID:   in.ParentRunID,
		GeneratedAt:   time.Now().UTC(),
		Learnings:     allAccepted,
	}
	if learningsArtifact.Learnings == nil {
		learningsArtifact.Learnings = []LearningEntry{}
	}
	learningsPath, err := writeLearningsArtifact(runDir, learningsArtifact)
	if err != nil {
		return ReflectorResult{}, fmt.Errorf("learnings.Reflector: write learnings.json: %w", err)
	}

	// AllFailed: 1 件も accept されず、かつ何らかの failure / skip / drop が起きた場合。
	// in.Sessions が空のときは早期 return しているので、ここでは len(in.Sessions) > 0。
	allFailed := totalAccepted == 0 && len(in.Sessions) > 0

	return ReflectorResult{
		ReflectorJSONPath:    path,
		LearningsJSONPath:    learningsPath,
		AcceptedCount:        totalAccepted,
		DroppedCount:         totalDropped,
		PerSessionErrors:     totalFailures,
		SkippedProviderCount: len(artifact.SkippedProvider),
		AllFailed:            allFailed,
		NoCredentials:        noCredentials,
	}, nil
}

// writeReflectorArtifact は ReflectorArtifact を `<runDir>/reflector.json` に
// atomic write する。prefetch.go の writeJSONAtomic を再利用 (同 package private)。
func writeReflectorArtifact(runDir string, artifact ReflectorArtifact) (string, error) {
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir runDir %q: %w", runDir, err)
	}
	path := filepath.Join(runDir, reflectorFilename)
	if err := writeJSONAtomic(path, artifact); err != nil {
		return "", err
	}
	return path, nil
}

// writeLearningsArtifact は LearningsArtifact を `<runDir>/learnings.json` に atomic write する。
// curator phase の入力契約 (Req 8.7) として下流が file 不在を仮定しないことを保証する。
func writeLearningsArtifact(runDir string, artifact LearningsArtifact) (string, error) {
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir runDir %q: %w", runDir, err)
	}
	path := filepath.Join(runDir, learningsArtifactFilename)
	if err := writeJSONAtomic(path, artifact); err != nil {
		return "", err
	}
	return path, nil
}

// buildReflectorPrompt は skill SSoT 起動用の slash command 文字列を構築する。
// 通常 in.Skill が空文字なら `"/agent-learnings-curator reflector <runDir>"` を組み立てる。
// in.Skill に明示的な値が渡された場合はそれを優先する (test 経路で stub する用途)。
func buildReflectorPrompt(skill, runDir string) string {
	if strings.TrimSpace(skill) != "" {
		return skill
	}
	return fmt.Sprintf("%s %s %s", reflectorSkillSlash, reflectorMode, runDir)
}

// isErrNoCredentials は err が provider 認証情報不在に該当するかを文字列 heuristic で
// 判定する。provider 抽象化層に sentinel error が存在しないため、CLI / SDK が返す
// 共通的な credential 不在メッセージのキーワードを部分一致でチェックする。
//
// 過検知より「拾えなかった」が安全側 (検知時は applied-no-op に降格するため、誤検知で
// 学習が消えるのを避ける)。
func isErrNoCredentials(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	keywords := []string{
		"no credentials",
		"no_credentials",
		"errnocredentials",
		"missing api key",
		"api_key not set",
		"api key not set",
		"please run.*login",
		"unauthorized",
		"not logged in",
	}
	for _, kw := range keywords {
		if strings.Contains(msg, kw) {
			return true
		}
	}
	return false
}

// newLearningID は learnings.id 用の 16 byte hex (32 char) 識別子を生成する純関数。
// crypto/rand を使用し UUID 互換ライブラリを追加せずに済ませる (CGO 非依存維持)。
func newLearningID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// rand.Read 失敗時は時刻 fallback (実運用では到達しない)。
		return fmt.Sprintf("learning-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// truncateForArtifact は debug 用 detail 文字列を上限 chars に切り詰める。
// 切り詰めた場合は末尾に切詰メッセージを足し、reflector.json で human readable に保つ。
func truncateForArtifact(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	return s[:max] + fmt.Sprintf("... (%d bytes truncated)", len(s)-max)
}

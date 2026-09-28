// Package workflow の artifacts.go は、ワークフロー実装者が中間成果物を
// 1 行で akashic レコードに登録できる薄い入口 RecordRunArtifact を提供する
// (akashic-artifacts spec design.md "workflow.RecordRunArtifact (新ヘルパ)")。
//
// 設計責務:
//   - 固定された処理順序 (kind 検証 → RunMetadata 解決 → Recorder 解決 → path
//     正規化 → os.Stat → MIME 検出 → Recorder.RecordRunArtifact) の "1 行入口"
//   - 失敗時は呼び出し側が `_ = workflow.RecordRunArtifact(...)` で握りつぶせる
//     ように error を返すのみ (Req 2.6 の伝播禁止は呼び出し側責務)
//   - Recorder 解決後の失敗 (path 正規化 / stat / Record そのもの) は best-effort
//     で raw_events に "artifact_record_failed" warning を残す (Req 2.6 の
//     "アカシックレコードのログ層に記録" を Recorder 利用可能経路で満たす)
//   - Recorder 未解決 (ErrMissingRecorder) や kind 不正の段階では raw_events に
//     残せないため、呼び出し側 (テスト / 統合経路) が error 値を観測する責務を
//     持つ (design.md "ErrMissingRecorder 時のログ fallback 方針")
package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"path/filepath"

	"github.com/kazuki-oshino/rilll/internal/akashic"
	"github.com/kazuki-oshino/rilll/internal/workflowctx"
)

// ErrMissingRunMetadata は ctx に RunMetadata が乗っていない、もしくは
// WorkflowRunID が空文字列のときに RecordRunArtifact が返す sentinel。
//
// 典型的な発生原因はプログラミングエラー (Definition.Run 経路外からヘルパを
// 直接呼んだ等)。呼び出し側はテスト経由で `errors.Is(err, ErrMissingRunMetadata)`
// で判別する。
var ErrMissingRunMetadata = errors.New("workflow: ctx is missing RunMetadata")

// ErrMissingRecorder は ctx に akashic.Recorder が乗っていない (key 未注入 /
// nil 注入のいずれも) ときに RecordRunArtifact が返す sentinel。
//
// design.md "Definition.Run は d.Recorder == nil でも ContextWithRecorder を
// 常に呼ぶ" 不変式により、本番経路ではこの error は発生しない (テスト経路でのみ
// 観測される)。Recorder 不在では raw_events 経路も使えないため、呼び出し側が
// error 値を観測する以外の fallback はない。
var ErrMissingRecorder = errors.New("workflow: ctx is missing akashic Recorder")

// rawEventArtifactRecordFailedType は raw_events に warning を記録するときの Type。
// design.md "Monitoring" セクションの規約値で、Subtype には kind を入れる。
const rawEventArtifactRecordFailedType = "artifact_record_failed"

// artifactRecordFailedPayload は artifact_record_failed warning の PayloadJSON 形。
// {error, path} の 2 フィールド固定 (Req 2.6 の "失敗内容を観測可能にする" 最小情報)。
type artifactRecordFailedPayload struct {
	Error string `json:"error"`
	Path  string `json:"path"`
}

// RecordRunArtifact はワークフロー実装者が中間成果物 1 件を akashic レコードに
// 登録するヘルパ (akashic-artifacts spec Req 1.1〜1.5, 1.7, 2.6)。
//
// 処理順序 (厳守):
//  1. akashic.ValidateArtifactKind(kind) - closed set 4 値でなければ
//     ErrUnknownArtifactKind を返す (Req 1.7)
//  2. RunMetadataFromContext(ctx) - 失敗 / WorkflowRunID 空文字なら
//     ErrMissingRunMetadata を返す (Req 1.4)
//  3. recorderFromContext(ctx) - 未注入 / nil 注入は ErrMissingRecorder を返す
//     (raw_events fallback / stderr fallback はいずれも行わない)
//  4. akashic.NormalizeArtifactPath(metadata.CWD, path) - .rilll ルート配下の
//     絶対 clean path に正規化 (失敗: ErrArtifactOutsideRillRoot /
//     ErrArtifactFileMissing)
//  5. os.Stat - サイズと mtime (UTC) を取得。target 不在 → ErrArtifactFileMissing
//  6. MIME 検出 - mime.TypeByExtension → http.DetectContentType (先頭 512 byte)
//     → 空文字列の優先順位
//  7. recorder.RecordRunArtifact - 上記で組んだ akashic.RunArtifact を upsert
//
// Recorder が解決済み (step 3 通過後) のあらゆる失敗は、best-effort で
// raw_events に "artifact_record_failed" warning を残す (raw_events 自体の発行
// 失敗は握りつぶす)。
//
// 失敗は error で返し、呼び出し側 (既存ワークフロー) が `_ = RecordRunArtifact(...)`
// で握りつぶすことで Req 2.6 (登録失敗をワークフロー結果に伝播させない) を満たす。
func RecordRunArtifact(ctx context.Context, kind string, path string) error {
	// 1. kind 検証 (recorder 解決前なので raw_events は発行不可能)
	if err := akashic.ValidateArtifactKind(kind); err != nil {
		return err
	}

	// 2. RunMetadata 解決 (cwd と workflow_run_id を ctx 経由で取得)
	metadata, ok := workflowctx.RunMetadataFromContext(ctx)
	if !ok || metadata.WorkflowRunID == "" {
		return fmt.Errorf("workflow: RecordRunArtifact: ctx に RunMetadata が無い: %w", ErrMissingRunMetadata)
	}

	// 3. Recorder 解決 (key 未注入 / nil 注入はいずれも ErrMissingRecorder)
	recorder, ok := recorderFromContext(ctx)
	if !ok {
		return fmt.Errorf("workflow: RecordRunArtifact: ctx に akashic Recorder が無い: %w", ErrMissingRecorder)
	}

	// 4. path 正規化 (.rilll ルート配下の絶対 clean path に揃える)
	normalized, err := akashic.NormalizeArtifactPath(metadata.CWD, path)
	if err != nil {
		emitArtifactRecordFailed(ctx, recorder, kind, path, err)
		return err
	}

	// 5. os.Stat (サイズと mtime を観測)
	info, err := os.Stat(normalized)
	if err != nil {
		// EvalSymlinks 後に削除されたケース (race) のフォールバック。
		// fs.ErrNotExist は ErrArtifactFileMissing で wrap し直して
		// 呼び出し側が errors.Is で同じ sentinel として観測できるようにする。
		if errors.Is(err, fs.ErrNotExist) {
			err = fmt.Errorf("workflow: RecordRunArtifact: stat 失敗 (%s): %w",
				normalized, akashic.ErrArtifactFileMissing)
		} else {
			err = fmt.Errorf("workflow: RecordRunArtifact: stat 失敗 (%s): %w", normalized, err)
		}
		emitArtifactRecordFailed(ctx, recorder, kind, normalized, err)
		return err
	}

	// 6. MIME 検出 (失敗時は空文字列。空文字列は SQLite 層で NULL として保存される)
	mimeType := detectArtifactMIME(normalized)

	// 7. Recorder.RecordRunArtifact (SQLite 層への upsert を委譲)
	if err := recorder.RecordRunArtifact(ctx, akashic.RunArtifact{
		WorkflowRunID: metadata.WorkflowRunID,
		Kind:          kind,
		Path:          normalized,
		SizeBytes:     info.Size(),
		Mtime:         info.ModTime().UTC(),
		MIME:          mimeType,
	}); err != nil {
		wrapped := fmt.Errorf("workflow: RecordRunArtifact: Recorder への upsert 失敗 (%s): %w",
			normalized, err)
		emitArtifactRecordFailed(ctx, recorder, kind, normalized, wrapped)
		return wrapped
	}
	return nil
}

// detectArtifactMIME は normalized (絶対 clean path) のファイルから MIME 型を検出する。
//
// 検出順序:
//  1. mime.TypeByExtension(filepath.Ext(path)) - 拡張子で確定できる場合は最優先
//  2. http.DetectContentType - ファイル先頭 512 byte のスニッフィング
//  3. 空文字列 - 上記いずれも有意な型を返さない場合 (DB 上は NULL となる)
//
// 注意:
//   - http.DetectContentType は判定不能時に "application/octet-stream" を返すが、
//     本ヘルパでは「拡張子も内容も判定不能」と等価扱いし空文字列に丸める
//     (Req 1.1 の "mime" カラムは "検出失敗時は空文字列" 規約)
//   - ファイル open / 読み込み失敗は MIME 検出失敗として吸収し空文字列を返す
//     (この段階で error にしない: 既に os.Stat は通っている前提で、MIME 検出は
//     best-effort の補助情報のため)
func detectArtifactMIME(absPath string) string {
	if ext := filepath.Ext(absPath); ext != "" {
		if t := mime.TypeByExtension(ext); t != "" {
			return t
		}
	}
	f, err := os.Open(absPath)
	if err != nil {
		return ""
	}
	defer f.Close()
	buf := make([]byte, 512)
	n, _ := f.Read(buf)
	if n == 0 {
		return ""
	}
	t := http.DetectContentType(buf[:n])
	if t == "" || t == "application/octet-stream" {
		return ""
	}
	return t
}

// emitArtifactRecordFailed は recorder が解決済みの失敗経路で raw_events に
// "artifact_record_failed" warning を 1 件 best-effort で発行する (Req 2.6)。
//
//   - PayloadJSON は {error, path} の 2 フィールド固定 (artifactRecordFailedPayload)
//   - JSON 化に失敗した場合は空 payload を返し、warning 自体は記録経路に流す
//   - recorder.RecordRawEvent の戻り error は握りつぶす (failure of failure logging
//     を許容する best-effort 経路)
//   - recorder == nil の防御 (本来は recorderFromContext で弾かれているが、
//     呼び出し側の安全網として nil チェックも保持する)
func emitArtifactRecordFailed(ctx context.Context, recorder akashic.Recorder, kind, path string, cause error) {
	if recorder == nil {
		return
	}
	payload, jerr := json.Marshal(artifactRecordFailedPayload{
		Error: cause.Error(),
		Path:  path,
	})
	if jerr != nil {
		// JSON 化失敗時もログ自体は残す。空 JSON を最低限の互換 payload として送る。
		payload = []byte("{}")
	}
	_ = recorder.RecordRawEvent(ctx, akashic.RawEvent{
		Type:        rawEventArtifactRecordFailedType,
		Subtype:     kind,
		PayloadJSON: payload,
	})
}

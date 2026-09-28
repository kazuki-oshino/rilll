package workflow

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/kazuki-oshino/rilll/internal/akashic"
	"github.com/kazuki-oshino/rilll/internal/workflowctx"
)

// artifactFakeRecorder は workflow.RecordRunArtifact のテスト用 Recorder。
// RecordRunArtifact / RecordRawEvent への呼び出しを蓄積し、ヘルパが Req 1.1〜1.5,
// 1.7, 2.6 の振る舞い (送信内容と raw_events 経路) を満たすかを観測する。
type artifactFakeRecorder struct {
	artifacts []akashic.RunArtifact
	rawEvents []akashic.RawEvent
}

func (r *artifactFakeRecorder) RecordWorkflowRunStart(context.Context, akashic.WorkflowRun) error {
	return nil
}
func (r *artifactFakeRecorder) RecordWorkflowRunFinish(context.Context, akashic.WorkflowRunFinish) error {
	return nil
}
func (r *artifactFakeRecorder) RecordAgentCallStart(context.Context, akashic.AgentCall) error {
	return nil
}
func (r *artifactFakeRecorder) RecordAgentCallFinish(context.Context, akashic.AgentCallResult) error {
	return nil
}
func (r *artifactFakeRecorder) RecordRawEvent(_ context.Context, e akashic.RawEvent) error {
	r.rawEvents = append(r.rawEvents, e)
	return nil
}
func (r *artifactFakeRecorder) RecordRunArtifact(_ context.Context, a akashic.RunArtifact) error {
	r.artifacts = append(r.artifacts, a)
	return nil
}

// RecordUserCorrection は memory-distiller-trigger-policy spec Task 3.3 で追加した
// Recorder interface 拡張への追従 (no-op)。本テストは Artifact 経路の検証が責務で
// user-correction 経路は対象外のため呼び出し履歴は保持しない。
func (r *artifactFakeRecorder) RecordUserCorrection(context.Context, string) error {
	return nil
}

// RecordLearning は agent-learnings spec (task 3.2) で Recorder interface に追加された
// メソッドへの no-op 追従。本テストは Artifact 経路の検証が責務で learnings 経路は
// 対象外のため呼び出し履歴は保持しない。
func (r *artifactFakeRecorder) RecordLearning(context.Context, akashic.LearningRow) error {
	return nil
}

func (r *artifactFakeRecorder) Close() error { return nil }

// setupRillArtifactFile は cwd/.rilll/<workflow>/<runID>/<file> を本物の
// I/O で配置し、戻り値の absPath は登録経路に渡せる絶対パス。
func setupRillArtifactFile(t *testing.T, runID, fileName, body string) (cwd, absPath string) {
	t.Helper()
	cwd = t.TempDir()
	dir := filepath.Join(cwd, ".rilll", "git-commit", runID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", dir, err)
	}
	absPath = filepath.Join(dir, fileName)
	if err := os.WriteFile(absPath, []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile(%s): %v", absPath, err)
	}
	return cwd, absPath
}

// newArtifactCtx は RunMetadata と (任意で) Recorder を ctx に注入する。
// withRecorder=false のときは recorderKey 未注入の ctx を返す (Req 1.4 のテスト用)。
func newArtifactCtx(t *testing.T, cwd, runID string, recorder akashic.Recorder, withRecorder bool) context.Context {
	t.Helper()
	ctx := context.Background()
	ctx = workflowctx.ContextWithRunMetadata(ctx, workflowctx.RunMetadata{
		WorkflowRunID: runID,
		WorkflowID:    "git-commit",
		WorkflowName:  "git commit",
		CWD:           cwd,
	})
	if withRecorder {
		ctx = ContextWithRecorder(ctx, recorder)
	}
	return ctx
}

// canonicalPath は EvalSymlinks 適用後の絶対 clean path を返すテストヘルパ。
// 登録経路 (NormalizeArtifactPath) と同じ正規化ルールに揃え、macOS の
// /var → /private/var symlink chain を吸収する。
func canonicalPath(t *testing.T, p string) string {
	t.Helper()
	abs, err := filepath.Abs(p)
	if err != nil {
		t.Fatalf("filepath.Abs(%s): %v", p, err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		t.Fatalf("filepath.EvalSymlinks(%s): %v", abs, err)
	}
	return filepath.Clean(resolved)
}

// (a) 正常登録: recorder が RunArtifact{Kind, Path} を 1 件受信し戻り値 nil、
// raw_events warning が 0 件であることを確認する (Req 1.1, 1.2, 2.6)。
func TestRecordRunArtifact_Success(t *testing.T) {
	cwd, absPath := setupRillArtifactFile(t, "run-aaa", "result.json", `{"ok":true}`)
	rec := &artifactFakeRecorder{}
	ctx := newArtifactCtx(t, cwd, "run-aaa", rec, true)

	if err := RecordRunArtifact(ctx, akashic.ArtifactKindResult, absPath); err != nil {
		t.Fatalf("RecordRunArtifact: %v", err)
	}
	if len(rec.artifacts) != 1 {
		t.Fatalf("recorder.RunArtifact 受信件数=%d, want 1 (artifacts=%#v)", len(rec.artifacts), rec.artifacts)
	}
	got := rec.artifacts[0]
	if got.WorkflowRunID != "run-aaa" {
		t.Fatalf("WorkflowRunID=%q, want run-aaa", got.WorkflowRunID)
	}
	if got.Kind != akashic.ArtifactKindResult {
		t.Fatalf("Kind=%q, want %q", got.Kind, akashic.ArtifactKindResult)
	}
	wantPath := canonicalPath(t, absPath)
	if got.Path != wantPath {
		t.Fatalf("Path=%q, want %q (EvalSymlinks 済み絶対 clean path)", got.Path, wantPath)
	}
	if got.SizeBytes <= 0 {
		t.Fatalf("SizeBytes=%d, want > 0", got.SizeBytes)
	}
	if got.Mtime.IsZero() {
		t.Fatalf("Mtime is zero, want non-zero UTC")
	}
	if got.Mtime.Location().String() != "UTC" {
		t.Fatalf("Mtime location=%s, want UTC", got.Mtime.Location())
	}
	// MIME は `.json` 拡張子からの検出が期待されるが、システム依存のため空でないことだけ確認する。
	if got.MIME == "" {
		t.Fatalf("MIME is empty, want detected from .json extension or content sniff")
	}
	if len(rec.rawEvents) != 0 {
		t.Fatalf("rawEvents=%d, want 0 (正常系では artifact_record_failed を発行しない)", len(rec.rawEvents))
	}
}

// (b) ctx に RunMetadata が無いとき ErrMissingRunMetadata を返し、
// recorder への RecordRunArtifact 呼び出しが 0 件であることを確認する (Req 1.4)。
func TestRecordRunArtifact_MissingRunMetadata(t *testing.T) {
	rec := &artifactFakeRecorder{}
	ctx := ContextWithRecorder(context.Background(), rec)

	err := RecordRunArtifact(ctx, akashic.ArtifactKindResult, "/tmp/whatever")
	if !errors.Is(err, ErrMissingRunMetadata) {
		t.Fatalf("err=%v, want errors.Is(ErrMissingRunMetadata)", err)
	}
	if len(rec.artifacts) != 0 {
		t.Fatalf("recorder.RunArtifact 呼び出し件数=%d, want 0", len(rec.artifacts))
	}
	// recorder 解決前の失敗のため raw_events も発行しない。
	if len(rec.rawEvents) != 0 {
		t.Fatalf("rawEvents=%d, want 0 (recorder 解決前の失敗)", len(rec.rawEvents))
	}
}

// (c) Recorder key 未注入: ErrMissingRecorder を返す (Req 1.4 解釈)。
func TestRecordRunArtifact_MissingRecorderKey(t *testing.T) {
	ctx := workflowctx.ContextWithRunMetadata(context.Background(), workflowctx.RunMetadata{
		WorkflowRunID: "run",
		CWD:           t.TempDir(),
	})

	err := RecordRunArtifact(ctx, akashic.ArtifactKindResult, "/tmp/whatever")
	if !errors.Is(err, ErrMissingRecorder) {
		t.Fatalf("err=%v, want errors.Is(ErrMissingRecorder)", err)
	}
}

// (c') Recorder に nil を注入したケースも ErrMissingRecorder で同じ扱い
// (design.md "未注入 / nil 注入 を一意の (nil, false) シグナル化" 不変式)。
func TestRecordRunArtifact_NilRecorderInjected(t *testing.T) {
	ctx := workflowctx.ContextWithRunMetadata(context.Background(), workflowctx.RunMetadata{
		WorkflowRunID: "run",
		CWD:           t.TempDir(),
	})
	ctx = ContextWithRecorder(ctx, nil)

	err := RecordRunArtifact(ctx, akashic.ArtifactKindResult, "/tmp/whatever")
	if !errors.Is(err, ErrMissingRecorder) {
		t.Fatalf("err=%v, want errors.Is(ErrMissingRecorder)", err)
	}
}

// (d) 不正 kind は ErrUnknownArtifactKind を返し、recorder への呼び出しは 0 件
// (kind 検証は recorder 解決前ステップなので raw_events も発行されない、Req 1.7)。
func TestRecordRunArtifact_UnknownKind(t *testing.T) {
	cwd, absPath := setupRillArtifactFile(t, "run-x", "result.json", "{}")
	rec := &artifactFakeRecorder{}
	ctx := newArtifactCtx(t, cwd, "run-x", rec, true)

	err := RecordRunArtifact(ctx, "definitely_not_a_kind", absPath)
	if !errors.Is(err, akashic.ErrUnknownArtifactKind) {
		t.Fatalf("err=%v, want errors.Is(akashic.ErrUnknownArtifactKind)", err)
	}
	if len(rec.artifacts) != 0 {
		t.Fatalf("recorder.RunArtifact=%d, want 0", len(rec.artifacts))
	}
	// kind 検証は step 1 (recorder 解決前) のため raw_events は発行しない。
	if len(rec.rawEvents) != 0 {
		t.Fatalf("rawEvents=%d, want 0", len(rec.rawEvents))
	}
}

// (e) `.rilll` 外の path は ErrArtifactOutsideRillRoot を返す。recorder は
// 解決済みなので raw_events に "artifact_record_failed" warning が 1 件残る (Req 1.5, 2.6)。
func TestRecordRunArtifact_OutsideRillRoot(t *testing.T) {
	cwd := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cwd, ".rilll"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.rilll): %v", err)
	}
	outsideDir := t.TempDir()
	outsideFile := filepath.Join(outsideDir, "evil.txt")
	if err := os.WriteFile(outsideFile, []byte("pwn"), 0o644); err != nil {
		t.Fatalf("WriteFile(outsideFile): %v", err)
	}
	rec := &artifactFakeRecorder{}
	ctx := newArtifactCtx(t, cwd, "run-out", rec, true)

	err := RecordRunArtifact(ctx, akashic.ArtifactKindResult, outsideFile)
	if !errors.Is(err, akashic.ErrArtifactOutsideRillRoot) {
		t.Fatalf("err=%v, want errors.Is(akashic.ErrArtifactOutsideRillRoot)", err)
	}
	if len(rec.artifacts) != 0 {
		t.Fatalf("recorder.RunArtifact=%d, want 0", len(rec.artifacts))
	}
	if len(rec.rawEvents) != 1 {
		t.Fatalf("rawEvents=%d, want 1 (artifact_record_failed)", len(rec.rawEvents))
	}
	ev := rec.rawEvents[0]
	if ev.Type != "artifact_record_failed" {
		t.Fatalf("rawEvent.Type=%q, want artifact_record_failed", ev.Type)
	}
	if ev.Subtype != akashic.ArtifactKindResult {
		t.Fatalf("rawEvent.Subtype=%q, want %q", ev.Subtype, akashic.ArtifactKindResult)
	}
	if len(ev.PayloadJSON) == 0 {
		t.Fatalf("rawEvent.PayloadJSON is empty, want JSON {error, path}")
	}
}

// (f) ファイル不在は ErrArtifactFileMissing を返し fs.ErrNotExist 互換でラップされる。
// 同時に raw_events に "artifact_record_failed" warning が 1 件残る (Req 1.3, 2.6)。
func TestRecordRunArtifact_FileMissing(t *testing.T) {
	cwd := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cwd, ".rilll", "git-commit", "run"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	missing := filepath.Join(cwd, ".rilll", "git-commit", "run", "missing.json")
	rec := &artifactFakeRecorder{}
	ctx := newArtifactCtx(t, cwd, "run", rec, true)

	err := RecordRunArtifact(ctx, akashic.ArtifactKindResult, missing)
	if !errors.Is(err, akashic.ErrArtifactFileMissing) {
		t.Fatalf("err=%v, want errors.Is(akashic.ErrArtifactFileMissing)", err)
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("err=%v, want errors.Is(fs.ErrNotExist) (ErrArtifactFileMissing wraps fs.ErrNotExist)", err)
	}
	if len(rec.artifacts) != 0 {
		t.Fatalf("recorder.RunArtifact=%d, want 0", len(rec.artifacts))
	}
	if len(rec.rawEvents) != 1 {
		t.Fatalf("rawEvents=%d, want 1 (artifact_record_failed)", len(rec.rawEvents))
	}
	ev := rec.rawEvents[0]
	if ev.Type != "artifact_record_failed" {
		t.Fatalf("rawEvent.Type=%q, want artifact_record_failed", ev.Type)
	}
	if ev.Subtype != akashic.ArtifactKindResult {
		t.Fatalf("rawEvent.Subtype=%q, want %q", ev.Subtype, akashic.ArtifactKindResult)
	}
}

// (g) 同じ (workflow_run_id, path) で 2 回呼び出すと、ヘルパは 2 回 recorder に
// upsert を発行するが、SQLite テーブル上は ON CONFLICT DO UPDATE で 1 行に収まる
// (Req 1.6 冪等)。本テストでは実 SQLiteRecorder を経由して観測する。
func TestRecordRunArtifact_IdempotentSQLiteUpsert(t *testing.T) {
	cwd, absPath := setupRillArtifactFile(t, "run-idem", "result.json", `{"v":1}`)
	dbPath := filepath.Join(t.TempDir(), "akashic.sqlite")
	rec, err := akashic.OpenSQLite(dbPath)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer rec.Close()
	// SQLiteRecorder.RecordWorkflowRunStart で workflow_runs 行を作っておくと
	// 後続テストで参考にしやすい (run_artifacts 自体は FK 強制ではないため必須ではない)。
	ctx := newArtifactCtx(t, cwd, "run-idem", rec, true)

	if err := RecordRunArtifact(ctx, akashic.ArtifactKindResult, absPath); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if err := RecordRunArtifact(ctx, akashic.ArtifactKindResult, absPath); err != nil {
		t.Fatalf("second call: %v", err)
	}

	rows, err := rec.GetArtifacts(ctx, "run-idem")
	if err != nil {
		t.Fatalf("GetArtifacts: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("len(GetArtifacts rows)=%d, want 1 (SQLite 上は冪等 upsert)", len(rows))
	}
	if rows[0].Kind != akashic.ArtifactKindResult {
		t.Fatalf("rows[0].Kind=%q, want %q", rows[0].Kind, akashic.ArtifactKindResult)
	}
	wantPath := canonicalPath(t, absPath)
	if rows[0].Path != wantPath {
		t.Fatalf("rows[0].Path=%q, want %q", rows[0].Path, wantPath)
	}
}

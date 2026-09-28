package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kazuki-oshino/rilll/internal/akashic"
)

// fakeReader は runAkashicExport の RED → GREEN を駆動するテスト用 Reader 実装である。
//
// listResult / listErr フィールドで挙動を制御し、listCalls で渡された ListOptions を
// 観測する。task 4.6 の golden test では本 fake をベースに固定 fixture を設定する想定で
// あり、3.1 時点では smoke レベルのカバレッジに留める (フラグ → ListOptions マッピング /
// JSON 出力の null↔[] 切り替え / pretty / 不正フラグの exit 2)。
type fakeReader struct {
	listResult akashic.ListResult
	listErr    error
	listCalls  []akashic.ListOptions
}

func (f *fakeReader) List(_ context.Context, opts akashic.ListOptions) (akashic.ListResult, error) {
	f.listCalls = append(f.listCalls, opts)
	return f.listResult, f.listErr
}

func (f *fakeReader) AggregateByWorkflow(_ context.Context, _ akashic.WorkflowAggregateOptions) (akashic.WorkflowAggregateResult, error) {
	return akashic.WorkflowAggregateResult{}, nil
}

func (f *fakeReader) AggregateByProvider(_ context.Context, _ akashic.ProviderAggregateOptions) (akashic.ProviderAggregateResult, error) {
	return akashic.ProviderAggregateResult{}, nil
}

func (f *fakeReader) Detail(_ context.Context, _ akashic.DetailOptions) (akashic.RunDetail, error) {
	return akashic.RunDetail{}, nil
}

func (f *fakeReader) FailureClusters(_ context.Context, _ akashic.FailureOptions) (akashic.FailureClustersResult, error) {
	return akashic.FailureClustersResult{}, nil
}

func (f *fakeReader) InRunResolutionsForCluster(_ context.Context, _ akashic.InRunResolutionOptions) (akashic.InRunResolutionsResult, error) {
	return akashic.InRunResolutionsResult{}, nil
}

func (f *fakeReader) NextSuccessAcrossRuns(_ context.Context, _ akashic.NextSuccessOptions) (akashic.NextSuccessResult, error) {
	return akashic.NextSuccessResult{}, nil
}

// AgentCallsByWorkflowRun は agent-learnings spec (task 3.2) で追加された Reader 拡張への
// no-op 追従。本 cli test は export 経路の検証が責務で agent-learnings 経路は対象外のため
// 履歴は保持しない。
func (f *fakeReader) AgentCallsByWorkflowRun(_ context.Context, _ string) ([]akashic.AgentCallRow, error) {
	return nil, nil
}

// LearningsByWorkflowRun は agent-learnings spec (task 3.2) で追加された Reader 拡張への
// no-op 追従。同上の理由で履歴は保持しない。
func (f *fakeReader) LearningsByWorkflowRun(_ context.Context, _ string) ([]akashic.LearningRow, error) {
	return nil, nil
}

// depsWithReader は fakeReader を注入した Deps を構築する。
// stdout / stderr は bytes.Buffer に差し替え、CLI 経路の出力をテストから検証可能にする。
func depsWithReader(reader akashic.Reader) (Deps, *bytes.Buffer, *bytes.Buffer) {
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	return Deps{Stdout: stdout, Stderr: stderr, Reader: reader, CWD: "/tmp/rilll-test"}, stdout, stderr
}

// 3.1 verification: fake Reader による smoke test 群。
// 4.6 の golden test で詳細 fixture / 不正フラグを comprehensive に検証する。

// 空の ListResult でも JSON は items: [] で出力する (null にしない、Req 6.10)。
func TestRunAkashicExport_EmptyResultEmitsItemsAsArray(t *testing.T) {
	reader := &fakeReader{listResult: akashic.ListResult{Limit: 50, Offset: 0}}
	deps, stdout, stderr := depsWithReader(reader)
	if exit := runAkashicExport(context.Background(), nil, deps); exit != ExitOK {
		t.Fatalf("expected exit 0, got %d. stderr=%s", exit, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"items":[]`) {
		t.Errorf("expected items as empty array, got: %s", stdout.String())
	}
	// next_offset は nil で null 出力
	if !strings.Contains(stdout.String(), `"next_offset":null`) {
		t.Errorf("expected next_offset:null when no next page, got: %s", stdout.String())
	}
}

// AgentCalls が nil でも JSON は "agent_calls": [] で出力する (null にしない、Req 6.10)。
func TestRunAkashicExport_AgentCallsNilEmitsEmptyArray(t *testing.T) {
	reader := &fakeReader{listResult: akashic.ListResult{
		Items: []akashic.WorkflowRunListItem{{
			RunHeader: akashic.RunHeader{
				RunID:        "run-1",
				WorkflowID:   "wf",
				WorkflowName: "Workflow",
				StartedAt:    time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
			},
			AgentCalls: nil,
		}},
		Limit: 50,
	}}
	deps, stdout, _ := depsWithReader(reader)
	if exit := runAkashicExport(context.Background(), nil, deps); exit != ExitOK {
		t.Fatalf("expected exit 0, got %d", exit)
	}
	if !strings.Contains(stdout.String(), `"agent_calls":[]`) {
		t.Errorf("expected agent_calls as empty array, got: %s", stdout.String())
	}
}

// --pretty フラグでインデント付き JSON を出力する (Req 6.7)。
func TestRunAkashicExport_PrettyOutputIsIndented(t *testing.T) {
	reader := &fakeReader{listResult: akashic.ListResult{Limit: 50}}
	deps, stdout, _ := depsWithReader(reader)
	if exit := runAkashicExport(context.Background(), []string{"--pretty"}, deps); exit != ExitOK {
		t.Fatalf("expected exit 0, got %d", exit)
	}
	// インデント (2 スペース) が含まれること
	if !strings.Contains(stdout.String(), "\n  \"items\"") {
		t.Errorf("expected pretty indented output, got: %q", stdout.String())
	}
	// 末尾 LF 1 つで終端
	if !strings.HasSuffix(stdout.String(), "\n") {
		t.Errorf("expected output to end with LF, got: %q", stdout.String())
	}
}

// --success の不正値は exit 2 + stderr エラー (Req 6.4, 6.9)。
func TestRunAkashicExport_InvalidSuccessExitsTwo(t *testing.T) {
	reader := &fakeReader{}
	deps, _, stderr := depsWithReader(reader)
	if exit := runAkashicExport(context.Background(), []string{"--success", "foo"}, deps); exit != ExitUsageError {
		t.Fatalf("expected exit 2, got %d. stderr=%s", exit, stderr.String())
	}
	if !strings.Contains(stderr.String(), "--success") {
		t.Errorf("expected stderr to mention --success, got: %s", stderr.String())
	}
}

// --since が RFC3339 でパースできない場合は exit 2 + stderr エラー (Req 6.3, 6.9)。
func TestRunAkashicExport_InvalidSinceExitsTwo(t *testing.T) {
	reader := &fakeReader{}
	deps, _, stderr := depsWithReader(reader)
	if exit := runAkashicExport(context.Background(), []string{"--since", "not-a-date"}, deps); exit != ExitUsageError {
		t.Fatalf("expected exit 2, got %d. stderr=%s", exit, stderr.String())
	}
	if !strings.Contains(stderr.String(), "--since") {
		t.Errorf("expected stderr to mention --since, got: %s", stderr.String())
	}
}

// --limit=-1 は ListOptions.Validate で ErrInvalidLimit を返す → CLI 不正入力扱い (Req 6.9, 7.4)。
func TestRunAkashicExport_InvalidLimitExitsTwo(t *testing.T) {
	// fake Reader が ErrInvalidLimit を返す状況を模擬する (実 Reader でも -1 で同じ挙動)。
	reader := &fakeReader{listErr: akashic.ErrInvalidLimit}
	deps, _, stderr := depsWithReader(reader)
	if exit := runAkashicExport(context.Background(), []string{"--limit", "-1"}, deps); exit != ExitUsageError {
		t.Fatalf("expected exit 2, got %d. stderr=%s", exit, stderr.String())
	}
	if !strings.Contains(stderr.String(), "limit") {
		t.Errorf("expected stderr to mention limit, got: %s", stderr.String())
	}
}

// 全フラグが ListOptions に正しく転写されること (Req 6.2〜6.5)。
func TestRunAkashicExport_FlagsPassedToReader(t *testing.T) {
	reader := &fakeReader{listResult: akashic.ListResult{Limit: 10, Offset: 5}}
	deps, _, stderr := depsWithReader(reader)
	args := []string{
		"--workflow", "git-commit",
		"--since", "2024-01-01T00:00:00Z",
		"--until", "2024-02-01T00:00:00Z",
		"--provider", "claude",
		"--cwd", "/repo",
		"--success", "true",
		"--limit", "10",
		"--offset", "5",
	}
	if exit := runAkashicExport(context.Background(), args, deps); exit != ExitOK {
		t.Fatalf("expected exit 0, got %d. stderr=%s", exit, stderr.String())
	}
	if len(reader.listCalls) != 1 {
		t.Fatalf("expected 1 List call, got %d", len(reader.listCalls))
	}
	got := reader.listCalls[0]
	if got.WorkflowID != "git-commit" {
		t.Errorf("expected WorkflowID=git-commit, got %q", got.WorkflowID)
	}
	if got.ProviderID != "claude" {
		t.Errorf("expected ProviderID=claude, got %q", got.ProviderID)
	}
	if got.CWD != "/repo" {
		t.Errorf("expected CWD=/repo, got %q", got.CWD)
	}
	if got.Limit != 10 || got.Offset != 5 {
		t.Errorf("expected Limit=10 Offset=5, got Limit=%d Offset=%d", got.Limit, got.Offset)
	}
	if got.Success == nil || *got.Success != true {
		t.Errorf("expected Success=&true, got %v", got.Success)
	}
	want := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	if !got.Range.Since.Equal(want) {
		t.Errorf("expected Since=%v, got %v", want, got.Range.Since)
	}
	wantUntil := time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC)
	if !got.Range.Until.Equal(wantUntil) {
		t.Errorf("expected Until=%v, got %v", wantUntil, got.Range.Until)
	}
}

// 既定パスでの JSON ペイロードが妥当 (時刻 RFC3339Nano / フィールド型 / next_offset)。
func TestRunAkashicExport_DefaultJSONIsValid(t *testing.T) {
	now := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	finished := now.Add(time.Hour)
	successTrue := true
	cost := 0.05
	dur := int64(1234)
	tokensIn := int64(100)
	tokensOut := int64(200)
	reader := &fakeReader{listResult: akashic.ListResult{
		Items: []akashic.WorkflowRunListItem{{
			RunHeader: akashic.RunHeader{
				RunID:        "run-1",
				WorkflowID:   "wf",
				WorkflowName: "Workflow",
				CWD:          "/repo",
				StartedAt:    now,
				FinishedAt:   &finished,
				Success:      &successTrue,
			},
			AgentCalls: []akashic.AgentCallSummary{{
				ID:           "ac-1",
				ProviderID:   "claude",
				StepID:       "step-1",
				StepName:     "step name",
				Success:      &successTrue,
				DurationMS:   &dur,
				TotalCostUSD: &cost,
				InputTokens:  &tokensIn,
				OutputTokens: &tokensOut,
				StartedAt:    now,
				FinishedAt:   &finished,
			}},
		}},
		Limit: 50,
	}}
	deps, stdout, _ := depsWithReader(reader)
	if exit := runAkashicExport(context.Background(), nil, deps); exit != ExitOK {
		t.Fatalf("expected exit 0, got %d", exit)
	}
	var payload ExportPayload
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("invalid JSON: %v\nstdout=%s", err, stdout.String())
	}
	if len(payload.Items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(payload.Items))
	}
	item := payload.Items[0]
	if item.StartedAt != now.Format(time.RFC3339Nano) {
		t.Errorf("expected RFC3339Nano started_at, got %s", item.StartedAt)
	}
	if item.FinishedAt == nil || *item.FinishedAt != finished.Format(time.RFC3339Nano) {
		t.Errorf("expected RFC3339Nano finished_at, got %v", item.FinishedAt)
	}
	if len(item.AgentCalls) != 1 {
		t.Fatalf("expected 1 agent_call, got %d", len(item.AgentCalls))
	}
	ac := item.AgentCalls[0]
	if ac.DurationMS == nil || *ac.DurationMS != dur {
		t.Errorf("expected duration_ms=%d, got %v", dur, ac.DurationMS)
	}
	if ac.TotalCostUSD == nil || *ac.TotalCostUSD != cost {
		t.Errorf("expected total_cost_usd=%v, got %v", cost, ac.TotalCostUSD)
	}
	// next_offset は nil (len(Items) != Limit)
	if payload.NextOffset != nil {
		t.Errorf("expected next_offset=nil, got %v", payload.NextOffset)
	}
}

// next_offset が ListResult に含まれているとき JSON は数値で出力する。
func TestRunAkashicExport_NextOffsetEmitsNumber(t *testing.T) {
	next := 50
	reader := &fakeReader{listResult: akashic.ListResult{
		Items:      []akashic.WorkflowRunListItem{}, // 中身は問わない
		Limit:      50,
		Offset:     0,
		NextOffset: &next,
	}}
	deps, stdout, _ := depsWithReader(reader)
	if exit := runAkashicExport(context.Background(), nil, deps); exit != ExitOK {
		t.Fatalf("expected exit 0, got %d", exit)
	}
	if !strings.Contains(stdout.String(), `"next_offset":50`) {
		t.Errorf("expected next_offset:50, got: %s", stdout.String())
	}
}

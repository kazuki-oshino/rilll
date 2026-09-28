// Package ui のアカシックモード関連 tea.Cmd ヘルパに対するテスト
// (akashic-mode-tui spec Task 3.1, 3.2, 3.3)。
//
// 本ファイルは fake Reader / fake ArtifactReader を用いて、
// akashic_io.go の Cmd ヘルパが
//
//	(a) 渡された Options を期待通り Reader に渡すこと
//	(b) 戻り msg の型と中身が想定通りであること
//	(c) Reader / ArtifactReader が nil のとき Reader メソッドを呼ばずエラー msg を返すこと
//
// を検証する。
//
// Task 3.2 の追加事項:
//   - ArtifactReader 系 Cmd ヘルパ (GetArtifactsCmd / ReadArtifactCmd) と msg
//     型の挙動を観測する fakeArtifactReader と動的テスト
//   - akashic_io.go ソース上で ReadArtifactUnlimited を一切参照しないこと、
//     ReadArtifactCmd が akashic.ContextWithRunCWD を経由することを担保する
//     静的検査
//
// Task 3.3 の追加事項:
//   - LaunchHandoffCmd の実装テスト。fake workflow.Definition を用いて
//     (a) def.Run 起動時 ctx に HandoffParams が乗っていること、
//     (b) 起動成功時 HandoffStartedMsg / 即時エラー時 HandoffFailedMsg が
//     Cmd 戻り値として返ること
//     を観測する。
package akashic

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"testing"
	"time"

	"github.com/kazuki-oshino/rilll/internal/akashic"
	"github.com/kazuki-oshino/rilll/internal/workflow"
)

// fakeReader はテスト用の akashic.Reader 実装。
// 各メソッドの呼び出し回数と最後に渡された Options を記録する。
type fakeReader struct {
	listCalls  int
	listOpts   akashic.ListOptions
	listResult akashic.ListResult
	listErr    error

	aggWorkflowCalls  int
	aggWorkflowOpts   akashic.WorkflowAggregateOptions
	aggWorkflowResult akashic.WorkflowAggregateResult
	aggWorkflowErr    error

	aggProviderCalls  int
	aggProviderOpts   akashic.ProviderAggregateOptions
	aggProviderResult akashic.ProviderAggregateResult
	aggProviderErr    error

	detailCalls  int
	detailOpts   akashic.DetailOptions
	detailResult akashic.RunDetail
	detailErr    error

	failureCalls  int
	failureOpts   akashic.FailureOptions
	failureResult akashic.FailureClustersResult
	failureErr    error
}

func (r *fakeReader) List(_ context.Context, opts akashic.ListOptions) (akashic.ListResult, error) {
	r.listCalls++
	r.listOpts = opts
	return r.listResult, r.listErr
}

func (r *fakeReader) AggregateByWorkflow(_ context.Context, opts akashic.WorkflowAggregateOptions) (akashic.WorkflowAggregateResult, error) {
	r.aggWorkflowCalls++
	r.aggWorkflowOpts = opts
	return r.aggWorkflowResult, r.aggWorkflowErr
}

func (r *fakeReader) AggregateByProvider(_ context.Context, opts akashic.ProviderAggregateOptions) (akashic.ProviderAggregateResult, error) {
	r.aggProviderCalls++
	r.aggProviderOpts = opts
	return r.aggProviderResult, r.aggProviderErr
}

func (r *fakeReader) Detail(_ context.Context, opts akashic.DetailOptions) (akashic.RunDetail, error) {
	r.detailCalls++
	r.detailOpts = opts
	return r.detailResult, r.detailErr
}

func (r *fakeReader) FailureClusters(_ context.Context, opts akashic.FailureOptions) (akashic.FailureClustersResult, error) {
	r.failureCalls++
	r.failureOpts = opts
	return r.failureResult, r.failureErr
}

func (r *fakeReader) InRunResolutionsForCluster(_ context.Context, _ akashic.InRunResolutionOptions) (akashic.InRunResolutionsResult, error) {
	return akashic.InRunResolutionsResult{}, nil
}

func (r *fakeReader) NextSuccessAcrossRuns(_ context.Context, _ akashic.NextSuccessOptions) (akashic.NextSuccessResult, error) {
	return akashic.NextSuccessResult{}, nil
}

// AgentCallsByWorkflowRun は agent-learnings spec (task 3.2) で追加された Reader 拡張への
// no-op 追従。本 ui/akashic test は handoff/list/detail 経路の検証が責務で agent-learnings
// 経路は対象外のため履歴は保持しない。
func (r *fakeReader) AgentCallsByWorkflowRun(_ context.Context, _ string) ([]akashic.AgentCallRow, error) {
	return nil, nil
}

// LearningsByWorkflowRun は agent-learnings spec (task 3.2) で追加された Reader 拡張への
// no-op 追従。同上の理由で履歴は保持しない。
func (r *fakeReader) LearningsByWorkflowRun(_ context.Context, _ string) ([]akashic.LearningRow, error) {
	return nil, nil
}

// --- ListRunsCmd ---

func TestListRunsCmdSetsDefaultLimitAndReturnsListRunsMsg(t *testing.T) {
	want := akashic.ListResult{
		Items: []akashic.WorkflowRunListItem{
			{RunHeader: akashic.RunHeader{RunID: "run-1", WorkflowID: "wf-1", StartedAt: time.Now()}},
		},
		Limit:  50,
		Offset: 0,
	}
	reader := &fakeReader{listResult: want}

	// 呼び出し側が Limit=0 を渡しても、Cmd が DefaultListLimit (=50) に補完すること
	opts := akashic.ListOptions{WorkflowID: "wf-1", Offset: 100}
	cmd := ListRunsCmd(reader, opts)
	if cmd == nil {
		t.Fatal("expected non-nil Cmd")
	}

	msg := cmd()
	got, ok := msg.(ListRunsMsg)
	if !ok {
		t.Fatalf("expected ListRunsMsg, got %T", msg)
	}
	if got.Err != nil {
		t.Fatalf("expected no error, got %v", got.Err)
	}
	if reader.listCalls != 1 {
		t.Fatalf("expected List to be called once, got %d", reader.listCalls)
	}
	// (a) 渡された Options が期待通り (Limit=50)
	if reader.listOpts.Limit != akashic.DefaultListLimit {
		t.Fatalf("expected Limit=%d, got %d", akashic.DefaultListLimit, reader.listOpts.Limit)
	}
	if reader.listOpts.WorkflowID != "wf-1" {
		t.Fatalf("expected WorkflowID=wf-1, got %q", reader.listOpts.WorkflowID)
	}
	if reader.listOpts.Offset != 100 {
		t.Fatalf("expected Offset=100, got %d", reader.listOpts.Offset)
	}
	// (b) 戻り msg の中身
	if len(got.Result.Items) != 1 || got.Result.Items[0].RunID != "run-1" {
		t.Fatalf("unexpected result items: %#v", got.Result.Items)
	}
}

func TestListRunsCmdOverridesNonDefaultLimit(t *testing.T) {
	// Req 9.5: ページサイズは本 spec で固定 (DefaultListLimit=50)
	// 呼び出し側が異なる Limit を渡しても、Cmd は必ず DefaultListLimit に上書きする
	reader := &fakeReader{}
	opts := akashic.ListOptions{Limit: 10}
	cmd := ListRunsCmd(reader, opts)
	_ = cmd()
	if reader.listOpts.Limit != akashic.DefaultListLimit {
		t.Fatalf("expected Limit to be forced to %d, got %d", akashic.DefaultListLimit, reader.listOpts.Limit)
	}
}

func TestListRunsCmdReturnsErrFromReader(t *testing.T) {
	wantErr := errors.New("boom list")
	reader := &fakeReader{listErr: wantErr}
	cmd := ListRunsCmd(reader, akashic.ListOptions{})
	msg := cmd()
	got, ok := msg.(ListRunsMsg)
	if !ok {
		t.Fatalf("expected ListRunsMsg, got %T", msg)
	}
	if !errors.Is(got.Err, wantErr) {
		t.Fatalf("expected err=%v, got %v", wantErr, got.Err)
	}
}

func TestListRunsCmdReaderNilReturnsErrorMsgWithoutCalling(t *testing.T) {
	// (c) Reader=nil 時は Reader メソッドを呼ばずエラー msg を返す
	cmd := ListRunsCmd(nil, akashic.ListOptions{})
	if cmd == nil {
		t.Fatal("expected non-nil Cmd even when reader is nil")
	}
	msg := cmd()
	got, ok := msg.(ListRunsMsg)
	if !ok {
		t.Fatalf("expected ListRunsMsg, got %T", msg)
	}
	if !errors.Is(got.Err, errReaderUnavailable) {
		t.Fatalf("expected errReaderUnavailable, got %v", got.Err)
	}
	if len(got.Result.Items) != 0 {
		t.Fatalf("expected zero result, got %#v", got.Result)
	}
}

// --- AggregateByWorkflowCmd ---

func TestAggregateByWorkflowCmdReturnsResult(t *testing.T) {
	want := akashic.WorkflowAggregateResult{
		Buckets: []akashic.WorkflowAggregateBucket{
			{WorkflowID: "wf-1", RunCount: 5, SuccessRate: 0.8},
		},
	}
	reader := &fakeReader{aggWorkflowResult: want}
	opts := akashic.WorkflowAggregateOptions{WorkflowID: "wf-1"}
	cmd := AggregateByWorkflowCmd(reader, opts)
	msg := cmd()
	got, ok := msg.(AggregateByWorkflowMsg)
	if !ok {
		t.Fatalf("expected AggregateByWorkflowMsg, got %T", msg)
	}
	if got.Err != nil {
		t.Fatalf("expected no error, got %v", got.Err)
	}
	if reader.aggWorkflowCalls != 1 {
		t.Fatalf("expected AggregateByWorkflow to be called once, got %d", reader.aggWorkflowCalls)
	}
	if reader.aggWorkflowOpts.WorkflowID != "wf-1" {
		t.Fatalf("expected WorkflowID=wf-1, got %q", reader.aggWorkflowOpts.WorkflowID)
	}
	if len(got.Result.Buckets) != 1 || got.Result.Buckets[0].WorkflowID != "wf-1" {
		t.Fatalf("unexpected buckets: %#v", got.Result.Buckets)
	}
}

func TestAggregateByWorkflowCmdReturnsErrFromReader(t *testing.T) {
	wantErr := errors.New("boom agg wf")
	reader := &fakeReader{aggWorkflowErr: wantErr}
	cmd := AggregateByWorkflowCmd(reader, akashic.WorkflowAggregateOptions{})
	msg := cmd()
	got := msg.(AggregateByWorkflowMsg)
	if !errors.Is(got.Err, wantErr) {
		t.Fatalf("expected err=%v, got %v", wantErr, got.Err)
	}
}

func TestAggregateByWorkflowCmdReaderNilReturnsErrorMsg(t *testing.T) {
	cmd := AggregateByWorkflowCmd(nil, akashic.WorkflowAggregateOptions{})
	msg := cmd()
	got, ok := msg.(AggregateByWorkflowMsg)
	if !ok {
		t.Fatalf("expected AggregateByWorkflowMsg, got %T", msg)
	}
	if !errors.Is(got.Err, errReaderUnavailable) {
		t.Fatalf("expected errReaderUnavailable, got %v", got.Err)
	}
	if len(got.Result.Buckets) != 0 {
		t.Fatalf("expected zero result, got %#v", got.Result)
	}
}

// --- AggregateByProviderCmd ---

func TestAggregateByProviderCmdReturnsResult(t *testing.T) {
	want := akashic.ProviderAggregateResult{
		Buckets: []akashic.ProviderAggregateBucket{
			{ProviderID: "claude", CallCount: 10, SuccessRate: 0.9},
		},
	}
	reader := &fakeReader{aggProviderResult: want}
	opts := akashic.ProviderAggregateOptions{ProviderID: "claude"}
	cmd := AggregateByProviderCmd(reader, opts)
	msg := cmd()
	got, ok := msg.(AggregateByProviderMsg)
	if !ok {
		t.Fatalf("expected AggregateByProviderMsg, got %T", msg)
	}
	if got.Err != nil {
		t.Fatalf("expected no error, got %v", got.Err)
	}
	if reader.aggProviderCalls != 1 {
		t.Fatalf("expected AggregateByProvider to be called once, got %d", reader.aggProviderCalls)
	}
	if reader.aggProviderOpts.ProviderID != "claude" {
		t.Fatalf("expected ProviderID=claude, got %q", reader.aggProviderOpts.ProviderID)
	}
	if len(got.Result.Buckets) != 1 || got.Result.Buckets[0].ProviderID != "claude" {
		t.Fatalf("unexpected buckets: %#v", got.Result.Buckets)
	}
}

func TestAggregateByProviderCmdReturnsErrFromReader(t *testing.T) {
	wantErr := errors.New("boom agg pr")
	reader := &fakeReader{aggProviderErr: wantErr}
	cmd := AggregateByProviderCmd(reader, akashic.ProviderAggregateOptions{})
	msg := cmd()
	got := msg.(AggregateByProviderMsg)
	if !errors.Is(got.Err, wantErr) {
		t.Fatalf("expected err=%v, got %v", wantErr, got.Err)
	}
}

func TestAggregateByProviderCmdReaderNilReturnsErrorMsg(t *testing.T) {
	cmd := AggregateByProviderCmd(nil, akashic.ProviderAggregateOptions{})
	msg := cmd()
	got, ok := msg.(AggregateByProviderMsg)
	if !ok {
		t.Fatalf("expected AggregateByProviderMsg, got %T", msg)
	}
	if !errors.Is(got.Err, errReaderUnavailable) {
		t.Fatalf("expected errReaderUnavailable, got %v", got.Err)
	}
	if len(got.Result.Buckets) != 0 {
		t.Fatalf("expected zero result, got %#v", got.Result)
	}
}

// --- RunDetailCmd ---

func TestRunDetailCmdReturnsResult(t *testing.T) {
	want := akashic.RunDetail{
		Run: akashic.RunHeader{RunID: "run-7", WorkflowID: "wf-7", StartedAt: time.Now()},
	}
	reader := &fakeReader{detailResult: want}
	opts := akashic.DetailOptions{
		WorkflowRunID: "run-7",
		RawEvents:     akashic.RawEventsQuery{Limit: 100, Offset: 0},
	}
	cmd := RunDetailCmd(reader, opts)
	msg := cmd()
	got, ok := msg.(RunDetailMsg)
	if !ok {
		t.Fatalf("expected RunDetailMsg, got %T", msg)
	}
	if got.Err != nil {
		t.Fatalf("expected no error, got %v", got.Err)
	}
	if reader.detailCalls != 1 {
		t.Fatalf("expected Detail to be called once, got %d", reader.detailCalls)
	}
	if reader.detailOpts.WorkflowRunID != "run-7" {
		t.Fatalf("expected WorkflowRunID=run-7, got %q", reader.detailOpts.WorkflowRunID)
	}
	if reader.detailOpts.RawEvents.Limit != 100 {
		t.Fatalf("expected RawEvents.Limit=100, got %d", reader.detailOpts.RawEvents.Limit)
	}
	if got.Detail.Run.RunID != "run-7" {
		t.Fatalf("unexpected detail: %#v", got.Detail)
	}
}

func TestRunDetailCmdReturnsErrFromReader(t *testing.T) {
	wantErr := akashic.ErrWorkflowRunNotFound
	reader := &fakeReader{detailErr: wantErr}
	cmd := RunDetailCmd(reader, akashic.DetailOptions{WorkflowRunID: "missing"})
	msg := cmd()
	got := msg.(RunDetailMsg)
	// Req 5.4: ErrWorkflowRunNotFound は errors.Is で判別可能でなければならない
	if !errors.Is(got.Err, wantErr) {
		t.Fatalf("expected ErrWorkflowRunNotFound, got %v", got.Err)
	}
}

func TestRunDetailCmdReaderNilReturnsErrorMsg(t *testing.T) {
	cmd := RunDetailCmd(nil, akashic.DetailOptions{WorkflowRunID: "run-1"})
	msg := cmd()
	got, ok := msg.(RunDetailMsg)
	if !ok {
		t.Fatalf("expected RunDetailMsg, got %T", msg)
	}
	if !errors.Is(got.Err, errReaderUnavailable) {
		t.Fatalf("expected errReaderUnavailable, got %v", got.Err)
	}
	if got.Detail.Run.RunID != "" {
		t.Fatalf("expected zero RunDetail, got %#v", got.Detail)
	}
}

// --- 包括的: Reader=nil 時に Reader メソッドが一切呼ばれないこと ---

func TestReaderCmdsDoNotInvokeReaderWhenNil(t *testing.T) {
	// Reader=nil で 4 つのヘルパを invoke しても、fakeReader の呼び出し計数は
	// 介在しない (nil 渡しのため)。代わりに全 Cmd が errReaderUnavailable を返すこと
	// を確認することで間接的に Reader メソッド未呼出を担保する。
	cases := []struct {
		name string
		run  func() error
	}{
		{
			name: "list",
			run: func() error {
				m := ListRunsCmd(nil, akashic.ListOptions{})().(ListRunsMsg)
				return m.Err
			},
		},
		{
			name: "aggWorkflow",
			run: func() error {
				m := AggregateByWorkflowCmd(nil, akashic.WorkflowAggregateOptions{})().(AggregateByWorkflowMsg)
				return m.Err
			},
		},
		{
			name: "aggProvider",
			run: func() error {
				m := AggregateByProviderCmd(nil, akashic.ProviderAggregateOptions{})().(AggregateByProviderMsg)
				return m.Err
			},
		},
		{
			name: "runDetail",
			run: func() error {
				m := RunDetailCmd(nil, akashic.DetailOptions{})().(RunDetailMsg)
				return m.Err
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := c.run(); !errors.Is(err, errReaderUnavailable) {
				t.Fatalf("expected errReaderUnavailable, got %v", err)
			}
		})
	}
}

// --- ArtifactReader 系 Cmd ヘルパ (Task 3.2) ---

// fakeArtifactReader はテスト用の akashic.ArtifactReader 実装。
// 各メソッドの呼び出し回数と最後に観測した引数 (path / ctx) を記録する。
//
// 観測点:
//   - getArtifactsCalls / getArtifactsRunID: GetArtifacts への入力
//   - readArtifactCalls / readArtifactPath / readArtifactCtx:
//     ReadArtifact への入力 (cwd は ctx 経由で渡る)
//   - readArtifactUnlimitedCalls: ReadArtifactUnlimited の呼び出し計数。
//     Req 6.6 / 8.4 (ReadArtifactUnlimited 既定経路禁止) の不変式を観測する
//     ためのカウンタで、Task 3.2 完了条件 (c) で 0 回であることを検証する。
//
// 注意: akashic パッケージの runCWDFromContext は private のため、ctx 内の
// cwd 値を直接取り出すことはできない。代わりに ReadArtifact が受領した ctx を
// そのまま保存し、テスト側は (1) Cmd 内で akashic.ContextWithRunCWD を必ず
// 通すことを TestAkashicIOUsesContextWithRunCWDForReadArtifact (静的検査) で、
// (2) Cmd が cwd 引数を受け取り、その cwd を ReadArtifact に届けるフロー全体を
// fake のメソッド呼び出し計数と path 一致で担保する、という二段構えで観測する。
type fakeArtifactReader struct {
	getArtifactsCalls int
	getArtifactsRunID string
	getArtifactsRows  []akashic.ArtifactRow
	getArtifactsErr   error

	readArtifactCalls int
	readArtifactPath  string
	readArtifactCtx   context.Context // 受領した ctx をそのまま保持 (テスト側で観測する場合に備える)
	readArtifactData  []byte
	readArtifactErr   error

	readArtifactUnlimitedCalls int
}

func (a *fakeArtifactReader) GetArtifacts(_ context.Context, runID string) ([]akashic.ArtifactRow, error) {
	a.getArtifactsCalls++
	a.getArtifactsRunID = runID
	return a.getArtifactsRows, a.getArtifactsErr
}

func (a *fakeArtifactReader) ReadArtifact(ctx context.Context, path string) ([]byte, error) {
	a.readArtifactCalls++
	a.readArtifactPath = path
	a.readArtifactCtx = ctx
	return a.readArtifactData, a.readArtifactErr
}

func (a *fakeArtifactReader) ReadArtifactUnlimited(_ context.Context, _ string) ([]byte, error) {
	// Req 6.6 / 8.4: 本メソッドは akashic_io.go から絶対に呼ばれない。
	// 呼び出し計数を観測することで、不変式違反 (akashic_io.go 内での誤呼び出し)
	// を回帰テストで検知する。
	a.readArtifactUnlimitedCalls++
	return nil, errors.New("fakeArtifactReader: ReadArtifactUnlimited must not be called from akashic_io.go")
}

func TestGetArtifactsCmdReturnsRows(t *testing.T) {
	// (a) GetArtifacts が 1 回だけ呼ばれること、(b) 戻り msg が期待通り
	want := []akashic.ArtifactRow{
		{WorkflowRunID: "run-1", Kind: akashic.ArtifactKindResult, Path: "/abs/.rilll/run-1/result.md", SizeBytes: 42, MIME: "text/markdown"},
	}
	reader := &fakeArtifactReader{getArtifactsRows: want}
	cmd := GetArtifactsCmd(reader, "run-1")
	if cmd == nil {
		t.Fatal("expected non-nil Cmd")
	}
	msg := cmd()
	got, ok := msg.(GetArtifactsMsg)
	if !ok {
		t.Fatalf("expected GetArtifactsMsg, got %T", msg)
	}
	if got.Err != nil {
		t.Fatalf("expected no error, got %v", got.Err)
	}
	if reader.getArtifactsCalls != 1 {
		t.Fatalf("expected GetArtifacts to be called once, got %d", reader.getArtifactsCalls)
	}
	if reader.getArtifactsRunID != "run-1" {
		t.Fatalf("expected runID=run-1, got %q", reader.getArtifactsRunID)
	}
	if len(got.Rows) != 1 || got.Rows[0].WorkflowRunID != "run-1" {
		t.Fatalf("unexpected rows: %#v", got.Rows)
	}
	// (c) ReadArtifactUnlimited は呼ばれない
	if reader.readArtifactUnlimitedCalls != 0 {
		t.Fatalf("expected ReadArtifactUnlimited never to be called, got %d", reader.readArtifactUnlimitedCalls)
	}
}

func TestGetArtifactsCmdReturnsErrFromReader(t *testing.T) {
	wantErr := errors.New("boom get artifacts")
	reader := &fakeArtifactReader{getArtifactsErr: wantErr}
	cmd := GetArtifactsCmd(reader, "run-1")
	msg := cmd()
	got, ok := msg.(GetArtifactsMsg)
	if !ok {
		t.Fatalf("expected GetArtifactsMsg, got %T", msg)
	}
	if !errors.Is(got.Err, wantErr) {
		t.Fatalf("expected err=%v, got %v", wantErr, got.Err)
	}
	if reader.getArtifactsCalls != 1 {
		t.Fatalf("expected GetArtifacts to be called once, got %d", reader.getArtifactsCalls)
	}
}

func TestGetArtifactsCmdReaderNilReturnsErrorMsgWithoutCalling(t *testing.T) {
	// (d) ArtifactReader=nil 時は何も呼ばれない
	cmd := GetArtifactsCmd(nil, "run-1")
	if cmd == nil {
		t.Fatal("expected non-nil Cmd even when reader is nil")
	}
	msg := cmd()
	got, ok := msg.(GetArtifactsMsg)
	if !ok {
		t.Fatalf("expected GetArtifactsMsg, got %T", msg)
	}
	if !errors.Is(got.Err, errArtifactReaderUnavailable) {
		t.Fatalf("expected errArtifactReaderUnavailable, got %v", got.Err)
	}
	if got.Rows != nil {
		t.Fatalf("expected nil rows, got %#v", got.Rows)
	}
}

func TestReadArtifactCmdReturnsData(t *testing.T) {
	// (b) ReadArtifactCmd の中で ContextWithRunCWD 経由の cwd が
	// ArtifactReader 側で取得できることを観測する。
	want := []byte("hello artifact body")
	reader := &fakeArtifactReader{readArtifactData: want}
	cmd := ReadArtifactCmd(reader, "/work/cwd", "/abs/.rilll/run-1/result.md")
	if cmd == nil {
		t.Fatal("expected non-nil Cmd")
	}
	msg := cmd()
	got, ok := msg.(ReadArtifactMsg)
	if !ok {
		t.Fatalf("expected ReadArtifactMsg, got %T", msg)
	}
	if got.Err != nil {
		t.Fatalf("expected no error, got %v", got.Err)
	}
	if reader.readArtifactCalls != 1 {
		t.Fatalf("expected ReadArtifact to be called once, got %d", reader.readArtifactCalls)
	}
	if reader.readArtifactPath != "/abs/.rilll/run-1/result.md" {
		t.Fatalf("expected path=/abs/.rilll/run-1/result.md, got %q", reader.readArtifactPath)
	}
	if got.Path != "/abs/.rilll/run-1/result.md" {
		t.Fatalf("expected msg.Path=/abs/.rilll/run-1/result.md, got %q", got.Path)
	}
	if string(got.Data) != string(want) {
		t.Fatalf("expected data=%q, got %q", want, got.Data)
	}
	// (c) ReadArtifactUnlimited は呼ばれない
	if reader.readArtifactUnlimitedCalls != 0 {
		t.Fatalf("expected ReadArtifactUnlimited never to be called, got %d", reader.readArtifactUnlimitedCalls)
	}
}

// TestReadArtifactCmdPropagatesNonNilCtxToReader は ReadArtifactCmd が
// 必ず非 nil の ctx で ReadArtifact を呼ぶことを観測する。
// (cwd の ctx 注入そのものは ContextWithRunCWD の private key 経由のため
// 直接観測できないので、別途静的検査 TestAkashicIOUsesContextWithRunCWDForReadArtifact
// で akashic.ContextWithRunCWD の呼び出しを担保する)
func TestReadArtifactCmdPropagatesNonNilCtxToReader(t *testing.T) {
	want := []byte("body")
	reader := &fakeArtifactReader{readArtifactData: want}
	cmd := ReadArtifactCmd(reader, "/some/cwd", "/abs/.rilll/run-1/file.md")
	msg := cmd()
	got := msg.(ReadArtifactMsg)
	if got.Err != nil {
		t.Fatalf("expected no err, got %v", got.Err)
	}
	if reader.readArtifactCalls != 1 {
		t.Fatalf("expected ReadArtifact to be called once, got %d", reader.readArtifactCalls)
	}
	if reader.readArtifactCtx == nil {
		t.Fatal("expected non-nil ctx delivered to ReadArtifact")
	}
}

func TestReadArtifactCmdReturnsErrFromReader(t *testing.T) {
	wantErr := akashic.ErrArtifactTooLarge
	reader := &fakeArtifactReader{readArtifactErr: wantErr}
	cmd := ReadArtifactCmd(reader, "/cwd", "/abs/.rilll/run-1/big.bin")
	msg := cmd()
	got, ok := msg.(ReadArtifactMsg)
	if !ok {
		t.Fatalf("expected ReadArtifactMsg, got %T", msg)
	}
	if !errors.Is(got.Err, wantErr) {
		t.Fatalf("expected ErrArtifactTooLarge, got %v", got.Err)
	}
	if got.Path != "/abs/.rilll/run-1/big.bin" {
		t.Fatalf("expected path passthrough, got %q", got.Path)
	}
}

func TestReadArtifactCmdReaderNilReturnsErrorMsgWithoutCalling(t *testing.T) {
	// (d) ArtifactReader=nil 時は何も呼ばれない
	cmd := ReadArtifactCmd(nil, "/cwd", "/abs/.rilll/run-1/x.md")
	if cmd == nil {
		t.Fatal("expected non-nil Cmd even when reader is nil")
	}
	msg := cmd()
	got, ok := msg.(ReadArtifactMsg)
	if !ok {
		t.Fatalf("expected ReadArtifactMsg, got %T", msg)
	}
	if !errors.Is(got.Err, errArtifactReaderUnavailable) {
		t.Fatalf("expected errArtifactReaderUnavailable, got %v", got.Err)
	}
	if got.Data != nil {
		t.Fatalf("expected nil data, got %q", got.Data)
	}
	if got.Path != "/abs/.rilll/run-1/x.md" {
		// path はエラー時もメタ表示維持に使うため、入力 path をそのまま返す契約
		t.Fatalf("expected path passthrough even on error, got %q", got.Path)
	}
}

func TestArtifactReaderCmdsDoNotInvokeReaderWhenNil(t *testing.T) {
	// (d) 包括的: ArtifactReader=nil で 2 つのヘルパを invoke しても
	// errArtifactReaderUnavailable を返すことで間接的にメソッド未呼出を担保する。
	cases := []struct {
		name string
		run  func() error
	}{
		{
			name: "getArtifacts",
			run: func() error {
				m := GetArtifactsCmd(nil, "run-1")().(GetArtifactsMsg)
				return m.Err
			},
		},
		{
			name: "readArtifact",
			run: func() error {
				m := ReadArtifactCmd(nil, "/cwd", "/abs/.rilll/run-1/x.md")().(ReadArtifactMsg)
				return m.Err
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := c.run(); !errors.Is(err, errArtifactReaderUnavailable) {
				t.Fatalf("expected errArtifactReaderUnavailable, got %v", err)
			}
		})
	}
}

// TestAkashicIODoesNotReferenceReadArtifactUnlimited は
// internal/ui/akashic_io.go の AST 上で ReadArtifactUnlimited メソッド名が
// 識別子 (Selector) として参照されていないことを観測する静的検査
// (Req 6.6 / 8.4 の不変式)。
//
// 動機: Req 6.6 は ReadArtifactUnlimited を本 spec の既定経路で使わないことを
// 規定している。実装上の違反 (将来のリファクタで誤って参照すること) を回帰
// テストで検知できる構造にしておく。コメント中の言及は許容し、コード上の
// 識別子参照のみを対象にする (AST 走査は go/parser でコメントを除いた
// SelectorExpr を観測する形で実現する)。
func TestAkashicIODoesNotReferenceReadArtifactUnlimited(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "io.go", nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse akashic_io.go: %v", err)
	}
	ast.Inspect(file, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if sel.Sel != nil && sel.Sel.Name == "ReadArtifactUnlimited" {
			t.Fatalf("akashic_io.go must not reference ReadArtifactUnlimited as identifier (Req 6.6 / 8.4 invariant); pos=%s", fset.Position(sel.Pos()))
		}
		return true
	})
}

// TestAkashicIOUsesContextWithRunCWDForReadArtifact は
// internal/ui/akashic_io.go 内で akashic.ContextWithRunCWD を呼び出して
// いることを AST レベルで観測する (Req 6.3 / 8.3 の不変式)。
//
// 動機: ArtifactReader.ReadArtifact 経路で必ず cwd を ctx に注入することを
// 実装ソース上で担保する。fake 経由の動的観測では akashic 側の private key
// (runCWDFromContext) に到達できないため、構造的には Selector
// "akashic.ContextWithRunCWD" の参照存在を直接検査する。
func TestAkashicIOUsesContextWithRunCWDForReadArtifact(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "io.go", nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse akashic_io.go: %v", err)
	}
	found := false
	ast.Inspect(file, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		ident, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}
		// internal/akashic は新パッケージ内で `record` alias で import されている
		// (パッケージ名 akashic と衝突するため)。alias 名を観測する。
		if ident.Name == "record" && sel.Sel != nil && sel.Sel.Name == "ContextWithRunCWD" {
			found = true
			return false
		}
		return true
	})
	if !found {
		t.Fatalf("io.go must call record.ContextWithRunCWD (Req 6.3 / 8.3 invariant)")
	}
}

// --- LaunchHandoffCmd (Task 3.3) ---

// fakeWorkflowFn は workflow.Definition.Fn として渡せる関数を生成する観測ヘルパ。
//
// 目的:
//   - LaunchHandoffCmd が def.Run 経由で渡す ctx と cwd を捕捉する
//   - 戻り err を制御することで「起動成功 / 即時エラー」両ケースを切り替える
//
// 注意: LaunchHandoffCmd (Approach: 同期呼出) は cmd() 内で def.Run を直接呼び、
// 戻り値で起動成功 / 失敗を判定する。fake 側は同期で値を捕捉するだけでよく、
// goroutine 同期 (channel) を持つ必要はない。
type fakeWorkflowFn struct {
	callCount   int
	receivedCtx context.Context
	receivedCwd string
	receivedAsk workflow.WorkflowAsk
	emit        []workflow.WorkflowEvent // Fn 内で onEvent に渡すイベント (events チャネルへのフォワード観測用)
	err         error
}

// capture は workflow.Func 形式の関数を返す。Definition.Fn にそのまま代入できる。
func (f *fakeWorkflowFn) capture() workflow.Func {
	return func(ctx context.Context, cwd string, onEvent func(workflow.WorkflowEvent), ask workflow.WorkflowAsk) error {
		f.callCount++
		f.receivedCtx = ctx
		f.receivedCwd = cwd
		f.receivedAsk = ask
		// 既存 startWorkflow と同じ events チャネルへのフォワードパターンを観測するため、
		// 指定されたイベントを onEvent に流す (onEvent は LaunchHandoffCmd 側で
		// forward callback を呼ぶ closure)。
		for _, ev := range f.emit {
			onEvent(ev)
		}
		return f.err
	}
}

// noopAsk は workflow.WorkflowAsk を満たす最小実装。
// LaunchHandoffCmd のテストで ask 入力経路を握る必要がない場合に使う。
type noopAsk struct{}

func (noopAsk) Select(_ context.Context, _ workflow.SelectParams) (string, bool, error) {
	return "", false, nil
}

// TestLaunchHandoffCmdSuccessReturnsHandoffStartedMsgAndInjectsParams は
// 観測完了条件 (a) (b 成功側) を満たす:
//   - cmd() は HandoffStartedMsg{params} を返す
//   - def.Run が受領した ctx から HandoffFromContext で params が取り出せる (Req 7.2 / 7.5)
//   - cwd / 呼び出し回数も期待通り (Req 9.2: 同期で観測可能な決定的写像)
func TestLaunchHandoffCmdSuccessReturnsHandoffStartedMsgAndInjectsParams(t *testing.T) {
	forwarded := make([]workflow.WorkflowEvent, 0, 4)
	forward := func(ev workflow.WorkflowEvent) { forwarded = append(forwarded, ev) }
	fn := &fakeWorkflowFn{}
	def := workflow.Definition{
		ID:    "improve-workflow",
		Label: "Self Improve",
		Fn:    fn.capture(),
	}
	params := workflow.HandoffParams{
		TargetWorkflowID: "wf-7",
		RunIDs:           []string{"run-7"},
	}

	cmd := LaunchHandoffCmd(forward, noopAsk{}, def, params, "/work/cwd")
	if cmd == nil {
		t.Fatal("expected non-nil Cmd")
	}
	msg := cmd()

	started, ok := msg.(HandoffStartedMsg)
	if !ok {
		t.Fatalf("expected HandoffStartedMsg, got %T", msg)
	}
	if !reflect.DeepEqual(started.Params, params) {
		t.Fatalf("expected params=%+v, got %+v", params, started.Params)
	}

	if fn.callCount != 1 {
		t.Fatalf("expected def.Run to be called once, got %d", fn.callCount)
	}
	if fn.receivedCwd != "/work/cwd" {
		t.Fatalf("expected cwd=/work/cwd, got %q", fn.receivedCwd)
	}

	// (a) ctx に HandoffParams が乗っていること
	if fn.receivedCtx == nil {
		t.Fatal("expected non-nil ctx delivered to def.Run")
	}
	gotParams, ok := workflow.HandoffFromContext(fn.receivedCtx)
	if !ok {
		t.Fatal("expected HandoffFromContext to return ok=true on injected ctx")
	}
	if !reflect.DeepEqual(gotParams, params) {
		t.Fatalf("expected ctx params=%+v, got %+v", params, gotParams)
	}
}

// TestLaunchHandoffCmdImmediateErrorReturnsHandoffFailedMsg は
// 観測完了条件 (b 失敗側) を満たす:
//   - def.Run が同期的に err を返したとき cmd() は HandoffFailedMsg{params, err} を返す
//   - err は errors.Is で判別可能 (呼び出し側の Detail.HandoffErr 格納に使う)
//   - 失敗時でも HandoffParams が ctx に注入されていること (起動経路自体は実行された)
func TestLaunchHandoffCmdImmediateErrorReturnsHandoffFailedMsg(t *testing.T) {
	forward := func(_ workflow.WorkflowEvent) {}
	wantErr := errors.New("boom run")
	fn := &fakeWorkflowFn{err: wantErr}
	def := workflow.Definition{
		ID:    "improve-workflow",
		Label: "Self Improve",
		Fn:    fn.capture(),
	}
	params := workflow.HandoffParams{
		TargetWorkflowID: "wf-1",
		RunIDs:           []string{"run-1"},
	}

	cmd := LaunchHandoffCmd(forward, noopAsk{}, def, params, "/cwd")
	msg := cmd()

	failed, ok := msg.(HandoffFailedMsg)
	if !ok {
		t.Fatalf("expected HandoffFailedMsg, got %T", msg)
	}
	if !errors.Is(failed.Err, wantErr) {
		t.Fatalf("expected err=%v, got %v", wantErr, failed.Err)
	}
	if !reflect.DeepEqual(failed.Params, params) {
		t.Fatalf("expected params=%+v, got %+v", params, failed.Params)
	}

	// 失敗時でも ctx に HandoffParams は乗っている (Req 7.2: ctx 経由の hand-off 契約)
	if fn.receivedCtx == nil {
		t.Fatal("expected non-nil ctx delivered to def.Run even on failure")
	}
	gotParams, ok := workflow.HandoffFromContext(fn.receivedCtx)
	if !ok {
		t.Fatal("expected HandoffFromContext to return ok=true even on error path")
	}
	if !reflect.DeepEqual(gotParams, params) {
		t.Fatalf("expected ctx params=%+v, got %+v", params, gotParams)
	}
}

// TestLaunchHandoffCmdForwardsWorkflowEventsToChannel は task 3.3 の
// 「既存 startWorkflow と同じ events チャネルに WorkflowEvent をフォワードする
// パターンに合わせる」を観測する。
//
// 観測点: fake Fn が onEvent を呼んだとき、その WorkflowEvent が EventForwarder
// 経由で TUI シェル側に届けられること。本サブパッケージは tea.Msg 型へのラップを
// 行わない (TUI シェル側の責務) ため、forward 受領時の WorkflowEvent を直接
// 観測する。
func TestLaunchHandoffCmdForwardsWorkflowEventsToChannel(t *testing.T) {
	forwarded := make([]workflow.WorkflowEvent, 0, 2)
	forward := func(ev workflow.WorkflowEvent) { forwarded = append(forwarded, ev) }
	emitted := []workflow.WorkflowEvent{
		{Type: workflow.WorkflowText, Text: "hello"},
		{Type: workflow.WorkflowToolUse, Tool: "demo"},
	}
	fn := &fakeWorkflowFn{emit: emitted}
	def := workflow.Definition{
		ID:    "improve-workflow",
		Label: "Self Improve",
		Fn:    fn.capture(),
	}
	params := workflow.HandoffParams{
		TargetWorkflowID: "wf-1",
		RunIDs:           []string{"run-1"},
	}

	cmd := LaunchHandoffCmd(forward, noopAsk{}, def, params, "/cwd")
	msg := cmd()
	if _, ok := msg.(HandoffStartedMsg); !ok {
		t.Fatalf("expected HandoffStartedMsg, got %T", msg)
	}

	// forward callback に 2 件の WorkflowEvent が届いていることを観測
	if len(forwarded) != len(emitted) {
		t.Fatalf("expected %d forwarded events, got %d", len(emitted), len(forwarded))
	}
	for i, want := range emitted {
		got := forwarded[i]
		if got.Type != want.Type {
			t.Fatalf("event %d: expected type=%s, got %s", i, want.Type, got.Type)
		}
		if want.Type == workflow.WorkflowText && got.Text != want.Text {
			t.Fatalf("event %d: expected text=%q, got %q", i, want.Text, got.Text)
		}
		if want.Type == workflow.WorkflowToolUse && got.Tool != want.Tool {
			t.Fatalf("event %d: expected tool=%q, got %q", i, want.Tool, got.Tool)
		}
	}
}

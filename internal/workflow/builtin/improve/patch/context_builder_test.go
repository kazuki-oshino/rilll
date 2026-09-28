// Package improvepatch のテスト本ファイルは ContextBuilder 既定実装の
// 4 ケーステスト (通常 / Limited / Reader 失敗 / ArtifactReader 失敗) を提供する
// (tasks.md 8.1 / design.md §ContextBuilder / Req 2.1, 2.2, 2.3, 2.4, 2.5, 2.6)。
//
// 検証目的:
//   - akashic.Reader (Detail / List / FailureClusters) と akashic.ArtifactReader
//     (GetArtifacts / ReadArtifact) を fake で差し替えて、サンプリング件数 /
//     Limited フラグ / 失敗時 ErrContextLoad の sentinel chain を機械検証する。
//   - 起点 run のみのとき Limited=true、補完サンプルが取れたとき Limited=false。
//   - サイズが MaxArtifactBytes 超の artifact は Body 空、それ以下は ReadArtifact で
//     読み出して Body 非空とする契約を検証する (Req 2.4: AI 入力サイズ抑制)。
package patch

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kazuki-oshino/rilll/internal/akashic"
)

// --- fakes ---

// fakeReader は akashic.Reader interface を満たす最小 fake。
// ContextBuilder.Build が呼ぶ 3 メソッド (Detail / List / FailureClusters) のみ
// 振る舞いを差し替え可能にし、未使用メソッド (AggregateByWorkflow / AggregateByProvider)
// は呼ばれた場合に t.Fatalf で検出できるよう panic させる。
type fakeReader struct {
	t *testing.T

	// detailFn が nil の場合、Detail は ErrWorkflowRunNotFound を返す (= 未登録 ID)。
	detailFn  func(ctx context.Context, opts akashic.DetailOptions) (akashic.RunDetail, error)
	listFn    func(ctx context.Context, opts akashic.ListOptions) (akashic.ListResult, error)
	failureFn func(ctx context.Context, opts akashic.FailureOptions) (akashic.FailureClustersResult, error)

	detailCalls  int
	listCalls    int
	failureCalls int
}

func (f *fakeReader) Detail(ctx context.Context, opts akashic.DetailOptions) (akashic.RunDetail, error) {
	f.detailCalls++
	if f.detailFn == nil {
		return akashic.RunDetail{}, akashic.ErrWorkflowRunNotFound
	}
	return f.detailFn(ctx, opts)
}

func (f *fakeReader) List(ctx context.Context, opts akashic.ListOptions) (akashic.ListResult, error) {
	f.listCalls++
	if f.listFn == nil {
		return akashic.ListResult{}, nil
	}
	return f.listFn(ctx, opts)
}

func (f *fakeReader) FailureClusters(ctx context.Context, opts akashic.FailureOptions) (akashic.FailureClustersResult, error) {
	f.failureCalls++
	if f.failureFn == nil {
		return akashic.FailureClustersResult{}, nil
	}
	return f.failureFn(ctx, opts)
}

// AggregateByWorkflow / AggregateByProvider は ContextBuilder からは呼ばれない契約。
// 万一呼ばれた場合は t.Fatalf で検出する (関心分離契約の機械検証)。
func (f *fakeReader) AggregateByWorkflow(ctx context.Context, opts akashic.WorkflowAggregateOptions) (akashic.WorkflowAggregateResult, error) {
	f.t.Fatalf("AggregateByWorkflow が ContextBuilder から呼ばれてはならない")
	return akashic.WorkflowAggregateResult{}, nil
}

func (f *fakeReader) AggregateByProvider(ctx context.Context, opts akashic.ProviderAggregateOptions) (akashic.ProviderAggregateResult, error) {
	f.t.Fatalf("AggregateByProvider が ContextBuilder から呼ばれてはならない")
	return akashic.ProviderAggregateResult{}, nil
}

// InRunResolutionsForCluster / NextSuccessAcrossRuns は memory-distiller 専用の
// resolution-chain 拡張用メソッドで、ContextBuilder からは呼ばれない契約。
// 万一呼ばれた場合は t.Fatalf で検出する (関心分離契約の機械検証)。
func (f *fakeReader) InRunResolutionsForCluster(ctx context.Context, opts akashic.InRunResolutionOptions) (akashic.InRunResolutionsResult, error) {
	f.t.Fatalf("InRunResolutionsForCluster が ContextBuilder から呼ばれてはならない")
	return akashic.InRunResolutionsResult{}, nil
}

func (f *fakeReader) NextSuccessAcrossRuns(ctx context.Context, opts akashic.NextSuccessOptions) (akashic.NextSuccessResult, error) {
	f.t.Fatalf("NextSuccessAcrossRuns が ContextBuilder から呼ばれてはならない")
	return akashic.NextSuccessResult{}, nil
}

// AgentCallsByWorkflowRun は agent-learnings spec (task 3.2) で追加された Reader 拡張への
// 不可触ガード。ContextBuilder は agent-learnings 経路を経由しないため呼ばれてはならない。
func (f *fakeReader) AgentCallsByWorkflowRun(_ context.Context, _ string) ([]akashic.AgentCallRow, error) {
	f.t.Fatalf("AgentCallsByWorkflowRun が ContextBuilder から呼ばれてはならない")
	return nil, nil
}

// LearningsByWorkflowRun も同上 (ContextBuilder は learnings table を読まない)。
func (f *fakeReader) LearningsByWorkflowRun(_ context.Context, _ string) ([]akashic.LearningRow, error) {
	f.t.Fatalf("LearningsByWorkflowRun が ContextBuilder から呼ばれてはならない")
	return nil, nil
}

// fakeArtifactReader は akashic.ArtifactReader を満たす最小 fake。
// GetArtifacts / ReadArtifact を fn で差し替え可能にし、ReadArtifactUnlimited は
// ContextBuilder からは呼ばない契約のため呼ばれた場合 t.Fatalf で検出する。
type fakeArtifactReader struct {
	t *testing.T

	getFn  func(ctx context.Context, runID string) ([]akashic.ArtifactRow, error)
	readFn func(ctx context.Context, path string) ([]byte, error)

	getCalls  int
	readCalls int
}

func (f *fakeArtifactReader) GetArtifacts(ctx context.Context, runID string) ([]akashic.ArtifactRow, error) {
	f.getCalls++
	if f.getFn == nil {
		return nil, nil
	}
	return f.getFn(ctx, runID)
}

func (f *fakeArtifactReader) ReadArtifact(ctx context.Context, path string) ([]byte, error) {
	f.readCalls++
	if f.readFn == nil {
		return nil, nil
	}
	return f.readFn(ctx, path)
}

func (f *fakeArtifactReader) ReadArtifactUnlimited(ctx context.Context, path string) ([]byte, error) {
	f.t.Fatalf("ReadArtifactUnlimited は ContextBuilder から呼ばれない契約 (design.md §ContextBuilder)")
	return nil, nil
}

// --- helpers ---

// writeTempGoFile は cwd に dummy な Go ファイルを書き出して絶対 path を返す。
// CurrentSource 取得経路 (os.ReadFile) のテスト fixture として使用する。
func writeTempGoFile(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "git_commit.go")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("dummy go file の作成失敗: %v", err)
	}
	return path
}

// runHeader は test fixture 用の RunHeader builder。
func runHeader(runID, workflowID string, success *bool) akashic.RunHeader {
	now := time.Now().UTC()
	finished := now
	return akashic.RunHeader{
		RunID:        runID,
		WorkflowID:   workflowID,
		WorkflowName: workflowID,
		StartedAt:    now,
		FinishedAt:   &finished,
		Success:      success,
	}
}

func boolPtr(b bool) *bool { return &b }

// --- tests ---

// TestContextBuilder_Normal は通常経路 (origin + 補完 success/failure サンプル + 失敗クラスタ +
// artifact 読み出し) を検証する。観察可能完了条件:
//   - SampleRuns は 5 件 (origin 1 + success 2 + failure 2)
//   - Limited == false
//   - origin の IsOriginatedFromHandoff のみ true、残りは false
//   - artifact は size <= MaxArtifactBytes のものだけ Body 非空
//   - FailureClusters は Reader.FailureClusters の戻り値を引き継ぐ
//   - CurrentSource は TargetGoFile の os.ReadFile 結果と一致
func TestContextBuilder_Normal(t *testing.T) {
	src := "package workflow\n// dummy git_commit.go\n"
	targetFile := writeTempGoFile(t, src)
	originID := "run-origin"
	successIDs := []string{"run-s1", "run-s2"}
	failureIDs := []string{"run-f1", "run-f2"}

	// 各 run の Detail を返すマップを構築 (origin + samples)。
	detailMap := map[string]akashic.RunDetail{
		originID:      {Run: runHeader(originID, "git-commit", boolPtr(false))},
		successIDs[0]: {Run: runHeader(successIDs[0], "git-commit", boolPtr(true))},
		successIDs[1]: {Run: runHeader(successIDs[1], "git-commit", boolPtr(true))},
		failureIDs[0]: {Run: runHeader(failureIDs[0], "git-commit", boolPtr(false))},
		failureIDs[1]: {Run: runHeader(failureIDs[1], "git-commit", boolPtr(false))},
	}

	// artifact は origin run のみに 2 件 (small + large) 紐付け、size guard を検証する。
	smallPath := "/abs/.rilll/git-commit/small.json"
	largePath := "/abs/.rilll/git-commit/large.json"
	artifactBody := []byte(`{"hello": "world"}`)
	artifactsByRun := map[string][]akashic.ArtifactRow{
		originID: {
			{WorkflowRunID: originID, Kind: akashic.ArtifactKindResult, Path: smallPath, SizeBytes: int64(len(artifactBody))},
			{WorkflowRunID: originID, Kind: akashic.ArtifactKindContext, Path: largePath, SizeBytes: 99 * 1024 * 1024},
		},
	}

	reader := &fakeReader{
		t: t,
		detailFn: func(ctx context.Context, opts akashic.DetailOptions) (akashic.RunDetail, error) {
			d, ok := detailMap[opts.WorkflowRunID]
			if !ok {
				return akashic.RunDetail{}, akashic.ErrWorkflowRunNotFound
			}
			return d, nil
		},
		listFn: func(ctx context.Context, opts akashic.ListOptions) (akashic.ListResult, error) {
			// Success ポインタの中身で success / failure 分岐を区別する。
			if opts.Success == nil {
				t.Fatalf("ContextBuilder は List を Success フィルタ付きで呼ぶ契約: opts=%+v", opts)
			}
			if *opts.Success {
				return akashic.ListResult{Items: []akashic.WorkflowRunListItem{
					{RunHeader: detailMap[successIDs[0]].Run},
					{RunHeader: detailMap[successIDs[1]].Run},
				}}, nil
			}
			return akashic.ListResult{Items: []akashic.WorkflowRunListItem{
				{RunHeader: detailMap[failureIDs[0]].Run},
				{RunHeader: detailMap[failureIDs[1]].Run},
			}}, nil
		},
		failureFn: func(ctx context.Context, opts akashic.FailureOptions) (akashic.FailureClustersResult, error) {
			return akashic.FailureClustersResult{Clusters: []akashic.FailureCluster{
				{FailureCategory: "stream_idle_timeout", ProviderID: "claude", Count: 3},
			}}, nil
		},
	}

	artifacts := &fakeArtifactReader{
		t: t,
		getFn: func(ctx context.Context, runID string) ([]akashic.ArtifactRow, error) {
			return artifactsByRun[runID], nil
		},
		readFn: func(ctx context.Context, path string) ([]byte, error) {
			if path == smallPath {
				return artifactBody, nil
			}
			t.Fatalf("size guard 違反: large artifact %s が ReadArtifact 経由で読まれた", path)
			return nil, nil
		},
	}

	builder := NewDefaultContextBuilder(reader, artifacts)
	maxArtifactBytes := int64(1 * 1024 * 1024)
	pc, err := builder.Build(context.Background(), BuildInput{
		TargetWorkflowID: "git-commit",
		RunIDs:           []string{originID},
		TargetGoFile:     targetFile,
		SampleSuccess:    2,
		SampleFailure:    2,
		MaxRawEvents:     50,
		MaxArtifactBytes: maxArtifactBytes,
	})
	if err != nil {
		t.Fatalf("Build 失敗: %v", err)
	}

	// SampleRuns 件数: origin 1 + success 2 + failure 2 = 5
	if got, want := len(pc.SampleRuns), 5; got != want {
		t.Errorf("SampleRuns 件数: got %d, want %d", got, want)
	}
	if pc.Limited {
		t.Errorf("補完サンプルが取れた経路で Limited は false 期待: got true")
	}
	// 先頭 (= origin) のみ IsOriginatedFromHandoff == true
	if !pc.SampleRuns[0].IsOriginatedFromHandoff {
		t.Errorf("origin sample の IsOriginatedFromHandoff は true 期待: got false")
	}
	if pc.SampleRuns[0].Run.RunID != originID {
		t.Errorf("origin sample は SampleRuns[0]: got %q, want %q", pc.SampleRuns[0].Run.RunID, originID)
	}
	for i := 1; i < len(pc.SampleRuns); i++ {
		if pc.SampleRuns[i].IsOriginatedFromHandoff {
			t.Errorf("非 origin sample の IsOriginatedFromHandoff は false 期待: i=%d", i)
		}
	}

	// CurrentSource は target file の中身と一致
	if pc.CurrentSource != src {
		t.Errorf("CurrentSource mismatch: got %q, want %q", pc.CurrentSource, src)
	}
	if pc.TargetWorkflowID != "git-commit" {
		t.Errorf("TargetWorkflowID: got %q, want %q", pc.TargetWorkflowID, "git-commit")
	}
	if pc.TargetGoFile != targetFile {
		t.Errorf("TargetGoFile: got %q, want %q", pc.TargetGoFile, targetFile)
	}

	// FailureClusters 引き継ぎ
	if got, want := len(pc.FailureClusters), 1; got != want {
		t.Errorf("FailureClusters 件数: got %d, want %d", got, want)
	}

	// artifact size guard: small は Body 非空、large は Body 空 (size > MaxArtifactBytes)
	originArtifacts := pc.SampleRuns[0].Artifacts
	if got, want := len(originArtifacts), 2; got != want {
		t.Fatalf("origin artifacts 件数: got %d, want %d", got, want)
	}
	var smallSnippet, largeSnippet *ArtifactSnippet
	for i := range originArtifacts {
		switch originArtifacts[i].Path {
		case smallPath:
			smallSnippet = &originArtifacts[i]
		case largePath:
			largeSnippet = &originArtifacts[i]
		}
	}
	if smallSnippet == nil || largeSnippet == nil {
		t.Fatalf("small/large snippet 両方が SampleRuns[0].Artifacts に含まれることを期待: got %+v", originArtifacts)
	}
	if string(smallSnippet.Body) != string(artifactBody) {
		t.Errorf("small snippet Body は ReadArtifact 結果と一致を期待: got %q, want %q", smallSnippet.Body, artifactBody)
	}
	if len(largeSnippet.Body) != 0 {
		t.Errorf("large snippet (size > MaxArtifactBytes) は Body 空を期待: got len=%d", len(largeSnippet.Body))
	}

	// fake call 検証: ReadArtifact は size guard 通過した 1 件分のみ呼ばれる。
	if got := artifacts.readCalls; got != 1 {
		t.Errorf("ReadArtifact call count: got %d, want 1 (size guard 通過 1 件のみ)", got)
	}
}

// TestContextBuilder_Limited は補完サンプルが取得できないとき Limited=true が立つことを検証する。
//
// シナリオ:
//   - Reader.Detail は origin を返すが
//   - Reader.List は success/failure いずれも 0 件
//   - Reader.FailureClusters も空
//   - ArtifactReader.GetArtifacts も origin に対して空
//
// 期待:
//   - SampleRuns 件数 1 (origin のみ)
//   - Limited == true
//   - err == nil
func TestContextBuilder_Limited(t *testing.T) {
	src := "package workflow\n"
	targetFile := writeTempGoFile(t, src)
	originID := "run-origin-only"

	reader := &fakeReader{
		t: t,
		detailFn: func(ctx context.Context, opts akashic.DetailOptions) (akashic.RunDetail, error) {
			if opts.WorkflowRunID != originID {
				return akashic.RunDetail{}, akashic.ErrWorkflowRunNotFound
			}
			return akashic.RunDetail{Run: runHeader(originID, "git-commit", boolPtr(false))}, nil
		},
		// listFn / failureFn 未設定 (default = 空 Result + nil error)
	}
	artifacts := &fakeArtifactReader{t: t}

	builder := NewDefaultContextBuilder(reader, artifacts)
	pc, err := builder.Build(context.Background(), BuildInput{
		TargetWorkflowID: "git-commit",
		RunIDs:           []string{originID},
		TargetGoFile:     targetFile,
	})
	if err != nil {
		t.Fatalf("Build 失敗: %v", err)
	}
	if got, want := len(pc.SampleRuns), 1; got != want {
		t.Errorf("SampleRuns 件数: got %d, want %d (origin のみ)", got, want)
	}
	if !pc.Limited {
		t.Errorf("origin run のみのとき Limited=true を期待: got false")
	}
	if !pc.SampleRuns[0].IsOriginatedFromHandoff {
		t.Errorf("origin sample の IsOriginatedFromHandoff は true 期待: got false")
	}
}

// TestContextBuilder_ReaderFailure は Reader.Detail (origin) が失敗したとき
// ErrContextLoad を errors.Is でチェイン検出可能な形でラップして返すことを検証する。
//
// 観察可能完了条件:
//   - err != nil
//   - errors.Is(err, ErrContextLoad) == true
//   - PromptContext は zero value (CurrentSource 等が空)
func TestContextBuilder_ReaderFailure(t *testing.T) {
	targetFile := writeTempGoFile(t, "package workflow\n")
	wantErr := errors.New("simulated reader detail failure")

	reader := &fakeReader{
		t: t,
		detailFn: func(ctx context.Context, opts akashic.DetailOptions) (akashic.RunDetail, error) {
			return akashic.RunDetail{}, wantErr
		},
	}
	artifacts := &fakeArtifactReader{t: t}

	builder := NewDefaultContextBuilder(reader, artifacts)
	pc, err := builder.Build(context.Background(), BuildInput{
		TargetWorkflowID: "git-commit",
		RunIDs:           []string{"run-x"},
		TargetGoFile:     targetFile,
	})
	if err == nil {
		t.Fatalf("Reader.Detail 失敗時 error を期待: got nil (PromptContext=%+v)", pc)
	}
	if !errors.Is(err, ErrContextLoad) {
		t.Errorf("error chain: %v は ErrContextLoad を errors.Is で検出可能を期待", err)
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("error chain: %v は元 error %v を errors.Is で検出可能を期待", err, wantErr)
	}
	if pc.CurrentSource != "" || len(pc.SampleRuns) != 0 {
		t.Errorf("error 経路では PromptContext は zero value 期待: got %+v", pc)
	}
}

// TestContextBuilder_ArtifactReaderFailure は ArtifactReader.GetArtifacts が失敗したとき
// ErrContextLoad を errors.Is でチェイン検出可能な形でラップして返すことを検証する。
//
// 観察可能完了条件:
//   - err != nil
//   - errors.Is(err, ErrContextLoad) == true
//   - 失敗対象の sentinel が chain 経由で検出可能
func TestContextBuilder_ArtifactReaderFailure(t *testing.T) {
	targetFile := writeTempGoFile(t, "package workflow\n")
	originID := "run-origin"
	wantErr := errors.New("simulated artifact reader failure")

	reader := &fakeReader{
		t: t,
		detailFn: func(ctx context.Context, opts akashic.DetailOptions) (akashic.RunDetail, error) {
			return akashic.RunDetail{Run: runHeader(originID, "git-commit", boolPtr(false))}, nil
		},
	}
	artifacts := &fakeArtifactReader{
		t: t,
		getFn: func(ctx context.Context, runID string) ([]akashic.ArtifactRow, error) {
			return nil, wantErr
		},
	}

	builder := NewDefaultContextBuilder(reader, artifacts)
	_, err := builder.Build(context.Background(), BuildInput{
		TargetWorkflowID: "git-commit",
		RunIDs:           []string{originID},
		TargetGoFile:     targetFile,
	})
	if err == nil {
		t.Fatalf("ArtifactReader.GetArtifacts 失敗時 error を期待: got nil")
	}
	if !errors.Is(err, ErrContextLoad) {
		t.Errorf("error chain: %v は ErrContextLoad を errors.Is で検出可能を期待", err)
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("error chain: %v は元 error %v を errors.Is で検出可能を期待", err, wantErr)
	}
}

// TestContextBuilder_BuildInputDefaults は BuildInput の各フィールドがゼロ値で
// 渡された場合に default 値が適用されることを検証する (BuildInput.MaxRegenerateAttempts デフォルト 5)。
//
// 観察可能完了条件:
//   - MaxRegenerateAttempts ゼロ値時に DefaultMaxRegenerateAttempts (=5) が適用される
//   - 他デフォルト (SampleSuccess=3 / SampleFailure=5 / MaxRawEvents=50) も適用される
//
// 本テストは BuildInput.WithDefaults を直接検証する単体テストとして配置する
// (Build 経由だとデフォルト値が外部から観測しづらいため)。
func TestContextBuilder_BuildInputDefaults(t *testing.T) {
	in := BuildInput{}.WithDefaults()
	if in.SampleSuccess != DefaultSampleSuccess {
		t.Errorf("SampleSuccess default: got %d, want %d", in.SampleSuccess, DefaultSampleSuccess)
	}
	if in.SampleFailure != DefaultSampleFailure {
		t.Errorf("SampleFailure default: got %d, want %d", in.SampleFailure, DefaultSampleFailure)
	}
	if in.MaxRawEvents != DefaultMaxRawEvents {
		t.Errorf("MaxRawEvents default: got %d, want %d", in.MaxRawEvents, DefaultMaxRawEvents)
	}
	if in.MaxArtifactBytes != akashic.DefaultArtifactReadMaxBytes {
		t.Errorf("MaxArtifactBytes default: got %d, want %d", in.MaxArtifactBytes, akashic.DefaultArtifactReadMaxBytes)
	}
	if in.MaxRegenerateAttempts != DefaultMaxRegenerateAttempts {
		t.Errorf("MaxRegenerateAttempts default: got %d, want %d", in.MaxRegenerateAttempts, DefaultMaxRegenerateAttempts)
	}
	if DefaultMaxRegenerateAttempts != 5 {
		t.Errorf("DefaultMaxRegenerateAttempts は 5 (Resolved Decisions §2 / inquiryloop と整合): got %d", DefaultMaxRegenerateAttempts)
	}
}

package inquiryloop

import (
	"context"
	"testing"

	"github.com/kazuki-oshino/rilll/internal/provider"
)

// 公開定数の文字列値が design.md / requirements.md と一致することを固定する。
// これらは workflow.RunTerminationReason に「文字列値そのまま」昇格する契約のため、
// 値が変わると下流の result.json.terminationReason フォーマットが壊れる。
func TestStatusConstants(t *testing.T) {
	cases := []struct {
		got  Status
		want string
	}{
		{StatusCompleted, "completed"},
		{StatusAborted, "aborted"},
	}
	for _, c := range cases {
		if string(c.got) != c.want {
			t.Errorf("status %q = %q, want %q", c.want, string(c.got), c.want)
		}
	}
}

func TestTerminationReasonConstants(t *testing.T) {
	cases := []struct {
		got  TerminationReason
		want string
	}{
		{ReasonCompleted, "completed"},
		{ReasonLoopLimit, "loop-limit"},
		{ReasonPromptFailure, "prompt-failure"},
		{ReasonSchemaViolation, "schema-violation"},
	}
	for _, c := range cases {
		if string(c.got) != c.want {
			t.Errorf("termination %q = %q, want %q", c.want, string(c.got), c.want)
		}
	}
}

// AgentCaller は型としての契約のみが本タスクの責務。Engine 実装 (Task 2.2) で
// この interface を充足する fakeAgent を書きたいので、シグネチャをコンパイル時に固定する。
type fakeAgentCaller struct {
	called int
}

func (f *fakeAgentCaller) Call(
	_ context.Context,
	_ string,
	_ string,
	_ provider.AgentEventCallback,
	_ string,
	_ *provider.OutputFormat,
	_ string,
	_ string,
) (provider.AgentResponse, error) {
	f.called++
	return provider.AgentResponse{Success: true}, nil
}

// AgentCaller を満たす型を変数として保持できることを確認する（コンパイル時の契約検証）。
func TestAgentCallerInterfaceContract(t *testing.T) {
	var _ AgentCaller = (*fakeAgentCaller)(nil)
}

// 公開構造体は将来 engine.go から参照される。本タスクではフィールド名と型だけ固定する。
func TestPublicStructFields(t *testing.T) {
	in := RunInput{PromptCommand: "/kiro-spec-init demo", PromptLabel: "01-spec-init"}
	if in.PromptCommand == "" || in.PromptLabel == "" {
		t.Fatalf("RunInput fields not assignable")
	}

	opt := InquiryOption{Label: "A", Description: "alpha"}
	inq := Inquiry{Title: "T", Background: "B", Question: "Q", Options: []InquiryOption{opt}}
	if inq.Title == "" || len(inq.Options) != 1 {
		t.Fatalf("Inquiry fields not assignable")
	}

	round := Round{
		Index:         1,
		SessionID:     "s1",
		Inquiry:       inq,
		AnswerText:    "ans",
		AnswerSession: "b1",
	}
	if round.Index != 1 || round.SessionID != "s1" {
		t.Fatalf("Round fields not assignable")
	}

	fail := FailureDetail{
		Round:      2,
		Reason:     ReasonSchemaViolation,
		Message:    "broken",
		RawContent: "raw",
	}
	if fail.Round != 2 || fail.Reason != ReasonSchemaViolation || fail.Message != "broken" || fail.RawContent != "raw" {
		t.Fatalf("FailureDetail fields not round-trippable: %+v", fail)
	}

	final := FinalResult{
		Status:      StatusCompleted,
		Termination: ReasonCompleted,
		Rounds:      0,
		Summary:     "ok",
		LastSession: "s2",
		Trail:       []Round{round},
		Failure:     &fail,
	}
	if final.Status != StatusCompleted ||
		final.Termination != ReasonCompleted ||
		final.Rounds != 0 ||
		final.Summary != "ok" ||
		final.LastSession != "s2" ||
		len(final.Trail) != 1 ||
		final.Failure == nil {
		t.Fatalf("FinalResult fields not round-trippable: %+v", final)
	}
}

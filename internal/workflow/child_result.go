package workflow

// EventCapturePolicy は child event metadata を ChildResult に残す条件を定義する。
// zero value は event を保持しない。Predicate と MaxEvents の両方が指定された場合のみ、
// matching event を MaxEvents 件まで保持する。
type EventCapturePolicy struct {
	Predicate func(WorkflowEvent) bool
	MaxEvents int
}

func (p EventCapturePolicy) active() bool {
	return p.Predicate != nil && p.MaxEvents > 0
}

// ChildResult は child workflow completion の観測結果を表す。
// domain-specific な意味付けは持たず、consumer callback が必要な情報だけを解釈する。
type ChildResult struct {
	DefinitionID              string
	Label                     string
	RunID                     string
	ReturnedError             error
	FirstWorkflowError        *WorkflowEvent
	CapturedEvents            []WorkflowEvent
	CapturedEventLimitReached bool
}

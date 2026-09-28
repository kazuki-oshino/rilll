package workflow

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestParseOrderedCSVPreservesOrderAndTrimsTokens(t *testing.T) {
	got, err := ParseOrderedCSV(" beta,alpha ,gamma")
	if err != nil {
		t.Fatalf("ParseOrderedCSV returned error: %v", err)
	}
	want := []string{"beta", "alpha", "gamma"}
	if len(got) != len(want) {
		t.Fatalf("ParseOrderedCSV length = %d, want %d: %#v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ParseOrderedCSV[%d] = %q, want %q; full result %#v", i, got[i], want[i], got)
		}
	}
}

func TestParseOrderedCSVRejectsEmptyTokens(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{name: "empty value", input: ""},
		{name: "leading comma", input: ",alpha"},
		{name: "middle comma", input: "beta,,alpha"},
		{name: "trailing comma", input: "beta,"},
		{name: "blank token", input: "beta,  ,alpha"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseOrderedCSV(tt.input)
			if err == nil {
				t.Fatalf("ParseOrderedCSV(%q) returned nil error", tt.input)
			}
			if !strings.Contains(err.Error(), "empty token") {
				t.Fatalf("ParseOrderedCSV(%q) error = %q, want empty token diagnostic", tt.input, err.Error())
			}
		})
	}
}

func TestValidateOrderedSelectionAcceptsInputOrder(t *testing.T) {
	params := orderedSelectTestParams()
	selected := []string{"beta", "alpha"}
	if err := ValidateOrderedSelection(params, selected); err != nil {
		t.Fatalf("ValidateOrderedSelection returned error: %v", err)
	}
	if selected[0] != "beta" || selected[1] != "alpha" {
		t.Fatalf("ValidateOrderedSelection mutated order: %#v", selected)
	}
}

func TestValidateOrderedSelectionRejectsEmptyUnknownAndDuplicate(t *testing.T) {
	tests := []struct {
		name     string
		selected []string
		want     []string
	}{
		{
			name:     "empty list",
			selected: []string{},
			want:     []string{"--features", "at least 1", "alpha, beta"},
		},
		{
			name:     "unknown feature",
			selected: []string{"gamma"},
			want:     []string{"--features", "unknown feature", "gamma", "alpha, beta"},
		},
		{
			name:     "duplicate feature",
			selected: []string{"alpha", "beta", "alpha"},
			want:     []string{"--features", "duplicate feature", "alpha", "alpha, beta"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateOrderedSelection(orderedSelectTestParams(), tt.selected)
			if err == nil {
				t.Fatalf("ValidateOrderedSelection(%#v) returned nil error", tt.selected)
			}
			msg := err.Error()
			for _, want := range tt.want {
				if !strings.Contains(msg, want) {
					t.Fatalf("ValidateOrderedSelection(%#v) error = %q, want substring %q", tt.selected, msg, want)
				}
			}
		})
	}
}

func TestSelectOrderedUsesOptionalSeamAndValidatesResult(t *testing.T) {
	ask := &fakeOrderedWorkflowAsk{ids: []string{"beta", "alpha"}, ok: true}
	got, ok, err := SelectOrdered(context.Background(), ask, orderedSelectTestParams())
	if err != nil {
		t.Fatalf("SelectOrdered returned error: %v", err)
	}
	if !ok {
		t.Fatalf("SelectOrdered ok=false, want true")
	}
	want := []string{"beta", "alpha"}
	if len(got) != len(want) {
		t.Fatalf("SelectOrdered length = %d, want %d: %#v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("SelectOrdered[%d] = %q, want %q; full result %#v", i, got[i], want[i], got)
		}
	}
	if ask.singleSelectCalls != 0 {
		t.Fatalf("SelectOrdered called single Select %d times", ask.singleSelectCalls)
	}
	if len(ask.orderedParams) != 1 || ask.orderedParams[0].Key != "features" {
		t.Fatalf("SelectOrdered params = %#v, want one ordered call with key features", ask.orderedParams)
	}
}

func TestSelectOrderedReturnsClearErrorWhenAskDoesNotSupportOrderedSelection(t *testing.T) {
	_, _, err := SelectOrdered(context.Background(), singleOnlyAsk{}, orderedSelectTestParams())
	if err == nil {
		t.Fatal("SelectOrdered returned nil error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "ordered selection") || !strings.Contains(msg, "not supported") {
		t.Fatalf("SelectOrdered error = %q, want clear unsupported ordered selection diagnostic", msg)
	}
}

func TestSelectOrderedPropagatesCancelAndAskErrors(t *testing.T) {
	expectedErr := errors.New("ask failed")
	tests := []struct {
		name    string
		ask     *fakeOrderedWorkflowAsk
		wantOK  bool
		wantErr error
	}{
		{name: "cancel", ask: &fakeOrderedWorkflowAsk{ok: false}, wantOK: false},
		{name: "ask error", ask: &fakeOrderedWorkflowAsk{ok: false, err: expectedErr}, wantOK: false, wantErr: expectedErr},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, ok, err := SelectOrdered(context.Background(), tt.ask, orderedSelectTestParams())
			if ok != tt.wantOK {
				t.Fatalf("SelectOrdered ok = %v, want %v", ok, tt.wantOK)
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("SelectOrdered err = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestSelectOrderedRejectsInvalidIDsReturnedByAsk(t *testing.T) {
	ask := &fakeOrderedWorkflowAsk{ids: []string{"gamma"}, ok: true}
	_, _, err := SelectOrdered(context.Background(), ask, orderedSelectTestParams())
	if err == nil {
		t.Fatal("SelectOrdered returned nil error")
	}
	msg := err.Error()
	for _, want := range []string{"unknown feature", "gamma", "alpha, beta"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("SelectOrdered error = %q, want substring %q", msg, want)
		}
	}
}

func orderedSelectTestParams() OrderedSelectParams {
	return OrderedSelectParams{
		Key:             "features",
		Prompt:          "select features",
		Items:           []SelectItem{{ID: "alpha", Label: "Alpha"}, {ID: "beta", Label: "Beta"}},
		MinItems:        1,
		DuplicatePolicy: DuplicatePolicyReject,
	}
}

type fakeOrderedWorkflowAsk struct {
	ids               []string
	ok                bool
	err               error
	singleSelectCalls int
	orderedParams     []OrderedSelectParams
}

func (f *fakeOrderedWorkflowAsk) Select(_ context.Context, _ SelectParams) (string, bool, error) {
	f.singleSelectCalls++
	return "", false, errors.New("single select should not be called")
}

func (f *fakeOrderedWorkflowAsk) SelectOrdered(_ context.Context, params OrderedSelectParams) ([]string, bool, error) {
	f.orderedParams = append(f.orderedParams, params)
	return f.ids, f.ok, f.err
}

type singleOnlyAsk struct{}

func (singleOnlyAsk) Select(context.Context, SelectParams) (string, bool, error) {
	return "", false, nil
}

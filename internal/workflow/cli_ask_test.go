package workflow

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestCLIAskReturnsValueWhenPresent(t *testing.T) {
	ask := CLIAsk{Values: map[string]string{"feature": "my-feature"}}
	id, ok, err := ask.Select(context.Background(), SelectParams{
		Key:   "feature",
		Items: []SelectItem{{ID: "my-feature", Label: "my-feature"}, {ID: "other", Label: "other"}},
	})
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if !ok {
		t.Fatalf("expected ok=true")
	}
	if id != "my-feature" {
		t.Fatalf("expected id=my-feature, got %q", id)
	}
}

func TestCLIAskMissingValueReturnsErrorWithChoices(t *testing.T) {
	ask := CLIAsk{Values: map[string]string{}}
	_, _, err := ask.Select(context.Background(), SelectParams{
		Key:   "feature",
		Items: []SelectItem{{ID: "a", Label: "a"}, {ID: "b", Label: "b"}},
	})
	if err == nil {
		t.Fatal("expected error for missing value")
	}
	var askErr *CLIAskError
	if !errors.As(err, &askErr) {
		t.Fatalf("expected *CLIAskError sentinel, got %T", err)
	}
	if askErr.Key != "feature" {
		t.Errorf("expected key=feature, got %q", askErr.Key)
	}
	msg := err.Error()
	if !strings.Contains(msg, "--feature") {
		t.Errorf("expected error to mention --feature, got %q", msg)
	}
	if !strings.Contains(msg, "a") || !strings.Contains(msg, "b") {
		t.Errorf("expected error to list available choices, got %q", msg)
	}
}

func TestCLIAskInvalidValueReturnsErrorWithChoices(t *testing.T) {
	ask := CLIAsk{Values: map[string]string{"feature": "missing"}}
	_, _, err := ask.Select(context.Background(), SelectParams{
		Key:   "feature",
		Items: []SelectItem{{ID: "a", Label: "a"}, {ID: "b", Label: "b"}},
	})
	if err == nil {
		t.Fatal("expected error for invalid value")
	}
	msg := err.Error()
	if !strings.Contains(msg, "--feature=") {
		t.Errorf("expected error to mention --feature=, got %q", msg)
	}
	if !strings.Contains(msg, "missing") {
		t.Errorf("expected error to echo invalid value, got %q", msg)
	}
}

func TestCLIAskEmptyItemsStillReportsMissing(t *testing.T) {
	ask := CLIAsk{}
	_, _, err := ask.Select(context.Background(), SelectParams{
		Key:   "feature",
		Items: nil,
	})
	if err == nil {
		t.Fatal("expected error when Items empty and value missing")
	}
	if !strings.Contains(err.Error(), "(なし)") {
		t.Errorf("expected '(なし)' marker for empty Items, got %q", err.Error())
	}
}

func TestCLIAskRejectsEmptyKey(t *testing.T) {
	ask := CLIAsk{Values: map[string]string{"": "x"}}
	_, _, err := ask.Select(context.Background(), SelectParams{Key: ""})
	if err == nil {
		t.Fatal("expected error when Key is empty")
	}
}

func TestCLIAskSelectOrderedReturnsCommaSeparatedValueInInputOrder(t *testing.T) {
	ask := CLIAsk{Values: map[string]string{"features": "beta,alpha"}}
	ids, ok, err := ask.SelectOrdered(context.Background(), cliAskOrderedParams())
	if err != nil {
		t.Fatalf("SelectOrdered returned error: %v", err)
	}
	if !ok {
		t.Fatal("SelectOrdered ok=false, want true")
	}
	want := []string{"beta", "alpha"}
	if len(ids) != len(want) {
		t.Fatalf("SelectOrdered length = %d, want %d: %#v", len(ids), len(want), ids)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("SelectOrdered[%d] = %q, want %q; full result %#v", i, ids[i], want[i], ids)
		}
	}
}

func TestCLIAskSelectOrderedReturnsCLIAskErrorForMisuse(t *testing.T) {
	tests := []struct {
		name   string
		ask    CLIAsk
		want   []string
		errKey string
	}{
		{
			name:   "missing features flag",
			ask:    CLIAsk{Values: map[string]string{}},
			want:   []string{"--features", "alpha, beta"},
			errKey: "features",
		},
		{
			name:   "empty features flag",
			ask:    CLIAsk{Values: map[string]string{"features": ""}},
			want:   []string{"--features", "alpha, beta"},
			errKey: "features",
		},
		{
			name:   "empty token",
			ask:    CLIAsk{Values: map[string]string{"features": "alpha,,beta"}},
			want:   []string{"--features", "empty token", "alpha, beta"},
			errKey: "features",
		},
		{
			name:   "unknown feature",
			ask:    CLIAsk{Values: map[string]string{"features": "gamma"}},
			want:   []string{"--features", "unknown feature", "gamma", "alpha, beta"},
			errKey: "features",
		},
		{
			name:   "duplicate feature",
			ask:    CLIAsk{Values: map[string]string{"features": "alpha,beta,alpha"}},
			want:   []string{"--features", "duplicate feature", "alpha", "alpha, beta"},
			errKey: "features",
		},
		{
			name:   "json array is not accepted",
			ask:    CLIAsk{Values: map[string]string{"features": `["beta","alpha"]`}},
			want:   []string{"--features", "unknown feature", "alpha, beta"},
			errKey: "features",
		},
		{
			name:   "repeated flag is not accepted",
			ask:    CLIAsk{Values: map[string]string{"features": "alpha"}, Counts: map[string]int{"features": 2}},
			want:   []string{"--features", "複数回", "alpha, beta"},
			errKey: "features",
		},
		{
			name:   "singular repeated flag surface is not accepted",
			ask:    CLIAsk{Values: map[string]string{"feature": "beta,alpha"}},
			want:   []string{"--features", "alpha, beta"},
			errKey: "features",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := tt.ask.SelectOrdered(context.Background(), cliAskOrderedParams())
			if err == nil {
				t.Fatal("SelectOrdered returned nil error")
			}
			var askErr *CLIAskError
			if !errors.As(err, &askErr) {
				t.Fatalf("expected *CLIAskError sentinel, got %T", err)
			}
			if askErr.Key != tt.errKey {
				t.Fatalf("CLIAskError.Key = %q, want %q", askErr.Key, tt.errKey)
			}
			msg := err.Error()
			for _, want := range tt.want {
				if !strings.Contains(msg, want) {
					t.Fatalf("SelectOrdered error = %q, want substring %q", msg, want)
				}
			}
		})
	}
}

func TestCLIAskSelectOrderedRejectsEmptyKey(t *testing.T) {
	ask := CLIAsk{Values: map[string]string{"": "alpha,beta"}}
	_, _, err := ask.SelectOrdered(context.Background(), OrderedSelectParams{Key: ""})
	if err == nil {
		t.Fatal("expected error when Key is empty")
	}
}

func cliAskOrderedParams() OrderedSelectParams {
	return OrderedSelectParams{
		Key:             "features",
		Prompt:          "select features",
		Items:           []SelectItem{{ID: "alpha", Label: "Alpha"}, {ID: "beta", Label: "Beta"}},
		MinItems:        1,
		DuplicatePolicy: DuplicatePolicyReject,
	}
}

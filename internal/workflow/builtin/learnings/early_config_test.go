package learnings

import (
	"strings"
	"testing"
	"time"
)

func TestEarlyReflectionWaitBudgetConfig(t *testing.T) {
	tests := []struct {
		name       string
		envValue   string
		want       time.Duration
		wantWarns  int
		wantInWarn string
	}{
		{
			name:     "default when unset",
			envValue: "",
			want:     defaultEarlyReflectionWaitBudget,
		},
		{
			name:     "valid milliseconds",
			envValue: "2500",
			want:     2500 * time.Millisecond,
		},
		{
			name:     "trims whitespace",
			envValue: " 15 ",
			want:     15 * time.Millisecond,
		},
		{
			name:       "invalid falls back",
			envValue:   "not-a-number",
			want:       defaultEarlyReflectionWaitBudget,
			wantWarns:  1,
			wantInWarn: "not-a-number",
		},
		{
			name:       "negative falls back",
			envValue:   "-1",
			want:       defaultEarlyReflectionWaitBudget,
			wantWarns:  1,
			wantInWarn: "-1",
		},
		{
			name:       "zero falls back",
			envValue:   "0",
			want:       defaultEarlyReflectionWaitBudget,
			wantWarns:  1,
			wantInWarn: "0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(envRillLearningsEarlyWaitMS, tt.envValue)
			captured := withLearningsWarnLog(t)

			got := resolveEarlyReflectionWaitBudget()
			if got != tt.want {
				t.Fatalf("resolveEarlyReflectionWaitBudget()=%s, want %s", got, tt.want)
			}
			if len(*captured) != tt.wantWarns {
				t.Fatalf("warning count=%d, want %d: %q", len(*captured), tt.wantWarns, *captured)
			}
			if tt.wantInWarn != "" {
				if !strings.Contains((*captured)[0], envRillLearningsEarlyWaitMS) {
					t.Fatalf("warning should include env var name: %q", (*captured)[0])
				}
				if !strings.Contains((*captured)[0], tt.wantInWarn) {
					t.Fatalf("warning should include invalid value %q: %q", tt.wantInWarn, (*captured)[0])
				}
			}
		})
	}
}

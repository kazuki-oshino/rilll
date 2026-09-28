package kiro

import (
	"errors"
	"strings"
	"testing"

	"github.com/kazuki-oshino/rilll/internal/provider/middleware/streamidle"
)

func TestFormatStepErrorIncludesStreamIdleCategory(t *testing.T) {
	got := formatStepError("3.2", "実装エージェント", streamidle.ErrStreamIdleTimeout)
	if !strings.Contains(got, "[stream_idle_timeout]") {
		t.Fatalf("expected stream_idle_timeout prefix, got %q", got)
	}
	if !strings.Contains(got, "[3.2]") || !strings.Contains(got, "実装エージェント") {
		t.Fatalf("expected step id and label, got %q", got)
	}
}

func TestFormatStepErrorOmitsCategoryWhenUnknown(t *testing.T) {
	got := formatStepError("3.2", "実装エージェント", errors.New("boom"))
	if strings.Contains(got, "[stream_idle_timeout]") {
		t.Fatalf("non-idle errors should not get the idle prefix: %q", got)
	}
	if !strings.HasPrefix(got, "中断: [3.2]") {
		t.Fatalf("expected legacy prefix, got %q", got)
	}
}

func TestFormatStepErrorAcceptsNilError(t *testing.T) {
	got := formatStepError("3.2", "実装エージェント", nil)
	if strings.Contains(got, "[stream_idle_timeout]") {
		t.Fatalf("nil error should not get idle prefix: %q", got)
	}
}

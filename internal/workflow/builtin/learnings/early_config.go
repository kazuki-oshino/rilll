package learnings

import (
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	envRillLearningsEarlyWaitMS      = "RILLL_LEARNINGS_EARLY_WAIT_MS"
	defaultEarlyReflectionWaitMS     = 3000
	defaultEarlyReflectionWaitBudget = defaultEarlyReflectionWaitMS * time.Millisecond
)

func resolveEarlyReflectionWaitBudget() time.Duration {
	raw := strings.TrimSpace(os.Getenv(envRillLearningsEarlyWaitMS))
	if raw == "" {
		return defaultEarlyReflectionWaitBudget
	}
	ms, err := strconv.Atoi(raw)
	if err != nil {
		learningsWarnLog(
			"%s=%q を整数ミリ秒として解釈できません。default=%dms にフォールバックします: %v",
			envRillLearningsEarlyWaitMS, raw, defaultEarlyReflectionWaitMS, err,
		)
		return defaultEarlyReflectionWaitBudget
	}
	if ms < 1 {
		learningsWarnLog(
			"%s=%d は >= 1 である必要があります。default=%dms にフォールバックします",
			envRillLearningsEarlyWaitMS, ms, defaultEarlyReflectionWaitMS,
		)
		return defaultEarlyReflectionWaitBudget
	}
	return time.Duration(ms) * time.Millisecond
}

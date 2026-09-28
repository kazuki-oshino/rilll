package workflow

import (
	"fmt"
	"time"
)

// generateRunID はワークフロー run の一意 ID を生成する。
// `<UTC ISO8601>-<UnixNano>` 形式で、Definition.Run と RunIDFromContext (RunMetadata 不在時) から呼ばれる。
func generateRunID() string {
	return fmt.Sprintf("%s-%d", time.Now().UTC().Format("2006-01-02T15-04-05"), time.Now().UnixNano())
}

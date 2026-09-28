package kiro

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestLegacyPersonaProductionTreeIsRemoved(t *testing.T) {
	legacyDir := filepath.Join("..", "..", "..", "persona")
	legacyAssetDir := filepath.Join(legacyDir, "personas")

	for _, path := range []string{legacyDir, legacyAssetDir} {
		_, err := os.Stat(path)
		if !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("legacy session B persona production tree still exists at %s; err=%v", path, err)
		}
	}
}

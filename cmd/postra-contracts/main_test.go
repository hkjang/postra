package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"postra/internal/transport/httpapi/contracts"
)

func TestCheckRejectsDriftWithoutOverwritingFiles(t *testing.T) {
	root := t.TempDir()
	if err := run(root, true); err == nil {
		t.Fatal("missing artifacts accepted")
	}
	if err := run(root, false); err != nil {
		t.Fatal(err)
	}
	if err := run(root, true); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, filepath.FromSlash(contracts.TypeScriptPath))
	drift := []byte("// deliberate test drift\n")
	if err := os.WriteFile(path, drift, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run(root, true); err == nil {
		t.Fatal("changed artifact accepted")
	}
	actual, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(actual, drift) {
		t.Fatal("check mode overwrote an artifact")
	}
}

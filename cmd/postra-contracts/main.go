// postra-contracts deterministically generates the checked-in core API contract.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"postra/internal/transport/httpapi/contracts"
)

func main() {
	check := flag.Bool("check", false, "verify checked-in artifacts without writing")
	root := flag.String("root", ".", "repository root")
	flag.Parse()
	if err := run(*root, *check); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run(root string, check bool) error {
	files, err := contracts.Artifacts()
	if err != nil {
		return err
	}
	paths := make([]string, 0, len(files))
	for path := range files {
		paths = append(paths, path)
	}
	slices.Sort(paths)
	stale := []string{}
	for _, path := range paths {
		absolute := filepath.Join(root, filepath.FromSlash(path))
		if check {
			existing, err := os.ReadFile(absolute) // #nosec G304 -- Developer-selected repository root and fixed generator-owned relative filenames; no server input.
			if err != nil || !bytes.Equal(existing, files[path]) {
				stale = append(stale, path)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(absolute), 0o750); err != nil {
			return err
		}
		if err := os.WriteFile(absolute, files[path], 0o600); err != nil {
			return err
		} // #nosec G703 -- Only fixed generated public contract filenames under the developer-selected root.
		fmt.Println("generated", path)
	}
	if len(stale) > 0 {
		return fmt.Errorf("API contract drift in %v; run go run ./cmd/postra-contracts and review/commit the generated diff", stale)
	}
	return nil
}

// mkimage assembles a docker-load-compatible image tarball for the fully
// static postra binary, without needing a Docker daemon. The resulting
// image can be loaded on an offline host with:  docker load -i postra-image.tar.gz
package main

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"time"
)

func main() {
	binPath := os.Args[1]
	outPath := os.Args[2]
	tag := os.Args[3]

	bin, err := os.ReadFile(binPath)
	must(err)

	var caCerts []byte
	for _, p := range []string{
		"/etc/ssl/certs/ca-certificates.crt",
		"/etc/pki/tls/certs/ca-bundle.crt",
	} {
		if b, err := os.ReadFile(p); err == nil {
			caCerts = b
			break
		}
	}

	now := time.Now().UTC()

	// ---- build the rootfs layer tar (uncompressed) ----
	var layer bytes.Buffer
	lw := tar.NewWriter(&layer)
	writeDir(lw, "usr/", now)
	writeDir(lw, "usr/local/", now)
	writeDir(lw, "usr/local/bin/", now)
	writeFile(lw, "usr/local/bin/postra", bin, 0o755, now)
	writeDir(lw, "app/", now)
	writeDirMode(lw, "data/", 0o777, now) // world-writable so any mounted volume is usable
	writeDirMode(lw, "tmp/", 0o777, now)
	writeDir(lw, "etc/", now)
	// minimal passwd/group so the runtime has a resolvable root user
	writeFile(lw, "etc/passwd", []byte("root:x:0:0:root:/root:/sbin/nologin\n"), 0o644, now)
	writeFile(lw, "etc/group", []byte("root:x:0:\n"), 0o644, now)
	if caCerts != nil {
		writeDir(lw, "etc/ssl/", now)
		writeDir(lw, "etc/ssl/certs/", now)
		writeFile(lw, "etc/ssl/certs/ca-certificates.crt", caCerts, 0o644, now)
	}
	must(lw.Close())
	layerBytes := layer.Bytes()
	diffID := "sha256:" + sha256hex(layerBytes)

	// ---- image config ----
	config := map[string]any{
		"architecture": "amd64",
		"os":           "linux",
		"created":      now.Format(time.RFC3339),
		"config": map[string]any{
			"Env": []string{
				"PATH=/usr/local/bin:/usr/local/sbin:/usr/bin:/bin",
				"POSTRA_DATA_DIR=/data",
			},
			"Entrypoint":   []string{"postra"},
			"Cmd":          []string{"serve"},
			"WorkingDir":   "/app",
			"ExposedPorts": map[string]any{"8480/tcp": map[string]any{}, "8481/tcp": map[string]any{}},
			"Volumes":      map[string]any{"/data": map[string]any{}},
		},
		"rootfs": map[string]any{
			"type":     "layers",
			"diff_ids": []string{diffID},
		},
		"history": []map[string]any{
			{"created": now.Format(time.RFC3339), "created_by": "postra mkimage (daemonless offline build)"},
		},
	}
	configBytes, err := json.MarshalIndent(config, "", "  ")
	must(err)
	configName := sha256hex(configBytes) + ".json"

	// ---- manifest ----
	layerName := "layer.tar"
	manifest := []map[string]any{{
		"Config":   configName,
		"RepoTags": []string{tag},
		"Layers":   []string{layerName},
	}}
	manifestBytes, err := json.MarshalIndent(manifest, "", "  ")
	must(err)

	// ---- final image tar ----
	out, err := os.Create(outPath)
	must(err)
	defer out.Close()
	iw := tar.NewWriter(out)
	writeFile(iw, configName, configBytes, 0o644, now)
	writeFile(iw, layerName, layerBytes, 0o644, now)
	writeFile(iw, "manifest.json", manifestBytes, 0o644, now)
	must(iw.Close())

	fmt.Printf("image tag:   %s\n", tag)
	fmt.Printf("layer bytes: %d\n", len(layerBytes))
	fmt.Printf("diff_id:     %s\n", diffID)
	fmt.Printf("wrote:       %s\n", outPath)
}

func writeDir(w *tar.Writer, name string, t time.Time)           { writeDirMode(w, name, 0o755, t) }
func writeDirMode(w *tar.Writer, name string, mode int64, t time.Time) {
	must(w.WriteHeader(&tar.Header{
		Name: name, Typeflag: tar.TypeDir, Mode: mode, ModTime: t,
	}))
}

func writeFile(w *tar.Writer, name string, data []byte, mode int64, t time.Time) {
	must(w.WriteHeader(&tar.Header{
		Name: name, Typeflag: tar.TypeReg, Mode: mode, Size: int64(len(data)), ModTime: t,
	}))
	_, err := w.Write(data)
	must(err)
}

func sha256hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}

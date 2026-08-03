package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestAssetAndBinaryName(t *testing.T) {
	if got := assetName("v1.2.3", "linux", "amd64"); got != "lark-acp-bridge_v1.2.3_linux_amd64.tar.gz" {
		t.Fatalf("assetName = %q", got)
	}
	if got := binaryName("windows"); got != "lark-acp-bridge.exe" {
		t.Fatalf("binaryName(windows) = %q", got)
	}
	if got := binaryName("linux"); got != "lark-acp-bridge" {
		t.Fatalf("binaryName(linux) = %q", got)
	}
}

func TestCompareVersion(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"v1.2.3", "v1.2.3", 0},
		{"v1.2.4", "v1.2.3", 1},
		{"v1.3.0", "v1.2.9", 1},
		{"v2.0.0", "v1.9.9", 1},
		{"v1.2.3", "v1.2.4", -1},
		{"1.2.3", "v1.2.3", 0},
		{"v1.2.3", "v1.2", 1},
		{"v2026-08-02", "v1.0.0", 1},
	}
	for _, c := range cases {
		if got := compareVersion(c.a, c.b); got != c.want {
			t.Errorf("compareVersion(%q,%q)=%d want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestIsNewer(t *testing.T) {
	if !IsNewer("dev", "v1.0.0") {
		t.Error("dev should be upgradable to a concrete version")
	}
	if IsNewer("", "") {
		t.Error("empty vs empty should be false")
	}
	if !IsNewer("v1.0.0", "v1.0.1") {
		t.Error("1.0.0 < 1.0.1")
	}
	if IsNewer("v1.0.1", "v1.0.1") {
		t.Error("same version not newer")
	}
}

func makeTarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tr := tar.NewWriter(gz)
	for name, content := range files {
		hdr := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(content))}
		if err := tr.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(tr, content); err != nil {
			t.Fatal(err)
		}
	}
	if err := tr.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestExtractBinary(t *testing.T) {
	data := makeTarGz(t, map[string]string{
		"lark-acp-bridge": "BINARY",
		"README.md":       "readme",
		"LICENSE":         "license",
	})
	bin, err := extractBinary(data, "lark-acp-bridge")
	if err != nil {
		t.Fatal(err)
	}
	if string(bin) != "BINARY" {
		t.Fatalf("got %q", bin)
	}
	if _, err := extractBinary(data, "missing"); err == nil {
		t.Fatal("expected error for missing binary")
	}
}

func TestReleaseForTag(t *testing.T) {
	o := &Options{Repo: "owner/repo", GOOS: "linux", GOARCH: "amd64"}
	rel := o.releaseForTag("v1.5.0", "notes")
	if rel.Tag != "v1.5.0" || rel.Body != "notes" {
		t.Fatalf("bad release: %+v", rel)
	}
	want := "v1.5.0/lark-acp-bridge_v1.5.0_linux_amd64.tar.gz"
	if !strings.HasSuffix(rel.AssetURL, want) {
		t.Fatalf("AssetURL = %q, want suffix %q", rel.AssetURL, want)
	}
	if !strings.HasSuffix(rel.Sha256URL, want+".sha256") {
		t.Fatalf("Sha256URL = %q", rel.Sha256URL)
	}
}

func TestParseTagFromLocation(t *testing.T) {
	cases := map[string]string{
		"https://github.com/owner/repo/releases/tag/v9.9.9": "v9.9.9",
		"https://example/x/y/tag/v1.0.0":                    "v1.0.0",
	}
	for loc, want := range cases {
		got, err := parseTagFromLocation(loc)
		if err != nil {
			t.Fatalf("parseTagFromLocation(%q): %v", loc, err)
		}
		if got != want {
			t.Fatalf("parseTagFromLocation(%q)=%q want %q", loc, got, want)
		}
	}
	if _, err := parseTagFromLocation("https://example/tag/"); err == nil {
		t.Fatal("expected error for trailing slash")
	}
}

func TestApplyEndToEnd(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("原子替换路径在 unix 上验证")
	}
	payload := []byte("#!/bin/sh\necho new\n")
	archive := makeTarGz(t, map[string]string{"lark-acp-bridge": string(payload)})
	sum := sha256.Sum256(archive)
	hashHex := hex.EncodeToString(sum[:])

	assetPath := "/download/v1.2.3/lark-acp-bridge_v1.2.3_linux_amd64.tar.gz"
	mux := http.NewServeMux()
	mux.HandleFunc(assetPath, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(archive)
	})
	mux.HandleFunc(assetPath+".sha256", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s  lark-acp-bridge_v1.2.3_linux_amd64.tar.gz\n", hashHex)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	rel := &Release{
		Tag:       "v1.2.3",
		AssetURL:  srv.URL + assetPath,
		Sha256URL: srv.URL + assetPath + ".sha256",
	}

	dir := t.TempDir()
	target := filepath.Join(dir, "lark-acp-bridge")
	if err := os.WriteFile(target, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}

	o := &Options{
		CurrentVersion: "v1.0.0",
		GOOS:           "linux",
		GOARCH:         "amd64",
		ExePath:        target,
		HTTPClient:     srv.Client(),
	}
	res, err := o.Apply(context.Background(), rel)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if res.To != "v1.2.3" || res.ExePath != target {
		t.Fatalf("bad result: %+v", res)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("replaced content = %q", got)
	}
}

func TestApplyHashMismatch(t *testing.T) {
	archive := makeTarGz(t, map[string]string{"lark-acp-bridge": "x"})
	mux := http.NewServeMux()
	mux.HandleFunc("/asset", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(archive) })
	mux.HandleFunc("/asset.sha256", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, strings.Repeat("0", 64)+"  x\n")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	dir := t.TempDir()
	target := filepath.Join(dir, "bin")
	if err := os.WriteFile(target, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}

	o := &Options{GOOS: "linux", GOARCH: "amd64", ExePath: target, HTTPClient: srv.Client()}
	_, err := o.Apply(context.Background(), &Release{Tag: "v1", AssetURL: srv.URL + "/asset", Sha256URL: srv.URL + "/asset.sha256"})
	if err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("expected sha256 error, got %v", err)
	}
	got, _ := os.ReadFile(target)
	if string(got) != "old" {
		t.Fatalf("target mutated on hash failure: %q", got)
	}
}

func TestApplyFallsBackToMirror(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("原子替换路径在 unix 上验证")
	}
	payload := []byte("#!/bin/sh\necho from-mirror\n")
	archive := makeTarGz(t, map[string]string{"lark-acp-bridge": string(payload)})
	sum := sha256.Sum256(archive)
	hashHex := hex.EncodeToString(sum[:])

	// GitHub 主源：sha256 可达但包返回 404，模拟下载失败。
	ghMux := http.NewServeMux()
	ghMux.HandleFunc("/asset", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	ghMux.HandleFunc("/asset.sha256", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s  x\n", hashHex)
	})
	gh := httptest.NewServer(ghMux)
	defer gh.Close()

	// Gitee 镜像：正常返回。
	mirrorMux := http.NewServeMux()
	mirrorMux.HandleFunc("/asset", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(archive) })
	mirrorMux.HandleFunc("/asset.sha256", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s  x\n", hashHex)
	})
	mirror := httptest.NewServer(mirrorMux)
	defer mirror.Close()

	dir := t.TempDir()
	target := filepath.Join(dir, "lark-acp-bridge")
	if err := os.WriteFile(target, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}

	rel := &Release{
		Tag:       "v1.2.3",
		AssetURL:  gh.URL + "/asset",
		Sha256URL: gh.URL + "/asset.sha256",
		Mirrors: []Mirror{
			{Name: "gitee", AssetURL: mirror.URL + "/asset", Sha256URL: mirror.URL + "/asset.sha256"},
		},
	}
	o := &Options{GOOS: "linux", GOARCH: "amd64", ExePath: target, HTTPClient: gh.Client()}
	res, err := o.Apply(context.Background(), rel)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if res.Source != "gitee" {
		t.Fatalf("Source = %q, want gitee", res.Source)
	}
	got, _ := os.ReadFile(target)
	if !bytes.Equal(got, payload) {
		t.Fatalf("content = %q", got)
	}
}

func TestReleaseForTagBuildsMirrors(t *testing.T) {
	o := &Options{Repo: "owner/repo", GOOS: "linux", GOARCH: "amd64"}
	rel := o.releaseForTag("v1.2.3", "")
	if !strings.Contains(rel.AssetURL, "github.com/owner/repo/releases/download/v1.2.3/") {
		t.Fatalf("AssetURL = %q", rel.AssetURL)
	}
	if len(rel.Mirrors) != 1 || rel.Mirrors[0].Name != "gitee" {
		t.Fatalf("mirrors = %+v", rel.Mirrors)
	}
	if !strings.Contains(rel.Mirrors[0].AssetURL, "gitee.com/") {
		t.Fatalf("mirror url = %q", rel.Mirrors[0].AssetURL)
	}

	disabled := &Options{Repo: "owner/repo", GOOS: "linux", GOARCH: "amd64", GiteeRepo: "-"}
	if len(disabled.releaseForTag("v1", "").Mirrors) != 0 {
		t.Fatal("GiteeRepo=\"-\" should disable mirror")
	}
}

func parseTagFromLocation(loc string) (string, error) {
	idx := strings.LastIndex(loc, "/")
	if idx < 0 || idx == len(loc)-1 {
		return "", fmt.Errorf("bad location %q", loc)
	}
	return loc[idx+1:], nil
}

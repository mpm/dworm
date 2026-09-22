package version

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/creativeprojects/go-selfupdate"
)

func fixtureUpdater(t *testing.T, goos, arch, mode string) *selfupdate.Updater {
	t.Helper()
	name := "dworm_v2.0.0_" + goos + "_" + arch + ".tar.gz"
	var archive bytes.Buffer
	gz := gzip.NewWriter(&archive)
	tw := tar.NewWriter(gz)
	body := []byte("#!/bin/sh\necho v2.0.0\n")
	if err := tw.WriteHeader(&tar.Header{Name: strings.TrimSuffix(name, ".tar.gz") + "/dworm", Mode: 0755, Size: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
	tw.Write(body)
	tw.Close()
	gz.Close()
	checksum := fmt.Sprintf("%x  %s\n", sha256.Sum256(archive.Bytes()), name)
	if mode == "mismatch" {
		checksum = strings.Repeat("0", 64) + "  " + name + "\n"
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/releases"):
			if mode == "network" {
				http.Error(w, "unavailable", 500)
				return
			}
			assets := []map[string]any{{"id": 1, "name": name}}
			if mode != "missing" {
				assets = append(assets, map[string]any{"id": 2, "name": "checksums.txt"})
			}
			if mode == "platform" {
				assets = assets[1:]
			}
			json.NewEncoder(w).Encode([]map[string]any{
				{"tag_name": "v3.0.0-beta.1", "prerelease": true, "assets": assets},
				{"tag_name": "v2.0.0", "html_url": "https://example.test/release", "assets": assets},
			})
		case strings.HasSuffix(r.URL.Path, "/assets/1"):
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Write(archive.Bytes())
		case strings.HasSuffix(r.URL.Path, "/assets/2"):
			w.Header().Set("Content-Type", "application/octet-stream")
			io.WriteString(w, checksum)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	source, err := selfupdate.NewGitHubSource(selfupdate.GitHubConfig{EnterpriseBaseURL: server.URL + "/", APIToken: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	up, err := newUpdater(source, goos, arch)
	if err != nil {
		t.Fatal(err)
	}
	return up
}

func TestUpdateExecutable(t *testing.T) {
	for _, mode := range []string{"success", "current", "newer", "missing", "mismatch", "network", "platform", "permission"} {
		t.Run(mode, func(t *testing.T) {
			up := fixtureUpdater(t, "linux", "amd64", mode)
			dir := t.TempDir()
			target := filepath.Join(dir, "dworm")
			old := []byte("old executable")
			if err := os.WriteFile(target, old, 0755); err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(dir, "link")
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}
			current := "v1.0.0"
			if mode == "current" {
				current = "v2.0.0"
			}
			if mode == "newer" {
				current = "v3.0.0"
			}
			if mode == "permission" {
				if os.Geteuid() == 0 {
					t.Skip("root bypasses directory permissions")
				}
				os.Chmod(dir, 0555)
				defer os.Chmod(dir, 0755)
			}
			err := updateExecutable(context.Background(), up, current, link, io.Discard)
			wantError := mode != "success" && mode != "current" && mode != "newer"
			if (err != nil) != wantError {
				t.Fatalf("error = %v", err)
			}
			if mode == "permission" && (!strings.Contains(err.Error(), target) || !strings.Contains(err.Error(), "https://example.test/release")) {
				t.Fatal(err)
			}
			info, _ := os.Lstat(link)
			if info.Mode()&os.ModeSymlink == 0 {
				t.Fatal("symlink replaced")
			}
			if mode == "success" {
				out, err := exec.Command(link).Output()
				if err != nil || string(out) != "v2.0.0\n" {
					t.Fatalf("installed fixture: %s, %v", out, err)
				}
			} else {
				data, _ := os.ReadFile(target)
				if !bytes.Equal(data, old) {
					t.Fatal("original executable changed")
				}
			}
		})
	}
}

func TestPlatformDiscovery(t *testing.T) {
	for _, goos := range []string{"linux", "darwin"} {
		for _, arch := range []string{"amd64", "arm64"} {
			up := fixtureUpdater(t, goos, arch, "")
			rel, err := discover(context.Background(), up)
			if err != nil {
				t.Fatal(err)
			}
			if rel.Version() != "2.0.0" || rel.AssetName != "dworm_v2.0.0_"+goos+"_"+arch+".tar.gz" {
				t.Fatalf("wrong release: %+v", rel)
			}
		}
	}
	if _, err := newUpdater(nil, "windows", "amd64"); err == nil {
		t.Fatal("accepted unsupported host")
	}
}

func TestVersionPolicy(t *testing.T) {
	for _, v := range []string{"dev", "", "abc123", "v1.0.0-dirty", "v1.0.0-2-gabc123", "v1.0.0-beta.1"} {
		if releaseVersion(v) {
			t.Errorf("accepted %q", v)
		}
	}
	if !releaseVersion("v1.2.3") {
		t.Fatal("rejected release")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	oldVersion := Version
	Version = "v1.0.0"
	defer func() { Version = oldVersion }()
	if result := CheckForUpdateWithContext(ctx); result != nil {
		t.Fatal("incidental check should fail quietly")
	}
}

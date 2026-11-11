package cli

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSelectAssets(t *testing.T) {
	all := []ghAsset{
		{Name: "awsport_linux_amd64.tar.gz"},
		{Name: "awsport_linux_arm64.tar.gz"},
		{Name: "awsport_darwin_arm64.tar.gz"},
		{Name: "awsport_windows_amd64.zip"},
		{Name: "awsport_windows_amd64.tar.gz"},
		{Name: "checksums.txt"},
	}
	tests := []struct {
		name         string
		assets       []ghAsset
		goos, goarch string
		want         string
		wantErr      string
	}{
		{"linux amd64", all, "linux", "amd64", "awsport_linux_amd64.tar.gz", ""},
		{"linux arm64", all, "linux", "arm64", "awsport_linux_arm64.tar.gz", ""},
		{"darwin arm64", all, "darwin", "arm64", "awsport_darwin_arm64.tar.gz", ""},
		{"windows prefers zip", all, "windows", "amd64", "awsport_windows_amd64.zip", ""},
		{"missing platform", all, "darwin", "amd64", "", "no asset"},
		{"missing checksums", all[:1], "linux", "amd64", "", "checksums.txt"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			archive, sums, err := selectAssets(tt.assets, tt.goos, tt.goarch)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if archive.Name != tt.want || sums.Name != "checksums.txt" {
				t.Fatalf("got %q, %q", archive.Name, sums.Name)
			}
		})
	}
}

func sha(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func TestVerifyChecksum(t *testing.T) {
	data := []byte("archive bytes")
	name := "awsport_linux_amd64.tar.gz"
	other := sha([]byte("other"))
	tests := []struct {
		name      string
		checksums string
		wantErr   string
	}{
		{"match", other + "  awsport_darwin_arm64.tar.gz\n" + sha(data) + "  " + name + "\n", ""},
		{"uppercase hex and binary marker", strings.ToUpper(sha(data)) + " *" + name + "\n", ""},
		{"mismatch", other + "  " + name + "\n", "checksum mismatch"},
		{"missing entry", other + "  awsport_darwin_arm64.tar.gz\n", "no entry"},
		{"prefix name is not a match", sha(data) + "  x" + name + "\n", "no entry"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := verifyChecksum(data, name, []byte(tt.checksums))
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestExtractBinary(t *testing.T) {
	bin := []byte("#!binary")

	var tgz bytes.Buffer
	gz := gzip.NewWriter(&tgz)
	tw := tar.NewWriter(gz)
	for _, f := range []struct{ name, body string }{{"README.md", "readme"}, {"awsport", string(bin)}} {
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0o755, Size: int64(len(f.body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(f.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}

	var zbuf bytes.Buffer
	zw := zip.NewWriter(&zbuf)
	w, err := zw.Create("dir/awsport.exe")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(bin); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name, archive, bin string
		data               []byte
		wantErr            bool
	}{
		{"tar.gz", "awsport_linux_amd64.tar.gz", "awsport", tgz.Bytes(), false},
		{"zip nested", "awsport_windows_amd64.zip", "awsport.exe", zbuf.Bytes(), false},
		{"missing binary", "awsport_windows_amd64.zip", "awsport", zbuf.Bytes(), true},
		{"unknown format", "awsport.rar", "awsport", nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := extractBinary(tt.data, tt.archive, tt.bin)
			if tt.wantErr {
				if err == nil {
					t.Fatal("want error")
				}
				return
			}
			if err != nil || !bytes.Equal(got, bin) {
				t.Fatalf("got %q, %v", got, err)
			}
		})
	}
}

func TestReplaceExecutable(t *testing.T) {
	for _, goos := range []string{"linux", "windows"} {
		t.Run(goos, func(t *testing.T) {
			dir := t.TempDir()
			exe := filepath.Join(dir, "awsport")
			if err := os.WriteFile(exe, []byte("old"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := replaceExecutable(exe, []byte("new"), goos); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(exe)
			if err != nil || string(got) != "new" {
				t.Fatalf("exe = %q, %v", got, err)
			}
			_, err = os.Stat(exe + ".old")
			if hasOld := err == nil; hasOld != (goos == "windows") {
				t.Fatalf("old binary present = %v for %s", hasOld, goos)
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range entries {
				if strings.HasPrefix(e.Name(), ".awsport-update-") {
					t.Fatalf("temp file left behind: %s", e.Name())
				}
			}
		})
	}
}

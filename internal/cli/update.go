package cli

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

const (
	latestReleaseURL = "https://api.github.com/repos/nhl4000/awsport/releases/latest"
	checksumsName    = "checksums.txt"
	maxDownload      = 256 << 20
)

type ghRelease struct {
	TagName string    `json:"tag_name"`
	Assets  []ghAsset `json:"assets"`
}

type ghAsset struct {
	Name               string `json:"name"`
	URL                string `json:"url"` // API URL; serves bytes with Accept: application/octet-stream
	BrowserDownloadURL string `json:"browser_download_url"`
}

func newUpdateCmd(version string) *cobra.Command {
	return &cobra.Command{
		Use:   "update",
		Short: "Update awsport to the latest GitHub release",
		Long: "Download the latest release for this OS/architecture, verify its SHA-256\n" +
			"checksum and replace the running binary. $GITHUB_TOKEN is used if set.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runUpdate(cmd.Context(), version, cmd.OutOrStdout())
		},
	}
}

func runUpdate(ctx context.Context, version string, out io.Writer) error {
	client := &http.Client{Timeout: 5 * time.Minute}
	token := os.Getenv("GITHUB_TOKEN")

	body, err := httpGet(ctx, client, latestReleaseURL, "application/vnd.github+json", token)
	if err != nil {
		return fmt.Errorf("fetch latest release: %w", err)
	}
	var rel ghRelease
	if err := json.Unmarshal(body, &rel); err != nil {
		return fmt.Errorf("parse release: %w", err)
	}
	if rel.TagName == "" {
		return errors.New("latest release has no tag")
	}
	if strings.TrimPrefix(rel.TagName, "v") == strings.TrimPrefix(version, "v") {
		fmt.Fprintf(out, "awsport %s is already the latest version\n", version)
		return nil
	}
	archive, sums, err := selectAssets(rel.Assets, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return fmt.Errorf("release %s: %w", rel.TagName, err)
	}
	fmt.Fprintf(out, "Downloading %s (%s)…\n", archive.Name, rel.TagName)
	data, err := downloadAsset(ctx, client, archive, token)
	if err != nil {
		return err
	}
	sumData, err := downloadAsset(ctx, client, sums, token)
	if err != nil {
		return err
	}
	if err := verifyChecksum(data, archive.Name, sumData); err != nil {
		return err
	}
	binName := "awsport"
	if runtime.GOOS == "windows" {
		binName += ".exe"
	}
	bin, err := extractBinary(data, archive.Name, binName)
	if err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate running binary: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	if err := replaceExecutable(exe, bin, runtime.GOOS); err != nil {
		return err
	}
	fmt.Fprintf(out, "Updated %s to %s\n", exe, rel.TagName)
	return nil
}

func httpGet(ctx context.Context, client *http.Client, url, accept, token string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("User-Agent", "awsport-updater")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		hint := ""
		if resp.StatusCode == http.StatusNotFound && token == "" {
			hint = " (private repository? set GITHUB_TOKEN)"
		}
		return nil, fmt.Errorf("GET %s: %s%s: %s", url, resp.Status, hint, strings.TrimSpace(string(msg)))
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxDownload+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", url, err)
	}
	if len(data) > maxDownload {
		return nil, fmt.Errorf("%s is larger than %d bytes", url, maxDownload)
	}
	return data, nil
}

func downloadAsset(ctx context.Context, client *http.Client, a ghAsset, token string) ([]byte, error) {
	url, accept := a.URL, "application/octet-stream"
	if url == "" {
		url, accept = a.BrowserDownloadURL, "*/*"
	}
	data, err := httpGet(ctx, client, url, accept, token)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", a.Name, err)
	}
	return data, nil
}

// selectAssets picks the archive awsport_<os>_<arch>.(tar.gz|zip) and
// checksums.txt. Windows prefers zip, other systems tar.gz.
func selectAssets(assets []ghAsset, goos, goarch string) (archive, checksums ghAsset, err error) {
	base := "awsport_" + goos + "_" + goarch
	exts := []string{".tar.gz", ".zip"}
	if goos == "windows" {
		exts = []string{".zip", ".tar.gz"}
	}
	byName := make(map[string]ghAsset, len(assets))
	for _, a := range assets {
		byName[a.Name] = a
	}
	found := false
	for _, ext := range exts {
		if a, ok := byName[base+ext]; ok {
			archive, found = a, true
			break
		}
	}
	if !found {
		return ghAsset{}, ghAsset{}, fmt.Errorf("no asset %s.tar.gz or %s.zip for %s/%s", base, base, goos, goarch)
	}
	checksums, ok := byName[checksumsName]
	if !ok {
		return ghAsset{}, ghAsset{}, fmt.Errorf("release has no %s", checksumsName)
	}
	return archive, checksums, nil
}

// verifyChecksum checks data against the sha256sum-format line for name.
func verifyChecksum(data []byte, name string, checksums []byte) error {
	want := ""
	sc := bufio.NewScanner(bytes.NewReader(checksums))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == name {
			want = strings.ToLower(fields[0])
			break
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("read %s: %w", checksumsName, err)
	}
	if want == "" {
		return fmt.Errorf("%s has no entry for %s", checksumsName, name)
	}
	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); got != want {
		return fmt.Errorf("checksum mismatch for %s: got %s, want %s", name, got, want)
	}
	return nil
}

// extractBinary returns the file named binName from a .tar.gz or .zip archive.
func extractBinary(data []byte, archiveName, binName string) ([]byte, error) {
	switch {
	case strings.HasSuffix(archiveName, ".zip"):
		zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			return nil, fmt.Errorf("open %s: %w", archiveName, err)
		}
		for _, f := range zr.File {
			if f.FileInfo().IsDir() || path.Base(f.Name) != binName {
				continue
			}
			rc, err := f.Open()
			if err != nil {
				return nil, fmt.Errorf("open %s in %s: %w", f.Name, archiveName, err)
			}
			defer rc.Close()
			return readLimited(rc, archiveName)
		}
	case strings.HasSuffix(archiveName, ".tar.gz"):
		gz, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, fmt.Errorf("open %s: %w", archiveName, err)
		}
		defer gz.Close()
		tr := tar.NewReader(gz)
		for {
			h, err := tr.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return nil, fmt.Errorf("read %s: %w", archiveName, err)
			}
			if h.Typeflag == tar.TypeReg && path.Base(h.Name) == binName {
				return readLimited(tr, archiveName)
			}
		}
	default:
		return nil, fmt.Errorf("unsupported archive %s", archiveName)
	}
	return nil, fmt.Errorf("%s does not contain %s", archiveName, binName)
}

func readLimited(r io.Reader, archiveName string) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, maxDownload+1))
	if err != nil {
		return nil, fmt.Errorf("extract from %s: %w", archiveName, err)
	}
	if len(b) > maxDownload {
		return nil, fmt.Errorf("binary in %s is larger than %d bytes", archiveName, maxDownload)
	}
	return b, nil
}

// replaceExecutable atomically swaps exe for bin. Windows cannot overwrite a
// running binary, so the old one is first renamed to <exe>.old.
func replaceExecutable(exe string, bin []byte, goos string) error {
	dir := filepath.Dir(exe)
	tmp, err := os.CreateTemp(dir, ".awsport-update-*")
	if err != nil {
		return fmt.Errorf("cannot write to %s (reinstall with install.sh or run with sufficient permissions): %w", dir, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(bin); err != nil {
		tmp.Close()
		return fmt.Errorf("write %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write %s: %w", tmpName, err)
	}
	if err := os.Chmod(tmpName, 0o755); err != nil {
		return fmt.Errorf("chmod %s: %w", tmpName, err)
	}
	if goos == "windows" {
		old := exe + ".old"
		if err := os.Remove(old); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove %s: %w", old, err)
		}
		if err := os.Rename(exe, old); err != nil {
			return fmt.Errorf("move %s aside: %w", exe, err)
		}
		if err := os.Rename(tmpName, exe); err != nil {
			if rerr := os.Rename(old, exe); rerr != nil {
				return fmt.Errorf("install %s: %w (restore failed: %v; old binary is at %s)", exe, err, rerr, old)
			}
			return fmt.Errorf("install %s: %w", exe, err)
		}
		return nil
	}
	if err := os.Rename(tmpName, exe); err != nil {
		return fmt.Errorf("replace %s: %w", exe, err)
	}
	return nil
}

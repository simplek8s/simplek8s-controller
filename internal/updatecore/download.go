package updatecore

// Artifact download + integrity (PLAN-M2 3.7 step 3). The kernel
// artifact is streamed to a temp file in the destination directory while
// its sha256 is computed in flight, then verified against the GPG-
// verified index value; on mismatch the partial file is removed.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
)

// maxArtifactBytes bounds a single download (the compressed kernel is
// tens of MB; the guard rejects a hostile repo serving unbounded data).
const maxArtifactBytes = 1 << 30 // 1 GiB

// downloadAndVerify streams the artifact at url into dst (via a temp
// file in dst's directory), hashing in flight, and verifies the sha256
// against want (lowercase hex). A mismatch removes the partial file.
func downloadAndVerify(ctx context.Context, c *http.Client, url, want, dst string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d for %s", resp.StatusCode, url)
	}

	dir := filepath.Dir(dst)
	tmp, err := os.CreateTemp(dir, ".dl-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(resp.Body, maxArtifactBytes)); err != nil {
		tmp.Close()
		return fmt.Errorf("download: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got != want {
		return fmt.Errorf("checksum mismatch for %s: got %s want %s", filepath.Base(url), got, want)
	}
	return os.Rename(tmpName, dst)
}

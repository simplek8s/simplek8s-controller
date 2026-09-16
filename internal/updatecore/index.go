package updatecore

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
)

// sha256Line matches one sha256sum line: 64 hex chars, a space, then a
// mode marker (space for text, '*' for binary), then the filename.
var sha256Line = regexp.MustCompile(`^([0-9a-fA-F]{64}) [ *](.+)$`)

// ParseIndex parses the SHA256SUMS index content into filename -> sha256.
// Non-matching lines (comments, blanks, foreign files) are skipped; the
// caller filters by release grammar.
func ParseIndex(b []byte) (map[string]string, error) {
	out := make(map[string]string)
	s := bufio.NewScanner(bytes.NewReader(b))
	s.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for s.Scan() {
		line := strings.TrimSuffix(s.Text(), "\r")
		m := sha256Line.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		out[m[2]] = strings.ToLower(m[1])
	}
	if err := s.Err(); err != nil {
		return nil, fmt.Errorf("parse index: %w", err)
	}
	return out, nil
}

// Index files inside the release repo (PLAN-M2 3.6).
const (
	IndexFile      = "SHA256SUMS"
	IndexSignature = "SHA256SUMS.gpg"
)

// httpGet downloads one file with a bounded size (index + signature are
// small; the bound guards against a hostile repo).
func httpGet(ctx context.Context, c *http.Client, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d for %s", resp.StatusCode, url)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxIndexBytes))
	if err != nil {
		return nil, err
	}
	return b, nil
}

const maxIndexBytes = 8 * 1024 * 1024

// FetchBytes downloads one bounded file (exported for controller
// callers that verify separately, e.g. Feature.Check).
func FetchBytes(ctx context.Context, c *http.Client, url string) ([]byte, error) {
	return httpGet(ctx, c, url)
}

package update

import (
	"bufio"
	"bytes"
	"fmt"
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

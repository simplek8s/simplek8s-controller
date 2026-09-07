package update

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// zstdCompress returns the zstd-encoded form of in.
func zstdCompress(t *testing.T, in []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc, err := zstd.NewWriter(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := enc.Write(in); err != nil {
		t.Fatal(err)
	}
	if err := enc.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestExtractZstdRoundtrip(t *testing.T) {
	payload := bytes.Repeat([]byte("simplek8s-kernel-bytes-"), 1024)
	src := filepath.Join(t.TempDir(), "in.zst")
	if err := os.WriteFile(src, zstdCompress(t, payload), 0644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "out")
	if err := extractZstd(src, dst); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("extracted %d bytes, want %d", len(got), len(payload))
	}
}

func TestExtractZstdGarbageFails(t *testing.T) {
	src := filepath.Join(t.TempDir(), "bad.zst")
	if err := os.WriteFile(src, []byte("not zstd data"), 0644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "out")
	if err := extractZstd(src, dst); err == nil {
		t.Fatal("extractZstd = no error on garbage input")
	}
}

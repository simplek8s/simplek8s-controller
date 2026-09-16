package updatecore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDownloadAndVerifyOK(t *testing.T) {
	body := []byte("artifact-bytes")
	sum := sha256.Sum256(body)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(body)
	}))
	defer srv.Close()

	dst := filepath.Join(t.TempDir(), "art.zst")
	if err := downloadAndVerify(context.Background(), srv.Client(), srv.URL+"/art.zst", hex.EncodeToString(sum[:]), dst); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(body) {
		t.Fatalf("downloaded %q, want %q", got, body)
	}
}

func TestDownloadAndVerifyMismatchRemovesPartial(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("artifact-bytes"))
	}))
	defer srv.Close()

	dst := filepath.Join(t.TempDir(), "art.zst")
	if err := downloadAndVerify(context.Background(), srv.Client(), srv.URL+"/art.zst", strings.Repeat("0", 64), dst); err == nil {
		t.Fatal("downloadAndVerify = no error on checksum mismatch")
	}
	if fileExists(dst) {
		t.Fatal("partial file left behind after mismatch")
	}
}

func TestDownloadAndVerifyHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	dst := filepath.Join(t.TempDir(), "art.zst")
	if err := downloadAndVerify(context.Background(), srv.Client(), srv.URL+"/missing", "x", dst); err == nil {
		t.Fatal("downloadAndVerify = no error on HTTP 404")
	}
}

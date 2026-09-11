package update

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/simplek8s/simplek8s-controller/internal/kube"
)

func testFeature(t *testing.T, key *testKey) *Feature {
	t.Helper()
	return &Feature{
		cfg: Config{
			EmbeddedKeyring: key.keyring,
			CustomKeyring:   filepath.Join(t.TempDir(), "absent-custom.gpg"),
		},
		http: http.DefaultClient,
	}
}

func testNode(arch, kernelVersion string) *kube.Node {
	n := &kube.Node{}
	n.Metadata.Name = "w1"
	n.Status.NodeInfo = kube.NodeInfo{Architecture: arch, KernelVersion: kernelVersion}
	return n
}

func TestCheckAvailable(t *testing.T) {
	k := newTestKey(t, true)
	srv := releaseRepo(t, k, true, kernelIndex())
	f := testFeature(t, k)

	res, err := f.Check(context.Background(),
		testNode("amd64", "6.18.48-simplek8s-202601010000 (amd64)"), srv.URL, "x86-64")
	if err != nil {
		t.Fatal(err)
	}
	if res.Latest != "202608291203" || res.Running != "202601010000" || !res.Available {
		t.Fatalf("res = %+v", res)
	}
	if res.URL != srv.URL {
		t.Fatalf("URL = %q", res.URL)
	}
}

func TestCheckUpToDate(t *testing.T) {
	k := newTestKey(t, true)
	srv := releaseRepo(t, k, true, kernelIndex())
	f := testFeature(t, k)

	res, err := f.Check(context.Background(),
		testNode("amd64", "6.18.48-simplek8s-202608291203 (amd64)"), srv.URL, "x86-64")
	if err != nil {
		t.Fatal(err)
	}
	if res.Latest != "202608291203" || res.Available {
		t.Fatalf("res = %+v", res)
	}
}

func TestCheckRunningNewerThanRepo(t *testing.T) {
	k := newTestKey(t, true)
	srv := releaseRepo(t, k, true, kernelIndex())
	f := testFeature(t, k)

	res, err := f.Check(context.Background(),
		testNode("amd64", "6.18.48-simplek8s-999999999999 (amd64)"), srv.URL, "x86-64")
	if err != nil {
		t.Fatal(err)
	}
	if res.Available {
		t.Fatalf("dev kernel must not be downgraded: %+v", res)
	}
}

func TestCheckNonSimplek8sKernel(t *testing.T) {
	k := newTestKey(t, true)
	srv := releaseRepo(t, k, true, kernelIndex())
	f := testFeature(t, k)

	res, err := f.Check(context.Background(), testNode("amd64", "6.18.48 (amd64)"), srv.URL, "x86-64")
	if err != nil {
		t.Fatal(err)
	}
	if res.Running != "" || !res.Available || res.Latest != "202608291203" {
		t.Fatalf("res = %+v", res)
	}
}

func TestCheckFlavorFiltering(t *testing.T) {
	k := newTestKey(t, true)
	files := map[string][]byte{
		"simplek8s.202608291203.x86-64.efi.zst": []byte("x86"),
		"simplek8s.202608241828.rpi4.efi.zst":   []byte("rpi4"),
	}
	srv := releaseRepo(t, k, true, files)
	f := testFeature(t, k)

	res, err := f.Check(context.Background(),
		testNode("arm64", "6.18.48-simplek8s-202601010000 (aarch64)"), srv.URL, "rpi4")
	if err != nil {
		t.Fatal(err)
	}
	if res.Latest != "202608241828" || !res.Available || res.Arch != "rpi4" {
		t.Fatalf("rpi4 res = %+v", res)
	}
	if res.Artifact != "simplek8s.202608241828.rpi4.efi.zst" {
		t.Fatalf("artifact = %q", res.Artifact)
	}
}

func TestCheckNoKernelForFlavor(t *testing.T) {
	k := newTestKey(t, true)
	files := map[string][]byte{"simplek8s.202601010000.x86-64.efi.zst": []byte("k")}
	srv := releaseRepo(t, k, true, files)
	f := testFeature(t, k)

	res, err := f.Check(context.Background(),
		testNode("arm64", "6.18.48-simplek8s-202501010000 (aarch64)"), srv.URL, "rpi4")
	if err != nil {
		t.Fatal(err)
	}
	if res.Latest != "" || res.Available {
		t.Fatalf("no rpi4 kernel: %+v", res)
	}
}

func TestCheckTrailingSlash(t *testing.T) {
	k := newTestKey(t, true)
	srv := releaseRepo(t, k, false, kernelIndex()) // binary signature too
	f := testFeature(t, k)

	res, err := f.Check(context.Background(),
		testNode("amd64", "6.18.48-simplek8s-202601010000 (amd64)"), srv.URL+"/", "x86-64")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Available || res.Latest != "202608291203" {
		t.Fatalf("trailing slash: %+v", res)
	}
}

func TestCheckMissingIndex(t *testing.T) {
	k := newTestKey(t, true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	f := testFeature(t, k)

	if _, err := f.Check(context.Background(), testNode("amd64", "k"), srv.URL, "x86-64"); err == nil {
		t.Fatal("want error for 404 index")
	}
}

func TestCheckBadSignature(t *testing.T) {
	k := newTestKey(t, true)
	index := "deadbeef  simplek8s.202601010000.x86-64.efi.zst\n"
	forger := newTestKey(t, true)
	sig := forger.sign(t, []byte(index), true)
	mux := http.NewServeMux()
	mux.HandleFunc("/SHA256SUMS", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(index)) })
	mux.HandleFunc("/SHA256SUMS.gpg", func(w http.ResponseWriter, r *http.Request) { w.Write(sig) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	f := testFeature(t, k)

	if _, err := f.Check(context.Background(), testNode("amd64", "k"), srv.URL, "x86-64"); err == nil {
		t.Fatal("want error for signature from unknown issuer")
	}
}

func TestCheckEmptyFlavor(t *testing.T) {
	k := newTestKey(t, true)
	srv := releaseRepo(t, k, true, kernelIndex())
	f := testFeature(t, k)

	if _, err := f.Check(context.Background(), testNode("amd64", "k"), srv.URL, ""); err == nil {
		t.Fatal("want error for unresolved flavor")
	}
}

func TestCheckNoRepoURL(t *testing.T) {
	f := &Feature{http: http.DefaultClient}
	if _, err := f.Check(context.Background(), testNode("amd64", "k"), "", "x86-64"); err == nil {
		t.Fatal("want error for empty repo URL")
	}
}

func TestCheckCustomKeyringWins(t *testing.T) {
	custom := newTestKey(t, true)
	embedded := newTestKey(t, true)
	srv := releaseRepo(t, custom, true, kernelIndex())
	f := &Feature{
		cfg: Config{
			EmbeddedKeyring: embedded.keyring, // would reject the custom key's signature
			CustomKeyring:   custom.keyring,
		},
		http: http.DefaultClient,
	}
	res, err := f.Check(context.Background(),
		testNode("amd64", "6.18.48-simplek8s-202601010000 (amd64)"), srv.URL, "x86-64")
	if err != nil {
		t.Fatalf("custom keyring must be used: %v", err)
	}
	if !res.Available {
		t.Fatalf("res = %+v", res)
	}
}

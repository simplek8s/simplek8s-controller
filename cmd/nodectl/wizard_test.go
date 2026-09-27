package main

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func withSeams(t *testing.T, yaml, kube, admin string) {
	t.Helper()
	oldY, oldK, oldA := wizardYAMLPath, wizardKubeDir, wizardAdminConf
	wizardYAMLPath, wizardKubeDir, wizardAdminConf = yaml, kube, admin
	t.Cleanup(func() { wizardYAMLPath, wizardKubeDir, wizardAdminConf = oldY, oldK, oldA })
}

func TestDetectWizardState(t *testing.T) {
	dir := t.TempDir()
	yaml := filepath.Join(dir, "simplek8s.yaml")
	kube := filepath.Join(dir, "kubernetes")
	admin := filepath.Join(kube, "admin.conf")
	withSeams(t, yaml, kube, admin)

	if got := detectWizardState(); got != wizLive {
		t.Fatalf("empty = %s, want live", got)
	}
	if err := os.WriteFile(yaml, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := detectWizardState(); got != wizFresh {
		t.Fatalf("yaml only = %s, want fresh", got)
	}
	if err := os.Mkdir(kube, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := detectWizardState(); got != wizWorker {
		t.Fatalf("k8s w/o admin = %s, want worker", got)
	}
	if err := os.WriteFile(admin, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := detectWizardState(); got != wizCP {
		t.Fatalf("admin.conf = %s, want cp", got)
	}
}

func TestParseInstallRequest(t *testing.T) {
	cases := []struct {
		name    string
		req     installRequest
		wantErr string
		wantCfg bool
	}{
		{"password ok", installRequest{Device: "/dev/sda", ConfirmDevice: "/dev/sda", Mode: "password", Password: "pw", PasswordConfirm: "pw"}, "", false},
		{"default mode is password", installRequest{Device: "/dev/sda", ConfirmDevice: "/dev/sda", Password: "pw", PasswordConfirm: "pw"}, "", false},
		{"missing device", installRequest{ConfirmDevice: "/dev/sda", Password: "pw", PasswordConfirm: "pw"}, "device is required", false},
		{"confirm mismatch", installRequest{Device: "/dev/sda", ConfirmDevice: "/dev/sdb", Password: "pw", PasswordConfirm: "pw"}, "does not match", false},
		{"empty password", installRequest{Device: "/dev/sda", ConfirmDevice: "/dev/sda", Mode: "password"}, "password is required", false},
		{"password mismatch", installRequest{Device: "/dev/sda", ConfirmDevice: "/dev/sda", Password: "a", PasswordConfirm: "b"}, "do not match", false},
		{"config ok", installRequest{Device: "/dev/sda", ConfirmDevice: "/dev/sda", Mode: "config", ConfigYAML: "users: []\n"}, "", true},
		{"password with keys", installRequest{Device: "/dev/sda", ConfirmDevice: "/dev/sda", Password: "pw", PasswordConfirm: "pw", SSHPublicKeys: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIabc user@h\n\nssh-rsa AAAAB3x== other@h\n"}, "", false},
		{"bad key line", installRequest{Device: "/dev/sda", ConfirmDevice: "/dev/sda", Password: "pw", PasswordConfirm: "pw", SSHPublicKeys: "not-a-key"}, "line 1", false},
		{"config empty", installRequest{Device: "/dev/sda", ConfirmDevice: "/dev/sda", Mode: "config"}, "config yaml is required", false},
		{"bad mode", installRequest{Device: "/dev/sda", ConfirmDevice: "/dev/sda", Mode: "yaml"}, "unknown mode", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mode, hash, keys, cfg, err := parseInstallRequest(tc.req)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if tc.wantCfg && cfg == "" {
				t.Fatal("want config yaml back")
			}
			if !tc.wantCfg && !strings.HasPrefix(hash, "$6$") {
				t.Fatalf("hash = %q, want $6$ shape", hash)
			}
			_ = mode
			_ = keys
		})
	}
}

func TestMinimalInstallYAMLWithKeys(t *testing.T) {
	out := minimalInstallYAML("202609210825", "$6$salt$hash", []string{"ssh-ed25519 AAAA user@h", "ssh-rsa BBBB other@h"})
	for _, want := range []string{
		"password_hash: $6$salt$hash\n",
		"ssh_authorized_keys:\n",
		`- "ssh-ed25519 AAAA user@h"` + "\n",
		`- "ssh-rsa BBBB other@h"` + "\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("keys yaml missing %q:\n%s", want, out)
		}
	}
}

func joinBlob(role, host string, certKey string) json.RawMessage {
	m := map[string]string{
		"address": "192.168.1.10:6443", "token": "abcdef.0123456789abcdef",
		"caCertHash": "sha256:" + strings.Repeat("a", 64),
		"role":       role, "hostname": host,
	}
	if certKey != "" {
		m["certificateKey"] = certKey
	}
	raw, _ := json.Marshal(m)
	return raw
}

func TestParseJoinConfig(t *testing.T) {
	key := strings.Repeat("b", 64)
	t.Run("worker object", func(t *testing.T) {
		jc, err := parseJoinConfig(joinBlob("worker", "wk3", ""))
		if err != nil {
			t.Fatal(err)
		}
		if args := buildJoinArgs(jc); len(args) != 8 || args[0] != "join" || args[len(args)-2] != "--node-name" {
			t.Fatalf("worker argv = %v", args)
		}
	})
	t.Run("worker string-wrapped", func(t *testing.T) {
		wrapped, _ := json.Marshal(string(joinBlob("worker", "wk3", "")))
		if _, err := parseJoinConfig(wrapped); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("control-plane argv", func(t *testing.T) {
		jc, err := parseJoinConfig(joinBlob("control-plane", "cp4", key))
		if err != nil {
			t.Fatal(err)
		}
		args := buildJoinArgs(jc)
		if len(args) != 11 || args[6] != "--control-plane" || args[7] != "--certificate-key" || args[8] != key {
			t.Fatalf("cp argv = %v", args)
		}
	})
	bads := []struct {
		name string
		mut  func(map[string]string)
		want string
	}{
		{"missing port", func(m map[string]string) { m["address"] = "192.168.1.10" }, "host:port"},
		{"bad token", func(m map[string]string) { m["token"] = "nope" }, "token must look like"},
		{"bad hash", func(m map[string]string) { m["caCertHash"] = "md5:abc" }, "caCertHash"},
		{"bad role", func(m map[string]string) { m["role"] = "etcd" }, "worker|control-plane"},
		{"certkey on worker", func(m map[string]string) { m["certificateKey"] = key }, "forbidden for worker"},
		{"missing certkey on cp", func(m map[string]string) { m["role"] = "control-plane" }, "required for control-plane"},
		{"bad hostname", func(m map[string]string) { m["hostname"] = "Wk_3!" }, "RFC 1123"},
	}
	for _, tc := range bads {
		t.Run(tc.name, func(t *testing.T) {
			m := map[string]string{
				"address": "192.168.1.10:6443", "token": "abcdef.0123456789abcdef",
				"caCertHash": "sha256:" + strings.Repeat("a", 64),
				"role":       "worker", "hostname": "wk3",
			}
			tc.mut(m)
			raw, _ := json.Marshal(m)
			if _, err := parseJoinConfig(raw); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want containing %q", err, tc.want)
			}
		})
	}
	t.Run("unknown field rejected", func(t *testing.T) {
		raw := []byte(`{"address":"h:1","token":"abcdef.0123456789abcdef","caCertHash":"sha256:` + strings.Repeat("a", 64) + `","role":"worker","hostname":"wk3","zzz":1}`)
		if _, err := parseJoinConfig(raw); err == nil {
			t.Fatal("want strict rejection")
		}
	})
}

func TestEditHostsContent(t *testing.T) {
	in := "127.0.0.1 localhost\n127.0.1.1 oldname\n# comment\n"
	got := editHostsContent(in, "wk3")
	if !strings.Contains(got, "127.0.1.1 wk3\n") || strings.Contains(got, "oldname") {
		t.Fatalf("replace:\n%s", got)
	}
	if !strings.Contains(got, "127.0.0.1 localhost") || !strings.Contains(got, "# comment") {
		t.Fatalf("passthrough broken:\n%s", got)
	}
	in2 := "127.0.0.1 localhost\n"
	got2 := editHostsContent(in2, "wk3")
	if !strings.HasSuffix(got2, "127.0.1.1 wk3\n") {
		t.Fatalf("append:\n%s", got2)
	}
	in3 := "127.0.0.1 localhost"
	if got3 := editHostsContent(in3, "wk3"); got3 != "127.0.0.1 localhost\n127.0.1.1 wk3" {
		t.Fatalf("no-trailing-newline:\n%q", got3)
	}
}

func TestMountinfoRootDev(t *testing.T) {
	in := "22 1 8:1 / / rw - ext4 /dev/sda1\n23 1 8:2 /boot /boot rw - ext4 /dev/sda2\n"
	maj, min, ok := mountinfoRootDev(in)
	if !ok || maj != 8 || min != 1 {
		t.Fatalf("got %d:%d ok=%v", maj, min, ok)
	}
	if _, _, ok := mountinfoRootDev("garbage\n"); ok {
		t.Fatal("want not-ok")
	}
}

func TestDefaultRouteInterface(t *testing.T) {
	routes := "Iface\tDestination\tGateway\tFlags\tRefCnt\tUse\tMetric\tMask\n" +
		"eth0\t00000000\t0102A8C0\t0003\t0\t0\t0\t00000000\n" +
		"eth0\t0042A8C0\t00000000\t0001\t0\t0\t0\t00FFFFFF\n"
	iface, ok := defaultRouteInterface(routes)
	if !ok || iface != "eth0" {
		t.Fatalf("got %q,%v want eth0,true", iface, ok)
	}
	noroute := "Iface\tDestination\tGateway\tFlags\tRefCnt\tUse\tMetric\tMask\n" +
		"eth0\t0042A8C0\t00000000\t0001\t0\t0\t0\t00FFFFFF\n"
	if _, ok := defaultRouteInterface(noroute); ok {
		t.Fatal("want no default")
	}
	// Default without gateway flag is not a default route.
	nogw := "Iface\tDestination\tGateway\tFlags\tRefCnt\tUse\tMetric\tMask\n" +
		"eth0\t00000000\t00000000\t0001\t0\t0\t0\t00000000\n"
	if _, ok := defaultRouteInterface(nogw); ok {
		t.Fatal("gateway-less default must not match")
	}
}

func TestDiskModel(t *testing.T) {
	sys := t.TempDir()
	dev := filepath.Join(sys, "sda", "device")
	if err := os.MkdirAll(dev, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sys, "sda", "size"), []byte("4194304\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldSys, oldMounts := wizardSysBlock, wizardMountinfo
	wizardSysBlock = sys
	wizardMountinfo = filepath.Join(t.TempDir(), "mountinfo")
	if err := os.WriteFile(wizardMountinfo, []byte("22 1 8:1 / / rw - ext4 /dev/sda1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { wizardSysBlock, wizardMountinfo = oldSys, oldMounts })

	if got := diskModel("sda"); got != "" {
		t.Fatalf("absent model = %q, want empty", got)
	}
	if err := os.WriteFile(filepath.Join(dev, "model"), []byte("QEMU HARDDISK  \n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := diskModel("sda"); got != "QEMU HARDDISK" {
		t.Fatalf("model = %q", got)
	}
	if err := os.WriteFile(filepath.Join(dev, "rev"), []byte("2.5+\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := diskModel("sda"); got != "QEMU HARDDISK (rev 2.5+)" {
		t.Fatalf("model+rev = %q", got)
	}

	// End to end through the handler.
	tok, err := sessions.mint()
	if err != nil {
		t.Fatal(err)
	}
	defer sessions.drop(tok)
	r := httptest.NewRequest("GET", "/api/disks", nil)
	r.AddCookie(&http.Cookie{Name: wizardSessionCookie, Value: tok})
	rec := httptest.NewRecorder()
	withSeams(t, filepath.Join(t.TempDir(), "y.yaml"), filepath.Join(t.TempDir(), "k"), filepath.Join(t.TempDir(), "k", "a"))
	disksHandler(testLog())(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("disks = %d, body %s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, `"model":"QEMU HARDDISK (rev 2.5+)"`) {
		t.Fatalf("body missing model: %s", body)
	}

	// virtio-blk has no model file: vendor is the fallback.
	if err := os.RemoveAll(dev); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dev, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dev, "vendor"), []byte("0x1af4\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := diskModel("sda"); got != "virtio 0x1af4" {
		t.Fatalf("vendor fallback = %q", got)
	}
}

func TestVirtualDisk(t *testing.T) {
	for _, n := range []string{"loop0", "ram1", "dm-0", "md0", "zram0", "sr0", "nbd0"} {
		if !virtualDisk(n) {
			t.Fatalf("%s should be virtual", n)
		}
	}
	for _, n := range []string{"sda", "vda", "nvme0n1", "mmcblk0", "xda"} {
		if virtualDisk(n) {
			t.Fatalf("%s should be real", n)
		}
	}
}

func TestParseJoinCommand(t *testing.T) {
	out := "kubeadm join 192.168.1.10:6443 --token abcdef.0123456789abcdef --discovery-token-ca-cert-hash sha256:" + strings.Repeat("c", 64) + "\n"
	addr, tok, h, err := parseJoinCommand("[preflight] ok\n" + out)
	if err != nil || addr != "192.168.1.10:6443" || tok != "abcdef.0123456789abcdef" || h != "sha256:"+strings.Repeat("c", 64) {
		t.Fatalf("got %q %q %q err=%v", addr, tok, h, err)
	}
	if _, _, _, err := parseJoinCommand("nothing here\n"); err == nil {
		t.Fatal("want error")
	}
}

func TestManualSetupCommands(t *testing.T) {
	hostCmd, hostsCmd := manualSetupCommands("wk3")
	if hostCmd != "hostnamectl set-hostname wk3" {
		t.Fatalf("hostCmd = %q", hostCmd)
	}
	wantHosts := "sed -i '/^127\\.0\\.1\\.1 /d' /etc/hosts && echo '127.0.1.1 wk3' >> /etc/hosts"
	if hostsCmd != wantHosts {
		t.Fatalf("hostsCmd = %q", hostsCmd)
	}
}

func TestBuildInitArgs(t *testing.T) {
	args := buildInitArgs("192.168.1.10", "cp1")
	want := []string{"init", "--apiserver-advertise-address=192.168.1.10", "--control-plane-endpoint=192.168.1.10:6443", "--pod-network-cidr=10.244.0.0/16", "--node-name=cp1"}
	if len(args) != len(want) {
		t.Fatalf("args = %v", args)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Fatalf("args = %v, want %v", args, want)
		}
	}
}

func TestKubeadmRunStreamsAndMasks(t *testing.T) {
	p := filepath.Join(t.TempDir(), "kubeadm")
	body := "#!/bin/sh\nprintf 'line1 secret-XYZ\\n'; sleep 5; printf 'line2 secret-XYZ\\n'"
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	old := wizardKubeadmPath
	wizardKubeadmPath = p
	t.Cleanup(func() { wizardKubeadmPath = old })
	j, ok := jobs.start("test-stream", "owner", false)
	if !ok {
		t.Fatal("job start failed")
	}
	type result struct {
		out string
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := kubeadmRun(context.Background(), testLog(), j, []string{}, []string{"secret-XYZ"})
		done <- result{out, err}
	}()
	// Lines must show up while the process still runs, masked.
	deadline := time.Now().Add(4 * time.Second)
	for {
		if n := len(j.snapshot().Log); n >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no streamed lines while process runs")
		}
		time.Sleep(200 * time.Millisecond)
	}
	res := <-done
	if res.err != nil {
		t.Fatalf("err = %v", res.err)
	}
	if !strings.Contains(res.out, "secret-XYZ") {
		t.Fatal("raw output must keep secrets for parsing")
	}
	want := []string{"line1 ***", "line2 ***"}
	if got := j.snapshot().Log; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("log = %v, want %v", got, want)
	}
}

func TestParseTokenExpiry(t *testing.T) {
	single := `{"kind":"BootstrapToken","apiVersion":"output.kubeadm.k8s.io/v1alpha3","token":"abcdef.0123456789abcdef","expires":"2026-09-28T00:00:00Z"}`
	if got := parseTokenExpiry([]byte(single), "abcdef"); got != "2026-09-28T00:00:00Z" {
		t.Fatalf("single got %q", got)
	}
	if got := parseTokenExpiry([]byte(single), "zzzzzz"); got != "" {
		t.Fatalf("single other got %q, want empty", got)
	}
	stream := single + "\n" + `{"kind":"BootstrapToken","token":"zzzzzz.1111111111111111","expires":"2026-09-29T00:00:00Z"}` + "\n"
	if got := parseTokenExpiry([]byte(stream), "zzzzzz"); got != "2026-09-29T00:00:00Z" {
		t.Fatalf("stream got %q", got)
	}
	if got := parseTokenExpiry([]byte("not json"), "abcdef"); got != "" {
		t.Fatalf("garbage got %q, want empty", got)
	}
}

func TestWizardEphemeralCertSANs(t *testing.T) {
	cert, err := wizardEphemeralCert()
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	foundDNS := false
	for _, n := range leaf.DNSNames {
		if n == "localhost" {
			foundDNS = true
		}
	}
	if !foundDNS {
		t.Fatalf("DNSNames = %v, want localhost", leaf.DNSNames)
	}
	foundIP := false
	for _, ip := range leaf.IPAddresses {
		if ip.Equal(net.ParseIP("127.0.0.1")) {
			foundIP = true
		}
	}
	if !foundIP {
		t.Fatalf("IPAddresses = %v, want 127.0.0.1", leaf.IPAddresses)
	}
}

func TestWizardIssueText(t *testing.T) {
	if got, want := wizardIssueText(5443), "\nSetup wizard available at: https://\\4:5443\n"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestSameOrigin(t *testing.T) {
	mk := func(origin, referer, host string) *http.Request {
		r := httptest.NewRequest("POST", "https://192.168.1.10:5443/api/login", nil)
		r.Host = host
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		if referer != "" {
			r.Header.Set("Referer", referer)
		}
		return r
	}
	if !sameOrigin(mk("https://192.168.1.10:5443", "", "192.168.1.10:5443")) {
		t.Fatal("matching origin should pass")
	}
	if sameOrigin(mk("https://evil.example", "", "192.168.1.10:5443")) {
		t.Fatal("foreign origin should fail")
	}
	if !sameOrigin(mk("", "https://192.168.1.10:5443/x", "192.168.1.10:5443")) {
		t.Fatal("matching referer should pass")
	}
	if !sameOrigin(mk("", "", "192.168.1.10:5443")) {
		t.Fatal("absent headers (non-browser) should pass")
	}
	if sameOrigin(mk("::bad::", "", "h")) {
		t.Fatal("unparseable origin should fail")
	}
}

func TestSessions(t *testing.T) {
	s := &wizardSessions{by: map[string]time.Time{}}
	mine, err := s.mint()
	if err != nil {
		t.Fatal(err)
	}
	if !s.valid(mine) {
		t.Fatal("fresh session invalid")
	}
	if s.valid("nope") {
		t.Fatal("unknown session valid")
	}
	s.mu.Lock()
	s.by[mine] = time.Now().Add(-time.Minute)
	s.mu.Unlock()
	if s.valid(mine) {
		t.Fatal("expired session valid")
	}
	s2, _ := s.mint()
	s.drop(s2)
	if s.valid(s2) {
		t.Fatal("dropped session valid")
	}
}

func TestLimiter(t *testing.T) {
	l := &wizardLimiter{ips: map[string]*wizardAttempt{}}
	ip := "10.0.0.9"
	for i := 0; i < 5; i++ {
		l.fail(ip)
		if l.denied(ip) {
			t.Fatalf("attempt %d should be free", i+1)
		}
	}
	l.fail(ip)
	if !l.denied(ip) {
		t.Fatal("6th attempt should block")
	}
	l.reset(ip)
	if l.denied(ip) {
		t.Fatal("reset should clear")
	}
}

// stubSu installs an executable stub as the su validator and
// returns its path.
func stubSu(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "su")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	old := wizardSuPath
	wizardSuPath = p
	t.Cleanup(func() { wizardSuPath = old })
	return p
}

func TestVerifyRootPassword(t *testing.T) {
	stub := stubSu(t, `read -r line; if [ "$line" = "good-pw" ]; then exit 0; else exit 1; fi`)
	oldCmd := suCommand
	suCommand = func(ctx context.Context, stdin io.Reader) *exec.Cmd {
		cmd := exec.CommandContext(ctx, stub)
		cmd.Stdin = stdin
		return cmd
	}
	t.Cleanup(func() { suCommand = oldCmd })
	if ok, err := verifyRootPassword("good-pw"); err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if ok, err := verifyRootPassword("bad-pw"); err != nil || ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
}

func TestVerifyRootPasswordTimeout(t *testing.T) {
	stub := stubSu(t, `sleep 30`)
	oldCmd := suCommand
	suCommand = func(ctx context.Context, stdin io.Reader) *exec.Cmd {
		cmd := exec.CommandContext(ctx, stub)
		cmd.Stdin = stdin
		return cmd
	}
	t.Cleanup(func() { suCommand = oldCmd })
	old := wizardAuthTimeout
	wizardAuthTimeout = 150 * time.Millisecond
	t.Cleanup(func() { wizardAuthTimeout = old })
	if _, err := verifyRootPassword("x"); err == nil {
		t.Fatal("want timeout error")
	}
}

func TestStateGate(t *testing.T) {
	dir := t.TempDir()
	withSeams(t, filepath.Join(dir, "y.yaml"), filepath.Join(dir, "k"), filepath.Join(dir, "k", "a"))
	log := testLog()
	// live: disks allowed with a session.
	tok, _ := sessions.mint()
	defer sessions.drop(tok)
	ok := func(target string, states []wizardState) int {
		h := withSession(log, states, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		})
		r := httptest.NewRequest("GET", target, nil)
		r.AddCookie(&http.Cookie{Name: wizardSessionCookie, Value: tok})
		rec := httptest.NewRecorder()
		h(rec, r)
		return rec.Code
	}
	if code := ok("/api/disks", []wizardState{wizLive}); code != http.StatusNoContent {
		t.Fatalf("live disks = %d", code)
	}
	// no cookie → 401.
	h := withSession(log, nil, func(w http.ResponseWriter, r *http.Request) {})
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest("GET", "/api/x", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no cookie = %d", rec.Code)
	}
	// fresh (yaml, no k8s): live-only route → 403.
	if err := os.WriteFile(filepath.Join(dir, "y.yaml"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := ok("/api/disks", []wizardState{wizLive}); code != http.StatusForbidden {
		t.Fatalf("fresh disks = %d, want 403", code)
	}
	// jobs route (nil states) still passes across the flip.
	if code := ok("/api/jobs/1", nil); code != http.StatusNoContent {
		t.Fatalf("jobs across flip = %d", code)
	}
}

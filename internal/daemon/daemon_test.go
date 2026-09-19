package daemon

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.qingyu31.com/servd/internal/config"
)

var testBinary string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "servd-daemon-bin-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	name := "servd"
	if runtime.GOOS == "windows" {
		name = "servd.exe"
	}
	testBinary = filepath.Join(dir, name)
	build := exec.Command("go", "build", "-o", testBinary, "./cmd/servd")
	build.Dir = filepath.Join("..", "..")
	if out, err := build.CombinedOutput(); err != nil {
		os.RemoveAll(dir)
		fmt.Fprintf(os.Stderr, "build test binary: %v\n%s", err, out)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

const defaultTestPolicy = `{"maxRestarts":3,"backoffBaseMs":100,"stableResetMs":2000,"stopTimeoutMs":3000}`

// writeDaemonTestCert stores a self-signed PEM pair for 127.0.0.1 in dir.
func writeDaemonTestCert(t *testing.T, dir string) (certPath, keyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "servd daemon test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPath = filepath.Join(dir, "fullchain.pem")
	keyPath = filepath.Join(dir, "privkey.pem")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}

type daemonTest struct {
	t        *testing.T
	binary   string
	port     int
	cache    string
	static   string
	policy   string
	certPath string
	keyPath  string
	client   *http.Client
}

func newDaemonTest(t *testing.T) *daemonTest {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	static := t.TempDir()
	if err := os.WriteFile(filepath.Join(static, "index.html"), []byte("hello daemon"), 0o644); err != nil {
		t.Fatal(err)
	}
	d := &daemonTest{
		t: t, binary: testBinary, port: port,
		cache: t.TempDir(), static: static, policy: defaultTestPolicy,
	}
	t.Setenv(cacheDirEnv, d.cache)
	t.Cleanup(func() { d.stopQuiet() })
	return d
}

func (d *daemonTest) command(args ...string) *exec.Cmd {
	cmd := exec.Command(d.binary, args...)
	cmd.Env = append(os.Environ(),
		cacheDirEnv+"="+d.cache,
		policyEnv+"="+d.policy,
	)
	return cmd
}

func (d *daemonTest) run(args ...string) (int, string, string) {
	d.t.Helper()
	var stdout, stderr strings.Builder
	cmd := d.command(args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			d.t.Fatalf("run %v: %v", args, err)
		}
		return exit.ExitCode(), stdout.String(), stderr.String()
	}
	return 0, stdout.String(), stderr.String()
}

func (d *daemonTest) start() {
	d.t.Helper()
	args := []string{"--daemon", "--port=" + strconv.Itoa(d.port), "--static=" + d.static}
	marker := "daemon started on 0.0.0.0:" + strconv.Itoa(d.port)
	if d.certPath != "" {
		args = append(args, "--tls-cert="+d.certPath, "--tls-key="+d.keyPath)
		marker = "https " + marker
	}
	code, stdout, stderr := d.run(args...)
	if code != 0 {
		d.t.Fatalf("daemon start failed: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, marker) {
		d.t.Fatalf("start output missing %q: %q", marker, stdout)
	}
}

// enableTLS generates a certificate pair for 127.0.0.1 and installs a client
// that trusts it; the trust roots are snapshotted before any later mutation
// of the files on disk.
func (d *daemonTest) enableTLS() {
	d.t.Helper()
	certPath, keyPath := writeDaemonTestCert(d.t, d.t.TempDir())
	d.certPath, d.keyPath = certPath, keyPath
	pool := x509.NewCertPool()
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		d.t.Fatal(err)
	}
	if !pool.AppendCertsFromPEM(certPEM) {
		d.t.Fatal("daemon test certificate rejected by pool")
	}
	d.client = &http.Client{
		Timeout:   2 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}},
	}
}

func (d *daemonTest) httpsGet() (string, error) {
	resp, err := d.client.Get(fmt.Sprintf("https://127.0.0.1:%d/", d.port))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

func (d *daemonTest) awaitHTTPS(timeout time.Duration) string {
	d.t.Helper()
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		body, err := d.httpsGet()
		if err == nil {
			return body
		}
		lastErr = err
		time.Sleep(50 * time.Millisecond)
	}
	d.t.Fatalf("HTTPS on port %d never became ready: %v", d.port, lastErr)
	return ""
}

func (d *daemonTest) stop() {
	d.t.Helper()
	if code, stdout, stderr := d.run("--stop", "--port="+strconv.Itoa(d.port)); code != 0 {
		d.t.Fatalf("daemon stop failed: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

func (d *daemonTest) stopQuiet() {
	// Cleanup must never abort on a missing binary or a failed command.
	cmd := exec.Command(d.binary, "--stop", "--port="+strconv.Itoa(d.port))
	cmd.Env = append(os.Environ(),
		cacheDirEnv+"="+d.cache,
		policyEnv+"="+d.policy,
	)
	_ = cmd.Run()
}

func (d *daemonTest) instanceDir() string {
	return filepath.Join(d.cache, "servd", strconv.Itoa(d.port))
}

func (d *daemonTest) logPath() string {
	return filepath.Join(d.instanceDir(), logFile)
}

func (d *daemonTest) httpGet() (string, error) {
	client := &http.Client{Timeout: time.Second}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/", d.port))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

func (d *daemonTest) awaitHTTP(timeout time.Duration) string {
	d.t.Helper()
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		body, err := d.httpGet()
		if err == nil {
			return body
		}
		lastErr = err
		time.Sleep(50 * time.Millisecond)
	}
	d.t.Fatalf("HTTP on port %d never became ready: %v", d.port, lastErr)
	return ""
}

func (d *daemonTest) awaitHTTPDown(timeout time.Duration) {
	d.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := d.httpGet(); err != nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	d.t.Fatalf("HTTP on port %d never went down", d.port)
}

func (d *daemonTest) awaitStatus(timeout time.Duration, want func(controlResponse) bool) controlResponse {
	d.t.Helper()
	deadline := time.Now().Add(timeout)
	var last controlResponse
	for time.Now().Before(deadline) {
		resp, err := query(d.port, "status")
		if err == nil && resp != nil && want(*resp) {
			return *resp
		}
		if resp != nil {
			last = *resp
		}
		time.Sleep(50 * time.Millisecond)
	}
	d.t.Fatalf("timed out waiting for daemon status; last=%+v", last)
	return last
}

func isRunning(r controlResponse) bool { return r.Status == "running" }
func isFailed(r controlResponse) bool  { return r.Status == "failed" }
func isBackoff(r controlResponse) bool { return r.Status == "backoff" }
func isState(r string) func(controlResponse) bool {
	return func(c controlResponse) bool { return c.Status == r }
}

func killProcess(t *testing.T, pid int) {
	t.Helper()
	if pid == 0 {
		return
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return
	}
	if err := proc.Signal(os.Kill); err != nil && !errors.Is(err, os.ErrProcessDone) {
		t.Logf("kill pid %d: %v", pid, err)
	}
}

func readDiscovery(t *testing.T, dir string) discovery {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, discoveryFile))
	if err != nil {
		t.Fatal(err)
	}
	var d discovery
	if err := json.Unmarshal(data, &d); err != nil {
		t.Fatalf("discovery unreadable: %v", err)
	}
	return d
}

func TestDaemonLifecycleAndStatus(t *testing.T) {
	d := newDaemonTest(t)

	code, stdout, _ := d.run("--status", "--port="+strconv.Itoa(d.port))
	if code != 1 || !strings.Contains(stdout, "not running") {
		t.Fatalf("status before start: code=%d stdout=%q", code, stdout)
	}
	code, stdout, _ = d.run("--stop", "--port="+strconv.Itoa(d.port))
	if code != 0 || !strings.Contains(stdout, "not running") {
		t.Fatalf("stop before start must be idempotent: code=%d stdout=%q", code, stdout)
	}

	d.start()
	resp := d.awaitStatus(5*time.Second, isRunning)
	if resp.SupervisorPid == 0 || resp.WorkerPid == 0 || resp.Restarts != 0 {
		t.Fatalf("unexpected running status: %+v", resp)
	}
	if body := d.awaitHTTP(5 * time.Second); body != "hello daemon" {
		t.Fatalf("body = %q", body)
	}

	// status is location independent: query from an unrelated working directory
	cmd := d.command("--status", "--port="+strconv.Itoa(d.port))
	cmd.Dir = t.TempDir()
	out, err := cmd.Output()
	if err != nil || !strings.Contains(string(out), "running") {
		t.Fatalf("status from unrelated cwd: %v %q", err, out)
	}

	if runtime.GOOS != "windows" {
		info, err := os.Stat(d.instanceDir())
		if err != nil || info.Mode().Perm() != 0o700 {
			t.Fatalf("instance dir perms: %v %v", info.Mode().Perm(), err)
		}
		state, err := os.Stat(filepath.Join(d.instanceDir(), discoveryFile))
		if err != nil || state.Mode().Perm() != 0o600 {
			t.Fatalf("discovery perms: %v %v", state.Mode().Perm(), err)
		}
	}

	d.stop()
	d.awaitHTTPDown(5 * time.Second)
	if _, err := query(d.port, "status"); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("status after stop: %v", err)
	}
	code, stdout, _ = d.run("--status", "--port="+strconv.Itoa(d.port))
	if code != 1 || !strings.Contains(stdout, "not running") {
		t.Fatalf("status after stop: code=%d stdout=%q", code, stdout)
	}

	logData, err := os.ReadFile(d.logPath())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(logData), "supervisor: handoff complete") || !strings.Contains(string(logData), "worker: servd") {
		t.Fatalf("log missing supervisor/worker entries:\n%s", logData)
	}
}

func TestDaemonStartFailsOnOccupiedPort(t *testing.T) {
	d := newDaemonTest(t)
	listener, err := net.Listen("tcp4", net.JoinHostPort("0.0.0.0", strconv.Itoa(d.port)))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	code, _, stderr := d.run("--daemon", "--port="+strconv.Itoa(d.port), "--static="+d.static)
	if code != 1 {
		t.Fatalf("expected failure code 1, got %d", code)
	}
	if !strings.Contains(stderr, "daemon failed to start") {
		t.Fatalf("stderr missing start failure: %q", stderr)
	}
	if _, err := query(d.port, "status"); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("instance lock leaked after failed start: %v", err)
	}
}

func TestDaemonDuplicateStartRejected(t *testing.T) {
	d := newDaemonTest(t)
	d.start()
	before := d.awaitStatus(5*time.Second, isRunning)

	code, _, stderr := d.run("--daemon", "--port="+strconv.Itoa(d.port), "--static="+d.static)
	if code != 1 || !strings.Contains(stderr, "already has a daemon") {
		t.Fatalf("duplicate start: code=%d stderr=%q", code, stderr)
	}
	after := d.awaitStatus(5*time.Second, isRunning)
	if after.WorkerPid != before.WorkerPid || after.SupervisorPid != before.SupervisorPid {
		t.Fatalf("duplicate start disturbed the running instance: %+v -> %+v", before, after)
	}
}

func TestDaemonConcurrentStartSingleWinner(t *testing.T) {
	d := newDaemonTest(t)
	const starters = 4
	results := make(chan int, starters)
	for i := 0; i < starters; i++ {
		go func() {
			cmd := d.command("--daemon", "--port="+strconv.Itoa(d.port), "--static="+d.static)
			err := cmd.Run()
			if err == nil {
				results <- 0
				return
			}
			var exit *exec.ExitError
			if errors.As(err, &exit) {
				results <- exit.ExitCode()
				return
			}
			results <- -1
		}()
	}
	successes := 0
	for i := 0; i < starters; i++ {
		if <-results == 0 {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("expected exactly one successful start, got %d", successes)
	}
	d.awaitStatus(5*time.Second, isRunning)
	d.awaitHTTP(5 * time.Second)
}

func TestDaemonIndependentPorts(t *testing.T) {
	a := newDaemonTest(t)
	b := newDaemonTest(t)
	a.start()
	b.start()
	if body := a.awaitHTTP(5 * time.Second); body != "hello daemon" {
		t.Fatalf("a body = %q", body)
	}
	if body := b.awaitHTTP(5 * time.Second); body != "hello daemon" {
		t.Fatalf("b body = %q", body)
	}

	a.stop()
	a.awaitHTTPDown(5 * time.Second)
	if _, err := b.httpGet(); err != nil {
		t.Fatalf("stopping port %d disturbed port %d: %v", a.port, b.port, err)
	}
	b.stop()
	b.awaitHTTPDown(5 * time.Second)
}

func TestDaemonCrashRestart(t *testing.T) {
	d := newDaemonTest(t)
	d.start()
	first := d.awaitStatus(5*time.Second, isRunning)

	killProcess(t, first.WorkerPid)
	if body := d.awaitHTTP(10 * time.Second); body != "hello daemon" {
		t.Fatalf("body after restart = %q", body)
	}
	second := d.awaitStatus(10*time.Second, func(r controlResponse) bool {
		return r.Status == "running" && r.WorkerPid != first.WorkerPid
	})
	if second.Restarts != 1 {
		t.Fatalf("restarts after one crash = %d, want 1", second.Restarts)
	}
}

func TestDaemonRestartBudgetExhaustion(t *testing.T) {
	d := newDaemonTest(t) // maxRestarts=3
	d.start()

	deadline := time.Now().Add(30 * time.Second)
	for {
		resp, err := query(d.port, "status")
		if err != nil {
			t.Fatalf("status while exhausting budget: %v", err)
		}
		if resp.Status == "failed" {
			if resp.Restarts != 3 {
				t.Fatalf("failed after %d restarts, want 3", resp.Restarts)
			}
			break
		}
		if resp.Status == "running" {
			killProcess(t, resp.WorkerPid)
		}
		if time.Now().After(deadline) {
			t.Fatalf("never reached failed state, last=%+v", resp)
		}
		time.Sleep(100 * time.Millisecond)
	}

	// failed state stays queryable and does not restart
	code, stdout, _ := d.run("--status", "--port="+strconv.Itoa(d.port))
	if code != 1 || !strings.Contains(stdout, "failed") {
		t.Fatalf("status of failed daemon: code=%d stdout=%q", code, stdout)
	}
	d.awaitHTTPDown(5 * time.Second)

	d.stop()
	d.start()
	if body := d.awaitHTTP(10 * time.Second); body != "hello daemon" {
		t.Fatalf("body after recovery = %q", body)
	}
}

func TestDaemonStableRuntimeResetsBudget(t *testing.T) {
	d := newDaemonTest(t)
	d.policy = `{"maxRestarts":1,"backoffBaseMs":100,"stableResetMs":1500,"stopTimeoutMs":3000}`
	d.start()
	time.Sleep(1700 * time.Millisecond) // exceed the stable window
	first := d.awaitStatus(5*time.Second, isRunning)

	killProcess(t, first.WorkerPid)
	// budget was reset, so the restart happens despite maxRestarts=1
	second := d.awaitStatus(15*time.Second, func(r controlResponse) bool {
		return r.Status == "running" && r.WorkerPid != first.WorkerPid
	})
	// immediate second crash without a stable window exhausts the budget
	killProcess(t, second.WorkerPid)
	d.awaitStatus(15*time.Second, isFailed)
}

func TestDaemonStopDuringBackoff(t *testing.T) {
	d := newDaemonTest(t)
	d.policy = `{"maxRestarts":5,"backoffBaseMs":3000,"stableResetMs":60000,"stopTimeoutMs":3000}`
	d.start()
	first := d.awaitStatus(5*time.Second, isRunning)

	killProcess(t, first.WorkerPid)
	d.awaitStatus(5*time.Second, isBackoff)

	begin := time.Now()
	d.stop()
	if elapsed := time.Since(begin); elapsed > 5*time.Second {
		t.Fatalf("stop during backoff took %s", elapsed)
	}
	d.awaitHTTPDown(5 * time.Second)
	time.Sleep(500 * time.Millisecond)
	if _, err := d.httpGet(); err == nil {
		t.Fatal("worker restarted after stop during backoff")
	}
	if _, err := query(d.port, "status"); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("status after stop during backoff: %v", err)
	}
}

func TestDaemonWorkerExitsWhenSupervisorDies(t *testing.T) {
	d := newDaemonTest(t)
	d.start()
	resp := d.awaitStatus(5*time.Second, isRunning)

	killProcess(t, resp.SupervisorPid)
	d.awaitHTTPDown(10 * time.Second)
	if _, err := query(d.port, "status"); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("status after supervisor death: %v", err)
	}
}

func TestDaemonControlAuthentication(t *testing.T) {
	d := newDaemonTest(t)
	d.start()
	d.awaitStatus(5*time.Second, isRunning)
	disc := readDiscovery(t, d.instanceDir())

	if disc.Port != d.port || disc.V != protocolVersion {
		t.Fatalf("discovery content: %+v", disc)
	}
	address, err := loopbackAddress(disc.ControlAddr)
	if err != nil {
		t.Fatal(err)
	}

	conn, err := net.DialTimeout("tcp", address, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	jc := newJSONConn(conn)
	if err := jc.write(controlRequest{V: protocolVersion, Token: "forged-token", Command: "status"}); err != nil {
		t.Fatal(err)
	}
	var resp controlResponse
	if err := jc.read(&resp, 3*time.Second); err != nil {
		t.Fatal(err)
	}
	if resp.OK {
		t.Fatal("control accepted a forged token")
	}
	jc.close()

	conn, err = net.DialTimeout("tcp", address, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	jc = newJSONConn(conn)
	if err := jc.write(controlRequest{V: protocolVersion, Token: disc.Token, Command: "explode"}); err != nil {
		t.Fatal(err)
	}
	if err := jc.read(&resp, 3*time.Second); err != nil {
		t.Fatal(err)
	}
	if resp.OK || !strings.Contains(resp.Error, "unknown command") {
		t.Fatalf("unexpected response to unknown command: %+v", resp)
	}
	jc.close()
}

func TestDaemonRejectsForgedDiscoveryState(t *testing.T) {
	d := newDaemonTest(t)
	dir := d.instanceDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	// state without a lock holder is reported as not running, never dialed
	bad := discovery{V: protocolVersion, ControlAddr: "10.9.8.7:1234", Token: "x", Port: d.port}
	writeDiscoveryFile(t, dir, bad)
	if _, err := query(d.port, "status"); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("unlocked state was used: %v", err)
	}

	lock, err := acquireLock(filepath.Join(dir, "instance.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()

	_, err = query(d.port, "status")
	if err == nil || !strings.Contains(err.Error(), "loopback") {
		t.Fatalf("non-loopback control address accepted: %v", err)
	}

	// discovery for a different port must be rejected as unreadable
	bad.Port = d.port + 1
	writeDiscoveryFile(t, dir, bad)
	_, err = query(d.port, "status")
	if err == nil || !strings.Contains(err.Error(), "unreadable") {
		t.Fatalf("mismatched discovery accepted: %v", err)
	}
}

func writeDiscoveryFile(t *testing.T, dir string, d discovery) {
	t.Helper()
	if err := writeDiscovery(dir, d.ControlAddr, d.Token, d.Port); err != nil {
		t.Fatal(err)
	}
}

func TestDaemonRestartsAfterOriginalBinaryRemoved(t *testing.T) {
	d := newDaemonTest(t)
	source, err := os.ReadFile(testBinary)
	if err != nil {
		t.Fatal(err)
	}
	original := filepath.Join(t.TempDir(), filepath.Base(testBinary))
	if err := os.WriteFile(original, source, 0o700); err != nil {
		t.Fatal(err)
	}
	d.binary = original

	d.start()
	first := d.awaitStatus(5*time.Second, isRunning)
	if err := os.Remove(original); err != nil {
		t.Fatal(err)
	}
	// a reinstalled CLI must still be able to manage the orphaned instance
	d.binary = testBinary

	killProcess(t, first.WorkerPid)
	if body := d.awaitHTTP(10 * time.Second); body != "hello daemon" {
		t.Fatalf("body after restart without original binary = %q", body)
	}
	second := d.awaitStatus(10*time.Second, func(r controlResponse) bool {
		return r.Status == "running" && r.WorkerPid != first.WorkerPid
	})
	if second.Restarts != 1 {
		t.Fatalf("restarts = %d, want 1", second.Restarts)
	}
}

func TestSupervisorRecyclesWhenStarterDisappears(t *testing.T) {
	d := newDaemonTest(t)
	// Reserve then close a port so the supervisor's handshake dial fails.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	starter := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	dir := d.instanceDir()
	wire, err := marshalConfig(config.Config{
		Port:        d.port,
		Directories: []config.Directory{{Mount: "/", Path: d.static}},
	})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(testBinary)
	cmd.Env = append(os.Environ(),
		cacheDirEnv+"="+d.cache,
		roleEnv+"="+roleSupervisor,
		configEnv+"="+string(wire),
		handshakeAddrEnv+"="+starter,
		handshakeTokenEnv+"="+"starter-token",
		instanceDirEnv+"="+dir,
	)
	if err := cmd.Run(); err == nil {
		t.Fatal("supervisor should exit non-zero when the starter is unreachable")
	}

	held, err := lockHeld(filepath.Join(dir, "instance.lock"))
	if err != nil {
		t.Fatal(err)
	}
	if held {
		t.Fatal("instance lock leaked after failed handoff")
	}
	if _, err := os.Stat(filepath.Join(dir, discoveryFile)); !os.IsNotExist(err) {
		t.Fatalf("discovery file survived failed handoff: %v", err)
	}
	// the freshly started worker must have been recycled
	d.awaitHTTPDown(10 * time.Second)
}

func TestDaemonWireConfigTLSRoundTrip(t *testing.T) {
	cfg := config.Config{
		Port:    8443,
		TLSCert: "/certs/fullchain.pem",
		TLSKey:  "/certs/privkey.pem",
		Directories: []config.Directory{
			{Mount: "/", Path: "/srv/www"},
		},
	}
	data, err := marshalConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	got, err := unmarshalConfig(data)
	if err != nil {
		t.Fatal(err)
	}
	if got.TLSCert != cfg.TLSCert || got.TLSKey != cfg.TLSKey {
		t.Fatalf("TLS paths lost in wire round trip: cert=%q key=%q", got.TLSCert, got.TLSKey)
	}

	plain, err := marshalConfig(config.Config{Port: 8080, Directories: cfg.Directories})
	if err != nil {
		t.Fatal(err)
	}
	got, err = unmarshalConfig(plain)
	if err != nil {
		t.Fatal(err)
	}
	if got.TLSCert != "" || got.TLSKey != "" {
		t.Fatalf("plain config gained TLS paths: cert=%q key=%q", got.TLSCert, got.TLSKey)
	}
}

func TestDaemonHTTPS(t *testing.T) {
	d := newDaemonTest(t)
	d.enableTLS()

	d.start()
	if body := d.awaitHTTPS(10 * time.Second); body != "hello daemon" {
		t.Fatalf("https body = %q", body)
	}
	first := d.awaitStatus(5*time.Second, isRunning)

	// A plaintext request against the TLS port never serves site content.
	if body, err := d.httpGet(); err == nil && body == "hello daemon" {
		t.Fatal("plaintext request served protected content")
	}

	// Crash recovery keeps serving HTTPS.
	killProcess(t, first.WorkerPid)
	if body := d.awaitHTTPS(15 * time.Second); body != "hello daemon" {
		t.Fatalf("https body after restart = %q", body)
	}
	second := d.awaitStatus(10*time.Second, func(r controlResponse) bool {
		return r.Status == "running" && r.WorkerPid != first.WorkerPid
	})
	if second.Restarts != 1 {
		t.Fatalf("restarts = %d, want 1", second.Restarts)
	}

	d.stop()
	d.awaitHTTPDown(5 * time.Second)
	if body, err := d.httpsGet(); err == nil && body == "hello daemon" {
		t.Fatal("https still serving after stop")
	}
}

func TestDaemonTLSBadCertificateFailsFast(t *testing.T) {
	d := newDaemonTest(t)
	d.enableTLS()
	if err := os.WriteFile(d.keyPath, []byte("not a pem key"), 0o600); err != nil {
		t.Fatal(err)
	}

	code, _, stderr := d.run("--daemon", "--port="+strconv.Itoa(d.port), "--static="+d.static,
		"--tls-cert="+d.certPath, "--tls-key="+d.keyPath)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr, "daemon failed to start") || !strings.Contains(stderr, "load TLS certificate") {
		t.Fatalf("stderr missing certificate failure: %q", stderr)
	}
	if _, err := query(d.port, "status"); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("failed first start left state behind: %v", err)
	}
	// The failure is a first-start error, so no restart loop runs.
	d.awaitHTTPDown(5 * time.Second)
}

func TestDaemonTLSCorruptAfterRunNeverFallsBackToPlaintext(t *testing.T) {
	d := newDaemonTest(t)
	d.policy = `{"maxRestarts":2,"backoffBaseMs":100,"stableResetMs":60000,"stopTimeoutMs":3000}`
	d.enableTLS()
	d.start()
	if body := d.awaitHTTPS(10 * time.Second); body != "hello daemon" {
		t.Fatalf("https body = %q", body)
	}

	if err := os.WriteFile(d.certPath, []byte("corrupted certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	first := d.awaitStatus(5*time.Second, isRunning)
	killProcess(t, first.WorkerPid)

	failed := d.awaitStatus(20*time.Second, isFailed)
	if !strings.Contains(failed.LastExit, "load TLS certificate") {
		t.Fatalf("restarts did not fail on the certificate: last exit %q", failed.LastExit)
	}
	if body, err := d.httpsGet(); err == nil && body == "hello daemon" {
		t.Fatal("https still serving after certificate failure")
	}
	if body, err := d.httpGet(); err == nil && body == "hello daemon" {
		t.Fatal("daemon fell back to plaintext HTTP")
	}
	d.stop()
}

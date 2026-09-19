package daemon

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"go.qingyu31.com/servd/internal/app"
	"go.qingyu31.com/servd/internal/config"
)

const (
	roleEnv           = "SERVD_INTERNAL_ROLE"
	configEnv         = "SERVD_INTERNAL_CONFIG"
	policyEnv         = "SERVD_INTERNAL_POLICY"
	cacheDirEnv       = "SERVD_INTERNAL_CACHE_DIR"
	handshakeAddrEnv  = "SERVD_INTERNAL_HANDSHAKE_ADDR"
	handshakeTokenEnv = "SERVD_INTERNAL_HANDSHAKE_TOKEN"
	instanceDirEnv    = "SERVD_INTERNAL_INSTANCE_DIR"
	manageAddrEnv     = "SERVD_INTERNAL_MANAGE_ADDR"
	manageTokenEnv    = "SERVD_INTERNAL_MANAGE_TOKEN"

	roleSupervisor = "supervisor"
	roleWorker     = "worker"

	discoveryFile = "control.json"
	logFile       = "servd.log"
)

// ErrNotRunning reports that no daemon holds the instance for a port.
var ErrNotRunning = errors.New("not running")

// RunInternal executes an internal supervisor or worker role when this
// process was spawned by servd itself. handled reports whether the process
// is internal and must not parse user flags.
func RunInternal(stdout, stderr io.Writer) (code int, handled bool) {
	switch os.Getenv(roleEnv) {
	case "":
		return 0, false
	case roleSupervisor:
		return runSupervisor(os.Getenv(instanceDirEnv)), true
	case roleWorker:
		return runWorker(stdout), true
	default:
		fmt.Fprintln(stderr, "servd: unknown internal role")
		return 2, true
	}
}

// Start launches a background daemon for cfg.Port and returns only after the
// worker is actually listening.
func Start(cfg config.Config, stdout io.Writer) error {
	dir, err := instanceDir(cfg.Port)
	if err != nil {
		return err
	}
	launch, err := acquireLock(filepath.Join(dir, "launch.lock"))
	if err != nil {
		return fmt.Errorf("another servd command is still starting or stopping port %d", cfg.Port)
	}
	defer launch.Close()
	held, err := lockHeld(filepath.Join(dir, "instance.lock"))
	if err != nil {
		return err
	}
	if held {
		return fmt.Errorf("port %d already has a daemon (check --status, or run --stop first)", cfg.Port)
	}
	cleanBinaries(dir)
	binPath, err := copySelf(dir)
	if err != nil {
		return fmt.Errorf("prepare daemon binary: %w", err)
	}
	wire, err := marshalConfig(cfg)
	if err != nil {
		return err
	}
	handshake, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer handshake.Close()
	token, err := randomToken()
	if err != nil {
		return err
	}
	cmd := exec.Command(binPath)
	cmd.SysProcAttr = detachAttr()
	cmd.Env = append(filteredEnv(),
		roleEnv+"="+roleSupervisor,
		configEnv+"="+string(wire),
		handshakeAddrEnv+"="+handshake.Addr().String(),
		handshakeTokenEnv+"="+token,
		instanceDirEnv+"="+dir,
	)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start supervisor: %w", err)
	}
	procDone := make(chan error, 1)
	go func() { procDone <- cmd.Wait() }()
	msg, err := awaitHandshake(handshake, token, 45*time.Second, procDone)
	if err != nil {
		select {
		case <-procDone:
		default:
			_ = cmd.Process.Kill()
			<-procDone
		}
		return err
	}
	scheme := "http"
	if cfg.TLSCert != "" {
		scheme = "https"
	}
	fmt.Fprintf(stdout, "servd %s %s daemon started on 0.0.0.0:%d (supervisor pid %d, worker pid %d)\n",
		app.Version, scheme, cfg.Port, msg.SupervisorPid, msg.WorkerPid)
	fmt.Fprintf(stdout, "log: %s\n", msg.LogPath)
	return nil
}

// Status prints the daemon state for port and returns 0 only while running.
func Status(port int, stdout, stderr io.Writer) int {
	resp, err := query(port, "status")
	if errors.Is(err, ErrNotRunning) {
		fmt.Fprintf(stdout, "servd :%d is not running\n", port)
		return 1
	}
	if err != nil {
		fmt.Fprintln(stderr, "servd:", err)
		return 1
	}
	printStatus(stdout, *resp)
	if resp.Status != "running" {
		return 1
	}
	return 0
}

// Stop terminates the daemon for port; stopping an absent daemon succeeds.
func Stop(port int, stdout, stderr io.Writer) int {
	resp, err := query(port, "stop")
	if errors.Is(err, ErrNotRunning) {
		fmt.Fprintf(stdout, "servd :%d is not running\n", port)
		return 0
	}
	if err != nil {
		fmt.Fprintln(stderr, "servd:", err)
		return 1
	}
	fmt.Fprintf(stdout, "servd :%d stopped\n", resp.Port)
	return 0
}

func printStatus(w io.Writer, r controlResponse) {
	switch r.Status {
	case "running":
		fmt.Fprintf(w, "servd :%d running (supervisor pid %d, worker pid %d, restarts %d)\n",
			r.Port, r.SupervisorPid, r.WorkerPid, r.Restarts)
	case "backoff":
		fmt.Fprintf(w, "servd :%d restarting (restart %d/%d, next in %dms, last exit: %s)\n",
			r.Port, r.Restarts, r.MaxRestarts, r.NextRestartMs, r.LastExit)
	case "failed":
		fmt.Fprintf(w, "servd :%d failed (restarts %d, last exit: %s); run --stop then --daemon to recover\n",
			r.Port, r.Restarts, r.LastExit)
	default:
		fmt.Fprintf(w, "servd :%d %s (supervisor pid %d, restarts %d", r.Port, r.Status, r.SupervisorPid, r.Restarts)
		if r.LastExit != "" {
			fmt.Fprintf(w, ", last exit: %s", r.LastExit)
		}
		fmt.Fprintln(w, ")")
	}
	fmt.Fprintf(w, "log: %s\n", r.LogPath)
}

// query sends one authenticated command to the daemon control endpoint.
func query(port int, command string) (*controlResponse, error) {
	dir, err := instanceDir(port)
	if err != nil {
		return nil, err
	}
	held, err := lockHeld(filepath.Join(dir, "instance.lock"))
	if err != nil {
		return nil, err
	}
	if !held {
		return nil, ErrNotRunning
	}
	data, err := os.ReadFile(filepath.Join(dir, discoveryFile))
	if err != nil {
		return nil, fmt.Errorf("daemon state unreadable, try again: %w", err)
	}
	var d discovery
	if err := json.Unmarshal(data, &d); err != nil || d.V != protocolVersion || d.Port != port {
		return nil, fmt.Errorf("daemon state unreadable, try again")
	}
	address, err := loopbackAddress(d.ControlAddr)
	if err != nil {
		return nil, err
	}
	conn, err := net.DialTimeout("tcp", address, 3*time.Second)
	if err != nil {
		return nil, fmt.Errorf("daemon control unreachable: %w", err)
	}
	defer conn.Close()
	jc := newJSONConn(conn)
	if err := jc.write(controlRequest{V: protocolVersion, Token: d.Token, Command: command}); err != nil {
		return nil, fmt.Errorf("daemon control unreachable: %w", err)
	}
	timeout := 5 * time.Second
	if command == "stop" {
		timeout = 30 * time.Second
	}
	var resp controlResponse
	if err := jc.read(&resp, timeout); err != nil {
		return nil, fmt.Errorf("daemon control unreachable: %w", err)
	}
	if !resp.OK {
		return nil, errors.New(resp.Error)
	}
	return &resp, nil
}

func loopbackAddress(value string) (string, error) {
	host, port, err := net.SplitHostPort(value)
	if err != nil {
		return "", fmt.Errorf("invalid control address %q", value)
	}
	if ip := net.ParseIP(host); ip != nil {
		if !ip.IsLoopback() {
			return "", fmt.Errorf("control address %q is not loopback", value)
		}
		return net.JoinHostPort(ip.String(), port), nil
	}
	if strings.EqualFold(host, "localhost") {
		return net.JoinHostPort(host, port), nil
	}
	return "", fmt.Errorf("control address %q is not loopback", value)
}

// awaitHandshake accepts connections until an authenticated supervisor
// handshake arrives, the supervisor exits, or the timeout expires.
func awaitHandshake(listener net.Listener, token string, timeout time.Duration, procDone <-chan error) (*handshakeMsg, error) {
	type result struct {
		msg *handshakeMsg
		err error
	}
	ch := make(chan result, 1)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				ch <- result{nil, err}
				return
			}
			jc := newJSONConn(conn)
			var msg handshakeMsg
			err = jc.read(&msg, timeout)
			if err == nil && (msg.V != protocolVersion || msg.Token != token) {
				err = errors.New("supervisor handshake failed authentication")
			}
			if err != nil {
				conn.Close()
				continue
			}
			ch <- result{&msg, nil}
			return
		}
	}()
	handle := func(r result) (*handshakeMsg, error) {
		if r.err != nil {
			return nil, r.err
		}
		switch r.msg.Event {
		case "ready":
			return r.msg, nil
		case "error":
			return nil, fmt.Errorf("daemon failed to start: %s", r.msg.Message)
		default:
			return nil, fmt.Errorf("unexpected handshake event %q", r.msg.Event)
		}
	}
	select {
	case r := <-ch:
		return handle(r)
	case err := <-procDone:
		// Prefer an in-flight handshake message over the bare exit reason.
		select {
		case r := <-ch:
			return handle(r)
		default:
			return nil, fmt.Errorf("supervisor exited during startup: %w", err)
		}
	case <-time.After(timeout):
		return nil, errors.New("timed out waiting for the daemon to start")
	}
}

// cacheBase resolves the per-user cache root; the internal override exists so
// tests can isolate instance state, since UserCacheDir ignores XDG on darwin.
func cacheBase() (string, error) {
	if base := os.Getenv(cacheDirEnv); base != "" {
		return base, nil
	}
	return os.UserCacheDir()
}

func instanceDir(port int) (string, error) {
	base, err := cacheBase()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(base, "servd", strconv.Itoa(port))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	info, err := os.Stat(dir)
	if err != nil {
		return "", err
	}
	if info.Mode().Perm()&0o077 != 0 {
		if err := os.Chmod(dir, 0o700); err != nil {
			return "", fmt.Errorf("insecure instance directory %s: %w", dir, err)
		}
	}
	return dir, nil
}

func lockHeld(path string) (bool, error) {
	lock, err := acquireLock(path)
	if err != nil {
		if errors.Is(err, errLocked) {
			return true, nil
		}
		return false, err
	}
	return false, lock.Close()
}

// copySelf stores a private generation copy of the running executable so
// restarts survive `go run` cleanup and npm reinstalls.
func copySelf(dir string) (string, error) {
	source, err := os.Executable()
	if err != nil {
		return "", err
	}
	generation, err := randomToken()
	if err != nil {
		return "", err
	}
	binDir := filepath.Join(dir, "bin-"+generation[:12])
	if err := os.Mkdir(binDir, 0o700); err != nil {
		return "", err
	}
	name := "servd"
	if runtime.GOOS == "windows" {
		name = "servd.exe"
	}
	target := filepath.Join(binDir, name)
	in, err := os.Open(source)
	if err != nil {
		return "", err
	}
	defer in.Close()
	out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o700)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return "", err
	}
	if err := out.Close(); err != nil {
		return "", err
	}
	return target, os.Chmod(target, 0o700)
}

// cleanBinaries removes copies from previous generations. A copy still held
// by a running process fails to delete on Windows and is retried next start.
func cleanBinaries(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "bin-") {
			_ = os.RemoveAll(filepath.Join(dir, entry.Name()))
		}
	}
}

func randomToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// filteredEnv drops internal role variables so children never inherit a
// stale role; the policy override is kept for supervisor tuning and tests.
func filteredEnv() []string {
	drop := map[string]bool{
		roleEnv: true, configEnv: true, handshakeAddrEnv: true, handshakeTokenEnv: true,
		instanceDirEnv: true, manageAddrEnv: true, manageTokenEnv: true,
	}
	env := os.Environ()
	kept := env[:0]
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		if drop[key] {
			continue
		}
		kept = append(kept, entry)
	}
	return kept
}

type wireProxy struct {
	Mount  string `json:"mount"`
	Target string `json:"target"`
}

type wireConfig struct {
	Port        int                `json:"port"`
	Directories []config.Directory `json:"directories,omitempty"`
	Proxies     []wireProxy        `json:"proxies,omitempty"`
	WebSockets  []wireProxy        `json:"webSockets,omitempty"`
	Hosts       []string           `json:"hosts,omitempty"`
	Origins     []string           `json:"origins,omitempty"`
	TLSCert     string             `json:"tlsCert,omitempty"`
	TLSKey      string             `json:"tlsKey,omitempty"`
}

func marshalConfig(cfg config.Config) ([]byte, error) {
	wire := wireConfig{
		Port: cfg.Port, Directories: cfg.Directories, Hosts: cfg.Hosts, Origins: cfg.Origins,
		TLSCert: cfg.TLSCert, TLSKey: cfg.TLSKey,
	}
	for _, p := range cfg.Proxies {
		wire.Proxies = append(wire.Proxies, wireProxy{Mount: p.Mount, Target: p.Target.String()})
	}
	for _, p := range cfg.WebSockets {
		wire.WebSockets = append(wire.WebSockets, wireProxy{Mount: p.Mount, Target: p.Target.String()})
	}
	return json.Marshal(wire)
}

func unmarshalConfig(data []byte) (config.Config, error) {
	var wire wireConfig
	if err := json.Unmarshal(data, &wire); err != nil {
		return config.Config{}, err
	}
	cfg := config.Config{
		Port: wire.Port, Directories: wire.Directories, Hosts: wire.Hosts, Origins: wire.Origins,
		TLSCert: wire.TLSCert, TLSKey: wire.TLSKey,
	}
	for _, list := range []struct {
		source []wireProxy
		dest   *[]config.Proxy
	}{{wire.Proxies, &cfg.Proxies}, {wire.WebSockets, &cfg.WebSockets}} {
		for _, p := range list.source {
			target, err := url.Parse(p.Target)
			if err != nil {
				return config.Config{}, err
			}
			*list.dest = append(*list.dest, config.Proxy{Mount: p.Mount, Target: target})
		}
	}
	return cfg, nil
}

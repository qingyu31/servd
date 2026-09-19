package daemon

import (
	"bufio"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"go.qingyu31.com/servd/internal/app"
	"go.qingyu31.com/servd/internal/config"
)

// wirePolicy carries test-tunable restart timings through the environment so
// tests can shrink the backoff and stability windows without mocking clocks.
type wirePolicy struct {
	MaxRestarts   int   `json:"maxRestarts"`
	BackoffBaseMs int64 `json:"backoffBaseMs"`
	StableResetMs int64 `json:"stableResetMs"`
	StopTimeoutMs int64 `json:"stopTimeoutMs"`
}

type policy struct {
	maxRestarts int
	backoffBase time.Duration
	stableReset time.Duration
	stopTimeout time.Duration
}

func loadPolicy() policy {
	p := policy{
		maxRestarts: 5, backoffBase: time.Second,
		stableReset: 60 * time.Second, stopTimeout: 10 * time.Second,
	}
	if raw := os.Getenv(policyEnv); raw != "" {
		var w wirePolicy
		if json.Unmarshal([]byte(raw), &w) == nil {
			if w.MaxRestarts > 0 {
				p.maxRestarts = w.MaxRestarts
			}
			if w.BackoffBaseMs > 0 {
				p.backoffBase = time.Duration(w.BackoffBaseMs) * time.Millisecond
			}
			if w.StableResetMs > 0 {
				p.stableReset = time.Duration(w.StableResetMs) * time.Millisecond
			}
			if w.StopTimeoutMs > 0 {
				p.stopTimeout = time.Duration(w.StopTimeoutMs) * time.Millisecond
			}
		}
	}
	return p
}

func (p policy) backoff(restart int) time.Duration {
	shift := restart - 1
	if shift > 16 {
		shift = 16
	}
	return p.backoffBase << shift
}

// mgmtConn is a worker management connection whose hello was authenticated;
// it is adopted by the generation that knows its token.
type mgmtConn struct {
	token string
	jc    *jsonConn
}

type workerProc struct {
	cmd    *exec.Cmd
	token  string
	conn   *jsonConn
	ready  chan struct{}
	failed chan string
	exit   chan string
}

type supervisor struct {
	cfg            config.Config
	wire           string
	pol            policy
	log            *logWriter
	logPath        string
	binPath        string
	controlAddr    string
	controlToken   string
	manageAddr     string
	handshakeAddr  string
	handshakeToken string

	mgmtHelloCh   chan mgmtConn
	stopRequested chan struct{}
	stopOnce      sync.Once
	stoppedCh     chan struct{}

	mu          sync.Mutex
	state       string
	restarts    int
	lastExit    string
	nextRestart time.Time
	workerPid   int
}

// runSupervisor executes the detached supervisor role: it holds the instance
// lock, serves the control endpoint, and keeps a worker running with the
// restart policy until stopped.
func runSupervisor(dir string) int {
	wire := os.Getenv(configEnv)
	cfg, err := unmarshalConfig([]byte(wire))
	if err != nil || cfg.Port < 1 || cfg.Port > 65535 {
		fmt.Fprintln(os.Stderr, "servd: invalid internal config")
		return 2
	}
	pol := loadPolicy()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		fmt.Fprintln(os.Stderr, "servd:", err)
		return 1
	}
	logPath := filepath.Join(dir, logFile)
	logw, err := openLog(logPath, 0)
	if err != nil {
		fmt.Fprintln(os.Stderr, "servd:", err)
		return 1
	}
	defer logw.Close()
	binPath, err := os.Executable()
	if err != nil {
		logw.logf("supervisor: locate executable: %v", err)
		return 1
	}
	lock, err := acquireLock(filepath.Join(dir, "instance.lock"))
	if err != nil {
		logw.logf("supervisor: instance lock: %v", err)
		return 1
	}
	controlLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		lock.Close()
		logw.logf("supervisor: control listen: %v", err)
		return 1
	}
	defer controlLn.Close()
	controlToken, err := randomToken()
	if err != nil {
		lock.Close()
		logw.logf("supervisor: %v", err)
		return 1
	}
	manageLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		lock.Close()
		logw.logf("supervisor: manage listen: %v", err)
		return 1
	}
	defer manageLn.Close()
	if err := writeDiscovery(dir, controlLn.Addr().String(), controlToken, cfg.Port); err != nil {
		lock.Close()
		logw.logf("supervisor: publish control state: %v", err)
		return 1
	}

	s := &supervisor{
		cfg: cfg, wire: wire, pol: pol, log: logw, logPath: logPath,
		binPath:        binPath,
		controlAddr:    controlLn.Addr().String(),
		controlToken:   controlToken,
		manageAddr:     manageLn.Addr().String(),
		handshakeAddr:  os.Getenv(handshakeAddrEnv),
		handshakeToken: os.Getenv(handshakeTokenEnv),
		mgmtHelloCh:    make(chan mgmtConn, 4),
		stopRequested:  make(chan struct{}),
		stoppedCh:      make(chan struct{}),
		state:          "starting",
	}
	logw.logf("supervisor: started (pid %d) for port %d", os.Getpid(), cfg.Port)

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	go func() {
		for range signals {
			s.requestStop()
		}
	}()

	go s.serveControl(controlLn)
	go s.serveManage(manageLn)

	code := s.loop()
	// Release the lock before reporting completion so an immediate restart
	// never collides with the outgoing instance.
	lock.Close()
	close(s.stoppedCh)
	_ = os.Remove(filepath.Join(dir, discoveryFile))
	s.logf("supervisor: stopped (exit %d)", code)
	return code
}

// loop runs the restart state machine: starting -> running -> backoff or
// failed, and stopping on request. A first start that never becomes ready is
// reported as an error without any restart attempt.
func (s *supervisor) loop() int {
	firstGeneration := true
	for {
		s.setState("starting")
		wp := s.spawn()
		var (
			ready      bool
			readyAt    time.Time
			failMsg    string
			exitReason string
		)
	wait:
		for {
			select {
			case mc := <-s.mgmtHelloCh:
				s.adopt(wp, mc)
			case <-wp.ready:
				ready = true
				readyAt = time.Now()
				pid := 0
				if wp.cmd != nil && wp.cmd.Process != nil {
					pid = wp.cmd.Process.Pid
				}
				s.workerRunning(pid)
				if firstGeneration {
					if err := s.sendHandshake(handshakeMsg{
						Event:         "ready",
						ControlAddr:   s.controlAddr,
						ControlToken:  s.controlToken,
						SupervisorPid: os.Getpid(),
						WorkerPid:     pid,
						LogPath:       s.logPath,
					}); err != nil {
						s.logf("starter handoff failed: %v; recycling instance", err)
						s.stopWorker(wp)
						return 1
					}
					firstGeneration = false
					s.logf("handoff complete; worker pid %d listening on port %d", pid, s.cfg.Port)
				}
			case msg := <-wp.failed:
				failMsg = msg
			case reason := <-wp.exit:
				exitReason = reason
				break wait
			case <-s.stopRequested:
				s.stopWorker(wp)
				return 0
			}
		}
		s.workerExited()
		reason := exitReason
		if failMsg != "" {
			reason = failMsg + " (" + exitReason + ")"
		}
		if firstGeneration {
			s.logf("first start failed: %s", reason)
			if err := s.sendHandshake(handshakeMsg{Event: "error", Message: reason}); err != nil {
				s.logf("report first start failure: %v", err)
			}
			return 1
		}
		s.setLastExit(reason)
		s.logf("worker exited: %s", reason)
		if ready && time.Since(readyAt) >= s.pol.stableReset {
			s.resetRestarts()
			s.logf("worker ran stably for %s; restart budget reset", s.pol.stableReset)
		}
		if s.restartCount() >= s.pol.maxRestarts {
			s.setState("failed")
			s.logf("restart budget exhausted; holding for --stop")
			for {
				select {
				case mc := <-s.mgmtHelloCh:
					mc.jc.close()
				case <-s.stopRequested:
					return 0
				}
			}
		}
		n := s.bumpRestarts()
		delay := s.pol.backoff(n)
		s.setBackoff(time.Now().Add(delay))
		s.logf("restarting worker in %s (restart %d/%d)", delay, n, s.pol.maxRestarts)
		timer := time.NewTimer(delay)
	backoff:
		for {
			select {
			case <-timer.C:
				break backoff
			case mc := <-s.mgmtHelloCh:
				mc.jc.close()
			case <-s.stopRequested:
				timer.Stop()
				return 0
			}
		}
	}
}

// spawn starts one worker generation. Every failure path, including a failed
// exec, eventually reports through wp.exit.
func (s *supervisor) spawn() *workerProc {
	token, err := randomToken()
	wp := &workerProc{
		token:  token,
		ready:  make(chan struct{}, 1),
		failed: make(chan string, 1),
		exit:   make(chan string, 1),
	}
	if err != nil {
		wp.exit <- "generate worker token: " + err.Error()
		return wp
	}
	cmd := exec.Command(s.binPath)
	cmd.Env = append(filteredEnv(),
		roleEnv+"="+roleWorker,
		configEnv+"="+s.wire,
		manageAddrEnv+"="+s.manageAddr,
		manageTokenEnv+"="+token,
	)
	pipeRead, pipeWrite, err := os.Pipe()
	if err != nil {
		wp.exit <- "create log pipe: " + err.Error()
		return wp
	}
	cmd.Stdout = pipeWrite
	cmd.Stderr = pipeWrite
	if err := cmd.Start(); err != nil {
		pipeWrite.Close()
		pipeRead.Close()
		wp.exit <- "spawn worker: " + err.Error()
		return wp
	}
	pipeWrite.Close()
	wp.cmd = cmd
	logsDone := make(chan struct{})
	go s.pipeToLog(pipeRead, logsDone)
	go func() {
		waitErr := cmd.Wait()
		<-logsDone
		wp.exit <- describeExit(waitErr)
	}()
	return wp
}

func (s *supervisor) pipeToLog(r io.ReadCloser, done chan<- struct{}) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		s.log.logf("worker: %s", scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		s.log.logf("worker: dropped oversized log line: %v", err)
		_, _ = io.Copy(io.Discard, r)
	}
	r.Close()
	close(done)
}

// adopt attaches an authenticated management connection to the current
// generation; connections from any other generation are closed.
func (s *supervisor) adopt(wp *workerProc, mc mgmtConn) {
	if mc.token != wp.token {
		mc.jc.close()
		return
	}
	wp.conn = mc.jc
	go s.readMgmt(wp, mc.jc)
}

func (s *supervisor) readMgmt(wp *workerProc, jc *jsonConn) {
	for {
		var msg mgmtMsg
		if err := jc.read(&msg, 0); err != nil {
			return
		}
		switch msg.Event {
		case "ready":
			select {
			case wp.ready <- struct{}{}:
			default:
			}
		case "failed":
			select {
			case wp.failed <- msg.Message:
			default:
			}
		}
	}
}

// stopWorker asks the worker to exit through its management connection and
// falls back to killing the process it owns once the bounded timeout expires.
func (s *supervisor) stopWorker(wp *workerProc) {
	s.logf("stopping worker")
	if wp.conn != nil {
		if err := wp.conn.write(mgmtMsg{V: protocolVersion, Command: "stop"}); err != nil {
			s.logf("deliver stop command: %v", err)
		}
	}
	timeout := time.After(s.pol.stopTimeout)
	for {
		select {
		case mc := <-s.mgmtHelloCh:
			s.adopt(wp, mc)
			if wp.conn != nil {
				if err := wp.conn.write(mgmtMsg{V: protocolVersion, Command: "stop"}); err != nil {
					s.logf("deliver stop command: %v", err)
				}
			}
		case <-wp.exit:
			s.logf("worker stopped")
			return
		case <-timeout:
			if wp.cmd != nil && wp.cmd.Process != nil {
				s.logf("worker did not stop within %s; killing", s.pol.stopTimeout)
				_ = wp.cmd.Process.Kill()
			}
			select {
			case <-wp.exit:
			case <-time.After(5 * time.Second):
				s.logf("worker process did not exit after kill")
			}
			return
		}
	}
}

func (s *supervisor) serveControl(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go s.handleControl(conn)
	}
}

func (s *supervisor) handleControl(conn net.Conn) {
	defer conn.Close()
	jc := newJSONConn(conn)
	var req controlRequest
	if err := jc.read(&req, 10*time.Second); err != nil {
		return
	}
	if req.V != protocolVersion || subtle.ConstantTimeCompare([]byte(req.Token), []byte(s.controlToken)) != 1 {
		_ = jc.write(controlResponse{Error: "unauthorized"})
		return
	}
	switch req.Command {
	case "status":
		resp := s.snapshot()
		resp.OK = true
		_ = jc.write(resp)
	case "stop":
		s.requestStop()
		select {
		case <-s.stoppedCh:
			_ = jc.write(controlResponse{OK: true, Status: "stopped", Port: s.cfg.Port})
		case <-time.After(s.pol.stopTimeout + 10*time.Second):
			_ = jc.write(controlResponse{Error: "stop timed out", Status: "stopping", Port: s.cfg.Port})
		}
	default:
		_ = jc.write(controlResponse{Error: "unknown command"})
	}
}

func (s *supervisor) serveManage(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go func(conn net.Conn) {
			jc := newJSONConn(conn)
			var msg mgmtMsg
			if err := jc.read(&msg, 15*time.Second); err != nil {
				jc.close()
				return
			}
			if msg.V != protocolVersion || msg.Event != "hello" || msg.Token == "" {
				jc.close()
				return
			}
			select {
			case s.mgmtHelloCh <- mgmtConn{token: msg.Token, jc: jc}:
			case <-time.After(15 * time.Second):
				jc.close()
			}
		}(conn)
	}
}

func (s *supervisor) sendHandshake(msg handshakeMsg) error {
	if s.handshakeAddr == "" {
		return errors.New("no starter address")
	}
	conn, err := net.DialTimeout("tcp", s.handshakeAddr, 5*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	msg.V = protocolVersion
	msg.Token = s.handshakeToken
	return newJSONConn(conn).write(msg)
}

func (s *supervisor) requestStop() {
	s.stopOnce.Do(func() {
		s.setState("stopping")
		close(s.stopRequested)
	})
}

func (s *supervisor) snapshot() controlResponse {
	s.mu.Lock()
	defer s.mu.Unlock()
	resp := controlResponse{
		Status:        s.state,
		Port:          s.cfg.Port,
		SupervisorPid: os.Getpid(),
		WorkerPid:     s.workerPid,
		Restarts:      s.restarts,
		MaxRestarts:   s.pol.maxRestarts,
		LastExit:      s.lastExit,
		LogPath:       s.logPath,
	}
	if s.state == "backoff" && !s.nextRestart.IsZero() {
		if ms := time.Until(s.nextRestart).Milliseconds(); ms > 0 {
			resp.NextRestartMs = ms
		}
	}
	return resp
}

func (s *supervisor) setState(state string) {
	s.mu.Lock()
	s.state = state
	s.mu.Unlock()
}

func (s *supervisor) workerRunning(pid int) {
	s.mu.Lock()
	s.state = "running"
	s.workerPid = pid
	s.mu.Unlock()
}

func (s *supervisor) workerExited() {
	s.mu.Lock()
	s.workerPid = 0
	s.mu.Unlock()
}

func (s *supervisor) setLastExit(reason string) {
	s.mu.Lock()
	s.lastExit = reason
	s.mu.Unlock()
}

func (s *supervisor) restartCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.restarts
}

func (s *supervisor) bumpRestarts() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.restarts++
	return s.restarts
}

func (s *supervisor) resetRestarts() {
	s.mu.Lock()
	s.restarts = 0
	s.mu.Unlock()
}

func (s *supervisor) setBackoff(next time.Time) {
	s.mu.Lock()
	s.state = "backoff"
	s.nextRestart = next
	s.mu.Unlock()
}

func (s *supervisor) logf(format string, args ...any) {
	s.log.logf("supervisor: "+format, args...)
}

func writeDiscovery(dir, addr, token string, port int) error {
	data, err := json.Marshal(discovery{V: protocolVersion, ControlAddr: addr, Token: token, Port: port})
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, discoveryFile+".tmp")
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, discoveryFile))
}

func describeExit(err error) string {
	if err == nil {
		return "exit 0"
	}
	return err.Error()
}

// runWorker executes the worker role: it serves the configured routes until
// the supervisor orders a stop or its management connection is lost.
func runWorker(stdout io.Writer) int {
	cfg, err := unmarshalConfig([]byte(os.Getenv(configEnv)))
	if err != nil {
		fmt.Fprintln(os.Stderr, "servd: internal config:", err)
		return 2
	}
	jc, err := dialManage(os.Getenv(manageAddrEnv))
	if err != nil {
		fmt.Fprintln(os.Stderr, "servd: reach supervisor:", err)
		return 2
	}
	defer jc.close()
	if err := jc.write(mgmtMsg{V: protocolVersion, Token: os.Getenv(manageTokenEnv), Event: "hello"}); err != nil {
		fmt.Fprintln(os.Stderr, "servd: register with supervisor:", err)
		return 2
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		defer cancel()
		for {
			var msg mgmtMsg
			if err := jc.read(&msg, 0); err != nil {
				return
			}
			if msg.Command == "stop" {
				return
			}
		}
	}()
	if err := app.Run(ctx, cfg, stdout, os.Stderr, func() {
		_ = jc.write(mgmtMsg{V: protocolVersion, Event: "ready", Port: cfg.Port})
	}); err != nil {
		_ = jc.write(mgmtMsg{V: protocolVersion, Event: "failed", Message: err.Error()})
		fmt.Fprintln(os.Stderr, "servd:", err)
		return 1
	}
	return 0
}

func dialManage(addr string) (*jsonConn, error) {
	var lastErr error
	for i := 0; i < 25; i++ {
		conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err == nil {
			return newJSONConn(conn), nil
		}
		lastErr = err
		time.Sleep(200 * time.Millisecond)
	}
	return nil, lastErr
}

package main

// End-to-end integration test: the whole binary, no test harness inside
// the daemon. The test binary doubles as the clow binary (the helper
// process trick: a CLOWDER_HELPER=1 re-exec of os.Args[0] runs the
// real command and exits), so this file needs no bash glue and no
// in-process daemon wiring: every step is the real CLI driving a real
// daemon process over its IPC socket, with real pairing over the real
// DERP network.
//
// Gated like the daemon package's integration tests, because it needs
// outbound access to the DERP relays; the CI integration job runs it:
//
//	CLOWDER_INTEGRATION=1 go test . -run TestIntegrationEndToEnd -v -count=1

import (
	"bytes"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// TestMain is the helper-process gate (see the file comment) and keeps
// t.TempDir() paths short: config dirs host the daemon's unix IPC
// socket, and macOS rejects socket paths over ~103 bytes.
func TestMain(m *testing.M) {
	if os.Getenv("CLOWDER_HELPER") == "1" {
		if err := run(os.Args[1:]); err != nil {
			fmt.Fprintf(os.Stderr, "clow: %v\n", err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	if root, err := os.MkdirTemp("/tmp", "clowder-test-"); err == nil {
		_ = os.Setenv("TMPDIR", root)
		code := m.Run()
		_ = os.RemoveAll(root)
		os.Exit(code)
	}
	os.Exit(m.Run())
}

// cat is one real cat: an initialized config dir plus a daemon process
// running from it, with the daemon's logs kept for failure reports.
type cat struct {
	t      *testing.T
	name   string
	dir    string
	inbox  string
	env    map[string]string
	health string
	logs   bytes.Buffer
	mu     sync.Mutex
	cmd    *exec.Cmd
}

// newCat initializes a cat (identity, inbox) without starting it.
func newCat(t *testing.T, name string, env map[string]string) *cat {
	t.Helper()
	dir := t.TempDir()
	c := &cat{
		t:     t,
		name:  name,
		dir:   dir,
		inbox: filepath.Join(dir, "inbox"),
		env:   env,
	}
	c.clow("init", "--name", name, "--inbox", c.inbox)
	return c
}

// start runs the cat's daemon as a real subprocess and returns once
// its health endpoint answers, so the test never sleeps on startup.
func (c *cat) start() {
	c.t.Helper()
	port := freePort(c.t)
	c.health = port
	cmd := exec.Command(os.Args[0], "daemon", "--health", port)
	cmd.Env = c.envWith(map[string]string{"CLOWDER_DIR": c.dir, "CLOWDER_HELPER": "1"})
	c.mu.Lock()
	c.cmd = cmd
	cmd.Stdout = &c.logs
	cmd.Stderr = &c.logs
	c.mu.Unlock()
	if err := cmd.Start(); err != nil {
		c.t.Fatalf("cat %s: starting daemon: %v", c.name, err)
	}
	c.t.Cleanup(func() { c.stop() })
	c.waitHealthy()
}

// stop terminates the daemon process, waiting for a graceful exit.
// Once the test has failed, the daemon gets a SIGQUIT first: the Go
// runtime dumps every goroutine's stack into the logs, which makes a
// wedged daemon post-mortem readable straight from the test output.
func (c *cat) stop() {
	c.mu.Lock()
	cmd := c.cmd
	c.cmd = nil
	c.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	if c.t.Failed() {
		_ = cmd.Process.Signal(syscall.SIGQUIT)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		<-done
	}
	if c.t.Failed() {
		c.mu.Lock()
		logs := c.logs.String()
		c.mu.Unlock()
		c.t.Logf("cat %s daemon logs:\n%s", c.name, tail(logs, 60000))
	}
}

// clow runs one real CLI command against this cat's daemon and
// returns its stdout.
func (c *cat) clow(args ...string) string {
	c.t.Helper()
	return clow(c.t, c.envWith(map[string]string{"CLOWDER_DIR": c.dir}), args...)
}

// envWith layers overrides onto this cat's fixed environment.
func (c *cat) envWith(extra map[string]string) []string {
	env := map[string]string{}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env[k] = v
		}
	}
	for k, v := range c.env {
		env[k] = v
	}
	for k, v := range extra {
		env[k] = v
	}
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}

// clow runs the clow binary as a helper subprocess with the given
// environment and args, failing the test on a non-zero exit.
func clow(t *testing.T, env []string, args ...string) string {
	t.Helper()
	cmd := exec.Command(os.Args[0], args...)
	cmd.Env = append(env, "CLOWDER_HELPER=1")
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		t.Fatalf("clow %v: %v\nstdout:\n%s\nstderr:\n%s", args, err, out.String(), errBuf.String())
	}
	return out.String()
}

// invite returns the pairing code from `clow invite` output
// ("ask the other cat to run: clow join <words-region>").
func (c *cat) invite() string {
	c.t.Helper()
	out := c.clow("invite")
	i := strings.Index(out, "clow join ")
	if i < 0 {
		c.t.Fatalf("cat %s: invite output has no code: %q", c.name, out)
	}
	return strings.TrimSpace(out[i+len("clow join "):])
}

func (c *cat) waitHealthy() {
	c.t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get("http://" + c.health + "/healthz")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	c.t.Fatalf("cat %s: daemon never became healthy on %s", c.name, c.health)
}

// TestIntegrationEndToEnd is the full user story against real daemon
// processes and the real DERP network: three cats pair with real
// pairing codes, a file is sent directly, then a second file rides a
// storer while the target is offline, and the target pulls it with
// `clow fetch` after coming back. Asserts the files' contents, not
// internal state.
func TestIntegrationEndToEnd(t *testing.T) {
	if os.Getenv("CLOWDER_INTEGRATION") != "1" {
		t.Skip("set CLOWDER_INTEGRATION=1 (needs outbound DERP network)")
	}

	milo := newCat(t, "milo", nil)
	puma := newCat(t, "puma", nil)
	box := newCat(t, "box", map[string]string{
		"CLOWDER_STORER": "on",
		"CLOWDER_MAX":    "10M",
	})
	milo.start()
	puma.start()
	box.start()

	// Pair milo with both cats, using only the printed pairing codes.
	codePuma := milo.invite()
	puma.clow("join", codePuma)
	codeBox := milo.invite()
	box.clow("join", codeBox)
	waitFor(t, func() bool {
		return strings.Contains(puma.clow("cats"), "milo") &&
			strings.Contains(milo.clow("cats"), "puma") &&
			strings.Contains(milo.clow("cats"), "box")
	}, "pairing to show up in `clow cats` on both sides")

	// A direct send lands in the target's inbox with its content
	// intact.
	src := writeFile(t, "nap.txt", 128*1024)
	out := milo.clow("send", "puma", src)
	if !strings.Contains(out, "queued nap.txt for puma") {
		t.Fatalf("send output: %q", out)
	}
	waitForFile(t, puma, "nap.txt", src)

	// Now the target goes offline and a second file must ride the
	// storer: it is queued direct-failed and deposited at box.
	puma.stop()
	src2 := writeFile(t, "nap2.txt", 128*1024)
	milo.clow("send", "puma", src2)
	waitFor(t, func() bool {
		des, err := os.ReadDir(filepath.Join(box.dir, "spool"))
		return err == nil && len(des) > 0
	}, "the storer to hold the offline cat's file")

	// Back online, the target pulls the held file with one fetch.
	puma.start()
	puma.clow("fetch")
	waitForFile(t, puma, "nap2.txt", src2)

	// The sender's outbox drained: status reports nothing pending.
	if out := milo.clow("status"); !strings.Contains(out, "outbox: 0 pending") {
		t.Fatalf("sends still pending after delivery:\n%s", out)
	}
}

// ---- helpers ----

func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a port: %v", err)
	}
	defer func() { _ = ln.Close() }()
	return ln.Addr().String()
}

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// waitForFile waits until the cat's inbox holds name with exactly the
// source file's content.
func waitForFile(t *testing.T, c *cat, name, srcPath string) {
	t.Helper()
	want, err := os.ReadFile(srcPath)
	if err != nil {
		t.Fatalf("reading source: %v", err)
	}
	waitFor(t, func() bool {
		got, err := os.ReadFile(filepath.Join(c.inbox, name))
		return err == nil && bytes.Equal(got, want)
	}, fmt.Sprintf("%s to receive %s", c.name, name))
}

func writeFile(t *testing.T, name string, size int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	content := bytes.Repeat([]byte("clowder end-to-end test file\n"), size/28+1)
	if err := os.WriteFile(path, content[:size], 0o600); err != nil {
		t.Fatalf("writing %s: %v", name, err)
	}
	return path
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"clowder/roster"
)

// Request is one command from the clow CLI to the daemon. Ops: send,
// cats, storer, status, setinbox, invite, join, outbox.
type Request struct {
	Op      string `json:"op"`
	Target  string `json:"target,omitempty"`
	Path    string `json:"path,omitempty"`
	On      bool   `json:"on,omitempty"`
	Dropbox bool   `json:"dropbox,omitempty"`
	Max     string `json:"max,omitempty"`   // spool capacity for the storer role (e.g. "10G")
	Words   string `json:"words,omitempty"` // pairing code for join
}

// Response is the daemon's reply.
type Response struct {
	OK         bool         `json:"ok"`
	Error      string       `json:"error,omitempty"`
	Message    string       `json:"message,omitempty"`
	Me         *roster.Cat  `json:"me,omitempty"`
	Cats       []roster.Cat `json:"cats,omitempty"`
	Outbox     []Entry      `json:"outbox,omitempty"`
	Spool      int          `json:"spool,omitempty"`
	SpoolBytes int64        `json:"spool_bytes,omitempty"`
	Stats      *Stats       `json:"stats,omitempty"`
	// Liveness maps cat keys to the unix time they were last seen on a
	// successful connection (status op).
	Liveness map[string]int64 `json:"liveness,omitempty"`
	// Progress lists the in-flight transfers (status op).
	Progress []Progress `json:"progress,omitempty"`
	// Distrusted names the roster cats whose keys are on the local
	// blocklist (distrust, and forgotten cats), for cats/status tags.
	Distrusted []string `json:"distrusted,omitempty"`
	// Receipts are the most recent confirmed deliveries, newest first
	// (status op).
	Receipts []Receipt `json:"receipts,omitempty"`
	// Paths maps cat keys to their probed route (status op); cats
	// that did not answer the probe have no entry.
	Paths map[string]*PathInfo `json:"paths,omitempty"`
}

func fail(err error) Response { return Response{OK: false, Error: err.Error()} }
func okMsg(s string) Response { return Response{OK: true, Message: s} }

// shortAddr elides the middle of a long tailcat address for display.
func shortAddr(a string) string {
	if len(a) <= 24 {
		return a
	}
	return a[:12] + "..." + a[len(a)-9:]
}

// IPCPath returns the daemon's IPC socket path for a config dir.
func IPCPath(dir string) string { return filepath.Join(dir, "clow.sock") }

// listenIPC listens on a unix socket (0600: any local process could
// otherwise drive the daemon), or TCP if path contains a colon (tests).
func listenIPC(path string) (net.Listener, error) {
	if strings.Contains(path, ":") {
		return net.Listen("tcp", path)
	}
	_ = os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		return nil, err
	}
	return ln, nil
}

func (d *Daemon) serveIPCConn(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Minute))
	var req Request
	if err := json.NewDecoder(conn).Decode(&req); err != nil {
		return
	}
	resp := d.handleIPC(req)
	if err := json.NewEncoder(conn).Encode(resp); err != nil {
		d.cfg.logf("clowder: writing IPC response: %v", err)
	}
}

func (d *Daemon) handleIPC(req Request) Response {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	switch req.Op {
	case "send":
		id, err := d.Send(req.Target, req.Path)
		if err != nil {
			return fail(err)
		}
		return okMsg(fmt.Sprintf("queued %s for %s (id %s)", filepath.Base(req.Path), req.Target, id))

	case "cats":
		me := d.Me()
		return Response{OK: true, Me: &me, Cats: d.ros.All(), Distrusted: d.distrustedNames()}

	case "storer":
		capacity, err := ParseSize(req.Max)
		if req.Max == "" {
			capacity = 0
		} else if err != nil {
			return fail(err)
		}
		switch {
		case req.Dropbox && req.On:
			err = d.SetDropbox(true, capacity)
		case req.On:
			err = d.SetStorer(true, capacity)
		default:
			err = d.SetStorer(false, 0)
		}
		if err != nil {
			return fail(err)
		}
		switch {
		case d.Me().Dropbox:
			return okMsg("dropbox role on (storer, third-party only)")
		case d.Me().Storer:
			return okMsg("storer role on")
		default:
			return okMsg("storer role off")
		}

	case "setinbox":
		if req.Path == "" {
			return fail(fmt.Errorf("setinbox needs a path"))
		}
		if err := d.SetInbox(req.Path); err != nil {
			return fail(err)
		}
		return okMsg(fmt.Sprintf("inbox now %s", d.InboxDir()))

	case "rotate":
		newAddr, err := d.RotateAddress(ctx)
		if err != nil {
			return fail(err)
		}
		return okMsg(fmt.Sprintf("address rotated to %s; restart the daemon to use it", shortAddr(newAddr)))

	case "leave":
		n, err := d.Leave(ctx)
		if err != nil {
			return fail(err)
		}
		return okMsg(fmt.Sprintf("left the clowder (told %d cat(s)); your identity is kept — pair again with clow invite or clow join", n))

	case "distrust":
		if req.Target == "" {
			return fail(fmt.Errorf("distrust needs a cat name"))
		}
		if _, err := d.Distrust(req.Target); err != nil {
			return fail(err)
		}
		return okMsg(fmt.Sprintf("distrusted %s (local only; roster entry kept; undo with: clow trust %s)", req.Target, req.Target))

	case "trust":
		if req.Target == "" {
			return fail(fmt.Errorf("trust needs a cat name"))
		}
		if _, err := d.Trust(req.Target); err != nil {
			return fail(err)
		}
		return okMsg(fmt.Sprintf("trusted %s again", req.Target))

	case "forget":
		if req.Target == "" {
			return fail(fmt.Errorf("forget needs a cat name"))
		}
		if c, ok := d.Forget(req.Target); !ok {
			return fail(fmt.Errorf("no cat named %s", req.Target))
		} else {
			return okMsg(fmt.Sprintf("forgot %s", c.Name))
		}

	case "invite":
		code, err := d.StartInvite(ctx)
		if err != nil {
			return fail(err)
		}
		return okMsg(fmt.Sprintf("ask the other cat to run: clow join %s", code))

	case "join":
		if req.Words == "" {
			return fail(fmt.Errorf("join needs a pairing code"))
		}
		if err := d.Join(ctx, req.Words); err != nil {
			return fail(err)
		}
		return okMsg("paired")

	case "outbox":
		if req.Path != "clear" {
			return fail(fmt.Errorf("usage: clow outbox clear"))
		}
		n, err := d.ob.Clear()
		if err != nil {
			return fail(err)
		}
		return okMsg(fmt.Sprintf("cleared %d pending sends", n))

	case "status":
		me := d.Me()
		st := d.stats.snapshot()
		return Response{
			OK:         true,
			Me:         &me,
			Cats:       d.ros.All(),
			Distrusted: d.distrustedNames(),
			Receipts:   d.receipts.recent(10),
			Outbox:     d.ob.All(),
			Spool:      d.spool.Count(),
			SpoolBytes: d.spool.Usage(),
			Stats:      &st,
			Liveness:   d.livenessSnapshot(),
			Progress:   d.prog.snapshot(),
			Paths:      d.pathSnapshot(),
		}

	default:
		return fail(fmt.Errorf("unknown op %q", req.Op))
	}
}

func CallIPC(path string, req Request) (Response, error) {
	var conn net.Conn
	var err error
	if strings.Contains(path, ":") {
		conn, err = net.Dial("tcp", path)
	} else {
		conn, err = net.Dial("unix", path)
	}
	if err != nil {
		return Response{}, fmt.Errorf("daemon not running (%s): %w", path, err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Minute))
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return Response{}, fmt.Errorf("sending request: %w", err)
	}
	var resp Response
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return Response{}, fmt.Errorf("reading response: %w", err)
	}
	return resp, nil
}

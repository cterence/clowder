package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/cterence/clowder/roster"
)

// Request is one command from the clow CLI to the daemon. Ops: send,
// cancel, cats, storer, status, setinbox, invite, join, ping.
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
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
	// Log is a step line streamed while a long op (join, leave) runs;
	// a response without it is the final one.
	Log string `json:"log,omitempty"`
	// ID names the transfer a "send" queued, so the CLI can follow it.
	ID         string       `json:"id,omitempty"`
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
	// Settled maps a finished transfer ID to how it ended —
	// "delivered", or "stored via <storer>" — for the status op,
	// so a watcher can say which one happened. Bounded history.
	Settled map[string]string `json:"settled,omitempty"`
	// Blocked lists the keys this cat refuses (forget, or a leave
	// tombstone it applied): connections and roster re-adds both.
	Blocked map[string]bool `json:"blocked,omitempty"`
	// Tombstones lists the signed leaves on record, so a refusal can
	// be traced to when the cat left.
	Tombstones []roster.Tombstone `json:"tombstones,omitempty"`
}

func fail(err error) Response { return Response{OK: false, Error: err.Error()} }
func okMsg(s string) Response { return Response{OK: true, Message: s} }

// IPCPath returns the daemon's IPC socket path for a config dir.
func IPCPath(dir string) string { return filepath.Join(dir, "clow.sock") }

// listenIPC listens on a unix socket, user-only (any local process
// could otherwise drive the daemon).
func listenIPC(path string) (net.Listener, error) {
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
	enc := json.NewEncoder(conn)
	d.setStepLog(func(line string) { _ = enc.Encode(Response{OK: true, Log: line}) })
	resp := d.handleIPC(req)
	d.setStepLog(nil)
	if err := enc.Encode(resp); err != nil {
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
		return Response{OK: true, ID: id,
			Message: fmt.Sprintf("queued %s for %s (id %s)", filepath.Base(req.Path), req.Target, id)}

	case "cats":
		me := d.Me()
		return Response{OK: true, Me: &me, Cats: d.ros.All()}

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

	case "leave":
		n, err := d.Leave()
		if err != nil {
			return fail(err)
		}
		return okMsg(fmt.Sprintf("left the clowder, told %d cat(s). identity kept, pair again with clow invite or clow join", n))

	case "forget":
		if req.Target == "" {
			return fail(fmt.Errorf("forget needs a cat name or key"))
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

	case "cancel":
		n, err := d.Cancel(req.Target)
		if err != nil {
			return fail(err)
		}
		if n == 0 {
			return okMsg("nothing to cancel")
		}
		if req.Target != "" {
			return okMsg("cancelled the pending send")
		}
		return okMsg(fmt.Sprintf("cancelled %d pending send(s)", n))

	case "ping":
		if req.Target == "" {
			return fail(fmt.Errorf("ping needs a cat name or key"))
		}
		pingCtx, cancel := context.WithTimeout(ctx, pingTimeout)
		defer cancel()
		msg, err := d.Ping(pingCtx, req.Target)
		if err != nil {
			return fail(err)
		}
		return okMsg(msg)

	case "status":
		me := d.Me()
		st := d.stats.snapshot()
		return Response{
			OK:         true,
			Me:         &me,
			Cats:       d.ros.All(),
			Outbox:     d.ob.All(),
			Spool:      d.spool.Count(),
			SpoolBytes: d.spool.Usage(),
			Stats:      &st,
			Liveness:   d.livenessSnapshot(),
			Progress:   d.prog.snapshot(),
			Settled:    d.settledSnapshot(),
			Blocked:    d.blockedSnapshot(),
			Tombstones: d.ros.Tombstones(),
		}

	default:
		return fail(fmt.Errorf("unknown op %q", req.Op))
	}
}

// WatchIPC sends req and feeds every step log to logf as it arrives,
// returning the final response. Daemons that stream nothing (older
// builds) behave exactly like CallIPC.
func WatchIPC(path string, req Request, logf func(string)) (Response, error) {
	conn, err := dialIPC(path)
	if err != nil {
		return Response{}, err
	}
	defer func() { _ = conn.Close() }()
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return Response{}, fmt.Errorf("sending request: %w", err)
	}
	dec := json.NewDecoder(conn)
	for {
		var resp Response
		if err := dec.Decode(&resp); err != nil {
			return Response{}, fmt.Errorf("reading response: %w", err)
		}
		if resp.Log == "" {
			return resp, nil
		}
		logf(resp.Log)
	}
}

func dialIPC(path string) (net.Conn, error) {
	conn, err := net.Dial("unix", path)
	if err != nil {
		return nil, fmt.Errorf("daemon not running (%s): %w", path, err)
	}
	_ = conn.SetDeadline(time.Now().Add(2 * time.Minute))
	return conn, nil
}

func CallIPC(path string, req Request) (Response, error) {
	conn, err := dialIPC(path)
	if err != nil {
		return Response{}, err
	}
	defer func() { _ = conn.Close() }()
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return Response{}, fmt.Errorf("sending request: %w", err)
	}
	var resp Response
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return Response{}, fmt.Errorf("reading response: %w", err)
	}
	return resp, nil
}

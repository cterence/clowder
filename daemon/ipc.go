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

// Request is one command from the clow CLI to the daemon. Ops: add,
// send, fetch, cats, storer, status.
type Request struct {
	Op     string `json:"op"`
	Name   string `json:"name,omitempty"`
	Addr   string `json:"addr,omitempty"`
	Target string `json:"target,omitempty"`
	Path   string `json:"path,omitempty"`
	On     bool   `json:"on,omitempty"`
}

// Response is the daemon's reply.
type Response struct {
	OK      bool         `json:"ok"`
	Error   string       `json:"error,omitempty"`
	Message string       `json:"message,omitempty"`
	Me      *roster.Cat  `json:"me,omitempty"`
	Cats    []roster.Cat `json:"cats,omitempty"`
	Outbox  []Entry      `json:"outbox,omitempty"`
	Spool   int          `json:"spool,omitempty"`
}

func fail(err error) Response { return Response{OK: false, Error: err.Error()} }
func okMsg(s string) Response { return Response{OK: true, Message: s} }

// IPCPath returns the daemon's IPC socket path for a config dir.
func IPCPath(dir string) string { return filepath.Join(dir, "clow.sock") }

// listenIPC listens on a unix socket, or a TCP address if path contains a
// colon (useful for tests).
func listenIPC(path string) (net.Listener, error) {
	if strings.Contains(path, ":") {
		return net.Listen("tcp", path)
	}
	_ = os.Remove(path)
	return net.Listen("unix", path)
}

// serveIPCConn handles one CLI connection: one request, one response.
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
	switch req.Op {
	case "add":
		if req.Name == "" || req.Addr == "" {
			return fail(fmt.Errorf("add needs a name and an address"))
		}
		if err := d.AddCat(req.Name, req.Addr); err != nil {
			return fail(err)
		}
		return okMsg(fmt.Sprintf("added %s", req.Name))

	case "send":
		id, err := d.Send(req.Target, req.Path)
		if err != nil {
			return fail(err)
		}
		return okMsg(fmt.Sprintf("queued %s for %s (id %s)", filepath.Base(req.Path), req.Target, id))

	case "fetch":
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		d.Poll(ctx)
		return okMsg("fetch pass complete")

	case "cats":
		me := d.Me()
		return Response{OK: true, Me: &me, Cats: d.ros.All()}

	case "storer":
		if err := d.SetStorer(req.On); err != nil {
			return fail(err)
		}
		if req.On {
			return okMsg("storer role on")
		}
		return okMsg("storer role off")

	case "setinbox":
		if req.Path == "" {
			return fail(fmt.Errorf("setinbox needs a path"))
		}
		if err := d.SetInbox(req.Path); err != nil {
			return fail(err)
		}
		return okMsg(fmt.Sprintf("inbox now %s", d.InboxDir()))

	case "status":
		me := d.Me()
		return Response{
			OK:     true,
			Me:     &me,
			Cats:   d.ros.All(),
			Outbox: d.ob.All(),
			Spool:  d.spool.Count(),
		}

	default:
		return fail(fmt.Errorf("unknown op %q", req.Op))
	}
}

// CallIPC is the CLI's client: dial the daemon's socket, send one
// request, read one response.
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

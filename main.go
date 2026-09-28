// Command clow is the clowder CLI: it manages the local cat's identity
// and talks to the clowder daemon over its IPC socket.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"clowder/daemon"
	"clowder/roster"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "clow: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		return usage()
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "init":
		return cmdInit(rest)
	case "daemon":
		return cmdDaemon(rest)
	case "invite":
		return cmdInvite()
	case "join":
		return cmdJoin(rest)
	case "cancel":
		return cmdCancel(rest)
	case "forget":
		return cmdForget(rest)
	case "leave":
		return printResp(call(daemon.Request{Op: "leave"}))
	case "reset":
		return cmdReset(rest)
	case "send":
		return cmdSend(rest)
	case "storer":
		return cmdStorer(rest)
	case "status":
		return cmdStatus(rest)
	case "inbox":
		return cmdInbox(rest)
	default:
		return fmt.Errorf("unknown command %q (run \"clow --help\" for usage)", cmd)
	}
}

// usage is the whole help: grouped by what you are doing, one line per
// command, printed for bare `clow` and `clow --help` alike.
func usage() error {
	fmt.Print(`clow - a member of a clowder, an async file-transfer mesh over tailcat

usage: clow <command> [args]

identity and lifecycle:
  clow init [--name NAME] [--dir DIR] [--inbox DIR]  create this cat's identity
  clow daemon [--port N] [--health ADDR] [--derp-map URL]
                                                      run the mesh daemon
  clow leave                                          depart: signed goodbye, every cat drops you
  clow reset [--yes]                                  wipe this cat's identity and rosters — a
                                                      local wipe; depart with clow leave,
                                                      stop the daemon first

pairing (the only source of trust):
  clow invite                                         print an 8-word pairing code (5 min, one use)
  clow join <CODE>                                    pair with the cat that invited

transferring files:
  clow send [--async] <CAT> <FILE>                    send a file; watches progress until it is
                                                      delivered or a storer holds it (--async
                                                      queues and returns; Ctrl-C cancels)
  clow inbox [--set DIR]                              list received files, or move the inbox
  clow cancel [<ID>]                                  cancel one pending send by its id, or all

roles:
  clow storer [--max SIZE] on|off|dropbox             hold sealed files for others
                                                      (dropbox: third parties only)

mesh state:
  clow status [--addresses]                           roster, liveness, transfers, outbox and spool
                                                      (--addresses also prints tailcat addresses)
  clow forget <CAT-OR-KEY>                            drop a cat from the roster for good (the
                                                      key disambiguates duplicate names)

config dir:  $CLOWDER_DIR, else the OS user config home (~/.config/clowder
             on Linux, ~/Library/Application Support/clowder on macOS)
inbox:       ~/Downloads/clowder by default; "clow inbox --set DIR" moves it
pairing:     trust comes only from invite/join codes, never from exchanging addresses
daemon:      every command except init and inbox needs it running
`)
	return nil
}

func configDir() string {
	if d := os.Getenv("CLOWDER_DIR"); d != "" {
		return d
	}
	base, err := os.UserConfigDir()
	if err != nil {
		log.Fatalf("clow: no config home: %v", err)
	}
	return filepath.Join(base, "clowder")
}

func cmdInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	name := fs.String("name", defaultName(), "this cat's declared name")
	dir := fs.String("dir", configDir(), "config directory")
	inbox := fs.String("inbox", "", "directory received files land in (default: ~/Downloads/clowder)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *name == "" {
		return fmt.Errorf("cat name is required")
	}
	if err := daemon.Init(*dir, *name); err != nil {
		return err
	}
	if *inbox != "" {
		abs, err := filepath.Abs(*inbox)
		if err != nil {
			return err
		}
		if err := daemon.SetInboxAt(*dir, abs); err != nil {
			return err
		}
	}
	fmt.Printf("initialized cat %s\n", *name)
	fmt.Printf("config: %s\n", *dir)
	fmt.Printf("inbox:  %s\n", daemon.InboxDir(*dir))
	fmt.Println("run `clow daemon` to join the clowder")
	return nil
}

// defaultName: $CLOWDER_NAME if set, else the short hostname.
func defaultName() string {
	if n := os.Getenv("CLOWDER_NAME"); n != "" {
		return n
	}
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "cat"
	}
	for i := 0; i < len(h); i++ {
		if h[i] == '.' {
			return h[:i]
		}
	}
	return h
}

func cmdDaemon(args []string) error {
	fs := flag.NewFlagSet("daemon", flag.ContinueOnError)
	port := fs.Uint("port", daemon.DefaultPort, "clowder protocol port")
	name := fs.String("name", defaultName(), "cat name, used to auto-initialize a fresh cat")
	health := fs.String("health", os.Getenv("CLOWDER_HEALTH_ADDR"), "HTTP health endpoint for container probes, e.g. :8080 (empty disables)")
	derpMap := fs.String("derp-map", os.Getenv("CLOWDER_DERPMAP_URL"), "URL of a JSON DERP map to use instead of tailcat's default (for self-hosted relays)")
	pprofOn := fs.Bool("pprof", os.Getenv("CLOWDER_PPROF") == "1", "serve net/http/pprof on the health endpoint (requires --health)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	dir := configDir()
	if _, err := os.Stat(filepath.Join(dir, "identity.json")); os.IsNotExist(err) {
		// First start in a fresh volume: init before joining.
		if err := daemon.Init(dir, *name); err != nil {
			return err
		}
		fmt.Printf("initialized new cat %s in %s\n", *name, dir)
	}
	env, err := daemon.Open(dir)
	if err != nil {
		return err
	}
	logf := func(format string, args ...any) { log.Printf(format, args...) }
	cfg := daemon.Config{Dir: dir, Logf: logf, HealthAddr: *health, DERPMapURL: *derpMap, Pprof: *pprofOn}
	tr := daemon.NewTailcatTransport(env.Identity, env.ClientIdentity, uint16(*port), logf)
	tr.DERPMapURL = *derpMap
	d, err := daemon.New(cfg, tr)
	if err != nil {
		return err
	}
	// Desired state, applied on every start. Empty leaves the role alone.
	if role := os.Getenv("CLOWDER_STORER"); role != "" {
		var roleErr error
		capacity, _ := daemon.ParseSize(os.Getenv("CLOWDER_MAX"))
		switch role {
		case "on":
			roleErr = d.SetStorer(true, capacity)
		case "dropbox":
			roleErr = d.SetDropbox(true, capacity)
		case "off":
			roleErr = d.SetStorer(false, 0)
		default:
			roleErr = fmt.Errorf("CLOWDER_STORER must be on, off or dropbox (got %q)", role)
		}
		if roleErr != nil {
			return roleErr
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	fmt.Printf("cat %s joining the clowder (config: %s)\n", env.Me.Name, dir)
	return d.Run(ctx)
}

func call(req daemon.Request) (daemon.Response, error) {
	return daemon.CallIPC(daemon.IPCPath(configDir()), req)
}

func printResp(resp daemon.Response, err error) error {
	if err != nil {
		return err
	}
	if !resp.OK {
		return fmt.Errorf("%s", resp.Error)
	}
	if resp.Message != "" {
		fmt.Println(resp.Message)
	}
	return nil
}

func cmdInvite() error {
	return printResp(call(daemon.Request{Op: "invite"}))
}

func cmdJoin(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: clow join <CODE> (the words from `clow invite`)")
	}
	return printResp(call(daemon.Request{Op: "join", Words: strings.Join(args, " ")}))
}

func cmdCancel(args []string) error {
	fs := flag.NewFlagSet("cancel", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 1 {
		return fmt.Errorf("usage: clow cancel [<ID>]")
	}
	target := ""
	if fs.NArg() == 1 {
		target = fs.Arg(0)
	}
	return printResp(call(daemon.Request{Op: "cancel", Target: target}))
}

func cmdForget(args []string) error {
	fs := flag.NewFlagSet("forget", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: clow forget <CAT-OR-KEY>")
	}
	return printResp(call(daemon.Request{Op: "forget", Target: fs.Arg(0)}))
}

// cmdReset wipes the config dir (the inbox is kept). Refuses while the
// daemon runs: a confirmed reset first tells it to announce a leave while
// it can still reach anyone.
// cmdReset wipes the config dir (the inbox is kept) — a purely local
// operation. Departure is `clow leave`, the only thing that can still
// sign a goodbye; reset refuses to run under a live daemon because it
// owns the dir.
func cmdReset(args []string) error {
	fs := flag.NewFlagSet("reset", flag.ContinueOnError)
	yes := fs.Bool("yes", false, "skip the confirmation prompt")
	if err := fs.Parse(args); err != nil {
		return err
	}
	dir := configDir()

	if conn, err := net.Dial("unix", daemon.IPCPath(dir)); err == nil {
		_ = conn.Close()
		return fmt.Errorf("the daemon is running in %s; stop it before resetting (run \"clow leave\" first if you want the clowder to drop you)", dir)
	}
	if _, err := os.Stat(filepath.Join(dir, "identity.json")); err != nil {
		return fmt.Errorf("no cat to reset in %s", dir)
	}
	if err := confirmReset(dir, *yes); err != nil {
		return err
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("resetting %s: %w", dir, err)
	}
	fmt.Printf("cat reset; run \"clow init\" to create a new one\n")
	return nil
}

func confirmReset(dir string, yes bool) error {
	if yes {
		return nil
	}
	fmt.Printf("reset the cat in %s? its identity, rosters, spool and outbox will be deleted [y/N] ", dir)
	var answer string
	if _, err := fmt.Scanln(&answer); err != nil {
		return err
	}
	if answer != "y" && answer != "Y" && answer != "yes" {
		return fmt.Errorf("aborted")
	}
	return nil
}

// dupTag marks a name claimed by more than one key, naming this cat's
// short key and entry age so `clow forget <KEY>` can disambiguate.
func dupTag(c roster.Cat, dup bool) string {
	if !dup {
		return ""
	}
	return fmt.Sprintf(" [duplicate name · key %s · updated %s ago]", shortKey(c.Key), sinceStr(c.Updated))
}

// shortKey renders the identifying prefix of a node key ("nodekey:...").
func shortKey(k string) string {
	if i := strings.IndexByte(k, ':'); i >= 0 {
		k = k[i+1:]
	}
	if len(k) > 8 {
		k = k[:8]
	}
	return k
}

func nameCounts(cats []roster.Cat) map[string]int {
	counts := map[string]int{}
	for _, c := range cats {
		counts[c.Name]++
	}
	return counts
}

func storerTag(storer, dropbox bool) string {
	switch {
	case dropbox:
		return " [dropbox]"
	case storer:
		return " [storer]"
	}
	return ""
}

func cmdSend(args []string) error {
	fs := flag.NewFlagSet("send", flag.ContinueOnError)
	async := fs.Bool("async", false, "queue the send and return (default: watch until delivered or a storer holds it)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return fmt.Errorf("usage: clow send [--async] <CAT> <FILE>")
	}
	path, err := filepath.Abs(fs.Arg(1))
	if err != nil {
		return err
	}
	resp, err := call(daemon.Request{Op: "send", Target: fs.Arg(0), Path: path})
	if err != nil {
		return err
	}
	if !resp.OK {
		return fmt.Errorf("%s", resp.Error)
	}
	fmt.Println(resp.Message)
	if *async || resp.ID == "" {
		return nil
	}
	return watchSend(resp.ID, filepath.Base(path), fs.Arg(0))
}

// watchSend follows a queued transfer until it leaves the outbox:
// delivered directly, or accepted by a storer for an offline target.
// Ctrl-C cancels the send — the entry is dropped and any in-flight
// attempt is aborted.
func watchSend(id, file, target string) error {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	defer signal.Stop(sig)
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	var last string
	for {
		select {
		case <-sig:
			if _, err := call(daemon.Request{Op: "cancel", Target: id}); err != nil {
				return err
			}
			fmt.Printf("\r%-72s\n", fmt.Sprintf("cancelled %s (pending send dropped; an in-flight attempt is aborted)", file))
			return nil
		case <-tick.C:
		}
		resp, err := call(daemon.Request{Op: "status"})
		if err != nil {
			return err
		}
		if !resp.OK {
			return fmt.Errorf("%s", resp.Error)
		}
		queued := false
		for _, e := range resp.Outbox {
			if e.ID == id {
				queued = true
				break
			}
		}
		if !queued {
			fmt.Printf("\r%-72s\n", fmt.Sprintf("sent %s to %s (delivered, or held by a storer until it is online)", file, target))
			return nil
		}
		line := fmt.Sprintf("waiting: %s queued for %s", file, target)
		if p, ok := sendingID(resp.Progress, id); ok {
			line = fmt.Sprintf("sending %s to %s: %3.0f%% of %s%s",
				file, target, p.Percent()*100, daemon.HumanBytes(p.Total), rateSuffix(p))
		}
		if line != last {
			fmt.Printf("\r%-72s", line)
			last = line
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func cmdStorer(args []string) error {
	fs := flag.NewFlagSet("storer", flag.ContinueOnError)
	max := fs.String("max", "", "spool capacity, e.g. 500M or 10G (required to enable)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: clow storer [--max SIZE] on|off|dropbox")
	}
	mode := fs.Arg(0)
	switch mode {
	case "on", "dropbox":
		if *max == "" {
			return fmt.Errorf("storer %s requires --max (e.g. --max 10G)", mode)
		}
	case "off":
	default:
		return fmt.Errorf("usage: clow storer [--max SIZE] on|off|dropbox")
	}
	return printResp(call(daemon.Request{
		Op:      "storer",
		On:      mode != "off",
		Dropbox: mode == "dropbox",
		Max:     *max,
	}))
}

func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	addresses := fs.Bool("addresses", false, "also print each cat's tailcat address")
	if err := fs.Parse(args); err != nil {
		return err
	}
	resp, err := call(daemon.Request{Op: "status"})
	if err != nil {
		return err
	}
	if !resp.OK {
		return fmt.Errorf("%s", resp.Error)
	}

	if resp.Me != nil {
		fmt.Printf("me:      %s%s\n", resp.Me.Name, storerTag(resp.Me.Storer, resp.Me.Dropbox))
		if *addresses {
			fmt.Printf("         address: %s\n", resp.Me.Addr)
		}
	}
	fmt.Printf("config:  %s\n", configDir())
	fmt.Printf("inbox:   %s\n", inboxDir())

	dups := nameCounts(resp.Cats)
	fmt.Printf("\nclowder: %d %s\n", len(resp.Cats), plural(len(resp.Cats), "cat", "cats"))
	for _, c := range resp.Cats {
		name := c.Name + storerTag(c.Storer, c.Dropbox)
		seen, ok := resp.Liveness[c.Key]
		var life string
		switch {
		case !ok:
			life = "offline (never seen)"
		case time.Since(time.Unix(seen, 0)) < 2*time.Minute:
			life = fmt.Sprintf("online (seen %s ago)", sinceStr(seen))
		default:
			life = fmt.Sprintf("offline (seen %s ago)", sinceStr(seen))
		}
		fmt.Printf("  %-22s %-24s%s\n", name, life, dupTag(c, dups[c.Name] > 1))
		if *addresses {
			fmt.Printf("    address: %s\n", c.Addr)
		}
	}

	if len(resp.Progress) > 0 {
		fmt.Println("\ntransfers:")
		for _, p := range resp.Progress {
			if p.Receiving {
				fmt.Printf("  receiving %-24s from %-16s %3.0f%% of %s%s\n",
					p.FileName, p.Peer, p.Percent()*100, daemon.HumanBytes(p.Total), rateSuffix(p))
			}
		}
		for _, p := range resp.Progress {
			if !p.Receiving {
				fmt.Printf("  sending   %-24s to   %-16s %3.0f%% of %s%s\n",
					p.FileName, p.Peer, p.Percent()*100, daemon.HumanBytes(p.Total), rateSuffix(p))
			}
		}
	}

	fmt.Printf("\noutbox: %d pending\n", len(resp.Outbox))
	for _, e := range resp.Outbox {
		queue := sinceStr(e.AddedAt) + " old"
		if p, ok := sendingID(resp.Progress, e.ID); ok {
			queue = fmt.Sprintf("%s, sending %.0f%%", sinceStr(e.AddedAt)+" old", p.Percent()*100)
		}
		fmt.Printf("  %s -> %s (%s)\n", e.FileName, e.TargetName, queue)
	}

	if resp.Spool > 0 || (resp.Me != nil && resp.Me.Storer) {
		fmt.Printf("\nspool:  %s held", daemon.HumanBytes(resp.SpoolBytes))
		if resp.Me != nil && resp.Me.Storer && resp.Me.Capacity > 0 {
			fmt.Printf(" (of %s capacity)", daemon.HumanBytes(resp.Me.Capacity))
		}
		fmt.Printf(", %d %s\n", resp.Spool, plural(resp.Spool, "file", "files"))
	}

	if resp.Stats != nil {
		st := resp.Stats
		fmt.Printf("\nstats:  sent %d %s (%s), received %d %s (%s)\n",
			st.Sent, plural(int(st.Sent), "file", "files"), daemon.HumanBytes(st.SentBytes),
			st.Received, plural(int(st.Received), "file", "files"), daemon.HumanBytes(st.ReceivedBytes))
		fmt.Printf("        spooled %d, pushed %d\n", st.Spooled, st.Pushed)
	}
	return nil
}

func sendingID(progress []daemon.Progress, id string) (daemon.Progress, bool) {
	for _, p := range progress {
		if p.ID == id && !p.Receiving {
			return p, true
		}
	}
	return daemon.Progress{}, false
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// rateSuffix renders the transfer rate, when measurable.
func rateSuffix(p daemon.Progress) string {
	if p.Bps <= 0 {
		return ""
	}
	return fmt.Sprintf(" at %s/s", daemon.HumanBytes(int64(p.Bps)))
}

func sinceStr(unix int64) string {
	return time.Since(time.Unix(unix, 0)).Round(time.Second).String()
}

func inboxDir() string { return daemon.InboxDir(configDir()) }

// cmdInbox lists received files with their full paths, or sets the
// directory they land in.
func cmdInbox(args []string) error {
	fs := flag.NewFlagSet("inbox", flag.ContinueOnError)
	set := fs.String("set", "", "change the directory received files land in")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *set != "" {
		abs, err := filepath.Abs(*set)
		if err != nil {
			return err
		}
		dir := configDir()
		// A running daemon applies it live; without one it persists for the
		// next start.
		if conn, err := net.Dial("unix", daemon.IPCPath(dir)); err == nil {
			_ = conn.Close()
			return printResp(call(daemon.Request{Op: "setinbox", Path: abs}))
		}
		if err := daemon.SetInboxAt(dir, abs); err != nil {
			return err
		}
		fmt.Printf("daemon not running; inbox set to %s for the next daemon start\n", abs)
		return nil
	}
	dir := inboxDir()
	des, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Printf("no inbox yet at %s\n", dir)
			return nil
		}
		return fmt.Errorf("reading inbox: %w", err)
	}
	if len(des) == 0 {
		fmt.Printf("inbox %s is empty\n", dir)
		return nil
	}
	for _, de := range des {
		info, err := de.Info()
		if err != nil {
			continue
		}
		fmt.Printf("%s\t%s\n", info.ModTime().Format(time.DateTime), filepath.Join(dir, de.Name()))
	}
	return nil
}

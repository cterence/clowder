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
	if len(args) == 0 {
		return shortUsage()
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
	case "outbox":
		return cmdOutbox(rest)
	case "forget":
		return cmdForget(rest)
	case "distrust":
		return cmdDistrust(rest)
	case "trust":
		return cmdTrust(rest)
	case "rotate":
		return printResp(call(daemon.Request{Op: "rotate"}))
	case "reset":
		return cmdReset(rest)
	case "help":
		return longUsage()
	case "cats":
		return cmdCats()
	case "send":
		return cmdSend(rest)
	case "fetch":
		return cmdFetch()
	case "storer":
		return cmdStorer(rest)
	case "status":
		return cmdStatus()
	case "inbox":
		return cmdInbox(rest)
	default:
		return fmt.Errorf("unknown command %q", cmd)
	}
}

// shortUsage prints the one-screen command list.
func shortUsage() error {
	fmt.Fprint(os.Stderr, `usage: clow <command> [args]

  init, daemon, invite, join, send, fetch, inbox, cats, storer,
  outbox, forget, rotate, reset, status, help

run "clow help" for details.
`)
	return nil
}

// longUsage prints the full command reference and conventions.
func longUsage() error {
	fmt.Print(`clow - a member of a clowder, an async file-transfer mesh over tailcat

usage:
  clow init [--name NAME] [--dir CONFIG_DIR] [--inbox INBOX_DIR]
                                           create this cat's identity
  clow daemon [--port N] [--health ADDR] [--derp-map URL]
                                           run the mesh daemon
  clow invite                              print a pairing code (8 words, 5 min)
  clow join <CODE>                         pair with the cat that invited
  clow send <CAT> <FILE>                   send a file asynchronously
  clow fetch                               pull files storers hold for me
  clow cats                                list the clowder
  clow inbox [--set DIR]                   list received files, or move the inbox
  clow storer on|off|dropbox               storer duty; dropbox = third parties only
  clow outbox clear                        drop all pending sends
  clow distrust <CAT>                     block a cat locally, both ways (no gossip)
  clow trust <CAT>                         undo distrust
  clow forget <CAT>                        drop a cat from the roster
  clow rotate                              new address, announced to the clowder
  clow reset [--yes]                       wipe this cat's identity and rosters
  clow status                              config, outbox, spool and roster summary

config dir:  $CLOWDER_DIR, else the OS user config home (~/.config/clowder
             on Linux, ~/Library/Application Support/clowder on macOS)
inbox:       ~/Downloads/clowder by default; "clow inbox --set DIR" moves it
pairing:     trust comes only from "clow invite" / "clow join" pairing
             codes, never from exchanging addresses
daemon:      every command except init and inbox needs it running
`)
	return nil
}

// configDir resolves the cat's config directory.
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
	if err := fs.Parse(flagsFirst(fs, args)); err != nil {
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

// defaultName derives a cat name from the environment: $CLOWDER_NAME if
// set (the usual way in containers), else the short hostname.
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
	if err := fs.Parse(flagsFirst(fs, args)); err != nil {
		return err
	}
	dir := configDir()
	if _, err := os.Stat(filepath.Join(dir, "identity.json")); os.IsNotExist(err) {
		// First start (typical in a container with a fresh volume):
		// become a cat before joining the clowder.
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
	// CLOWDER_STORER declares the role as desired state, the
	// container-friendly way: applied on every start, so a restart
	// reasserts it. Empty means leave the persisted role alone.
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

// call sends one IPC request and prints the reply.
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

// cmdInvite starts a pairing code the other cat joins with.
func cmdInvite() error {
	return printResp(call(daemon.Request{Op: "invite"}))
}

// cmdJoin pairs with an inviter using the given code words.
func cmdJoin(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: clow join <CODE> (the words from `clow invite`)")
	}
	return printResp(call(daemon.Request{Op: "join", Words: strings.Join(args, " ")}))
}

// cmdOutbox manages pending sends; only "clear" exists for now.
func cmdOutbox(args []string) error {
	if len(args) != 1 || args[0] != "clear" {
		return fmt.Errorf("usage: clow outbox clear")
	}
	return printResp(call(daemon.Request{Op: "outbox", Path: "clear"}))
}

// cmdForget drops a cat from the roster.
func cmdForget(args []string) error {
	fs := flag.NewFlagSet("forget", flag.ContinueOnError)
	if err := fs.Parse(flagsFirst(fs, args)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: clow forget <CAT>")
	}
	return printResp(call(daemon.Request{Op: "forget", Target: fs.Arg(0)}))
}

// cmdDistrust blocks a cat locally, both ways, without gossip.
func cmdDistrust(args []string) error {
	fs := flag.NewFlagSet("distrust", flag.ContinueOnError)
	if err := fs.Parse(flagsFirst(fs, args)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: clow distrust <CAT>")
	}
	return printResp(call(daemon.Request{Op: "distrust", Target: fs.Arg(0)}))
}

// cmdTrust undoes cmdDistrust.
func cmdTrust(args []string) error {
	fs := flag.NewFlagSet("trust", flag.ContinueOnError)
	if err := fs.Parse(flagsFirst(fs, args)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: clow trust <CAT>")
	}
	return printResp(call(daemon.Request{Op: "trust", Target: fs.Arg(0)}))
}

// cmdReset wipes the cat's config dir: identity, rosters, spool and
// outbox. Received files in the inbox dir are outside the config dir and
// are kept. Refuses while the daemon is running.
func cmdReset(args []string) error {
	fs := flag.NewFlagSet("reset", flag.ContinueOnError)
	yes := fs.Bool("yes", false, "skip the confirmation prompt")
	if err := fs.Parse(flagsFirst(fs, args)); err != nil {
		return err
	}
	dir := configDir()

	if conn, err := net.Dial("unix", daemon.IPCPath(dir)); err == nil {
		_ = conn.Close()
		return fmt.Errorf("daemon is running in %s; stop it before resetting", dir)
	}
	if _, err := os.Stat(filepath.Join(dir, "identity.json")); err != nil {
		return fmt.Errorf("no cat to reset in %s", dir)
	}
	if !*yes {
		fmt.Printf("reset the cat in %s? its identity, rosters, spool and outbox will be deleted [y/N] ", dir)
		var answer string
		if _, err := fmt.Scanln(&answer); err != nil {
			return err
		}
		if answer != "y" && answer != "Y" && answer != "yes" {
			return fmt.Errorf("aborted")
		}
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("resetting %s: %w", dir, err)
	}
	fmt.Printf("cat reset; run \"clow init\" to create a new one\n")
	return nil
}

// flagsFirst reorders args so flags precede positionals, letting Go's
// flag package (which stops at the first positional) parse
// `clow add <addr> --name x` as well as `clow add --name x <addr>`.
// A flag that takes a value keeps the token after it attached; boolean
// flags do not.
func flagsFirst(fs *flag.FlagSet, args []string) []string {
	var flags, pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "-" || !strings.HasPrefix(a, "-") {
			pos = append(pos, a)
			continue
		}
		flags = append(flags, a)
		if strings.Contains(a, "=") {
			continue // --flag=value carries its own value
		}
		name := strings.TrimPrefix(strings.TrimPrefix(a, "--"), "-")
		if fl := fs.Lookup(name); fl != nil && !isBoolFlag(fl) && i+1 < len(args) {
			i++
			flags = append(flags, args[i])
		}
	}
	return append(flags, pos...)
}

// isBoolFlag reports whether a flag's value is boolean, which never
// consumes the following argument.
func isBoolFlag(fl *flag.Flag) bool {
	bv, ok := fl.Value.(interface{ IsBoolFlag() bool })
	return ok && bv.IsBoolFlag()
}

func cmdCats() error {
	resp, err := call(daemon.Request{Op: "cats"})
	if err != nil {
		return err
	}
	if !resp.OK {
		return fmt.Errorf("%s", resp.Error)
	}
	distrusted := map[string]bool{}
	for _, n := range resp.Distrusted {
		distrusted[n] = true
	}
	dups := nameCounts(resp.Cats)
	if resp.Me != nil {
		fmt.Printf("%s (me)%s\n", resp.Me.Name, storerTag(resp.Me.Storer, resp.Me.Dropbox))
		fmt.Printf("  address: %s\n", resp.Me.Addr)
	}
	for _, c := range resp.Cats {
		fmt.Printf("%s%s%s%s\n", c.Name, storerTag(c.Storer, c.Dropbox), distrustTag(distrusted[c.Name]), dupTag(dups[c.Name] > 1))
		fmt.Printf("  address: %s\n", c.Addr)
	}
	return nil
}

// dupTag marks names claimed by more than one cat.
func dupTag(dup bool) string {
	if dup {
		return " [duplicate name]"
	}
	return ""
}

// nameCounts counts name claims across the roster for dupTag.
func nameCounts(cats []roster.Cat) map[string]int {
	counts := map[string]int{}
	for _, c := range cats {
		counts[c.Name]++
	}
	return counts
}

// distrustTag marks cats on the local blocklist.
func distrustTag(on bool) string {
	if on {
		return " [distrusted]"
	}
	return ""
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
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return fmt.Errorf("usage: clow send <CAT> <FILE>")
	}
	path, err := filepath.Abs(fs.Arg(1))
	if err != nil {
		return err
	}
	return printResp(call(daemon.Request{Op: "send", Target: fs.Arg(0), Path: path}))
}

func cmdFetch() error {
	return printResp(call(daemon.Request{Op: "fetch"}))
}

func cmdStorer(args []string) error {
	fs := flag.NewFlagSet("storer", flag.ContinueOnError)
	max := fs.String("max", "", "spool capacity, e.g. 500M or 10G (required to enable)")
	if err := fs.Parse(flagsFirst(fs, args)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: clow storer on|off|dropbox [--max SIZE]")
	}
	mode := fs.Arg(0)
	switch mode {
	case "on", "dropbox":
		if *max == "" {
			return fmt.Errorf("storer %s requires --max (e.g. --max 10G)", mode)
		}
	case "off":
	default:
		return fmt.Errorf("usage: clow storer on|off|dropbox [--max SIZE]")
	}
	return printResp(call(daemon.Request{
		Op:      "storer",
		On:      mode != "off",
		Dropbox: mode == "dropbox",
		Max:     *max,
	}))
}

func cmdStatus() error {
	resp, err := call(daemon.Request{Op: "status"})
	if err != nil {
		return err
	}
	if !resp.OK {
		return fmt.Errorf("%s", resp.Error)
	}

	// Me and directories.
	if resp.Me != nil {
		fmt.Printf("me:      %s%s\n", resp.Me.Name, storerTag(resp.Me.Storer, resp.Me.Dropbox))
	}
	fmt.Printf("config:  %s\n", configDir())
	fmt.Printf("inbox:   %s\n", inboxDir())

	// The clowder: one row per cat with liveness and route.
	distrusted := map[string]bool{}
	for _, n := range resp.Distrusted {
		distrusted[n] = true
	}
	dups := nameCounts(resp.Cats)
	fmt.Printf("\nclowder: %d %s\n", len(resp.Cats), plural(len(resp.Cats), "cat", "cats"))
	for _, c := range resp.Cats {
		name := c.Name + storerTag(c.Storer, c.Dropbox)
		seen, ok := resp.Liveness[c.Key]
		var life string
		switch {
		case !ok:
			life = "never seen"
		case time.Since(time.Unix(seen, 0)) < 2*time.Minute:
			life = fmt.Sprintf("online (seen %s ago)", sinceStr(seen))
		default:
			life = fmt.Sprintf("seen %s ago", sinceStr(seen))
		}
		route := ""
		if p := resp.Paths[c.Key]; p != nil {
			if p.Direct {
				route = "direct " + p.Endpoint
			} else {
				// The relay's region label is not reliably
				// recoverable from roster addresses, so keep it
				// generic.
				route = "relayed via DERP"
			}
		}
		fmt.Printf("  %-22s %-24s %s%s%s\n", name, life, route, distrustTag(distrusted[c.Name]), dupTag(dups[c.Name] > 1))
	}

	// In-flight transfers, both directions.
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

	// Queued but not in flight.
	fmt.Printf("\noutbox: %d pending\n", len(resp.Outbox))
	for _, e := range resp.Outbox {
		queue := age(e.AddedAt)
		if p, ok := sendingID(resp.Progress, e.ID); ok {
			queue = fmt.Sprintf("%s, sending %.0f%%", age(e.AddedAt), p.Percent()*100)
		}
		fmt.Printf("  %s -> %s (%s)\n", e.FileName, e.TargetName, queue)
	}

	// Storer duty.
	if resp.Spool > 0 || (resp.Me != nil && resp.Me.Storer) {
		fmt.Printf("\nspool:  %s held", daemon.HumanBytes(resp.SpoolBytes))
		if resp.Me != nil && resp.Me.Storer && resp.Me.Capacity > 0 {
			fmt.Printf(" (of %s capacity)", daemon.HumanBytes(resp.Me.Capacity))
		}
		fmt.Printf(", %d %s\n", resp.Spool, plural(resp.Spool, "file", "files"))
	}

	// Confirmed deliveries (receipts), newest first.
	if len(resp.Receipts) > 0 {
		fmt.Printf("\nreceipts: %d shown\n", len(resp.Receipts))
		for i := len(resp.Receipts) - 1; i >= 0; i-- {
			r := resp.Receipts[i]
			fmt.Printf("  %-24s from %-16s delivered %s\n", r.FileName, r.From, age(r.DeliveredAt))
		}
	}

	// Lifetime counters.
	if resp.Stats != nil {
		st := resp.Stats
		fmt.Printf("\nstats:  sent %d %s (%s), received %d %s (%s)\n",
			st.Sent, plural(int(st.Sent), "file", "files"), daemon.HumanBytes(st.SentBytes),
			st.Received, plural(int(st.Received), "file", "files"), daemon.HumanBytes(st.ReceivedBytes))
		fmt.Printf("        spooled %d, fetched %d\n", st.Spooled, st.Fetched)
	}
	return nil
}

// sendingID returns the in-flight send progress for a transfer ID.
func sendingID(progress []daemon.Progress, id string) (daemon.Progress, bool) {
	for _, p := range progress {
		if p.ID == id && !p.Receiving {
			return p, true
		}
	}
	return daemon.Progress{}, false
}

// plural picks the singular or plural form for n.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// age renders a coarse "x ago" for pending entries.
// rateSuffix renders an in-flight transfer's current rate, when enough
// of a window has elapsed to measure one.
func rateSuffix(p daemon.Progress) string {
	if p.Bps <= 0 {
		return ""
	}
	return fmt.Sprintf(" at %s/s", daemon.HumanBytes(int64(p.Bps)))
}

func age(unix int64) string {
	d := time.Since(time.Unix(unix, 0)).Round(time.Second)
	return d.String() + " old"
}

// sinceStr renders how long ago a unix time was, e.g. "4s" or "1m20s".
func sinceStr(unix int64) string {
	return time.Since(time.Unix(unix, 0)).Round(time.Second).String()
}

// inboxDir resolves where received files land for the current config dir.
func inboxDir() string { return daemon.InboxDir(configDir()) }

// cmdInbox lists received files with their full paths, or sets the
// directory they land in.
func cmdInbox(args []string) error {
	fs := flag.NewFlagSet("inbox", flag.ContinueOnError)
	set := fs.String("set", "", "change the directory received files land in")
	if err := fs.Parse(flagsFirst(fs, args)); err != nil {
		return err
	}
	if *set != "" {
		abs, err := filepath.Abs(*set)
		if err != nil {
			return err
		}
		dir := configDir()
		// A running daemon applies the change live; without one the
		// new inbox persists for the next start instead of failing on
		// the IPC socket.
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

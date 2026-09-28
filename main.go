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
	"slices"
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
		return printUsage()
	}
	cmd, rest := args[0], args[1:]
	if cmd == "-h" || cmd == "--help" {
		return printUsage()
	}
	if slices.Contains(rest, "-h") || slices.Contains(rest, "--help") {
		fmt.Print(helpText(cmd))
		return nil
	}
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
		return cmdLeave()
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

// commandDoc is one command's help entry; usageText renders the whole
// list for `clow --help`, helpText renders one command for
// `clow <command> --help` — with only the hints that concern it.
type commandDoc struct {
	name    string
	group   string
	summary string // one line, shown on the root help
	usage   string
	flags   *flag.FlagSet // nil when the command takes no flags
	hints   []string
}

// doc finds one command's help entry; unknown names get the zero doc.
func doc(name string) commandDoc {
	for _, c := range commandDocs {
		if c.name == name {
			return c
		}
	}
	return commandDoc{}
}

const (
	hintConfigDir = "config dir: $CLOWDER_DIR, else the OS user config home (~/.config/clowder on Linux,\n             ~/Library/Application Support/clowder on macOS)"
	hintInbox     = "inbox: ~/Downloads/clowder by default, \"clow inbox --set DIR\" moves it"
	hintPair      = "pairing: trust comes only from invite/join codes, never from exchanging addresses"
	hintDaemon    = "daemon: this command needs the daemon running (clow daemon)"
)

var commandDocs = []commandDoc{
	{"init", "identity and lifecycle",
		"create this cat's identity",
		"clow init [--name NAME] [--dir DIR] [--inbox DIR]", initFS,
		[]string{hintConfigDir, hintInbox}},
	{"daemon", "identity and lifecycle",
		"run the mesh daemon",
		"clow daemon [--port N] [--health ADDR] [--derp-map URL] [--pprof]", daemonFS,
		[]string{hintConfigDir}},
	{"leave", "identity and lifecycle",
		"depart: signed goodbye, every cat drops you",
		"clow leave", nil, []string{hintDaemon}},
	{"reset", "identity and lifecycle",
		"wipe this cat's identity and rosters (a local wipe, not a departure)",
		"clow reset [--yes]", resetFS, []string{hintConfigDir}},
	{"invite", "pairing (the only source of trust)",
		"print an 8-word pairing code (5 min, one use)",
		"clow invite", nil, []string{hintPair, hintDaemon}},
	{"join", "pairing (the only source of trust)",
		"pair with the cat that invited",
		"clow join <CODE>", nil, []string{hintPair, hintDaemon}},
	{"send", "transferring files",
		"send a file, watching progress until it is delivered or a storer holds it",
		"clow send [--async] <CAT> <FILE>", sendFS, []string{hintDaemon}},
	{"inbox", "transferring files",
		"list received files, or move the inbox",
		"clow inbox [--set DIR]", inboxFS, []string{hintInbox}},
	{"cancel", "transferring files",
		"cancel one pending send by its id, or all",
		"clow cancel [<ID>]", nil, []string{hintDaemon}},
	{"storer", "roles",
		"hold sealed files for others (dropbox: third parties only)",
		"clow storer [--max SIZE] on|off|dropbox", storerFS, []string{hintDaemon}},
	{"status", "mesh state",
		"roster, liveness, transfers, outbox and spool",
		"clow status [--addresses]", statusFS, []string{hintDaemon}},
	{"forget", "mesh state",
		"drop a cat from the roster for good",
		"clow forget <CAT-OR-KEY>", nil, []string{hintDaemon}},
}

// usageText is the root help, urfave-cli style: command names aligned
// to cmdWidth, one-line summaries, no flags and no footers.
func usageText() string {
	var b strings.Builder
	b.WriteString("clow - a member of a clowder, an async file-transfer mesh over tailcat\n\n")
	b.WriteString("usage: clow <command> [args]  (clow <command> -h for flags and details)\n")
	var last string
	for _, c := range commandDocs {
		if c.group != last {
			fmt.Fprintf(&b, "\n%s:\n", c.group)
			last = c.group
		}
		fmt.Fprintf(&b, "  %-*s  %s\n", cmdWidth, c.name, c.summary)
	}
	return b.String()
}

// cmdWidth is the longest command name; keep it honest when adding one.
const cmdWidth = len("daemon")

func printUsage() error {
	fmt.Print(usageText())
	return nil
}

// helpText is one command's help: usage, summary, options, then the
// hints that concern it. An unknown command gets the global usage.
func helpText(cmd string) string {
	for _, c := range commandDocs {
		if c.name != cmd {
			continue
		}
		var b strings.Builder
		fmt.Fprintf(&b, "usage: %s\n\n%s\n", c.usage, c.summary)
		if c.flags != nil {
			fs := c.flags
			fs.SetOutput(&b)
			fmt.Fprint(&b, "\noptions:\n")
			fs.PrintDefaults()
		}
		if len(c.hints) > 0 {
			b.WriteString("\n")
		}
		for _, h := range c.hints {
			fmt.Fprintf(&b, "%s\n", h)
		}
		return b.String()
	}
	return usageText()
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

var initFS = flag.NewFlagSet("init", flag.ContinueOnError)

var (
	initName  = initFS.String("name", defaultName(), "this cat's declared name")
	initDir   = initFS.String("dir", "", "config directory (default: $CLOWDER_DIR, else the OS user config home)")
	initInbox = initFS.String("inbox", "", "directory received files land in (default: ~/Downloads/clowder)")
)

func cmdInit(args []string) error {
	if err := initFS.Parse(args); err != nil {
		return err
	}
	name, inbox := *initName, *initInbox
	dir := *initDir
	if dir == "" {
		dir = configDir()
	}
	if name == "" {
		return fmt.Errorf("cat name is required")
	}
	if err := daemon.Init(dir, name); err != nil {
		return err
	}
	if inbox != "" {
		abs, err := filepath.Abs(inbox)
		if err != nil {
			return err
		}
		if err := daemon.SetInboxAt(dir, abs); err != nil {
			return err
		}
	}
	fmt.Printf("initialized cat %s\n", name)
	fmt.Printf("config: %s\n", dir)
	fmt.Printf("inbox:  %s\n", daemon.InboxDir(dir))
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

var daemonFS = flag.NewFlagSet("daemon", flag.ContinueOnError)

var (
	daemonPort   = daemonFS.Uint("port", daemon.DefaultPort, "clowder protocol port")
	daemonName   = daemonFS.String("name", defaultName(), "cat name, used to auto-initialize a fresh cat")
	daemonHealth = daemonFS.String("health", os.Getenv("CLOWDER_HEALTH_ADDR"), "HTTP health endpoint for container probes, e.g. :8080 (empty disables)")
	daemonDerp   = daemonFS.String("derp-map", os.Getenv("CLOWDER_DERPMAP_URL"), "URL of a JSON DERP map to use instead of tailcat's default (for self-hosted relays)")
	daemonPprof  = daemonFS.Bool("pprof", os.Getenv("CLOWDER_PPROF") == "1", "serve net/http/pprof on the health endpoint (requires --health)")
)

func cmdDaemon(args []string) error {
	if err := daemonFS.Parse(args); err != nil {
		return err
	}
	name, health, derpMap, pprofOn := *daemonName, *daemonHealth, *daemonDerp, *daemonPprof
	dir := configDir()
	if _, err := os.Stat(filepath.Join(dir, "identity.json")); os.IsNotExist(err) {
		// First start in a fresh volume: init before joining.
		if err := daemon.Init(dir, name); err != nil {
			return err
		}
		fmt.Printf("initialized new cat %s in %s\n", name, dir)
	}
	env, err := daemon.Open(dir)
	if err != nil {
		return err
	}
	logf := func(format string, args ...any) { log.Printf(format, args...) }
	cfg := daemon.Config{Dir: dir, Logf: logf, HealthAddr: health, DERPMapURL: derpMap, Pprof: pprofOn}
	tr := daemon.NewTailcatTransport(env.Identity, env.ClientIdentity, uint16(*daemonPort), logf)
	tr.DERPMapURL = derpMap
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

// cmdJoin pairs via the daemon, blocking until the attempt finishes
// (the daemon logs which phase it is in). A Ctrl-C'd attempt keeps
// running in the daemon, so the pairing may still land.
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
		return fmt.Errorf("usage: %s", doc("cancel").usage)
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
		return fmt.Errorf("usage: %s", doc("forget").usage)
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
var resetFS = flag.NewFlagSet("reset", flag.ContinueOnError)
var resetYes = resetFS.Bool("yes", false, "skip the confirmation prompt")

// cmdLeave announces the leave through the daemon. Announcing to every
// cat can take a few seconds, so say so before the blocking call.
func cmdLeave() error {
	fmt.Println("leaving the clowder")
	return printResp(call(daemon.Request{Op: "leave"}))
}

func cmdReset(args []string) error {
	if err := resetFS.Parse(args); err != nil {
		return err
	}
	yes := *resetYes
	dir := configDir()

	if conn, err := net.Dial("unix", daemon.IPCPath(dir)); err == nil {
		_ = conn.Close()
		return fmt.Errorf("the daemon is running in %s, stop it before resetting (run \"clow leave\" first if you want the clowder to drop you)", dir)
	}
	if _, err := os.Stat(filepath.Join(dir, "identity.json")); err != nil {
		return fmt.Errorf("no cat to reset in %s", dir)
	}
	if err := confirmReset(dir, yes); err != nil {
		return err
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("resetting %s: %w", dir, err)
	}
	fmt.Printf("cat reset, run \"clow init\" to create a new one\n")
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
	return fmt.Sprintf(" [duplicate name, key %s, updated %s ago]", shortKey(c.Key), sinceStr(c.Updated))
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

var sendFS = flag.NewFlagSet("send", flag.ContinueOnError)
var sendAsync = sendFS.Bool("async", false, "queue the send and return (default: watch until delivered or a storer holds it)")

func cmdSend(args []string) error {
	if err := sendFS.Parse(args); err != nil {
		return err
	}
	async := *sendAsync
	if sendFS.NArg() != 2 {
		return fmt.Errorf("usage: %s", doc("send").usage)
	}
	path, err := filepath.Abs(sendFS.Arg(1))
	if err != nil {
		return err
	}
	resp, err := call(daemon.Request{Op: "send", Target: sendFS.Arg(0), Path: path})
	if err != nil {
		return err
	}
	if !resp.OK {
		return fmt.Errorf("%s", resp.Error)
	}
	fmt.Println(resp.Message)
	if async || resp.ID == "" {
		return nil
	}
	return watchSend(resp.ID, filepath.Base(path), sendFS.Arg(0))
}

// watchSend follows a queued transfer until it leaves the outbox:
// delivered directly, or accepted by a storer for an offline target.
// Ctrl-C cancels the send — the entry is dropped and any in-flight
// attempt is aborted.
func watchSend(id, file, target string) error {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	defer signal.Stop(sig)
	// Once a second: the percent and rate stay readable instead of
	// flickering at sub-second cadence.
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	var last string
	for {
		select {
		case <-sig:
			if _, err := call(daemon.Request{Op: "cancel", Target: id}); err != nil {
				return err
			}
			fmt.Printf("\r%-72s\n", fmt.Sprintf("cancelled %s", file))
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
	}
}

var storerFS = flag.NewFlagSet("storer", flag.ContinueOnError)
var storerMax = storerFS.String("max", "", "spool capacity, e.g. 500M or 10G (required to enable)")

func cmdStorer(args []string) error {
	if err := storerFS.Parse(args); err != nil {
		return err
	}
	max := *storerMax
	if storerFS.NArg() != 1 {
		return fmt.Errorf("usage: %s", doc("storer").usage)
	}
	mode := storerFS.Arg(0)
	switch mode {
	case "on", "dropbox":
		if max == "" {
			return fmt.Errorf("storer %s requires --max (e.g. --max 10G)", mode)
		}
	case "off":
	default:
		return fmt.Errorf("usage: %s", doc("storer").usage)
	}
	return printResp(call(daemon.Request{
		Op:      "storer",
		On:      mode != "off",
		Dropbox: mode == "dropbox",
		Max:     max,
	}))
}

var statusFS = flag.NewFlagSet("status", flag.ContinueOnError)
var statusAddresses = statusFS.Bool("addresses", false, "also print each cat's tailcat address")

func cmdStatus(args []string) error {
	if err := statusFS.Parse(args); err != nil {
		return err
	}
	addresses := *statusAddresses
	resp, err := call(daemon.Request{Op: "status"})
	if err != nil {
		return err
	}
	if !resp.OK {
		return fmt.Errorf("%s", resp.Error)
	}

	if resp.Me != nil {
		fmt.Printf("me:      %s%s\n", resp.Me.Name, storerTag(resp.Me.Storer, resp.Me.Dropbox))
		if addresses {
			fmt.Printf("         address: %s\n", resp.Me.Addr)
		}
	}
	fmt.Printf("config:  %s\n", configDir())
	fmt.Printf("inbox:   %s\n", inboxDir())

	dups := nameCounts(resp.Cats)
	fmt.Printf("\nclowder: %d %s\n", len(resp.Cats), plural(len(resp.Cats), "cat", "cats"))
	for _, c := range onlineFirst(resp.Cats, resp.Liveness) {
		name := c.Name + storerTag(c.Storer, c.Dropbox)
		seen, ok := resp.Liveness[c.Key]
		var life string
		switch {
		case !ok:
			life = "offline (never seen)"
		case time.Since(time.Unix(seen, 0)) < onlineWithin:
			life = fmt.Sprintf("online (seen %s ago)", sinceStr(seen))
		default:
			life = fmt.Sprintf("offline (seen %s ago)", sinceStr(seen))
		}
		fmt.Printf("  %-22s %-24s%s\n", name, life, dupTag(c, dups[c.Name] > 1))
		if addresses {
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

// onlineWithin is how recently a cat must have been seen to count as
// online, shared by the status line and its ordering.
const onlineWithin = 2 * time.Minute

// onlineFirst orders cats for `clow status`: online above offline,
// name order within each group. The input is name-sorted, so the
// stable partition keeps names ordered.
func onlineFirst(cats []roster.Cat, liveness map[string]int64) []roster.Cat {
	online := make([]roster.Cat, 0, len(cats))
	offline := make([]roster.Cat, 0, len(cats))
	for _, c := range cats {
		if time.Since(time.Unix(liveness[c.Key], 0)) < onlineWithin {
			online = append(online, c)
		} else {
			offline = append(offline, c)
		}
	}
	return append(online, offline...)
}

// sinceStr renders a coarse "how long ago": seconds, then minutes,
// hours, days — sub-unit precision is noise in a roster listing.
func sinceStr(unix int64) string {
	d := time.Since(time.Unix(unix, 0))
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

func inboxDir() string { return daemon.InboxDir(configDir()) }

// cmdInbox lists received files with their full paths, or sets the
// directory they land in.
var inboxFS = flag.NewFlagSet("inbox", flag.ContinueOnError)
var inboxSet = inboxFS.String("set", "", "change the directory received files land in")

func cmdInbox(args []string) error {
	if err := inboxFS.Parse(args); err != nil {
		return err
	}
	set := *inboxSet
	if set != "" {
		abs, err := filepath.Abs(set)
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
		fmt.Printf("daemon not running, inbox set to %s for the next daemon start\n", abs)
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

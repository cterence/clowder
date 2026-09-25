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
  clow init [--name NAME] [--dir DIR] [--inbox DIR]  create this cat's identity
  clow daemon [--port N]                  run the mesh daemon
  clow invite                             print a pairing code (5 words, 5 min)
  clow join <CODE>                        pair with the cat that invited
  clow send <CAT> <FILE>                  send a file asynchronously
  clow fetch                              pull files storers hold for me
  clow cats                               list the clowder
  clow inbox [--set DIR]                  list received files (full paths) or
                                           change where they land
  clow storer on|off|dropbox              storer duty; dropbox = third parties only
  clow outbox clear                       drop all pending sends
  clow forget <CAT>                      drop a cat from the roster
  clow rotate                            new address, announced to the clowder
  clow reset [--yes]                      wipe this cat's identity and rosters
  clow status                             config, outbox, spool and roster summary

The config dir defaults to $CLOWDER_DIR, else <user config home>/clowder
(~/.config/clowder on Linux, ~/Library/Application Support/clowder on
macOS). Received files land in a distinct inbox dir, defaulting to
~/Downloads/clowder, changeable with "clow inbox --set". Cats trust each
other via "clow invite" / "clow join" pairing codes; tailcat addresses
are never exchanged by hand. Everything but init and inbox needs the
daemon running.
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
	cfg := daemon.Config{Dir: dir, Logf: logf}
	tr := daemon.NewTailcatTransport(env.Identity, env.ClientIdentity, uint16(*port), logf)
	d, err := daemon.New(cfg, tr)
	if err != nil {
		return err
	}
	// CLOWDER_STORER declares the role as desired state, the
	// container-friendly way: applied on every start, so a restart
	// reasserts it. Empty means leave the persisted role alone.
	if role := os.Getenv("CLOWDER_STORER"); role != "" {
		var roleErr error
		switch role {
		case "on":
			roleErr = d.SetStorer(true)
		case "dropbox":
			roleErr = d.SetDropbox(true)
		case "off":
			roleErr = d.SetStorer(false)
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
	if resp.Me != nil {
		fmt.Printf("%s (me)%s\n", resp.Me.Name, storerTag(resp.Me.Storer, resp.Me.Dropbox))
		fmt.Printf("  address: %s\n", resp.Me.Addr)
	}
	for _, c := range resp.Cats {
		fmt.Printf("%s%s\n", c.Name, storerTag(c.Storer, c.Dropbox))
		fmt.Printf("  address: %s\n", c.Addr)
	}
	return nil
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
	if len(args) != 1 {
		return fmt.Errorf("usage: clow storer on|off|dropbox")
	}
	switch args[0] {
	case "on", "off", "dropbox":
		return printResp(call(daemon.Request{
			Op:      "storer",
			On:      args[0] != "off",
			Dropbox: args[0] == "dropbox",
		}))
	default:
		return fmt.Errorf("usage: clow storer on|off|dropbox")
	}
}

func cmdStatus() error {
	resp, err := call(daemon.Request{Op: "status"})
	if err != nil {
		return err
	}
	if !resp.OK {
		return fmt.Errorf("%s", resp.Error)
	}
	if resp.Me != nil {
		fmt.Printf("me:     %s%s\n", resp.Me.Name, storerTag(resp.Me.Storer, resp.Me.Dropbox))
	}
	fmt.Printf("config: %s\n", configDir())
	fmt.Printf("inbox:  %s\n", inboxDir())
	fmt.Printf("roster: %d %s\n", len(resp.Cats), plural(len(resp.Cats), "cat", "cats"))
	for _, c := range resp.Cats {
		name := c.Name + storerTag(c.Storer, c.Dropbox)
		seen, ok := resp.Liveness[c.Key]
		switch {
		case !ok:
			fmt.Printf("  %-28s never seen\n", name)
		case time.Since(time.Unix(seen, 0)) < 2*time.Minute:
			fmt.Printf("  %-28s online (seen %s ago)\n", name, sinceStr(seen))
		default:
			fmt.Printf("  %-28s seen %s ago\n", name, sinceStr(seen))
		}
	}
	fmt.Printf("outbox: %d pending\n", len(resp.Outbox))
	for _, e := range resp.Outbox {
		inFlight := ""
		for _, p := range resp.Progress {
			if p.ID == e.ID && !p.Receiving {
				inFlight = fmt.Sprintf(", sending %.0f%%", p.Percent()*100)
			}
		}
		fmt.Printf("  %s -> %s (%s%s)\n", e.FileName, e.TargetName, age(e.AddedAt), inFlight)
	}
	for _, p := range resp.Progress {
		if p.Receiving {
			fmt.Printf("receiving %-24s from %-16s %3.0f%% of %s\n",
				p.FileName, p.Peer, p.Percent()*100, daemon.HumanBytes(p.Total))
		}
	}
	fmt.Printf("spool:  %d held\n", resp.Spool)
	if resp.Stats != nil {
		st := resp.Stats
		fmt.Printf("stats:  sent %d %s (%s), received %d %s (%s)\n",
			st.Sent, plural(int(st.Sent), "file", "files"), daemon.HumanBytes(st.SentBytes),
			st.Received, plural(int(st.Received), "file", "files"), daemon.HumanBytes(st.ReceivedBytes))
		fmt.Printf("        spooled %d, fetched %d\n", st.Spooled, st.Fetched)
	}
	return nil
}

// plural picks the singular or plural form for n.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// age renders a coarse "x ago" for pending entries.
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
		return printResp(call(daemon.Request{Op: "setinbox", Path: *set}))
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

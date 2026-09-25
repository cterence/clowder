// Command clow is the clowder CLI: it manages the local cat's identity
// and talks to the clowder daemon over its IPC socket.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
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
		return usage(nil)
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "init":
		return cmdInit(rest)
	case "daemon":
		return cmdDaemon(rest)
	case "add":
		return cmdAdd(rest)
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
		return cmdInbox()
	default:
		return fmt.Errorf("unknown command %q", cmd)
	}
}

func usage(nilErr error) error {
	fmt.Fprint(os.Stderr, `clow - a member of a clowder, an async file-transfer mesh over tailcat

usage:
  clow init [--name NAME] [--dir DIR]     create this cat's identity
  clow daemon [--port N]                  run the mesh daemon
  clow add <ADDR> --name NAME             trust another cat (out of band)
  clow send <CAT> <FILE>                  send a file asynchronously
  clow fetch                              pull files storers hold for me
  clow cats                               list the clowder
  clow inbox                              list received files with full paths
  clow storer on|off                      declare or retract storer duty
  clow status                             config, outbox, spool and roster summary

The config dir defaults to $CLOWDER_DIR, else <user config home>/clowder
(~/.config/clowder on Linux, ~/Library/Application Support/clowder on
macOS). Received files land in <config dir>/inbox. Everything but init
and inbox needs the daemon running.
`)
	return nilErr
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

// inboxDir is where received files land.
func inboxDir() string { return filepath.Join(configDir(), "inbox") }

func cmdInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	name := fs.String("name", defaultName(), "this cat's declared name")
	dir := fs.String("dir", configDir(), "config directory")
	if err := fs.Parse(flagsFirst(fs, args)); err != nil {
		return err
	}
	if *name == "" {
		return fmt.Errorf("cat name is required")
	}
	if err := daemon.Init(*dir, *name); err != nil {
		return err
	}
	fmt.Printf("initialized cat %s in %s\n", *name, *dir)
	fmt.Println("run `clow daemon` to join the clowder")
	return nil
}

// defaultName derives a cat name from the hostname.
func defaultName() string {
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
	if err := fs.Parse(flagsFirst(fs, args)); err != nil {
		return err
	}
	dir := configDir()
	env, err := daemon.Open(dir)
	if err != nil {
		return err
	}
	logf := func(format string, args ...any) { log.Printf(format, args...) }
	cfg := daemon.Config{Dir: dir, Logf: logf}
	tr := daemon.NewTailcatTransport(env.Identity, uint16(*port), logf)
	d, err := daemon.New(cfg, tr)
	if err != nil {
		return err
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

func cmdAdd(args []string) error {
	fs := flag.NewFlagSet("add", flag.ContinueOnError)
	name := fs.String("name", "", "the new cat's declared name (required)")
	if err := fs.Parse(flagsFirst(fs, args)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: clow add <ADDR> --name NAME")
	}
	return printResp(call(daemon.Request{Op: "add", Name: *name, Addr: fs.Arg(0)}))
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
		fmt.Printf("%s (me)%s\n", resp.Me.Name, storerTag(resp.Me.Storer))
		fmt.Printf("  address: %s\n", resp.Me.Addr)
	}
	for _, c := range resp.Cats {
		fmt.Printf("%s%s\n", c.Name, storerTag(c.Storer))
		fmt.Printf("  address: %s\n", c.Addr)
	}
	return nil
}

func storerTag(storer bool) string {
	if storer {
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
	if len(args) != 1 || (args[0] != "on" && args[0] != "off") {
		return fmt.Errorf("usage: clow storer on|off")
	}
	return printResp(call(daemon.Request{Op: "storer", On: args[0] == "on"}))
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
		fmt.Printf("me:     %s%s\n", resp.Me.Name, storerTag(resp.Me.Storer))
	}
	fmt.Printf("config: %s\n", configDir())
	fmt.Printf("inbox:  %s\n", inboxDir())
	fmt.Printf("roster: %d cats\n", len(resp.Cats))
	fmt.Printf("outbox: %d pending\n", len(resp.Outbox))
	for _, e := range resp.Outbox {
		fmt.Printf("  %s -> %s (%s)\n", e.FileName, e.TargetName, age(e.AddedAt))
	}
	fmt.Printf("spool:  %d held\n", resp.Spool)
	return nil
}

// age renders a coarse "x ago" for pending entries.
func age(unix int64) string {
	d := time.Since(time.Unix(unix, 0)).Round(time.Second)
	return d.String() + " old"
}

// cmdInbox lists received files with their full paths.
func cmdInbox() error {
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

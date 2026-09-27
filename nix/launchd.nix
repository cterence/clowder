# macOS: the per-user launchd agent. `nix build .#launchd-agent`
# produces Library/LaunchAgents/dev.clowder.clow.plist wired to the
# store-path clow binary. Install and load it once:
#
#   cp -f result/Library/LaunchAgents/dev.clowder.clow.plist \
#       ~/Library/LaunchAgents/
#   launchctl load ~/Library/LaunchAgents/dev.clowder.clow.plist
#
# RunAtLoad starts it at login, KeepAlive restarts it whenever it exits.
# launchd freezes it with the machine on suspend, so it survives
# suspend/resume without restarting. The plist pins the current store
# path; rebuild and re-copy to upgrade (a garbage-collected path leaves
# the agent dead until reinstalled).
{ clow, lib, stdenv }:

stdenv.mkDerivation {
  pname = "clow-launchd-agent";
  inherit (clow) version;

  buildCommand = ''
    mkdir -p $out/Library/LaunchAgents
    cat > $out/Library/LaunchAgents/dev.clowder.clow.plist <<'EOF'
    <?xml version="1.0" encoding="UTF-8"?>
    <!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
    <plist version="1.0">
    <dict>
        <key>Label</key>
        <string>dev.clowder.clow</string>
        <key>ProgramArguments</key>
        <array>
            <string>${clow}/bin/clow</string>
            <string>daemon</string>
        </array>
        <key>RunAtLoad</key>
        <true/>
        <key>KeepAlive</key>
        <true/>
        <key>ProcessType</key>
        <string>Background</string>
        <key>StandardOutPath</key>
        <string>~/Library/Logs/clowder.log</string>
        <key>StandardErrorPath</key>
        <string>~/Library/Logs/clowder.log</string>
    </dict>
    </plist>
    EOF
  '';

  meta = with lib; {
    description = "launchd agent for the clow daemon (per-user)";
    platforms = platforms.darwin;
  };
}

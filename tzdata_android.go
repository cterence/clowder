//go:build android

package main

// Android sandboxes carry no zoneinfo database and /etc/localtime is
// unreliable there, so std log timestamps resolve to UTC. The app
// passes the phone's zone in TZ; the embedded database resolves it.
import _ "time/tzdata"

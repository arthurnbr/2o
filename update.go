package main

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/arthurnbr/2o/internal/ui"
)

// buildInfo identifies a 2o binary. It is printed by the hidden
// `2o version-info` command as one line: "VERSION PROTOCOL BUILDTIME".
// Old binaries do not know the command; they are reported as unknown.
type buildInfo struct {
	version  string
	protocol int // 0: unknown (a build older than version-info)
	built    time.Time
}

func (b buildInfo) String() string {
	if b.version == "" {
		return "an older 2o"
	}
	return b.version
}

// label names b for a message about b and self: their versions, or their
// build dates when the versions read the same (e.g. two local builds).
func (b buildInfo) label(self buildInfo) string {
	if b.version == self.version && !b.built.IsZero() {
		return b.version + " built " + b.built.Local().Format("2006-01-02 15:04")
	}
	return b.String()
}
func thisBuild() buildInfo {
	t, _ := time.Parse(time.RFC3339, buildTime)
	return buildInfo{version: version, protocol: protocol, built: t}
}

func versionInfoLine() string {
	bt := buildTime
	if bt == "" {
		bt = "-"
	}
	return fmt.Sprintf("%s %d %s", version, protocol, bt)
}

func parseBuildInfo(s string) buildInfo {
	f := strings.Fields(s)
	if len(f) != 3 {
		return buildInfo{}
	}
	p, err := strconv.Atoi(f[1])
	if err != nil {
		return buildInfo{}
	}
	t, _ := time.Parse(time.RFC3339, f[2])
	return buildInfo{version: f[0], protocol: p, built: t}
}

// drift compares a peer build with this one.
type drift int

const (
	sameBuild    drift = iota // identical binary or same version: nothing to do
	peerOlder                 // peer is an older build: offer to update it
	peerNewer                 // peer is newer than this 2o: keep it, warn
	peerUnknown               // cannot tell (dev builds, missing info): offer to update
	incompatible              // different protocol: they cannot talk, update is required
)

func compareBuilds(peer, self buildInfo) drift {
	switch {
	case peer.protocol == 0:
		return peerUnknown // too old to report itself: offer, do not force
	case peer.protocol != self.protocol:
		return incompatible
	case peer.version == self.version && peer.built.Equal(self.built):
		return sameBuild
	case !peer.built.IsZero() && !self.built.IsZero() && peer.built.Before(self.built):
		return peerOlder
	case !peer.built.IsZero() && !self.built.IsZero() && peer.built.After(self.built):
		return peerNewer
	default:
		return peerUnknown
	}
}

// updatePolicy is TWO_O_UPDATE: "ask" (default on a terminal), "always", "never".
func updatePolicy() string {
	switch p := strings.ToLower(strings.TrimSpace(os.Getenv("TWO_O_UPDATE"))); p {
	case "always", "yes", "1", "true":
		return "always"
	case "never", "no", "0", "false":
		return "never"
	case "", "ask":
		return "ask"
	default:
		ui.Warn("ignoring TWO_O_UPDATE=%q (use ask, always or never)", p)
		return "ask"
	}
}

// askTTY is where Y/n answers are read from; replaced in tests. It reads
// /dev/tty so a piped `2o ssh host cmd < input` still asks the human.
var askTTY = func() (*os.File, error) { return os.Open("/dev/tty") }

// isInteractive reports whether a human can answer; replaced in tests.
var isInteractive = ui.Interactive

// confirmUpdate asks "question [Y/n]" and reports the answer. Without a
// terminal it does not block: it applies defaultYes and says so.
func confirmUpdate(question string, defaultYes bool) bool {
	switch updatePolicy() {
	case "always":
		return true
	case "never":
		return false
	}
	tty, err := askTTY()
	if err != nil || !isInteractive() {
		if tty != nil {
			tty.Close()
		}
		if defaultYes {
			ui.Note("no terminal to ask; updating (set TWO_O_UPDATE=never to keep it)")
		} else {
			ui.Note("no terminal to ask; keeping it (set TWO_O_UPDATE=always to update)")
		}
		return defaultYes
	}
	defer tty.Close()
	hint := "[Y/n]"
	if !defaultYes {
		hint = "[y/N]"
	}
	ui.Prompt("%s %s ", question, ui.Bold(hint))
	line, _ := bufio.NewReader(tty).ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "":
		return defaultYes
	case "y", "yes", "o", "oui":
		return true
	default:
		return false
	}
}

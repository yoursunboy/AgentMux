package terminal

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
)

// The device labels this server reports.
//
// They are a closed vocabulary rather than free text, and that is the point:
// a label is shown to every other viewer of a project, as "Controller: iPad
// Safari" in a panel header. A closed set cannot carry a script, an escape
// sequence, or a remark, and it cannot be chosen by the client either - it is
// the server's own reading of a header it already received.
const (
	DeviceUnknown = "Unknown device"
)

// deviceLabel turns a User-Agent header into the short label other clients see.
//
// # Why the server does this rather than the browser
//
// A browser could send whatever it liked as its device name, and a name that
// appears on somebody else's screen is a name worth not trusting. Reading the
// header here means the label is derived from something the browser had to send
// anyway in order to get a socket, and it means the label can never be a
// sentence, a URL, or a piece of terminal output.
//
// # Why the order of the checks matters
//
// Every Chromium browser claims to be Chrome and every browser on macOS claims
// to be Safari, so a naive search reports "Chrome Safari on macOS" for Edge on
// Windows. The checks below therefore run from the most specific claim to the
// least, which is the order the user-agent string itself is built in.
func deviceLabel(userAgent string) string {
	if userAgent == "" {
		return DeviceUnknown
	}
	ua := strings.ToLower(userAgent)

	browser := ""
	switch {
	case strings.Contains(ua, "edg/") || strings.Contains(ua, "edgios") || strings.Contains(ua, "edga"):
		browser = "Edge"
	case strings.Contains(ua, "opr/") || strings.Contains(ua, "opera"):
		browser = "Opera"
	case strings.Contains(ua, "firefox/") || strings.Contains(ua, "fxios"):
		browser = "Firefox"
	case strings.Contains(ua, "chrome/") || strings.Contains(ua, "crios"):
		browser = "Chrome"
	case strings.Contains(ua, "safari/"):
		browser = "Safari"
	}

	platform := ""
	switch {
	case strings.Contains(ua, "ipad"):
		// iPadOS reports itself as a Mac, so this has to be looked for before
		// the desktop platform below or every iPad is a Macintosh.
		platform = "iPad"
	case strings.Contains(ua, "iphone") || strings.Contains(ua, "ipod"):
		platform = "iPhone"
	case strings.Contains(ua, "android"):
		platform = "Android"
	case strings.Contains(ua, "windows"):
		platform = "Windows"
	case strings.Contains(ua, "mac os x") || strings.Contains(ua, "macintosh"):
		platform = "macOS"
	case strings.Contains(ua, "cros"):
		platform = "ChromeOS"
	case strings.Contains(ua, "linux"):
		platform = "Linux"
	}

	label := joinDevice(browser, platform)
	if label == "" {
		return DeviceUnknown
	}
	return label
}

// joinDevice combines the two halves, tolerating either being missing.
func joinDevice(browser, platform string) string {
	switch {
	case browser != "" && platform != "":
		return truncateDevice(browser + " on " + platform)
	case browser != "":
		return truncateDevice(browser)
	case platform != "":
		return truncateDevice(platform)
	default:
		return ""
	}
}

// truncateDevice bounds a label and strips anything that is not printable.
//
// The vocabulary above cannot produce a control character, so this is belt and
// braces rather than a live defence - but the label is copied into a JSON
// document that another browser renders, and a helper that assumes its input is
// already clean is the kind of assumption that stops being true.
func truncateDevice(label string) string {
	if len(label) > MaxDeviceLen {
		label = label[:MaxDeviceLen]
	}
	var b strings.Builder
	b.Grow(len(label))
	for _, r := range label {
		if r < 0x20 || r == 0x7f {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// validClientID reports whether a browser session identifier has the shape this
// protocol defines.
//
// It is a shape check and not an authorisation, and the distinction is the
// whole of it: the identifier is chosen by the client, so any client can
// present any identifier, and nothing here stops that. What it stops is an
// identifier that is obviously not one - a path, a name with a separator or a
// control character in it, an enormous string - being carried into a lookup, a
// log line, an error message, or another client's screen.
//
// The alphabet is deliberately narrow. There is no reason for a session
// identifier to contain anything but lowercase letters and digits, and every
// character allowed here is one that cannot be mistaken for syntax in a URL, a
// JSON document or a shell.
func validClientID(id string) bool {
	if len(id) <= len(ClientIDPrefix) || len(id) > MaxClientIDLen {
		return false
	}
	if !strings.HasPrefix(id, ClientIDPrefix) {
		return false
	}
	for _, r := range id[len(ClientIDPrefix):] {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
}

// clientIDRandomBytes is the entropy in a server-issued client identifier.
//
// Sixteen hex characters is 64 bits, which is far more than a single-user
// installation can collide on and short enough to read in a log line.
const clientIDRandomBytes = 8

// newClientID mints an identifier for a client that did not state one.
//
// # Why a client that sends nothing is not refused
//
// The identifier exists so that a reconnect can be recognised as the same
// browser session. A client that does not send one has not asked for that, and
// refusing it would refuse every non-browser client - including the ones this
// project's own tests use - in exchange for nothing. It is given an identifier
// so that the rest of the server has one rule rather than two, and the greeting
// tells it what it was given.
//
// # Why it is random rather than derived
//
// There is nothing to derive it from. It must not be the remote address, which
// changes when a phone moves between networks and is shared by everyone behind
// a NAT; it must not be a counter, which would make one client's identifier
// guessable from another's; and it must not be anything the server already
// knows about the person, because there is nothing the server knows about the
// person - see docs/MULTI_DEVICE.md §11.
func newClientID() string {
	buf := make([]byte, clientIDRandomBytes)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand does not fail on any platform this runs on. A panic here
		// is right: the alternative is handing out an identifier that is not
		// random, and two clients sharing one is two clients sharing a keyboard.
		panic("terminal: generate client id: " + err.Error())
	}
	return ClientIDPrefix + hex.EncodeToString(buf)
}

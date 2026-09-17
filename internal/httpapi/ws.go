package httpapi

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"github.com/kutonlagos/agentmux/internal/terminal"
)

// This file is the WebSocket half of the API: one endpoint, through which a
// browser watches and types into a project's terminal.
//
// It is deliberately small. The HTTP layer's job here is to decide whether
// this request may become a socket at all - the origin it comes from, the
// protocol version it claims - and then to hand the socket to the terminal
// hub. Everything after the handshake is a protocol this package does not
// interpret: it has no opinion about subscriptions, sequences or snapshots,
// and the code that does is in internal/terminal where it can be tested
// without an HTTP server.
//
// # Why one endpoint and not one per project
//
// A socket per terminal would put the project identifier in the URL, which
// reads well and behaves badly. Every subscription would be a new handshake, a
// new origin check and a new set of buffers; a reconnecting browser would
// open several at once; and the six-terminal grid this is heading towards
// would hold six sockets where one would do. The identifier travels in the
// message instead, where it is checked against the projects the server knows
// rather than against a route pattern.

// WebSocket handshake tuning.
const (
	// wsHandshakeTimeout bounds the upgrade itself.
	wsHandshakeTimeout = 10 * time.Second

	// Buffer sizes are small on purpose. A read buffer holds one client
	// message, and client messages are typed input and small commands; the
	// write buffer is where a frame waits for a client that is not reading, and
	// a large one there would be memory held for a client that is not being
	// served anyway. The real queues are in internal/terminal, which is where
	// the limits that matter are enforced.
	wsReadBufferBytes  = 4 << 10
	wsWriteBufferBytes = 4 << 10
)

// newUpgrader builds the upgrader for this server.
//
// It is built per server rather than being a package variable so that
// CheckOrigin can be the server's own policy. That policy is the one thing a
// WebSocket handshake must not get wrong, and a package-level upgrader with
// CheckOrigin returning true - which is what most examples show - would open
// a terminal to any page in any browser on the machine, since a WebSocket is
// not subject to the same-origin policy once the server accepts it.
func (s *Server) newUpgrader() websocket.Upgrader {
	return websocket.Upgrader{
		HandshakeTimeout: wsHandshakeTimeout,
		ReadBufferSize:   wsReadBufferBytes,
		WriteBufferSize:  wsWriteBufferBytes,
		CheckOrigin: func(r *http.Request) bool {
			return s.websocketOriginAllowed(r)
		},
		// The subprotocol is named here and therefore echoed to a client that
		// offers it. That matters for a browser specifically: a browser that
		// offers a subprotocol and is answered without one fails the handshake
		// outright, so offering it from the client without listing it here is not
		// a no-op, it is a connection that never opens. A client that offers
		// nothing - every test in this package, and any program that just wants
		// the bytes - is answered with no subprotocol, exactly as before.
		Subprotocols: []string{terminal.Subprotocol},
		// Compression is off. Terminal output is a run of escape sequences and
		// short text, not a document; several escape sequences in a frame
		// compress beautifully and the compression costs a context per
		// connection forever. A terminal is not the workload for it, and a
		// feature that costs always and pays rarely is one to leave alone.
		EnableCompression: false,
	}
}

// websocketOriginAllowed decides whether a browser page may open a terminal.
//
// It is the same policy CORS uses, plus one case CORS cannot express: a page
// served by this server is allowed to talk to it, wherever this server is
// reachable. That case is what makes AgentMux usable from a tablet on a LAN
// without configuring anything, and it is safe for the obvious reason - the
// page came from here.
func (s *Server) websocketOriginAllowed(r *http.Request) bool {
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		// Not a browser page. Browsers always send Origin on a WebSocket
		// handshake, so a request without one comes from a program on this
		// machine or on the network, and such a program has no need of a
		// browser's origin to reach a local server. Refusing it would break
		// every non-browser client - including this project's own tests -
		// without protecting anything: the cross-origin attack this check
		// exists to stop requires a browser.
		return true
	}
	if sameOrigin(origin, r.Host) {
		return true
	}
	return s.originAllowed(origin)
}

// sameOrigin reports whether an Origin names the host this request was sent
// to.
func sameOrigin(origin, host string) bool {
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Host == "" || host == "" {
		return false
	}
	return strings.EqualFold(parsed.Host, host)
}

// handleWebSocket implements GET /api/ws.
//
// It upgrades the request and then serves the connection until it ends, which
// means this handler does not return while a browser is watching a terminal.
// That is what a WebSocket is, and it is why the server's write timeout is
// disabled: a deadline there would be a deadline on a connection that is
// meant to stay open for hours. The limits that matter are enforced per frame
// by the terminal package.
func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	if s.terminal == nil {
		writeError(w, http.StatusServiceUnavailable, CodeInternal,
			"this server was started without a terminal transport", nil)
		return
	}
	if !s.websocketOriginAllowed(r) {
		s.log.Warn("refused a terminal connection from an unpermitted origin",
			"origin", r.Header.Get("Origin"), "remote", r.RemoteAddr)
		writeError(w, http.StatusForbidden, CodeForbidden,
			"this origin may not open a terminal connection", nil)
		return
	}
	if err := checkProtocolVersion(r); err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, err.Error(), nil)
		return
	}

	upgrader := s.newUpgrader()
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		// Upgrade has already written a response describing the failure, so
		// there is nothing to add but a record of it.
		s.log.Debug("terminal upgrade failed", "remote", r.RemoteAddr, "error", err)
		return
	}
	s.terminal.Serve(ws, r.RemoteAddr)
}

// checkProtocolVersion refuses a client that speaks a version this server does
// not.
//
// An absent parameter means the client has not stated one, which is allowed:
// the endpoint answers it with the current protocol's greeting, and a client
// that cannot use that greeting is expected to close. What is refused is a
// client that states a version, because stating one and being wrong is a
// deliberate question with a knowable answer, and the answer is not "guess".
func checkProtocolVersion(r *http.Request) error {
	stated := strings.TrimSpace(r.URL.Query().Get(terminal.ProtocolParam))
	if stated == "" {
		return nil
	}
	version, err := strconv.Atoi(stated)
	if err != nil {
		return fmt.Errorf("%s must be an integer protocol version", terminal.ProtocolParam)
	}
	if version != terminal.ProtocolVersion {
		return fmt.Errorf("this server speaks terminal protocol version %d, not %d",
			terminal.ProtocolVersion, version)
	}
	return nil
}

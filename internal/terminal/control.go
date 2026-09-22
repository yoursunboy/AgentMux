package terminal

import (
	"time"

	"github.com/kutonlagos/agentmux/internal/usage"
)

// This file is the browser-facing half of the control plane.
//
// authority.go decides who may type and who may resize; this file is how a
// browser asks, how it is answered, and how everybody else finds out. The split
// is deliberate. A rule that lives next to the socket it is enforced at is a
// rule that gets re-derived at the second socket, and the second socket is
// always the one somebody forgets - so the rule is over there, with no
// connection, no address and no wire format in it, and this file only carries
// its answers.
//
// # The one mechanism
//
// Every message in the control family carries the same roster: who holds the
// lease, whether it is suspended, when it lapses, who is waiting, and how many
// other people are watching. The directed messages - granted, denied, revoked -
// are not a second source of truth; they say what just happened *to this
// client*, and they arrive with the roster attached so that a client applies
// one thing rather than reconciling two.
//
// That is why there is no "control.requested" message. A request that was
// queued changes the roster, and the roster is already broadcast to everybody
// watching - including the controller, who is the one person who needs to know
// that somebody is asking. A separate message would be a second way to learn
// the same fact, and the two could disagree.
//
// # Why the broadcast is per-recipient
//
// controlView.Viewers counts the connections watching a project *other than
// this recipient's own and other than the controller's*. That number is
// different for every recipient, so the roster is built once per recipient
// rather than once per change. The cost is a map walk per viewer per change,
// and control changes are things a person does with their hands.

// controlMessage builds the message a roster change is reported with.
//
// `reason` is carried only by the two kinds that have one; the roster-only kinds
// have no field for it, which is what keeps "why" from being attached to a
// message that is not about a why.
func (h *Hub) controlMessage(kind, projectID, reason string, forConn *Conn) any {
	view := h.controlRoster(projectID, forConn)
	switch kind {
	case MsgControlGranted:
		return controlGrantedMessage{
			Type: MsgControlGranted, ProjectID: projectID,
			Reason: reason, Control: view,
		}
	case MsgControlDenied:
		return controlDeniedMessage{
			Type: MsgControlDenied, ProjectID: projectID,
			Reason: reason, Control: view,
		}
	case MsgControlRevoked:
		return controlRevokedMessage{
			Type: MsgControlRevoked, ProjectID: projectID,
			Reason: reason, Control: view,
		}
	case MsgControlExpired:
		return controlExpiredMessage{
			Type: MsgControlExpired, ProjectID: projectID, Control: view,
		}
	default:
		return controlChangedMessage{
			Type: MsgControlChanged, ProjectID: projectID, Control: view,
		}
	}
}

// controlRoster builds the view of a project's control for one recipient.
//
// The recipient is a parameter rather than something the caller filters
// afterwards because the viewer count is not a property of the project. It is
// "how many other people are watching this", and the answer depends on who is
// asking.
func (h *Hub) controlRoster(projectID string, forConn *Conn) controlView {
	lease := h.authority.Lease(projectID)
	view := controlView{
		Suspended: lease.Suspended,
		Viewers:   h.countViewers(projectID, forConn, lease.ControllerID),
	}
	if lease.Holder() {
		view.Controller = &controlHolder{ClientID: lease.ControllerID, Device: lease.Device}
	}
	if lease.Suspended && !lease.ExpiresAt.IsZero() {
		// Only while suspended. A connected controller holds its lease for as
		// long as it is there, so there is no moment to name.
		view.ExpiresAt = lease.ExpiresAt.UTC().Format(time.RFC3339)
	}
	for _, request := range lease.Pending {
		view.Pending = append(view.Pending, controlPending{
			ClientID: request.ClientID, Device: request.Device,
		})
	}
	return view
}

// countViewers counts the connections watching a project, other than the
// recipient's own and other than the controller's.
//
// The controller is excluded because a person does not count themselves among
// the people watching them, and the recipient is excluded for the same reason.
// What is left is the number a header can honestly show: how many *others* are
// looking at this terminal.
func (h *Hub) countViewers(projectID string, forConn *Conn, controllerID string) int {
	viewers := 0
	for _, c := range h.connections() {
		if c == forConn || (controllerID != "" && c.clientID == controllerID) {
			continue
		}
		if c.watches(projectID) {
			viewers++
		}
	}
	return viewers
}

// subscribersOf returns the connections watching a project.
//
// The set is copied under the hub's lock and the subscriptions are tested after
// it is released, because a connection's own lock is taken below the hub's
// everywhere else - and a helper that inverted that order once would be a
// deadlock that appears only under a broadcast during a disconnect.
func (h *Hub) subscribersOf(projectID string) []*Conn {
	subscribers := make([]*Conn, 0, 4)
	for _, c := range h.connections() {
		if c.watches(projectID) {
			subscribers = append(subscribers, c)
		}
	}
	return subscribers
}

// connections returns the hub's current connection set as a slice.
func (h *Hub) connections() []*Conn {
	h.mu.Lock()
	defer h.mu.Unlock()
	conns := make([]*Conn, 0, len(h.conns))
	for c := range h.conns {
		conns = append(conns, c)
	}
	return conns
}

// broadcastControl tells every viewer of a project that its roster changed.
//
// It is the only way a control change reaches a client that is not a party to
// it, and it is called after the authority has already changed - never before.
// A broadcast that went out first would show every viewer a roster that the
// next one contradicts.
func (h *Hub) broadcastControl(projectID, kind string) {
	for _, c := range h.subscribersOf(projectID) {
		c.sendControl(h.controlMessage(kind, projectID, "", c))
	}
}

// broadcastControlExcept is broadcastControl without one connection.
//
// It exists for the single case where a connection is being told something that
// everybody else is also being told, but must be told before them: a client that
// has just started watching is sent its roster before its snapshot, and the
// others are sent theirs once it is watching. Sending it to the newcomer a
// second time would put a roster after the first frame, which is the ordering
// the first send exists to prevent.
func (h *Hub) broadcastControlExcept(projectID, kind string, except *Conn) {
	for _, c := range h.subscribersOf(projectID) {
		if c == except {
			continue
		}
		c.sendControl(h.controlMessage(kind, projectID, "", c))
	}
}

// sendToClient sends a directed control message to one client's connections.
//
// It reaches every connection of that client that is watching the project, not
// just one: a client with two tabs open on the same terminal should not have
// one of them still drawing a Request Control button. Only the watching ones
// are told, because a connection that is not drawing this terminal has nothing
// to apply it to.
func (h *Hub) sendToClient(projectID, clientID, kind, reason string) {
	for _, c := range h.subscribersOf(projectID) {
		if c.clientID == clientID {
			c.sendControl(h.controlMessage(kind, projectID, reason, c))
		}
	}
}

// ---------------------------------------------------------------------------
// Connection

// sendControl queues one control message.
//
// It never drops one. Control messages go through the same bounded queue as
// terminal output, but unlike terminal bytes they are not abandoned when the
// queue is full: they wait, because a client that never learns it acquired
// control is a client with a dead keyboard, and the alternative - ending the
// connection - is worse than the wait. The wait is bounded by the connection
// itself: enqueue returns when the socket ends.
func (c *Conn) sendControl(v any) {
	c.enqueueText(encodeJSON(v))
}

// handleControlRequest asks for a project's control lease.
func (c *Conn) handleControlRequest(msg clientMessage) bool {
	if !validProjectID(msg.ProjectID) {
		c.sendError(CodeBadProject,
			"projectId must be an AgentMux project identifier", msg.ProjectID, MsgControlRequest)
		return true
	}
	if msg.hasExtraFields() {
		c.sendError(CodeBadMessage,
			"a control request carries only a projectId", msg.ProjectID, MsgControlRequest)
		return true
	}
	// A client can only ask to control a terminal it is watching. The check is
	// not about permission - it is that a lease held by a client which is not
	// drawing the terminal is a lease held by somebody who cannot see what they
	// are typing into, and every roster would name a controller nobody can see.
	if !c.watches(msg.ProjectID) {
		c.sendError(CodeNotSubscribed,
			"subscribe to this project before asking to control it",
			msg.ProjectID, MsgControlRequest)
		return true
	}

	// Recorded before the authority is asked rather than after it answered, so
	// that a request which was refused is counted too: the beta is measuring
	// how many people reach for a keyboard, and a lease somebody else holds is
	// still somebody reaching. Every answer below is a different outcome of one
	// event.
	//
	// The three refusals above this line are not recorded and are not the same
	// thing: a malformed project id, a message with extra fields and a request
	// to control a terminal the client is not watching are all a broken client
	// rather than a person, and counting them would make the number mean
	// "messages received".
	c.hub.noteUsage(c.ctx, usage.EventControllerRequest)

	outcome, reason := c.hub.authority.Request(msg.ProjectID, c.clientID, c.id, c.device)
	switch outcome {
	case RequestGranted:
		// The asker is told first, with the roster as it stands after the grant,
		// and then everybody else. The order matters only for readability in a
		// devtools frame list: both go through this connection's queue in order,
		// and no client can observe the roster before its own change to it.
		c.sendControl(c.hub.controlMessage(MsgControlGranted, msg.ProjectID, reason, c))
		c.hub.broadcastControl(msg.ProjectID, MsgControlChanged)
	case RequestHeld:
		// Already the controller. The grant is re-sent rather than answered with
		// an error, because the client's intent - I want control - is already
		// true, and nothing changed, so nobody else is told.
		c.sendControl(c.hub.controlMessage(MsgControlGranted, msg.ProjectID, reason, c))
	case RequestQueued:
		// Somebody has it and has now been told. There is no message for the
		// asker here: it is in the roster, which the broadcast carries to
		// everybody including itself.
		c.hub.broadcastControl(msg.ProjectID, MsgControlChanged)
	default:
		message := "another device is controlling this project"
		if reason == ReasonTooManyRequests {
			message = "too many devices are already waiting to control this project"
		}
		c.sendControl(controlDeniedMessage{
			Type: MsgControlDenied, ProjectID: msg.ProjectID,
			Reason: reason, Message: message,
			Control: c.hub.controlRoster(msg.ProjectID, c),
		})
	}
	return true
}

// handleControlRelease gives up a project's control lease.
func (c *Conn) handleControlRelease(msg clientMessage) bool {
	if !validProjectID(msg.ProjectID) {
		c.sendError(CodeBadProject,
			"projectId must be an AgentMux project identifier", msg.ProjectID, MsgControlRelease)
		return true
	}
	if msg.hasExtraFields() {
		c.sendError(CodeBadMessage,
			"a control release carries only a projectId", msg.ProjectID, MsgControlRelease)
		return true
	}
	// There is deliberately no "are you watching this" check. A controller that
	// paged away from a project is not watching it any more and still holds its
	// lease, and giving it up is the one thing it must be able to do from there.
	if !c.hub.authority.Release(msg.ProjectID, c.clientID) {
		c.sendError(CodeNotController,
			"you are not controlling this project", msg.ProjectID, MsgControlRelease)
		return true
	}
	// Recorded only where a lease actually changed hands. Unlike a request, a
	// release that was refused is not an event worth counting: it says a client
	// sent a message about a keyboard it never had, and the count it would join
	// is "keyboards given back".
	c.hub.noteUsage(c.ctx, usage.EventControllerRelease)

	c.sendControl(c.hub.controlMessage(MsgControlRevoked, msg.ProjectID, ReasonReleased, c))
	c.hub.broadcastControl(msg.ProjectID, MsgControlChanged)
	return true
}

// handleControlAccept handles both halves of a transfer: handing the lease to
// the client that asked, and declining to.
//
// They are one function because they are one exchange. The two messages differ
// in a single word and in what the authority is asked to do; splitting them
// would duplicate the validation, and the validation is the part that must not
// drift between them.
func (c *Conn) handleControlAccept(msg clientMessage, accept bool) bool {
	about := MsgControlAccept
	if !accept {
		about = MsgControlReject
	}

	if !validProjectID(msg.ProjectID) {
		c.sendError(CodeBadProject,
			"projectId must be an AgentMux project identifier", msg.ProjectID, about)
		return true
	}
	if len(msg.Data) > 0 || msg.Cols != 0 || msg.Rows != 0 {
		c.sendError(CodeBadMessage,
			"a control transfer carries a projectId and a clientId", msg.ProjectID, about)
		return true
	}
	if msg.ClientID == "" {
		c.sendError(CodeBadMessage,
			"a control transfer must name the client it is for", msg.ProjectID, about)
		return true
	}
	if !validClientID(msg.ClientID) {
		c.sendError(CodeBadMessage,
			"clientId is not a valid client identifier", msg.ProjectID, about)
		return true
	}
	if msg.ClientID == c.clientID {
		// Handing control to yourself is not a transfer, and the honest answer is
		// that the named client has not asked. It is caught here rather than
		// left to the authority because the message is malformed, not merely
		// unsatisfiable - and a client that sent it has confused a clientId with
		// a connectionId.
		c.sendError(CodeBadMessage,
			"a control transfer names a different client", msg.ProjectID, about)
		return true
	}

	var outcome TransferOutcome
	if accept {
		outcome = c.hub.authority.Accept(msg.ProjectID, c.clientID, msg.ClientID)
	} else {
		outcome = c.hub.authority.Reject(msg.ProjectID, c.clientID, msg.ClientID)
	}
	switch outcome {
	case TransferNotController:
		c.sendError(CodeNotController,
			"you must be controlling this project to hand it over", msg.ProjectID, about)
		return true
	case TransferNotPending:
		c.sendError(CodeNotPending,
			"that client has not asked to control this project", msg.ProjectID, about)
		return true
	}

	if accept {
		// The new controller is told before the old one is, and both before the
		// broadcast. A client that has just been handed a terminal should not
		// learn it from the roster of somebody else's message.
		//
		// The line names the project, and the acting client is on the logger
		// already. The client receiving control is not named a second time: a
		// record here carries one client, and the roster the broadcast carries
		// is what says who holds the lease now.
		c.hub.sendToClient(msg.ProjectID, msg.ClientID, MsgControlGranted, ReasonTransferred)
		c.log.Info("control handed over", "projectId", msg.ProjectID)
	} else {
		c.hub.sendToClient(msg.ProjectID, msg.ClientID, MsgControlDenied, ReasonRejected)
		c.log.Info("control request declined", "projectId", msg.ProjectID)
	}
	c.hub.broadcastControl(msg.ProjectID, MsgControlChanged)
	return true
}

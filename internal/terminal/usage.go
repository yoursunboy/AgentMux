package terminal

import (
	"context"

	"github.com/kutonlagos/agentmux/internal/usage"
)

// This file is the terminal package's whole knowledge of the beta's usage
// events, and there are three of them: a terminal being watched, a keyboard
// being asked for, and a keyboard being given back.
//
// # Why these three live here
//
// Every other event the beta records happens at a page or a model. These three
// happen at a socket, and a socket is the only thing that can witness them: a
// subscribe message is what makes a terminal appear on a screen, and a control
// lease is what makes a keyboard somebody's. An HTTP middleware could count
// requests, but a socket carries many messages after its one upgrade, and a
// count of upgrades would be a count of tabs rather than of terminals opened.
//
// # What is not recorded
//
// Nothing here has a byte of terminal content in it, and that is structural
// rather than careful: the events are values from a closed list, and the record
// they produce is an identifier, a name and a time. The input a controller
// types, the output a terminal prints, and the size of a pane are all absent,
// because none of them is an argument to anything below.
//
// The refusals are counted too, for the two that can be refused. A control
// request that was denied because another device holds the lease is still
// somebody having asked for a keyboard, and an installation where that happens
// constantly is one whose people are fighting over a terminal - which is a
// finding, and one that a count of granted leases alone would hide.

// noteUsage records a usage event, or does nothing.
//
// It never returns an error and never blocks on anything that could fail: the
// thing being counted has already happened - the subscription exists, the lease
// has been granted or refused - and a socket that disconnected because a count
// could not be written would be the count taking down the terminal it was
// counting. internal/usage.Service is where that contract is kept.
//
// A hub with no recorder is the ordinary case: recording is off unless the
// deployment turned the beta on, so this is a nil check on a path every
// subscribe and every lease takes.
func (h *Hub) noteUsage(ctx context.Context, eventType usage.EventType) {
	if h == nil || h.usage == nil {
		return
	}
	h.usage.Record(ctx, eventType)
}

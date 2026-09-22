package httpapi

import (
	"context"
	"regexp"
	"strings"

	"github.com/kutonlagos/agentmux/internal/usage"
)

// This file turns a page request into one of the five usage events, and it is
// the only place in this package that knows the events exist.
//
// # What is recorded, and what that is worth
//
// A page request is not a page view. The server sees that the entry point was
// asked for at /dashboard; whether a person then read it, and for how long, is
// not something this endpoint can know and is not something it tries to
// approximate. The number this produces answers "was the console opened at
// all", which is the question a beta needs answered and is honest about being
// the question.
//
// The other four events are recorded where they happen rather than from a path:
// terminal.connect from the socket that carries a terminal, and controller
// request and release from the two hub messages that take and give a keyboard -
// see internal/terminal. Only the two page events are derived from a URL here,
// because a page request is the one thing that happens nowhere else.
//
// # Why this is not middleware
//
// It would be shorter as a middleware over every request, and it would be
// wrong: a middleware would have to decide what to do about /api/... paths, a
// static asset, a 404, and a POST, and every one of those decisions would be a
// rule about counting that had nothing to do with serving. Here there is one
// call site - the single-page fallback, which is the only path in this server
// that means "a page was opened" - and the function below only has to answer
// which page.

// usageEventForPath maps a location path to the event it records, if any.
//
// It is a reading of web/src/dashboard/route.ts: the client decides which page
// to draw from the same string, and this decides which event to record, so the
// two have to agree about what a path means. The grammar is the client's -
// `/dashboard` is the console, `/actions` is the queue, `/actions/<one
// segment>` is one row of it, and everything else is the workspace - and the
// workspace is deliberately silent, because "/" is also what a deep link, a
// bookmark and a reload all land on, and a count of those is a count of the
// browser rather than of a person.
//
// The second return is false for every path that records nothing. That is the
// ordinary answer, not an error: the queue itself and the workspace both serve
// pages and neither is an event.
func usageEventForPath(pathname string) (usage.EventType, bool) {
	// A trailing slash is the same address, which is the client's rule too. The
	// empty result is the workspace's own path with its slashes taken off, and
	// it records nothing.
	trimmed := strings.TrimRight(pathname, "/")
	if trimmed == "" {
		return "", false
	}

	switch {
	case trimmed == dashboardPath:
		return usage.EventDashboardOpen, true

	case strings.HasPrefix(trimmed, actionsPath+"/"):
		// The same one-segment-and-a-shape test the client applies in
		// actionIdOf, and it is repeated here for the reason the paths above
		// are: the two have to agree about what a path means. A path that the
		// client answers by drawing the queue must not be recorded as somebody
		// reading one action.
		//
		// The shape test is not a security boundary and is not written as one -
		// an action that does not exist is still a page somebody opened to find
		// that out, and the server's own 404 is what answers for it.
		if actionIDPattern.MatchString(strings.TrimPrefix(trimmed, actionsPath+"/")) {
			return usage.EventActionView, true
		}
	}
	return "", false
}

// actionIDPattern is the shape of an action identifier, mirroring the regexp in
// actionIdOf. A single segment is the whole grammar, so the pattern implies it.
var actionIDPattern = regexp.MustCompile(`^act_[0-9a-f]+$`)

// The two page paths, mirroring DASHBOARD_PATH and ACTIONS_PATH in route.ts.
//
// They are spelled here rather than imported because there is nothing to import
// from: the client is TypeScript and this is Go, and the two are kept in step
// by the test that reads the client's own routing table. A path that changed on
// one side and not the other would record nothing, which is the failure mode
// that is visible rather than the one that silently doubles a count.
const (
	dashboardPath = "/dashboard"
	actionsPath   = "/actions"
)

// noteUsage records a usage event, or does nothing.
//
// It never returns an error and never fails its caller. The thing being
// counted has already happened by the time this is called - the page is about
// to be served, the socket is already open - and a navigation that failed
// because a count could not be written would be the count taking down the
// feature it exists to measure. internal/usage.Recorder is where that contract
// is stated and internal/usage.Service is where it is kept.
//
// A server with no recorder is the ordinary case: recording is off unless the
// deployment turned the beta on, so this is a nil check on the path every page
// load takes, and it is deliberately the first thing here.
func (s *Server) noteUsage(ctx context.Context, eventType usage.EventType) {
	if s.usage == nil {
		return
	}
	s.usage.Record(ctx, eventType)
}

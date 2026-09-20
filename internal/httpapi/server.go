// Package httpapi exposes the AgentMux REST surface.
//
// Handlers stay thin: they decode a request, call a service, and encode the
// result. Every rule about projects lives in the project package, and every
// statement about storage lives in the storage package. A handler that starts
// making decisions is a handler that has stopped being a handler.
package httpapi

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"time"

	"github.com/kutonlagos/agentmux/internal/agent"
	"github.com/kutonlagos/agentmux/internal/claude"
	"github.com/kutonlagos/agentmux/internal/config"
	"github.com/kutonlagos/agentmux/internal/event"
	"github.com/kutonlagos/agentmux/internal/host"
	"github.com/kutonlagos/agentmux/internal/project"
	"github.com/kutonlagos/agentmux/internal/session"
	"github.com/kutonlagos/agentmux/internal/task"
	"github.com/kutonlagos/agentmux/internal/terminal"
	"github.com/kutonlagos/agentmux/internal/version"
)

// maxRequestBodyBytes bounds a request body. AgentMux accepts small JSON
// objects; anything larger is a mistake or an attack.
const maxRequestBodyBytes = 64 << 10

// AgentResolver reports the coding agent this server would launch.
//
// It is an interface so that this package never resolves a binary to answer a
// request, and so that the capability report can be tested on a machine with no
// Claude Code, one with an old version, and one with several - the three cases
// the report exists to distinguish.
type AgentResolver interface {
	Resolve(ctx context.Context) claude.Installation
}

// Server is the AgentMux HTTP API.
type Server struct {
	cfg        *config.Config
	host       host.Adapter
	projects   *project.Service
	discoverer *project.Discoverer
	runtime    *session.Manager
	events     *event.Service
	tasks      *task.Service
	agent      AgentResolver
	agents     *agent.Service
	terminal   *terminal.Hub
	log        *slog.Logger

	startedAt time.Time
	webDir    string

	handler http.Handler

	// now is injectable so tests can produce a deterministic uptime.
	now func() time.Time
}

// Options configures a Server. Config, Host, Projects, Discoverer, and Runtime
// are required.
type Options struct {
	Config     *config.Config
	Host       host.Adapter
	Projects   *project.Service
	Discoverer *project.Discoverer

	// Runtime is the terminal runtime manager. It is required: a server without
	// one cannot answer a single runtime request, and a nil field would turn
	// that into a panic in a handler instead of an explanation.
	Runtime *session.Manager

	// Events is the event log, read by the two timeline endpoints. It is
	// optional, like Terminal and for the same reason: a test of the REST
	// surface that is not about events should not have to build one. A server
	// without it explains itself on those two routes.
	//
	// It is a *Service and not a Repository, because the HTTP layer talks to
	// services and never to storage.
	Events *event.Service

	// Tasks is the task and agent session model. It is optional, like Events
	// and Terminal and for the same reason: a test of the REST surface that is
	// not about tasks should not have to build one. A server without it
	// explains itself on those seven routes.
	//
	// It is a *Service and not a Repository for the same reason Events is: no
	// handler in this package sees a database handle.
	Tasks *task.Service

	// Agent resolves the coding agent this server would launch, for the
	// capability report. It is optional: a server with no agent configured
	// reports that it has none, which is a legitimate configuration.
	Agent AgentResolver

	// Agents is the coordinator that starts a coding agent inside a runtime,
	// observes it and records the attempt. It is the thing the two agent
	// endpoints act through.
	//
	// It is optional in the same way Events and Tasks are - a test of the REST
	// surface that is not about agents should not have to build one - and a
	// server without it explains itself on those two routes rather than
	// launching an unobserved agent. There is deliberately no fallback that
	// starts the agent without it: an agent that is running and not observed
	// looks exactly like one that is running and has nothing to say, and that
	// is the confusion this phase exists to remove.
	Agents *agent.Service

	// Terminal carries browser sockets to the runtime. It is optional only so
	// that tests of the REST surface do not have to build one; a server
	// without it answers the real-time endpoint with an explanation rather
	// than a panic.
	Terminal *terminal.Hub

	// Logger receives request and error records. Nil means slog.Default.
	Logger *slog.Logger

	// StartedAt is when the process started. Zero means time.Now.
	StartedAt time.Time

	// WebDir is the absolute path of the built frontend. Empty disables
	// static serving.
	WebDir string

	// Now supplies the current time. Nil means time.Now.
	Now func() time.Time
}

// New builds the API server and its routes.
func New(o Options) (*Server, error) {
	switch {
	case o.Config == nil:
		return nil, errors.New("httpapi: Config is required")
	case o.Host == nil:
		return nil, errors.New("httpapi: Host adapter is required")
	case o.Projects == nil:
		return nil, errors.New("httpapi: project Service is required")
	case o.Discoverer == nil:
		return nil, errors.New("httpapi: Discoverer is required")
	case o.Runtime == nil:
		return nil, errors.New("httpapi: runtime Manager is required")
	}

	s := &Server{
		cfg:        o.Config,
		host:       o.Host,
		projects:   o.Projects,
		discoverer: o.Discoverer,
		runtime:    o.Runtime,
		events:     o.Events,
		tasks:      o.Tasks,
		agent:      o.Agent,
		agents:     o.Agents,
		terminal:   o.Terminal,
		log:        o.Logger,
		startedAt:  o.StartedAt,
		webDir:     o.WebDir,
		now:        o.Now,
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.startedAt.IsZero() {
		s.startedAt = s.now()
	}
	s.handler = s.routes()
	return s, nil
}

// Handler returns the fully wired HTTP handler.
func (s *Server) Handler() http.Handler { return s.handler }

// routes builds the routing table.
func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/server", s.handleServerInfo)

	// The probe a supervisor, a load balancer or a person with curl asks. It is
	// deliberately outside /api: it is not part of the product's API surface,
	// it is not versioned with it, and a monitor should not have to track the
	// protocol version to ask whether the process is alive.
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /api/projects", s.handleListProjects)
	mux.HandleFunc("GET /api/projects/discover", s.handleDiscoverProjects)
	mux.HandleFunc("POST /api/projects", s.handleCreateProject)
	mux.HandleFunc("POST /api/projects/register", s.handleRegisterProject)
	mux.HandleFunc("GET /api/projects/{id}", s.handleGetProject)

	// The one mutable field on a project: where it sits in the workspace. It is
	// a PATCH on the project rather than a separate resource because it is a
	// property of the project, and it is the only one this build lets a client
	// change - see handleUpdateProject for why that is the whole of the
	// endpoint.
	mux.HandleFunc("PATCH /api/projects/{id}", s.handleUpdateProject)

	// The runtime of one project. The operations are nested under the runtime
	// because that is the resource they act on: the session is what is started,
	// stopped, or removed, and a project is not.
	mux.HandleFunc("GET /api/projects/{id}/runtime", s.handleGetRuntime)
	mux.HandleFunc("POST /api/projects/{id}/runtime/start", s.handleStartRuntime)
	mux.HandleFunc("POST /api/projects/{id}/runtime/stop", s.handleStopRuntime)
	mux.HandleFunc("DELETE /api/projects/{id}/runtime", s.handleDestroyRuntime)

	// The coding agent inside a project's runtime. It is nested under the
	// runtime because that is what it lives in: there is no agent without a
	// terminal for it to run in, and starting one where there is none is
	// refused rather than queued.
	mux.HandleFunc("GET /api/projects/{id}/runtime/agent", s.handleGetAgent)
	mux.HandleFunc("POST /api/projects/{id}/runtime/agent/start", s.handleStartAgent)
	mux.HandleFunc("POST /api/projects/{id}/runtime/agent/stop", s.handleStopAgent)

	// What happened, as opposed to what is true now. The runtime resource above
	// answers the second question; these two answer the first, and the two are
	// deliberately separate resources because they are separate kinds of fact -
	// see docs/AGENT_EVENTS.md §1.
	//
	// There is no POST here, and no events route on a runtime's sub-resources: a
	// client reads a history, and never writes one.
	mux.HandleFunc("GET /api/projects/{id}/events", s.handleListProjectEvents)
	mux.HandleFunc("GET /api/runtime/{id}/events", s.handleListRuntimeEvents)

	// The work somebody wants done, and the attempts made at it.
	//
	// A task is created under its project and is then addressed by its own id,
	// because the id is what outlives the relationship: a task belongs to one
	// project for its whole life, while the things a caller does with it - move
	// its status, record an attempt - are about the task.
	//
	// An attempt is nested under its task, and is then also addressed by its own
	// id. Nothing here deletes either one: there is no DELETE, and its absence
	// is the design rather than a gap. See internal/httpapi/tasks.go.
	//
	// The status is changed with a PATCH rather than a POST to a /status
	// sub-resource, because a status is a property of a task and not a resource
	// of its own.
	mux.HandleFunc("POST /api/projects/{id}/tasks", s.handleCreateTask)
	mux.HandleFunc("GET /api/projects/{id}/tasks", s.handleListTasks)
	mux.HandleFunc("GET /api/tasks/{id}", s.handleGetTask)
	mux.HandleFunc("PATCH /api/tasks/{id}", s.handleUpdateTask)
	mux.HandleFunc("POST /api/tasks/{id}/sessions", s.handleCreateSession)
	mux.HandleFunc("GET /api/tasks/{id}/sessions", s.handleListSessions)
	mux.HandleFunc("GET /api/sessions/{id}", s.handleGetSession)

	// One endpoint, two things it can change: the attempt's status, and the
	// runtime it ran in. They share a request because they are the two facts
	// that are known at the moment an attempt is under way, and a client that
	// has just started a runtime for a session would otherwise make two calls
	// and have to decide their order itself.
	mux.HandleFunc("PATCH /api/sessions/{id}", s.handleUpdateSession)

	// The one real-time endpoint. One socket per browser, carrying every
	// project's terminal; see internal/terminal and docs/PROTOCOL.md.
	mux.HandleFunc("GET /api/ws", s.handleWebSocket)

	// Anything else under /api is an API error, not a page. Without this the
	// SPA fallback would answer a mistyped endpoint with index.html and a
	// confusing 200.
	mux.HandleFunc("/api/", s.handleUnknownAPI)

	mux.Handle("/", s.staticHandler())

	return s.withRecovery(s.withRequestLog(s.withCORS(mux)))
}

// handleUnknownAPI answers an unrouted API path.
func (s *Server) handleUnknownAPI(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotFound, CodeNotFound,
		fmt.Sprintf("no API endpoint matches %s %s", r.Method, r.URL.Path), nil)
}

// withRecovery turns a panic into a 500 instead of a dropped connection.
func (s *Server) withRecovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recovered := recover(); recovered != nil {
				// The stack is valuable; the request body is not logged, so a
				// credential in a body cannot reach the log here.
				s.log.Error("panic while handling request",
					"method", r.Method,
					"path", r.URL.Path,
					"panic", fmt.Sprint(recovered),
				)
				writeError(w, http.StatusInternalServerError, CodeInternal,
					"the server encountered an unexpected error", nil)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// statusRecorder captures the response status for logging.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}

// Hijack lets a handler take the connection over, which is what a WebSocket
// upgrade does.
//
// It has to be written out even though nothing in this file hijacks anything,
// and the reason is a property of Go's embedding that is easy to be caught by:
// a struct that embeds an interface gets only the methods that interface
// declares. http.ResponseWriter has three, Hijack is not one of them, so
// *statusRecorder does not implement http.Hijacker even though the writer it
// wraps does. A WebSocket upgrade behind this wrapper therefore fails with
// "response does not implement http.Hijacker" and answers 500 - a working
// endpoint and an error status at the same time, which is the kind of thing
// that costs an afternoon to find.
func (r *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := r.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("httpapi: %T does not support hijacking", r.ResponseWriter)
	}
	conn, buf, err := hijacker.Hijack()
	if err == nil && r.status == 0 {
		// The request became a socket. Recording the switch is what keeps the
		// request log honest: the alternative is a 200 for a request that was
		// never answered as HTTP, and the duration that follows is the whole
		// life of the connection, which is correct rather than a defect.
		r.status = http.StatusSwitchingProtocols
	}
	return conn, buf, err
}

// Flush forwards a flush, for a handler that streams a response.
func (r *statusRecorder) Flush() {
	if flusher, ok := r.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

// withRequestLog records method, route, status, and duration.
//
// Bodies and headers are deliberately absent: an Authorization header or a
// request body could carry a credential, and a log is the easiest place for
// one to leak.
func (s *Server) withRequestLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := s.now()
		recorder := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(recorder, r)

		status := recorder.status
		if status == 0 {
			status = http.StatusOK
		}
		level := slog.LevelInfo
		switch {
		case status >= 500:
			level = slog.LevelError
		case status >= 400:
			level = slog.LevelWarn
		}
		s.log.Log(r.Context(), level, "http request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", status,
			"bytes", recorder.bytes,
			"durationMs", s.now().Sub(started).Milliseconds(),
		)
	})
}

// withCORS permits loopback browser origins, plus anything explicitly listed
// in the configuration, and refuses cross-origin requests that would change
// something.
//
// The development setup proxies /api through Vite and needs no CORS at all;
// this exists so that running the frontend on its own dev port also works.
//
// Two different things are being defended here, and only one of them is CORS.
//
// CORS protects the *reply*: withholding Access-Control-Allow-Origin stops a
// page on another origin from reading what this server said. That is all the
// header alone can do, and it is not enough, because the browser still sends
// the request and the server still acts on it. A cross-origin POST with a
// simple content type - text/plain, say, carrying a JSON body - needs no
// preflight and is delivered whatever this middleware sets, so a page the user
// happened to open could register a project, start a runtime or start an agent
// in one, and simply not be able to read the answer. That was measured against
// a running installation, not reasoned about: it answered 201.
//
// The origin check below protects the *effect*. It is not a second policy: it
// is the same browserOriginAllowed the terminal socket already applies at its
// own upgrade, asked here as well, because a browser can reach this server in
// two ways and a check made at one of them is a check with a way around it. A
// request that carries no Origin is not from a browser and is unaffected, which
// is what keeps curl, the tests and the recovery script working.
//
// A read from another origin is still answered, and still not readable by the
// caller. There is no effect to protect, and refusing it would break a listed
// origin for no gain.
func (s *Server) withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && s.originAllowed(origin) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Add("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
			w.Header().Set("Access-Control-Max-Age", "600")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if changesSomething(r.Method) && !s.browserOriginAllowed(r) {
			s.log.Warn("refused a request that would change something from an unpermitted origin",
				"origin", origin, "method", r.Method, "path", r.URL.Path, "remote", r.RemoteAddr)
			writeError(w, http.StatusForbidden, CodeForbidden,
				"this origin may not change anything on this server", nil)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// changesSomething reports whether a method is one a cross-origin page can send
// without asking the browser first. GET, HEAD and OPTIONS are the methods that
// need no preflight, and they are also the methods that must have no effect;
// everything else is treated as a change and has to say where it came from.
func changesSomething(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	default:
		return true
	}
}

func (s *Server) originAllowed(origin string) bool {
	for _, allowed := range s.cfg.Server.AllowedOrigins {
		if strings.EqualFold(strings.TrimSpace(allowed), origin) {
			return true
		}
	}
	parsed, err := url.Parse(origin)
	if err != nil {
		return false
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return false
	}
	name := parsed.Hostname()
	if name == "localhost" || name == "::1" {
		return true
	}
	if ip := net.ParseIP(name); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// staticHandler serves the built frontend, or explains how to build it.
func (s *Server) staticHandler() http.Handler {
	webDir := strings.TrimSpace(s.webDir)
	if webDir == "" {
		return http.HandlerFunc(s.handleNoFrontend)
	}
	info, err := os.Stat(webDir)
	if err != nil || !info.IsDir() {
		s.log.Warn("frontend directory is missing; the API will run without a UI", "webDir", webDir)
		return http.HandlerFunc(s.handleNoFrontend)
	}

	fsys := os.DirFS(webDir)
	files := http.FileServer(http.FS(fsys))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			writeError(w, http.StatusMethodNotAllowed, CodeNotFound,
				"only GET is supported here", nil)
			return
		}
		cleaned := path.Clean("/" + r.URL.Path)
		if hasHiddenSegment(cleaned) {
			http.NotFound(w, r)
			return
		}
		if cleaned != "/" {
			if info, err := fs.Stat(fsys, strings.TrimPrefix(cleaned, "/")); err == nil && !info.IsDir() {
				files.ServeHTTP(w, r)
				return
			}
		}
		// Single-page app: unknown routes fall back to the entry point.
		index, err := fs.ReadFile(fsys, "index.html")
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(index)
	})
}

// handleNoFrontend explains the state of the server when there is no UI to
// serve, instead of returning a bare 404 that looks like a bug.
func (s *Server) handleNoFrontend(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		s.handleUnknownAPI(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
	fmt.Fprintf(w, `%s %s is running, but no frontend build was found.

Expected frontend directory: %s

Build it with:
    npm --prefix web install
    npm --prefix web run build

The API itself is available at /api/server
`, version.AppName, version.Version, s.cfg.Web.Dir)
}

// hasHiddenSegment reports whether any path segment starts with a dot, which
// would expose editor and VCS metadata if it happened to be in the build
// output.
func hasHiddenSegment(cleaned string) bool {
	for _, segment := range strings.Split(strings.Trim(cleaned, "/"), "/") {
		if strings.HasPrefix(segment, ".") && segment != "." {
			return true
		}
	}
	return false
}

// errEmptyBody is what decodeJSON returns for a request with no body.
//
// It is a named error rather than a message so that a handler can decide
// whether an empty body is a mistake or an ordinary request. For most endpoints
// it is a mistake - a POST that creates something out of fields needs the
// fields - and for a few it is not: creating an attempt at a task names
// everything it needs in the path, and demanding a body would be demanding
// punctuation.
var errEmptyBody = errors.New("request body is empty")

// decodeJSON reads a JSON body into dst.
//
// Unknown fields are rejected: AgentMux has exactly one client, and a typo
// such as "initGti" should fail loudly rather than silently doing nothing.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)

	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(dst); err != nil {
		var maxBytes *http.MaxBytesError
		var syntaxErr *json.SyntaxError
		var typeErr *json.UnmarshalTypeError
		switch {
		case errors.As(err, &maxBytes):
			return fmt.Errorf("request body must be at most %d bytes", maxBytes.Limit)
		case errors.As(err, &syntaxErr):
			return fmt.Errorf("request body is not valid JSON at byte %d", syntaxErr.Offset)
		case errors.As(err, &typeErr):
			return fmt.Errorf("field %q has the wrong type", typeErr.Field)
		case errors.Is(err, io.EOF):
			return errEmptyBody
		default:
			return errors.New(strings.TrimPrefix(err.Error(), "json: "))
		}
	}
	// A second value in the stream means the client sent something unexpected.
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("request body must contain a single JSON object")
	}
	return nil
}

// contextWithTimeout bounds a handler's work.
func (s *Server) contextWithTimeout(r *http.Request, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), d)
}

// Package httpapi exposes the AgentMux REST surface.
//
// Handlers stay thin: they decode a request, call a service, and encode the
// result. Every rule about projects lives in the project package, and every
// statement about storage lives in the storage package. A handler that starts
// making decisions is a handler that has stopped being a handler.
package httpapi

import (
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

	"github.com/kutonlagos/agentmux/internal/claude"
	"github.com/kutonlagos/agentmux/internal/config"
	"github.com/kutonlagos/agentmux/internal/host"
	"github.com/kutonlagos/agentmux/internal/project"
	"github.com/kutonlagos/agentmux/internal/session"
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
	agent      AgentResolver
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

	// Agent resolves the coding agent this server would launch, for the
	// capability report. It is optional: a server with no agent configured
	// reports that it has none, which is a legitimate configuration.
	Agent AgentResolver

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
		agent:      o.Agent,
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
	mux.HandleFunc("GET /api/projects", s.handleListProjects)
	mux.HandleFunc("GET /api/projects/discover", s.handleDiscoverProjects)
	mux.HandleFunc("POST /api/projects", s.handleCreateProject)
	mux.HandleFunc("POST /api/projects/register", s.handleRegisterProject)
	mux.HandleFunc("GET /api/projects/{id}", s.handleGetProject)

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

	s.registerDebugRoutes(mux)

	// Anything else under /api is an API error, not a page. Without this the
	// SPA fallback would answer a mistyped endpoint with index.html and a
	// confusing 200.
	mux.HandleFunc("/api/", s.handleUnknownAPI)

	mux.Handle("/", s.staticHandler())

	return s.withRecovery(s.withRequestLog(s.withCORS(mux)))
}

// registerDebugRoutes adds the diagnostic endpoints when they are enabled.
//
// They are registered only when the configuration asks for them, so an
// ordinary installation does not have a route that accepts raw terminal input.
// Registering them and refusing inside the handler would leave the surface
// present and one flag away from being live; this way it does not exist.
func (s *Server) registerDebugRoutes(mux *http.ServeMux) {
	if s.cfg == nil || !s.cfg.Server.DebugAPI {
		return
	}
	s.log.Warn("the diagnostic runtime API is enabled",
		"note", "these endpoints expose raw terminal input and output and are not a product API")

	mux.HandleFunc("GET /api/debug/runtimes", s.handleDebugRuntimes)
	mux.HandleFunc("POST /api/debug/reconcile", s.handleDebugReconcile)
	mux.HandleFunc("GET /api/debug/projects/{id}/runtime/output", s.handleDebugRuntimeOutput)
	mux.HandleFunc("POST /api/debug/projects/{id}/runtime/input", s.handleDebugRuntimeInput)
	mux.HandleFunc("POST /api/debug/projects/{id}/runtime/resize", s.handleDebugRuntimeResize)
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
// in the configuration.
//
// The development setup proxies /api through Vite and needs no CORS at all;
// this exists so that running the frontend on its own dev port also works.
// Only loopback origins are allowed by default, so a page on the public
// internet cannot reach a local AgentMux through a browser.
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
		next.ServeHTTP(w, r)
	})
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
			return errors.New("request body is empty")
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

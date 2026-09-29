package httpapi

import (
	"fmt"
	"net/http"
	"time"

	"github.com/kutonlagos/agentmux/internal/claude"
	"github.com/kutonlagos/agentmux/internal/project"
)

// Project launch settings.
//
// Two endpoints, and both of them are about the same small thing: how much
// Claude is asked before it acts, for one project. It is a resource of its own
// rather than two more fields on PATCH /api/projects/{id} because it is stored
// in its own table and read by a different caller - the launch path reads one
// project's mode and nothing else about the project at all.
//
// # What these endpoints deliberately do not do
//
// There is no endpoint here that changes the mode of an agent that is already
// running. Claude reads its permission mode once, from its own command line,
// and the only thing that can change it afterwards is Shift+Tab inside its TUI.
// Sending that keystroke from a menu is a keystroke on somebody's behalf into
// work that is in progress, which docs/TERMINAL_CONTROLLER.md §9 refuses and
// §2 of the phase brief rules out for this one. So a mode stored here reaches
// the project's *next* launch, and the console says so in as many words.
//
// There is also nothing here that answers a permission prompt, approves a tool
// call, or reads what Claude is doing. A mode decides how often a person is
// asked; it never answers for them.

// settingsResponse is the body of both settings endpoints.
//
// It is a named top-level object wrapping the resource, like every other
// response in this API. A bare object would be the only endpoint whose body
// could not grow a field without changing what a client already reads.
type settingsResponse struct {
	Settings settingsView `json:"settings"`
}

// settingsView is one project's launch configuration as a client sees it.
//
// It names its fields rather than serialising project.Settings directly, for
// the reason every view in this API does: what crosses to a client is a
// decision. The model's timestamps are deliberately not here - nobody acts on
// when a mode was chosen, and a field a client must be told about and then
// ignore is a field that should not have been sent.
type settingsView struct {
	// PermissionMode is one of the modes named by claude.PermissionModes, and
	// it is what the project's next launch will pass as `--permission-mode`.
	PermissionMode string `json:"permissionMode"`
}

// settingsViewOf renders a project's settings for a client.
func settingsViewOf(s project.Settings) settingsView {
	return settingsView{PermissionMode: string(s.PermissionMode)}
}

// handleGetProjectSettings implements GET /api/projects/{id}/settings.
//
// A project that exists and has never been configured is answered with the
// default rather than with an empty object or a 404. Those are different facts
// and a client that had to tell them apart would be a client that knew what the
// default is - which is the server's to decide, and which is the whole reason
// the answer is a value rather than an absence.
func (s *Server) handleGetProjectSettings(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := s.contextWithTimeout(r, settingsTimeout)
	defer cancel()

	settings, err := s.projects.Settings(ctx, r.PathValue("id"))
	if err != nil {
		writeServiceError(w, s.log, err)
		return
	}
	writeJSON(w, s.log, http.StatusOK, settingsResponse{Settings: settingsViewOf(settings)})
}

// updateProjectSettingsRequest is the body of PATCH /api/projects/{id}/settings.
type updateProjectSettingsRequest struct {
	// PermissionMode is the mode the project's next launch will start in.
	//
	// It is an optionalString rather than a plain string even though it is the
	// only field, so that "not sent" and "sent as null" are told apart. There
	// is no mode that means "unset": every project has one, and a project that
	// has never been configured has `manual`. A request asking to clear the
	// field is therefore asking for something this resource cannot express, and
	// saying so is a better answer than storing the absence as a value.
	PermissionMode optionalString `json:"permissionMode"`
}

// handleUpdateProjectSettings implements PATCH /api/projects/{id}/settings.
//
// The value is checked twice before it is stored and neither check is
// redundant: this one turns a malformed request into a 400 with the vocabulary
// in it, and project.Service.SetPermissionMode refuses a well-formed request
// for a mode this build does not offer. Neither is the check that makes the
// command line safe - claude.Quote is, by rendering every argument as one
// quoted shell word - but together they are what keeps a value that is not a
// mode from ever reaching a database, let alone a shell.
func (s *Server) handleUpdateProjectSettings(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := s.contextWithTimeout(r, settingsTimeout)
	defer cancel()

	var req updateProjectSettingsRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, err.Error(), nil)
		return
	}
	if !req.PermissionMode.Present || req.PermissionMode.Value == nil {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest,
			fmt.Sprintf("the request must set permissionMode, to one of %s",
				claude.PermissionModeNames()), nil)
		return
	}

	settings, err := s.projects.SetPermissionMode(ctx, r.PathValue("id"),
		claude.PermissionMode(*req.PermissionMode.Value))
	if err != nil {
		writeServiceError(w, s.log, err)
		return
	}
	writeJSON(w, s.log, http.StatusOK, settingsResponse{Settings: settingsViewOf(settings)})
}

// settingsTimeout bounds both settings requests.
//
// They are two statements against a local SQLite file and nothing else - no
// process, no filesystem, no network - so the budget is a guard against a
// wedged database rather than an allowance for slow work. It matches the read
// timeout rather than a start's: choosing a mode starts nothing.
const settingsTimeout = 10 * time.Second

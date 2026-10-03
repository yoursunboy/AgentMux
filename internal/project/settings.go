package project

import (
	"context"
	"errors"
	"time"

	"github.com/kutonlagos/agentmux/internal/claude"
)

// ErrSettingsNotFound is returned by a SettingsRepository when a project has no
// settings row.
//
// It is a sentinel rather than an empty value for the reason every sentinel in
// this package is one: a project nobody has configured is a real and ordinary
// state, and only the service knows that it means "the default" rather than
// "nothing". Storage reports what it found; the decision is made here.
var ErrSettingsNotFound = errors.New("project settings not found")

// DefaultPermissionMode is the mode a project launches under until somebody
// configures it.
//
// It is `bypassPermissions`, and that is a product decision rather than the
// CLI's own default. Claude asks before it acts unless told otherwise, which is
// the right answer for a person typing at a terminal and the wrong one for a
// workbench whose whole purpose is to run agents while nobody is watching: a
// session that stops on a prompt at midnight has done nothing by morning.
//
// # Why it is a constant here and not a field anywhere
//
// Nothing stores this. A project has a settings row only once somebody has
// chosen a mode, so changing this value changes what every *unconfigured*
// project launches under and reaches no project that has a row - which is what
// makes it a safe thing to change and the reason the table is shaped this way.
// docs/PERMISSION_MODE.md §5 is the long form.
//
// `manual` and `acceptEdits` remain offered. They are what a person who wants
// to be asked chooses, and this default is not a claim that asking is wrong -
// it is which of the three a project nobody has decided about gets.
const DefaultPermissionMode = claude.PermissionBypass

// Settings is one project's launch configuration.
//
// It is what the project's *next* launch will be told. It is not a record of
// what any launch was told: Claude reads its permission mode once, from its own
// command line, so a mode stored after an agent started has not reached that
// agent and nothing here claims it did. docs/PERMISSION_MODE.md §4 is the long
// form, and it is why the console says "Restart Agent to apply" rather than
// showing the change as though it had taken effect.
type Settings struct {
	// ProjectID is the project this configures.
	ProjectID string

	// PermissionMode is what the next launch passes to Claude as
	// `--permission-mode`.
	PermissionMode claude.PermissionMode

	// CreatedAt is when the project was first configured, and UpdatedAt when
	// the row was last written.
	//
	// Both are zero for a project that has never been configured, which is the
	// honest value: there is no time at which nothing happened. Neither is
	// serialised - the API reports the mode and nothing else, because a
	// timestamp nobody acts on is a field a client has to be told about and
	// then ignore.
	CreatedAt time.Time
	UpdatedAt time.Time
}

// SettingsRepository is the persistence contract for project settings.
//
// It is a second, narrower interface rather than two more methods on
// Repository, for the reason Repository itself is declared here at all: a
// caller states what it needs. The launch path reads one project's mode and
// nothing else about a project, and a caller that only does that should not
// depend on the ability to create and archive one.
type SettingsRepository interface {
	// GetSettings returns a project's settings, or ErrSettingsNotFound.
	GetSettings(ctx context.Context, projectID string) (Settings, error)

	// SettingsByProjects returns the settings of each of the given projects.
	// A project with no settings is absent from the result rather than present
	// with a zero value.
	//
	// It is a listing rather than a loop over GetSettings because the console
	// reads one of these per card, and a card per query is the N+1
	// docs/CONTROLLER_API.md §13 forbids.
	SettingsByProjects(ctx context.Context, projectIDs []string) ([]Settings, error)

	// SaveSettings stores a project's settings, replacing any row already
	// there.
	//
	// It is an upsert rather than a create-or-update pair because the caller
	// should not have to know whether this is the first time a project has been
	// configured. There is exactly one row per project, so there is nothing an
	// insert-only path would be protecting.
	SaveSettings(ctx context.Context, s Settings) error
}

// PermissionModesForProjects returns the permission mode of every named
// project, including the projects nobody has configured.
//
// The map has an entry for every id it was given. The default is applied here
// rather than by the caller for the reason it is applied anywhere: what a
// project nobody has configured launches under is this package's answer to
// give, and a caller that had to supply it would be a caller that had to know
// it - in a second place, which is the thing that drifts.
//
// It does not check that the projects exist. It is for a caller that has
// already listed them, and listing them again to check the first listing would
// be the extra query docs/CONTROLLER_API.md §13 exists to forbid. An id that
// names nothing comes back with the default, which costs nothing because
// nothing draws a card for it.
func (s *Service) PermissionModesForProjects(ctx context.Context, ids []string) (map[string]claude.PermissionMode, error) {
	modes := make(map[string]claude.PermissionMode, len(ids))
	for _, id := range ids {
		modes[id] = DefaultPermissionMode
	}
	settings, err := s.settings.SettingsByProjects(ctx, ids)
	if err != nil {
		return nil, err
	}
	for _, one := range settings {
		modes[one.ProjectID] = one.PermissionMode
	}
	return modes, nil
}

// Settings returns a project's launch configuration.
//
// A project nobody has configured is reported with the default mode rather than
// as an error, because it is not an error - it is the state every project
// starts in. The row is not created by reading: a project nobody configures
// never gets one.
//
// An identifier that names no project is a not-found, which is what makes one
// question have one answer whether it is asked of a project that does not exist
// or of a mode that does not exist.
func (s *Service) Settings(ctx context.Context, id string) (Settings, error) {
	if !ValidID(id) {
		return Settings{}, newError(CodeNotFound, "no project with id %q", id)
	}
	if _, err := s.repo.GetByID(ctx, id); err != nil {
		return Settings{}, mapNotFound(err, id)
	}
	return s.settingsFor(ctx, id)
}

// SetPermissionMode stores the mode this project's next launch will start in.
//
// # What it does not do
//
// It changes nothing about an agent that is already running. Making it take
// effect would mean typing Shift+Tab into somebody's terminal - a keystroke
// sent on their behalf, into work that is in progress, from a menu that said it
// was a setting. Choosing how much to be asked and answering a question already
// asked are different acts, and this endpoint only does the first. The running
// process keeps the mode it started with, and the console says so.
//
// # Why the value is checked here
//
// This is where the vocabulary is enforced, and it is the only place. The
// column is TEXT without a constraint and the flag is rendered without a second
// check, both deliberately: one list in one place cannot drift from itself.
func (s *Service) SetPermissionMode(ctx context.Context, id string, mode claude.PermissionMode) (Settings, error) {
	if !ValidID(id) {
		return Settings{}, newError(CodeNotFound, "no project with id %q", id)
	}
	if !claude.ValidPermissionMode(mode) {
		return Settings{}, newError(CodeInvalidInput,
			"%q is not a Claude permission mode; expected one of %s",
			mode, claude.PermissionModeNames()).
			withDetail("field", "permissionMode")
	}
	if _, err := s.repo.GetByID(ctx, id); err != nil {
		return Settings{}, mapNotFound(err, id)
	}

	now := s.now().UTC()
	current, err := s.settingsFor(ctx, id)
	if err != nil {
		return Settings{}, err
	}
	current.PermissionMode = mode
	current.UpdatedAt = now
	if current.CreatedAt.IsZero() {
		current.CreatedAt = now
	}
	if err := s.settings.SaveSettings(ctx, current); err != nil {
		return Settings{}, err
	}
	s.log.Info("permission mode changed",
		"projectId", id, "permissionMode", string(mode),
		"note", "applies to the next launch")
	return current, nil
}

// settingsFor reads a project's stored settings, or returns the default when
// there is no row.
//
// It assumes the project exists, which every caller has already established.
func (s *Service) settingsFor(ctx context.Context, id string) (Settings, error) {
	stored, err := s.settings.GetSettings(ctx, id)
	if errors.Is(err, ErrSettingsNotFound) {
		return Settings{ProjectID: id, PermissionMode: DefaultPermissionMode}, nil
	}
	if err != nil {
		return Settings{}, err
	}
	return stored, nil
}

package httpapi

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/kutonlagos/agentmux/internal/attention"
	"github.com/kutonlagos/agentmux/internal/claude"
	"github.com/kutonlagos/agentmux/internal/controller"
	"github.com/kutonlagos/agentmux/internal/task"
)

// These tests are about the console's two action routes: one queue across every
// project, and one action by id.
//
// The rows are produced by the real chain wherever a test can reach it. The
// chain that fails produces a VIEW_FAILURE without a Claude, which covers the
// notices half; the permission half is produced by delivering the same hook
// event a real Claude delivers, to the receiver the product itself configured.

// ---------------------------------------------------------------------------
// GET /api/actions
// ---------------------------------------------------------------------------

// TestTheQueueAcrossEveryProject is the page the Action Center is built on.
//
// Two projects, each with an attempt that failed, and one request that answers
// for both. The route is flat rather than nested under a project because this is
// the question a project-scoped route cannot be asked: which agent needs me,
// across everything.
func TestTheQueueAcrossEveryProject(t *testing.T) {
	h := newHarnessOpts(t, harnessOptions{agent: pinnedAgent{spec: testAgentSpec}, runtimeAvailable: true})
	first, _ := h.registerProject(t, "checkout-service")
	second, _ := h.registerProject(t, "invoice-parser")
	h.startAnAttempt(t, first)
	h.startAnAttempt(t, second)

	recorder := h.call(http.MethodGet, "/api/actions", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d; body was %s", recorder.Code, recorder.Body)
	}
	queue := decode[queueResponse](t, recorder)

	if queue.Count != 2 {
		t.Fatalf("count = %d; want 2 - one failed attempt in each project: %+v", queue.Count, queue.Actions)
	}
	if queue.Actions == nil {
		t.Error("actions = null; want an empty list rather than null, so a client renders one shape")
	}

	// Both projects are named. The name is resolved by the controller's join,
	// which is the whole reason this route reads through the aggregation.
	want := map[string]bool{"checkout-service": false, "invoice-parser": false}
	for _, a := range queue.Actions {
		if _, ok := want[a.ProjectName]; !ok {
			t.Errorf("the queue named a project %q; want one of the two registered", a.ProjectName)
			continue
		}
		want[a.ProjectName] = true

		if a.Type != string(attention.ActionViewFailure) {
			t.Errorf("%s: type = %s; want VIEW_FAILURE", a.ProjectName, a.Type)
		}
		if a.Level != string(attention.LevelWarning) {
			t.Errorf("%s: level = %s; want WARNING - the level is read from the type",
				a.ProjectName, a.Level)
		}
		if a.Status != string(attention.ActionPending) {
			t.Errorf("%s: status = %s; want PENDING", a.ProjectName, a.Status)
		}
		if a.Reason == "" {
			t.Errorf("%s: the action carries no reason", a.ProjectName)
		}
		if a.ResolvedAt != nil {
			t.Errorf("%s: a pending action carries a resolution time: %v", a.ProjectName, a.ResolvedAt)
		}
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("the queue does not carry %s's action", name)
		}
	}

	// A failed attempt is worth reading and stops nothing, so both counts say
	// the same thing: nothing needs a person, two things are worth a look.
	if queue.NeedsYou != 0 {
		t.Errorf("needsYou = %d; want 0 - a failed attempt is not something work stops for", queue.NeedsYou)
	}
	if queue.Notices != 2 {
		t.Errorf("notices = %d; want 2", queue.Notices)
	}
}

// TestAPermissionRequestIsWhatNeedsSomebody is the headline's positive case.
//
// It is the one action type that means "nothing progresses without you", and the
// level is read from the type rather than stored beside it. The event is
// delivered to the adapter's own receiver, at the address the settings document
// names, which is exactly the input a real Claude sends.
func TestAPermissionRequestIsWhatNeedsSomebody(t *testing.T) {
	h := newHarnessOpts(t, harnessOptions{agent: pinnedAgent{spec: testAgentSpec}, runtimeAvailable: true})
	projectID, _ := h.registerProject(t, "checkout-service")
	attempt := h.startAnAttempt(t, projectID)

	// The attempt failed at the launch, but it was bound to its runtime first,
	// so an agent event on that runtime is attributed to it - the same
	// attribution a live Claude's hook gets.
	runtimeID := boundRuntime(t, attempt)
	h.record(t, projectID, runtimeID, claude.TypeAgentPermissionRequested,
		permissionHookPayload)

	recorder := h.call(http.MethodGet, "/api/actions", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d; body was %s", recorder.Code, recorder.Body)
	}
	queue := decode[queueResponse](t, recorder)

	var permission *controller.ActionItem
	for i := range queue.Actions {
		if queue.Actions[i].Type == string(attention.ActionPermissionRequest) {
			permission = &queue.Actions[i]
		}
	}
	if permission == nil {
		t.Fatalf("the queue holds no permission request: %+v", queue.Actions)
	}
	if permission.Level != string(attention.LevelActionRequired) {
		t.Errorf("level = %s; want ACTION_REQUIRED", permission.Level)
	}
	if permission.Status != string(attention.ActionPending) {
		t.Errorf("status = %s; want PENDING", permission.Status)
	}
	if permission.Reason != attention.ReasonPermissionRequested {
		t.Errorf("reason = %q; want %q - the fixed phrase, never anything Claude sent",
			permission.Reason, attention.ReasonPermissionRequested)
	}
	if queue.NeedsYou != 1 {
		t.Errorf("needsYou = %d; want 1 - a permission request is what needs somebody", queue.NeedsYou)
	}
}

// TestTheQueuePutsWhatNeedsSomebodyFirst is the ordering, at the route.
//
// The permission request is raised after the failure, so it is also the newer
// row - which means the ordering has to be sorting on status for this to come
// out right, not merely happening to agree with the clock.
func TestTheQueuePutsWhatNeedsSomebodyFirst(t *testing.T) {
	h := newHarnessOpts(t, harnessOptions{agent: pinnedAgent{spec: testAgentSpec}, runtimeAvailable: true})
	projectID, _ := h.registerProject(t, "checkout-service")
	attempt := h.startAnAttempt(t, projectID)

	runtimeID := boundRuntime(t, attempt)
	h.record(t, projectID, runtimeID, claude.TypeAgentPermissionRequested,
		permissionHookPayload)

	queue := decode[queueResponse](t, h.call(http.MethodGet, "/api/actions", ""))
	if queue.Count < 2 {
		t.Fatalf("the queue holds %d action(s); want the failure and the request", queue.Count)
	}
	if queue.Actions[0].Type != string(attention.ActionPermissionRequest) {
		t.Errorf("the queue leads with %s; want the permission request - pending first, by level",
			queue.Actions[0].Type)
	}
}

// TestTheQueueClampsAndRejectsLimits is the limit, at the route.
func TestTheQueueClampsAndRejectsLimits(t *testing.T) {
	h := newHarnessOpts(t, harnessOptions{agent: pinnedAgent{spec: testAgentSpec}, runtimeAvailable: true})
	projectID, _ := h.registerProject(t, "checkout-service")
	h.startAnAttempt(t, projectID)

	h.wantError(t, h.call(http.MethodGet, "/api/actions?limit=0", ""),
		http.StatusBadRequest, CodeInvalidRequest)

	// Above the ceiling is clamped rather than refused: a client asking for more
	// than this server will give is answered, not told off.
	recorder := h.call(http.MethodGet, "/api/actions?limit=1000", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("limit=1000 answered %d; want it clamped rather than refused; body was %s",
			recorder.Code, recorder.Body)
	}
	if queue := decode[queueResponse](t, recorder); queue.Count != 1 {
		t.Errorf("count = %d; want 1", queue.Count)
	}
}

// TestTheQueueIsUnavailableWithoutTheProjection is a server built without the
// attention projection.
//
// Such a server has an aggregation and no queue behind it, so the answer comes
// from the aggregation rather than from a route guard: `controller_unavailable`,
// the code the controller uses for a queue it cannot read. A server with no
// aggregation at all answers 503 from requireController, which is a different
// fact - the console's route is missing rather than its data.
func TestTheQueueIsUnavailableWithoutTheProjection(t *testing.T) {
	h := newHarnessOpts(t, harnessOptions{withoutAttention: true})

	h.wantError(t, h.call(http.MethodGet, "/api/actions", ""),
		http.StatusInternalServerError, controller.CodeUnavailable)
	h.wantError(t, h.call(http.MethodGet, "/api/actions/act_00000000000000000000000000000000", ""),
		http.StatusInternalServerError, controller.CodeUnavailable)
}

// boundRuntime returns the runtime an attempt was bound to, which is what an
// agent event has to name for the projection to attribute it.
//
// The binding is what makes a hook delivery reach an attempt at all: subjectOf
// resolves an agent event's runtime back to the attempt through the state table,
// and skips the event when nothing is bound - docs/AGENT_ATTENTION.md §5. So an
// attempt with no runtime is not a test that would pass for the wrong reason, it
// is a test that could not be asked, and it says so here rather than reporting a
// missing action three screens later.
func boundRuntime(t *testing.T, attempt *task.AgentSession) string {
	t.Helper()
	if attempt.RuntimeID == "" {
		t.Fatal("the attempt was not bound to a runtime, so no agent event can be attributed to it")
	}
	return attempt.RuntimeID
}

// ---------------------------------------------------------------------------
// GET /api/actions/{id}
// ---------------------------------------------------------------------------

// TestReadingOneAction is the detail page's read.
//
// The id comes from the queue rather than being invented, so this is the whole
// path a client takes: list, follow one, read it.
func TestReadingOneAction(t *testing.T) {
	h := newHarnessOpts(t, harnessOptions{agent: pinnedAgent{spec: testAgentSpec}, runtimeAvailable: true})
	projectID, _ := h.registerProject(t, "checkout-service")
	attempt := h.startAnAttempt(t, projectID)

	queue := decode[queueResponse](t, h.call(http.MethodGet, "/api/actions", ""))
	if queue.Count != 1 {
		t.Fatalf("the queue holds %d action(s); want 1", queue.Count)
	}
	fromQueue := queue.Actions[0]

	recorder := h.call(http.MethodGet, "/api/actions/"+fromQueue.ID, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d; body was %s", recorder.Code, recorder.Body)
	}
	action := decode[controller.ActionItem](t, recorder)

	// The one-row read says exactly what the listing said about the same action.
	if action.ID != fromQueue.ID {
		t.Errorf("id = %q; want %q", action.ID, fromQueue.ID)
	}
	if action.ProjectID != projectID || action.ProjectName != "checkout-service" {
		t.Errorf("project = %q/%q; want %q/checkout-service",
			action.ProjectID, action.ProjectName, projectID)
	}
	if action.AgentSessionID != attempt.ID {
		t.Errorf("agentSessionId = %q; want %q", action.AgentSessionID, attempt.ID)
	}
	if action.Type != fromQueue.Type || action.Level != fromQueue.Level || action.Status != fromQueue.Status {
		t.Errorf("the one-row read and the listing disagree:\n one     = %+v\n listing = %+v",
			action, fromQueue)
	}
}

// TestAnUnknownActionIsNotFound is the absence.
//
// A 404 rather than an empty object, because an empty object is the one answer a
// client cannot tell from an action whose fields happen to be empty.
func TestAnUnknownActionIsNotFound(t *testing.T) {
	h := newHarness(t)

	h.wantError(t,
		h.call(http.MethodGet, "/api/actions/act_00000000000000000000000000000000", ""),
		http.StatusNotFound, attention.CodeNotFound)
}

// TestAnActionIDThatIsNotOneIsNotFound is the same answer for a path segment
// that could never be an id.
//
// It is not a validation error: nothing was sent to validate. The queue holds no
// such action, and that is what the answer says - which also means the route
// needs no shape check of its own. A malformed id and an absent one are the same
// absence, and only the store can tell an id apart from a string that merely
// looks like one.
func TestAnActionIDThatIsNotOneIsNotFound(t *testing.T) {
	h := newHarness(t)

	h.wantError(t, h.call(http.MethodGet, "/api/actions/nonsense", ""),
		http.StatusNotFound, attention.CodeNotFound)
}

// ---------------------------------------------------------------------------
// What the queue must never carry
// ---------------------------------------------------------------------------

// permissionHookPayload is the stored shape of a permission request, as the
// adapter writes it.
//
// It is copied from hookPayloadOf rather than invented: the adapter reduces a
// Claude hook to named fields on purpose, keeping the tool name and dropping
// `prompt` and `tool_input`, and a fixture that carried the raw hook body would
// be testing an input this product never produces - internal/claude/hooks.go:116.
const permissionHookPayload = `{"event":"permission_request","tool":"Bash"}`

// permissionHookPayloadWithContent is a payload this product never writes.
//
// It carries what the adapter deliberately drops: the prompt, and a tool input
// with a command line and a credential in it. That is the point. The assertion
// this feeds is that an action response is built from named fields rather than
// from whatever the row happens to hold, and a fixture that was already clean
// could not tell the two apart.
const permissionHookPayloadWithContent = `{"event":"permission_request","tool":"Bash",` +
	`"prompt":"deploy the scratch build",` +
	`"tool_input":{"command":"deploy.sh --token sk-test-0000"}}`

// forbiddenInActionQueue are the words no action response may contain, anywhere
// in its bytes.
//
// They are the things a Claude request carries with it - a path, a token, an
// environment variable, the tool input itself - and the whole security position
// of this phase is that an action is a fixed phrase and an enumeration, never a
// copy of what was asked. A test over the encoded bytes is the one that survives
// a field being added to the model later.
var forbiddenInActionQueue = []string{
	"password",
	"token",
	"secret",
	"prompt",
	"tool_input",
	"toolInput",
	"transcript",
	"command",
	"apikey",
	"api_key",
}

// TestTheQueueNeverCarriesAForbiddenField is §十六's rule, checked on the wire.
func TestTheQueueNeverCarriesAForbiddenField(t *testing.T) {
	h := newHarnessOpts(t, harnessOptions{agent: pinnedAgent{spec: testAgentSpec}, runtimeAvailable: true})
	projectID, _ := h.registerProject(t, "checkout-service")
	attempt := h.startAnAttempt(t, projectID)

	h.record(t, projectID, boundRuntime(t, attempt), claude.TypeAgentPermissionRequested,
		permissionHookPayloadWithContent)

	listing := h.call(http.MethodGet, "/api/actions", "")
	if listing.Code != http.StatusOK {
		t.Fatalf("status = %d; body was %s", listing.Code, listing.Body)
	}

	queue := decode[queueResponse](t, listing)
	if queue.Count == 0 {
		t.Fatal("the queue is empty, so this test proves nothing")
	}

	one := h.call(http.MethodGet, "/api/actions/"+queue.Actions[0].ID, "")
	if one.Code != http.StatusOK {
		t.Fatalf("status = %d; body was %s", one.Code, one.Body)
	}

	for _, body := range []string{listing.Body.String(), one.Body.String()} {
		lowered := strings.ToLower(body)
		for _, word := range forbiddenInActionQueue {
			if strings.Contains(lowered, word) {
				t.Errorf("an action response carries %q, which a Claude request would have brought with it:\n%s",
					word, body)
			}
		}
	}
}

// TestTheQueueCarriesOnlyTheFieldsItDeclares is the same rule from the other
// side.
//
// The forbidden-word test catches a leak with the wrong word in it. This catches
// a field that was added to the model and reached the client without anybody
// deciding it should - which is how a leak usually happens.
func TestTheQueueCarriesOnlyTheFieldsItDeclares(t *testing.T) {
	h := newHarnessOpts(t, harnessOptions{agent: pinnedAgent{spec: testAgentSpec}, runtimeAvailable: true})
	projectID, _ := h.registerProject(t, "checkout-service")
	h.startAnAttempt(t, projectID)

	recorder := h.call(http.MethodGet, "/api/actions", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d; body was %s", recorder.Code, recorder.Body)
	}

	want := []string{
		"agentSessionId", "createdAt", "id", "level", "projectId", "projectName",
		"reason", "resolvedAt", "status", "type",
	}
	if got := sortedKeysOfFirstAction(t, recorder.Body.Bytes()); !sameStrings(got, want) {
		t.Errorf("an action carries %v;\n want exactly %v", got, want)
	}
}

// sortedKeysOfFirstAction reads the field names of the first action in a queue
// response, without decoding it into a type.
//
// Decoding into the model would assert that the model round-trips, which is not
// the question: a field added to the model and to the response together would
// pass. Reading the wire is what makes this a statement about what leaves the
// process.
func sortedKeysOfFirstAction(t *testing.T, body []byte) []string {
	t.Helper()

	var queue struct {
		Actions []map[string]json.RawMessage `json:"actions"`
	}
	if err := json.Unmarshal(body, &queue); err != nil {
		t.Fatalf("unmarshalling the queue: %v", err)
	}
	if len(queue.Actions) == 0 {
		t.Fatal("the queue is empty, so this test proves nothing")
	}

	keys := make([]string, 0, len(queue.Actions[0]))
	for key := range queue.Actions[0] {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// sameStrings reports whether two string slices hold the same strings in the
// same order.
func sameStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

package httpapi

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/kutonlagos/agentmux/internal/project"
)

// These tests cover the one field of a project a client may change: where it
// sits in the workspace.
//
// The endpoint is deliberately narrow - PATCH takes a pinned slot and nothing
// else - so what is worth testing is not only that pinning works, but that
// everything else is refused rather than quietly ignored.

// pin sends a PATCH and fails the test unless it succeeded.
func (h *harness) pin(id string, body string) *project.Project {
	h.t.Helper()
	recorder := h.call(http.MethodPatch, "/api/projects/"+id, body)
	if recorder.Code != http.StatusOK {
		h.t.Fatalf("PATCH %s returned status %d: %s", body, recorder.Code, recorder.Body.String())
	}
	return decode[projectResponse](h.t, recorder).Project
}

func TestPatchProjectPinsAWorkspaceSlot(t *testing.T) {
	h := newHarness(t)
	p := h.register("Accounting")

	pinned := h.pin(p.ID, `{"pinnedSlot":2}`)
	if pinned.PinnedSlot == nil || *pinned.PinnedSlot != 2 {
		t.Fatalf("PinnedSlot = %v, want 2", pinned.PinnedSlot)
	}

	// The write is stored: GET is a different question from the PATCH that
	// answered the first one.
	recorder := h.call(http.MethodGet, "/api/projects/"+p.ID, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET returned status %d", recorder.Code)
	}
	got := decode[projectResponse](h.t, recorder).Project
	if got.PinnedSlot == nil || *got.PinnedSlot != 2 {
		t.Errorf("GET returned PinnedSlot = %v, want 2", got.PinnedSlot)
	}
}

func TestPatchProjectClearsAWorkspaceSlot(t *testing.T) {
	h := newHarness(t)
	p := h.register("Accounting")

	h.pin(p.ID, `{"pinnedSlot":1}`)
	cleared := h.pin(p.ID, `{"pinnedSlot":null}`)
	if cleared.PinnedSlot != nil {
		t.Errorf("PinnedSlot = %d after clearing, want null", *cleared.PinnedSlot)
	}
}

func TestPatchProjectRefusesASlotOutOfRange(t *testing.T) {
	h := newHarness(t)
	p := h.register("Accounting")

	for _, body := range []string{`{"pinnedSlot":-1}`, `{"pinnedSlot":200}`} {
		recorder := h.call(http.MethodPatch, "/api/projects/"+p.ID, body)
		body := h.wantError(t, recorder, http.StatusBadRequest, project.CodeInvalidInput)
		if body.Error.Details["pinnedSlot"] == nil {
			t.Errorf("%s reported no pinnedSlot detail, so a client cannot tell which value was refused", body)
		}
	}
}

func TestPatchProjectNeedsTheFieldToBePresent(t *testing.T) {
	h := newHarness(t)
	p := h.register("Accounting")

	// An empty object is ambiguous - it could mean "change nothing" or "clear
	// the pin" - and guessing between them would silently unpin a project.
	recorder := h.call(http.MethodPatch, "/api/projects/"+p.ID, `{}`)
	h.wantError(t, recorder, http.StatusBadRequest, CodeInvalidRequest)
}

// TestPatchProjectRefusesEveryOtherField is the whole reason this is not a
// general update endpoint. A rename arriving here is refused by name rather
// than written without the validation a rename would need.
func TestPatchProjectRefusesEveryOtherField(t *testing.T) {
	h := newHarness(t)
	p := h.register("Accounting")

	for _, body := range []string{
		`{"name":"Renamed"}`,
		`{"archived":true}`,
		`{"hostPath":"D:\\elsewhere"}`,
		`{"pinnedSlot":1,"name":"Renamed"}`,
	} {
		recorder := h.call(http.MethodPatch, "/api/projects/"+p.ID, body)
		h.wantError(t, recorder, http.StatusBadRequest, CodeInvalidRequest)
	}

	after := h.call(http.MethodGet, "/api/projects/"+p.ID, "")
	got := decode[projectResponse](h.t, after).Project
	if got.Name != p.Name || got.Archived != p.Archived || got.PinnedSlot != nil {
		t.Errorf("a refused PATCH changed the project: %+v", got)
	}
}

func TestPatchProjectReportsAnUnknownProject(t *testing.T) {
	h := newHarness(t)

	recorder := h.call(http.MethodPatch, "/api/projects/p_00000000000000000000", `{"pinnedSlot":0}`)
	h.wantError(t, recorder, http.StatusNotFound, project.CodeNotFound)
}

func TestPatchProjectRejectsAMalformedBody(t *testing.T) {
	h := newHarness(t)
	p := h.register("Accounting")

	for _, body := range []string{`{`, `[]`, `{"pinnedSlot":"first"}`, `{"pinnedSlot":1} {"pinnedSlot":2}`} {
		recorder := h.call(http.MethodPatch, "/api/projects/"+p.ID, body)
		h.wantError(t, recorder, http.StatusBadRequest, CodeInvalidRequest)
	}
}

// TestListProjectsReturnsWorkspaceOrder checks the ordering through the API,
// which is where the workspace reads it from. Pinned first by slot, then
// everything else by registration order.
func TestListProjectsReturnsWorkspaceOrder(t *testing.T) {
	h := newHarness(t)

	first := h.register("Alpha")
	second := h.register("Bravo")
	third := h.register("Charlie")

	h.pin(third.ID, `{"pinnedSlot":0}`)

	recorder := h.call(http.MethodGet, "/api/projects", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET returned status %d", recorder.Code)
	}
	body := decode[struct {
		Projects []*project.Project `json:"projects"`
		Count    int                `json:"count"`
	}](h.t, recorder)

	want := []string{third.ID, first.ID, second.ID}
	if len(body.Projects) != len(want) {
		t.Fatalf("list returned %d projects, want %d", len(body.Projects), len(want))
	}
	for i, id := range want {
		if body.Projects[i].ID != id {
			got := make([]string, 0, len(body.Projects))
			for _, p := range body.Projects {
				got = append(got, p.ID)
			}
			t.Fatalf("position %d is %q, want %q (list was %v)", i, body.Projects[i].ID, id, got)
		}
	}
}

// TestProjectEndpointsKeepTheirMethods checks that the new PATCH did not widen
// what the existing read endpoint answers.
func TestProjectEndpointsKeepTheirMethods(t *testing.T) {
	h := newHarness(t)
	p := h.register("Accounting")

	recorder := h.call(http.MethodGet, "/api/projects/"+p.ID, "")
	if recorder.Code != http.StatusOK {
		t.Errorf("GET /api/projects/{id} returned %d, want 200", recorder.Code)
	}

	// A PUT is not a PATCH with different spelling: nothing here replaces a
	// project wholesale, and answering one would be a promise this build does
	// not keep.
	recorder = h.call(http.MethodPut, "/api/projects/"+p.ID, `{"pinnedSlot":0}`)
	if recorder.Code == http.StatusOK {
		t.Errorf("PUT /api/projects/{id} returned 200, want it to be refused")
	}
}

// TestSlotResponseDoesNotLeakTheStore checks the PATCH body is the same shape
// the rest of the API uses, so a client has one project decoder.
func TestSlotResponseDoesNotLeakTheStore(t *testing.T) {
	h := newHarness(t)
	p := h.register("Accounting")
	h.pin(p.ID, `{"pinnedSlot":3}`)

	recorder := h.call(http.MethodGet, "/api/projects/"+p.ID, "")
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(recorder.Body.Bytes(), &raw); err != nil {
		t.Fatalf("the response is not a JSON object: %v", err)
	}
	if _, ok := raw["project"]; !ok {
		t.Errorf("the response has no project key: %s", recorder.Body.String())
	}
}

package cmd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/session"
)

func createProjectOrderTestProject(t *testing.T, store *session.SQLiteStore, name string) *session.Project {
	t.Helper()
	project := &session.Project{Name: name, CanonicalDir: t.TempDir()}
	if err := store.CreateProject(context.Background(), project); err != nil {
		t.Fatalf("CreateProject(%s): %v", name, err)
	}
	return project
}

func patchProjectOrder(srv *serveServer, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPatch, "/v1/projects/order", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.handleProjectsOrder(rr, req)
	return rr
}

// sidebarGroupNames lists the sidebar's groups by project name, with the
// no-project group as "<no project>", and each project's rank.
func sidebarGroupNames(t *testing.T, srv *serveServer, query string) ([]string, map[string]int64) {
	t.Helper()
	rr := httptest.NewRecorder()
	srv.handleSidebar(rr, httptest.NewRequest(http.MethodGet, "/v1/sidebar?"+query, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("sidebar status=%d body=%s", rr.Code, rr.Body.String())
	}
	var payload struct {
		Groups []struct {
			NoProject bool `json:"no_project"`
			Project   *struct {
				Name      string `json:"name"`
				SortOrder int64  `json:"sort_order"`
			} `json:"project"`
		} `json:"groups"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(payload.Groups))
	ranks := map[string]int64{}
	for _, group := range payload.Groups {
		if group.Project == nil {
			names = append(names, "<no project>")
			continue
		}
		names = append(names, group.Project.Name)
		ranks[group.Project.Name] = group.Project.SortOrder
	}
	return names, ranks
}

func TestProjectsOrderReordersAtomicallyAndPublishes(t *testing.T) {
	srv, store := newServeProjectTestServer(t)
	srv.cfgRef = &config.Config{Providers: map[string]config.ProviderConfig{}}
	alpha := createProjectOrderTestProject(t, store, "Alpha")
	beta := createProjectOrderTestProject(t, store, "Beta")
	gamma := createProjectOrderTestProject(t, store, "Gamma")
	subscription, err := srv.ensureEventBroker().Subscribe(nil)
	if err != nil {
		t.Fatal(err)
	}

	rr := patchProjectOrder(srv, `{"project_ids":["`+gamma.ID+`","`+alpha.ID+`"]}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("reorder status=%d body=%s", rr.Code, rr.Body.String())
	}
	var body struct {
		Projects []session.ProjectPosition `json:"projects"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	want := []session.ProjectPosition{{ID: gamma.ID, SortOrder: 1}, {ID: beta.ID, SortOrder: 2}, {ID: alpha.ID, SortOrder: 3}}
	if !reflect.DeepEqual(body.Projects, want) {
		t.Fatalf("reorder response = %+v, want %+v", body.Projects, want)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	published := map[string]bool{}
	for len(published) < 2 {
		event, err := waitForServeEvent(ctx, subscription.Events)
		if err != nil {
			t.Fatalf("waiting for reorder events: %v (saw %v)", err, published)
		}
		if event.Type == serveEventProjectUpdated {
			published[event.ProjectID] = true
		}
	}
	if !reflect.DeepEqual(published, map[string]bool{gamma.ID: true, alpha.ID: true}) {
		t.Fatalf("published project updates = %v, want only the moved projects", published)
	}
	names, ranks := sidebarGroupNames(t, srv, "per_project=12&include_archived_projects=1")
	if !reflect.DeepEqual(names, []string{"Gamma", "Beta", "Alpha"}) || ranks["Gamma"] != 1 || ranks["Alpha"] != 3 {
		t.Fatalf("sidebar after reorder = %v ranks %v", names, ranks)
	}

	for _, tc := range []struct {
		name   string
		body   string
		status int
		code   string
	}{
		{name: "unknown", body: `{"project_ids":["` + beta.ID + `","prj_missing"]}`, status: http.StatusNotFound, code: "project_not_found"},
		{name: "duplicate", body: `{"project_ids":["` + beta.ID + `","` + beta.ID + `"]}`, status: http.StatusBadRequest, code: "invalid_project_order"},
		{name: "empty", body: `{"project_ids":[]}`, status: http.StatusBadRequest, code: "invalid_project_order"},
		{name: "unknown field", body: `{"ids":["` + beta.ID + `"]}`, status: http.StatusBadRequest, code: "invalid_project_order"},
		{name: "malformed", body: `{"project_ids":`, status: http.StatusBadRequest, code: "invalid_project_order"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rr := patchProjectOrder(srv, tc.body)
			var payload projectError
			if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil || rr.Code != tc.status || payload.Error.Code != tc.code {
				t.Fatalf("status=%d body=%s, want %d %s", rr.Code, rr.Body.String(), tc.status, tc.code)
			}
			if names, _ := sidebarGroupNames(t, srv, "per_project=12&include_archived_projects=1"); !reflect.DeepEqual(names, []string{"Gamma", "Beta", "Alpha"}) {
				t.Fatalf("rejected request changed order to %v", names)
			}
		})
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/projects/order", strings.NewReader(`{"project_ids":["`+alpha.ID+`"]}`))
	req.Header.Set("Content-Type", "application/json")
	methodRR := httptest.NewRecorder()
	srv.handleProjectsOrder(methodRR, req)
	if methodRR.Code != http.StatusMethodNotAllowed || methodRR.Header().Get("Allow") != http.MethodPatch {
		t.Fatalf("POST status=%d allow=%q", methodRR.Code, methodRR.Header().Get("Allow"))
	}
	typeRR := httptest.NewRecorder()
	srv.handleProjectsOrder(typeRR, httptest.NewRequest(http.MethodPatch, "/v1/projects/order", strings.NewReader(`{"project_ids":["`+alpha.ID+`"]}`)))
	if typeRR.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("missing content type status=%d", typeRR.Code)
	}
}

// The order route must win over the /v1/projects/ prefix (which would treat
// "order" as a project ID) and keep the project API's auth wrapper.
func TestProjectsOrderRouteThroughServeMux(t *testing.T) {
	srv, store := newServeProjectTestServer(t)
	srv.cfg = serveServerConfig{basePath: "/ui", requireAuth: true, token: "route-secret"}
	alpha := createProjectOrderTestProject(t, store, "Alpha")
	beta := createProjectOrderTestProject(t, store, "Beta")
	handler := srv.httpHandler()
	send := func(token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPatch, "/ui/v1/projects/order", strings.NewReader(`{"project_ids":["`+beta.ID+`","`+alpha.ID+`"]}`))
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		return rr
	}
	if rr := send(""); rr.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated reorder status=%d body=%s", rr.Code, rr.Body.String())
	}
	rr := send("route-secret")
	if rr.Code != http.StatusOK {
		t.Fatalf("reorder through mux status=%d body=%s", rr.Code, rr.Body.String())
	}
	var body struct {
		Projects []session.ProjectPosition `json:"projects"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if want := []session.ProjectPosition{{ID: beta.ID, SortOrder: 1}, {ID: alpha.ID, SortOrder: 2}}; !reflect.DeepEqual(body.Projects, want) {
		t.Fatalf("reorder through mux = %+v, want %+v", body.Projects, want)
	}
}

func TestProjectsOrderUnavailable(t *testing.T) {
	disabled, _ := newServeProjectTestServer(t)
	disabled.projectsEnabled = false
	if rr := patchProjectOrder(disabled, `{"project_ids":["prj_a"]}`); rr.Code != http.StatusNotFound || !strings.Contains(rr.Body.String(), "projects_disabled") {
		t.Fatalf("disabled status=%d body=%s", rr.Code, rr.Body.String())
	}
	unsupported := &serveServer{store: &session.NoopStore{}, projectsEnabled: true}
	if rr := patchProjectOrder(unsupported, `{"project_ids":["prj_a"]}`); rr.Code != http.StatusServiceUnavailable || !strings.Contains(rr.Body.String(), "projects_unavailable") {
		t.Fatalf("unsupported store status=%d body=%s", rr.Code, rr.Body.String())
	}
}

// Replies and the archived-sessions toggle must not move project groups;
// only an explicit reorder does, and archived projects list after active ones.
func TestSidebarProjectOrderIsStaticAcrossActivityAndArchivedVisibility(t *testing.T) {
	srv, store := newServeProjectTestServer(t)
	srv.cfgRef = &config.Config{Providers: map[string]config.ProviderConfig{}}
	ctx := context.Background()
	alpha := createProjectOrderTestProject(t, store, "Alpha")
	beta := createProjectOrderTestProject(t, store, "Beta")
	gamma := createProjectOrderTestProject(t, store, "Gamma")
	base := time.Now().Add(-time.Hour)
	createPinnedOrderTestSession(t, store, "alpha-chat", alpha.ID, alpha.CanonicalDir, base)
	createPinnedOrderTestSession(t, store, "beta-chat", beta.ID, beta.CanonicalDir, base.Add(time.Minute))
	createPinnedOrderTestSession(t, store, "gamma-chat", gamma.ID, gamma.CanonicalDir, base.Add(2*time.Minute))
	createPinnedOrderTestSession(t, store, "loose-chat", "", "", base.Add(3*time.Minute))

	want := []string{"Alpha", "Beta", "Gamma", "<no project>"}
	// Activity that would have moved Gamma, then Beta, to the top.
	touchPinnedOrderTestSession(t, store, "gamma-chat", base.Add(20*time.Minute))
	touchPinnedOrderTestSession(t, store, "beta-chat", base.Add(30*time.Minute))
	archived, err := store.Get(ctx, "alpha-chat")
	if err != nil {
		t.Fatal(err)
	}
	archived.Archived = true
	if err := store.Update(ctx, archived); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		"per_project=12&include_archived_projects=1&include_archived_sessions=0",
		"per_project=12&include_archived_projects=1&include_archived_sessions=1",
	} {
		if got, _ := sidebarGroupNames(t, srv, query); !reflect.DeepEqual(got, want) {
			t.Fatalf("sidebar %s = %v, want %v", query, got, want)
		}
	}

	archivedProject := true
	if _, err := store.UpdateProject(ctx, alpha.ID, session.ProjectUpdate{Archived: &archivedProject}); err != nil {
		t.Fatal(err)
	}
	if got, _ := sidebarGroupNames(t, srv, "per_project=12&include_archived_projects=1"); !reflect.DeepEqual(got, []string{"Beta", "Gamma", "Alpha", "<no project>"}) {
		t.Fatalf("sidebar with archived Alpha = %v", got)
	}
	if rr := patchProjectOrder(srv, `{"project_ids":["`+gamma.ID+`","`+beta.ID+`"]}`); rr.Code != http.StatusOK {
		t.Fatalf("reorder status=%d body=%s", rr.Code, rr.Body.String())
	}
	restored := false
	if _, err := store.UpdateProject(ctx, alpha.ID, session.ProjectUpdate{Archived: &restored}); err != nil {
		t.Fatal(err)
	}
	if got, _ := sidebarGroupNames(t, srv, "per_project=12&include_archived_projects=1"); !reflect.DeepEqual(got, []string{"Alpha", "Gamma", "Beta", "<no project>"}) {
		t.Fatalf("sidebar after reorder and restore = %v", got)
	}

	// The project list API shares the sidebar's order.
	rr := httptest.NewRecorder()
	srv.handleProjects(rr, httptest.NewRequest(http.MethodGet, "/v1/projects?include_archived=1", nil))
	var listed struct {
		Data []struct {
			Name      string `json:"name"`
			SortOrder int64  `json:"sort_order"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &listed); err != nil || rr.Code != http.StatusOK {
		t.Fatalf("project list status=%d body=%s err=%v", rr.Code, rr.Body.String(), err)
	}
	var listedNames []string
	for _, project := range listed.Data {
		listedNames = append(listedNames, project.Name)
	}
	if !reflect.DeepEqual(listedNames, []string{"Alpha", "Gamma", "Beta"}) || listed.Data[0].SortOrder != 1 {
		t.Fatalf("project list = %+v", listed.Data)
	}
}

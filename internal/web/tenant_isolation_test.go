package web

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/peaceful/cloud-console/internal/auth"
	"github.com/peaceful/cloud-console/internal/instances"
	"github.com/peaceful/cloud-console/internal/models"
)

// tenantRequest builds a request as the given console user.
func tenantRequest(method, target string, user *models.User) *http.Request {
	r := httptest.NewRequest(method, target, nil)
	if user != nil {
		r = r.WithContext(context.WithValue(r.Context(), userKey{}, user))
	}
	return r
}

func tenantFormRequest(method, target string, user *models.User, form string) *http.Request {
	r := httptest.NewRequest(method, target, strings.NewReader(form))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if user != nil {
		r = r.WithContext(context.WithValue(r.Context(), userKey{}, user))
	}
	return r
}

func TestRequireInstanceUsesOwnership(t *testing.T) {
	s := newTestServer(t)

	alice, err := s.App.DB.CreateUser("alice", "x", auth.RoleOperator)
	if err != nil {
		t.Fatal(err)
	}
	bob, err := s.App.DB.CreateUser("bob", "x", auth.RoleOperator)
	if err != nil {
		t.Fatal(err)
	}
	root, err := s.App.DB.CreateUser("root", "x", auth.RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}

	for _, meta := range []models.InstanceMeta{
		{Name: "alice-box", OwnerID: alice.ID},
		{Name: "bob-box", OwnerID: bob.ID},
		{Name: "loose-box"}, // created outside the console
	} {
		if err := s.App.DB.SaveInstanceMeta(&meta); err != nil {
			t.Fatal(err)
		}
	}

	cases := []struct {
		name string
		user *models.User
		inst string
		want int
	}{
		{"owner", alice, "alice-box", http.StatusNoContent},
		{"other tenant", bob, "alice-box", http.StatusNotFound},
		{"unowned instance", bob, "loose-box", http.StatusNotFound},
		{"admin bypasses ownership", root, "loose-box", http.StatusNoContent},
		{"unknown instance", alice, "missing", http.StatusNotFound},
		{"anonymous", nil, "alice-box", http.StatusNotFound},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wrapped := s.requireInstance(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			})
			r := tenantRequest(http.MethodGet, "/instances/"+tc.inst, tc.user)
			r.SetPathValue("name", tc.inst)
			rec := httptest.NewRecorder()
			wrapped(rec, r)
			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d", rec.Code, tc.want)
			}
		})
	}
}

func TestRequireDomainUsesInstanceOwner(t *testing.T) {
	s := newTestServer(t)

	alice, err := s.App.DB.CreateUser("alice", "x", auth.RoleOperator)
	if err != nil {
		t.Fatal(err)
	}
	bob, err := s.App.DB.CreateUser("bob", "x", auth.RoleOperator)
	if err != nil {
		t.Fatal(err)
	}
	root, err := s.App.DB.CreateUser("root", "x", auth.RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}

	if err := s.App.DB.SaveInstanceMeta(&models.InstanceMeta{Name: "alice-box", OwnerID: alice.ID}); err != nil {
		t.Fatal(err)
	}
	if err := s.App.DB.SaveInstanceMeta(&models.InstanceMeta{Name: "loose-box"}); err != nil {
		t.Fatal(err)
	}

	aliceDomain, err := s.App.DB.CreateDomain("alice-box", "alice.example.com", 80)
	if err != nil {
		t.Fatal(err)
	}
	looseDomain, err := s.App.DB.CreateDomain("loose-box", "loose.example.com", 80)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		user *models.User
		id   int64
		want int
	}{
		{"owner", alice, aliceDomain.ID, http.StatusNoContent},
		{"other tenant", bob, aliceDomain.ID, http.StatusNotFound},
		{"unowned instance domain", alice, looseDomain.ID, http.StatusNotFound},
		{"admin bypasses ownership", root, looseDomain.ID, http.StatusNoContent},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wrapped := s.requireDomain(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			})
			id := strconv.FormatInt(tc.id, 10)
			r := tenantRequest(http.MethodPost, "/domains/"+id+"/delete", tc.user)
			r.SetPathValue("id", id)
			rec := httptest.NewRecorder()
			wrapped(rec, r)
			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d", rec.Code, tc.want)
			}
		})
	}
}

// A tenant may hold the {name} of one of their instances while posting the id
// of a domain belonging to somebody else's instance. The handler must not
// trust the path mix and delete it.
func TestInstanceDomainDeleteRejectsMismatchedInstance(t *testing.T) {
	s := newTestServer(t)

	alice, err := s.App.DB.CreateUser("alice", "x", auth.RoleOperator)
	if err != nil {
		t.Fatal(err)
	}
	bob, err := s.App.DB.CreateUser("bob", "x", auth.RoleOperator)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.App.DB.SaveInstanceMeta(&models.InstanceMeta{Name: "alice-box", OwnerID: alice.ID}); err != nil {
		t.Fatal(err)
	}
	if err := s.App.DB.SaveInstanceMeta(&models.InstanceMeta{Name: "bob-box", OwnerID: bob.ID}); err != nil {
		t.Fatal(err)
	}
	victim, err := s.App.DB.CreateDomain("bob-box", "bob.example.com", 80)
	if err != nil {
		t.Fatal(err)
	}

	r := tenantRequest(http.MethodPost, "/instances/alice-box/domains/1/delete", alice)
	r.SetPathValue("name", "alice-box")
	r.SetPathValue("id", strconv.FormatInt(victim.ID, 10))
	rec := httptest.NewRecorder()
	s.handleInstanceDomainDelete(rec, r)

	if _, err := s.App.DB.GetDomain(victim.ID); err != nil {
		t.Fatalf("another tenant's domain was deleted: %v", err)
	}
}

// A tenant who can reach their own instance must still not be able to hand it
// to somebody else or make it admin-only. Only admins may reassign ownership.
func TestInstanceOwnerReassignmentIsAdminOnly(t *testing.T) {
	s := newTestServer(t)
	s.Instances = instances.New(s.App)

	alice, err := s.App.DB.CreateUser("alice", "x", auth.RoleOperator)
	if err != nil {
		t.Fatal(err)
	}
	bob, err := s.App.DB.CreateUser("bob", "x", auth.RoleOperator)
	if err != nil {
		t.Fatal(err)
	}
	root, err := s.App.DB.CreateUser("root", "x", auth.RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.App.DB.SaveInstanceMeta(&models.InstanceMeta{Name: "alice-box", OwnerID: alice.ID}); err != nil {
		t.Fatal(err)
	}

	post := func(user *models.User, owner string) {
		t.Helper()
		r := tenantFormRequest(http.MethodPost, "/instances/alice-box/owner", user, "owner_id="+owner)
		r.SetPathValue("name", "alice-box")
		s.handleInstanceOwner(httptest.NewRecorder(), r)
	}
	owner := func() int64 {
		t.Helper()
		meta, err := s.App.DB.GetInstanceMeta("alice-box")
		if err != nil {
			t.Fatal(err)
		}
		return meta.OwnerID
	}

	post(alice, strconv.FormatInt(bob.ID, 10))
	if got := owner(); got != alice.ID {
		t.Errorf("a tenant moved the instance to owner %d", got)
	}
	post(alice, "0")
	if got := owner(); got != alice.ID {
		t.Errorf("a tenant unassigned their own instance (owner %d)", got)
	}

	post(root, "not-a-number")
	if got := owner(); got != alice.ID {
		t.Errorf("a malformed owner changed the instance owner to %d", got)
	}
	post(root, strconv.FormatInt(bob.ID, 10))
	if got := owner(); got != bob.ID {
		t.Errorf("admin reassignment left owner %d, want %d", got, bob.ID)
	}
	post(root, "0")
	if got := owner(); got != 0 {
		t.Errorf("admin unassignment left owner %d, want 0", got)
	}
}

func TestUserQuotaIsAdminOnly(t *testing.T) {
	s := newTestServer(t)

	alice, err := s.App.DB.CreateUser("alice", "x", auth.RoleOperator)
	if err != nil {
		t.Fatal(err)
	}
	root, err := s.App.DB.CreateUser("root", "x", auth.RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	id := strconv.FormatInt(alice.ID, 10)

	r := tenantFormRequest(http.MethodPost, "/settings/users/"+id+"/quota", alice, "max_instances=5")
	r.SetPathValue("id", id)
	s.handleUserQuota(httptest.NewRecorder(), r)
	if fresh, _ := s.App.DB.GetUser(alice.ID); fresh.MaxInstances != 0 {
		t.Errorf("a non-admin changed their own quota to %d", fresh.MaxInstances)
	}

	r = tenantFormRequest(http.MethodPost, "/settings/users/"+id+"/quota", root, "max_instances=5")
	r.SetPathValue("id", id)
	s.handleUserQuota(httptest.NewRecorder(), r)
	fresh, err := s.App.DB.GetUser(alice.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.MaxInstances != 5 {
		t.Errorf("quota = %d, want 5", fresh.MaxInstances)
	}

	// Negative limits are refused rather than silently normalised.
	r = tenantFormRequest(http.MethodPost, "/settings/users/"+id+"/quota", root, "max_instances=-3")
	r.SetPathValue("id", id)
	s.handleUserQuota(httptest.NewRecorder(), r)
	if fresh, _ := s.App.DB.GetUser(alice.ID); fresh.MaxInstances != 5 {
		t.Errorf("a negative quota changed the stored value to %d", fresh.MaxInstances)
	}
}

func TestUserDeleteRefusesOwnersAndActiveJobs(t *testing.T) {
	s := newTestServer(t)

	alice, err := s.App.DB.CreateUser("alice", "x", auth.RoleOperator)
	if err != nil {
		t.Fatal(err)
	}
	root, err := s.App.DB.CreateUser("root", "x", auth.RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	id := strconv.FormatInt(alice.ID, 10)

	deleteAlice := func() {
		r := tenantRequest(http.MethodPost, "/settings/users/"+id+"/delete", root)
		r.SetPathValue("id", id)
		s.handleUserDelete(httptest.NewRecorder(), r)
	}

	if err := s.App.DB.SaveInstanceMeta(&models.InstanceMeta{Name: "alice-box", OwnerID: alice.ID}); err != nil {
		t.Fatal(err)
	}
	deleteAlice()
	if _, err := s.App.DB.GetUser(alice.ID); err != nil {
		t.Fatal("a user owning instances was deleted")
	}

	if err := s.App.DB.DeleteInstanceMeta("alice-box"); err != nil {
		t.Fatal(err)
	}
	if err := s.App.DB.CreateJob(&models.Job{
		ID: "alice-job", Kind: "instance.create", Target: "new-box", Status: "queued", OwnerID: alice.ID,
	}); err != nil {
		t.Fatal(err)
	}
	deleteAlice()
	if _, err := s.App.DB.GetUser(alice.ID); err != nil {
		t.Fatal("a user with an active job was deleted")
	}

	if err := s.App.DB.FinishJob("alice-job", "done", "Completed", ""); err != nil {
		t.Fatal(err)
	}
	deleteAlice()
	if _, err := s.App.DB.GetUser(alice.ID); err == nil {
		t.Fatal("a user with no instances or active jobs was not deleted")
	}
}

func TestJobTrackerSeparatesTenants(t *testing.T) {
	s := newTestServer(t)
	s.Renderer = newTestRenderer(t)

	alice, err := s.App.DB.CreateUser("alice", "x", auth.RoleOperator)
	if err != nil {
		t.Fatal(err)
	}
	bob, err := s.App.DB.CreateUser("bob", "x", auth.RoleOperator)
	if err != nil {
		t.Fatal(err)
	}

	for _, job := range []models.Job{
		{ID: "a1", Kind: "instance.create", Target: "alice-job", Status: "running", OwnerID: alice.ID},
		{ID: "b1", Kind: "instance.create", Target: "bob-job", Status: "running", OwnerID: bob.ID},
		{ID: "u1", Kind: "instance.create", Target: "unowned-job", Status: "running"},
	} {
		if err := s.App.DB.CreateJob(&job); err != nil {
			t.Fatal(err)
		}
	}

	html, _, err := s.renderJobTracker(models.Scope{UserID: alice.ID})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(html, "alice-job") {
		t.Error("alice cannot see her own job")
	}
	for _, leaked := range []string{"bob-job", "unowned-job"} {
		if strings.Contains(html, leaked) {
			t.Errorf("alice's tracker leaked %q", leaked)
		}
	}

	html, _, err = s.renderJobTracker(models.Scope{Admin: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"alice-job", "bob-job", "unowned-job"} {
		if !strings.Contains(html, want) {
			t.Errorf("the admin tracker is missing %q", want)
		}
	}

	if s.jobFeed(models.Scope{UserID: alice.ID}) == s.jobFeed(models.Scope{UserID: bob.ID}) {
		t.Error("two tenants share one job feed")
	}
	if s.jobFeed(models.Scope{UserID: alice.ID}) != s.jobFeed(models.Scope{UserID: alice.ID}) {
		t.Error("one tenant got a fresh feed per request")
	}
	if s.jobFeed(models.Scope{Admin: true}) == s.jobFeed(models.Scope{UserID: alice.ID}) {
		t.Error("an admin and a tenant share one job feed")
	}
}

func TestActivityIsFiledUnderTheTenantItConcerns(t *testing.T) {
	s := newTestServer(t)

	alice, err := s.App.DB.CreateUser("alice", "x", auth.RoleOperator)
	if err != nil {
		t.Fatal(err)
	}
	bob, err := s.App.DB.CreateUser("bob", "x", auth.RoleOperator)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.App.DB.SaveInstanceMeta(&models.InstanceMeta{Name: "alice-box", OwnerID: alice.ID}); err != nil {
		t.Fatal(err)
	}

	// An administrator rebooting a tenant's instance belongs to that tenant.
	s.App.Activity.Record("admin", "Reboot instance", "alice-box", "", nil)
	// An account-level action belongs to the actor.
	s.App.Activity.Record("bob", "Change password", "bob", "", nil)

	aliceRows, err := s.App.DB.ListActivityScoped(models.Scope{UserID: alice.ID}, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(aliceRows) != 1 || aliceRows[0].Target != "alice-box" {
		t.Errorf("alice's activity = %+v", aliceRows)
	}

	bobRows, err := s.App.DB.ListActivityScoped(models.Scope{UserID: bob.ID}, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(bobRows) != 1 || bobRows[0].Target != "bob" {
		t.Errorf("bob's activity = %+v", bobRows)
	}

	adminRows, err := s.App.DB.ListActivityScoped(models.Scope{Admin: true}, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(adminRows) != 2 {
		t.Errorf("admin activity rows = %d, want 2", len(adminRows))
	}
}

// The create wizard and the instance settings tab must not draw ownership
// controls for tenants, even if their data ever carried a user list.
func TestOwnershipControlsAreAdminOnly(t *testing.T) {
	renderer := newTestRenderer(t)

	operator := models.User{ID: 2, Username: "alice", Role: auth.RoleOperator}
	admin := models.User{ID: 1, Username: "root", Role: auth.RoleAdmin}
	inst := sampleInstance()
	inst.OwnerID = operator.ID

	baseAs := func(user models.User) baseData {
		base := sampleBase()
		base.User = &user
		return base
	}

	t.Run("create wizard", func(t *testing.T) {
		data := createData{
			baseData: baseAs(operator),
			Owners:   []models.User{admin, operator},
			Form:     createForm{Platform: "os", Image: "ubuntu-24.04"},
		}

		var buf bytes.Buffer
		if err := renderer.Execute(&buf, "create", data); err != nil {
			t.Fatalf("render create: %v", err)
		}
		if strings.Contains(buf.String(), `name="owner_id"`) {
			t.Error("a tenant can pick who owns a new instance")
		}

		data.baseData = baseAs(admin)
		buf.Reset()
		if err := renderer.Execute(&buf, "create", data); err != nil {
			t.Fatalf("render create as admin: %v", err)
		}
		out := buf.String()
		if !strings.Contains(out, `name="owner_id"`) {
			t.Error("an administrator cannot choose the owner of a new instance")
		}
		if !strings.Contains(out, "alice (operator)") {
			t.Error("the create-on-behalf list is missing a tenant")
		}
	})

	t.Run("instance settings", func(t *testing.T) {
		data := instanceData{
			baseData: baseAs(operator),
			Inst:     &inst,
			Tab:      "settings",
			Owners:   []models.User{admin, operator},
		}

		var buf bytes.Buffer
		if err := renderer.ExecutePartial(&buf, "instance", "tab_settings", data); err != nil {
			t.Fatalf("render tab_settings: %v", err)
		}
		if strings.Contains(buf.String(), `name="owner_id"`) {
			t.Error("a tenant can reassign an instance")
		}

		data.baseData = baseAs(admin)
		buf.Reset()
		if err := renderer.ExecutePartial(&buf, "instance", "tab_settings", data); err != nil {
			t.Fatalf("render tab_settings as admin: %v", err)
		}
		out := buf.String()
		if !strings.Contains(out, `name="owner_id"`) {
			t.Error("an administrator cannot reassign an instance")
		}
		if !strings.Contains(out, "Unassigned — admins only") {
			t.Error("the owner list is missing the unassigned option")
		}
		if !strings.Contains(out, "alice (operator)") {
			t.Error("the owner list is missing a tenant")
		}
	})
}

// Network usage counts and Caddy details describe the whole host, so they are
// reserved for administrators.
func TestNetworkingHostDetailsAreAdminOnly(t *testing.T) {
	renderer := newTestRenderer(t)

	operator := models.User{ID: 2, Username: "alice", Role: auth.RoleOperator}
	admin := models.User{ID: 1, Username: "root", Role: auth.RoleAdmin}

	data := networkingData{
		baseData: baseData{Title: "Networking", User: &operator},
		Networks: []networkInfo{
			{Name: "incusbr0", Type: "bridge", Managed: "yes", Address: "10.10.0.1/24", UsedBy: 7},
			{Name: "lo", Type: "loopback", Managed: "no", Address: "127.0.0.1/8", UsedBy: 0},
		},
		CaddyEnabled: true,
		CaddyPath:    "/etc/caddy/Caddyfile",
		CaddyAdmin:   "http://127.0.0.1:2019",
	}

	render := func() string {
		t.Helper()
		var buf bytes.Buffer
		if err := renderer.Execute(&buf, "networking", data); err != nil {
			t.Fatalf("render networking: %v", err)
		}
		return buf.String()
	}

	out := render()
	if strings.Contains(out, `class="num"`) || strings.Contains(out, ">7<") {
		t.Error("a tenant can see how many instances use a host network")
	}
	if strings.Contains(out, "incusbr0") || strings.Contains(out, "10.10.0.1/24") || strings.Contains(out, "127.0.0.1/8") {
		t.Error("a tenant can see host network names or subnets")
	}
	if strings.Count(out, "Private network") != 1 {
		t.Errorf("expected only 1 managed private network row for tenant, got %d", strings.Count(out, "Private network"))
	}
	if !strings.Contains(out, "Connectivity") || !strings.Contains(out, "Internal · outbound NAT") {
		t.Error("tenant networks table missing Connectivity column")
	}
	if strings.Contains(out, "Config path") || strings.Contains(out, "/etc/caddy/Caddyfile") {
		t.Error("a tenant can see host Caddy details")
	}

	data.User = &admin
	out = render()
	if !strings.Contains(out, `class="num"`) || !strings.Contains(out, ">7<") {
		t.Error("the administrator cannot see network usage counts")
	}
	if !strings.Contains(out, "incusbr0") || !strings.Contains(out, "10.10.0.1/24") {
		t.Error("the administrator cannot see network name or subnet")
	}
	if strings.Contains(out, "/etc/caddy/Caddyfile") || strings.Contains(out, "Config path") {
		t.Error("host routing details must never be rendered")
	}
}

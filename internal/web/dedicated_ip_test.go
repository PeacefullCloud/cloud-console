package web

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/peaceful/cloud-console/internal/auth"
	"github.com/peaceful/cloud-console/internal/instances"
	"github.com/peaceful/cloud-console/internal/models"
)

func TestDedicatedIPTemplatesAreAdminOnly(t *testing.T) {
	renderer := newTestRenderer(t)

	operator := models.User{ID: 2, Username: "alice", Role: auth.RoleOperator}
	viewer := models.User{ID: 3, Username: "bob", Role: auth.RoleViewer}
	admin := models.User{ID: 1, Username: "root", Role: auth.RoleAdmin}

	inst := sampleInstance()
	inst.OwnerID = operator.ID
	inst.DedicatedIPs = []models.DedicatedIP{{
		ID: 1, PublicIP: "203.0.113.50", ListenIP: "172.31.1.10",
		Network: "incusbr0", Label: "Edge IP", InstanceName: inst.Name, TargetIP: "10.10.0.21", SNAT: true,
	}}
	avail := []models.DedicatedIP{{
		ID: 2, PublicIP: "198.51.100.77", ListenIP: "172.31.1.11", Network: "incusbr0", Label: "Spare pool IP",
	}}

	t.Run("networking page", func(t *testing.T) {
		for _, user := range []models.User{operator, viewer} {
			data := networkingData{
				baseData:     baseData{Title: "Networking", User: &user, CSRF: "tok"},
				Networks:     []networkInfo{{Name: "incusbr0", Type: "bridge", Managed: "yes", Address: "10.10.0.1/24", UsedBy: 2}},
				Instances:    []instances.Instance{},
				DedicatedIPs: append(append([]models.DedicatedIP{}, inst.DedicatedIPs...), avail...),
			}
			var buf bytes.Buffer
			if err := renderer.Execute(&buf, "networking", data); err != nil {
				t.Fatalf("render networking for %s: %v", user.Role, err)
			}
			out := buf.String()
			for _, forbidden := range []string{
				"Dedicated public IPs", "Add dedicated IP", "198.51.100.77",
				"172.31.1.10", "172.31.1.11", "/networking/ips", "Spare pool IP",
			} {
				if strings.Contains(out, forbidden) {
					t.Errorf("%s saw admin-only string %q on /networking", user.Role, forbidden)
				}
			}
		}

		data := networkingData{
			baseData:     baseData{Title: "Networking", User: &admin, CSRF: "tok"},
			Networks:     []networkInfo{{Name: "incusbr0", Type: "bridge", Managed: "yes", Address: "10.10.0.1/24", UsedBy: 2}},
			DedicatedIPs: append(append([]models.DedicatedIP{}, inst.DedicatedIPs...), avail...),
		}
		var buf bytes.Buffer
		if err := renderer.Execute(&buf, "networking", data); err != nil {
			t.Fatalf("render networking for admin: %v", err)
		}
		out := buf.String()
		for _, want := range []string{"Dedicated public IPs", "Add dedicated IP", "203.0.113.50", "198.51.100.77", "172.31.1.10", "/networking/ips"} {
			if !strings.Contains(out, want) {
				t.Errorf("admin networking page missing %q", want)
			}
		}
	})

	t.Run("instance networking tab", func(t *testing.T) {
		for _, user := range []models.User{operator, viewer} {
			base := sampleBase()
			base.User = &user
			data := instanceData{
				baseData:              base,
				Inst:                  &inst,
				Tab:                   "networking",
				AvailableDedicatedIPs: avail,
			}
			var buf bytes.Buffer
			if err := renderer.ExecutePartial(&buf, "instance", "tab_networking", data); err != nil {
				t.Fatalf("render tab_networking for %s: %v", user.Role, err)
			}
			out := buf.String()
			// Tenant sees their own assigned public IP, never internal listen IP or pool controls.
			if !strings.Contains(out, "203.0.113.50") {
				t.Errorf("%s could not see their assigned dedicated public IP", user.Role)
			}
			for _, forbidden := range []string{
				"172.31.1.10", "198.51.100.77", "Spare pool IP",
				"/dedicated-ips/assign", "/dedicated-ips/1/unassign", "Manage pool",
			} {
				if strings.Contains(out, forbidden) {
					t.Errorf("%s saw admin-only string %q on instance networking tab", user.Role, forbidden)
				}
			}
		}

		base := sampleBase()
		base.User = &admin
		data := instanceData{
			baseData:              base,
			Inst:                  &inst,
			Tab:                   "networking",
			AvailableDedicatedIPs: avail,
		}
		var buf bytes.Buffer
		if err := renderer.ExecutePartial(&buf, "instance", "tab_networking", data); err != nil {
			t.Fatalf("render tab_networking for admin: %v", err)
		}
		out := buf.String()
		for _, want := range []string{"203.0.113.50", "172.31.1.10", "198.51.100.77", "/dedicated-ips/assign", "/dedicated-ips/1/unassign"} {
			if !strings.Contains(out, want) {
				t.Errorf("admin instance networking tab missing %q", want)
			}
		}
	})
}

func TestDedicatedIPHandlersAreAdminOnly(t *testing.T) {
	s := newTestServer(t)
	s.Instances = instances.New(s.App)

	alice, err := s.App.DB.CreateUser("alice", "x", auth.RoleOperator)
	if err != nil {
		t.Fatal(err)
	}
	viewer, err := s.App.DB.CreateUser("bob", "x", auth.RoleViewer)
	if err != nil {
		t.Fatal(err)
	}
	root, err := s.App.DB.CreateUser("root", "x", auth.RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}

	// Non-admins cannot add pool IPs.
	for _, u := range []*models.User{alice, viewer} {
		r := tenantFormRequest(http.MethodPost, "/networking/ips", u, "public_ip=203.0.113.50&network=incusbr0")
		s.handleDedicatedIPAdd(httptest.NewRecorder(), r)
	}
	if list, err := s.App.DB.ListDedicatedIPs(); err != nil || len(list) != 0 {
		t.Fatalf("non-admin added pool IP: list=%+v err=%v", list, err)
	}

	// Admin can add, and non-admins cannot assign, unassign, or delete it.
	r := tenantFormRequest(http.MethodPost, "/networking/ips", root, "public_ip=203.0.113.50&network=incusbr0&label=Edge")
	s.handleDedicatedIPAdd(httptest.NewRecorder(), r)

	list, err := s.App.DB.ListDedicatedIPs()
	if err != nil || len(list) != 1 {
		t.Fatalf("admin pool list = %+v, err=%v", list, err)
	}
	id := strconv.FormatInt(list[0].ID, 10)

	// Mark assigned at DB level so we can verify non-admins cannot unassign either.
	if err := s.App.DB.AssignDedicatedIP(list[0].ID, "alice-box", "10.48.36.55", true); err != nil {
		t.Fatal(err)
	}

	for _, u := range []*models.User{alice, viewer} {
		ru := tenantRequest(http.MethodPost, "/networking/ips/"+id+"/unassign", u)
		ru.SetPathValue("id", id)
		s.handleDedicatedIPUnassign(httptest.NewRecorder(), ru)

		ri := tenantRequest(http.MethodPost, "/instances/alice-box/dedicated-ips/"+id+"/unassign", u)
		ri.SetPathValue("name", "alice-box")
		ri.SetPathValue("id", id)
		s.handleInstanceDedicatedIPUnassign(httptest.NewRecorder(), ri)

		ra := tenantFormRequest(http.MethodPost, "/instances/alice-box/dedicated-ips/assign", u, "ip_id="+id)
		ra.SetPathValue("name", "alice-box")
		s.handleInstanceDedicatedIPAssign(httptest.NewRecorder(), ra)

		rd := tenantRequest(http.MethodPost, "/networking/ips/"+id+"/delete", u)
		rd.SetPathValue("id", id)
		s.handleDedicatedIPDelete(httptest.NewRecorder(), rd)
	}

	after, err := s.App.DB.GetDedicatedIP(list[0].ID)
	if err != nil || after.InstanceName != "alice-box" {
		t.Fatalf("non-admin mutated dedicated IP: %+v, err=%v", after, err)
	}

	// Admin can unassign (with nil Incus in test server) and delete.
	if err := s.App.DB.UnassignDedicatedIP(list[0].ID); err != nil {
		t.Fatal(err)
	}
	rd := tenantRequest(http.MethodPost, "/networking/ips/"+id+"/delete", root)
	rd.SetPathValue("id", id)
	s.handleDedicatedIPDelete(httptest.NewRecorder(), rd)
	if final, _ := s.App.DB.ListDedicatedIPs(); len(final) != 0 {
		t.Fatalf("admin delete left %+v", final)
	}
}

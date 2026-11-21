package web

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/peaceful/cloud-console/internal/auth"
	"github.com/peaceful/cloud-console/internal/models"
)

func sshKeyUser(t *testing.T, s *Server, name, role string) *models.User {
	t.Helper()
	u, err := s.App.DB.CreateUser(name, "x", role)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// TestSSHKeysTenantIsolation proves keys are private to their owner at the DB
// layer: a foreign id cannot be fetched, renamed or deleted by another tenant.
func TestSSHKeysTenantIsolation(t *testing.T) {
	s := newTestServer(t)
	alice := sshKeyUser(t, s, "alice", auth.RoleOperator)
	bob := sshKeyUser(t, s, "bob", auth.RoleOperator)

	key := &models.SSHKey{OwnerID: alice.ID, Name: "laptop", PublicKey: "ssh-ed25519 AAAA alice", Fingerprint: "SHA256:alice"}
	if err := s.App.DB.CreateSSHKey(key); err != nil {
		t.Fatal(err)
	}

	if got, err := s.App.DB.ListSSHKeysForUser(bob.ID); err != nil || len(got) != 0 {
		t.Fatalf("bob listed %d of alice's keys (err %v)", len(got), err)
	}
	if _, err := s.App.DB.GetSSHKeyForUser(bob.ID, key.ID); err == nil {
		t.Fatal("bob could fetch alice's key")
	}
	if err := s.App.DB.RenameSSHKeyForUser(bob.ID, key.ID, "stolen"); err == nil {
		t.Fatal("bob could rename alice's key")
	}
	if err := s.App.DB.DeleteSSHKeyForUser(bob.ID, key.ID); err == nil {
		t.Fatal("bob could delete alice's key")
	}

	if got, err := s.App.DB.GetSSHKeyForUser(alice.ID, key.ID); err != nil || got.Name != "laptop" {
		t.Fatalf("alice's key changed: %+v (err %v)", got, err)
	}
}

// TestGenerateOnlyStoresPublicKey checks that a generated private key is shown
// in the one-time HTMX response but never written to the database.
func TestGenerateOnlyStoresPublicKey(t *testing.T) {
	s := newTestServer(t)
	s.Renderer = newTestRenderer(t)
	alice := sshKeyUser(t, s, "alice", auth.RoleOperator)

	r := tenantFormRequest(http.MethodPost, "/ssh-keys/generate", alice, "name=laptop")
	r.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	s.handleSSHKeyGenerate(rec, r)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "BEGIN OPENSSH PRIVATE KEY") {
		t.Fatal("response did not reveal the one-time private key")
	}

	keys, err := s.App.DB.ListSSHKeysForUser(alice.ID)
	if err != nil || len(keys) != 1 {
		t.Fatalf("stored %d keys (err %v)", len(keys), err)
	}
	if !strings.HasPrefix(keys[0].PublicKey, "ssh-ed25519 ") {
		t.Fatalf("stored key is not an authorized_keys line: %q", keys[0].PublicKey)
	}
	if strings.Contains(keys[0].PublicKey, "PRIVATE KEY") || strings.Contains(keys[0].Fingerprint, "PRIVATE") {
		t.Fatal("a private key was persisted")
	}
}

// TestSSHKeyDeleteHandlerIsScoped checks the HTTP layer, not just the DB: an
// HTMX delete from another tenant leaves the owner's key intact.
func TestSSHKeyDeleteHandlerIsScoped(t *testing.T) {
	s := newTestServer(t)
	s.Renderer = newTestRenderer(t)
	alice := sshKeyUser(t, s, "alice", auth.RoleOperator)
	bob := sshKeyUser(t, s, "bob", auth.RoleOperator)

	key := &models.SSHKey{OwnerID: alice.ID, Name: "laptop", PublicKey: "ssh-ed25519 AAAA alice", Fingerprint: "SHA256:alice"}
	if err := s.App.DB.CreateSSHKey(key); err != nil {
		t.Fatal(err)
	}

	r := tenantRequest(http.MethodPost, "/ssh-keys/"+itoa(key.ID)+"/delete", bob)
	r.Header.Set("HX-Request", "true")
	r.SetPathValue("id", itoa(key.ID))
	rec := httptest.NewRecorder()
	s.handleSSHKeyDelete(rec, r)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if _, err := s.App.DB.GetSSHKeyForUser(alice.ID, key.ID); err != nil {
		t.Fatal("bob's delete removed alice's key")
	}
}

// TestCreateSubmitRejectsForeignKey proves the instance-create handler always
// resolves a saved key against the instance owner, never the actor.
func TestCreateSubmitRejectsForeignKey(t *testing.T) {
	s := newTestServer(t)
	alice := sshKeyUser(t, s, "alice", auth.RoleOperator)
	bob := sshKeyUser(t, s, "bob", auth.RoleOperator)

	key := &models.SSHKey{OwnerID: bob.ID, Name: "bob-key", PublicKey: "ssh-ed25519 AAAA bob", Fingerprint: "SHA256:bob"}
	if err := s.App.DB.CreateSSHKey(key); err != nil {
		t.Fatal(err)
	}

	form := "name=alice-box&image=ubuntu-24.04&kind=container&size=1|512|10&ssh_key_id=" + itoa(key.ID)
	r := tenantFormRequest(http.MethodPost, "/instances", alice, form)
	rec := httptest.NewRecorder()
	s.handleCreateSubmit(rec, r)

	// A refused request redirects with a flash instead of reaching Incus.
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rec.Code)
	}
	if rec.Header().Get("Location") != "/create" {
		t.Fatalf("redirect = %q, want /create", rec.Header().Get("Location"))
	}
}

// TestCreateKeysScopedToOwner covers the HTMX refresh of the create wizard's
// key selector: a tenant cannot use owner_id to see another tenant's keys.
func TestCreateKeysScopedToOwner(t *testing.T) {
	s := newTestServer(t)
	s.Renderer = newTestRenderer(t)
	alice := sshKeyUser(t, s, "alice", auth.RoleOperator)
	bob := sshKeyUser(t, s, "bob", auth.RoleOperator)

	key := &models.SSHKey{OwnerID: alice.ID, Name: "alice-laptop", PublicKey: "ssh-ed25519 AAAA alice", Fingerprint: "SHA256:alice"}
	if err := s.App.DB.CreateSSHKey(key); err != nil {
		t.Fatal(err)
	}

	r := tenantRequest(http.MethodGet, "/create/keys?owner_id="+itoa(alice.ID), bob)
	rec := httptest.NewRecorder()
	s.handleCreateKeys(rec, r)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "alice-laptop") {
		t.Fatal("a non-admin saw another tenant's key in the selector")
	}
}

func itoa(n int64) string {
	return strconv.FormatInt(n, 10)
}

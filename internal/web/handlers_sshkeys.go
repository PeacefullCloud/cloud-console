package web

import (
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/peaceful/cloud-console/internal/models"
	"github.com/peaceful/cloud-console/internal/sshkeys"
)

// maxKeyUpload is the largest .pub file the add form will accept. A public key
// is a single short line, so anything bigger is a mistake or an abuse attempt.
const maxKeyUpload = 8 << 10

// handleSSHKeys renders the tenant's own key vault. The list is always scoped
// to the signed-in user — there is no admin "view all keys" mode.
func (s *Server) handleSSHKeys(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r)
	if user == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	data := sshKeysData{baseData: s.newBase(w, r, "ssh-keys", "SSH keys")}
	keys, err := s.App.DB.ListSSHKeysForUser(user.ID)
	if err != nil {
		s.renderError(w, r, "SSH keys", "ssh-keys", err)
		return
	}
	data.Keys = keys
	s.render(w, r, "ssh_keys", http.StatusOK, data)
}

// serveSSHKeysBody re-renders the whole key card after any mutation. Including
// the one-time PrivateKey here (only on a generate response) means a revealed
// private key vanishes on the next action instead of lingering in the DOM.
func (s *Server) serveSSHKeysBody(w http.ResponseWriter, r *http.Request, actionErr error, notice, privateKey, privateKeyName string) {
	data := sshKeysFragmentData{
		CSRF:           csrfFrom(r),
		User:           userFrom(r),
		Error:          friendlyError(actionErr),
		Notice:         notice,
		PrivateKey:     privateKey,
		PrivateKeyName: privateKeyName,
	}
	if user := userFrom(r); user != nil {
		keys, err := s.App.DB.ListSSHKeysForUser(user.ID)
		if err != nil {
			data.Error = friendlyError(err)
		} else {
			data.Keys = keys
		}
	}

	if err := s.Renderer.RenderPartial(w, "ssh_keys", "ssh_keys_body", data); err != nil {
		s.log.Error("render ssh keys card", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

// keyDisplayName validates a key label and returns a friendly error for blank
// or overly long values. Names are unique per tenant, checked separately.
func keyDisplayName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" {
		return "", errors.New("give the key a name")
	}
	if len(name) > 64 {
		return "", errors.New("the key name must be 64 characters or fewer")
	}
	return name, nil
}

// keyNameTaken enforces the per-tenant unique name rule before hitting the DB,
// so the UI can explain the conflict instead of showing a constraint error.
func (s *Server) keyNameTaken(userID int64, name string, exceptID int64) (bool, error) {
	keys, err := s.App.DB.ListSSHKeysForUser(userID)
	if err != nil {
		return false, err
	}
	for _, k := range keys {
		if k.ID != exceptID && strings.EqualFold(k.Name, name) {
			return true, nil
		}
	}
	return false, nil
}

// handleSSHKeyCreate adds a public key, either pasted or uploaded as a .pub
// file. The file takes precedence when present.
func (s *Server) handleSSHKeyCreate(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r)
	if user == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	var err error
	if aerr := checkWriteAccess(r); aerr != nil {
		err = aerr
	}

	var name string
	if err == nil {
		name, err = keyDisplayName(r.FormValue("name"))
	}
	if err == nil {
		var taken bool
		taken, err = s.keyNameTaken(user.ID, name, 0)
		if err == nil && taken {
			err = errors.New("a key with that name already exists")
		}
	}

	var normalized, fingerprint string
	if err == nil {
		value := ""
		// FormFile works for both multipart uploads and the pasted field.
		if file, _, ferr := r.FormFile("key_file"); ferr == nil {
			defer file.Close()
			buf, readErr := io.ReadAll(io.LimitReader(file, maxKeyUpload))
			if readErr != nil {
				err = errors.New("could not read the uploaded key file")
			} else {
				value = string(buf)
			}
		}
		if err == nil && strings.TrimSpace(value) == "" {
			value = r.FormValue("public_key")
		}
		if err == nil {
			normalized, fingerprint, err = sshkeys.Parse(value)
		}
	}

	var key *models.SSHKey
	if err == nil {
		key = &models.SSHKey{OwnerID: user.ID, Name: name, PublicKey: normalized, Fingerprint: fingerprint}
		err = s.App.DB.CreateSSHKey(key)
	}

	notice := ""
	if err == nil {
		notice = "Added key " + name + "."
	}
	s.App.Activity.Record(user.Username, "Add SSH key", name, fingerprint, err)

	if isHTMX(r) {
		s.serveSSHKeysBody(w, r, err, notice, "", "")
		return
	}
	if err != nil {
		s.fail(w, r, "/ssh-keys", err)
		return
	}
	s.succeed(w, r, "/ssh-keys", notice)
}

// handleSSHKeyGenerate creates a new Ed25519 keypair. Only the public key is
// stored; the private key is returned in this one response and is gone on the
// next render or reload.
func (s *Server) handleSSHKeyGenerate(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r)
	if user == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	var err error
	if aerr := checkWriteAccess(r); aerr != nil {
		err = aerr
	}

	var name, public, private, fingerprint string
	if err == nil {
		name, err = keyDisplayName(r.FormValue("name"))
	}
	if err == nil {
		var taken bool
		taken, err = s.keyNameTaken(user.ID, name, 0)
		if err == nil && taken {
			err = errors.New("a key with that name already exists")
		}
	}
	if err == nil {
		public, private, fingerprint, err = sshkeys.Generate(name)
	}
	if err == nil {
		key := &models.SSHKey{OwnerID: user.ID, Name: name, PublicKey: public, Fingerprint: fingerprint}
		err = s.App.DB.CreateSSHKey(key)
	}

	notice := ""
	if err == nil {
		notice = "Generated key " + name + ". Save the private key now — it will not be shown again."
	}
	s.App.Activity.Record(user.Username, "Generate SSH key", name, fingerprint, err)

	if err != nil {
		if isHTMX(r) {
			s.serveSSHKeysBody(w, r, err, "", "", "")
			return
		}
		s.fail(w, r, "/ssh-keys", err)
		return
	}

	fileName := sanitizeFileName(name) + ".pem"
	if isHTMX(r) {
		s.serveSSHKeysBody(w, r, nil, notice, private, fileName)
		return
	}
	// No-JS fallback: render the full page with the one-time secret inline.
	data := sshKeysData{baseData: s.newBase(w, r, "ssh-keys", "SSH keys"),
		PrivateKey: private, PrivateKeyName: fileName, Notice: notice}
	if keys, kerr := s.App.DB.ListSSHKeysForUser(user.ID); kerr == nil {
		data.Keys = keys
	}
	s.render(w, r, "ssh_keys", http.StatusOK, data)
}

// handleSSHKeyDelete removes one of the signed-in user's keys.
func (s *Server) handleSSHKeyDelete(w http.ResponseWriter, r *http.Request) {
	s.mutateSSHKey(w, r, "Delete SSH key", func(id int64) error {
		return s.App.DB.DeleteSSHKeyForUser(userFrom(r).ID, id)
	}, func(name string) string { return "Deleted key " + name + "." })
}

// handleSSHKeyRename renames one of the signed-in user's keys.
func (s *Server) handleSSHKeyRename(w http.ResponseWriter, r *http.Request) {
	name, nerr := keyDisplayName(r.FormValue("name"))
	s.mutateSSHKey(w, r, "Rename SSH key", func(id int64) error {
		if nerr != nil {
			return nerr
		}
		var taken bool
		taken, err := s.keyNameTaken(userFrom(r).ID, name, id)
		if err != nil {
			return err
		}
		if taken {
			return errors.New("a key with that name already exists")
		}
		return s.App.DB.RenameSSHKeyForUser(userFrom(r).ID, id, name)
	}, func(string) string { return "Key renamed to " + name + "." })
}

// mutateSSHKey is the shared shell for rename/delete: parse/authorize, act,
// audit, and answer either an HTMX fragment or a redirect.
func (s *Server) mutateSSHKey(w http.ResponseWriter, r *http.Request, action string,
	mutate func(id int64) error, noticeFor func(name string) string) {
	user := userFrom(r)
	if user == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	id := int64(atoiDefault(r.PathValue("id"), 0))
	var key *models.SSHKey
	var err error
	if aerr := checkWriteAccess(r); aerr != nil {
		err = aerr
	}
	if err == nil {
		key, err = s.App.DB.GetSSHKeyForUser(user.ID, id)
	}
	if err == nil && mutate != nil {
		err = mutate(id)
	}

	label := ""
	detail := ""
	if key != nil {
		label = key.Name
		detail = key.Fingerprint
	}
	notice := ""
	if err == nil {
		notice = noticeFor(label)
	}
	s.App.Activity.Record(user.Username, action, label, detail, err)

	if isHTMX(r) {
		s.serveSSHKeysBody(w, r, err, notice, "", "")
		return
	}
	if err != nil {
		s.fail(w, r, "/ssh-keys", err)
		return
	}
	s.succeed(w, r, "/ssh-keys", notice)
}

// sanitizeFileName keeps a key name safe to use as a suggested download name.
func sanitizeFileName(s string) string {
	s = strings.TrimSpace(s)
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			b.WriteRune(r)
		} else {
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-.")
	if out == "" {
		return "ssh-key"
	}
	return out
}

// createKeyOptions loads the saved keys for the instance owner. It deliberately
// avoids newBase/createData so the HTMX refresh does not consume a flash cookie
// or depend on Incus being reachable.
func (s *Server) createKeyOptions(r *http.Request) []keyOption {
	keyOwner := int64(0)
	if user := userFrom(r); user != nil {
		keyOwner = user.ID
	}
	if checkAdminAccess(r) == nil {
		if id, err := parseInt64(r.URL.Query().Get("owner_id")); err == nil && id > 0 {
			keyOwner = id
		}
	}
	if keyOwner <= 0 {
		return nil
	}

	keys, err := s.App.DB.ListSSHKeysForUser(keyOwner)
	if err != nil {
		return nil
	}
	options := make([]keyOption, 0, len(keys))
	for _, k := range keys {
		options = append(options, keyOption{ID: k.ID, Label: k.Name, Fingerprint: k.Fingerprint})
	}
	return options
}

// handleCreateKeys re-renders just the saved-key <option> list when an admin
// changes the "Create on behalf of" selection in the wizard. A fresh Form means
// any key selected for the previous owner is cleared.
func (s *Server) handleCreateKeys(w http.ResponseWriter, r *http.Request) {
	data := keyOptionsFragmentData{SSHKeys: s.createKeyOptions(r)}
	if err := s.Renderer.RenderPartial(w, "create", "ssh_key_options", data); err != nil {
		s.log.Error("render ssh key options", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

// parseInt64 is a small helper for optional numeric form fields.
func parseInt64(raw string) (int64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	return strconv.ParseInt(raw, 10, 64)
}

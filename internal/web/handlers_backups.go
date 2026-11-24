package web

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/peaceful/cloud-console/internal/models"
	"github.com/peaceful/cloud-console/internal/sshkeys"
)

func (s *Server) handleBackupCreate(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	username := usernameOf(userFrom(r))

	backupName := strings.TrimSpace(r.FormValue("backup_name"))
	exportToS3 := r.FormValue("export_s3") != ""

	if werr := checkWriteAccess(r); werr != nil {
		s.App.Activity.Record(username, "Create backup", name, backupName, werr)
		s.finish(w, r, s.InstanceURL(name)+"?tab=backups", werr, "")
		return
	}

	job, err := s.Backups.EnqueueCreate(username, name, backupName, exportToS3)
	s.App.Activity.Record(username, "Create backup", name, backupName, err)

	if err != nil {
		s.finish(w, r, s.InstanceURL(name)+"?tab=backups", err, "")
		return
	}

	s.setFlash(w, "ok", "Backup started for "+name+" (job "+job.ID+").")
	http.Redirect(w, r, s.InstanceURL(name)+"?tab=backups", http.StatusSeeOther)
}

func (s *Server) handleBackupDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	backup := r.PathValue("backup")
	username := usernameOf(userFrom(r))

	err := checkWriteAccess(r)
	if err == nil && s.Backups.InProgress(name, backup) {
		err = errors.New("backup " + backup + " is still in progress — wait for it to finish")
	}
	if err == nil {
		ctx, cancel := operationContext(r)
		defer cancel()
		err = s.Backups.Delete(ctx, name, backup)
	}
	s.App.Activity.Record(username, "Delete backup", name, backup, err)

	s.finish(w, r, s.InstanceURL(name)+"?tab=backups", err, "Backup "+backup+" deleted.")
}

func (s *Server) handleBackupRestore(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	backup := r.PathValue("backup")
	username := usernameOf(userFrom(r))

	if werr := checkWriteAccess(r); werr != nil {
		s.App.Activity.Record(username, "Restore backup", name, backup, werr)
		s.finish(w, r, s.InstanceURL(name)+"?tab=backups", werr, "")
		return
	}
	if s.Backups.InProgress(name, backup) {
		busy := errors.New("backup " + backup + " is still in progress — wait for it to finish")
		s.App.Activity.Record(username, "Restore backup", name, backup, busy)
		s.finish(w, r, s.InstanceURL(name)+"?tab=backups", busy, "")
		return
	}

	job, err := s.Backups.EnqueueRestore(username, name, backup)
	s.App.Activity.Record(username, "Restore backup", name, backup, err)

	if err != nil {
		s.finish(w, r, s.InstanceURL(name)+"?tab=backups", err, "")
		return
	}

	s.setFlash(w, "ok", "Restoring "+name+" from "+backup+" (job "+job.ID+").")
	http.Redirect(w, r, s.InstanceURL(name), http.StatusSeeOther)
}

func (s *Server) handleBackupExport(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	backup := r.PathValue("backup")
	username := usernameOf(userFrom(r))

	if werr := checkWriteAccess(r); werr != nil {
		s.App.Activity.Record(username, "Export backup", name, backup, werr)
		s.finish(w, r, s.InstanceURL(name)+"?tab=backups", werr, "")
		return
	}
	if s.Backups.InProgress(name, backup) {
		busy := errors.New("backup " + backup + " is still in progress — wait for it to finish")
		s.App.Activity.Record(username, "Export backup", name, backup, busy)
		s.finish(w, r, s.InstanceURL(name)+"?tab=backups", busy, "")
		return
	}

	job, err := s.Backups.EnqueueExport(username, name, backup)
	s.App.Activity.Record(username, "Export backup", name, backup, err)

	if err != nil {
		s.finish(w, r, s.InstanceURL(name)+"?tab=backups", err, "")
		return
	}

	s.setFlash(w, "ok", "Exporting "+backup+" to object storage (job "+job.ID+").")
	http.Redirect(w, r, s.InstanceURL(name)+"?tab=backups", http.StatusSeeOther)
}

func (s *Server) handleBackups(w http.ResponseWriter, r *http.Request) {
	scope := scopeFrom(r)
	list, err := s.Backups.All(r.Context(), scope)
	if err != nil {
		s.renderError(w, r, "Backups", "backups", err)
		return
	}

	dests, _ := s.App.DB.ListBackupDestinationsScoped(scope)
	snaps, _ := s.App.DB.ListAllResticSnapshotsScoped(scope)

	var total int64
	for _, b := range list {
		total += b.SizeBytes
	}
	for _, sn := range snaps {
		total += sn.DataAddedBytes
	}

	data := backupsData{
		baseData:           s.newBase(w, r, "backups", "Backups"),
		Backups:            list,
		ResticDestinations: dests,
		ResticSnapshots:    snaps,
		S3Enabled:          s.Backups.S3Configured(),
		S3Bucket:           s.Backups.S3Bucket(),
		TotalBytes:         total,
	}
	s.render(w, r, "backups", http.StatusOK, data)
}

func (s *Server) destinationAllowed(r *http.Request, id int64) (*models.BackupDestination, error) {
	dest, err := s.App.DB.GetBackupDestination(id)
	if err != nil {
		return nil, errors.New("backup destination not found")
	}
	scope := scopeFrom(r)
	if !scope.All() && dest.UserID != scope.UserID {
		return nil, errors.New("you do not own that backup destination")
	}
	return dest, nil
}

func (s *Server) handleBackupDestinationCreate(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r)
	if user == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	if werr := checkWriteAccess(r); werr != nil {
		s.App.Activity.Record(user.Username, "Add backup destination", "", "", werr)
		s.finish(w, r, "/backups", werr, "")
		return
	}

	name := strings.TrimSpace(r.FormValue("name"))
	kind := strings.TrimSpace(r.FormValue("kind"))
	repoURL := strings.TrimSpace(r.FormValue("repo_url"))
	repoPass := r.FormValue("repo_password")
	if name == "" || repoURL == "" || repoPass == "" {
		s.finish(w, r, "/backups", errors.New("name, repository URL and encryption password are required"), "")
		return
	}
	if kind != "s3" && kind != "sftp" && kind != "local" {
		s.finish(w, r, "/backups", errors.New("destination kind must be s3, sftp or local"), "")
		return
	}

	keepLast, _ := strconv.Atoi(strings.TrimSpace(r.FormValue("keep_last")))
	keepDaily, _ := strconv.Atoi(strings.TrimSpace(r.FormValue("keep_daily")))
	keepWeekly, _ := strconv.Atoi(strings.TrimSpace(r.FormValue("keep_weekly")))
	if keepLast <= 0 {
		keepLast = 7
	}
	if keepDaily < 0 {
		keepDaily = 0
	}
	if keepWeekly < 0 {
		keepWeekly = 0
	}

	dest := &models.BackupDestination{
		UserID:        user.ID,
		Name:          name,
		Kind:          kind,
		RepoURL:       repoURL,
		RepoPassword:  repoPass,
		S3AccessKey:   strings.TrimSpace(r.FormValue("s3_access_key")),
		S3SecretKey:   strings.TrimSpace(r.FormValue("s3_secret_key")),
		S3Region:      strings.TrimSpace(r.FormValue("s3_region")),
		SSHKnownHosts: strings.TrimSpace(r.FormValue("ssh_known_hosts")),
		KeepLast:      keepLast,
		KeepDaily:     keepDaily,
		KeepWeekly:    keepWeekly,
	}

	if kind == "sftp" {
		pub, priv, _, err := sshkeys.Generate("cloud-console-restic-" + user.Username)
		if err != nil {
			s.finish(w, r, "/backups", err, "")
			return
		}
		dest.SSHPublicKey = pub
		dest.SSHPrivateKey = priv
	}

	_, err := s.App.DB.CreateBackupDestination(dest)
	s.App.Activity.Record(user.Username, "Add backup destination", name, kind, err)
	msg := "Backup destination " + name + " added."
	if kind == "sftp" && err == nil {
		msg = "SFTP destination " + name + " added. Copy its SSH public key into your storage server's authorized_keys."
	}
	s.finish(w, r, "/backups", err, msg)
}

func (s *Server) handleBackupDestinationDelete(w http.ResponseWriter, r *http.Request) {
	username := usernameOf(userFrom(r))
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		s.finish(w, r, "/backups", errors.New("invalid destination"), "")
		return
	}
	if werr := checkWriteAccess(r); werr != nil {
		s.finish(w, r, "/backups", werr, "")
		return
	}
	dest, err := s.destinationAllowed(r, id)
	if err == nil {
		err = s.App.DB.DeleteBackupDestination(dest.ID)
	}
	s.App.Activity.Record(username, "Delete backup destination", strconv.FormatInt(id, 10), "", err)
	s.finish(w, r, "/backups", err, "Backup destination removed.")
}

func (s *Server) handleBackupDestinationCheck(w http.ResponseWriter, r *http.Request) {
	username := usernameOf(userFrom(r))
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		s.finish(w, r, "/backups", errors.New("invalid destination"), "")
		return
	}
	if werr := checkWriteAccess(r); werr != nil {
		s.finish(w, r, "/backups", werr, "")
		return
	}
	dest, err := s.destinationAllowed(r, id)
	if err != nil {
		s.finish(w, r, "/backups", err, "")
		return
	}
	job, err := s.Backups.EnqueueResticCheck(username, dest.ID)
	s.App.Activity.Record(username, "Verify backup destination", dest.Name, "", err)
	if err != nil {
		s.finish(w, r, "/backups", err, "")
		return
	}
	s.setFlash(w, "ok", "Verifying "+dest.Name+" (job "+job.ID+").")
	http.Redirect(w, r, "/backups", http.StatusSeeOther)
}

func (s *Server) handleBackupDestinationPrune(w http.ResponseWriter, r *http.Request) {
	username := usernameOf(userFrom(r))
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		s.finish(w, r, "/backups", errors.New("invalid destination"), "")
		return
	}
	if werr := checkWriteAccess(r); werr != nil {
		s.finish(w, r, "/backups", werr, "")
		return
	}
	dest, err := s.destinationAllowed(r, id)
	if err != nil {
		s.finish(w, r, "/backups", err, "")
		return
	}
	job, err := s.Backups.EnqueueResticPrune(username, dest.ID, "")
	s.App.Activity.Record(username, "Prune backup destination", dest.Name, "", err)
	if err != nil {
		s.finish(w, r, "/backups", err, "")
		return
	}
	s.setFlash(w, "ok", "Pruning "+dest.Name+" according to retention policy (job "+job.ID+").")
	http.Redirect(w, r, "/backups", http.StatusSeeOther)
}

func (s *Server) handleResticBackupCreate(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	username := usernameOf(userFrom(r))
	if werr := checkWriteAccess(r); werr != nil {
		s.finish(w, r, s.InstanceURL(name)+"?tab=backups", werr, "")
		return
	}
	destID, err := strconv.ParseInt(strings.TrimSpace(r.FormValue("destination_id")), 10, 64)
	if err != nil || destID <= 0 {
		s.finish(w, r, s.InstanceURL(name)+"?tab=backups", errors.New("select a backup destination"), "")
		return
	}
	dest, err := s.destinationAllowed(r, destID)
	if err != nil {
		s.finish(w, r, s.InstanceURL(name)+"?tab=backups", err, "")
		return
	}
	backupName := strings.TrimSpace(r.FormValue("backup_name"))
	job, err := s.Backups.EnqueueResticBackup(username, name, dest.ID, backupName)
	s.App.Activity.Record(username, "Restic backup", name, dest.Name, err)
	if err != nil {
		s.finish(w, r, s.InstanceURL(name)+"?tab=backups", err, "")
		return
	}
	s.setFlash(w, "ok", "Restic backup to "+dest.Name+" started (job "+job.ID+").")
	http.Redirect(w, r, s.InstanceURL(name)+"?tab=backups", http.StatusSeeOther)
}

func (s *Server) handleResticSnapshotRestore(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	username := usernameOf(userFrom(r))
	if werr := checkWriteAccess(r); werr != nil {
		s.finish(w, r, s.InstanceURL(name)+"?tab=backups", werr, "")
		return
	}
	snapRowID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || snapRowID <= 0 {
		s.finish(w, r, s.InstanceURL(name)+"?tab=backups", errors.New("invalid snapshot"), "")
		return
	}
	snap, err := s.App.DB.GetResticSnapshot(snapRowID)
	if err != nil || snap.InstanceName != name {
		s.finish(w, r, s.InstanceURL(name)+"?tab=backups", errors.New("snapshot not found"), "")
		return
	}
	if _, err := s.destinationAllowed(r, snap.DestinationID); err != nil {
		s.finish(w, r, s.InstanceURL(name)+"?tab=backups", err, "")
		return
	}
	job, err := s.Backups.EnqueueResticRestore(username, snap.ID, name)
	s.App.Activity.Record(username, "Restic restore", name, snap.SnapshotID, err)
	if err != nil {
		s.finish(w, r, s.InstanceURL(name)+"?tab=backups", err, "")
		return
	}
	s.setFlash(w, "ok", "Restoring "+name+" from Restic snapshot "+snap.SnapshotID+" (job "+job.ID+").")
	http.Redirect(w, r, s.InstanceURL(name), http.StatusSeeOther)
}

func (s *Server) handleResticSnapshotDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	username := usernameOf(userFrom(r))
	if werr := checkWriteAccess(r); werr != nil {
		s.finish(w, r, s.InstanceURL(name)+"?tab=backups", werr, "")
		return
	}
	snapRowID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || snapRowID <= 0 {
		s.finish(w, r, s.InstanceURL(name)+"?tab=backups", errors.New("invalid snapshot"), "")
		return
	}
	snap, err := s.App.DB.GetResticSnapshot(snapRowID)
	if err != nil || snap.InstanceName != name {
		s.finish(w, r, s.InstanceURL(name)+"?tab=backups", errors.New("snapshot not found"), "")
		return
	}
	if _, err := s.destinationAllowed(r, snap.DestinationID); err != nil {
		s.finish(w, r, s.InstanceURL(name)+"?tab=backups", err, "")
		return
	}
	ctx, cancel := operationContext(r)
	defer cancel()
	err = s.Backups.DeleteResticSnapshot(ctx, snap.ID)
	s.App.Activity.Record(username, "Delete Restic snapshot", name, snap.SnapshotID, err)
	s.finish(w, r, s.InstanceURL(name)+"?tab=backups", err, "Restic snapshot removed.")
}

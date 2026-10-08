package web

import (
	"errors"
	"net/http"
	"strings"
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
		err = s.Backups.Delete(r.Context(), name, backup)
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
	list, err := s.Backups.All(r.Context())
	if err != nil {
		s.renderError(w, r, "Backups", "backups", err)
		return
	}

	var total int64
	for _, b := range list {
		total += b.SizeBytes
	}

	data := backupsData{
		baseData:   s.newBase(w, r, "backups", "Backups"),
		Backups:    list,
		S3Enabled:  s.Backups.S3Configured(),
		S3Bucket:   s.Backups.S3Bucket(),
		TotalBytes: total,
	}
	s.render(w, r, "backups", http.StatusOK, data)
}

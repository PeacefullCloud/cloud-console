package web

import (
	"net/http"
	"strings"

	"github.com/peaceful/cloud-console/internal/snapshots"
)

func (s *Server) handleSnapshotCreate(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	username := usernameOf(userFrom(r))

	snapName := strings.TrimSpace(r.FormValue("snapshot_name"))
	note := strings.TrimSpace(r.FormValue("note"))

	err := s.Snapshots.Create(r.Context(), name, snapName, note, username)
	s.App.Activity.Record(username, "Create snapshot", name, snapName, err)

	if err != nil && strings.Contains(err.Error(), "already exists") {
		// Retry once with a fresh timestamped name so the button always works.
		retry := snapshots.SuggestName(timeNow())
		err = s.Snapshots.Create(r.Context(), name, retry, note, username)
		if err == nil {
			s.App.Activity.Record(username, "Create snapshot", name, retry, nil)
			s.succeed(w, r, s.InstanceURL(name)+"?tab=snapshots", "Snapshot "+retry+" created.")
			return
		}
	}

	s.finish(w, r, s.InstanceURL(name)+"?tab=snapshots", err, "Snapshot created.")
}

func (s *Server) handleSnapshotDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	snap := r.PathValue("snapshot")
	username := usernameOf(userFrom(r))

	err := s.Snapshots.Delete(r.Context(), name, snap)
	s.App.Activity.Record(username, "Delete snapshot", name, snap, err)

	s.finish(w, r, s.InstanceURL(name)+"?tab=snapshots", err, "Snapshot "+snap+" deleted.")
}

func (s *Server) handleSnapshotRestore(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	snap := r.PathValue("snapshot")
	username := usernameOf(userFrom(r))

	err := s.Snapshots.Restore(r.Context(), name, snap)
	s.App.Activity.Record(username, "Restore snapshot", name, snap, err)

	s.finish(w, r, s.InstanceURL(name)+"?tab=snapshots", err, "Restored "+name+" to "+snap+".")
}

func (s *Server) handleSnapshots(w http.ResponseWriter, r *http.Request) {
	list, err := s.Snapshots.All()
	if err != nil {
		s.renderError(w, r, "Snapshots", "snapshots", err)
		return
	}

	data := snapshotsData{
		baseData:  s.newBase(w, r, "snapshots", "Snapshots"),
		Snapshots: list,
	}
	s.render(w, r, "snapshots", http.StatusOK, data)
}

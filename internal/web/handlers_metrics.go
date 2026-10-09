package web

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/csv"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/peaceful/cloud-console/internal/auth"
)

// metricsAuthorized reports whether the request carries the configured
// bearer token. Both sides are hashed first so the comparison takes the same
// time whatever the lengths.
func (s *Server) metricsAuthorized(r *http.Request) bool {
	want := s.App.Cfg.MetricsToken
	if want == "" {
		return false
	}
	got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	a, b := sha256.Sum256([]byte(got)), sha256.Sum256([]byte(want))
	return subtle.ConstantTimeCompare(a[:], b[:]) == 1
}

// handleMetrics serves Prometheus text exposition. Without a configured token
// it does not exist, and with one it answers only to that token: instance
// names and usage are not for the open internet.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if s.App.Cfg.MetricsToken == "" {
		http.NotFound(w, r)
		return
	}
	if !s.metricsAuthorized(r) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="metrics"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var b strings.Builder
	gauge := func(name, help string) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s gauge\n", name, help, name)
	}
	counter := func(name, help string) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s counter\n", name, help, name)
	}

	gauge("console_up", "1 when the console is serving.")
	fmt.Fprintf(&b, "console_up 1\n")

	incusUp := 1
	list, err := s.Instances.List(r.Context())
	if err != nil {
		incusUp = 0
		s.log.Warn("metrics: could not list instances", "err", err)
	}
	gauge("console_incus_up", "1 when the Incus daemon answered.")
	fmt.Fprintf(&b, "console_incus_up %d\n", incusUp)

	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })

	gauge("console_instance_running", "1 when the instance is running.")
	for _, i := range list {
		fmt.Fprintf(&b, "console_instance_running{instance=%s} %d\n", label(i.Name), boolInt(i.Running))
	}
	gauge("console_instance_cpu_percent", "CPU use over the last sample interval.")
	for _, i := range list {
		fmt.Fprintf(&b, "console_instance_cpu_percent{instance=%s} %g\n", label(i.Name), i.CPUPercent)
	}
	gauge("console_instance_memory_used_bytes", "Memory in use.")
	for _, i := range list {
		fmt.Fprintf(&b, "console_instance_memory_used_bytes{instance=%s} %d\n", label(i.Name), i.MemUsed)
	}
	gauge("console_instance_memory_limit_bytes", "Configured memory limit (0 = unlimited).")
	for _, i := range list {
		fmt.Fprintf(&b, "console_instance_memory_limit_bytes{instance=%s} %d\n", label(i.Name), i.MemoryBytes)
	}
	gauge("console_instance_disk_used_bytes", "Root disk in use.")
	for _, i := range list {
		fmt.Fprintf(&b, "console_instance_disk_used_bytes{instance=%s} %d\n", label(i.Name), i.DiskUsed)
	}
	gauge("console_instance_disk_limit_bytes", "Root disk size (0 = unknown).")
	for _, i := range list {
		fmt.Fprintf(&b, "console_instance_disk_limit_bytes{instance=%s} %d\n", label(i.Name), i.DiskBytes)
	}
	counter("console_instance_network_receive_bytes_total", "Bytes received.")
	for _, i := range list {
		fmt.Fprintf(&b, "console_instance_network_receive_bytes_total{instance=%s} %d\n", label(i.Name), i.NetRx)
	}
	counter("console_instance_network_transmit_bytes_total", "Bytes sent.")
	for _, i := range list {
		fmt.Fprintf(&b, "console_instance_network_transmit_bytes_total{instance=%s} %d\n", label(i.Name), i.NetTx)
	}

	if active, err := s.App.DB.ListActiveJobs(); err == nil {
		gauge("console_jobs_active", "Queued or running background jobs.")
		fmt.Fprintf(&b, "console_jobs_active %d\n", len(active))
	}
	if recent, err := s.App.DB.ListJobs(200); err == nil {
		failed := 0
		for _, j := range recent {
			if j.Status == "failed" {
				failed++
			}
		}
		gauge("console_jobs_failed_recent", "Failed jobs among the 200 most recent.")
		fmt.Fprintf(&b, "console_jobs_failed_recent %d\n", failed)
	}
	if n, size, err := s.App.DB.BackupTotals(); err == nil {
		gauge("console_backups", "Completed backups tracked by the console.")
		fmt.Fprintf(&b, "console_backups %d\n", n)
		gauge("console_backups_bytes", "Total size of completed backups.")
		fmt.Fprintf(&b, "console_backups_bytes %d\n", size)
	}
	if n, err := s.App.DB.CountSessions(); err == nil {
		gauge("console_sessions_active", "Signed-in browser sessions.")
		fmt.Fprintf(&b, "console_sessions_active %d\n", n)
	}

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = w.Write([]byte(b.String()))
}

// label quotes a Prometheus label value.
func label(v string) string {
	v = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(v)
	return `"` + v + `"`
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// maxExportRows bounds one export so it cannot hold a request open forever.
const maxExportRows = 100000

// handleActivityExport downloads the audit log as CSV. Admin only: it names
// every user and every action.
func (s *Server) handleActivityExport(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r)
	if user == nil || !auth.CanAdmin(user.Role) {
		http.Error(w, errAdminOnly.Error(), http.StatusForbidden)
		return
	}

	name := "activity-" + time.Now().UTC().Format("20060102-150405") + ".csv"
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)

	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"time_utc", "user", "action", "target", "detail", "result"})

	const page = 500
	written := 0
	for offset := 0; written < maxExportRows; offset += page {
		rows, err := s.App.DB.ListActivity(page, offset)
		if err != nil {
			s.log.Error("activity export", "err", err)
			break
		}
		for _, a := range rows {
			_ = cw.Write([]string{
				a.TS.UTC().Format(time.RFC3339),
				csvSafe(a.Username), csvSafe(a.Action), csvSafe(a.Target), csvSafe(a.Detail), a.Status,
			})
		}
		written += len(rows)
		if len(rows) < page {
			break
		}
	}
	cw.Flush()
	s.App.Activity.Record(user.Username, "Export audit log", "", fmt.Sprintf("%d rows", written), nil)
}

// csvSafe stops a spreadsheet from running a cell as a formula. Targets and
// details contain names users chose, so a value like =HYPERLINK(...) would
// otherwise execute when an admin opens the export.
func csvSafe(v string) string {
	if v != "" && strings.ContainsRune("=+-@\t\r", rune(v[0])) {
		return "'" + v
	}
	return v
}

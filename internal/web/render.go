// Package web contains the console's HTTP layer: routing, middleware, template
// rendering and handlers.
package web

import (
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/peaceful/cloud-console/internal/auth"
	"github.com/peaceful/cloud-console/internal/incus"
	"github.com/peaceful/cloud-console/internal/models"
)

// Renderer parses one template set per page.
//
// Go templates have no inheritance, so each page is parsed together with the
// shared layout and partials. In development mode templates are re-parsed on
// every request so edits appear without a rebuild.
type Renderer struct {
	fsys fs.FS
	dev  bool
	log  *slog.Logger

	pages map[string]string

	mu    sync.RWMutex
	cache map[string]*template.Template
}

// NewRenderer prepares the renderer from an embedded (or on-disk) file system.
func NewRenderer(fsys fs.FS, dev bool, log *slog.Logger) (*Renderer, error) {
	r := &Renderer{
		fsys:  fsys,
		dev:   dev,
		log:   log,
		pages: map[string]string{},
		cache: map[string]*template.Template{},
	}

	if err := r.discover(); err != nil {
		return nil, err
	}

	// Fail fast: parse every page once at startup so template errors surface
	// immediately instead of on the first request.
	for name := range r.pages {
		if _, err := r.parse(name); err != nil {
			return nil, fmt.Errorf("parse template %s: %w", name, err)
		}
	}

	return r, nil
}

func (r *Renderer) discover() error {
	entries, err := fs.ReadDir(r.fsys, "templates")
	if err != nil {
		return fmt.Errorf("read templates: %w", err)
	}

	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".html") || name == "layout.html" {
			continue
		}
		r.pages[strings.TrimSuffix(name, ".html")] = path.Join("templates", name)
	}
	return nil
}

func (r *Renderer) parse(page string) (*template.Template, error) {
	files := []string{"templates/layout.html"}

	partials, err := fs.Glob(r.fsys, "templates/partials/*.html")
	if err != nil {
		return nil, err
	}
	files = append(files, partials...)
	files = append(files, r.pages[page])

	return template.New("").Funcs(r.funcMap()).ParseFS(r.fsys, files...)
}

func (r *Renderer) set(page string) (*template.Template, error) {
	if r.dev {
		return r.parse(page)
	}

	r.mu.RLock()
	cached := r.cache[page]
	r.mu.RUnlock()
	if cached != nil {
		return cached, nil
	}

	parsed, err := r.parse(page)
	if err != nil {
		return nil, err
	}

	r.mu.Lock()
	r.cache[page] = parsed
	r.mu.Unlock()
	return parsed, nil
}

// Render writes a page using the layout.
func (r *Renderer) Render(w http.ResponseWriter, status int, page string, data any) error {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	return r.Execute(w, page, data)
}

// Execute renders a full page into any writer.
func (r *Renderer) Execute(w io.Writer, page string, data any) error {
	set, err := r.set(page)
	if err != nil {
		return err
	}

	// Buffer the page so a template error halfway through does not emit a
	// half-rendered document.
	var buf strings.Builder
	if err := set.ExecuteTemplate(&buf, "layout", data); err != nil {
		return fmt.Errorf("render %s: %w", page, err)
	}

	_, err = io.WriteString(w, buf.String())
	return err
}

// RenderPartial writes a single named template (used by HTMX endpoints).
func (r *Renderer) RenderPartial(w http.ResponseWriter, page, templateName string, data any) error {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	return r.ExecutePartial(w, page, templateName, data)
}

// ExecutePartial renders a named template into any writer.
func (r *Renderer) ExecutePartial(w io.Writer, page, templateName string, data any) error {
	set, err := r.set(page)
	if err != nil {
		return err
	}

	var buf strings.Builder
	if err := set.ExecuteTemplate(&buf, templateName, data); err != nil {
		return fmt.Errorf("render partial %s/%s: %w", page, templateName, err)
	}

	_, err = io.WriteString(w, buf.String())
	return err
}

// RenderStandalone writes a page that does not use the shared layout (login).
func (r *Renderer) RenderStandalone(w http.ResponseWriter, file, templateName string, data any) error {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	return r.ExecuteStandalone(w, file, templateName, data)
}

// ExecuteStandalone renders a layout-free page into any writer.
func (r *Renderer) ExecuteStandalone(w io.Writer, file, templateName string, data any) error {
	parsed, err := template.New("").Funcs(r.funcMap()).ParseFS(r.fsys, file)
	if err != nil {
		return err
	}

	var buf strings.Builder
	if err := parsed.ExecuteTemplate(&buf, templateName, data); err != nil {
		return err
	}

	_, err = io.WriteString(w, buf.String())
	return err
}

// funcMap lists every helper the templates may call.
func (r *Renderer) funcMap() template.FuncMap {
	return template.FuncMap{
		"initials":        initials,
		"title":           title,
		"osClass":         osClass,
		"osInitial":       osInitial,
		"platformInitial": platformInitial,
		"statusClass":     statusClass,
		"gb":              func(n int64) string { return incus.FormatGB(n) },
		"gbNum":           func(n int64) int { return int(n >> 30) },
		"mb":              func(n int64) int { return int(n >> 20) },
		"bytes":           func(n int64) string { return incus.FormatBytes(n) },
		"date":            func(t time.Time) string { return humanDate(t) },
		"datetime":        func(t time.Time) string { return humanDateTime(t) },
		"ago":             func(t time.Time) string { return humanAgo(t) },
		"duration":        humanDuration,
		"pct":             func(v float64) string { return fmt.Sprintf("%.1f", v) },
		"half":            func(v float64) float64 { return v / 2 },
		"usageClass":      usageClass,
		"memPercent":      memPercent,
		"diskPercent":     diskPercent,
		"poolPercent":     poolPercent,
		"add":             func(a, b int) int { return a + b },
		"sub":             func(a, b int) int { return a - b },
		"jobTitle":        jobTitle,
		"jobPill":         jobPill,
		"greeting":        greeting,
		"dict":            dict,
		"safeURL":         func(s string) template.URL { return template.URL(s) },
		// canWrite/canAdmin gate mutating UI by role. They accept a
		// *models.User, a role string, or anything else (unknown and
		// missing values deny access, so viewers never see admin UI).
		"canWrite": func(v any) bool { return auth.CanWrite(roleName(v)) },
		"canAdmin": func(v any) bool { return auth.CanAdmin(roleName(v)) },
	}
}

// dict builds a map from alternating key/value arguments so a template can pass
// more than one value into a sub-template.
func dict(values ...any) (map[string]any, error) {
	if len(values)%2 != 0 {
		return nil, fmt.Errorf("dict: odd number of arguments")
	}

	out := make(map[string]any, len(values)/2)
	for i := 0; i < len(values); i += 2 {
		key, ok := values[i].(string)
		if !ok {
			return nil, fmt.Errorf("dict: keys must be strings")
		}
		out[key] = values[i+1]
	}
	return out, nil
}

// roleName extracts a role string from a *models.User, a models.User, a
// plain role string, or anything else. Unknown and missing values yield an
// empty role, which CanWrite/CanAdmin both reject.
func roleName(v any) string {
	switch t := v.(type) {
	case *models.User:
		if t == nil {
			return ""
		}
		return t.Role
	case models.User:
		return t.Role
	case string:
		return t
	default:
		return ""
	}
}

// --- template helpers -----------------------------------------------------

func initials(name string) string {
	if name == "" {
		return "?"
	}
	parts := strings.FieldsFunc(name, func(r rune) bool { return r == ' ' || r == '.' || r == '_' || r == '-' })
	if len(parts) == 0 {
		return strings.ToUpper(name[:1])
	}
	if len(parts) == 1 {
		runes := []rune(parts[0])
		if len(runes) > 1 {
			return strings.ToUpper(string(runes[:2]))
		}
		return strings.ToUpper(parts[0])
	}
	return strings.ToUpper(parts[0][:1] + parts[1][:1])
}

// title turns "virtual-machine" into "Virtual machine".
func title(s string) string {
	s = strings.ReplaceAll(s, "-", " ")
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// osClass maps an image label to a CSS class for the OS monogram badge.
func osClass(label string) string {
	l := strings.ToLower(label)
	switch {
	case strings.Contains(l, "ubuntu"):
		return "os-ubuntu"
	case strings.Contains(l, "debian"):
		return "os-debian"
	case strings.Contains(l, "alma") || strings.Contains(l, "rocky") || strings.Contains(l, "centos") || strings.Contains(l, "red hat"):
		return "os-almalinux"
	case strings.Contains(l, "alpine"):
		return "os-alpine"
	case strings.Contains(l, "wordpress") || strings.Contains(l, "php"):
		return "os-wordpress"
	default:
		return "os-custom"
	}
}

// osInitial returns the monogram letter for an image label.
func osInitial(label string) string {
	l := strings.ToLower(label)
	switch {
	case strings.Contains(l, "ubuntu"):
		return "U"
	case strings.Contains(l, "debian"):
		return "D"
	case strings.Contains(l, "alma"):
		return "A"
	case strings.Contains(l, "alpine"):
		return "A"
	case strings.Contains(l, "wordpress"):
		return "W"
	case label == "":
		return "?"
	default:
		return strings.ToUpper(label[:1])
	}
}

func platformInitial(id string) string {
	switch id {
	case "apps":
		return "A"
	case "os":
		return "L"
	default:
		return strings.ToUpper(id[:1])
	}
}

// statusClass maps an Incus status to a status colour class.
func statusClass(status string) string {
	switch strings.ToLower(status) {
	case "running":
		return "status-running"
	case "stopped", "frozen":
		return "status-stopped"
	case "error":
		return "status-error"
	case "":
		return "status-stopped"
	default:
		return "status-pending"
	}
}

// usageClass colours a progress bar by how full it is.
func usageClass(percent float64) string {
	switch {
	case percent >= 90:
		return "danger"
	case percent >= 75:
		return "warn"
	default:
		return ""
	}
}

func memPercent(used, limit int64) float64 {
	if limit <= 0 {
		if used <= 0 {
			return 0
		}
		// Without a limit, scale against 1 GiB so the bar still moves.
		return clampPercent(float64(used) / float64(1<<30) * 100)
	}
	return clampPercent(float64(used) / float64(limit) * 100)
}

func diskPercent(used, total int64) float64 {
	if total <= 0 {
		return 0
	}
	return clampPercent(float64(used) / float64(total) * 100)
}

func poolPercent(used, total int64) float64 {
	if total <= 0 {
		return 0
	}
	return clampPercent(float64(used) / float64(total) * 100)
}

func clampPercent(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}

func jobTitle(kind string) string {
	switch kind {
	case "instance.create":
		return "Create instance"
	case "instance.rebuild":
		return "Rebuild instance"
	case "backup.create":
		return "Create backup"
	case "backup.export":
		return "Export backup"
	case "backup.restore":
		return "Restore backup"
	default:
		return title(strings.ReplaceAll(kind, ".", " "))
	}
}

func jobPill(status string) string {
	switch status {
	case "done":
		return "pill-ok"
	case "failed":
		return "pill-err"
	case "running":
		return "pill-info"
	default:
		return ""
	}
}

// greeting returns a time-of-day greeting for the dashboard.
func greeting() string {
	switch h := time.Now().Hour(); {
	case h < 12:
		return "Good morning"
	case h < 18:
		return "Good afternoon"
	default:
		return "Good evening"
	}
}

func humanDate(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.Local().Format("2 Jan 2006")
}

func humanDateTime(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.Local().Format("2 Jan 2006, 15:04")
}

func humanAgo(t time.Time) string {
	if t.IsZero() {
		return "—"
	}

	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 7*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	default:
		return humanDate(t)
	}
}

func humanDuration(d time.Duration) string {
	if d <= 0 {
		return "—"
	}
	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	minutes := int(d.Minutes()) % 60

	switch {
	case days > 0:
		return fmt.Sprintf("%dd %dh", days, hours)
	case hours > 0:
		return fmt.Sprintf("%dh %dm", hours, minutes)
	default:
		return fmt.Sprintf("%dm", minutes)
	}
}

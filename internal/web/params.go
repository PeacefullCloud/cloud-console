package web

import (
	"net/http"
	"regexp"
)

// Path parameters reach handlers URL-decoded, so "%2F" arrives as "/". Every
// route is checked once, here, so no handler has to remember to validate and
// no value can later be joined into a file path or an Incus API URL.
var (
	// Instance names follow Incus' own rules (letters, digits, hyphens),
	// which also covers instances created outside the console.
	pathName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,62}$`)
	// Snapshot and backup names additionally allow "_" and ".", but never a
	// leading dot.
	pathLabel = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,127}$`)
	pathID    = regexp.MustCompile(`^[0-9]{1,18}$`)
	pathTab   = regexp.MustCompile(`^[a-z]{1,32}$`)
)

var pathRules = map[string]*regexp.Regexp{
	"name":     pathName,
	"snapshot": pathLabel,
	"backup":   pathLabel,
	"id":       pathID,
	"provider": pathID,
	"tab":      pathTab,
}

// withValidPathParams answers 404 when any known path parameter is malformed.
func withValidPathParams(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		for param, rule := range pathRules {
			if value := r.PathValue(param); value != "" && !rule.MatchString(value) {
				http.NotFound(w, r)
				return
			}
		}
		next(w, r)
	}
}

// validatedMux registers handlers wrapped in withValidPathParams.
type validatedMux struct{ mux *http.ServeMux }

func (v validatedMux) HandleFunc(pattern string, handler http.HandlerFunc) {
	v.mux.HandleFunc(pattern, withValidPathParams(handler))
}

package web

import (
	"net/http"
	"os"
	"path/filepath"
	"sort"
)

// api_profiles.go implements GET /api/profiles — lists .yaml files in the
// profiles/ directory (relative to the running exe, same convention
// cmd/mbgw/server.go's nextToExe uses for its own files) so the Web UI can
// offer a dropdown instead of the operator typing a path by hand (fix for
// the 2026-08-23 feedback: "Путь к профилю я замучился заполнять
// вручную"). Returns paths in the SAME "profiles/xxx.yaml" form
// profile.Parse already expects (see cmd/mbgw/server.go's device-loading
// loop), so a selected dropdown value can be saved as-is into
// devices.profile with no transformation needed.
func (s *Server) handleProfiles(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "используйте GET")
		return
	}

	dir := profilesDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		// Missing/unreadable profiles dir is not fatal to the whole UI —
		// report an empty list rather than a 500, so the rest of the
		// admin page still works; the operator sees "нет доступных
		// профилей" and can still type a path manually as a fallback.
		writeJSON(w, http.StatusOK, []string{})
		return
	}

	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if filepath.Ext(name) != ".yaml" && filepath.Ext(name) != ".yml" {
			continue
		}
		out = append(out, "profiles/"+name)
	}
	sort.Strings(out)
	writeJSON(w, http.StatusOK, out)
}

// profilesDir resolves the profiles/ directory next to the running exe —
// mirrors cmd/mbgw/server.go's nextToExe logic (duplicated here in
// miniature since internal/web cannot import cmd/mbgw without an import
// cycle: cmd/mbgw already imports internal/web).
func profilesDir() string {
	exePath, err := os.Executable()
	if err != nil {
		return "profiles"
	}
	exeDir, err := filepath.Abs(filepath.Dir(exePath))
	if err != nil {
		return "profiles"
	}
	return filepath.Join(exeDir, "profiles")
}

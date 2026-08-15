package server

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/ollie/monitorrr/internal/store"
)

// starterScript is one entry in the built-in library.
type starterScript struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Interpreter string `json:"interpreter"`
	File        string `json:"file"`
	TimeoutSecs int    `json:"timeout_seconds"`
}

// loadStarterScripts reads the library embedded in the binary.
func loadStarterScripts() ([]starterScript, error) {
	raw, err := webFS.ReadFile("web/starter/manifest.json")
	if err != nil {
		return nil, fmt.Errorf("read starter manifest: %w", err)
	}
	var scripts []starterScript
	if err := json.Unmarshal(raw, &scripts); err != nil {
		return nil, fmt.Errorf("parse starter manifest: %w", err)
	}
	return scripts, nil
}

// handleListStarterScripts describes the library without importing it, so the
// UI can show what would be added.
func (s *Server) handleListStarterScripts(w http.ResponseWriter, r *http.Request) {
	starters, err := loadStarterScripts()
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "could not read the starter library", err)
		return
	}

	existing, err := s.st.ListScripts(r.Context())
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "could not list scripts", err)
		return
	}
	have := map[string]bool{}
	for _, sc := range existing {
		have[sc.Name] = true
	}

	type entry struct {
		starterScript
		AlreadyPresent bool `json:"already_present"`
	}
	out := make([]entry, 0, len(starters))
	for _, st := range starters {
		out = append(out, entry{st, have[st.Name]})
	}
	writeJSON(w, http.StatusOK, map[string]any{"starters": out})
}

// handleImportStarterScripts adds the built-in library to this server.
//
// Import is by name and never overwrites: someone who has edited "disk-space"
// to suit their fleet must not have it silently reverted by pressing the button
// again. Re-importing therefore only fills in what is missing.
func (s *Server) handleImportStarterScripts(w http.ResponseWriter, r *http.Request) {
	starters, err := loadStarterScripts()
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "could not read the starter library", err)
		return
	}

	existing, err := s.st.ListScripts(r.Context())
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "could not list scripts", err)
		return
	}
	have := map[string]bool{}
	for _, sc := range existing {
		have[sc.Name] = true
	}

	// Non-nil so the JSON API emits [] rather than null on either list — a
	// client doing len(imported) should not have to special-case "none".
	imported := []string{}
	skipped := []string{}
	for _, st := range starters {
		if have[st.Name] {
			skipped = append(skipped, st.Name)
			continue
		}
		body, err := webFS.ReadFile("web/starter/" + st.File)
		if err != nil {
			s.log.Error("starter script missing from the binary", "file", st.File, "error", err)
			continue
		}
		if !store.ValidInterpreter(st.Interpreter) {
			s.log.Error("starter script has an unsupported interpreter",
				"name", st.Name, "interpreter", st.Interpreter)
			continue
		}
		if _, err := s.st.SaveScript(r.Context(), "", st.Name, st.Description,
			st.Interpreter, string(body), st.TimeoutSecs); err != nil {
			s.log.Error("could not import starter script", "name", st.Name, "error", err)
			continue
		}
		imported = append(imported, st.Name)
	}

	s.log.Info("starter scripts imported", "added", len(imported),
		"already_present", len(skipped), "by", adminUser(r))
	writeJSON(w, http.StatusOK, map[string]any{
		"imported": imported,
		"skipped":  skipped,
	})
}

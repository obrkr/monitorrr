package server

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/ollie/monitorrr/internal/proto"
	"github.com/ollie/monitorrr/internal/store"
)

// handleJobResult accepts a finished execution from an agent. It is on the
// agent API, authenticated by the device token, and a device may only write
// results for jobs dispatched to itself.
func (s *Server) handleJobResult(w http.ResponseWriter, r *http.Request) {
	id, token, ok := agentCredentials(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, proto.Error{Error: "missing agent credentials"})
		return
	}
	if err := s.st.Authenticate(r.Context(), id, token); err != nil {
		writeJSON(w, http.StatusUnauthorized, proto.Error{Error: "unknown device or bad token"})
		return
	}

	var result proto.JobResult
	if !decodeJSON(w, r, &result) {
		return
	}

	jobID := r.PathValue("id")
	err := s.st.CompleteJob(r.Context(), jobID, id, result.ExitCode,
		result.Stdout, result.Stderr, result.Error, result.DurationMS, result.Truncated)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, proto.Error{Error: "no such job for this device"})
			return
		}
		s.fail(w, http.StatusInternalServerError, "could not record job result", err)
		return
	}

	s.log.Info("job result recorded", "job", jobID, "device", id,
		"exit", result.ExitCode, "error", result.Error)
	w.WriteHeader(http.StatusNoContent)
}

// --- script management ---

func (s *Server) handleListScripts(w http.ResponseWriter, r *http.Request) {
	scripts, err := s.st.ListScripts(r.Context())
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "could not list scripts", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"scripts": scripts})
}

func (s *Server) handleSaveScript(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		Description string `json:"description"`
		Interpreter string `json:"interpreter"`
		Content     string `json:"content"`
		TimeoutSecs int    `json:"timeout_seconds"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}

	script, err := s.st.SaveScript(r.Context(), body.ID, body.Name, body.Description,
		body.Interpreter, body.Content, body.TimeoutSecs)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, proto.Error{Error: "no such script"})
			return
		}
		writeJSON(w, http.StatusBadRequest, proto.Error{Error: err.Error()})
		return
	}

	s.log.Info("script saved", "id", script.ID, "name", script.Name,
		"interpreter", script.Interpreter, "sha256", script.SHA256, "by", adminUser(r))
	writeJSON(w, http.StatusOK, script)
}

func (s *Server) handleDeleteScript(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.st.DeleteScript(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, proto.Error{Error: "no such script"})
			return
		}
		s.fail(w, http.StatusInternalServerError, "could not delete script", err)
		return
	}
	s.log.Info("script deleted", "id", id, "by", adminUser(r))
	w.WriteHeader(http.StatusNoContent)
}

// handleDispatch queues a script against selected devices. Dispatch is always
// explicit — nothing here runs a script on its own.
func (s *Server) handleDispatch(w http.ResponseWriter, r *http.Request) {
	var body struct {
		DeviceIDs []string `json:"device_ids"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}

	scriptID := r.PathValue("id")
	by := adminUser(r)
	jobs, err := s.st.Dispatch(r.Context(), scriptID, body.DeviceIDs, by)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, proto.Error{Error: "no such script or device"})
			return
		}
		writeJSON(w, http.StatusBadRequest, proto.Error{Error: err.Error()})
		return
	}

	// Every dispatch is logged with who, what, and where: this is the audit
	// record for arbitrary code being sent to endpoints.
	for _, j := range jobs {
		s.log.Info("job queued", "job", j.ID, "script", j.ScriptName, "sha256", j.ScriptSHA256,
			"device", j.DeviceHostname, "by", by)
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": jobs, "queued": len(jobs)})
}

// --- job history ---

func (s *Server) handleListJobs(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	jobs, err := s.st.ListJobs(r.Context(), limit)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "could not list jobs", err)
		return
	}
	pending, err := s.st.PendingJobCount(r.Context())
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "could not count pending jobs", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": jobs, "pending": pending})
}

func (s *Server) handleGetJob(w http.ResponseWriter, r *http.Request) {
	job, err := s.st.GetJob(r.Context(), r.PathValue("id"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, proto.Error{Error: "no such job"})
			return
		}
		s.fail(w, http.StatusInternalServerError, "could not read job", err)
		return
	}
	writeJSON(w, http.StatusOK, job)
}

// --- pages ---

// handleGetDevice returns everything the device page needs in one request:
// the device, its timeline, its run history, and the scripts that can actually
// run on it.
func (s *Server) handleGetDevice(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	device, err := s.st.GetDevice(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, proto.Error{Error: "no such device"})
			return
		}
		s.fail(w, http.StatusInternalServerError, "could not read device", err)
		return
	}

	events, err := s.st.DeviceEvents(r.Context(), id, 50)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "could not read device timeline", err)
		return
	}
	jobs, err := s.st.DeviceJobs(r.Context(), id, 50)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "could not read device runs", err)
		return
	}
	scripts, err := s.st.ListScripts(r.Context())
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "could not list scripts", err)
		return
	}

	// Filter here rather than in the browser: which scripts can run on this OS
	// is a rule the server owns, and Dispatch enforces it anyway.
	runnable := []store.Script{}
	for _, sc := range scripts {
		if store.InterpreterSupports(sc.Interpreter, device.OS) {
			runnable = append(runnable, sc)
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"device":           device,
		"events":           events,
		"jobs":             jobs,
		"runnable_scripts": runnable,
		"supports_retire":  device.SupportsRetire(),
	})
}

// handleDeviceDispatch runs one script on this one device. It is the same
// operation as dispatching from the scripts side, addressed the other way
// round, which is how the device page needs it.
func (s *Server) handleDeviceDispatch(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ScriptID string `json:"script_id"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}

	deviceID := r.PathValue("id")
	by := adminUser(r)
	jobs, err := s.st.Dispatch(r.Context(), body.ScriptID, []string{deviceID}, by)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, proto.Error{Error: "no such script or device"})
			return
		}
		writeJSON(w, http.StatusBadRequest, proto.Error{Error: err.Error()})
		return
	}

	for _, j := range jobs {
		s.log.Info("job queued", "job", j.ID, "script", j.ScriptName, "sha256", j.ScriptSHA256,
			"device", j.DeviceHostname, "by", by)
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": jobs, "queued": len(jobs)})
}

func (s *Server) handleDevicePage(w http.ResponseWriter, r *http.Request) {
	s.render(w, "device.html", map[string]any{
		"Page":     "devices",
		"Version":  Version,
		"DeviceID": r.PathValue("id"),
	})
}

func (s *Server) handleScriptsPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, "scripts.html", map[string]any{"Page": "scripts", "Version": Version})
}

func (s *Server) handleRunsPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, "runs.html", map[string]any{"Page": "runs", "Version": Version})
}

// adminUser names whoever performed an action, for the audit trail. With auth
// disabled there is no identity to record, which is itself worth recording.
func adminUser(r *http.Request) string {
	if user, _, ok := r.BasicAuth(); ok && user != "" {
		return user
	}
	return "anonymous (no admin auth)"
}

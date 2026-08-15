package server

import (
	"fmt"
	"net/http"
	"os"

	"github.com/ollie/monitorrr/internal/proto"
)

// resetConfirmation is what an operator must type to confirm a destructive
// action. A checkbox is too easy to click through for something irreversible.
const resetConfirmation = "RESET"

// factoryResetConfirmation is deliberately different from the one above.
// Wiping the fleet and wiping the entire instance including its accounts are
// not the same decision, and should not be confirmable by the same reflex.
const factoryResetConfirmation = "FACTORY RESET"

// handleRetireAll asks every live agent to uninstall itself.
//
// This is the graceful way to stand an instance down: each machine removes its
// own service, identity and binary, and the records remain so you can see the
// decommissioning happen. Machines that are powered off retire whenever they
// next come back.
func (s *Server) handleRetireAll(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Confirm string `json:"confirm"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Confirm != resetConfirmation {
		writeJSON(w, http.StatusBadRequest, proto.Error{
			Error: "type " + resetConfirmation + " to confirm"})
		return
	}

	devices, err := s.st.ListDevices(r.Context())
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "could not list devices", err)
		return
	}

	by := adminUser(r)
	retired := 0
	for _, d := range devices {
		if d.RetiredAt != nil || d.Status == "retired" {
			continue
		}
		if err := s.st.RetireDevice(r.Context(), d.ID); err != nil {
			s.log.Error("could not retire device", "device", d.Hostname, "error", err)
			continue
		}
		retired++
	}

	auditf(r, "admin.retire-all", "", fmt.Sprintf("%d device(s) asked to uninstall their agents", retired))
	s.log.Warn("fleet-wide retirement requested", "devices", retired, "by", by)
	writeJSON(w, http.StatusOK, map[string]any{"retiring": retired})
}

// handleReset empties the instance.
//
// Everything about the fleet goes: devices, their history, scripts, uploaded
// files and collected files. Accounts are deliberately kept — you would
// otherwise be locked out of the server you just reset — and the enrollment
// token is rotated so old agents cannot silently re-enrol into the clean
// instance.
func (s *Server) handleReset(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Confirm string `json:"confirm"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Confirm != resetConfirmation {
		writeJSON(w, http.StatusBadRequest, proto.Error{
			Error: "type " + resetConfirmation + " to confirm"})
		return
	}

	by := adminUser(r)
	s.log.Warn("instance reset requested", "by", by)

	summary, err := s.st.ResetFleet(r.Context())
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "reset failed", err)
		return
	}

	// Stored bytes are not in the database, so they are removed separately.
	// Failing here is worth reporting but not worth failing the reset: the
	// records are already gone, and orphaned files are recoverable by hand.
	for _, dir := range []string{s.payloadDir(), s.collectDir()} {
		if err := os.RemoveAll(dir); err != nil && !os.IsNotExist(err) {
			s.log.Error("could not remove stored files", "dir", dir, "error", err)
		}
	}

	token, err := s.st.RotateEnrollToken(r.Context())
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "could not rotate the enrollment token", err)
		return
	}

	auditf(r, "admin.reset", "", fmt.Sprintf(
		"removed %d devices, %d scripts, %d runs, %d collections, %d files; enrollment token rotated",
		summary.Devices, summary.Scripts, summary.Jobs, summary.Collections, summary.Payloads))
	s.log.Warn("instance reset complete", "by", by,
		"devices", summary.Devices, "scripts", summary.Scripts, "jobs", summary.Jobs,
		"collections", summary.Collections, "payloads", summary.Payloads)

	writeJSON(w, http.StatusOK, map[string]any{
		"removed":      summary,
		"enroll_token": token,
	})
}

// handleFactoryReset returns the instance to its first-run state, accounts
// included.
//
// The session is ended as part of it: the account it belonged to no longer
// exists, and leaving a cookie that resolves to nothing would present a
// half-working console instead of the setup page.
func (s *Server) handleFactoryReset(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Confirm string `json:"confirm"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Confirm != factoryResetConfirmation {
		writeJSON(w, http.StatusBadRequest, proto.Error{
			Error: "type " + factoryResetConfirmation + " to confirm"})
		return
	}

	by := adminUser(r)
	s.log.Warn("factory reset requested", "by", by)

	// The audit table is emptied by the reset, and the middleware writes its
	// entry afterwards — so without this the only surviving record would be
	// this request, attributing it to an account that no longer exists.
	auditSkip(r)

	summary, err := s.st.FactoryReset(r.Context())
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "factory reset failed", err)
		return
	}

	for _, dir := range []string{s.payloadDir(), s.collectDir()} {
		if err := os.RemoveAll(dir); err != nil && !os.IsNotExist(err) {
			s.log.Error("could not remove stored files", "dir", dir, "error", err)
		}
	}

	// The audit trail was wiped with everything else, so this is recorded in
	// the server log — the only place left that remembers it happened.
	s.log.Warn("factory reset complete", "by", by,
		"devices", summary.Devices, "scripts", summary.Scripts, "jobs", summary.Jobs,
		"collections", summary.Collections, "payloads", summary.Payloads,
		"accounts", summary.Accounts)

	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, SameSite: http.SameSiteLaxMode,
	})
	writeJSON(w, http.StatusOK, map[string]any{"removed": summary})
}

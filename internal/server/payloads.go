package server

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"github.com/ollie/monitorrr/internal/proto"
	"github.com/ollie/monitorrr/internal/store"
)

// maxUpload caps a single pushed file. Large enough for real installers, small
// enough that a stray upload cannot fill the disk unnoticed.
const maxUpload = 2 << 30 // 2 GiB

// payloadDir is where uploaded files live: beside the database, so backing up
// the data directory captures both halves of a push together.
func (s *Server) payloadDir() string {
	return filepath.Join(filepath.Dir(s.cfg.DBPath), "payloads")
}

func (s *Server) payloadPath(id string) string {
	// The id is server-generated hex, but Base is belt and braces against it
	// ever becoming caller-influenced.
	return filepath.Join(s.payloadDir(), filepath.Base(id))
}

// handleUploadPayload accepts a file to be pushed to devices.
//
// The body is streamed to disk while hashing rather than buffered: an installer
// is routinely hundreds of megabytes, and holding one in memory would put the
// server at the mercy of whatever anyone uploads.
func (s *Server) handleUploadPayload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxUpload)

	reader, err := r.MultipartReader()
	if err != nil {
		writeJSON(w, http.StatusBadRequest, proto.Error{Error: "expected a multipart upload"})
		return
	}

	part, err := reader.NextPart()
	if err != nil {
		writeJSON(w, http.StatusBadRequest, proto.Error{Error: "no file in the upload"})
		return
	}
	defer part.Close()

	filename := filepath.Base(part.FileName())
	if filename == "" || filename == "." || filename == string(filepath.Separator) {
		writeJSON(w, http.StatusBadRequest, proto.Error{Error: "the upload has no usable filename"})
		return
	}

	id, err := randomID()
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "could not allocate an id", err)
		return
	}
	if err := os.MkdirAll(s.payloadDir(), 0o750); err != nil {
		s.fail(w, http.StatusInternalServerError, "could not create the payload directory", err)
		return
	}

	// Write to a temporary name first so a failed or truncated upload never
	// becomes a payload that agents would happily execute.
	tmp := s.payloadPath(id) + ".part"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "could not create the payload file", err)
		return
	}

	hash := sha256.New()
	size, err := io.Copy(io.MultiWriter(f, hash), part)
	closeErr := f.Close()
	if err != nil {
		os.Remove(tmp)
		writeJSON(w, http.StatusBadRequest, proto.Error{Error: "upload failed: " + err.Error()})
		return
	}
	if closeErr != nil {
		os.Remove(tmp)
		s.fail(w, http.StatusInternalServerError, "could not finish writing the payload", closeErr)
		return
	}
	if size == 0 {
		os.Remove(tmp)
		writeJSON(w, http.StatusBadRequest, proto.Error{Error: "the uploaded file is empty"})
		return
	}

	if err := os.Rename(tmp, s.payloadPath(id)); err != nil {
		os.Remove(tmp)
		s.fail(w, http.StatusInternalServerError, "could not store the payload", err)
		return
	}

	digest := hex.EncodeToString(hash.Sum(nil))
	payload, err := s.st.AddPayload(r.Context(), id, filename, size, digest, adminUser(r))
	if err != nil {
		os.Remove(s.payloadPath(id))
		writeJSON(w, http.StatusBadRequest, proto.Error{Error: err.Error()})
		return
	}

	auditf(r, "file.upload", filename, fmt.Sprintf("%d bytes, sha256 %s", size, digest[:12]))
	s.log.Info("payload uploaded", "id", id, "filename", filename,
		"bytes", size, "sha256", digest, "by", adminUser(r))
	writeJSON(w, http.StatusOK, payload)
}

func (s *Server) handleListPayloads(w http.ResponseWriter, r *http.Request) {
	payloads, err := s.st.ListPayloads(r.Context())
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "could not list payloads", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"payloads": payloads})
}

func (s *Server) handleDeletePayload(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.st.DeletePayload(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, proto.Error{Error: "no such payload"})
			return
		}
		s.fail(w, http.StatusInternalServerError, "could not delete payload", err)
		return
	}
	// The row is gone either way; a leftover file is tidied best-effort.
	if err := os.Remove(s.payloadPath(id)); err != nil && !os.IsNotExist(err) {
		s.log.Error("payload row deleted but the file remains", "id", id, "error", err)
	}
	auditf(r, "file.delete", id, "uploaded file deleted")
	s.log.Info("payload deleted", "id", id, "by", adminUser(r))
	w.WriteHeader(http.StatusNoContent)
}

// handleAttachPayload links a file to a script, so every dispatch pushes it.
func (s *Server) handleAttachPayload(w http.ResponseWriter, r *http.Request) {
	var body struct {
		PayloadID string `json:"payload_id"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}

	scriptID := r.PathValue("id")
	if err := s.st.AttachPayload(r.Context(), scriptID, body.PayloadID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, proto.Error{Error: "no such script or payload"})
			return
		}
		s.fail(w, http.StatusInternalServerError, "could not attach the payload", err)
		return
	}

	script, err := s.st.GetScript(r.Context(), scriptID)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "could not read the script", err)
		return
	}
	s.log.Info("payload attachment changed", "script", scriptID,
		"payload", body.PayloadID, "by", adminUser(r))
	writeJSON(w, http.StatusOK, script)
}

// handleServePayload streams a job's attached file to the agent running it.
//
// It is on the agent API and authorised per job: a device may only fetch the
// file for work actually dispatched to it, so knowing a payload id is not
// enough to pull it.
func (s *Server) handleServePayload(w http.ResponseWriter, r *http.Request) {
	deviceID, token, ok := agentCredentials(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, proto.Error{Error: "missing agent credentials"})
		return
	}
	if err := s.st.Authenticate(r.Context(), deviceID, token); err != nil {
		writeJSON(w, http.StatusUnauthorized, proto.Error{Error: "unknown device or bad token"})
		return
	}

	jobID := r.PathValue("job")
	payload, err := s.st.PayloadForJob(r.Context(), jobID, deviceID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, proto.Error{Error: "no payload for this job"})
			return
		}
		s.fail(w, http.StatusInternalServerError, "could not read the payload", err)
		return
	}

	f, err := os.Open(s.payloadPath(payload.ID))
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "payload file is missing", err)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "could not stat the payload", err)
		return
	}

	s.log.Info("serving payload", "job", jobID, "device", deviceID,
		"filename", payload.Filename, "bytes", payload.Size)

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", payload.Filename))
	http.ServeContent(w, r, payload.Filename, info.ModTime(), f)
}

// randomID generates an opaque identifier for a stored payload.
func randomID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate id: %w", err)
	}
	return hex.EncodeToString(b), nil
}

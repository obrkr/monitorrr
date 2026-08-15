package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/ollie/monitorrr/internal/proto"
	"github.com/ollie/monitorrr/internal/store"
)

// progressInterval is how often an in-flight transfer's byte count is written
// back. Per-chunk updates would hammer SQLite for no visible benefit; once a
// second is well under what anyone perceives as stale on a progress bar.
const progressInterval = time.Second

// collectDir holds files pulled off devices, beside the database and the
// pushed payloads.
func (s *Server) collectDir() string {
	return filepath.Join(filepath.Dir(s.cfg.DBPath), "collected")
}

func (s *Server) collectPath(id string) string {
	return filepath.Join(s.collectDir(), filepath.Base(id))
}

// --- admin API ---

// handleRequestCollection asks a device for a file.
func (s *Server) handleRequestCollection(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path string `json:"path"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}

	deviceID := r.PathValue("id")
	by := adminUser(r)
	c, err := s.st.RequestCollection(r.Context(), deviceID, body.Path, by)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, proto.Error{Error: "no such device"})
			return
		}
		writeJSON(w, http.StatusBadRequest, proto.Error{Error: err.Error()})
		return
	}

	s.log.Info("file collection requested", "collection", c.ID,
		"device", c.DeviceHostname, "path", c.Path, "by", by)
	writeJSON(w, http.StatusOK, c)
}

// handleDownloadCollection serves a collected file to the operator.
func (s *Server) handleDownloadCollection(w http.ResponseWriter, r *http.Request) {
	c, err := s.st.GetCollection(r.Context(), r.PathValue("id"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, proto.Error{Error: "no such collection"})
			return
		}
		s.fail(w, http.StatusInternalServerError, "could not read collection", err)
		return
	}
	if c.State == store.CollectExpired {
		writeJSON(w, http.StatusGone, proto.Error{
			Error: "this file has passed its 7 day retention and was deleted"})
		return
	}
	if c.State != store.CollectDone {
		writeJSON(w, http.StatusConflict, proto.Error{
			Error: "the transfer has not finished: " + c.State})
		return
	}

	f, err := os.Open(s.collectPath(c.ID))
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "collected file is missing", err)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "could not stat the collected file", err)
		return
	}

	name := c.Filename
	if name == "" {
		name = filepath.Base(c.Path)
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name))
	http.ServeContent(w, r, name, info.ModTime(), f)
}

func (s *Server) handleDeleteCollection(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.st.DeleteCollection(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, proto.Error{Error: "no such collection"})
			return
		}
		s.fail(w, http.StatusInternalServerError, "could not delete collection", err)
		return
	}
	if err := os.Remove(s.collectPath(id)); err != nil && !os.IsNotExist(err) {
		s.log.Error("collection row deleted but the file remains", "id", id, "error", err)
	}
	s.log.Info("collection deleted", "id", id, "by", adminUser(r))
	w.WriteHeader(http.StatusNoContent)
}

// --- agent API ---

// handleCollectMeta records what the agent found when it looked at the file.
func (s *Server) handleCollectMeta(w http.ResponseWriter, r *http.Request) {
	deviceID, ok := s.authenticateAgent(w, r)
	if !ok {
		return
	}

	var meta proto.CollectMeta
	if !decodeJSON(w, r, &meta) {
		return
	}

	id := r.PathValue("id")
	if err := s.st.RecordCollectionSize(r.Context(), id, deviceID,
		meta.Filename, meta.Size, meta.Copied, meta.Error); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, proto.Error{Error: "no such collection for this device"})
			return
		}
		s.fail(w, http.StatusInternalServerError, "could not record file size", err)
		return
	}

	if meta.Error != "" {
		s.log.Warn("collection failed on the device", "collection", id, "error", meta.Error)
	} else {
		s.log.Info("collection size reported", "collection", id,
			"filename", meta.Filename, "bytes", meta.Size, "copied", meta.Copied)
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleCollectData receives the file itself.
func (s *Server) handleCollectData(w http.ResponseWriter, r *http.Request) {
	deviceID, ok := s.authenticateAgent(w, r)
	if !ok {
		return
	}

	id := r.PathValue("id")
	c, err := s.st.GetCollection(r.Context(), id)
	if err != nil || c.DeviceID != deviceID {
		writeJSON(w, http.StatusNotFound, proto.Error{Error: "no such collection for this device"})
		return
	}
	if c.State != store.CollectTransferring {
		writeJSON(w, http.StatusConflict, proto.Error{
			Error: "this collection is not awaiting data: " + c.State})
		return
	}

	if err := os.MkdirAll(s.collectDir(), 0o750); err != nil {
		s.fail(w, http.StatusInternalServerError, "could not create the collection directory", err)
		return
	}

	// Written under a temporary name and renamed on success, so a dropped
	// connection never leaves a half file that looks like a complete one.
	tmp := s.collectPath(id) + ".part"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "could not create the file", err)
		return
	}

	body := http.MaxBytesReader(w, r.Body, maxUpload)
	written, copyErr := s.copyWithProgress(r, id, f, body)
	closeErr := f.Close()

	if copyErr != nil {
		os.Remove(tmp)
		_ = s.st.FailCollection(r.Context(), id, deviceID, "transfer failed: "+copyErr.Error())
		writeJSON(w, http.StatusBadRequest, proto.Error{Error: "transfer failed: " + copyErr.Error()})
		return
	}
	if closeErr != nil {
		os.Remove(tmp)
		s.fail(w, http.StatusInternalServerError, "could not finish writing the file", closeErr)
		return
	}
	if err := os.Rename(tmp, s.collectPath(id)); err != nil {
		os.Remove(tmp)
		s.fail(w, http.StatusInternalServerError, "could not store the file", err)
		return
	}

	if err := s.st.UpdateCollectionProgress(r.Context(), id, written); err != nil {
		s.log.Error("could not record final progress", "collection", id, "error", err)
	}
	s.log.Info("collection data received", "collection", id, "bytes", written)
	w.WriteHeader(http.StatusNoContent)
}

// copyWithProgress streams the body to disk, recording progress as it goes.
func (s *Server) copyWithProgress(r *http.Request, id string, dst io.Writer, src io.Reader) (int64, error) {
	var (
		written int64
		last    = time.Now()
		buf     = make([]byte, 256*1024)
	)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return written, werr
			}
			written += int64(n)

			if time.Since(last) >= progressInterval {
				last = time.Now()
				// Deliberately not the request context: it is cancelled the
				// moment the transfer ends, and a progress write racing that
				// would fail for no reason.
				if uerr := s.st.UpdateCollectionProgress(r.Context(), id, written); uerr != nil {
					s.log.Debug("progress update skipped", "collection", id, "error", uerr)
				}
			}
		}
		if err == io.EOF {
			return written, nil
		}
		if err != nil {
			return written, err
		}
	}
}

// handleCollectComplete finalises a transfer once the agent confirms its digest.
func (s *Server) handleCollectComplete(w http.ResponseWriter, r *http.Request) {
	deviceID, ok := s.authenticateAgent(w, r)
	if !ok {
		return
	}

	var body struct {
		SHA256 string `json:"sha256"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}

	id := r.PathValue("id")
	info, err := os.Stat(s.collectPath(id))
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "collected file is missing", err)
		return
	}

	if err := s.st.CompleteCollection(r.Context(), id, deviceID, body.SHA256, info.Size()); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, proto.Error{Error: "no such collection for this device"})
			return
		}
		s.fail(w, http.StatusInternalServerError, "could not complete the collection", err)
		return
	}

	s.log.Info("collection complete", "collection", id, "device", deviceID,
		"bytes", info.Size(), "sha256", body.SHA256,
		"expires_in", store.CollectRetention.String())
	w.WriteHeader(http.StatusNoContent)
}

// authenticateAgent verifies device credentials and returns the device id.
func (s *Server) authenticateAgent(w http.ResponseWriter, r *http.Request) (string, bool) {
	id, token, ok := agentCredentials(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, proto.Error{Error: "missing agent credentials"})
		return "", false
	}
	if err := s.st.Authenticate(r.Context(), id, token); err != nil {
		writeJSON(w, http.StatusUnauthorized, proto.Error{Error: "unknown device or bad token"})
		return "", false
	}
	return id, true
}

// sweepCollections deletes collected files past their retention and fails
// transfers that stalled. The row survives deletion: knowing a file was pulled,
// by whom, and that it has since been removed is the point of the audit trail.
func (s *Server) sweepCollections(ctx context.Context) {
	expired, err := s.st.ExpiredCollections(ctx)
	if err != nil {
		s.log.Error("could not list expired collections", "error", err)
		return
	}
	for _, c := range expired {
		if err := os.Remove(s.collectPath(c.ID)); err != nil && !os.IsNotExist(err) {
			s.log.Error("could not delete an expired file", "collection", c.ID, "error", err)
			continue
		}
		if err := s.st.MarkCollectionExpired(ctx, c.ID); err != nil {
			s.log.Error("could not mark a collection expired", "collection", c.ID, "error", err)
			continue
		}
		s.log.Info("expired collected file deleted", "collection", c.ID,
			"path", c.Path, "device", c.DeviceHostname, "age", store.CollectRetention.String())
	}

	// A transfer that never completes would otherwise sit at "transferring"
	// forever, looking like it is still making progress.
	if n, err := s.st.SweepStalledCollections(ctx, 6*time.Hour); err != nil {
		s.log.Error("could not sweep stalled collections", "error", err)
	} else if n > 0 {
		s.log.Warn("marked stalled collections failed", "count", n)
	}
}

package agent

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
)

// maxPayload caps what an agent will accept, as a guard against a malformed or
// hostile response filling the endpoint's disk.
const maxPayload = 2 << 30 // 2 GiB

// downloadPayload streams a job's attached file to dest.
//
// It is written straight to disk rather than buffered: a pushed installer can
// be hundreds of megabytes, and holding that in memory on a small VM is how you
// get an agent killed by the OOM reaper.
func (a *Agent) downloadPayload(ctx context.Context, jobID, dest string) error {
	url := a.cfg.ServerURL + "/v1/payload/" + jobID
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("build payload request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+a.st.AgentID+":"+a.st.AgentToken)
	req.Header.Set("User-Agent", "monitorrr-agent/"+Version)

	// A large file over a slow link must not hit the client's normal timeout,
	// which is sized for small JSON exchanges.
	client := *a.client
	client.Timeout = 0
	res, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("fetch payload: %w", err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("server returned %d fetching the payload", res.StatusCode)
	}

	f, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("create payload file: %w", err)
	}
	defer f.Close()

	n, err := io.Copy(f, io.LimitReader(res.Body, maxPayload))
	if err != nil {
		return fmt.Errorf("write payload: %w", err)
	}
	if n == maxPayload {
		return fmt.Errorf("payload exceeds the %d byte limit", int64(maxPayload))
	}
	return f.Sync()
}

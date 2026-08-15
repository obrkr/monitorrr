package server

import (
	"net/http"
	"os"
	"strings"
	"testing"
)

// The installers are the one pair of endpoints an unauthenticated machine is
// meant to reach, so the token check is the only thing standing in front of
// them.
func TestInstallersRequireTheEnrollToken(t *testing.T) {
	srv, st := testServer(t)
	token, err := st.EnrollToken(t.Context())
	if err != nil {
		t.Fatalf("enroll token: %v", err)
	}

	for _, path := range []string{"/install.sh", "/install.ps1"} {
		if rec := do(srv, http.MethodGet, path, nil); rec.Code != http.StatusUnauthorized {
			t.Errorf("GET %s with no token = %d, want 401", path, rec.Code)
		}
		if rec := do(srv, http.MethodGet, path+"?token=wrong", nil); rec.Code != http.StatusUnauthorized {
			t.Errorf("GET %s with a wrong token = %d, want 401", path, rec.Code)
		}
		if rec := do(srv, http.MethodGet, path+"?token="+token, nil); rec.Code != http.StatusOK {
			t.Errorf("GET %s with the right token = %d, want 200", path, rec.Code)
		}
	}
}

// Each path must serve its own script: the shell installer reaching a Windows
// machine is not a subtle failure, but it is an easy routing mistake.
func TestInstallersServeTheScriptForTheirPath(t *testing.T) {
	srv, st := testServer(t)
	token, err := st.EnrollToken(t.Context())
	if err != nil {
		t.Fatalf("enroll token: %v", err)
	}

	sh := do(srv, http.MethodGet, "/install.sh?token="+token, nil).Body.String()
	if !strings.HasPrefix(sh, "#!/bin/sh") {
		t.Errorf("install.sh does not start with a shebang:\n%.80s", sh)
	}
	if !strings.Contains(sh, "/usr/local/bin") {
		t.Error("install.sh does not mention its install prefix")
	}

	ps := do(srv, http.MethodGet, "/install.ps1?token="+token, nil).Body.String()
	if strings.HasPrefix(ps, "#!/bin/sh") {
		t.Error("install.ps1 served the shell installer")
	}
	if !strings.Contains(ps, "Register-ScheduledTask") {
		t.Error("install.ps1 does not register the scheduled task")
	}

	// The token is what the machine authenticates with; an installer that
	// rendered without it would fail at enrollment rather than at install.
	for name, body := range map[string]string{"install.sh": sh, "install.ps1": ps} {
		if !strings.Contains(body, token) {
			t.Errorf("%s does not carry the enrollment token", name)
		}
	}
}

// The installer and the agent have to agree on where the binary lives. They
// have no shared constant — one is a PowerShell template, the other a Go
// variable — and a disagreement is silent: the agent declines to replace
// itself during an auto-update, and declines to delete its binary on
// retirement, because neither matches the path it considers canonical.
func TestWindowsInstallerAgreesWithTheAgentsInstallPath(t *testing.T) {
	const canonical = `C:\Program Files\monitorrr\monitorrr-agent.exe`

	agentSrc, err := os.ReadFile("../agent/update_windows.go")
	if err != nil {
		t.Fatalf("read the agent's install path: %v", err)
	}
	if !strings.Contains(string(agentSrc), canonical) {
		t.Fatalf("internal/agent/update_windows.go no longer installs to %s — update the installer template to match", canonical)
	}

	installer, err := os.ReadFile("web/install.ps1.tmpl")
	if err != nil {
		t.Fatalf("read the installer: %v", err)
	}
	// The template joins the directory and the file name, so check the halves.
	dir, file := `C:\Program Files\monitorrr`, "monitorrr-agent.exe"
	if !strings.Contains(string(installer), dir) || !strings.Contains(string(installer), file) {
		t.Errorf("install.ps1.tmpl does not install to %s", canonical)
	}
}

// Package proto defines the wire contract between agent and server.
//
// Every exchange is agent-initiated outbound HTTPS, so agents work behind NAT
// with no inbound firewall rules. The check-in response doubles as the control
// channel: the server piggybacks configuration changes (and, from milestone 2,
// queued jobs) onto the reply the agent is already waiting for.
package proto

// Version is the wire protocol version. Bumped only on breaking changes so an
// older agent can be told to update rather than failing in confusing ways.
const Version = 1

// EnrollRequest is sent once, the first time an agent starts on a machine.
type EnrollRequest struct {
	EnrollToken  string `json:"enroll_token"`
	Hostname     string `json:"hostname"`
	OS           string `json:"os"`
	Arch         string `json:"arch"`
	AgentVersion string `json:"agent_version"`
}

// EnrollResponse hands back the durable identity the agent stores on disk.
type EnrollResponse struct {
	AgentID    string `json:"agent_id"`
	AgentToken string `json:"agent_token"`
	Interval   int    `json:"interval_seconds"`
}

// Feature names an agent capability. Version numbers cannot answer "does this
// agent understand instruction X" — a build identifier is opaque, and an old
// agent silently ignores fields it does not know. Advertising capabilities
// explicitly lets the server tell "will never act on this" apart from "has not
// got round to it yet".
const (
	FeatureJobs     = "jobs"
	FeatureRetire   = "retire"
	FeaturePayloads = "payloads"
	FeatureCollect  = "collect"
)

// AgentFeatures is what this build of the agent advertises.
var AgentFeatures = []string{FeatureJobs, FeatureRetire, FeaturePayloads, FeatureCollect}

// CheckinRequest is the heartbeat, sent every Interval seconds.
type CheckinRequest struct {
	Hostname     string   `json:"hostname"`
	OS           string   `json:"os"`
	Arch         string   `json:"arch"`
	AgentVersion string   `json:"agent_version"`
	LocalIPs     []string `json:"local_ips"`
	// PublicIP is the address the device appears as on the internet, resolved
	// by the agent against an external service. The server cannot derive this
	// itself: it only sees the source address of the connection, which is a
	// private address whenever the agent is on the same network.
	PublicIP string `json:"public_ip,omitempty"`
	// Features is absent on agents predating capability advertisement, which is
	// itself the signal that they are old.
	Features []string `json:"features,omitempty"`
}

// Job is a script execution dispatched to an agent, delivered on the check-in
// response. SHA256 covers Script: the agent verifies it before executing, so a
// payload mangled in transit is refused rather than half-run.
type Job struct {
	ID          string `json:"id"`
	Interpreter string `json:"interpreter"` // "sh" | "powershell"
	Script      string `json:"script"`
	SHA256      string `json:"sha256"`
	TimeoutSecs int    `json:"timeout_seconds"`
	// PayloadName is set when a file must be pushed to the device before the
	// script runs — an installer or package. The agent fetches the bytes from
	// /v1/payload/{job id} and verifies them against PayloadSHA256.
	PayloadName   string `json:"payload_name,omitempty"`
	PayloadSHA256 string `json:"payload_sha256,omitempty"`
}

// JobResult is posted back to /v1/jobs/{id}/result once execution finishes.
type JobResult struct {
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	// Error is set when the script could not be run to completion at all —
	// timeout, missing interpreter, checksum mismatch — as distinct from a
	// script that ran and exited non-zero.
	Error      string `json:"error"`
	DurationMS int64  `json:"duration_ms"`
	Truncated  bool   `json:"truncated"`
}

// Collect asks the agent to send back a file from the device.
type Collect struct {
	ID   string `json:"id"`
	Path string `json:"path"`
}

// CollectMeta is what the agent reports after looking at the requested file,
// before any bytes are transferred. Size is sent first deliberately: an
// operator who has asked for something enormous by mistake should find out
// before the transfer starts, not after.
type CollectMeta struct {
	Filename string `json:"filename"`
	Size     int64  `json:"size"`
	// Copied is set when the file had to be duplicated to a temporary location
	// before it could be read — the path taken for locked or in-use files.
	Copied bool   `json:"copied"`
	Error  string `json:"error,omitempty"`
}

// CheckinResponse carries control data back to the agent.
type CheckinResponse struct {
	Interval int   `json:"interval_seconds"`
	Jobs     []Job `json:"jobs,omitempty"`
	// Collections are files to send back to the server.
	Collections []Collect `json:"collections,omitempty"`
	// Retire tells the agent to uninstall itself and stop. It is the opposite
	// of deleting a device record, which a running agent simply recovers from
	// by re-enrolling.
	Retire bool `json:"retire,omitempty"`
}

// Error is the body returned with any non-2xx response.
type Error struct {
	Error string `json:"error"`
}

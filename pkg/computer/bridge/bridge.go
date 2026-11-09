// Package bridge is the wire protocol between HelloSeal and the helloseal
// bridge running on a person's computer. The server and the client share it,
// so it imports nothing beyond the standard library and stays CGO-free.
//
// The bridge pairs with a device-code flow (DeviceStartRequest, then
// DevicePollRequest until the person confirms the code), then long-polls for
// a Call and answers each with a Result. Every request after pairing is
// signed with the computer's ed25519 key (Sign, Verify).
package bridge

import (
	"encoding/json"
	"time"
)

// Folder access.
const (
	AccessRead  = "read"
	AccessWrite = "write"
)

// Pairing statuses of DevicePollResponse.
const (
	StatusPending  = "pending"
	StatusApproved = "approved"
	StatusDenied   = "denied"
	StatusExpired  = "expired"
)

// CallError codes.
const (
	ErrorOutsideFolder = "outside_folder"
	ErrorNotFound      = "not_found"
	ErrorReadOnly      = "read_only"
	ErrorTooLarge      = "too_large"
	ErrorTimeout       = "timeout"
	ErrorFailed        = "failed"
)

// Timing of the bridge API.
const (
	// LongPollSeconds is how long GET /next waits for a call.
	LongPollSeconds = 25
	// HeartbeatSeconds is how often a bridge whose call slots are all busy
	// sends GET /next?wait=0&claim=false, so HelloSeal still sees it online.
	// The server updates last_seen_at and answers 204 without claiming.
	HeartbeatSeconds = 20
)

// PathPrefix is the route prefix of the bridge API on the server; signed
// paths are relative to it, such as /next.
const PathPrefix = "/computer-bridge"

// Bridge API paths, relative to PathPrefix.
const (
	PathDeviceStart = "/device/start"
	PathDevicePoll  = "/device/poll"
	PathNext        = "/next"
	PathResult      = "/result"
	PathDisconnect  = "/disconnect"
)

type DeviceStartRequest struct {
	Name    string `json:"name"`
	OS      string `json:"os"`
	Arch    string `json:"arch"`
	Version string `json:"version"`
	// PublicKey is the computer's ed25519 public key, standard base64.
	PublicKey string `json:"publicKey"`
	// Cwd is where helloseal connect ran; the confirm dialog offers it as
	// the first folder.
	Cwd string `json:"cwd"`
}

type DeviceStartResponse struct {
	DeviceCode string `json:"deviceCode"`
	// UserCode is XXXX-XXXX over A-Z and 2-9 (NewUserCode).
	UserCode                string `json:"userCode"`
	VerificationURI         string `json:"verificationUri"`
	VerificationURIComplete string `json:"verificationUriComplete"`
	// ExpiresIn and Interval are seconds.
	ExpiresIn int `json:"expiresIn"`
	Interval  int `json:"interval"`
}

type DevicePollRequest struct {
	DeviceCode string `json:"deviceCode"`
}

type DevicePollResponse struct {
	// Status is pending, approved, denied or expired.
	Status string `json:"status"`
	// ComputerID is set once, on the poll that first sees approved.
	ComputerID string `json:"computerId,omitempty"`
}

// Folder is one folder the person allowed on a computer. Every path a call
// uses must resolve, after symlinks, inside one of them.
type Folder struct {
	Path string `json:"path"`
	// Access is read or write.
	Access string `json:"access"`
}

// Call is one action the bridge runs. Op is a machine operation
// (workspace.MachineOps) and Args its input exactly as the openseal.computer
// action got it; the bridge ignores the action's computer argument. Folders
// are the computer's allowed folders at delivery.
type Call struct {
	ID             string          `json:"id"`
	Op             string          `json:"op"`
	Args           json.RawMessage `json:"args"`
	Folders        []Folder        `json:"folders"`
	DeadlineUnixMs int64           `json:"deadlineUnixMs"`
}

// Deadline is when the server stops waiting for this call's result.
func (c Call) Deadline() time.Time { return time.UnixMilli(c.DeadlineUnixMs) }

// Result answers a Call: Result holds the op's result when OK (the
// workspace result type of the op, such as workspace.ReadFileResult), Error
// why not.
type Result struct {
	ID     string          `json:"id"`
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *CallError      `json:"error,omitempty"`
}

// CallError is a failed call. Message is shown to the model, so it says what
// went wrong in plain words and never carries secrets; a machine
// operation's failure carries the same text a chat's would (workspace
// errors).
type CallError struct {
	// Code is outside_folder, not_found, read_only, too_large, timeout or
	// failed.
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *CallError) Error() string { return e.Message }

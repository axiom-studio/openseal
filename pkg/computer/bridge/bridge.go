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

// Ops a Call carries. They are the openseal.computer actions the bridge runs;
// list_computers is answered by the server.
const (
	OpRunCommand = "run_command"
	OpReadFile   = "read_file"
	OpWriteFile  = "write_file"
	OpListFiles  = "list_files"
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

// FileEntry types.
const (
	EntryFile    = "file"
	EntryDir     = "dir"
	EntrySymlink = "symlink"
)

// Limits shared by the action schemas, the server and the bridge.
const (
	// MaxStreamBytes is how much of stdout and of stderr a command keeps:
	// the last 64 KiB of each.
	MaxStreamBytes = 64 << 10
	// MaxReadFileBytes is the most content read_file returns.
	MaxReadFileBytes = 256 << 10
	// MaxListEntries is the most entries list_files returns.
	MaxListEntries = 2000

	DefaultCommandSeconds = 120
	MaxCommandSeconds     = 600
	DefaultReadLines      = 2000
	DefaultListDepth      = 1
	MaxListDepth          = 3

	// MaxCommandBytes bounds a command; MaxWriteBytes bounds write_file
	// content, which stays well under the 4 MB proxy body limit.
	MaxCommandBytes = 20000
	MaxWriteBytes   = 1 << 20
	MaxPathBytes    = 4096

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

// Call is one action the bridge runs. Args is the op's input (RunCommandInput,
// ReadFileInput, WriteFileInput or ListFilesInput); Folders are the
// computer's allowed folders at delivery.
type Call struct {
	ID             string          `json:"id"`
	Op             string          `json:"op"`
	Args           json.RawMessage `json:"args"`
	Folders        []Folder        `json:"folders"`
	DeadlineUnixMs int64           `json:"deadlineUnixMs"`
}

// Deadline is when the server stops waiting for this call's result.
func (c Call) Deadline() time.Time { return time.UnixMilli(c.DeadlineUnixMs) }

// Result answers a Call: Result holds the op's output when OK, Error why not.
type Result struct {
	ID     string          `json:"id"`
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *CallError      `json:"error,omitempty"`
}

// CallError is a failed call. Message is shown to the model, so it says what
// went wrong in plain words and never carries secrets.
type CallError struct {
	// Code is outside_folder, not_found, read_only, too_large, timeout or
	// failed.
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *CallError) Error() string { return e.Message }

// Action inputs. computer is optional in each: the server picks the person's
// default online computer, else the only online one. The bridge ignores it.

type RunCommandInput struct {
	Computer string `json:"computer,omitempty"`
	Command  string `json:"command"`
	// Cwd defaults to the first write folder.
	Cwd string `json:"cwd,omitempty"`
	// TimeoutSeconds defaults to DefaultCommandSeconds, at most
	// MaxCommandSeconds.
	TimeoutSeconds int `json:"timeoutSeconds,omitempty"`
}

type ReadFileInput struct {
	Computer string `json:"computer,omitempty"`
	Path     string `json:"path"`
	// Offset is the first line, 1-based; Limit how many lines (default
	// DefaultReadLines).
	Offset int `json:"offset,omitempty"`
	Limit  int `json:"limit,omitempty"`
}

type WriteFileInput struct {
	Computer string `json:"computer,omitempty"`
	Path     string `json:"path"`
	Content  string `json:"content"`
	Append   bool   `json:"append,omitempty"`
}

type ListFilesInput struct {
	Computer string `json:"computer,omitempty"`
	Path     string `json:"path"`
	// Depth defaults to DefaultListDepth, at most MaxListDepth.
	Depth int `json:"depth,omitempty"`
}

// Action outputs. The bridge leaves Computer empty; the server fills in the
// computer's current name.

type RunCommandOutput struct {
	Computer string `json:"computer"`
	Cwd      string `json:"cwd"`
	ExitCode int    `json:"exitCode"`
	// Stdout and Stderr are each the last MaxStreamBytes; Truncated says
	// either lost its start.
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	Truncated  bool   `json:"truncated"`
	DurationMs int64  `json:"durationMs"`
	TimedOut   bool   `json:"timedOut"`
}

type ReadFileOutput struct {
	Computer   string `json:"computer"`
	Path       string `json:"path"`
	Content    string `json:"content"`
	TotalLines int    `json:"totalLines"`
	Truncated  bool   `json:"truncated"`
}

type WriteFileOutput struct {
	Computer string `json:"computer"`
	Path     string `json:"path"`
	Bytes    int64  `json:"bytes"`
}

type FileEntry struct {
	Path string `json:"path"`
	// Type is file, dir or symlink.
	Type string `json:"type"`
	Size int64  `json:"size"`
}

type ListFilesOutput struct {
	Computer  string      `json:"computer"`
	Path      string      `json:"path"`
	Entries   []FileEntry `json:"entries"`
	Truncated bool        `json:"truncated"`
}

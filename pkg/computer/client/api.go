package client

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/axiom-studio/openseal/pkg/computer/bridge"
)

// ErrDisconnected means HelloSeal no longer knows this computer: it was
// disconnected in Settings, or its key no longer matches.
var ErrDisconnected = errors.New("This computer was disconnected. Run helloseal connect.")

const (
	maxCallBytes     = 8 << 20
	maxResponseBytes = 1 << 20
	resultAttempts   = 3
)

// API talks to the bridge routes of one HelloSeal server.
type API struct {
	// Base is <server>/rest/v1/helloseal/bridge.
	Base       string
	HTTP       *http.Client
	ComputerID string
	Key        ed25519.PrivateKey
	// Now is the clock requests are signed with; time.Now when nil.
	Now func() time.Time

	// offsetMs is how far HelloSeal's clock is ahead of Now, from the Date
	// of its last answer; 0 while within clockTolerance.
	offsetMs atomic.Int64
}

// clockTolerance is the clock difference ServerOffset ignores: a Date
// header is only accurate to the second.
const clockTolerance = 2 * time.Second

// NewAPI is an API for server, signing as computerID when key is set.
func NewAPI(server, computerID string, key ed25519.PrivateKey) *API {
	return &API{
		Base:       server + APIPath,
		HTTP:       &http.Client{Timeout: time.Duration(bridge.LongPollSeconds+20) * time.Second},
		ComputerID: computerID,
		Key:        key,
	}
}

// StatusError is a response the bridge did not expect.
type StatusError struct {
	Status int
	Body   string
}

func (e *StatusError) Error() string {
	if e.Body != "" {
		return fmt.Sprintf("HelloSeal answered %d: %s", e.Status, e.Body)
	}
	return fmt.Sprintf("HelloSeal answered %d", e.Status)
}

// DeviceStart begins pairing.
func (a *API) DeviceStart(ctx context.Context, request bridge.DeviceStartRequest) (bridge.DeviceStartResponse, error) {
	var response bridge.DeviceStartResponse
	err := a.do(ctx, http.MethodPost, bridge.PathDeviceStart, "", request, false, &response)
	return response, err
}

// DevicePoll asks whether the person confirmed the code yet.
func (a *API) DevicePoll(ctx context.Context, deviceCode string) (bridge.DevicePollResponse, error) {
	var response bridge.DevicePollResponse
	err := a.do(ctx, http.MethodPost, bridge.PathDevicePoll, "", bridge.DevicePollRequest{DeviceCode: deviceCode}, false, &response)
	return response, err
}

// Next waits up to wait for a call. It returns nil when none came, and
// ErrDisconnected when HelloSeal no longer knows this computer.
func (a *API) Next(ctx context.Context, wait int) (*bridge.Call, error) {
	var call bridge.Call
	if err := a.do(ctx, http.MethodGet, bridge.PathNext, "wait="+strconv.Itoa(wait), nil, true, &call); err != nil {
		if errors.Is(err, errNoContent) {
			return nil, nil
		}
		return nil, err
	}
	if call.ID == "" || call.Op == "" {
		return nil, errors.New("HelloSeal sent a call without an id or op")
	}
	return &call, nil
}

// Heartbeat tells HelloSeal this computer is still there without claiming
// a call, for while every call slot is busy.
func (a *API) Heartbeat(ctx context.Context) error {
	err := a.do(ctx, http.MethodGet, bridge.PathNext, "wait=0&claim=false", nil, true, nil)
	if errors.Is(err, errNoContent) {
		return nil
	}
	if err == nil {
		return errors.New("HelloSeal sent a call to a heartbeat")
	}
	return err
}

// ServerOffset is how far HelloSeal's clock is ahead of this computer's, as
// of its last answer; 0 when they agree to within a couple of seconds.
func (a *API) ServerOffset() time.Duration {
	return time.Duration(a.offsetMs.Load()) * time.Millisecond
}

func (a *API) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

// noteServerTime updates ServerOffset from an answer's Date header.
func (a *API) noteServerTime(date string, received time.Time) {
	at, err := http.ParseTime(date)
	if err != nil {
		return
	}
	// Date is truncated to the second; take the middle of that second.
	offset := at.Add(500 * time.Millisecond).Sub(received)
	if offset > -clockTolerance && offset < clockTolerance {
		offset = 0
	}
	a.offsetMs.Store(offset.Milliseconds())
}

// PostResult answers a call, trying up to three times.
func (a *API) PostResult(ctx context.Context, result bridge.Result) error {
	var err error
	for attempt := 0; attempt < resultAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt) * time.Second):
			}
		}
		err = a.do(ctx, http.MethodPost, bridge.PathResult, "", result, true, nil)
		if err == nil || errors.Is(err, errNoContent) {
			return nil
		}
		var status *StatusError
		if errors.Is(err, ErrDisconnected) || (errors.As(err, &status) && status.Status < 500) {
			return err
		}
	}
	return err
}

// Disconnect deletes this computer from HelloSeal.
func (a *API) Disconnect(ctx context.Context) error {
	err := a.do(ctx, http.MethodPost, bridge.PathDisconnect, "", struct{}{}, true, nil)
	if errors.Is(err, errNoContent) {
		return nil
	}
	return err
}

var errNoContent = errors.New("no content")

// do sends one request. path is relative to the bridge API and is what a
// signature covers; query is not signed.
func (a *API) do(ctx context.Context, method, path, query string, in any, signed bool, out any) error {
	var body []byte
	if in != nil {
		var err error
		if body, err = json.Marshal(in); err != nil {
			return err
		}
	}
	url := a.Base + path
	if query != "" {
		url += "?" + query
	}
	request, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	if in != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	request.Header.Set("Accept", "application/json")
	if signed {
		bridge.Sign(request.Header, a.Key, a.ComputerID, method, path, body, a.now())
	}
	response, err := a.HTTP.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if err := a.answer(response, signed); err != nil {
		return err
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxResponseBytes))
		return nil
	}
	limit := int64(maxResponseBytes)
	if path == bridge.PathNext {
		limit = maxCallBytes
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, limit)).Decode(out); err != nil {
		return fmt.Errorf("read HelloSeal's answer: %w", err)
	}
	return nil
}

// answer notes HelloSeal's clock from a response and turns a status the
// bridge does not take as success into an error.
func (a *API) answer(response *http.Response, signed bool) error {
	a.noteServerTime(response.Header.Get("Date"), a.now())
	switch {
	case response.StatusCode == http.StatusNoContent:
		return errNoContent
	case signed && (response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusNotFound):
		return ErrDisconnected
	case response.StatusCode < 200 || response.StatusCode > 299:
		text, _ := io.ReadAll(io.LimitReader(response.Body, 4<<10))
		return &StatusError{Status: response.StatusCode, Body: errorMessage(text)}
	}
	return nil
}

// Upload sends one file of a present call: the raw bytes, signed over their
// sha256 so HelloSeal checks the sender before reading them. The file is
// read twice, once to hash it and once to send it, so even the largest file
// never sits in memory; a file that changes in between is refused. It tries
// up to three times when the network or HelloSeal fails.
func (a *API) Upload(ctx context.Context, callID string, index int, path string, size int64, mediaType string) error {
	file, err := openRegular(path)
	if err != nil {
		return err
	}
	defer file.Close()
	hasher := sha256.New()
	if read, err := io.Copy(hasher, io.LimitReader(file, size+1)); err != nil {
		return err
	} else if read != size {
		return errFileChanged
	}
	digest := hex.EncodeToString(hasher.Sum(nil))
	signedPath := bridge.UploadPath(callID, index)
	if mediaType == "" {
		mediaType = "application/octet-stream"
	}
	// A large file on a slow link takes minutes: the call's deadline bounds
	// the upload, not the long-poll client's timeout.
	client := &http.Client{Transport: a.HTTP.Transport}
	for attempt := 0; attempt < resultAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt) * time.Second):
			}
		}
		if _, err = file.Seek(0, io.SeekStart); err != nil {
			return err
		}
		var body io.Reader = http.NoBody
		if size > 0 {
			body = io.LimitReader(file, size)
		}
		request, buildErr := http.NewRequestWithContext(ctx, http.MethodPost, a.Base+signedPath, body)
		if buildErr != nil {
			return buildErr
		}
		request.ContentLength = size
		request.Header.Set("Content-Type", mediaType)
		request.Header.Set("Accept", "application/json")
		request.Header.Set(bridge.HeaderContentSHA256, digest)
		bridge.SignDigest(request.Header, a.Key, a.ComputerID, http.MethodPost, signedPath, digest, a.now())
		response, sendErr := client.Do(request)
		if sendErr != nil {
			err = sendErr
			if ctx.Err() != nil {
				return ctx.Err()
			}
			continue
		}
		err = a.answer(response, true)
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxResponseBytes))
		response.Body.Close()
		if err == nil || errors.Is(err, errNoContent) {
			return nil
		}
		var status *StatusError
		if errors.Is(err, ErrDisconnected) || (errors.As(err, &status) && status.Status < 500) {
			return err
		}
	}
	return err
}

// errorMessage pulls a message out of a JSON error body, else returns it.
func errorMessage(body []byte) string {
	var parsed struct {
		Error   any    `json:"error"`
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &parsed) == nil {
		if parsed.Message != "" {
			return parsed.Message
		}
		if text, ok := parsed.Error.(string); ok && text != "" {
			return text
		}
	}
	return string(bytes.TrimSpace(body))
}

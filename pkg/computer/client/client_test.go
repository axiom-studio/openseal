package client

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/computer/bridge"
)

// fakeHelloSeal serves the bridge API the way HelloSeal does, verifying
// every signed request against the paired key.
type fakeHelloSeal struct {
	t          *testing.T
	mu         sync.Mutex
	publicKey  ed25519.PublicKey
	start      bridge.DeviceStartRequest
	polls      int
	calls      []bridge.Call
	results    chan bridge.Result
	nextStatus int
	nextPaths  []string
	// clockAhead sets every answer's Date this far ahead of the real time.
	clockAhead time.Duration
}

const testComputerID = "c-123"

func newFakeHelloSeal(t *testing.T) (*fakeHelloSeal, *httptest.Server) {
	fake := &fakeHelloSeal{t: t, results: make(chan bridge.Result, 8)}
	server := httptest.NewServer(http.StripPrefix(APIPath, fake))
	t.Cleanup(server.Close)
	return fake, server
}

func (f *fakeHelloSeal) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.clockAhead != 0 {
		w.Header().Set("Date", time.Now().Add(f.clockAhead).UTC().Format(http.TimeFormat))
	}
	switch r.URL.Path {
	case bridge.PathDeviceStart:
		if err := json.Unmarshal(body, &f.start); err != nil {
			f.t.Errorf("device start body: %v", err)
		}
		key, err := bridge.ParsePublicKey(f.start.PublicKey)
		if err != nil {
			f.t.Errorf("device start key: %v", err)
		}
		f.publicKey = key
		writeJSON(w, bridge.DeviceStartResponse{
			DeviceCode: "device-secret", UserCode: "ABCD-EF23",
			VerificationURI:         "https://helloseal.test/?settings=computers",
			VerificationURIComplete: "https://helloseal.test/?settings=computers&pair=ABCD-EF23",
			ExpiresIn:               600, Interval: 1,
		})
		return
	case bridge.PathDevicePoll:
		var poll bridge.DevicePollRequest
		_ = json.Unmarshal(body, &poll)
		if poll.DeviceCode != "device-secret" {
			http.Error(w, "unknown device code", http.StatusBadRequest)
			return
		}
		f.polls++
		if f.polls < 3 {
			writeJSON(w, bridge.DevicePollResponse{Status: bridge.StatusPending})
			return
		}
		writeJSON(w, bridge.DevicePollResponse{Status: bridge.StatusApproved, ComputerID: testComputerID})
		return
	}
	if bridge.SignedComputer(r.Header) != testComputerID {
		http.Error(w, "unknown computer", http.StatusUnauthorized)
		return
	}
	if err := bridge.Verify(r.Header, f.publicKey, r.Method, r.URL.Path, body, time.Now()); err != nil {
		f.t.Errorf("%s %s: %v", r.Method, r.URL.Path, err)
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	switch r.URL.Path {
	case bridge.PathNext:
		f.nextPaths = append(f.nextPaths, r.URL.RawQuery)
		if f.nextStatus != 0 {
			w.WriteHeader(f.nextStatus)
			return
		}
		if len(f.calls) == 0 || r.URL.Query().Get("claim") == "false" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		call := f.calls[0]
		f.calls = f.calls[1:]
		writeJSON(w, call)
	case bridge.PathResult:
		var result bridge.Result
		if err := json.Unmarshal(body, &result); err != nil {
			f.t.Errorf("result body: %v", err)
		}
		f.results <- result
		w.WriteHeader(http.StatusNoContent)
	case bridge.PathDisconnect:
		w.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(w, r)
	}
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}

func testStore(t *testing.T) Store {
	return Store{Dir: filepath.Join(t.TempDir(), "helloseal")}
}

func TestPairWaitsForApprovalAndSavesConnection(t *testing.T) {
	pairSecond = time.Millisecond
	t.Cleanup(func() { pairSecond = time.Second })
	fake, server := newFakeHelloSeal(t)
	store := testStore(t)
	var out strings.Builder
	var opened string

	config, err := Pair(context.Background(), store, PairOptions{
		Server: server.URL, Name: "laptop", Version: "1.2.3", Cwd: "/home/me/project",
		Out: &out, OpenBrowser: func(url string) error { opened = url; return nil },
	})
	if err != nil {
		t.Fatalf("Pair: %v", err)
	}
	if config != (Config{Server: server.URL, ComputerID: testComputerID, Name: "laptop"}) {
		t.Fatalf("config = %+v", config)
	}
	if fake.polls != 3 {
		t.Fatalf("polls = %d, want 2 pending then approved", fake.polls)
	}
	if fake.start.Name != "laptop" || fake.start.Cwd != "/home/me/project" || fake.start.Version != "1.2.3" || fake.start.OS != runtime.GOOS {
		t.Fatalf("start request = %+v", fake.start)
	}
	if !strings.Contains(out.String(), "ABCD-EF23") || opened != "https://helloseal.test/?settings=computers&pair=ABCD-EF23" {
		t.Fatalf("printed %q, opened %q", out.String(), opened)
	}
	saved, err := store.Load()
	if err != nil || saved != config {
		t.Fatalf("saved = %+v, %v", saved, err)
	}
	key, err := store.Key()
	if err != nil || !key.Public().(ed25519.PublicKey).Equal(fake.publicKey) {
		t.Fatalf("saved key does not match the paired key: %v", err)
	}
	if runtime.GOOS != "windows" {
		for _, name := range []string{configFile, keyFile} {
			info, err := os.Stat(filepath.Join(store.Dir, name))
			if err != nil || info.Mode().Perm() != 0o600 {
				t.Fatalf("%s mode = %v, %v; want 0600", name, info.Mode().Perm(), err)
			}
		}
	}
}

func TestPairReportsDenied(t *testing.T) {
	pairSecond = time.Millisecond
	t.Cleanup(func() { pairSecond = time.Second })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, bridge.PathDeviceStart) {
			writeJSON(w, bridge.DeviceStartResponse{DeviceCode: "d", UserCode: "ABCD-EF23", Interval: 1, ExpiresIn: 600})
			return
		}
		writeJSON(w, bridge.DevicePollResponse{Status: bridge.StatusDenied})
	}))
	defer server.Close()
	store := testStore(t)
	if _, err := Pair(context.Background(), store, PairOptions{Server: server.URL, Name: "laptop"}); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("Pair = %v, want refused", err)
	}
	if _, err := store.Load(); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("a refused pairing saved a connection: %v", err)
	}
}

func pairedAPI(t *testing.T, fake *fakeHelloSeal, server *httptest.Server) *API {
	_, key, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	fake.publicKey = key.Public().(ed25519.PublicKey)
	return NewAPI(server.URL, testComputerID, key)
}

func TestServeRunsACommandAndAnswersSigned(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses /bin/sh")
	}
	fake, server := newFakeHelloSeal(t)
	api := pairedAPI(t, fake, server)
	folder := t.TempDir()
	args, _ := json.Marshal(bridge.RunCommandInput{Command: "printf 'hi from %s' \"$(basename \"$PWD\")\"; echo oops >&2; exit 3"})
	fake.calls = []bridge.Call{{
		ID: "call-1", Op: bridge.OpRunCommand, Args: args,
		Folders:        []bridge.Folder{{Path: folder, Access: bridge.AccessWrite}},
		DeadlineUnixMs: time.Now().Add(time.Minute).UnixMilli(),
	}}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, api, ServeOptions{}) }()
	var result bridge.Result
	select {
	case result = <-fake.results:
	case <-time.After(10 * time.Second):
		t.Fatal("no result")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Serve = %v", err)
	}
	if !result.OK || result.ID != "call-1" {
		t.Fatalf("result = %+v", result)
	}
	var output bridge.RunCommandOutput
	if err := json.Unmarshal(result.Result, &output); err != nil {
		t.Fatal(err)
	}
	resolved, _ := filepath.EvalSymlinks(folder)
	if output.ExitCode != 3 || output.Stdout != "hi from "+filepath.Base(folder) || output.Stderr != "oops\n" || output.Cwd != resolved || output.TimedOut {
		t.Fatalf("output = %+v", output)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.nextPaths[0] != "wait=25" {
		t.Fatalf("next query = %q", fake.nextPaths[0])
	}
}

func TestServeStopsWhenHelloSealForgotTheComputer(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusNotFound} {
		fake, server := newFakeHelloSeal(t)
		api := pairedAPI(t, fake, server)
		fake.nextStatus = status
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := Serve(ctx, api, ServeOptions{})
		cancel()
		if !errors.Is(err, ErrDisconnected) {
			t.Fatalf("status %d: Serve = %v, want ErrDisconnected", status, err)
		}
		if err.Error() != "This computer was disconnected. Run helloseal connect." {
			t.Fatalf("message = %q", err.Error())
		}
	}
}

func TestServeBacksOffOnServerErrors(t *testing.T) {
	fake, server := newFakeHelloSeal(t)
	api := pairedAPI(t, fake, server)
	fake.nextStatus = http.StatusBadGateway
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if err := Serve(ctx, api, ServeOptions{Backoff: 20 * time.Millisecond}); err != nil {
		t.Fatalf("Serve = %v", err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	// 20+40+80+160ms of backoff fit in 300ms: a handful of polls, not a spin.
	if n := len(fake.nextPaths); n < 2 || n > 6 {
		t.Fatalf("polled %d times", n)
	}
}

func TestServeHeartbeatsWhileEverySlotIsBusy(t *testing.T) {
	fake, server := newFakeHelloSeal(t)
	api := pairedAPI(t, fake, server)
	for i := 0; i <= Concurrency; i++ {
		fake.calls = append(fake.calls, bridge.Call{ID: "call-" + strconv.Itoa(i), Op: bridge.OpListFiles, DeadlineUnixMs: time.Now().Add(time.Minute).UnixMilli()})
	}
	release := make(chan struct{})
	started := make(chan string, Concurrency+1)
	execute := func(_ context.Context, call bridge.Call) bridge.Result {
		started <- call.ID
		<-release
		return bridge.Result{ID: call.ID, OK: true, Result: json.RawMessage("{}")}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, api, ServeOptions{Execute: execute, Heartbeat: 20 * time.Millisecond}) }()
	for i := 0; i < Concurrency; i++ {
		<-started
	}

	// Every slot is busy: HelloSeal keeps hearing from the computer, and the
	// fifth call stays queued there.
	heartbeats := func() int {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		n := 0
		for _, query := range fake.nextPaths {
			if query == "wait=0&claim=false" {
				n++
			}
		}
		return n
	}
	for wait := time.Now().Add(5 * time.Second); heartbeats() < 3; {
		if time.Now().After(wait) {
			t.Fatalf("heartbeats = %d", heartbeats())
		}
		time.Sleep(10 * time.Millisecond)
	}
	fake.mu.Lock()
	queued := len(fake.calls)
	fake.mu.Unlock()
	if queued != 1 || len(started) != 0 {
		t.Fatalf("queued %d, started %d extra", queued, len(started))
	}

	close(release)
	for i := 0; i <= Concurrency; i++ {
		select {
		case <-fake.results:
		case <-time.After(5 * time.Second):
			t.Fatalf("got %d results", i)
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Serve = %v", err)
	}
}

func TestServeRunsCallsOnThisComputersClock(t *testing.T) {
	fake, server := newFakeHelloSeal(t)
	api := pairedAPI(t, fake, server)
	// HelloSeal's clock is a minute ahead and gives the call 30s.
	fake.clockAhead = time.Minute
	fake.calls = []bridge.Call{{ID: "call-1", Op: bridge.OpListFiles, DeadlineUnixMs: time.Now().Add(time.Minute + 30*time.Second).UnixMilli()}}
	deadlines := make(chan time.Time, 1)
	execute := func(_ context.Context, call bridge.Call) bridge.Result {
		deadlines <- call.Deadline()
		return bridge.Result{ID: call.ID, OK: true, Result: json.RawMessage("{}")}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, api, ServeOptions{Execute: execute}) }()
	var deadline time.Time
	select {
	case deadline = <-deadlines:
	case <-time.After(5 * time.Second):
		t.Fatal("not run")
	}
	cancel()
	<-done
	if left := time.Until(deadline); left < 27*time.Second || left > 32*time.Second {
		t.Fatalf("time left = %s, want about 30s", left)
	}
	if offset := api.ServerOffset(); offset < 58*time.Second || offset > 62*time.Second {
		t.Fatalf("offset = %s", offset)
	}
}

func TestDisconnectDeletesTheConnection(t *testing.T) {
	fake, server := newFakeHelloSeal(t)
	store := testStore(t)
	key, err := store.KeyOrCreate()
	if err != nil {
		t.Fatal(err)
	}
	fake.publicKey = key.Public().(ed25519.PublicKey)
	config := Config{Server: server.URL, ComputerID: testComputerID, Name: "laptop"}
	if err := store.Save(config); err != nil {
		t.Fatal(err)
	}
	if err := Disconnect(context.Background(), store, config); err != nil {
		t.Fatalf("Disconnect: %v", err)
	}
	if _, err := store.Load(); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("config survived: %v", err)
	}
	if _, err := store.Key(); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("key survived: %v", err)
	}
}

func TestResolveServerOrder(t *testing.T) {
	t.Setenv(ServerEnv, "")
	if got := ResolveServer("", ""); got != DefaultServer {
		t.Fatalf("default = %q", got)
	}
	if got := ResolveServer("", "https://saved.test/"); got != "https://saved.test" {
		t.Fatalf("saved = %q", got)
	}
	t.Setenv(ServerEnv, "https://env.test")
	if got := ResolveServer("", "https://saved.test"); got != "https://env.test" {
		t.Fatalf("env = %q", got)
	}
	if got := ResolveServer("https://flag.test", "https://saved.test"); got != "https://flag.test" {
		t.Fatalf("flag = %q", got)
	}
}

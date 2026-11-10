package client

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/computer/bridge"
)

// recordingUploader keeps what Present sends instead of sending it.
type recordingUploader struct {
	sent []bridge.PresentedFile
	data [][]byte
	fail error
}

func (u *recordingUploader) Upload(_ context.Context, callID string, index int, path string, size int64, mediaType string) error {
	if u.fail != nil {
		return u.fail
	}
	if callID != "c" || index != len(u.sent) {
		return errors.New("uploaded out of order")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	u.sent = append(u.sent, bridge.PresentedFile{Path: path, Size: size, MediaType: mediaType})
	u.data = append(u.data, data)
	return nil
}

func present(t *testing.T, w folderSet, upload Uploader, paths ...string) bridge.Result {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"computer": "Laptop", "paths": paths})
	if err != nil {
		t.Fatal(err)
	}
	return Present(context.Background(), bridge.Call{ID: "c", Op: bridge.OpPresent, Args: raw, Folders: w.folders,
		DeadlineUnixMs: time.Now().Add(time.Minute).UnixMilli()}, upload)
}

func TestPresentSendsEachFileThenAnswers(t *testing.T) {
	w := newWorkspace(t)
	pdf := filepath.Join(w.write, "q3.pdf")
	mustWrite(t, pdf, "%PDF-1.7 report")
	upload := &recordingUploader{}
	// A read-only folder is enough: present changes nothing on the computer.
	result := present(t, w, upload, pdf, filepath.Join(w.read, "notes.txt"))
	if !result.OK {
		t.Fatalf("present = %+v", result.Error)
	}
	var output bridge.PresentResult
	if err := json.Unmarshal(result.Result, &output); err != nil {
		t.Fatal(err)
	}
	want := []bridge.PresentedFile{
		{Path: pdf, Size: 15, MediaType: "application/pdf"},
		{Path: filepath.Join(w.read, "notes.txt"), Size: 14, MediaType: "text/plain"},
	}
	if len(output.Files) != 2 || output.Files[0] != want[0] || output.Files[1] != want[1] {
		t.Fatalf("files = %+v", output.Files)
	}
	if len(upload.sent) != 2 || string(upload.data[0]) != "%PDF-1.7 report" {
		t.Fatalf("sent = %+v", upload.sent)
	}
	// The result names the files; the bytes went with the uploads only.
	if len(result.Result) > 512 {
		t.Fatalf("the result carries %d bytes", len(result.Result))
	}
}

func TestPresentChecksEveryPathBeforeSendingAny(t *testing.T) {
	w := newWorkspace(t)
	good := filepath.Join(w.write, "good.pdf")
	mustWrite(t, good, "%PDF")
	large := filepath.Join(w.write, "huge.mov")
	if err := os.WriteFile(large, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	// Sparse: as large as it looks without writing it.
	if err := os.Truncate(large, bridge.MaxPresentBytes+1); err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		path string
		code string
	}{
		"outside":   {filepath.Join(w.outside, "key.txt"), bridge.ErrorOutsideFolder},
		"missing":   {filepath.Join(w.write, "nope.pdf"), bridge.ErrorNotFound},
		"folder":    {w.read, bridge.ErrorFailed},
		"too large": {large, bridge.ErrorTooLarge},
	}
	if runtime.GOOS != "windows" {
		link := filepath.Join(w.write, "key-link.txt")
		if err := os.Symlink(filepath.Join(w.outside, "key.txt"), link); err != nil {
			t.Fatal(err)
		}
		cases["link out of the folder"] = struct {
			path string
			code string
		}{link, bridge.ErrorOutsideFolder}
	}
	for name, c := range cases {
		upload := &recordingUploader{}
		result := present(t, w, upload, good, c.path)
		wantError(t, result, c.code)
		if len(upload.sent) != 0 {
			t.Errorf("%s: sent %d files before refusing", name, len(upload.sent))
		}
	}
	upload := &recordingUploader{}
	if result := present(t, w, upload, large); result.Error == nil || result.Error.Message != large+" is 50.0 MB, more than the 50 MB a file shown in the chat may be." {
		t.Fatalf("too large = %+v", result.Error)
	}
	if result := present(t, w, &recordingUploader{fail: &StatusError{Status: http.StatusConflict, Body: "this call is no longer waiting for a result"}}, good); result.Error == nil ||
		result.Error.Message != "Could not send "+good+" to HelloSeal: this call is no longer waiting for a result" {
		t.Fatalf("refused upload = %+v", result.Error)
	}
	if result := Present(context.Background(), bridge.Call{ID: "c", Op: bridge.OpPresent, Args: json.RawMessage(`{"paths":[]}`), Folders: w.folders}, upload); result.OK {
		t.Fatal("present with no paths ran")
	}
}

// Serve runs a present call by uploading each file with its own signed
// request, then posting a result that names them.
func TestServePresentsThroughSignedUploads(t *testing.T) {
	fake, server := newFakeHelloSeal(t)
	api := pairedAPI(t, fake, server)
	w := newWorkspace(t)
	image := filepath.Join(w.write, "chart.png")
	data := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 64<<10)...)
	if err := os.WriteFile(image, data, 0o644); err != nil {
		t.Fatal(err)
	}
	args, _ := json.Marshal(map[string]any{"paths": []string{image, filepath.Join(w.read, "notes.txt")}})
	fake.calls = []bridge.Call{{ID: "call-9", Op: bridge.OpPresent, Args: args, Folders: w.folders, DeadlineUnixMs: time.Now().Add(time.Minute).UnixMilli()}}

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
	if !result.OK {
		t.Fatalf("result = %+v", result.Error)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	first, second := fake.uploads[bridge.UploadPath("call-9", 0)], fake.uploads[bridge.UploadPath("call-9", 1)]
	if string(first.body) != string(data) || first.contentType != "image/png" || string(second.body) != "one\ntwo\nthree\n" || len(fake.uploads) != 2 {
		t.Fatalf("uploads = %d, first %d bytes as %q", len(fake.uploads), len(first.body), first.contentType)
	}
}

func TestUploadSignatureCoversTheCallAndBytes(t *testing.T) {
	fake, server := newFakeHelloSeal(t)
	api := pairedAPI(t, fake, server)
	path := filepath.Join(t.TempDir(), "a.txt")
	mustWrite(t, path, "hello")
	request := func(callID string, index int) http.Header {
		header := http.Header{}
		bridge.SignDigest(header, api.Key, testComputerID, http.MethodPost, bridge.UploadPath(callID, index), bridge.BodySHA256([]byte("hello")), time.Now())
		return header
	}
	header := request("call-1", 0)
	if err := bridge.VerifyDigest(header, fake.publicKey, http.MethodPost, bridge.UploadPath("call-1", 0), bridge.BodySHA256([]byte("hello")), time.Now(), bridge.MaxUploadAge); err != nil {
		t.Fatal(err)
	}
	for name, check := range map[string]func() error{
		"another call": func() error {
			return bridge.VerifyDigest(header, fake.publicKey, http.MethodPost, bridge.UploadPath("call-2", 0), bridge.BodySHA256([]byte("hello")), time.Now(), bridge.MaxUploadAge)
		},
		"another index": func() error {
			return bridge.VerifyDigest(header, fake.publicKey, http.MethodPost, bridge.UploadPath("call-1", 1), bridge.BodySHA256([]byte("hello")), time.Now(), bridge.MaxUploadAge)
		},
		"other bytes": func() error {
			return bridge.VerifyDigest(header, fake.publicKey, http.MethodPost, bridge.UploadPath("call-1", 0), bridge.BodySHA256([]byte("hellO")), time.Now(), bridge.MaxUploadAge)
		},
	} {
		if err := check(); !errors.Is(err, bridge.ErrInvalidSignature) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A slow upload is still taken minutes later; a request that is not an
	// upload keeps the short window.
	if err := bridge.VerifyDigest(header, fake.publicKey, http.MethodPost, bridge.UploadPath("call-1", 0), bridge.BodySHA256([]byte("hello")), time.Now().Add(10*time.Minute), bridge.MaxUploadAge); err != nil {
		t.Fatalf("slow upload: %v", err)
	}
	if err := bridge.VerifyDigest(header, fake.publicKey, http.MethodPost, bridge.UploadPath("call-1", 0), bridge.BodySHA256([]byte("hello")), time.Now().Add(10*time.Minute), bridge.MaxClockSkew); !errors.Is(err, bridge.ErrStaleTimestamp) {
		t.Fatalf("stale = %v", err)
	}

	if err := api.Upload(t.Context(), "call-1", 0, path, 5, "text/plain"); err != nil {
		t.Fatal(err)
	}
	if got := fake.uploads[bridge.UploadPath("call-1", 0)]; string(got.body) != "hello" {
		t.Fatalf("upload = %q", got.body)
	}
	// A file that changed size since it was checked is not sent.
	if err := api.Upload(t.Context(), "call-1", 1, path, 4, "text/plain"); !errors.Is(err, errFileChanged) {
		t.Fatalf("changed file = %v", err)
	}
}

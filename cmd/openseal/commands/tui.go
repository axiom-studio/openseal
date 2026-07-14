package commands

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/internal/tui"
	"github.com/axiom-studio/openseal/pkg/client"
	"github.com/axiom-studio/openseal/pkg/runtime"
)

func tuiCmd(args []string) {
	defaults := tui.DefaultConfig()
	if endpoint := strings.TrimSpace(os.Getenv("OPENSEAL_API_URL")); endpoint != "" {
		defaults.Endpoint = endpoint
	}
	fs := flag.NewFlagSet("tui", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	endpoint := fs.String("endpoint", defaults.Endpoint, "OpenSeal kernel API URL")
	requestHeaders := make(requestHeaderFlags)
	fs.Var(&requestHeaders, "header", "non-secret host selector header as name=value; may be repeated")
	scope := fs.String("scope", defaults.Scope.Kind+":"+defaults.Scope.ID, "workspace scope as kind:id")
	owner := fs.String("owner", string(defaults.Owner.Type)+":"+defaults.Owner.ID, "work owner as agent:id or team:id")
	downloadDir := fs.String("download-dir", defaults.DownloadDir, "directory for verified artifact downloads")
	poll := fs.Duration("poll", defaults.PollInterval, "run refresh interval; use a negative duration to disable")
	help := fs.Bool("help", false, "print help for the terminal UI")
	if err := fs.Parse(args); err != nil {
		return
	}
	if *help {
		fmt.Println(`Usage: openseal tui [options]

Open the prompt-first OpenSeal terminal interface. The TUI is a thin client;
start "openseal daemon" separately so durable work remains active after exit.

Options:
  --endpoint <url>   Kernel origin or explicit versioned API root (default: http://127.0.0.1:8080)
  --header <n=v>     Non-secret host selector header; may be repeated
  --scope <kind:id>  Workspace scope (default: local:default)
  --owner <type:id>  Agent or Team that owns new work (default: agent:operator)
  --download-dir     Directory for verified artifact downloads (default: artifacts)
  --poll <duration>  Refresh interval (default: 5s; negative disables polling)
  --help             Print this help message

OPENSEAL_API_URL can set the default endpoint.`)
		return
	}

	scopeValue, err := parseScope(*scope)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid --scope: %v\n", err)
		return
	}
	ownerValue, err := parseOwner(*owner)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid --owner: %v\n", err)
		return
	}
	pollInterval := *poll
	if pollInterval < 0 {
		pollInterval = time.Duration(-1)
	}
	config := tui.Config{
		Endpoint: strings.TrimSpace(*endpoint), Scope: scopeValue, Owner: ownerValue,
		Actor: runtime.ActivityActor{Type: "user", ID: "local"}, DownloadDir: strings.TrimSpace(*downloadDir),
		PollInterval: pollInterval,
	}
	kernelClient := client.NewKernelHTTPClient(config.Endpoint, nil, client.WithRequestHeaders(http.Header(requestHeaders)))
	if err := tui.Run(context.Background(), kernelClient, config); err != nil {
		fmt.Fprintf(os.Stderr, "OpenSeal TUI failed: %v\n", err)
	}
}

type requestHeaderFlags http.Header

func (headers *requestHeaderFlags) String() string {
	if headers == nil {
		return ""
	}
	return fmt.Sprintf("%d configured", len(*headers))
}

func (headers *requestHeaderFlags) Set(value string) error {
	name, headerValue, ok := strings.Cut(value, "=")
	name = http.CanonicalHeaderKey(strings.TrimSpace(name))
	headerValue = strings.TrimSpace(headerValue)
	if !ok || name == "" || headerValue == "" || strings.ContainsAny(name, " \t\r\n:") || strings.ContainsAny(headerValue, "\r\n") {
		return fmt.Errorf("expected a valid non-empty name=value header")
	}
	if name == "Content-Type" || name == "Idempotency-Key" {
		return fmt.Errorf("%s is owned by the kernel protocol", name)
	}
	if *headers == nil {
		*headers = make(requestHeaderFlags)
	}
	http.Header(*headers).Add(name, headerValue)
	return nil
}

func parseScope(value string) (runtime.Scope, error) {
	kind, id, ok := strings.Cut(strings.TrimSpace(value), ":")
	scope := runtime.Scope{Kind: strings.TrimSpace(kind), ID: strings.TrimSpace(id)}
	if !ok || scope.Validate() != nil {
		return runtime.Scope{}, fmt.Errorf("expected kind:id")
	}
	return scope, nil
}

func parseOwner(value string) (runtime.ObjectiveOwner, error) {
	kind, id, ok := strings.Cut(strings.TrimSpace(value), ":")
	owner := runtime.ObjectiveOwner{Type: runtime.OwnerType(strings.TrimSpace(kind)), ID: strings.TrimSpace(id)}
	if !ok || owner.Validate() != nil {
		return runtime.ObjectiveOwner{}, fmt.Errorf("expected agent:id or team:id")
	}
	return owner, nil
}

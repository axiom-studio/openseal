package commands

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestReleaseLdflagsStampTheReportedVersion builds the real binary the way
// release builds do, so a wrong -X package path cannot silently ship v0.0.0-dev.
func TestReleaseLdflagsStampTheReportedVersion(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the openseal binary")
	}
	for _, test := range []struct{ ldflags, want string }{
		{ldflags: "-X github.com/axiom-studio/openseal/cmd/openseal/commands.version=1.2.3-rc.1", want: "OpenSeal v1.2.3-rc.1"},
		{want: "OpenSeal v0.0.0-dev"},
	} {
		binary := filepath.Join(t.TempDir(), "openseal")
		build := exec.Command("go", "build", "-ldflags", test.ldflags, "-o", binary, "github.com/axiom-studio/openseal/cmd/openseal")
		if output, err := build.CombinedOutput(); err != nil {
			t.Fatalf("build with ldflags %q: %v\n%s", test.ldflags, err, output)
		}
		output, err := exec.Command(binary, "version").Output()
		if err != nil || strings.TrimSpace(string(output)) != test.want {
			t.Fatalf("ldflags %q: version = %q, %v; want %q", test.ldflags, output, err, test.want)
		}
	}
}

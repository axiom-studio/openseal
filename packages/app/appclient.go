// Package appclient supplies the @openseal/app TypeScript sources to hosts
// that bundle apps, so an app's import of "@openseal/app" or
// "@openseal/app/react" compiles from the same code this package publishes,
// with no package install.
package appclient

import (
	"embed"
	"io/fs"
	"strings"
)

//go:embed src/index.ts src/protocol.ts src/react.ts src/transport.ts
var sources embed.FS

// Module is the import name an app uses.
const Module = "@openseal/app"

// Entries maps each import path an app may use to its source file.
var Entries = map[string]string{
	Module:               "src/index.ts",
	Module + "/react":    "src/react.ts",
	Module + "/protocol": "src/protocol.ts",
}

// Source returns a file's TypeScript source by its path within the package.
func Source(name string) (string, bool) {
	if !strings.HasPrefix(name, "src/") || strings.HasSuffix(name, ".test.ts") {
		return "", false
	}
	data, err := fs.ReadFile(sources, name)
	if err != nil {
		return "", false
	}
	return string(data), true
}

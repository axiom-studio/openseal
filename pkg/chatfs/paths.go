package chatfs

import "github.com/axiom-studio/openseal/pkg/chatpath"

// The path rules live in pkg/chatpath; these names keep chatfs readable.
var (
	Clean          = chatpath.Clean
	CleanDirectory = chatpath.CleanDirectory
	FromImport     = chatpath.FromImport
	MediaTypeOf    = chatpath.MediaTypeOf
	BaseMediaType  = chatpath.BaseMediaType
	Textual        = chatpath.Textual
	Folder         = chatpath.Folder
	Slug           = chatpath.Slug
	ErrPath        = chatpath.ErrPath
)

const (
	Apps    = chatpath.Apps
	Uploads = chatpath.Uploads
	Slides  = chatpath.Slides
	Pages   = chatpath.Pages
	Sheets  = chatpath.Sheets
	Images  = chatpath.Images
	Docs    = chatpath.Docs
	Media   = chatpath.Media
	Files   = chatpath.Files
)

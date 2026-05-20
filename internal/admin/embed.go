package admin

import (
	"embed"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
)

//go:embed html
var htmlFS embed.FS

// assetsFS returns the sub-filesystem rooted at html/.
func assetsFS() fs.FS {
	sub, err := fs.Sub(htmlFS, "html")
	if err != nil {
		// embed.FS guarantees html/ exists at build time; this is a programmer error.
		panic("admin: embed html sub-fs: " + err.Error())
	}
	return sub
}

// contentTypeFor returns a Content-Type for the given path, falling back to
// mime.TypeByExtension and finally to application/octet-stream.
func contentTypeFor(p string) string {
	ext := strings.ToLower(path.Ext(p))
	switch ext {
	case ".html", ".htm":
		return "text/html; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".js", ".mjs":
		return "application/javascript; charset=utf-8"
	case ".json":
		return "application/json; charset=utf-8"
	case ".svg":
		return "image/svg+xml"
	case ".png":
		return "image/png"
	case ".webp":
		return "image/webp"
	case ".woff2":
		return "font/woff2"
	}
	if ct := mime.TypeByExtension(ext); ct != "" {
		return ct
	}
	return "application/octet-stream"
}

// serveAsset reads the named asset (relative to html/) from the embed FS and
// writes it with the correct Content-Type. Missing assets produce 404.
func serveAsset(w http.ResponseWriter, _ *http.Request, name string) {
	data, err := fs.ReadFile(assetsFS(), name)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", contentTypeFor(name))
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(data)
}

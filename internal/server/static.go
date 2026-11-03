package server

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"net/http"
	"strings"
)

// assets are the files index.html references; each gets a content hash.
// A file that references other assets (the manifest references the icons)
// is listed after them: it is rewritten first, then hashed.
var assets = []string{
	"style.css", "app.js", "icon.svg", "hls.light.min.js",
	"icon-192.png", "icon-512.png", "icon-maskable-512.png", "apple-touch-icon.png",
	"manifest.webmanifest",
}

// rewritten are the assets that reference other assets.
var rewritten = map[string]bool{"manifest.webmanifest": true}

// staticHandler serves the embedded UI. index.html is rewritten so it
// references its assets as e.g. style.css?v=1a2b3c4d5e6f: any change to a
// file changes its URL, so no cache (browser or Cloudflare) can ever serve an
// asset from another version. 1.x also had a /style.css, and Cloudflare
// kept serving that stale copy after the upgrade to 2.0.0. The web app
// manifest is rewritten the same way for the icons it references.
type staticHandler struct {
	files   http.Handler
	index   []byte
	content map[string][]byte // rewritten assets, served from memory
	version map[string]string // asset → content hash
}

func newStaticHandler(fsys fs.FS) *staticHandler {
	h := &staticHandler{files: http.FileServerFS(fsys), content: map[string][]byte{}, version: map[string]string{}}
	// versioned replaces every quoted reference to an asset hashed so far.
	versioned := func(text string) string {
		for _, name := range assets {
			if v, ok := h.version[name]; ok {
				text = strings.ReplaceAll(text, `"`+name+`"`, `"`+name+"?v="+v+`"`)
			}
		}
		return text
	}
	for _, name := range assets {
		data, err := fs.ReadFile(fsys, name)
		if err != nil {
			panic("web: " + name + " missing from embedded files: " + err.Error())
		}
		if rewritten[name] {
			data = []byte(versioned(string(data)))
			h.content[name] = data
		}
		h.version[name] = contentHash(data)
	}
	index, err := fs.ReadFile(fsys, "index.html")
	if err != nil {
		panic("web: index.html missing from embedded files: " + err.Error())
	}
	h.index = []byte(versioned(string(index)))
	return h
}

func contentHash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:6])
}

func (h *staticHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/")
	if name == "" || name == "index.html" {
		// Never cache the page itself: it carries the asset hashes.
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(h.index)
		return
	}
	if v, ok := h.version[name]; ok && r.URL.Query().Get("v") == v {
		// This exact content never changes under this URL.
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}
	if strings.HasSuffix(name, ".webmanifest") {
		// Not in Go's MIME table, and Windows may not know it either.
		w.Header().Set("Content-Type", "application/manifest+json")
	}
	if data, ok := h.content[name]; ok {
		if w.Header().Get("Content-Type") == "" {
			w.Header().Set("Content-Type", "application/octet-stream")
		}
		w.Write(data)
		return
	}
	h.files.ServeHTTP(w, r)
}

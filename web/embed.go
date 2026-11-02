// Package web holds the static web UI, embedded in the executable.
package web

import "embed"

// FS contains index.html and its assets (styles, script, icons, web app manifest).
//
//go:embed index.html app.js style.css icon.svg icon-192.png icon-512.png icon-maskable-512.png apple-touch-icon.png manifest.webmanifest
var FS embed.FS

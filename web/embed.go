// Package web holds the screen files served by the Go server.
package web

import "embed"

// Files are the screen files (index.html, scripts, styles).
//
//go:embed index.html app.js charts.js i18n.js styles.css
var Files embed.FS

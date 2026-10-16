// Package web embeds the static frontend.
package web

import "embed"

// FS holds index.html, app.js and style.css under static/.
//
//go:embed static
var FS embed.FS

package web

import "embed"

// Files embeds all static web assets into the binary.
//
//go:embed index.html style.css app.js chart.min.js
var Files embed.FS

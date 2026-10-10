package web

import "embed"

// Assets are built by `npm run build` in web/ before compiling the binary.
//
//go:embed all:dist
var Assets embed.FS

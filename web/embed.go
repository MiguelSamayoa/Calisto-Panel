// Package web embeds the single-page panel. go:embed cannot reach parent
// directories, so this file has to live next to index.html.
package web

import _ "embed"

//go:embed index.html
var Index []byte

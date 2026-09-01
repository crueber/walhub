// Package web embeds the built UI (16_packaging.md §1.2). web/dist/repos.js is produced by
// `make web` (esbuild); .keep placeholders are committed so the embed resolves on fresh
// checkouts with no toolchain. The plain (non-all) index.html entry plus all:-prefixed dirs
// include placeholder dotfiles but exclude web/test/ (never embedded).
package web

import "embed"

//go:embed index.html all:dist all:sdk all:src all:css
var Files embed.FS

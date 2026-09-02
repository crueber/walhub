// Package web embeds the built UI (16_packaging.md §1.2, D-WEB-6): web/dist is
// produced by `make web` (vite build for the SolidJS SPA + esbuild for the SDK
// bundle); .keep is committed so the embed resolves on fresh checkouts with no
// toolchain. Only the BUILT artifacts ship in the binary — sources stay out.
package web

import "embed"

//go:embed all:dist
var Files embed.FS

// web/vite.config.mjs — the SPA build (D-WEB-6): SolidJS + Tailwind v4, no CDN.
// base "/_ui/" keeps every asset URL under the server's static lane; the Go
// binary embeds dist/ and serves /_ui/assets/* (immutable) + index.html.
import { defineConfig } from "vite";
import solid from "vite-plugin-solid";
import tailwindcss from "@tailwindcss/vite";

export default defineConfig({
  base: "/_ui/",
  plugins: [solid(), tailwindcss()],
  build: {
    outDir: "dist",
    emptyOutDir: true, // make web runs build:ui FIRST, then the SDK bundle
    target: "es2022",
  },
});

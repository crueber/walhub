package api

import (
	"encoding/json"
	"net/http"
)

// --- GET …/blob/{rev}/{path}[?raw] (§9.5) -------------------------------------------

type blobBody struct {
	Ref      string `json:"ref"`
	SHA      string `json:"sha"`
	Path     string `json:"path"`
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	Contents string `json:"contents,omitempty"`
	Binary   bool   `json:"binary,omitempty"`
	TooLarge bool   `json:"too_large,omitempty"`
}

func (h *handlers) blob(w http.ResponseWriter, r *http.Request) {
	if !h.open(w, r, AuthRead) {
		return
	}
	rev := r.PathValue("rev")
	path := r.PathValue("path")
	if path == "" {
		writePlain(w, http.StatusNotFound, "blob requires a path")
		return
	}
	res, err := h.env.Repo.Resolve(r.Context(), RepoOf(r), rev)
	if err != nil {
		mapViewErr(w, err)
		return
	}
	ctx := r.Context()
	class := ccSWR
	if revIsFullSHA(rev) {
		class = ccImmutable
	}

	if _, raw := r.URL.Query()["raw"]; raw {
		// ?raw bypasses JSON: full raw bytes, text/plain (§14: the 2 MiB cap
		// is a JSON-shape rule; raw is the download path).
		br, err := h.env.Repo.Blob(ctx, RepoOf(r), res.SHA, path, true)
		if err != nil {
			mapViewErr(w, err)
			return
		}
		w.Header().Set("Cache-Control", class)
		w.Header().Set("ETag", `"`+res.SHA+`"`)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(br.Contents)
		return
	}

	render := func() ([]byte, error) {
		br, err := h.env.Repo.Blob(ctx, RepoOf(r), res.SHA, path, false)
		if err != nil {
			return nil, err
		}
		body := blobBody{
			Ref:      res.Ref,
			SHA:      res.SHA,
			Path:     path,
			Name:     baseName(path),
			Size:     br.Size,
			Binary:   br.Binary,
			TooLarge: br.TooLarge,
		}
		if !br.Binary && !br.TooLarge {
			body.Contents = string(br.Contents)
		}
		return json.Marshal(body)
	}
	raw, err := h.env.renderImmutable(ctx, "blob/"+res.SHA+"/"+path, res.Revision, res.SHA, render)
	if err != nil {
		mapViewErr(w, err)
		return
	}
	writeBody(w, r, class, res.SHA, http.StatusOK, raw)
}

func baseName(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' {
			return path[i+1:]
		}
	}
	return path
}

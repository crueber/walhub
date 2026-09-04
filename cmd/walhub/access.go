// access.go — `walhub access get|put` (docs/features/01 §8, Seam 7): a thin
// store client over the same CAS path as the HTTP surface. Reads synthesize
// the §10 legacy default when access.json is absent; writes are full-doc
// PUTs carrying the version just read (stale versions 409 at the CAS).
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/identity"
)

// runAccess implements `walhub access get|put <OWNER/REPO> [--file F]`.
func runAccess(ctx context.Context, c *cli, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: walhub access get <OWNER/REPO> | walhub access put --file F <OWNER/REPO>")
		return exitArg
	}
	sub, rest := args[0], args[1:]
	fs := newFlagSet("access " + sub)
	file := fs.String("file", "", "access JSON path (put)")
	positional := mustParse(fs, rest)
	if len(positional) != 1 {
		fmt.Fprintln(os.Stderr, "usage: walhub access get <OWNER/REPO> | walhub access put --file F <OWNER/REPO>")
		return exitArg
	}
	id, err := git.ParseRepoId(positional[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "walhub: %v\n", err)
		return exitErr
	}
	_, st, cleanup := openEngine(ctx, c)
	defer cleanup()
	svc := identity.New(st, nil)

	switch sub {
	case "get":
		doc, _, err := svc.GetAccess(ctx, id.Owner, id.Name)
		if err != nil {
			fmt.Fprintf(os.Stderr, "walhub: %v\n", err)
			return exitErr
		}
		raw, _ := json.MarshalIndent(map[string]any{
			"version":       doc.Version,
			"visibility":    string(doc.Visibility),
			"role_bindings": doc.RoleBindings,
		}, "", "  ")
		fmt.Println(string(raw))
		return exitOK
	case "put":
		if *file == "" {
			fmt.Fprintln(os.Stderr, "walhub: access put requires --file F")
			return exitArg
		}
		body, err := os.ReadFile(*file)
		if err != nil {
			fmt.Fprintf(os.Stderr, "walhub: %v\n", err)
			return exitErr
		}
		var doc struct {
			Version      int                      `json:"version"`
			Visibility   string                   `json:"visibility"`
			RoleBindings []identity.AccessBinding `json:"role_bindings"`
		}
		if err := json.Unmarshal(body, &doc); err != nil {
			fmt.Fprintf(os.Stderr, "walhub: invalid access JSON: %v\n", err)
			return exitErr
		}
		cur, curVer, err := svc.GetAccess(ctx, id.Owner, id.Name)
		if err != nil {
			fmt.Fprintf(os.Stderr, "walhub: %v\n", err)
			return exitErr
		}
		if cur.Version != doc.Version {
			fmt.Fprintln(os.Stderr, "walhub: access.json changed under you; reload (access get) and retry")
			return exitErr
		}
		next, err := svc.PutAccess(ctx, id.Owner, id.Name, curVer, identity.Visibility(doc.Visibility), doc.RoleBindings)
		if err != nil {
			fmt.Fprintf(os.Stderr, "walhub: %v\n", err)
			return exitErr
		}
		fmt.Printf("access %s: version %d\n", id.String(), next.Version)
		return exitOK
	default:
		fmt.Fprintf(os.Stderr, "walhub: unknown access subcommand %q\n", sub)
		return exitArg
	}
}

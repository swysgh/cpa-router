package main

import "embed"

// panelFS holds the single-file management resource page (§10.3). No external
// scripts, fonts, or CDNs; themed via host CSS variables.
//
//go:embed panel.html
var panelFS embed.FS

// panelHTML returns the raw panel page bytes.
func panelHTML() []byte {
	raw, err := panelFS.ReadFile("panel.html")
	if err != nil {
		return nil
	}
	return raw
}

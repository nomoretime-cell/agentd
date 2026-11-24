//go:build webui

package main

import (
	"embed"
	"io/fs"
)

//go:embed ui/index.html ui/assets/*
var bundle embed.FS

func init() {
	root, err := fs.Sub(bundle, "ui")
	if err != nil {
		panic(err)
	}
	workbenchFiles = root
}

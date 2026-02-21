package main

import (
	"embed"
	"io/fs"
)

//go:generate npm --prefix ./ui run build
//go:embed ui/dist/*
var embeddedUIAssets embed.FS

func uiAssetsFS() (fs.FS, error) {
	return fs.Sub(embeddedUIAssets, "ui/dist")
}

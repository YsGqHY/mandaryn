package main

import (
	"embed"
	"log"

	"vip.oberon.mandaryn/internal/app"
)

//go:embed all:frontend/dist
var assets embed.FS

//go:embed frontend/public/wails.png
var trayIcon []byte

func main() {
	if err := app.Run(assets, trayIcon); err != nil {
		log.Fatal(err)
	}
}

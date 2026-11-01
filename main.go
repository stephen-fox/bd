// bd (bhyve daemon) is a bhyve virtual machine manager, similar to vm-bhyve.
package main

import (
	"log"

	"codeberg.org/stephen-fox/bd/internal/app"
)

func main() {
	log.SetFlags(0)

	// Refer to "internal/app/app.go" for RunApp's definition.
	err := app.RunApp()
	if err != nil {
		log.Fatalln("fatal:", err)
	}
}

package app

import (
	"os"

	"codeberg.org/stephen-fox/bd/internal/nettools"
)

func genmac() error {
	mac, err := nettools.GenMacAddr()
	if err != nil {
		return err
	}

	os.Stdout.WriteString(mac + "\n")

	return nil
}

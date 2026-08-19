package nettools

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
)

func GenMacAddr() (string, error) {
	var retriesRemaining int

	var mac string
	b := make([]byte, 1)

	for i := 0; i < 6; i++ {
		retriesRemaining = 5

	retry:
		if retriesRemaining == 0 {
			return "", errors.New("read 0 after 5 retries")
		}

		_, err := rand.Read(b)
		if err != nil {
			return "", err
		}

		if b[0] == 0x00 {
			retriesRemaining--
			goto retry
		}

		if i == 0 {
			// Last two bits need to be:
			// 1 0
			b[0] <<= 2
			b[0] ^= 0b00000010
		}

		mac += hex.EncodeToString(b)
		if i != 5 {
			mac += ":"
		}
	}

	return mac, nil
}

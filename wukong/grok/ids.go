package grok

import (
	"crypto/rand"
	"encoding/hex"
)

func newRequestUUID() string {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return newWebID("req")
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(value)
	return encoded[:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:]
}

func newWebID(prefix string) string {
	value := make([]byte, 16)
	_, _ = rand.Read(value)
	return prefix + "_" + hex.EncodeToString(value)
}

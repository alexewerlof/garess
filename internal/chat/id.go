package chat

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

// NewSessionID generates a session identifier in the legacy garess format:
// 20060102T150405-<hex>.
func NewSessionID() string {
	return time.Now().Format("20060102T150405") + "-" + randHex(4)
}

// NewEventID generates a random event identifier.
func NewEventID() string {
	return randHex(16)
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

package session

import (
	"crypto/rand"
	"fmt"
	"time"
)

// NewID returns a time-ordered UUIDv7 in canonical form. The first 48 bits are
// Unix milliseconds; the remaining bits are random. This keeps history entries
// monotonically sortable without taking a dependency on a UUID library.
func NewID() (string, error) {
	return newIDAt(time.Now())
}

func newIDAt(t time.Time) (string, error) {
	var b [16]byte
	ms := uint64(t.UnixMilli())
	b[0] = byte(ms >> 40)
	b[1] = byte(ms >> 32)
	b[2] = byte(ms >> 24)
	b[3] = byte(ms >> 16)
	b[4] = byte(ms >> 8)
	b[5] = byte(ms)

	if _, err := rand.Read(b[6:]); err != nil {
		return "", fmt.Errorf("generate entry id: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x70 // version 7
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 variant

	return fmt.Sprintf(
		"%x-%x-%x-%x-%x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16],
	), nil
}

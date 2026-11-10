package searchtools

import (
	"crypto/rand"
	"crypto/sha1"
	"fmt"
)

// namespaceURL is the RFC 4122 URL namespace, the one Python's
// uuid5(NAMESPACE_URL, ...) uses.
var namespaceURL = [16]byte{
	0x6b, 0xa7, 0xb8, 0x11, 0x9d, 0xad, 0x11, 0xd1,
	0x80, 0xb4, 0x00, 0xc0, 0x4f, 0xd4, 0x30, 0xc8,
}

// uuid5 derives a name-based UUID (RFC 4122 version 5, SHA-1).
func uuid5(namespace [16]byte, name string) string {
	h := sha1.New()
	_, _ = h.Write(namespace[:])
	_, _ = h.Write([]byte(name))
	sum := h.Sum(nil)
	var u [16]byte
	copy(u[:], sum[:16])
	u[6] = (u[6] & 0x0f) | 0x50
	u[8] = (u[8] & 0x3f) | 0x80
	return formatUUID(u)
}

// uuid4 returns a random UUID (RFC 4122 version 4).
func uuid4() (string, error) {
	var u [16]byte
	if _, err := rand.Read(u[:]); err != nil {
		return "", fmt.Errorf("searchtools: generate uuid: %w", err)
	}
	u[6] = (u[6] & 0x0f) | 0x40
	u[8] = (u[8] & 0x3f) | 0x80
	return formatUUID(u), nil
}

// formatUUID renders the canonical 8-4-4-4-12 form.
func formatUUID(u [16]byte) string {
	return fmt.Sprintf("%x-%x-%x-%x-%x", u[0:4], u[4:6], u[6:8], u[8:10], u[10:16])
}

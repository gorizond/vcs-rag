package store

import (
	"crypto/sha1"
	"encoding/hex"
)

// uuidNamespace — a fixed namespace for uuid5. Must not be changed:
// it would change all point IDs and break idempotency (FR-011).
var uuidNamespace = func() [16]byte {
	sum := sha1.Sum([]byte("vcs-rag"))
	var ns [16]byte
	copy(ns[:], sum[:16])
	return ns
}()

// uuid5 computes an RFC 4122 v5 UUID (SHA-1) with no external dependencies.
func uuid5(ns [16]byte, name string) string {
	h := sha1.New()
	h.Write(ns[:])
	h.Write([]byte(name))
	sum := h.Sum(nil)
	var u [16]byte
	copy(u[:], sum[:16])
	u[6] = (u[6] & 0x0f) | 0x50 // version 5
	u[8] = (u[8] & 0x3f) | 0x80 // RFC 4122 variant
	hx := hex.EncodeToString(u[:])
	return hx[0:8] + "-" + hx[8:12] + "-" + hx[12:16] + "-" + hx[16:20] + "-" + hx[20:32]
}

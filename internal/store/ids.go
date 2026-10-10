package store

import (
	"crypto/sha1"
	"encoding/hex"
)

// uuidNamespace — фиксированный namespace для uuid5. Менять нельзя:
// изменит все ID точек и сломает идемпотентность (FR-011).
var uuidNamespace = func() [16]byte {
	sum := sha1.Sum([]byte("vcs-rag"))
	var ns [16]byte
	copy(ns[:], sum[:16])
	return ns
}()

// uuid5 вычисляет RFC 4122 v5 UUID (SHA-1) без внешних зависимостей.
func uuid5(ns [16]byte, name string) string {
	h := sha1.New()
	h.Write(ns[:])
	h.Write([]byte(name))
	sum := h.Sum(nil)
	var u [16]byte
	copy(u[:], sum[:16])
	u[6] = (u[6] & 0x0f) | 0x50 // версия 5
	u[8] = (u[8] & 0x3f) | 0x80 // вариант RFC 4122
	hx := hex.EncodeToString(u[:])
	return hx[0:8] + "-" + hx[8:12] + "-" + hx[12:16] + "-" + hx[16:20] + "-" + hx[20:32]
}

package main // Already main, no change needed here

import (
	"unsafe"
)

func unsafeBytes(addr uintptr, n int) []byte {
	return unsafe.Slice((*byte)(unsafe.Pointer(addr)), n)
}

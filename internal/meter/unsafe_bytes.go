package meter

import (
	"unsafe"
)

func unsafeBytes(addr uintptr, n int) []byte {
	return unsafe.Slice((*byte)(unsafe.Pointer(addr)), n)
}


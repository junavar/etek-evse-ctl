package main // Already main, no change needed here

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	shmFlags = unix.IPC_CREAT
)

type Data struct {
	Timestamp              int64
	ActivePower            float32
	ReactivePower          float32
	Voltage                float32
	Current                float32
	Frequency              float32
	PowerFactor            float32
	ImportActiveEnergy     float32
	ExportActiveEnergy     float32
	ImportReactiveEnergy   float32
	ExportReactiveEnergy   float32
	PotenciaMediaImportada float32
	PotenciaMediaExportada float32
	Ventana                int32
	Crc32                  uint32
}

type Reader struct {
	key  int
	size int
	shmid int
	addr uintptr
}

type ReadStatus int

const (
	ReadInvalid ReadStatus = iota
	ReadValid
	ReadErrorFlag
	ReadCRCFail
)

func (s ReadStatus) String() string {
	switch s {
	case ReadValid:
		return "valid"
	case ReadErrorFlag:
		return "error_flag"
	case ReadCRCFail:
		return "crc_fail"
	default:
		return "invalid"
	}
}

type ReadDebug struct {
	WantCRC uint32
	GotCRC  uint32
}

func NewReader(key uint32, size int) (*Reader, error) {
	if size <= 0 {
		return nil, fmt.Errorf("invalid shm size %d", size)
	}

	r := &Reader{
		key:  int(key),
		size: size,
		shmid: -1,
		addr:  0,
	}
	if err := r.attach(); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *Reader) Close() error {
	var errOut error
	if r.addr != 0 {
		if err := shmdt(r.addr); err != nil {
			errOut = errors.Join(errOut, err)
		}
		r.addr = 0
	}
	r.shmid = -1
	return errOut
}

func (r *Reader) attach() error {
	shmid, err := shmget(r.key, r.size, shmFlags|0o666)
	if err != nil {
		return fmt.Errorf("shmget(key=0x%08x,size=%d): %w", uint32(r.key), r.size, err)
	}
	addr, err := shmat(shmid, nil, 0)
	if err != nil {
		return fmt.Errorf("shmat(shmid=%d): %w", shmid, err)
	}
	r.shmid = shmid
	r.addr = addr
	return nil
}

func (r *Reader) Read() (Data, ReadStatus, ReadDebug, error) {
	if r.addr == 0 {
		if err := r.attach(); err != nil {
			return Data{}, ReadInvalid, ReadDebug{}, err
		}
	}

	// Copy bytes out of SHM to avoid tearing while decoding.
	b := unsafeBytes(r.addr, r.size) // Call unsafeBytes directly
	buf := make([]byte, r.size)
	copy(buf, b)

	if len(buf) < 64 {
		return Data{}, ReadInvalid, ReadDebug{}, fmt.Errorf("shm size too small: got %d, need 64", len(buf))
	}

	// Validate CRC32 over first 60 bytes.
	want := binary.LittleEndian.Uint32(buf[60:64])
	dbg := ReadDebug{WantCRC: want}

	// 0xFFFFFFFF indica un error en la obtención o escritura de los datos en la SHM.
	if want == 0xFFFFFFFF {
		return Data{}, ReadErrorFlag, dbg, nil
	}

	// Si es 0x00000000 no se emplea CRC. Cualquier otro valor debe verificarse.
	if want != 0x00000000 {
		got := crc32.ChecksumIEEE(buf[:60])
		dbg.GotCRC = got
		if got != want {
			return Data{}, ReadCRCFail, dbg, nil
		}
	}

	var d Data
	if err := binary.Read(bytes.NewReader(buf[:64]), binary.LittleEndian, &d); err != nil {
		return Data{}, ReadInvalid, dbg, fmt.Errorf("decode meter data: %w", err)
	}
	return d, ReadValid, dbg, nil
}

func IsFresh(d Data, maxAge time.Duration, now time.Time) bool {
	if maxAge <= 0 {
		return true
	}
	ts := time.Unix(d.Timestamp, 0)
	age := now.Sub(ts)
	if age < 0 {
		age = -age
	}
	return age <= maxAge
}

func shmget(key, size, shmflg int) (int, error) {
	r0, _, e1 := unix.Syscall(unix.SYS_SHMGET, uintptr(key), uintptr(size), uintptr(shmflg))
	if e1 != 0 {
		return 0, e1
	}
	return int(r0), nil
}

func shmat(shmid int, shmaddr unsafe.Pointer, shmflg int) (uintptr, error) {
	r0, _, e1 := unix.Syscall(unix.SYS_SHMAT, uintptr(shmid), uintptr(shmaddr), uintptr(shmflg))
	if e1 != 0 {
		return 0, e1
	}
	return r0, nil
}

func shmdt(shmaddr uintptr) error {
	_, _, e1 := unix.Syscall(unix.SYS_SHMDT, shmaddr, 0, 0)
	if e1 != 0 {
		return e1
	}
	return nil
}

package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"golang.org/x/sys/unix"
	"unsafe" // Keep unsafe for direct memory access
)

// EVSEStatus representa el estado de un controlador individual (32 bytes con pad)
type EVSEStatus struct {
	ModbusID         uint8
	WorkingStatus    uint16
	MaxChargeCurrent uint16
	OutputPWMDuty    uint16
	RotarySwitchPWM  uint16
	RemoteStartStop  uint16
	Pad              [21]byte
}

// StatusData es la estructura completa que se escribe en shm-etek-write
type StatusData struct {
	Timestamp       int64
	ActivePower     float32
	IntegratedPower float32
	LimitW          float32
	MarginW         float32
	NumControllers  int32
	CommonPad       [32]byte
	Controllers     [4]EVSEStatus
	Crc32           uint32
}

type StatusWriter struct {
	key   int
	size  int
	shmid int
	addr  uintptr
}

func NewStatusWriter(key uint32, size int) (*StatusWriter, error) {
	shmid, _, errno := unix.Syscall(unix.SYS_SHMGET, uintptr(key), uintptr(size), unix.IPC_CREAT|0666)
	if errno != 0 {
		return nil, fmt.Errorf("shmget error: %v", errno)
	}

	addr, _, errno := unix.Syscall(unix.SYS_SHMAT, shmid, 0, 0)
	if errno != 0 {
		return nil, fmt.Errorf("shmat error: %v", errno)
	}

	return &StatusWriter{
		key:   int(key),
		size:  size,
		shmid: int(shmid),
		addr:  addr,
	}, nil
}

func (sw *StatusWriter) Write(data StatusData) error {
	// Calculamos CRC sobre todo menos el campo Crc32 (últimos 4 bytes)
	data.Crc32 = 0
	var buf bytes.Buffer
	// Usamos LittleEndian para consistencia con el lector de meter
	if err := binary.Write(&buf, binary.LittleEndian, data); err != nil {
		return err
	}

	allBytes := buf.Bytes()
	dataLen := int(unsafe.Sizeof(data))
	
	// El CRC se calcula sobre los primeros (N-4) bytes
	checkSum := crc32.ChecksumIEEE(allBytes[:dataLen-4])
	binary.LittleEndian.PutUint32(allBytes[dataLen-4:], checkSum)

	// Escribir físicamente en la memoria mapeada
	dst := (*[1024]byte)(unsafe.Pointer(sw.addr))
	copy(dst[:dataLen], allBytes)

	return nil
}

func (sw *StatusWriter) Close() {
	if sw.addr != 0 {
		unix.Syscall(unix.SYS_SHMDT, sw.addr, 0, 0)
		sw.addr = 0
	}
}
package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// --- Tipos y Estados Compartidos para SHM ---

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

// --- 1. SHM Medidor (Lectura de datos de Eastron - Clave 0x1264) ---

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
	key   int
	size  int
	shmid int
	addr  uintptr
}

func NewReader(key uint32, size int) (*Reader, error) {
	r := &Reader{key: int(key), size: size, shmid: -1}
	if err := r.attach(); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *Reader) Read() (Data, ReadStatus, ReadDebug, error) {
	if r.addr == 0 {
		if err := r.attach(); err != nil {
			return Data{}, ReadInvalid, ReadDebug{}, err
		}
	}
	src := unsafe.Slice((*byte)(unsafe.Pointer(r.addr)), r.size)
	buf := make([]byte, r.size)
	copy(buf, src)

	if len(buf) < 64 {
		return Data{}, ReadInvalid, ReadDebug{}, fmt.Errorf("shm size too small")
	}

	want := binary.LittleEndian.Uint32(buf[60:64])
	dbg := ReadDebug{WantCRC: want}
	if want == 0xFFFFFFFF {
		return Data{}, ReadErrorFlag, dbg, nil
	}
	if want != 0x00000000 {
		got := crc32.ChecksumIEEE(buf[:60])
		dbg.GotCRC = got
		if got != want {
			return Data{}, ReadCRCFail, dbg, nil
		}
	}
	var d Data
	if err := binary.Read(bytes.NewReader(buf[:64]), binary.LittleEndian, &d); err != nil {
		return Data{}, ReadInvalid, dbg, err
	}
	return d, ReadValid, dbg, nil
}

func (r *Reader) Close() error {
	if r.addr != 0 {
		shmdt(r.addr)
		r.addr = 0
	}
	return nil
}

func (r *Reader) attach() error {
	shmid, err := shmget(r.key, r.size, unix.IPC_CREAT|0666)
	if err != nil {
		return err
	}
	addr, err := shmat(shmid, nil, 0)
	r.shmid = shmid
	r.addr = addr
	return err
}

func IsFresh(d Data, maxAge time.Duration, now time.Time) bool {
	if maxAge <= 0 {
		return true
	}
	age := now.Sub(time.Unix(d.Timestamp, 0))
	if age < 0 {
		age = -age
	}
	return age <= maxAge
}

// --- 2. SHM Deye (Lectura de datos de Inversor - Clave 0x1238) ---

type DeyeData struct {
	Timestamp     int64
	PV1Power      float32
	PV2Power      float32
	PV3Power      float32
	PV4Power      float32
	PVTotalPower  float32
	BattPower     float32
	InverterPower float32
	GenInv        float32
	GridCTInt     float32
	GridCTExt     float32
	LoadTotal     float32
	LoadUPS       float32
	LoadNUPS      float32
	TempDisipDC   float32
	TempDisipAC   float32
	TempBatt      float32
	SOC           float32
	Padding       [432]byte
	CRC           uint32
}

type DeyeReader struct {
	key   int
	size  int
	shmid int
	addr  uintptr
}

func NewDeyeReader(key uint32, size int) (*DeyeReader, error) {
	dr := &DeyeReader{key: int(key), size: size, shmid: -1}
	if err := dr.attach(); err != nil {
		return nil, err
	}
	return dr, nil
}

func (dr *DeyeReader) Read() (DeyeData, ReadStatus, ReadDebug, error) {
	if dr.addr == 0 {
		if err := dr.attach(); err != nil {
			return DeyeData{}, ReadInvalid, ReadDebug{}, err
		}
	}
	src := unsafe.Slice((*byte)(unsafe.Pointer(dr.addr)), dr.size)
	buf := make([]byte, dr.size)
	copy(buf, src)

	if len(buf) < 512 {
		return DeyeData{}, ReadInvalid, ReadDebug{}, fmt.Errorf("shm deye size too small")
	}

	want := binary.LittleEndian.Uint32(buf[508:512])
	dbg := ReadDebug{WantCRC: want}
	if want == 0xFFFFFFFF {
		return DeyeData{}, ReadErrorFlag, dbg, nil
	}
	if want != 0x00000000 {
		got := crc32.ChecksumIEEE(buf[:508])
		dbg.GotCRC = got
		if got != want {
			return DeyeData{}, ReadCRCFail, dbg, nil
		}
	}
	var d DeyeData
	if err := binary.Read(bytes.NewReader(buf[:512]), binary.LittleEndian, &d); err != nil {
		return DeyeData{}, ReadInvalid, dbg, err
	}
	return d, ReadValid, dbg, nil
}

func (dr *DeyeReader) attach() error {
	shmid, err := shmget(dr.key, dr.size, unix.IPC_CREAT|0666)
	if err != nil {
		return err
	}
	addr, err := shmat(shmid, nil, 0)
	dr.shmid = shmid
	dr.addr = addr
	return err
}

// --- Constantes y Tipos para SHM de Comandos (0x1231) ---

const (
	SHM_COMMAND_KEY  uint32 = 0x1231
	SHM_COMMAND_SIZE int    = 64
)

type CommandPayload struct {
	Action      int32
	Value1      int32
	Value2      int32
	TimestampTx int64
	Source      [24]byte
	TimestampRx int64
	Pad         [8]byte
	Crc32       uint32
}

type CommandSHM struct {
	key   int
	size  int
	shmid int
	addr  uintptr
}

func NewCommandSHM(key uint32, size int) (*CommandSHM, error) {
	shmid, err := shmget(int(key), size, unix.IPC_CREAT|0666)
	if err != nil {
		return nil, fmt.Errorf("shmget error for command SHM (key 0x%08x, size %d): %w", key, size, err)
	}
	addr, err := shmat(shmid, nil, 0)
	if err != nil {
		return nil, fmt.Errorf("shmat error for command SHM (shmid %d): %w", shmid, err)
	}
	return &CommandSHM{key: int(key), size: size, shmid: int(shmid), addr: addr}, nil
}

func (cs *CommandSHM) Read() (CommandPayload, ReadStatus, ReadDebug, error) {
	var cmd CommandPayload
	if cs.addr == 0 {
		if err := cs.attach(); err != nil {
			return cmd, ReadInvalid, ReadDebug{}, err
		}
	}
	srcBytes := unsafe.Slice((*byte)(unsafe.Pointer(cs.addr)), cs.size)
	buf := make([]byte, cs.size)
	copy(buf, srcBytes)

	if len(buf) < SHM_COMMAND_SIZE {
		return cmd, ReadInvalid, ReadDebug{}, fmt.Errorf("shm command size too small")
	}

	want := binary.LittleEndian.Uint32(buf[SHM_COMMAND_SIZE-4 : SHM_COMMAND_SIZE])
	dbg := ReadDebug{WantCRC: want}
	if want == 0xFFFFFFFF {
		return cmd, ReadErrorFlag, dbg, nil
	}
	if want != 0x00000000 {
		got := crc32.ChecksumIEEE(buf[:SHM_COMMAND_SIZE-4])
		dbg.GotCRC = got
		if got != want {
			return cmd, ReadCRCFail, dbg, nil
		}
	}
	if err := binary.Read(bytes.NewReader(buf[:SHM_COMMAND_SIZE]), binary.LittleEndian, &cmd); err != nil {
		return cmd, ReadInvalid, dbg, fmt.Errorf("decode command data: %w", err)
	}
	return cmd, ReadValid, dbg, nil
}

func (cs *CommandSHM) Write(cmd CommandPayload) error {
	if cs.addr == 0 {
		return fmt.Errorf("command SHM not attached")
	}
	cmd.Crc32 = 0
	var buf bytes.Buffer
	binary.Write(&buf, binary.LittleEndian, cmd)
	allBytes := buf.Bytes()
	n := len(allBytes)
	checkSum := crc32.ChecksumIEEE(allBytes[:n-4])
	cmd.Crc32 = checkSum
	buf.Reset()
	binary.Write(&buf, binary.LittleEndian, cmd)
	allBytes = buf.Bytes()
	dst := unsafe.Slice((*byte)(unsafe.Pointer(cs.addr)), cs.size)
	copy(dst, allBytes)
	return nil
}

func (cs *CommandSHM) Close() error {
	if cs.addr != 0 {
		shmdt(cs.addr)
		cs.addr = 0
	}
	return nil
}

func (cs *CommandSHM) attach() error {
	shmid, err := shmget(cs.key, cs.size, unix.IPC_CREAT|0666)
	if err != nil {
		return err
	}
	addr, err := shmat(shmid, nil, 0)
	cs.shmid = shmid
	cs.addr = addr
	return err
}

// --- 3. SHM Estado (0x1230) ---

type EVSEStatus struct {
	ModbusID         uint8
	Reserved         uint8
	WorkingStatus    uint16
	MaxChargeCurrent uint16
	OutputPWMDuty    uint16
	RotarySwitchPWM  uint16
	RemoteStartStop  uint16
	Pad              [20]byte
}

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
	shmid, err := shmget(int(key), size, unix.IPC_CREAT|0666)
	if err != nil {
		return nil, err
	}
	addr, err := shmat(shmid, nil, 0)
	if err != nil {
		return nil, err
	}
	return &StatusWriter{key: int(key), size: size, shmid: int(shmid), addr: addr}, nil
}

func (sw *StatusWriter) Write(data StatusData) error {
	data.Crc32 = 0
	var buf bytes.Buffer
	if err := binary.Write(&buf, binary.LittleEndian, data); err != nil {
		return err
	}
	allBytes := buf.Bytes()
	n := len(allBytes)
	checkSum := crc32.ChecksumIEEE(allBytes[:n-4])
	binary.LittleEndian.PutUint32(allBytes[n-4:], checkSum)
	dst := unsafe.Slice((*byte)(unsafe.Pointer(sw.addr)), sw.size)
	copy(dst, allBytes)
	return nil
}

func (sw *StatusWriter) Close() {
	if sw.addr != 0 {
		shmdt(sw.addr)
		sw.addr = 0
	}
}

// --- 4. Wrappers de System V SHM (Syscalls) ---

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
package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	SHM_COMMAND_KEY  uint32 = 0x1231
	SHM_COMMAND_SIZE int    = 64
)

// CommandPayload define la estructura de los datos que se leerán de la SHM de comandos.
type CommandPayload struct {
	Action      int32      // Código de la acción a realizar (ej. 1: Start, 2: Stop, 3: SetPWM)
	Value1      int32      // Primer valor asociado a la acción (ej. ModbusID, PWM)
	Value2      int32      // Segundo valor asociado a la acción
	TimestampTx int64      // Tiempo Unix 64-bit cuando el comando fue enviado por el proceso externo
	Source      [24]byte   // Origen del comando (ej. "dee-iu", "manual")
	TimestampRx int64      // Tiempo Unix 64-bit cuando el comando fue recibido/procesado por etek-evse-ctl
	Pad         [8]byte    // Relleno para asegurar un tamaño total de 64 bytes
	Crc32       uint32     // CRC32 para la verificación de integridad
}

// CommandReader gestiona la lectura del segmento de memoria compartida de comandos.
type CommandSHM struct {
	key   int
	size  int
	shmid int
	addr  uintptr
}

// NewCommandSHM crea una nueva instancia de CommandSHM e intenta adjuntarse a la SHM.
// Crea el segmento si no existe y se adjunta con permisos de lectura/escritura.
func NewCommandSHM(key uint32, size int) (*CommandSHM, error) {
	// Intentamos obtener o crear el segmento con permisos 0666 para que otros puedan escribir.
	shmid, err := shmget(int(key), size, unix.IPC_CREAT|0666)
	if err != nil {
		return nil, fmt.Errorf("shmget error for command SHM (key 0x%08x, size %d): %w", key, size, err)
	}

	addr, err := shmat(shmid, nil, 0) // Adjuntamos con permisos de lectura/escritura
	if err != nil {
		return nil, fmt.Errorf("shmat error for command SHM (shmid %d): %w", shmid, err)
	}

	return &CommandSHM{
		key:   int(key),
		size:  size,
		shmid: int(shmid),
		addr:  addr,
	}, nil
}

// Read lee los datos de la SHM de comandos y verifica su integridad.
func (cs *CommandSHM) Read() (CommandPayload, ReadStatus, ReadDebug, error) {
	var cmd CommandPayload
	if cs.addr == 0 {
		// Si la SHM no está adjunta, intentamos adjuntarla de nuevo
		if err := cs.attach(); err != nil {
			return cmd, ReadInvalid, ReadDebug{}, err
		}
	}

	// Copiar bytes de la SHM para evitar "tearing" durante la decodificación.
	srcBytes := unsafe.Slice((*byte)(unsafe.Pointer(cs.addr)), cs.size)
	buf := make([]byte, cs.size)
	copy(buf, srcBytes)

	if len(buf) < SHM_COMMAND_SIZE {
		return cmd, ReadInvalid, ReadDebug{}, fmt.Errorf("shm command size too small: got %d, need %d", len(buf), SHM_COMMAND_SIZE)
	}

	// Validar CRC32 sobre los primeros (SHM_COMMAND_SIZE - 4) bytes.
	want := binary.LittleEndian.Uint32(buf[SHM_COMMAND_SIZE-4 : SHM_COMMAND_SIZE])
	dbg := ReadDebug{WantCRC: want}

	// 0xFFFFFFFF indica un error en la obtención o escritura de los datos en la SHM.
	if want == 0xFFFFFFFF {
		return cmd, ReadErrorFlag, dbg, nil
	}

	// Si es 0x00000000, el CRC no se emplea. Cualquier otro valor debe verificarse.
	if want != 0x00000000 {
		got := crc32.ChecksumIEEE(buf[:SHM_COMMAND_SIZE-4])
		dbg.GotCRC = got
		if got != want {
			return cmd, ReadCRCFail, dbg, nil
		}
	}

	// Decodificar el payload del comando
	if err := binary.Read(bytes.NewReader(buf[:SHM_COMMAND_SIZE]), binary.LittleEndian, &cmd); err != nil {
		return cmd, ReadInvalid, dbg, fmt.Errorf("decode command data: %w", err)
	}
	return cmd, ReadValid, dbg, nil
}

// Write escribe los datos del comando en la SHM.
func (cs *CommandSHM) Write(cmd CommandPayload) error {
	if cs.addr == 0 {
		return fmt.Errorf("command SHM not attached")
	}

	// Calculamos CRC sobre todo menos el campo Crc32 (últimos 4 bytes)
	cmd.Crc32 = 0 // Temporalmente a 0 para el cálculo del CRC
	var buf bytes.Buffer
	if err := binary.Write(&buf, binary.LittleEndian, cmd); err != nil {
		return fmt.Errorf("encode command data for CRC: %w", err)
	}

	allBytes := buf.Bytes()
	n := len(allBytes)
	if n < SHM_COMMAND_SIZE {
		return fmt.Errorf("encoded command size too small: got %d, need %d", n, SHM_COMMAND_SIZE)
	}
	
	// El CRC se calcula sobre los primeros (N-4) bytes
	checkSum := crc32.ChecksumIEEE(allBytes[:n-4])
	cmd.Crc32 = checkSum // Establecemos el CRC real

	// Re-codificamos con el CRC correcto
	buf.Reset()
	if err := binary.Write(&buf, binary.LittleEndian, cmd); err != nil {
		return fmt.Errorf("re-encode command data with CRC: %w", err)
	}
	allBytes = buf.Bytes()

	// Escribir físicamente en la memoria mapeada
	dst := unsafe.Slice((*byte)(unsafe.Pointer(cs.addr)), cs.size)
	copy(dst, allBytes)

	return nil
}

// Close desadjunta el segmento de memoria compartida.
func (cs *CommandSHM) Close() error {
	var errOut error
	if cs.addr != 0 {
		if err := shmdt(cs.addr); err != nil {
			errOut = fmt.Errorf("shmdt error for command SHM: %w", err)
		}
		cs.addr = 0
	}
	cs.shmid = -1
	return errOut
}

// attach intenta adjuntarse a un segmento de memoria compartida existente.
func (cs *CommandSHM) attach() error {
	shmid, err := shmget(cs.key, cs.size, unix.IPC_CREAT|0666)
	if err != nil {
		return fmt.Errorf("shmget error for command SHM (re-attach): %w", err)
	}
	addr, err := shmat(shmid, nil, 0) // Lectura/Escritura
	if err != nil {
		return fmt.Errorf("shmat error for command SHM (re-attach): %w", err)
	}
	cs.shmid = shmid
	cs.addr = addr
	return nil
}
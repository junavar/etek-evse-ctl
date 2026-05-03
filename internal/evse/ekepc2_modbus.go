package evse

import (
	"encoding/binary"
	"fmt"
	"time"

	"github.com/goburrow/modbus"
)

// Client representa una conexión persistente a un adaptador Modbus RTU.
type Client struct {
	handler *modbus.RTUClientHandler
	client  modbus.Client
}

// NewClient crea una instancia de cliente pero no abre el puerto todavía.
func NewClient(adapterPath string, baudRate int, timeout time.Duration) *Client {
	h := modbus.NewRTUClientHandler(adapterPath)
	h.BaudRate = baudRate
	h.DataBits = 8
	h.Parity = "N"
	h.StopBits = 1
	h.Timeout = timeout

	return &Client{
		handler: h,
		client:  modbus.NewClient(h),
	}
}

// EnsureConnected garantiza que el puerto esté abierto.
// Si ya está abierto, es una operación casi instantánea.
func (c *Client) EnsureConnected() error {
	return c.handler.Connect()
}

// Close cierra la conexión actual.
func (c *Client) Close() error {
	return c.handler.Close()
}

// ReadRegisters lee todos los registros de interés de un esclavo específico.
// Si ocurre un error de comunicación, cierra el handler para forzar una reapertura en el próximo intento.
func (c *Client) ReadRegisters(slaveID uint8) (Registers, error) {
	c.handler.SlaveId = slaveID
	if err := c.EnsureConnected(); err != nil {
		return Registers{}, err
	}

	regs, err := c.readAll()
	if err != nil {
		c.Close() // Forzar reapertura en caso de error
	}
	return regs, err
}

type Registers struct {
	RemoteStartStop  uint16
	DeviceAddress    uint16
	MaxChargeCurrent uint16
	SoftwareVersion  uint16
	WorkingStatus    uint16
	RotarySwitchPWM  uint16
	OutputPWMDuty    uint16
}

// readAll realiza las peticiones Modbus individuales y por bloques.
func (c *Client) readAll() (Registers, error) {
	regs := Registers{}
	slaveID := c.handler.SlaveId

	// Función auxiliar para registros individuales
	readOne := func(addr uint16) (uint16, error) {
		res, err := c.client.ReadHoldingRegisters(addr, 1)
		if err != nil || len(res) < 2 {
			return 0, fmt.Errorf("reg %d (slave %d): %w", addr, slaveID, err)
		}
		return binary.BigEndian.Uint16(res), nil
	}

	// Registros dispersos
	var err error
	if regs.RemoteStartStop, err = readOne(89); err != nil { return regs, err }
	if regs.DeviceAddress, err = readOne(100); err != nil { return regs, err }
	if regs.MaxChargeCurrent, err = readOne(109); err != nil { return regs, err }

	// Lectura de bloque: Software Version (140) y Working Status (141)
	res, err := c.client.ReadHoldingRegisters(140, 2)
	if err != nil || len(res) < 4 {
		return regs, fmt.Errorf("block 140-141 (slave %d): %w", slaveID, err)
	}
	regs.SoftwareVersion = binary.BigEndian.Uint16(res[0:2])
	regs.WorkingStatus = binary.BigEndian.Uint16(res[2:4])

	// Lectura de bloque: Rotary Switch (151) y Output PWM (152)
	res, err = c.client.ReadHoldingRegisters(151, 2)
	if err != nil || len(res) < 4 {
		return regs, fmt.Errorf("block 151-152 (slave %d): %w", slaveID, err)
	}
	regs.RotarySwitchPWM = binary.BigEndian.Uint16(res[0:2])
	regs.OutputPWMDuty = binary.BigEndian.Uint16(res[2:4])

	return regs, nil
}

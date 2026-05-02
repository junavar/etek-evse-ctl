package evse

import (
	"encoding/binary"
	"fmt"
	"time"

	"github.com/goburrow/modbus"
)

type ReadResult struct {
	WorkingStatus uint16
}

// ReadWorkingStatus141 opens the serial port, reads holding register 141 (1 word), closes the port.
func ReadWorkingStatus141(adapterPath string, baudRate int, slaveID uint8, timeout time.Duration) (ReadResult, error) {
	h := modbus.NewRTUClientHandler(adapterPath)
	h.BaudRate = baudRate
	h.DataBits = 8
	h.Parity = "N"
	h.StopBits = 1
	h.SlaveId = slaveID
	h.Timeout = timeout

	if err := h.Connect(); err != nil {
		return ReadResult{}, fmt.Errorf("connect %s (slave %d): %w", adapterPath, slaveID, err)
	}
	defer h.Close()

	c := modbus.NewClient(h)
	raw, err := c.ReadHoldingRegisters(141, 1)
	if err != nil {
		return ReadResult{}, fmt.Errorf("read reg 141 (slave %d): %w", slaveID, err)
	}
	if len(raw) < 2 {
		return ReadResult{}, fmt.Errorf("read reg 141 (slave %d): short response %d bytes", slaveID, len(raw))
	}

	return ReadResult{
		WorkingStatus: binary.BigEndian.Uint16(raw[:2]),
	}, nil
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

func ReadRegisters(adapterPath string, baudRate int, slaveID uint8, timeout time.Duration) (Registers, error) {
	h := modbus.NewRTUClientHandler(adapterPath)
	h.BaudRate = baudRate
	h.DataBits = 8
	h.Parity = "N"
	h.StopBits = 1
	h.SlaveId = slaveID
	h.Timeout = timeout

	if err := h.Connect(); err != nil {
		return Registers{}, fmt.Errorf("connect %s (slave %d): %w", adapterPath, slaveID, err)
	}
	defer h.Close()

	c := modbus.NewClient(h)
	regs := Registers{}

	readUint16 := func(addr uint16) (uint16, error) {
		raw, err := c.ReadHoldingRegisters(addr, 1)
		if err != nil {
			return 0, fmt.Errorf("read reg %d (slave %d): %w", addr, slaveID, err)
		}
		if len(raw) < 2 {
			return 0, fmt.Errorf("read reg %d (slave %d): short response %d bytes", addr, slaveID, len(raw))
		}
		return binary.BigEndian.Uint16(raw[:2]), nil
	}

	var err error
	regs.RemoteStartStop, err = readUint16(89)
	if err != nil {
		return Registers{}, err
	}
	regs.DeviceAddress, err = readUint16(100)
	if err != nil {
		return Registers{}, err
	}
	regs.MaxChargeCurrent, err = readUint16(109)
	if err != nil {
		return Registers{}, err
	}
	regs.SoftwareVersion, err = readUint16(140)
	if err != nil {
		return Registers{}, err
	}
	regs.WorkingStatus, err = readUint16(141)
	if err != nil {
		return Registers{}, err
	}
	regs.RotarySwitchPWM, err = readUint16(151)
	if err != nil {
		return Registers{}, err
	}
	regs.OutputPWMDuty, err = readUint16(152)
	if err != nil {
		return Registers{}, err
	}

	return regs, nil
}


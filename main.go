package main

import (
	"flag"
	"fmt"
	"bytes"
	"os"
	"sync"
	"time"
)

var (
	version = "0.0.47" // Versión 0.0.47: Límite de descarga de batería vía SHM Deye 0x1238
)

func main() {
	var configPath string
	var once bool

	flag.StringVar(&configPath, "config", "./etek-evse.toml", "config TOML path")
	flag.BoolVar(&once, "once", false, "read once and exit (only for --read-shm)")
	flag.Parse()

	if len(os.Args) > 1 && (os.Args[1] == "--version" || os.Args[1] == "-v") {
		fmt.Println(version) // version is now defined globally below
		return
	}

	cfg, err := Load(configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	fmt.Println("etek-evse-ctl:", version)

	// Inicializar SHM de salida (Status para UI)
	statusWriter, err := NewStatusWriter(0x1230, 1024) // NewStatusWriter is now in package main
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error inicializando SHM Status: %v\n", err)
	} else {
		fmt.Printf("SHM Status (0x1230) inicializada.\n")
		defer statusWriter.Close()
	}

	commandSHM, err := NewCommandSHM(SHM_COMMAND_KEY, SHM_COMMAND_SIZE)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error inicializando SHM de comandos: %v\n", err)
		// No salimos, el programa puede seguir funcionando sin la SHM de comandos
	} else {
		fmt.Printf("SHM Comandos (0x1231) inicializada.\n")
		defer commandSHM.Close()

		// REQUERIMIENTO: Inicializar con Action=0, Val1=0, Val2=0, Source="", Timestamps actuales
		initialCmd := CommandPayload{
			Action:      0,
			Value1:      0,
			Value2:      0,
			TimestampTx: time.Now().Unix(),
			TimestampRx: time.Now().Unix(),
		}
		// Source se inicializa a ceros (string vacío) automáticamente al ser [24]byte
		commandSHM.Write(initialCmd)
	}

	runLoop(cfg, statusWriter, commandSHM, once)
}

func runLoop(cfg *Config, statusWriter *StatusWriter, commandSHM *CommandSHM, once bool) { // Config and StatusWriter are now in package main
	poll := time.Duration(cfg.General.PollingIntervalMS) * time.Millisecond
	if poll <= 0 {
		poll = 1 * time.Second
	}

	var (
		lastCommandTimestamp int64 // Para rastrear comandos ya procesados
		lastTS    int64 // Reader and Data are now in package main
		currentData Data
		integratedPower float32
		currentDeye DeyeData
		deyeFresh   bool
		maxAge    = time.Duration(cfg.SHMMeterRead.MaxDataAgeS) * time.Second
		
		// Buffer circular para integración de potencia (12 muestras)
		powerHistory = make([]float32, 12)
		hIdx         int
		hCount       int

		tsStalled  bool
		dataStale  bool
		lastPeriod string

		evseStatus = make(map[uint8]Registers) // Registers is now in package main
		algorithmLimitW float32
		algorithmMarginW float32
		statusMu   sync.RWMutex
		clientMu   sync.Mutex
	)

	// Inicializamos el lector de SHM del medidor UNA SOLA VEZ antes del bucle
	var shmReader *Reader
	r, err := NewReader(cfg.SHMMeterRead.Key, cfg.SHMMeterRead.Size)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error conectando a SHM Medidor: %v. Continuando...\n", err)
	}
	shmReader = r

	var deyeReader *DeyeReader
	dr, err := NewDeyeReader(cfg.SHMDeyeRead.Key, cfg.SHMDeyeRead.Size)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error conectando a SHM Deye: %v. Continuando...\n", err)
	}
	deyeReader = dr

	// Agrupamos dispositivos por adaptador para evitar colisiones y permitir paralelismo
	devsByAdapter := make(map[string][]EVSEDevice) // EVSEDevice is now in package main
	for _, dev := range cfg.EVSEDevices {
		devsByAdapter[dev.AdapterPath] = append(devsByAdapter[dev.AdapterPath], dev)
	}

	// Mapa de clientes Modbus indexados por ruta de adaptador para reutilizar conexiones
	evseClients := make(map[string]*Client) // Client is now in package main
	defer func() {
		if shmReader != nil {
			shmReader.Close()
		}
		for _, client := range evseClients {
			client.Close()
		}
	}()

	ticker := time.NewTicker(poll)
	defer ticker.Stop()

	for range ticker.C {
		now := time.Now()
		fmt.Print("\033[H\033[2J") // Borrado de pantalla al inicio de cada ciclo

		// --- Procesar comandos de SHM ---
		if commandSHM != nil {
			cmd, st, _, err := commandSHM.Read()
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error leyendo SHM de comandos: %v\n", err) //
			} else if st == ReadValid && cmd.TimestampTx != 0 && cmd.TimestampTx > lastCommandTimestamp { //
				fmt.Printf("Comando recibido: Acción=%d, Valor1=%d, Valor2=%d, Origen=%s\n", //
					cmd.Action, cmd.Value1, cmd.Value2, string(bytes.Trim(cmd.Source[:], "\x00"))) //
				
				// Lógica para ejecutar el comando
				switch cmd.Action {
				case 1: // Start
					modbusID := uint8(cmd.Value1)
					fmt.Printf("Procesando comando START para EVSE Modbus ID: %d\n", modbusID)
					// Encontrar el cliente Modbus para este EVSE
					for _, dev := range cfg.EVSEDevices {
						if dev.ModbusID == modbusID {
							clientMu.Lock()
							client, ok := evseClients[dev.AdapterPath]
							clientMu.Unlock()
							if ok {
								if err := client.WriteRemoteStartStop(modbusID, 1); err != nil {
									fmt.Fprintf(os.Stderr, "Error al enviar START a EVSE %d: %v\n", modbusID, err)
								}
							}
							break
						}
					}
				case 2: // Stop
					modbusID := uint8(cmd.Value1)
					fmt.Printf("Procesando comando STOP para EVSE Modbus ID: %d\n", modbusID)
					for _, dev := range cfg.EVSEDevices {
						if dev.ModbusID == modbusID {
							clientMu.Lock()
							client, ok := evseClients[dev.AdapterPath]
							clientMu.Unlock()
							if ok {
								client.WriteMaxChargeCurrent(modbusID, 1000) // Forzar 6A antes de detener
								if err := client.WriteRemoteStartStop(modbusID, 2); err != nil {
									fmt.Fprintf(os.Stderr, "Error al enviar STOP a EVSE %d: %v\n", modbusID, err)
								}
							}
							break
						}
					}
				}

				// Actualizar el comando en la SHM para indicar que ha sido procesado
				cmd.Action = 0 // Resetear la acción
				cmd.TimestampRx = now.Unix()
				if err := commandSHM.Write(cmd); err != nil {
					fmt.Fprintf(os.Stderr, "Error al escribir comando procesado en SHM: %v\n", err)
				}
				lastCommandTimestamp = cmd.TimestampTx
			}
		}

		period := GetCurrentTariffPeriod(cfg, now) // GetCurrentTariffPeriod is now in package main
		if period != lastPeriod {
			fmt.Printf("Tariff period changed to: %s\n", period)
			lastPeriod = period
		}

		if shmReader != nil {
			d, st, _, err := shmReader.Read()
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}

			fresh := IsFresh(d, maxAge, now) // IsFresh is now in package main
			if st == ReadValid && d.Timestamp != 0 && fresh {
				if dataStale {
					fmt.Println("WARNING: SHM data is fresh again")
					dataStale = false
				}
				currentData = d

				// Actualizar buffer circular con la potencia instantánea
				powerHistory[hIdx] = d.ActivePower
				hIdx = (hIdx + 1) % len(powerHistory)
				if hCount < len(powerHistory) {
					hCount++
				}

				// Calcular la potencia media integrada en el buffer circular
				var sum float32
				for i := 0; i < hCount; i++ {
					sum += powerHistory[i]
				}
				integratedPower = sum / float32(hCount)

				fmt.Printf("Timestamp=%d ActivePower=%.0f W (media imp=%.0f W, media exp=%.0f W)\n",
					d.Timestamp, d.ActivePower, d.PotenciaMediaImportada, d.PotenciaMediaExportada)

				if d.Timestamp != lastTS {
					if tsStalled {
						fmt.Println("WARNING: SHM timestamp resumed updating")
						tsStalled = false
					}
					lastTS = d.Timestamp
				} else if !tsStalled && lastTS != 0 {
					fmt.Println("WARNING: SHM timestamp stopped updating")
					tsStalled = true
				}
			} else if !fresh && !dataStale && d.Timestamp != 0 {
				fmt.Printf("WARNING: SHM data is stale or invalid (status: %v)\n", st)
				dataStale = true
				if st == ReadValid && d.Timestamp != 0 { // ReadValid is now in package main
					fmt.Printf("Timestamp=%d ActivePower=%.0f W (media imp=%.0f W, media exp=%.0f W) [STALE]\n",
						d.Timestamp, d.ActivePower, d.PotenciaMediaImportada, d.PotenciaMediaExportada)
				}
			} else { //
			}
		}

		// --- Leer SHM Deye ---
		if deyeReader != nil {
			dd, st, _, _ := deyeReader.Read()
			if st == ReadValid && dd.Timestamp != 0 {
				age := now.Sub(time.Unix(dd.Timestamp, 0))
				if age >= 0 && age <= maxAge {
					currentDeye = dd
					deyeFresh = true
				} else {
					deyeFresh = false
				}
			}
		}

		timeout := time.Duration(cfg.General.ModbusTimeoutMS) * time.Millisecond
		if timeout <= 0 {
			timeout = 500 * time.Millisecond
		}

		var wg sync.WaitGroup
		for path, devices := range devsByAdapter {
			wg.Add(1)
			go func(adapterPath string, devs []EVSEDevice, d Data, deye DeyeData, dFresh bool, p string, stale bool, pInt float32, limit float32) {
				defer wg.Done()

				clientMu.Lock()
				client, ok := evseClients[adapterPath]
				if !ok {
					baud := devs[0].BaudRate
					if baud == 0 {
						baud = 9600
					}
					client = NewClient(adapterPath, baud, timeout) // NewClient is now in package main
					evseClients[adapterPath] = client
				}
				clientMu.Unlock()

				// Determinar límite de ICP según periodo actual
				var limitW float32 = float32(cfg.PotenciasICP.PPunta)
				if p == "P_VALLE" {
					limitW = float32(cfg.PotenciasICP.PValle)
				}
				algorithmLimitW = limitW

				batLimitW := float32(cfg.General.MaxBatteryDischargeW)
				if batLimitW <= 0 {
					batLimitW = 3000 // Valor por defecto si no está en TOML
				}

				for _, dev := range devs {
					regs, err := client.ReadRegisters(dev.ModbusID) // ReadRegisters is now in package main
					if err != nil {
						fmt.Fprintf(os.Stderr, "evse %q (id=%d) read error: %v\n", dev.Name, dev.ModbusID, err)
						continue
					}

					// 1. Inicialización de estado y rampa desde 6A
					statusMu.RLock()
					_, initialized := evseStatus[dev.ModbusID]
					statusMu.RUnlock()

					if !initialized && regs.MaxChargeCurrent != 1000 && regs.RemoteStartStop == 1 {
						fmt.Printf("EVSE %q: Inicializando rampa. Forzando 6A (PWM 1000)\n", dev.Name)
						client.WriteMaxChargeCurrent(dev.ModbusID, 1000)
						regs.MaxChargeCurrent = 1000
					}

					if d.Timestamp != 0 && !stale {
						// Lógica de Control de PWM (Estados 2 al 5 y 19)
						if (regs.WorkingStatus >= 2 && regs.WorkingStatus <= 5) || regs.WorkingStatus == 19 {
							volts := d.Voltage
							if volts < 180 { volts = 230 }

							// 2. Amperios actuales y Margen (Basado exclusivamente en la Potencia Integrada)
							currentA := (float32(regs.MaxChargeCurrent) / 100.0) * 0.6

							// Lógica del "Peor Caso": comparamos margen de potencia integrada e instantánea
							marginW_int := limitW - pInt
							marginW_inst := limitW - d.ActivePower

							marginW := marginW_int
							if marginW_inst < marginW {
								marginW = marginW_inst
							}
							algorithmMarginW = marginW

							// --- RESTRICCIÓN BATERÍA ---
							// Si tenemos datos frescos de Deye, limitamos la carga para no exceder la descarga de batería.
							if dFresh {
								marginW_bat := batLimitW - deye.BattPower
								if marginW_bat < marginW {
									marginW = marginW_bat
								}
							}

							// 3. Objetivo ideal
							idealTargetA := currentA + (marginW / volts)
							if regs.WorkingStatus != 5 {
								idealTargetA = 6.0
							}

							// 4. Ritmo y Rampa (Slew Rate) - Sin usar medias
							diffA_ideal := idealTargetA - currentA
							var stepLimitA float32

							if diffA_ideal > 0 {
								// Rampa de subida muy lenta: 0.05A por segundo
								stepLimitA = 0.05
							} else {
								// Rampa de bajada suave: 0.1A/s normal, 0.5A/s ante sobrecarga
								stepLimitA = 0.1
								if marginW < 0 {
									stepLimitA = 0.5
								}
							}

							diffA := diffA_ideal
							if diffA > stepLimitA {
								diffA = stepLimitA
							}
							if diffA < -stepLimitA {
								diffA = -stepLimitA
							}
							targetA := currentA + diffA

							// Límites físicos
							if targetA < 6.0 { targetA = 6.0 }
							hwLimitA := (float32(regs.RotarySwitchPWM) / 100.0) * 0.6
							if targetA > hwLimitA { targetA = hwLimitA }

							// 5. Escritura Modbus
							newReg109 := uint16((targetA / 0.6) * 100.0)
							if newReg109 != regs.MaxChargeCurrent && regs.RemoteStartStop == 1 {
								fmt.Printf("MODULANDO %s [%s]: %d -> %d (Status:%d Limit:%.0fW Pi:%.0fW Pint:%.0fW Margin:%.0fW Pace:%.2fA/s)\n",
									dev.Name, p, regs.MaxChargeCurrent, newReg109, regs.WorkingStatus, limitW, d.ActivePower, pInt, marginW, stepLimitA) // WriteMaxChargeCurrent is now in package main
								if errW := client.WriteMaxChargeCurrent(dev.ModbusID, newReg109); errW != nil {
									fmt.Fprintf(os.Stderr, "Error write reg 109 on %s: %v\n", dev.Name, errW)
								}
								regs.MaxChargeCurrent = newReg109
							}
						}
					} else {
						// Lógica de Fallback: Si los datos de SHM no son frescos o válidos,
						// forzamos el PWM al mínimo de seguridad (6A / 1000).
						newReg109 := uint16(1000)
						if regs.MaxChargeCurrent != newReg109 && regs.RemoteStartStop == 1 {
							fmt.Printf("FALLBACK %s: SHM data stale/invalid. Forcing 6A (PWM 1000)\n", dev.Name)
							if errW := client.WriteMaxChargeCurrent(dev.ModbusID, newReg109); errW != nil { // WriteMaxChargeCurrent is now in package main
								fmt.Fprintf(os.Stderr, "Error writing fallback PWM on %s: %v\n", dev.Name, errW)
							}
							regs.MaxChargeCurrent = newReg109
						}
					}

					// Actualizar el mapa de estado global para que la SHM tenga datos frescos
					statusMu.Lock()
					evseStatus[dev.ModbusID] = regs
					statusMu.Unlock()

					fmt.Printf("EVSE %q (id=%d): status=%d current=%d limit=%d reg152=%d\n",
						dev.Name, dev.ModbusID,
						regs.WorkingStatus,
						regs.MaxChargeCurrent,
						regs.RotarySwitchPWM,
						regs.OutputPWMDuty)
					
					// Pequeño retardo entre esclavos en el mismo bus para estabilizar RS485 en RPi
					time.Sleep(50 * time.Millisecond)
				}
			}(path, devices, currentData, currentDeye, deyeFresh, period, dataStale, integratedPower, algorithmLimitW)
		}
		wg.Wait()

		// Escribir estado en SHM para la UI
		if statusWriter != nil {
			statusData := StatusData{ // StatusData is now in package main
				Timestamp:       now.Unix(),
				ActivePower:     currentData.ActivePower,
				IntegratedPower: integratedPower,
				LimitW:          algorithmLimitW,
				MarginW:         algorithmMarginW,
				NumControllers:  int32(len(cfg.EVSEDevices)),
			}
			
			statusMu.RLock()
			idx := 0
			for _, dev := range cfg.EVSEDevices {
				if regs, ok := evseStatus[dev.ModbusID]; ok && idx < 4 {
					statusData.Controllers[idx] = EVSEStatus{ // EVSEStatus is now in package main
						ModbusID:         dev.ModbusID,
						WorkingStatus:    regs.WorkingStatus,
						MaxChargeCurrent: regs.MaxChargeCurrent,
						OutputPWMDuty:    regs.OutputPWMDuty,
						RotarySwitchPWM:  regs.RotarySwitchPWM,
						RemoteStartStop:  regs.RemoteStartStop,
					}
					idx++
				}
			}
			statusMu.RUnlock()
			statusWriter.Write(statusData) // Write is now in package main
		}

		if once {
			return
		}
	}
}

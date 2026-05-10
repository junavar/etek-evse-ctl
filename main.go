package main

import (
	"flag"
	"fmt"
	"os"
	"sync"
	"time"
)

var (
	version = "0.0.36"
)
func main() {
	var configPath string
	var printConfigPowers bool
	var printCurrentPeriod bool
	var readSHM bool
	var readEVSE bool
	var once bool
	var allSamples bool
	var debugSHM bool

	flag.StringVar(&configPath, "config", "./etek-evse.toml", "config TOML path")
	flag.BoolVar(&printConfigPowers, "print-config-powers", false, "print P_PUNTA/P_VALLE from config and exit")
	flag.BoolVar(&printCurrentPeriod, "print-current-period", false, "print current tariff period and exit")
	flag.BoolVar(&readSHM, "read-shm", true, "read meter data from SysV SHM and print powers")
	flag.BoolVar(&readEVSE, "read-evse", true, "read Modbus register 141 from all evse_device entries")
	flag.BoolVar(&once, "once", false, "read once and exit (only for --read-shm)")
	flag.BoolVar(&allSamples, "all-samples", false, "print every valid sample (not only when Timestamp changes)")
	flag.BoolVar(&debugSHM, "debug-shm", false, "print diagnostic info when samples are discarded")
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
	if printConfigPowers {
		fmt.Printf("P_PUNTA=%d W\n", cfg.PotenciasICP.PPunta)
		fmt.Printf("P_VALLE=%d W\n", cfg.PotenciasICP.PValle)
		return
	}

	if printCurrentPeriod {
		period := GetCurrentTariffPeriod(cfg, time.Now())
		fmt.Printf("Current tariff period: %s\n", period)
		return
	}

	// Inicializar SHM de salida (Status para UI)
	statusWriter, err := NewStatusWriter(0x1230, 1024) // NewStatusWriter is now in package main
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error inicializando SHM Status: %v\n", err)
	} else {
		defer statusWriter.Close()
	}

	runLoop(cfg, statusWriter, readSHM, readEVSE, once, allSamples, debugSHM)
}

func runLoop(cfg *Config, statusWriter *StatusWriter, readSHM bool, readEVSE bool, once bool, allSamples bool, debugSHM bool) { // Config and StatusWriter are now in package main
	poll := time.Duration(cfg.General.PollingIntervalMS) * time.Millisecond
	if poll <= 0 {
		poll = 1 * time.Second
	}

	var (
		shmReader *Reader
		lastTS    int64 // Reader and Data are now in package main
		currentData Data
		integratedPower float32
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

	// Agrupamos dispositivos por adaptador para evitar colisiones y permitir paralelismo
	devsByAdapter := make(map[string][]EVSEDevice) // EVSEDevice is now in package main
	for _, dev := range cfg.EVSEDevices {
		devsByAdapter[dev.AdapterPath] = append(devsByAdapter[dev.AdapterPath], dev)
	}

	// Mapa de clientes Modbus indexados por ruta de adaptador para reutilizar conexiones
	evseClients := make(map[string]*Client) // Client is now in package main
	defer func() {
		for _, client := range evseClients {
			client.Close()
		}
	}()

	if readSHM {
		r, err := NewReader(cfg.SHMMeterRead.Key, cfg.SHMMeterRead.Size) // NewReader is now in package main
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		shmReader = r
		defer shmReader.Close()
	}

	ticker := time.NewTicker(poll)
	defer ticker.Stop()

	for range ticker.C {
		now := time.Now()
		fmt.Print("\033[H\033[2J") // Borrado de pantalla al inicio de cada ciclo

		period := GetCurrentTariffPeriod(cfg, now) // GetCurrentTariffPeriod is now in package main
		if period != lastPeriod {
			fmt.Printf("Tariff period changed to: %s\n", period)
			lastPeriod = period
		}

		if readSHM && shmReader != nil {
			d, st, dbg, err := shmReader.Read()
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

				changed := d.Timestamp != lastTS

				if changed {
					if tsStalled {
						fmt.Println("WARNING: SHM timestamp resumed updating")
						tsStalled = false
					}
					lastTS = d.Timestamp
					fmt.Printf("Timestamp=%d ActivePower=%.0f W (media imp=%.0f W, media exp=%.0f W)\n",
						d.Timestamp, d.ActivePower, d.PotenciaMediaImportada, d.PotenciaMediaExportada)
					if once && !readEVSE {
						return
					}
				} else if !tsStalled && lastTS != 0 {
					fmt.Println("WARNING: SHM timestamp stopped updating")
					fmt.Printf("Timestamp=%d ActivePower=%.0f W (media imp=%.0f W, media exp=%.0f W)\n",
						d.Timestamp, d.ActivePower, d.PotenciaMediaImportada, d.PotenciaMediaExportada)
					tsStalled = true
					if once && !readEVSE {
						return
					}
				} else if allSamples {
					fmt.Printf("Timestamp=%d ActivePower=%.0f W (media imp=%.0f W, media exp=%.0f W)\n",
						d.Timestamp, d.ActivePower, d.PotenciaMediaImportada, d.PotenciaMediaExportada)
					if once && !readEVSE {
						return
					}
				}
			} else if !fresh && !dataStale && d.Timestamp != 0 {
				fmt.Printf("WARNING: SHM data is stale or invalid (status: %v)\n", st)
				dataStale = true
				if st == ReadValid && d.Timestamp != 0 { // ReadValid is now in package main
					fmt.Printf("Timestamp=%d ActivePower=%.0f W (media imp=%.0f W, media exp=%.0f W) [STALE]\n",
						d.Timestamp, d.ActivePower, d.PotenciaMediaImportada, d.PotenciaMediaExportada)
				}
			} else if debugSHM {
				fmt.Printf("discarded: status=%s ts=%d fresh=%t wantCrc=0x%08x gotCrc=0x%08x\n",
					st.String(), d.Timestamp, fresh, dbg.WantCRC, dbg.GotCRC)
			}
		}

		if readEVSE {
			timeout := time.Duration(cfg.General.ModbusTimeoutMS) * time.Millisecond
			if timeout <= 0 {
				timeout = 500 * time.Millisecond
			}

			var wg sync.WaitGroup
			for path, devices := range devsByAdapter {
				wg.Add(1)
				go func(adapterPath string, devs []EVSEDevice, d Data, p string, stale bool, pInt float32, limit float32) { // EVSEDevice and Data are now in package main
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
						if !initialized {
							if regs.MaxChargeCurrent != 1000 && regs.RemoteStartStop == 1 {
								fmt.Printf("EVSE %q: Inicializando rampa. Forzando 6A (PWM 1000)\n", dev.Name) // WriteMaxChargeCurrent is now in package main
								client.WriteMaxChargeCurrent(dev.ModbusID, 1000)
								regs.MaxChargeCurrent = 1000
							}
							statusMu.Lock()
							evseStatus[dev.ModbusID] = regs
							statusMu.Unlock()
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

						fmt.Printf("EVSE %q (id=%d): status=%d current=%d limit=%d reg152=%d\n",
							dev.Name, dev.ModbusID,
							regs.WorkingStatus,
							regs.MaxChargeCurrent,
							regs.RotarySwitchPWM,
							regs.OutputPWMDuty)
						
						// Pequeño retardo entre esclavos en el mismo bus para estabilizar RS485 en RPi
						time.Sleep(50 * time.Millisecond)
					}
				}(path, devices, currentData, period, dataStale, integratedPower, algorithmLimitW)
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
}

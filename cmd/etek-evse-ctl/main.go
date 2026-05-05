package main

import (
	"flag"
	"fmt"
	"os"
	"sync"
	"time"

	"etek-evse-ctl/internal/config"
	"etek-evse-ctl/internal/evse"
	"etek-evse-ctl/internal/meter"
)

var (
	version = "0.1.18"
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
		fmt.Println(version)
		return
	}

	cfg, err := config.Load(configPath)
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
		period := cfg.GetCurrentTariffPeriod(time.Now())
		fmt.Printf("Current tariff period: %s\n", period)
		return
	}

	runLoop(cfg, readSHM, readEVSE, once, allSamples, debugSHM)
}

func runLoop(cfg *config.Config, readSHM bool, readEVSE bool, once bool, allSamples bool, debugSHM bool) {
	poll := time.Duration(cfg.General.PollingIntervalMS) * time.Millisecond
	if poll <= 0 {
		poll = 1 * time.Second
	}

	var (
		shmReader *meter.Reader
		lastTS    int64
		currentData meter.Data
		maxAge    = time.Duration(cfg.SHMMeterRead.MaxDataAgeS) * time.Second
		tsStalled  bool
		dataStale  bool
		lastPeriod string

		// Estado compartido para coordinación entre hilos Modbus
		evseStatus = make(map[uint8]evse.Registers)
		statusMu   sync.RWMutex
	)

	// Agrupamos dispositivos por adaptador para evitar colisiones y permitir paralelismo
	devsByAdapter := make(map[string][]config.EVSEDevice)
	for _, dev := range cfg.EVSEDevices {
		devsByAdapter[dev.AdapterPath] = append(devsByAdapter[dev.AdapterPath], dev)
	}

	// Mapa de clientes Modbus indexados por ruta de adaptador para reutilizar conexiones
	evseClients := make(map[string]*evse.Client)
	defer func() {
		for _, client := range evseClients {
			client.Close()
		}
	}()

	if readSHM {
		r, err := meter.NewReader(cfg.SHMMeterRead.Key, cfg.SHMMeterRead.Size)
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
		period := cfg.GetCurrentTariffPeriod(now)
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

			fresh := meter.IsFresh(d, maxAge, now)
			if st == meter.ReadValid && d.Timestamp != 0 && fresh {
				if dataStale {
					fmt.Println("WARNING: SHM data is fresh again")
					dataStale = false
				}
				currentData = d
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
			} else if !fresh && !dataStale {
				fmt.Printf("WARNING: SHM data is stale or invalid (status: %s)\n", st.String())
				dataStale = true
				if st == meter.ReadValid && d.Timestamp != 0 {
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
				go func(adapterPath string, devs []config.EVSEDevice, d meter.Data, p string, stale bool) {
					defer wg.Done()

					client, ok := evseClients[adapterPath]
					if !ok {
						baud := devs[0].BaudRate
						if baud == 0 {
							baud = 9600
						}
						client = evse.NewClient(adapterPath, baud, timeout)
						evseClients[adapterPath] = client
					}

					// Determinar límite de ICP según periodo actual
					var limitW float32 = float32(cfg.PotenciasICP.PPunta)
					if p == "P_VALLE" {
						limitW = float32(cfg.PotenciasICP.PValle)
					}

					for _, dev := range devs {
						regs, err := client.ReadRegisters(dev.ModbusID)
						if err != nil {
							fmt.Fprintf(os.Stderr, "evse %q (id=%d) read error: %v\n", dev.Name, dev.ModbusID, err)
							continue
						}

						// 1. Inicialización de estado y rampa desde 6A
						_, initialized := evseStatus[dev.ModbusID]
						if !initialized {
							if regs.MaxChargeCurrent != 1000 && regs.RemoteStartStop == 1 {
								fmt.Printf("EVSE %q: Inicializando rampa. Forzando 6A (PWM 1000)\n", dev.Name)
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

								// 2. Amperios actuales y Márgenes
								currentA := (float32(regs.MaxChargeCurrent) / 100.0) * 0.6
								marginW_mean := limitW - d.PotenciaMediaImportada
								marginW_inst := limitW - d.ActivePower

								marginW := marginW_mean
								if marginW_inst < marginW {
									marginW = marginW_inst
								}

								// 3. Objetivo ideal
								idealTargetA := currentA + (marginW / volts)
								if regs.WorkingStatus != 5 {
									idealTargetA = 6.0
								}

								// 4. Ritmo y Rampa (Slew Rate)
								diffA_ideal := idealTargetA - currentA
								rhythmW := d.PotenciaMediaImportada - d.ActivePower
								if rhythmW < 0 { rhythmW = -rhythmW }

								dynamicStepA := float32(0.05) + (rhythmW/1000.0)*0.1
								var stepLimitA float32
								if diffA_ideal > 0 {
									stepLimitA = 0.5 // Rampa de subida rápida
									if dynamicStepA > stepLimitA { stepLimitA = dynamicStepA }
								} else {
									stepLimitA = dynamicStepA // Rampa de bajada dinámica
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
									fmt.Printf("MODULANDO %s [%s]: %d -> %d (Status:%d Limit:%.0fW Margin:%.0fW Pace:%.2fA/s)\n",
										dev.Name, p, regs.MaxChargeCurrent, newReg109, regs.WorkingStatus, limitW, marginW, stepLimitA)
									if errW := client.WriteMaxChargeCurrent(dev.ModbusID, newReg109); errW != nil {
										fmt.Fprintf(os.Stderr, "Error write reg 109 on %s: %v\n", dev.Name, errW)
									}
									regs.MaxChargeCurrent = newReg109
								}
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
				}(path, devices, currentData, period, dataStale)
			}
			wg.Wait()

			if once {
				return
			}
		}
	}
}

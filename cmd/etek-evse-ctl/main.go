package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"etek-evse-ctl/internal/config"
	"etek-evse-ctl/internal/evse"
	"etek-evse-ctl/internal/meter"
)

var (
	version = "0.0.13"
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
		maxAge    = time.Duration(cfg.SHMMeterRead.MaxDataAgeS) * time.Second
		tsStalled  bool
		dataStale  bool
		lastPeriod string
	)

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

	for {
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
				changed := d.Timestamp != lastTS

				if dataStale {
					fmt.Println("WARNING: SHM data is fresh again")
					dataStale = false
				}

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
				fmt.Println("WARNING: SHM data is stale")
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

			for _, dev := range cfg.EVSEDevices {
				baud := dev.BaudRate
				if baud == 0 {
					baud = 9600
				}

				// Obtener o crear el cliente para este adaptador
				client, ok := evseClients[dev.AdapterPath]
				if !ok {
					client = evse.NewClient(dev.AdapterPath, baud, timeout)
					evseClients[dev.AdapterPath] = client
				}

				regs, err := client.ReadRegisters(dev.ModbusID)
				if err != nil {
					fmt.Fprintf(os.Stderr, "evse %q (id=%d): %v\n", dev.Name, dev.ModbusID, err)
					continue
				}
				fmt.Printf("EVSE %q (id=%d): reg89=%d reg100=%d reg109=%d reg140=%d reg141=%d reg151=%d reg152=%d\n",
					dev.Name, dev.ModbusID,
					regs.RemoteStartStop,
					regs.DeviceAddress,
					regs.MaxChargeCurrent,
					regs.SoftwareVersion,
					regs.WorkingStatus,
					regs.RotarySwitchPWM,
					regs.OutputPWMDuty)
			}

			if once {
				return
			}
		}

		time.Sleep(poll)
	}
}

package config

import (
	"fmt"
	"time"

	"github.com/BurntSushi/toml"
)

type Config struct {
	General               General               `toml:"general"`
	SHMMeterRead          SHMMeterRead          `toml:"shm_meter_read"`
	EVSEDevices           []EVSEDevice          `toml:"evse_device"`
	PotenciasICP          PotenciasICP          `toml:"potencias_ICP"`
	HorariosLaborables    HorariosLaborables    `toml:"horarios_potencia_laborables"`
	Festivos              Festivos              `toml:"festivos"`
}

type General struct {
	Version           string `toml:"version"`
	PollingIntervalMS int    `toml:"polling_interval_ms"`
	ModbusTimeoutMS   int    `toml:"modbus_timeout_ms"`
	FallbackPWM       int    `toml:"fallback_pwm"`
}

type PotenciasICP struct {
	PPunta int `toml:"P_PUNTA"`
	PValle int `toml:"P_VALLE"`
}

type HorariosLaborables struct {
	Intervalos [][3]string `toml:"intervalos"`
}

type Festivos struct {
	DiasFinDeSemana       []string `toml:"dias_fin_de_semana"`
	DiasFestivosFijos     []string `toml:"dias_festivos_fijos"`
	DiasFestivosVariables []string `toml:"dias_festivos_variables"`
}

type SHMMeterRead struct {
	Key         uint32 `toml:"key"`
	Size        int    `toml:"size"`
	MaxDataAgeS int    `toml:"max_data_age_s"`
}

type EVSEDevice struct {
	Name        string `toml:"name"`
	ModbusID    uint8  `toml:"modbus_id"`
	AdapterPath string `toml:"adapter_path"`
	BaudRate    int    `toml:"baud_rate"`
}

// GetCurrentTariffPeriod returns "P_VALLE", "P_LLANO", or "P_PUNTA" based on current time and config.
// Note: P_LLANO is not defined in spec, assuming it's for times not specified, but currently only P_VALLE and P_PUNTA are used.
func (c *Config) GetCurrentTariffPeriod(now time.Time) string {
	// Check if it's a holiday or weekend
	if c.isHoliday(now) {
		return "P_VALLE"
	}

	// Weekday, check intervals
	for _, interval := range c.HorariosLaborables.Intervalos {
		if len(interval) != 3 {
			continue
		}
		start := interval[0]
		end := interval[1]
		period := interval[2]

		if c.isTimeInInterval(now, start, end) {
			return period
		}
	}

	// Default to P_VALLE if no match
	return "P_VALLE"
}

func (c *Config) isHoliday(t time.Time) bool {
	// Check weekends
	weekday := t.Weekday().String()
	for _, wd := range c.Festivos.DiasFinDeSemana {
		if wd == weekday {
			return true
		}
	}

	// Check fixed holidays
	dateStr := fmt.Sprintf("%02d-%02d", t.Day(), int(t.Month()))
	for _, d := range c.Festivos.DiasFestivosFijos {
		if d == dateStr {
			return true
		}
	}

	// Check variable holidays
	fullDateStr := fmt.Sprintf("%02d-%02d-%04d", t.Day(), int(t.Month()), t.Year())
	for _, d := range c.Festivos.DiasFestivosVariables {
		if d == fullDateStr {
			return true
		}
	}

	return false
}

func (c *Config) isTimeInInterval(t time.Time, start, end string) bool {
	startTime, err := time.Parse("15:04", start)
	if err != nil {
		return false
	}
	endTime, err := time.Parse("15:04", end)
	if err != nil {
		return false
	}

	currentTime := time.Date(0, 1, 1, t.Hour(), t.Minute(), 0, 0, time.UTC)

	if startTime.Before(endTime) {
		return !currentTime.Before(startTime) && currentTime.Before(endTime)
	} else {
		// Overnight interval, e.g., 22:00 to 00:00
		return !currentTime.Before(startTime) || currentTime.Before(endTime)
	}
}

func Load(path string) (*Config, error) {
	var cfg Config
	if _, err := toml.DecodeFile(path, &cfg); err != nil {
		return nil, fmt.Errorf("load config %q: %w", path, err)
	}
	return &cfg, nil
}


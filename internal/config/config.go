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

	// Campos cacheados para optimización en ARMv6
	parsedIntervals  []timeInterval
	weekendMap       map[string]bool
	fixedHolidays    map[string]bool
	variableHolidays map[string]bool
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

type timeInterval struct {
	start  time.Time
	end    time.Time
	period string
}

// GetCurrentTariffPeriod returns "P_VALLE", "P_LLANO", or "P_PUNTA" based on current time and config.
// Note: P_LLANO is not defined in spec, assuming it's for times not specified, but currently only P_VALLE and P_PUNTA are used.
func (c *Config) GetCurrentTariffPeriod(now time.Time) string {
	if c.isHoliday(now) {
		return "P_VALLE"
	}

	currentTime := time.Date(0, 1, 1, now.Hour(), now.Minute(), 0, 0, time.UTC)

	for _, ti := range c.parsedIntervals {
		if ti.start.Before(ti.end) {
			if !currentTime.Before(ti.start) && currentTime.Before(ti.end) {
				return ti.period
			}
		} else {
			// Intervalo nocturno (ej: 22:00 a 00:00)
			if !currentTime.Before(ti.start) || currentTime.Before(ti.end) {
				return ti.period
			}
		}
	}

	return "P_VALLE"
}

func (c *Config) isHoliday(t time.Time) bool {
	if c.weekendMap[t.Weekday().String()] {
		return true
	}
	if c.fixedHolidays[fmt.Sprintf("%02d-%02d", t.Day(), t.Month())] {
		return true
	}
	if c.variableHolidays[fmt.Sprintf("%02d-%02d-%04d", t.Day(), t.Month(), t.Year())] {
		return true
	}
	return false
}

func Load(path string) (*Config, error) {
	var cfg Config
	if _, err := toml.DecodeFile(path, &cfg); err != nil {
		return nil, fmt.Errorf("load config %q: %w", path, err)
	}

	// Pre-parsear intervalos de horas
	for _, interval := range cfg.HorariosLaborables.Intervalos {
		if len(interval) != 3 {
			continue
		}
		s, errS := time.Parse("15:04", interval[0])
		e, errE := time.Parse("15:04", interval[1])
		if errS == nil && errE == nil {
			cfg.parsedIntervals = append(cfg.parsedIntervals, timeInterval{
				start:  s,
				end:    e,
				period: interval[2],
			})
		}
	}

	// Pre-calcular mapas de festivos para búsqueda rápida O(1)
	cfg.weekendMap = make(map[string]bool)
	for _, d := range cfg.Festivos.DiasFinDeSemana {
		cfg.weekendMap[d] = true
	}
	cfg.fixedHolidays = make(map[string]bool)
	for _, d := range cfg.Festivos.DiasFestivosFijos {
		cfg.fixedHolidays[d] = true
	}
	cfg.variableHolidays = make(map[string]bool)
	for _, d := range cfg.Festivos.DiasFestivosVariables {
		cfg.variableHolidays[d] = true
	}

	return &cfg, nil
}

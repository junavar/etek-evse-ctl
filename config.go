package main // Already main, no change needed here

import (
	"github.com/BurntSushi/toml"
	"time"
)

type Config struct {
	General                  GeneralConfig             `toml:"general"`
	SHMMeterRead             SHMConfig                 `toml:"shm_meter_read"`
	SHMETEKCreateWriteRead   SHMConfig                 `toml:"shm_etek_create_write_read"`
	EVSEDevices              []EVSEDevice              `toml:"evse_device"`
	PotenciasICP             PotenciasICP              `toml:"potencias_ICP"`
	HorariosLaborables       HorariosPotencia          `toml:"horarios_potencia_laborables"`
	Festivos                 FestivosConfig            `toml:"festivos"`
}

type GeneralConfig struct {
	PollingIntervalMS int `toml:"polling_interval_ms"`
	ModbusTimeoutMS    int `toml:"modbus_timeout_ms"`
	FallbackPWM       int `toml:"fallback_pwm"`
}

type SHMConfig struct {
	Key          uint32 `toml:"key"`
	Size         int    `toml:"size"`
	MaxDataAgeS  int    `toml:"max_data_age_s"`
}

type EVSEDevice struct {
	Name        string `toml:"name"`
	ModbusID    uint8  `toml:"modbus_id"`
	MaxPower    int    `toml:"max_power"`
	AdapterPath string `toml:"adapter_path"`
	BaudRate    int    `toml:"baud_rate"`
}

type PotenciasICP struct {
	PPunta int `toml:"P_PUNTA"`
	PValle int `toml:"P_VALLE"`
}

type HorariosPotencia struct {
	Intervalos [][]string `toml:"intervalos"`
}

type FestivosConfig struct {
	DiasFinDeSemana       []string `toml:"dias_fin_de_semana"`
	DiasFestivosFijos     []string `toml:"dias_festivos_fijos"`
	DiasFestivosVariables []string `toml:"dias_festivos_variables"`
}

func Load(path string) (*Config, error) {
	var cfg Config
	if _, err := toml.DecodeFile(path, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func GetCurrentTariffPeriod(c *Config, t time.Time) string { // Already a function, no change needed here
	day := t.Weekday().String()
	date := t.Format("02-01")
	fullDate := t.Format("02-01-2006")

	// Fines de semana y festivos son siempre P_VALLE
	for _, d := range c.Festivos.DiasFinDeSemana {
		if d == day { return "P_VALLE" }
	}
	for _, d := range c.Festivos.DiasFestivosFijos {
		if d == date { return "P_VALLE" }
	}
	for _, d := range c.Festivos.DiasFestivosVariables {
		if d == fullDate { return "P_VALLE" }
	}

	// Días laborables: buscar en intervalos
	nowStr := t.Format("15:04")
	for _, interval := range c.HorariosLaborables.Intervalos {
		if nowStr >= interval[0] && nowStr < interval[1] {
			return interval[2]
		}
	}

	return "P_PUNTA" // Por defecto
}
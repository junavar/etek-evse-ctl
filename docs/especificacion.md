# Documento de Especificación Técnica — etek-evse-ctl

**Fecha**: 29 de abril de 2026  
**Versión**: 0.4.6  
**Estado**: Borrador

## 1. Arquitectura de Software

-   **Nombre del binario:** etek-evse-ctl (ejecución como
    daemon/servicio).

-   **Lenguaje:** Go (Golang).

-   **Plataforma objetivo:** Raspberry Pi ARMv6 (Pi Zero / Pi 1).

-   **Configuración:** Fichero externo etek-evse-ctl.toml (versionado).

-   **Entrada/salida:** Adaptador USB-RS485 Modbus RTU.

-   **Area de memoria compartida (IPC):**

    -   **Creación/Lectura/escritura:** Área propia para comunicación
        con futura App Web móvil.

    -   **Lectura:** Memoria compartida System V (segmento de 64 bytes)
        para el medidor de red.

-   **Gestión de Logs:** La RPi debe configurarse para que los logs
    residan en memoria volátil (RAM) para evitar el desgaste de la
    tarjeta SD.

## 2. Memoria Compartida del Medidor (lectura)

Segmento crítico actualizado por un proceso externo (lector Eastron).

-   **Clave SHM:** 0x00001264 \| **Flags:** IPC_CREAT (01000).

-   **Sincronismo:** El programa monitorizará cambios en el campo
    Timestamp para iniciar el ciclo de control (polling activo).

-   **Integridad:** Se debe validar cada lectura mediante
    crc32.ChecksumIEEE sobre los primeros 60 bytes. Si el resultado no
    coincide con el campo Crc32, la muestra se descarta.

-   **Estructura `Data` (64 bytes):**

```go
type Data struct {
	Timestamp                int64   // Tiempo Unix de 64 bits (Trigger de ciclo)
	ActivePower              float32 // Potencia activa (W). + Importada, - Exportada
	ReactivePower            float32 // Potencia reactiva (VAR)
	Voltage                  float32 // Tensión de red (V)
	Current                  float32 // Intensidad (A)
	Frequency                float32 // Frecuencia (Hz)
	PowerFactor              float32 // Factor de potencia
	ImportActiveEnergy       float32 // Energía activa importada acumulada
	ExportActiveEnergy       float32 // Energía activa exportada acumulada
	ImportReactiveEnergy     float32 // Energía reactiva importada acumulada
	ExportReactiveEnergy     float32 // Energía reactiva exportada acumulada
	PotenciaMediaImportada   float32 // Media de potencia importada en ventana
	PotenciaMediaExportada   float32 // Media de potencia exportada en ventana
	Ventana                  int32   // Tamaño de la ventana de integración (s)
	Crc32                    uint32  // Checksum. 0: No usado; 0xFFFFFFFF: Error de lectura
}
```

## 3. Interfaz de Hardware y Modbus (RS485)

-   **Adaptadores:** USB a RS485 RTU identificados por ruta física
    (by-path).

-   **Persistencia:** Cada adaptador se prefijará a una posición física
    del puerto USB.

    -   **Nota de puerto:** Se priorizará la firma física final (ej.
        usb-0:1.2:1.0-port0). Si el prefijo de plataforma no coincide
        pero la cadena final sí, se conectará emitiendo aviso en log.

-   **Bus:** Soporte para múltiples EKEPC2 (ID 1, 2, etc.) a 9600 bps.

-   **Nota de ID RS485:** El ID de fábrica es 255. Tras el cambio de ID,
    el dispositivo seguirá respondiendo también a solicitudes enviadas
    al ID 255.

**Mapa de Registros EKEPC2 (16-bit Integer):**

  ----------------------------------------------------------------------------------
  **Reg**   **Tipo**   **Función**       **Nota Técnica**
  --------- ---------- ----------------- -------------------------------------------
  89        R/W        Remote Start/Stop 1: Start, 2: Stop.

  100       R/W        Device Address    ID del dispositivo RS485.

  109       R/W        Max Charge        PWM (\$A \\times 100\$). Máx 9000.
                       Current           Fallback: 1000 (6A).

  140       R          Software Version  v1.1 = 1002; v1.2 = 2310.

  141       R          Working Status    Estado actual (0-19).

  151       R          Rotary Switch PWM Límite físico (Dial hardware).

  152       R          Output PWM Duty   10000 (100%) en estados de espera (1 o 3).
  ----------------------------------------------------------------------------------

## 4. Lógica de Control y Tarifas (2.0TD)

El programa interrogará a los EVSE cuando cambie el Timestamp en la SHM
o expire el modbus_timeout_ms.

-   **Validación:** Datos inválidos si Timestamp supera max_data_age_s o
    si Crc32 == 0xFFFFFFFF.

-   **Lógica de Fallback:** Ante datos inválidos o SHM inexistente, se
    forzará el Reg 109 a 1000 (6A).

-   **Lógica de Control de Potencia:**

    1.  **Importación:** No superar potencia contratada modulando EVSE
        (mín. PWM=1000). Desconexión (Stop) secuencial: 1º Cargador 2,
        2º Cargador 1.

    2.  **Restricción Horaria (Sin Solar):** Bloqueo de carga (Stop)
        fuera de P_VALLE salvo condiciones de exportación.

    3.  **Control Solar (Con Solar):** Evitar exportación forzando
        conexión de EVSE y modulando potencia según excedente
        disponible.

-   **Calendario:** Sábados, domingos y festivos nacionales (fijos y
    variables) son Valle (P3) 24h.

## 5. Fichero de configuración `etek-evse-ctl.toml`

-   **Ruta por defecto**: `./etek-evse-ctl.toml` (mismo directorio que el ejecutable).
-   **Ruta alternativa (servicio)**: `/etc/etek-evse-ctl/etek-evse-ctl.toml`.

```toml
[general]
version = "1.0"
polling_interval_ms = 1000
modbus_timeout_ms = 500
fallback_pwm = 1000

[shm_meter_read]
key = 0x00001264
size = 64
max_data_age_s = 5

[shm_etek_create_write_read]
key = 0x00001265
size = 1024

[[evse_device]]
name = "Cargador Garaje 1"
modbus_id = 1
max_power = 4000
adapter_path = '/dev/serial/by-path/platform-3f980000.usb-usb-0:1.2:1.0-port0'
baud_rate = 9600

[[evse_device]]
name = "Cargador Garaje 2"
modbus_id = 2
max_power = 4000
adapter_path = '/dev/serial/by-path/platform-3f980000.usb-usb-0:1.2:1.0-port1'
baud_rate = 9600

[potencias_ICP]
P_PUNTA = 4600
P_VALLE = 9000

[horarios_potencia_laborables]
intervalos = [
  ["00:00", "08:00", "P_VALLE"],
  ["08:00", "10:00", "P_PUNTA"],
  ["10:00", "14:00", "P_PUNTA"],
  ["14:00", "18:00", "P_PUNTA"],
  ["18:00", "22:00", "P_PUNTA"],
  ["22:00", "00:00", "P_PUNTA"],
]

[festivos]
dias_fin_de_semana = ["Saturday", "Sunday"]
dias_festivos_fijos = ["01-01", "06-01", "01-05", "15-08", "12-10", "01-11", "06-12", "08-12", "25-12"]
dias_festivos_variables = [
  "29-03-2024", "18-04-2025", "03-04-2026", "26-03-2027", "14-04-2028", "30-03-2029",
  "19-04-2030", "11-04-2031", "26-03-2032", "15-04-2033", "07-04-2034", "23-03-2035",
  "11-04-2036", "03-04-2037", "23-04-2038", "08-04-2039", "30-03-2040", "19-04-2041",
  "04-04-2042", "27-03-2043", "15-04-2044", "07-04-2045", "23-03-2046", "12-04-2047",
  "03-04-2048", "16-04-2049", "08-04-2050", "31-03-2051", "19-04-2052", "04-04-2053",
  "27-03-2054", "16-04-2055", "31-03-2056", "20-04-2057", "12-04-2058", "28-03-2059",
  "16-04-2060", "08-04-2061", "24-03-2062", "13-04-2063", "04-04-2064", "27-03-2065",
  "16-04-2066", "01-04-2067", "20-04-2068", "12-04-2069", "28-03-2070", "17-04-2071",
  "08-04-2072", "24-03-2073",
]
```

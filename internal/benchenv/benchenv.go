// Package benchenv - окружение замеров производительности:
// железо хоста и ресурсы процессов из /proc. Общий для cmd/syncload (слой /sync в
// процессе) и cmd/loadgen (сервер целиком по сети): цифры без железа не сравнимы,
// а формат должен совпадать, чтобы отчеты читались одинаково.
package benchenv

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
)

// procRoot - корень procfs (тесты подменяют).
var procRoot = "/proc"

// Hardware - на чем снят замер.
type Hardware struct {
	CPUModel   string  `json:"cpu_model"`
	CPUs       int     `json:"cpus"`
	MemTotalKB int64   `json:"mem_total_kb"`
	Load1      float64 `json:"load1"`
	Go         string  `json:"go"`
	Postgres   string  `json:"postgres"`
}

func (h Hardware) String() string {
	return fmt.Sprintf("%s, %d CPU, %d МиБ RAM, load1 %.1f, %s, %s",
		h.CPUModel, h.CPUs, h.MemTotalKB/1024, h.Load1, h.Go, h.Postgres)
}

// DetectHardware читает /proc (Linux); недоступное - пусто. postgres - версия
// сервера БД ("" - неизвестна), ее запрашивает вызывающий.
func DetectHardware(postgres string) Hardware {
	hw := Hardware{CPUs: runtime.NumCPU(), Go: runtime.Version(), CPUModel: "unknown", Postgres: postgres}

	if data, err := os.ReadFile(procRoot + "/cpuinfo"); err == nil {
		for line := range strings.SplitSeq(string(data), "\n") {
			if name, ok := strings.CutPrefix(line, "model name"); ok {
				hw.CPUModel = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(name), ":"))

				break
			}
		}
	}

	hw.MemTotalKB = ProcField(procRoot+"/meminfo", "MemTotal:")

	if data, err := os.ReadFile(procRoot + "/loadavg"); err == nil {
		if fields := strings.Fields(string(data)); len(fields) > 0 {
			hw.Load1, _ = strconv.ParseFloat(fields[0], 64)
		}
	}

	return hw
}

// ProcField читает числовое поле (кБ) из файла вида /proc/meminfo; нет - 0.
func ProcField(path, prefix string) int64 {
	data, err := os.ReadFile(path) //nolint:gosec // путь в /proc собирает код, а не пользователь
	if err != nil {
		return 0
	}

	for line := range strings.SplitSeq(string(data), "\n") {
		if !strings.HasPrefix(line, prefix) {
			continue
		}

		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0
		}

		value, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return 0
		}

		return value
	}

	return 0
}

// SelfRSSKB - RSS текущего процесса в килобайтах.
func SelfRSSKB() int64 { return ProcField(procRoot+"/self/status", "VmRSS:") }

// clockTicks - USER_HZ: в Linux на amd64/arm64 всегда 100 (sysconf без cgo недоступен).
const clockTicks = 100

// ProcessSample - ресурсы чужого процесса (сервер под нагрузкой) в момент замера.
type ProcessSample struct {
	// RSSKB - резидентная память, кБ.
	RSSKB int64 `json:"rss_kb"`
	// CPUSeconds - накопленное время CPU (user + system), секунды.
	CPUSeconds float64 `json:"cpu_seconds"`
	// Threads - число потоков ОС.
	Threads int64 `json:"threads"`
}

// ReadProcess читает /proc/<pid>/stat и status. Ошибка - процесса нет или /proc недоступен.
func ReadProcess(pid int) (ProcessSample, error) {
	base := fmt.Sprintf("%s/%d", procRoot, pid)

	data, err := os.ReadFile(base + "/stat") //nolint:gosec // pid - число, путь в /proc
	if err != nil {
		return ProcessSample{}, fmt.Errorf("benchenv: процесс %d: %w", pid, err)
	}

	cpu, err := parseStatCPU(string(data))
	if err != nil {
		return ProcessSample{}, fmt.Errorf("benchenv: процесс %d: %w", pid, err)
	}

	return ProcessSample{
		RSSKB:      ProcField(base+"/status", "VmRSS:"),
		CPUSeconds: cpu,
		Threads:    ProcField(base+"/status", "Threads:"),
	}, nil
}

// parseStatCPU достает utime+stime из /proc/<pid>/stat. Имя процесса (поле 2) в
// скобках может содержать пробелы, поэтому поля считаются после последней ")".
func parseStatCPU(stat string) (float64, error) {
	end := strings.LastIndexByte(stat, ')')
	if end < 0 {
		return 0, fmt.Errorf("stat без имени процесса: %.60q", stat)
	}

	// После ")" идут поля начиная с 3-го (state); utime - 14-е, stime - 15-е.
	fields := strings.Fields(stat[end+1:])
	if len(fields) < 13 {
		return 0, fmt.Errorf("stat: мало полей (%d)", len(fields))
	}

	utime, err := strconv.ParseFloat(fields[11], 64)
	if err != nil {
		return 0, fmt.Errorf("stat utime: %w", err)
	}

	stime, err := strconv.ParseFloat(fields[12], 64)
	if err != nil {
		return 0, fmt.Errorf("stat stime: %w", err)
	}

	return (utime + stime) / clockTicks, nil
}

package benchenv

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeProc - procfs в каталоге теста.
func fakeProc(t *testing.T, files map[string]string) {
	t.Helper()

	root := t.TempDir()
	for name, body := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	prev := procRoot
	procRoot = root

	t.Cleanup(func() { procRoot = prev })
}

func TestDetectHardware(t *testing.T) {
	fakeProc(t, map[string]string{
		"cpuinfo": "processor\t: 0\nmodel name\t: Test CPU @ 2.10GHz\n",
		"meminfo": "MemTotal:       65853576 kB\nMemFree: 1 kB\n",
		"loadavg": "2.35 1.00 0.50 1/100 42\n",
	})

	hw := DetectHardware("PostgreSQL 17.12")
	if hw.CPUModel != "Test CPU @ 2.10GHz" || hw.MemTotalKB != 65853576 || hw.Load1 != 2.35 || hw.Postgres != "PostgreSQL 17.12" {
		t.Fatalf("hw = %+v", hw)
	}

	if !strings.Contains(hw.String(), "64310 МиБ RAM") {
		t.Fatalf("String() = %q", hw.String())
	}
}

func TestDetectHardwareNoProc(t *testing.T) {
	fakeProc(t, nil)

	hw := DetectHardware("")
	if hw.CPUModel != "unknown" || hw.MemTotalKB != 0 || hw.CPUs == 0 {
		t.Fatalf("hw = %+v", hw)
	}
}

func TestProcField(t *testing.T) {
	fakeProc(t, map[string]string{
		"status": "Name: x\nVmRSS:\t  40424 kB\nBroken:\nBad: zz kB\n",
	})

	cases := map[string]int64{"VmRSS:": 40424, "Broken:": 0, "Bad:": 0, "Missing:": 0}
	for prefix, want := range cases {
		if got := ProcField(procRoot+"/status", prefix); got != want {
			t.Errorf("%s = %d, ожидали %d", prefix, got, want)
		}
	}

	if got := ProcField(procRoot+"/nope", "VmRSS:"); got != 0 {
		t.Errorf("нет файла: %d", got)
	}
}

func TestReadProcess(t *testing.T) {
	// Имя процесса со скобкой и пробелами: поля считаются после последней ")".
	stat := "4242 (sy ngod) x) S 1 4242 4242 0 -1 4194560 100 0 0 0 250 50 0 0 20 0 12 0 1 1 1"
	fakeProc(t, map[string]string{
		"4242/stat":   stat,
		"4242/status": "VmRSS:\t 1024 kB\nThreads:\t12\n",
		"7/stat":      "7 (short) S 1 2",
		"8/stat":      "8 (bad) S 1 2 3 4 5 6 7 8 9 10 x 1",
		"9/stat":      "9 no-parens",
	})

	got, err := ReadProcess(4242)
	if err != nil {
		t.Fatal(err)
	}

	if got.RSSKB != 1024 || got.Threads != 12 || got.CPUSeconds != 3 {
		t.Fatalf("sample = %+v", got)
	}

	for _, pid := range []int{1, 7, 8, 9} {
		if _, err := ReadProcess(pid); err == nil {
			t.Errorf("pid %d: ожидали ошибку", pid)
		}
	}
}

func TestSelfRSSKB(t *testing.T) {
	if SelfRSSKB() <= 0 {
		t.Skip("нет /proc/self/status")
	}
}

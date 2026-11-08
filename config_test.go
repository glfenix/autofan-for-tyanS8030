package main

import (
	"os"
	"strings"
	"testing"
)

const sampleConf = `
# комментарий
POLL_INTERVAL=15
CRITICAL_TEMP=78
FAILSAFE_DUTY=90
ON_EXIT_DUTY=55
BMC_INTERFACE=open
BMC_DEVICE=/dev/ipmi0
SRC1_NAME=gpu
SRC1_TYPE=gpu
SRC1_VM=104
SRC1_OPTIONAL=1
SRC2_NAME=air_outlet
SRC2_TYPE=bmc
SRC2_SENSOR=SYS_Air_Outlet
SRC3_NAME=dimm
SRC3_TYPE=bmc
SRC3_SENSORS=P0_UMC0_CH_A0 P0_UMC7_CH_F0
SRC3_AGG=max
G1_NAME=chassis
G1_PWM=2,3,4,5
G1_TACH=SYS_FAN_1,SYS_FAN_2
G1_SOURCES=gpu air_outlet dimm
G1_MIN_DUTY=40
G1_MAX_DUTY=100
G1_CURVE=40:40 60:62 85:100
G2_NAME=cpu
G2_PWM=0
G2_SOURCES=air_outlet
G2_MIN_DUTY=45
G2_CURVE=45:45 85:100
`

func TestLoadVarsConfig(t *testing.T) {
	path := t.TempDir() + "/autofan.conf"
	if err := os.WriteFile(path, []byte(sampleConf), 0o600); err != nil {
		t.Fatal(err)
	}
	os.Setenv("AUTOFAN_DRYRUN", "1")
	defer os.Unsetenv("AUTOFAN_DRYRUN")

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PollInterval != 15 || cfg.CriticalTemp != 78 || cfg.Failsafe.Duty != 90 {
		t.Errorf("global scalars wrong: %+v", cfg)
	}
	if cfg.OnExit == nil || *cfg.OnExit.Duty != 55 {
		t.Errorf("on_exit = %v", cfg.OnExit)
	}
	if len(cfg.Groups) != 2 {
		t.Fatalf("groups = %d, want 2", len(cfg.Groups))
	}
	g1, g2 := cfg.Groups[0], cfg.Groups[1]
	if g1.Name != "chassis" || len(g1.PWMIDs) != 4 || g1.PWMIDs[3] != 5 {
		t.Errorf("group1 = %+v", g1)
	}
	if len(g1.Tach) != 2 {
		t.Errorf("tach = %v", g1.Tach)
	}
	if len(g1.Curve.Points) != 3 || g1.Curve.Points[1].Duty != 62 {
		t.Errorf("curve = %+v", g1.Curve.Points)
	}
	if len(g2.PWMIDs) != 1 || g2.MinDuty != 45 {
		t.Errorf("group2 = %+v", g2)
	}
	if len(cfg.Sources) != 3 {
		t.Fatalf("sources = %d, want 3", len(cfg.Sources))
	}
	if cfg.Sources[0].VM != 104 || !cfg.Sources[0].Optional {
		t.Errorf("gpu source = %+v", cfg.Sources[0])
	}
	// список датчиков через пробел
	if len(cfg.Sources[2].Sensors) != 2 {
		t.Errorf("dimm sensors = %v", cfg.Sources[2].Sensors)
	}
	// нормализация: общие значения подставляются в группы
	if g2.FailsafeDuty != 90 || g2.CriticalTemp != 78 || g2.Dwell == 0 {
		t.Errorf("group2 defaults = %+v", g2)
	}
}

func TestGroupReferencesUnknownSource(t *testing.T) {
	path := t.TempDir() + "/bad.conf"
	os.WriteFile(path, []byte("SRC1_NAME=a\nSRC1_TYPE=bmc\nSRC1_SENSOR=X\nG1_NAME=g\nG1_PWM=2\nG1_SOURCES=does_not_exist\nG1_CURVE=40:40\n"), 0o600)
	os.Setenv("AUTOFAN_DRYRUN", "1")
	defer os.Unsetenv("AUTOFAN_DRYRUN")
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("expected error for unknown source reference")
	}
}

func TestRejectsDutyOutsideCurve(t *testing.T) {
	path := t.TempDir() + "/bad2.conf"
	os.WriteFile(path, []byte("SRC1_NAME=a\nSRC1_TYPE=bmc\nSRC1_SENSOR=X\nG1_NAME=g\nG1_PWM=2\nG1_SOURCES=a\nG1_MIN_DUTY=50\nG1_CURVE=40:20\n"), 0o600)
	os.Setenv("AUTOFAN_DRYRUN", "1")
	defer os.Unsetenv("AUTOFAN_DRYRUN")
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("curve duty below min_duty must be rejected")
	}
}

func TestBadLineIsReported(t *testing.T) {
	_, err := parseVars([]byte("POLL_INTERVAL=10\nэто не переменная\n"))
	if err == nil || !strings.Contains(err.Error(), "строка 2") {
		t.Fatalf("expected error about line 2, got %v", err)
	}
}

// Живой конфиг пишется по-shell-ному: значение + хвостовой комментарий.
func TestInlineCommentsAndQuotes(t *testing.T) {
	m, err := parseVars([]byte(
		"POLL_INTERVAL=25              # секунд между циклами\n" +
			"ON_EXIT_DUTY=45   # возврат при остановке\n" +
			"G1_CURVE=40:40 55:45 85:100  # кривая\n" +
			"G1_NAME=\"chassis\" # в кавычках\n" +
			"STATE_FILE= # пустое значение с комментарием\n"))
	if err != nil {
		t.Fatal(err)
	}
	if m["POLL_INTERVAL"] != "25" || m["ON_EXIT_DUTY"] != "45" {
		t.Errorf("inline comment not stripped: %v", m)
	}
	if m["G1_CURVE"] != "40:40 55:45 85:100" {
		t.Errorf("curve value = %q", m["G1_CURVE"])
	}
	if m["G1_NAME"] != "chassis" {
		t.Errorf("quoted value = %q", m["G1_NAME"])
	}
	if m["STATE_FILE"] != "" {
		t.Errorf("empty value = %q", m["STATE_FILE"])
	}
}

func TestCurveStep(t *testing.T) {
	c := CurveCfg{Mode: "step", Points: []CurvePoint{
		{Temp: 40, Duty: 40}, {Temp: 50, Duty: 50}, {Temp: 60, Duty: 62}, {Temp: 86, Duty: 100},
	}}
	tests := []struct {
		temp float64
		want int
	}{
		{-10, 40}, {40, 40}, {45, 40}, {49.9, 40}, {50, 50}, {59, 50},
		{60, 62}, {85, 62}, {86, 100}, {200, 100},
	}
	for _, tt := range tests {
		if got := curveValue(c, tt.temp, 35, 100); got != tt.want {
			t.Errorf("curveValue(%v) = %d, want %d", tt.temp, got, tt.want)
		}
	}
}

func TestCurveLinear(t *testing.T) {
	c := CurveCfg{Mode: "linear", Points: []CurvePoint{{Temp: 40, Duty: 40}, {Temp: 80, Duty: 90}}}
	if got := curveValue(c, 60, 35, 100); got != 65 {
		t.Errorf("midpoint = %d, want 65", got)
	}
	if got := curveValue(c, 100, 35, 100); got != 90 {
		t.Errorf("above range = %d, want 90", got)
	}
}

// Апстрим-баг, который мы заменили: отказ NVML давал 0 C и тем самым
// *минимальный* duty. Здесь нехватка данных уводит на failsafe.
func TestUnsortedCurveIsSorted(t *testing.T) {
	cfg := &Config{Groups: []GroupCfg{{
		Name: "g", PWMIDs: []int{2}, Sources: []string{"a"},
		Curve: CurveCfg{Points: []CurvePoint{{Temp: 80, Duty: 90}, {Temp: 40, Duty: 40}}},
	}}}
	cfg.normalize()
	if cfg.Groups[0].Curve.Points[0].Temp != 40 {
		t.Fatalf("curve not sorted: %+v", cfg.Groups[0].Curve.Points)
	}
}

// Ключ SENSOR должен попадать в строковое поле Sensor (одиночный датчик или
// команда gpu-источника), а не только в список Sensors.
func TestSingleSensorGoesToSensorField(t *testing.T) {
	cfg, err := fromVars([]byte("SOURCE1_NAME=gpu\n" +
		"SOURCE1_TYPE=gpu\n" +
		"SOURCE1_SENSOR=/usr/local/bin/fakegpu.sh\n" +
		"SOURCE2_NAME=outlet\n" +
		"SOURCE2_TYPE=bmc\n" +
		"SOURCE2_SENSOR=SYS_Air_Outlet\n" +
		"SOURCE3_NAME=mosfets\n" +
		"SOURCE3_TYPE=bmc\n" +
		"SOURCE3_SENSORS=CPU_MOSFET CPU_SOC_MOSFET\n" +
		"G1_NAME=chassis\nG1_PWM=2\nG1_SOURCES=gpu outlet mosfets\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Sources[0].Sensor; got != "/usr/local/bin/fakegpu.sh" {
		t.Errorf("gpu sensor = %q, want путь к команде", got)
	}
	if cfg.Sources[1].Sensor != "SYS_Air_Outlet" || len(cfg.Sources[1].Sensors) != 0 {
		t.Errorf("outlet: Sensor=%q Sensors=%v - одиночный датчик не должен дублироваться",
			cfg.Sources[1].Sensor, cfg.Sources[1].Sensors)
	}
	if len(cfg.Sources[2].Sensors) != 2 || cfg.Sources[2].Sensor != "" {
		t.Errorf("mosfets: Sensor=%q Sensors=%v", cfg.Sources[2].Sensor, cfg.Sources[2].Sensors)
	}
}

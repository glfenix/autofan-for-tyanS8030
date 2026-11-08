package main

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"
)

// fakeSource позволяет проверить логику контура без железа и без BMC.
type fakeSource struct {
	name  string
	temp  float64
	power float64
	skip  bool
	err   error
}

func (f *fakeSource) Name() string { return f.name }

func (f *fakeSource) Read(context.Context) Reading {
	if f.err != nil {
		return Reading{Name: f.name, Absent: true, Err: f.err}
	}
	return Reading{Name: f.name, Temp: f.temp, Power: f.power, Skip: f.skip}
}

func newTestController(t *testing.T, srcs []Source, last int) (*Controller, *GroupState) {
	t.Helper()
	cfg := DefaultConfig()
	cfg.VerifyRPM = false // не спать 15 секунд на каждый тест
	cfg.State.Path = t.TempDir() + "/state.json"
	cfg.Groups = []GroupCfg{{
		Name: "chassis", Profile: "tyan64", PWMIDs: []int{2, 3, 4, 5},
		Sources: []string{"s"}, Agg: "max",
		MinDuty: 35, MaxDuty: 100, Step: 5, Dwell: 120,
		FailsafeDuty: 85, StartupDuty: 35, CriticalTemp: 80,
		Curve: CurveCfg{Mode: "step", Points: []CurvePoint{
			{Temp: 45, Duty: 35}, {Temp: 50, Duty: 45}, {Temp: 60, Duty: 55},
			{Temp: 70, Duty: 70}, {Temp: 78, Duty: 85}, {Temp: 85, Duty: 100},
		}},
	}}
	cfg.normalize()
	bmc := NewBMC(cfg.BMC, cfg.BMC.Profile)
	bmc.SetDry(true)
	g := &GroupState{cfg: &cfg.Groups[0], lastDuty: last}
	for _, s := range srcs {
		g.srcs = append(g.srcs, s)
	}
	return &Controller{cfg: cfg, bmc: bmc, heat: []*GroupState{g}}, g
}

// Лестница из конфига: 45C=35%, 50C=45%, ... 85C=100%.
func TestStepCurvePoints(t *testing.T) {
	for _, tt := range []struct {
		temp float64
		want int
	}{
		{40, 35}, {47, 35}, {49.9, 35}, {50, 45}, {55, 45}, {60, 55}, {69, 55},
		{70, 70}, {78, 85}, {85, 100}, {95, 100},
	} {
		c, g := newTestController(t, []Source{&fakeSource{name: "s", temp: tt.temp}}, 0)
		if _, err := c.Step(context.Background()); err != nil {
			t.Fatal(err)
		}
		if g.lastDuty != tt.want {
			t.Errorf("temp %.1f -> duty %d%%, want %d%%", tt.temp, g.lastDuty, tt.want)
		}
	}
}

// Порог учёта (FLOOR): холодный датчик не должен поднимать duty.
func TestFloorSkipsColdSensor(t *testing.T) {
	hot := &fakeSource{name: "cpu", temp: 47}
	dimm := &fakeSource{name: "dimm", temp: 55, skip: true} // FLOOR=60
	c, g := newTestController(t, []Source{hot, dimm}, 35)
	if _, err := c.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if g.lastDuty != 35 {
		t.Fatalf("duty = %d%%, want 35%% (DIMM 55C ниже порога учёта 60C)", g.lastDuty)
	}
}

// Если все источники живы, но холодные - это не авария, а минимум.
func TestAllColdIsNotFailsafe(t *testing.T) {
	c, g := newTestController(t, []Source{
		&fakeSource{name: "a", temp: 40, skip: true},
		&fakeSource{name: "b", temp: 30, skip: true},
	}, 70)
	if _, err := c.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if g.lastDuty != 35 {
		t.Fatalf("duty = %d%%, want 35%% (минимум, а не failsafe 85%%)", g.lastDuty)
	}
}

// Главный урок апстрима: отсутствие данных не имеет права опускать вентиляторы.
func TestNoDataGoesToFailsafe(t *testing.T) {
	c, g := newTestController(t, []Source{&fakeSource{name: "s", err: errors.New("ipmitool dead")}}, 35)
	if _, err := c.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if g.lastDuty != 85 {
		t.Fatalf("duty = %d%%, want 85%% failsafe", g.lastDuty)
	}
}

func TestNoDataNeverLowers(t *testing.T) {
	c, g := newTestController(t, []Source{&fakeSource{name: "s", err: errors.New("dead")}}, 100)
	if _, err := c.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if g.lastDuty != 100 {
		t.Fatalf("duty = %d%%, want 100%% (только вверх)", g.lastDuty)
	}
}

// Вверх контур реагирует сразу, вниз - только после DWELL.
func TestRaiseImmediateLowerAfterDwell(t *testing.T) {
	c, g := newTestController(t, []Source{&fakeSource{name: "s", temp: 75}}, 35)
	if _, err := c.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if g.lastDuty != 70 {
		t.Fatalf("повышение задержано: %d%%, want 70%%", g.lastDuty)
	}
	// остываем: тот же канал должен остаться на 70%%, пока не прошла выдержка
	hot := c.heat[0].srcs[0].(*fakeSource)
	hot.temp = 40
	if _, err := c.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if g.lastDuty != 70 {
		t.Fatalf("понижение раньше выдержки: %d%%, want 70%%", g.lastDuty)
	}
	g.lastChange = time.Now().Add(-2 * time.Minute)
	if _, err := c.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if g.lastDuty != 35 {
		t.Fatalf("после выдержки duty = %d%%, want 35%%", g.lastDuty)
	}
}

func TestStartupAppliesStartupDuty(t *testing.T) {
	c, g := newTestController(t, []Source{&fakeSource{name: "s", temp: 40}}, 0)
	g.cfg.StartupDuty = 35
	c.Startup(context.Background())
	if g.lastDuty != 35 {
		t.Fatalf("startup duty = %d%%, want 35%%", g.lastDuty)
	}
}

// Упреждение по мощности GPU: карта с холодным ядром, но горячая по ваттам.
func TestPowerLeadRaisesDutyDespiteColdSensors(t *testing.T) {
	c, g := newTestController(t, []Source{&fakeSource{name: "gpu", temp: 40, power: 230}}, 35)
	g.cfg.PowerCurve = CurveCfg{Mode: "step", Points: []CurvePoint{
		{Temp: 80, Duty: 40}, {Temp: 150, Duty: 50}, {Temp: 220, Duty: 60}, {Temp: 300, Duty: 75},
	}}
	if _, err := c.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if g.lastDuty != 60 {
		t.Fatalf("duty = %d%%, want 60%% (230 Вт -> 60%%), хотя температуры дают 35%%", g.lastDuty)
	}
}

// Ниже первого пункта кривой мощности механизм не вмешивается.
func TestPowerLeadIgnoredBelowFirstPoint(t *testing.T) {
	c, g := newTestController(t, []Source{&fakeSource{name: "gpu", temp: 40, power: 60}}, 35)
	g.cfg.PowerCurve = CurveCfg{Mode: "step", Points: []CurvePoint{{Temp: 80, Duty: 40}, {Temp: 300, Duty: 75}}}
	if _, err := c.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if g.lastDuty != 35 {
		t.Fatalf("duty = %d%%, want 35%%", g.lastDuty)
	}
}

// Температура всё-таки важнее: 85C это 100%%, даже если ватты просят меньше.
func TestTemperatureBeatsPowerLead(t *testing.T) {
	c, g := newTestController(t, []Source{&fakeSource{name: "gpu", temp: 85, power: 100}}, 35)
	g.cfg.PowerCurve = CurveCfg{Mode: "step", Points: []CurvePoint{{Temp: 80, Duty: 40}}}
	if _, err := c.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if g.lastDuty != 100 {
		t.Fatalf("duty = %d%%, want 100%%", g.lastDuty)
	}
}

// Критический порог перекрывает кривую и ускоряет опрос.
func TestCriticalForcesMaxAndFastPoll(t *testing.T) {
	c, g := newTestController(t, []Source{&fakeSource{name: "s", temp: 81}}, 45)
	next, err := c.Step(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if g.lastDuty != 100 {
		t.Fatalf("duty = %d%%, want 100%%", g.lastDuty)
	}
	if next != time.Duration(c.cfg.FastPoll)*time.Second {
		t.Fatalf("interval = %v, want fast poll %ds", next, c.cfg.FastPoll)
	}
	if !c.HotCycle {
		t.Fatal("HotCycle not set")
	}
}

// Нули от сломанного источника - это не "холодно", иначе мёртвый датчик
// утащил бы duty к минимуму.
func TestZeroTempCountsAsNoData(t *testing.T) {
	c, g := newTestController(t, []Source{
		&fakeSource{name: "a", temp: 0},
		&fakeSource{name: "b", temp: 0},
	}, 35)
	if _, err := c.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if g.lastDuty != 85 {
		t.Fatalf("duty = %d%%, want 85%% (failsafe), а не 35%%", g.lastDuty)
	}
}

// hwmon без файлов обязан вернуть ошибку, а не 0 градусов.
func TestHwmonMissingPathReturnsError(t *testing.T) {
	s := &HwmonSource{cfg: SourceCfg{Name: "ghost", Path: "/var/tmp/tut-net-файлов/temp*_input"}}
	r := s.Read(context.Background())
	if r.Err == nil {
		t.Fatalf("ожидались %v, а вернулись %v (молчаливые нули опасны)", "ошибка", r)
	}
}

// Стартовое значение не имеет права опускать вентиляторы, которые остались
// после горячего цикла (рестарт сервиса посреди нагрузки).
func TestStartupKeepsHotDuty(t *testing.T) {
	c, g := newTestController(t, []Source{&fakeSource{name: "s", temp: 40}}, 85)
	c.Startup(context.Background())
	if g.lastDuty != 85 {
		t.Fatalf("duty = %d%%, want 85%%: стартовое значение не опускает горячий контур", g.lastDuty)
	}
	// но и не держит их высоко вечно: первый же холодный цикл снижает,
	// выдержка из прошлого запуска этому не мешает
	if _, err := c.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if g.lastDuty != 35 {
		t.Fatalf("duty = %d%%, want 35%% после холодного замера", g.lastDuty)
	}
}

// Калибровка тахометра должна соответствовать реальным замерам, иначе проверка
// результата либо молчит, либо орёт на каждом цикле.
func TestRPMCalibrationMatchesHardware(t *testing.T) {
	d := DefaultConfig()
	measured := map[int]float64{35: 828, 45: 966, 55: 1104, 60: 1242, 75: 1518, 85: 1656}
	for duty, want := range measured {
		got := d.RPMOffset + d.RPMPerDuty*float64(duty)
		if diff := math.Abs(got-want) / want; diff > 0.10 {
			t.Errorf("duty %d%%: формула даёт %.0f RPM, замерено %.0f RPM (разница %.0f%%)",
				duty, got, want, diff*100)
		}
	}
}

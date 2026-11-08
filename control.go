package main

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// curveValue maps a temperature to a duty cycle.
// mode "step"   -> duty of the highest point at or below the temperature
// mode "linear" -> interpolated duty between the bracketing points
func curveValue(curve CurveCfg, t float64, minDuty, maxDuty int) int {
	pts := curve.Points
	if len(pts) == 0 {
		return maxDuty
	}
	if t <= float64(pts[0].Temp) {
		return clamp(pts[0].Duty, minDuty, maxDuty)
	}
	last := pts[len(pts)-1]
	if t >= float64(last.Temp) {
		return clamp(last.Duty, minDuty, maxDuty)
	}
	for i := 0; i < len(pts)-1; i++ {
		a, b := pts[i], pts[i+1]
		if t >= float64(a.Temp) && t < float64(b.Temp) {
			if curve.Mode == "linear" {
				f := (t - float64(a.Temp)) / float64(b.Temp-a.Temp)
				return clamp(int(math.Round(float64(a.Duty)+f*float64(b.Duty-a.Duty))), minDuty, maxDuty)
			}
			return clamp(a.Duty, minDuty, maxDuty)
		}
	}
	return clamp(last.Duty, minDuty, maxDuty)
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// GroupState is the control state of one fan group.
type GroupState struct {
	cfg        *GroupCfg
	srcs       []Source
	lastDuty   int
	lastChange time.Time
	errStreak  int
}

// Controller drives every configured fan group.
type Controller struct {
	cfg  *Config
	bmc  *BMC
	heat []*GroupState
	// HotCycle is set when some group reached its critical temperature, which
	// makes the main loop poll faster on the next round.
	HotCycle bool
}

func NewController(cfg *Config, bmc *BMC) (*Controller, error) {
	all := map[string]Source{}
	for _, s := range NewSources(cfg, bmc) {
		all[s.Name()] = s
	}
	st, err := LoadState(cfg.State.Path)
	if err != nil {
		logf("WARN state: %v", err)
	}
	c := &Controller{cfg: cfg, bmc: bmc}
	for i := range cfg.Groups {
		g := &cfg.Groups[i]
		gs := &GroupState{cfg: g}
		for _, name := range g.Sources {
			s, ok := all[name]
			if !ok {
				return nil, fmt.Errorf("group %s: source %q not defined", g.Name, name)
			}
			gs.srcs = append(gs.srcs, s)
		}
		if st != nil {
			if d, ok := st.Duties[g.Name]; ok {
				gs.lastDuty = d
			} else if st.LegacyDuty > 0 {
				gs.lastDuty = st.LegacyDuty
			}
		}
		c.heat = append(c.heat, gs)
	}
	if st != nil && len(st.Duties) > 0 {
		logf("state: %s", describeDuties(c.heat))
	}
	return c, nil
}

func describeDuties(gs []*GroupState) string {
	var p []string
	for _, g := range gs {
		p = append(p, fmt.Sprintf("%s=%d%%", g.cfg.Name, g.lastDuty))
	}
	return "restored " + strings.Join(p, " ")
}

// leadDuty — упреждение по мощности GPU. Ниже первого пункта кривой механизм
// не вмешивается (возвращает 0), дальше работает как обычная ступенчатая
// кривая, только вместо градусов ватты.
func leadDuty(curve CurveCfg, watts float64, minDuty, maxDuty int) int {
	if len(curve.Points) == 0 || watts < float64(curve.Points[0].Temp) {
		return 0
	}
	return curveValue(curve, watts, minDuty, maxDuty)
}

// Startup выставляет группам стартовое значение (STARTUP_DUTY). Выполняется
// сразу после запуска сервиса, чтобы после загрузки ОС вентиляторы имели
// заведомо известную скорость, а не то, что осталось от прошлого запуска или
// от кривой BMC. Проверка тахометров заодно подтверждает, что вентиляторы
// вообще крутятся.
//
// Стартовое значение НИКОГДА не опускает duty: если прошлый цикл ушёл высоко
// (упал посреди жаркой нагрузки или сервис рестартовали), крутить медленнее
// до первого замера температур нельзя. А после холодного старта история
// выдержки обнуляется, иначе «горячее» состояние из state.json продержало бы
// вентиляторы высоко ещё две минуты.
func (c *Controller) Startup(ctx context.Context) {
	for _, g := range c.heat {
		want := g.cfg.StartupDuty
		why := "startup_duty"
		if g.lastDuty > want {
			want, why = g.lastDuty, "startup_duty не опускает (было горячо)"
		}
		if g.lastDuty == want {
			vlogf("INFO %s: уже на %d%%", g.cfg.Name, want)
			continue
		}
		if err := c.apply(ctx, g, want, why); err != nil {
			logf("ERROR %s: стартовое значение не применено: %v", g.cfg.Name, err)
		}
	}
	for _, g := range c.heat {
		g.lastChange = time.Time{}
	}
}

// consider применяет гистерезис: вверх сразу, вниз только крупным шагом и не
// чаще чем раз в DWELL секунд.
func (c *Controller) consider(ctx context.Context, g *GroupState, target int, why string) error {
	if target == g.lastDuty {
		vlogf("%s: %s duty=%d%% (без изменений)", g.cfg.Name, why, g.lastDuty)
		return nil
	}
	if target < g.lastDuty && g.lastDuty != 0 {
		if target+g.cfg.Step > g.lastDuty {
			vlogf("%s: %s -> хочу %d%%, оставляю %d%% (изменение меньше шага %d)",
				g.cfg.Name, why, target, g.lastDuty, g.cfg.Step)
			return nil
		}
		if d := time.Since(g.lastChange); d < time.Duration(g.cfg.Dwell)*time.Second {
			vlogf("%s: %s -> хочу %d%%, оставляю %d%% (выдержка %dс, осталось %dс)",
				g.cfg.Name, why, target, g.lastDuty, g.cfg.Dwell,
				int(time.Duration(g.cfg.Dwell)*time.Second-d)/int(time.Second))
			return nil
		}
	}
	return c.apply(ctx, g, target, why)
}

// Step runs one control cycle for every group. It returns the interval the
// caller should wait before the next cycle.
func (c *Controller) Step(ctx context.Context) (time.Duration, error) {
	c.bmc.ResetSDR()
	// one source is read at most once per cycle even if several groups use it
	cycle := map[string]Reading{}
	next := time.Duration(c.cfg.PollInterval) * time.Second
	c.HotCycle = false
	var lastErr error

	for _, g := range c.heat {
		var temps []float64
		cold, failed := 0, 0
		var watts float64
		for _, s := range g.srcs {
			r, ok := cycle[s.Name()]
			if !ok {
				r = s.Read(ctx)
				cycle[s.Name()] = r
			}
			// мощность интересна и у "холодного" источника: карта с холодным
			// ядром может выгружать в корпус 300 Вт
			if r.Power > watts {
				watts = r.Power
			}
			switch {
			case r.Err != nil:
				failed++
				if r.Absent {
					vlogf("INFO %s: источник %s без данных: %v", g.cfg.Name, r.Name, r.Err)
				} else {
					logf("WARN %s: источник %s: %v", g.cfg.Name, r.Name, r.Err)
				}
			case r.Temp <= 0:
				// 0 градусов в серверной не бывает: это сломанный источник,
				// а не «холодно». Опасно считать его валидным - он зальёт минимум.
				failed++
				logf("WARN %s: источник %s вернул %.1fC - не считываем это температурой",
					g.cfg.Name, r.Name, r.Temp)
			case r.Skip:
				cold++
				vlogf("INFO %s: %s = %.1fC ниже порога учёта, в расчёт не идёт",
					g.cfg.Name, r.Name, r.Temp)
			default:
				temps = append(temps, r.Temp)
			}
		}

		if len(temps) == 0 {
			// Вариант 1: источники живы, но ниже своих порогов - машина холодная.
			// Это не авария, а повод опуститься к минимуму.
			if cold > 0 && failed < len(g.srcs) {
				g.errStreak = 0
				if err := c.consider(ctx, g, g.cfg.MinDuty, "все источники холодные"); err != nil {
					lastErr = err
				}
				continue
			}
			// Вариант 2: данных нет вообще. Тут нельзя опускать вентиляторы.
			g.errStreak++
			if g.lastDuty < g.cfg.FailsafeDuty {
				logf("ERROR %s: нет данных от источников (%d циклов подряд) -> FAILSAFE %d%%",
					g.cfg.Name, g.errStreak, g.cfg.FailsafeDuty)
				if err := c.apply(ctx, g, g.cfg.FailsafeDuty, "failsafe: нет данных"); err != nil {
					lastErr = err
				}
			} else {
				logf("ERROR %s: нет данных от источников (%d циклов подряд), держу %d%%",
					g.cfg.Name, g.errStreak, g.lastDuty)
			}
			next = time.Duration(c.cfg.FastPoll) * time.Second
			continue
		}
		g.errStreak = 0

		t := agg(temps, g.cfg.Agg)
		target := curveValue(g.cfg.Curve, t, g.cfg.MinDuty, g.cfg.MaxDuty)
		why := fmt.Sprintf("temp=%.1fC (%d источников, ещё %d холодных, %d недоступных)",
			t, len(temps), cold, failed)
		if watts > 0 {
			why = fmt.Sprintf("temp=%.1fC gpu=%.0fВт (%d источников, %d холодных, %d недоступных)",
				t, watts, len(temps), cold, failed)
		}

		// упреждение по мощности: карта ещё холодная, а в корпус уже летят
		// сотни ватт, и этот воздух идёт прямо на зону VRM/DIMM
		if lead := leadDuty(g.cfg.PowerCurve, watts, g.cfg.MinDuty, g.cfg.MaxDuty); lead > target {
			target = lead
			why = fmt.Sprintf("gpu=%.0fВт -> упреждение %d%% (температуры ещё %d%%)",
				watts, lead, curveValue(g.cfg.Curve, t, g.cfg.MinDuty, g.cfg.MaxDuty))
		}

		if t >= g.cfg.CriticalTemp {
			c.HotCycle = true
			next = time.Duration(c.cfg.FastPoll) * time.Second
			if target < g.cfg.MaxDuty {
				target = g.cfg.MaxDuty
				why = fmt.Sprintf("temp=%.1fC >= критический %.0fC", t, g.cfg.CriticalTemp)
			}
		}

		if err := c.consider(ctx, g, target, why); err != nil {
			lastErr = err
		}
	}

	if err := SaveState(c.cfg.State.Path, c.heat); err != nil {
		logf("WARN state: %v", err)
	}
	return next, lastErr
}

func (c *Controller) apply(ctx context.Context, g *GroupState, duty int, why string) error {
	var failed []int
	for _, id := range g.cfg.PWMIDs {
		if err := c.bmc.SetDutyProfile(ctx, g.cfg.Profile, id, duty); err != nil {
			logf("WARN %s: pwm%d -> %d%%: %v", g.cfg.Name, id, duty, err)
			if err2 := c.bmc.SetDutyProfile(ctx, g.cfg.Profile, id, duty); err2 == nil {
				vlogf("INFO %s: pwm%d повтор удался", g.cfg.Name, id)
				continue
			}
			failed = append(failed, id)
		}
	}
	if len(failed) == len(g.cfg.PWMIDs) {
		// ничего не дошло до BMC: не притворяемся, что применили
		return fmt.Errorf("%s: duty %d%% НЕ применён (все каналы %v)", g.cfg.Name, duty, failed)
	}
	if len(failed) > 0 {
		logf("ERROR %s: duty %d%% применён частично, отказали pwm%v", g.cfg.Name, duty, failed)
	}

	before := g.lastDuty
	g.lastDuty = duty
	g.lastChange = time.Now()
	logf("APPLY %s: %d%% -> pwm%v (%s, было %d%%)", g.cfg.Name, duty, g.cfg.PWMIDs, why, before)

	if c.cfg.VerifyRPM && len(g.cfg.Tach) > 0 {
		c.verify(ctx, g, duty)
	}
	return nil
}

// verify compares the tachometers against the calibration curve. A fan that
// does not speed up when duty rises is the classic early warning for a dead
// bearing, a broken tachometer line or a fan plugged into the wrong header.
func (c *Controller) verify(ctx context.Context, g *GroupState, duty int) {
	time.Sleep(time.Duration(c.cfg.VerifyDelaySec) * time.Second)
	sdr, err := c.bmc.SDR(ctx)
	if err != nil {
		logf("WARN %s: verify: %v", g.cfg.Name, err)
		return
	}
	// калибровка: RPM = RPMOffset + RPMPerDuty*duty (наши 4 вентилятора:
	// 35%->828, 45%->966, 55%->1104, 60%->1242, 75%->1518, 85%->1656)
	expect := c.cfg.RPMOffset + g.cfg.RPMPerDuty*float64(duty)
	names := make([]string, 0, len(g.cfg.Tach))
	byName := map[string]int{} // sensor -> pwm index
	for i, sn := range g.cfg.Tach {
		if sn == "" || sn == "-" {
			continue
		}
		names = append(names, sn)
		byName[sn] = i
	}
	sort.Strings(names)
	var parts []string
	for _, sn := range names {
		v, ok := sdr[sn]
		if !ok {
			parts = append(parts, sn+"=НЕТ")
			continue
		}
		parts = append(parts, fmt.Sprintf("%s=%.0f", sn, v))
		id := g.cfg.PWMIDs[byName[sn]]
		if expect > 0 && math.Abs(v-expect) > c.cfg.RPMWarnFactor*expect {
			logf("WARN %s: %s (pwm%d) = %.0f RPM, ожидалось ~%.0f при %d%%",
				g.cfg.Name, sn, id, v, expect, duty)
		}
	}
	logf("INFO %s: тахометры @%d%%: %s", g.cfg.Name, duty, strings.Join(parts, " "))
}

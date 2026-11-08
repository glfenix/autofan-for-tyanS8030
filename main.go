package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

const version = "0.5.0"

func main() {
	cfgPath := flag.String("c", "/etc/autofan/autofan.conf", "путь к конфигу (KEY=VALUE или .yaml)")
	dryRun := flag.Bool("dry-run", false, "только показывать команды IPMI, ничего не писать в BMC")
	once := flag.Bool("once", false, "один цикл и выход")
	verbose := flag.Bool("v", false, "писать каждый цикл, даже если duty не меняется (флаги идут ДО команды)")
	showVersion := flag.Bool("version", false, "версия")
	flag.Parse()

	if *showVersion {
		fmt.Println("autofan", version)
		return
	}
	if *dryRun {
		os.Setenv("AUTOFAN_DRYRUN", "1")
	}

	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("autofan: ")
	flagParsedVerbose := *verbose

	cfg, err := LoadConfig(*cfgPath)
	if err != nil {
		log.Fatalf("конфиг: %v", err)
	}
	// флаг -v важнее, чем VERBOSE=0 в конфиге: иначе отладка невозможна
	if flagParsedVerbose {
		Verbose = true
	}

	bmc := NewBMC(cfg.BMC, cfg.BMC.Profile)
	bmc.SetDry(*dryRun)

	args := flag.Args()
	cmd := "run"
	if len(args) > 0 {
		cmd = args[0]
	}
	if *once {
		cmd = "once"
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)

	switch cmd {
	case "run":
		runLoop(ctx, hup, *cfgPath, cfg, *dryRun)
	case "once":
		c, err := NewController(cfg, bmc)
		if err != nil {
			log.Fatalf("конфиг: %v", err)
		}
		if _, err := c.Step(ctx); err != nil {
			log.Printf("цикл: %v", err)
		}
	case "probe":
		probe(ctx, cfg, bmc)
	case "status":
		status(ctx, cfg, bmc)
	case "set":
		if len(args) < 3 {
			log.Fatalf("использование: autofan set <pwm_id> <duty 0-100>")
		}
		pwm, _ := strconv.Atoi(args[1])
		duty, _ := strconv.Atoi(args[2])
		if err := bmc.SetDuty(ctx, pwm, duty); err != nil {
			log.Fatalf("set: %v", err)
		}
		log.Printf("pwm%d = %d%%", pwm, duty)
	case "restore":
		restoreAll(ctx, cfg, bmc)
	default:
		log.Fatalf("неизвестная команда %q (run|once|probe|status|set|restore)", cmd)
	}
}

func newEngine(cfg *Config, dryRun bool) (*BMC, *Controller, error) {
	bmc := NewBMC(cfg.BMC, cfg.BMC.Profile)
	bmc.SetDry(dryRun)
	c, err := NewController(cfg, bmc)
	return bmc, c, err
}

// wattString - то же, что curveString, только первая координата в ваттах.
func wattString(c CurveCfg) string {
	s := ""
	for _, p := range c.Points {
		if s != "" {
			s += " "
		}
		s += fmt.Sprintf("%dВт=%d%%", p.Temp, p.Duty)
	}
	return s
}

func intsToString(v []int) string {
	s := ""
	for _, n := range v {
		if s != "" {
			s += ","
		}
		s += strconv.Itoa(n)
	}
	return "[" + s + "]"
}

func describeGroups(cfg *Config) string {
	out := ""
	for _, g := range cfg.Groups {
		if out != "" {
			out += "; "
		}
		out += fmt.Sprintf("%s pwm%s %d..%d%% [%s]", g.Name, intsToString(g.PWMIDs), g.MinDuty, g.MaxDuty, curveString(g.Curve))
		if len(g.PowerCurve.Points) > 0 {
			out += " ватты[" + wattString(g.PowerCurve) + "]"
		}
	}
	return out
}

func curveString(c CurveCfg) string {
	out := ""
	for _, p := range c.Points {
		if out != "" {
			out += " "
		}
		out += fmt.Sprintf("%dC=%d%%", p.Temp, p.Duty)
	}
	return out
}

func runLoop(ctx context.Context, hup <-chan os.Signal, cfgPath string, cfg *Config, dryRun bool) {
	bmc, ctrl, err := newEngine(cfg, dryRun)
	if err != nil {
		log.Fatalf("конфиг: %v", err)
	}
	log.Printf("старт: bmc=%s профиль=%s опрос=%ds(горячий %ds) крит=%d.C отказ=%d%% старт=%d%% групп=%d [%s] dry-run=%v",
		bmcTarget(cfg), cfg.BMC.Profile, cfg.PollInterval, cfg.FastPoll, int(cfg.CriticalTemp),
		cfg.Failsafe.Duty, cfg.StartupDuty, len(cfg.Groups), describeGroups(cfg), dryRun)

	// сразу задаём стартовую скорость, чтобы после загрузки ОС вентиляторы
	// были в известном состоянии, и убеждаемся по тахометрам, что они крутятся
	ctrl.Startup(ctx)

	interval, err := ctrl.Step(ctx)
	if err != nil {
		log.Printf("цикл: %v", err)
	}
	timer := time.NewTimer(interval)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Printf("сигнал на остановку")
			shutdown(cfg, bmc)
			return
		case <-hup:
			log.Printf("SIGHUP: перечитываю %s", cfgPath)
			ncfg, err := LoadConfig(cfgPath)
			if err != nil {
				log.Printf("ERROR конфиг не перечитан, работаю по прежнему: %v", err)
				continue
			}
			nbmc, nctrl, err := newEngine(ncfg, dryRun)
			if err != nil {
				log.Printf("ERROR конфигурация не применена, работаю по прежнему: %v", err)
				continue
			}
			cfg, bmc, ctrl = ncfg, nbmc, nctrl
			log.Printf("конфиг перечитан: групп=%d [%s]", len(cfg.Groups), describeGroups(cfg))
			interval = time.Duration(cfg.PollInterval) * time.Second
			timer.Reset(interval)
		case <-timer.C:
			next, err := ctrl.Step(ctx)
			if err != nil {
				log.Printf("цикл: %v", err)
			}
			timer.Reset(next)
		}
	}
}

// shutdown returns every group to its configured safe duty. Note that this
// only runs on a clean SIGTERM: after SIGKILL the BMC simply keeps the last
// duty it was given, which is why min_duty must never be dangerously low.
func shutdown(cfg *Config, bmc *BMC) {
	if cfg.OnExit == nil || cfg.OnExit.Duty == nil {
		log.Printf("on_exit не задан - оставляю duty как есть")
		return
	}
	duty := *cfg.OnExit.Duty
	log.Printf("возврат на %d%% при остановке", duty)
	shutCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, g := range cfg.Groups {
		for _, id := range g.PWMIDs {
			if err := bmc.SetDutyProfile(shutCtx, g.Profile, id, duty); err != nil {
				log.Printf("ERROR %s pwm%d: %v", g.Name, id, err)
			}
		}
	}
	log.Printf("остановлен")
}

func restoreAll(ctx context.Context, cfg *Config, bmc *BMC) {
	if cfg.OnExit == nil || cfg.OnExit.Duty == nil {
		log.Fatalf("on_exit duty не задан")
	}
	for _, g := range cfg.Groups {
		for _, id := range g.PWMIDs {
			if err := bmc.SetDutyProfile(ctx, g.Profile, id, *cfg.OnExit.Duty); err != nil {
				log.Printf("%s pwm%d: %v", g.Name, id, err)
			} else {
				log.Printf("%s pwm%d = %d%%", g.Name, id, *cfg.OnExit.Duty)
			}
		}
	}
}

func bmcTarget(cfg *Config) string {
	if cfg.BMC.Interface == "open" {
		return cfg.BMC.Device
	}
	return fmt.Sprintf("%s/ipmi", cfg.BMC.Host)
}

func probe(ctx context.Context, cfg *Config, bmc *BMC) {
	fmt.Println("== конфигурация ==")
	fmt.Printf("  bmc      : %s (interface=%s, профиль по умолчанию=%s)\n", bmcTarget(cfg), cfg.BMC.Interface, cfg.BMC.Profile)
	fmt.Printf("  опрос    : %dс, горячий %dс, критический порог %.0f.C\n", cfg.PollInterval, cfg.FastPoll, cfg.CriticalTemp)
	fmt.Printf("  failsafe : %d%%   on_exit: %s   startup: %d%%\n", cfg.Failsafe.Duty, onExitString(cfg), cfg.StartupDuty)
	fmt.Printf("  проверка : ожидаемый RPM = %.0f + %.1f*duty, допуск %.0f%%, задержка %dс\n",
		cfg.RPMOffset, cfg.RPMPerDuty, cfg.RPMWarnFactor*100, cfg.VerifyDelaySec)
	fmt.Println("  группы:")
	for _, g := range cfg.Groups {
		fmt.Printf("    %-10s pwm%-12s duty %d..%d%% старт %d%% шаг %d выдержка %dс профиль %s\n",
			g.Name, intsToString(g.PWMIDs), g.MinDuty, g.MaxDuty, g.StartupDuty, g.Step, g.Dwell, g.Profile)
		fmt.Printf("               источники %v (агрегат %s, крит %.0f.C)\n", g.Sources, g.Agg, g.CriticalTemp)
		fmt.Printf("               кривая [%s] (%s)\n", curveString(g.Curve), g.Curve.Mode)
		if len(g.PowerCurve.Points) > 0 {
			fmt.Printf("               ватты  [%s] упреждение по мощности GPU\n", wattString(g.PowerCurve))
		}
		if len(g.Tach) > 0 {
			fmt.Printf("               тахометры %v\n", g.Tach)
		}
	}

	fmt.Println("\n== датчики BMC ==")
	sdr, err := bmc.SDR(ctx)
	if err != nil {
		fmt.Printf("  ОШИБКА %v\n", err)
	} else {
		for _, name := range sortedKeys(sdr) {
			fmt.Printf("  %-18s %.0f\n", name, sdr[name])
		}
	}

	fmt.Println("\n== температурные источники ==")
	for _, s := range NewSources(cfg, bmc) {
		r := s.Read(ctx)
		switch {
		case r.Err != nil:
			fmt.Printf("  %-18s %v\n", r.Name, r.Err)
		case r.Power > 0:
			fmt.Printf("  %-18s %.1f C  %.0f Вт\n", r.Name, r.Temp, r.Power)
		default:
			fmt.Printf("  %-18s %.1f C\n", r.Name, r.Temp)
		}
	}
}

func onExitString(cfg *Config) string {
	if cfg.OnExit == nil || cfg.OnExit.Duty == nil {
		return "не задан"
	}
	return fmt.Sprintf("%d%%", *cfg.OnExit.Duty)
}

func status(ctx context.Context, cfg *Config, bmc *BMC) {
	if st, err := LoadState(cfg.State.Path); err == nil && st != nil {
		fmt.Printf("состояние (%s): ", st.Updated.Format(time.RFC3339))
		if len(st.Duties) > 0 {
			for _, k := range sortedInts(st.Duties) {
				fmt.Printf("%s=%d%% ", k, st.Duties[k])
			}
		} else {
			fmt.Printf("duty=%d%% (старый формат)", st.LegacyDuty)
		}
		fmt.Println()
	}
	sdr, err := bmc.SDR(ctx)
	if err != nil {
		fmt.Printf("ОШИБКА %v\n", err)
		return
	}
	fmt.Println("тахометры:")
	for _, name := range sortedKeys(sdr) {
		if len(name) >= 3 && (name[:3] == "CPU" || name[:3] == "SYS") {
			fmt.Printf("  %-12s %.0f\n", name, sdr[name])
		}
	}
}

func sortedKeys(m map[string]float64) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

func sortedInts(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

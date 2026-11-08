package main

import (
	"bufio"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// ---------------------------------------------------------------------------
// Config model
//
// The daemon is driven by one external, human-editable file with plain
// variables (KEY=VALUE). YAML is supported as an alternative syntax, and any
// variable may also be overridden through the real environment, so
// systemd's EnvironmentFile= works too.
// ---------------------------------------------------------------------------

type Config struct {
	PollInterval   int         `yaml:"poll_interval"`      // секунд между циклами
	FastPoll       int         `yaml:"fast_poll_interval"` // цикл, пока что-то выше CriticalTemp
	CriticalTemp   float64     `yaml:"critical_temp"`      // общий порог "жарко"
	VerifyRPM      bool        `yaml:"verify_rpm"`
	VerifyDelaySec int         `yaml:"verify_delay_seconds"`
	RPMPerDuty     float64     `yaml:"rpm_per_duty"`
	RPMOffset      float64     `yaml:"rpm_offset"` // тахометр врёт нелинейно: RPM = смещение + наклон*duty
	RPMWarnFactor  float64     `yaml:"rpm_warn_factor"`
	Failsafe       FailsafeCfg `yaml:"failsafe"`
	OnExit         *OnExitCfg  `yaml:"on_exit"`
	State          StateCfg    `yaml:"state"`
	StartupDuty    int         `yaml:"startup_duty"` // при запуске сервиса, до первого решения
	BMC            BMCCfg      `yaml:"bmc"`
	Sources        []SourceCfg `yaml:"sources"`
	Groups         []GroupCfg  `yaml:"groups"`
}

type BMCCfg struct {
	Tool         string `yaml:"tool"`
	Interface    string `yaml:"interface"` // open | lanplus | lan
	Device       string `yaml:"device"`    // /dev/ipmi0
	Host         string `yaml:"host"`
	Username     string `yaml:"username"`
	PasswordFile string `yaml:"password_file"` // NOT a command line arg
	CipherSuite  int    `yaml:"cipher_suite"`
	Timeout      int    `yaml:"timeout"`
	Retries      int    `yaml:"retries"`
	Profile      string `yaml:"profile"` // tyan64 | tyan32, профиль по умолчанию
}

type GroupCfg struct {
	Name         string   `yaml:"name"`
	Profile      string   `yaml:"profile"` // tyan64 | tyan32 (пусто = общий)
	PWMIDs       []int    `yaml:"pwm_ids"`
	Sources      []string `yaml:"sources"` // имена из секции sources
	Agg          string   `yaml:"agg"`     // max | mean | min внутри группы
	Tach         []string `yaml:"tach"`    // тахометры для проверки результата
	MinDuty      int      `yaml:"min_duty"`
	MaxDuty      int      `yaml:"max_duty"`
	Step         int      `yaml:"step"`
	Dwell        int      `yaml:"dwell_seconds"`
	FailsafeDuty int      `yaml:"failsafe_duty"` // 0 = общий failsafe.duty
	StartupDuty  int      `yaml:"startup_duty"`  // 0 = общий startup_duty
	CriticalTemp float64  `yaml:"critical_temp"` // 0 = общий critical_temp
	RPMPerDuty   float64  `yaml:"rpm_per_duty"`  // 0 = общий
	// PowerCurve упреждающий нижний duty по мощности GPU (Вт вместо градусов).
	// Смысл: температура ядра карты почти не растёт (её охлаждение справляется),
	// а вот в корпус уходит 300+ Вт. Мощность = тепловыделение, поэтому она
	// поднимает вентиляторы раньше, чем до них догреют датчики корпуса.
	PowerCurve CurveCfg `yaml:"power_curve"`
	Curve      CurveCfg `yaml:"curve"`
}

type CurveCfg struct {
	Mode   string       `yaml:"mode"` // step | linear
	Points []CurvePoint `yaml:"points"`
}

type CurvePoint struct {
	Temp int `yaml:"temp"`
	Duty int `yaml:"duty"`
}

type SourceCfg struct {
	Name     string   `yaml:"name"`
	Type     string   `yaml:"type"` // gpu | bmc | hwmon
	Agg      string   `yaml:"agg"`
	Sensor   string   `yaml:"sensor"`
	Sensors  []string `yaml:"sensors"`
	Path     string   `yaml:"path"`
	VM       int      `yaml:"vm"`
	Optional bool     `yaml:"optional"`
	// Floor учитывать источник только выше этой температуры: датчики DIMM и
	// MOSFET остаются тёплыми (52-55C) даже в простое и иначе не дали бы
	// вентиляторам опуститься до стартового duty.
	Floor float64 `yaml:"floor"`
}

type FailsafeCfg struct {
	Duty int `yaml:"duty"`
}

type OnExitCfg struct {
	Duty *int `yaml:"duty"`
}

type StateCfg struct {
	Path string `yaml:"path"`
}

func DefaultConfig() *Config {
	return &Config{
		PollInterval: 20, FastPoll: 5, CriticalTemp: 80,
		VerifyRPM: true, VerifyDelaySec: 15, RPMPerDuty: 17.2, RPMOffset: 202, RPMWarnFactor: 0.30,
		Failsafe:    FailsafeCfg{Duty: 85},
		StartupDuty: 35,
		State:       StateCfg{Path: "/var/lib/autofan/state.json"},
		BMC:         BMCCfg{Tool: "ipmitool", Interface: "open", Device: "/dev/ipmi0", Timeout: 20, Retries: 2},
	}
}

// LoadConfig reads the external config file. Extension decides the syntax:
// .yaml/.yml -> YAML, anything else -> KEY=VALUE variables.
func LoadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg *Config
	if strings.HasSuffix(strings.ToLower(path), ".yaml") || strings.HasSuffix(strings.ToLower(path), ".yml") {
		cfg = DefaultConfig()
		if err := yaml.Unmarshal(raw, cfg); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	} else {
		cfg, err = fromVars(raw)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	}
	cfg.normalize()
	return cfg, cfg.validate()
}

// ---------------------------------------------------------------------------
// KEY=VALUE parser
//
//	# комментарий
//	POLL_INTERVAL=20
//	G1_NAME=chassis
//	G1_PWM=2,3,4,5
//	G1_CURVE=40:40 50:50 60:62 70:75 80:90 86:100
//
// Values may be quoted; `export` is tolerated so the file stays shell-sourceable.
// Real environment variables override the file (systemd EnvironmentFile=).
// ---------------------------------------------------------------------------

func parseVars(raw []byte) (map[string]string, error) {
	out := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(string(raw)))
	line := 0
	for sc.Scan() {
		line++
		s := strings.TrimSpace(sc.Text())
		if s == "" || strings.HasPrefix(s, "#") || strings.HasPrefix(s, ";") {
			continue
		}
		s = strings.TrimPrefix(s, "export ")
		k, v, ok := strings.Cut(s, "=")
		if !ok {
			return nil, fmt.Errorf("строка %d: ожидается КЛЮЧ=ЗНАЧЕНИЕ, получено %q", line, s)
		}
		k = strings.TrimSpace(k)
		if !validVarName(k) {
			return nil, fmt.Errorf("строка %d: некорректное имя переменной %q", line, k)
		}
		v = strings.TrimSpace(v)
		// значение может иметь хвостовой комментарий, как в shell:
		//   POLL_INTERVAL=25   # секунд между циклами
		if len(v) > 0 && (v[0] == '"' || v[0] == '\'') {
			q := v[0]
			end := strings.IndexByte(v[1:], q)
			if end < 0 {
				return nil, fmt.Errorf("строка %d: незакрытая кавычка в %q", line, s)
			}
			v = v[1 : 1+end]
		} else if i := strings.IndexByte(v, '#'); i >= 0 {
			v = strings.TrimSpace(v[:i])
		}
		out[k] = v
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	// environment wins, so EnvironmentFile= and systemctl set-property work
	for _, e := range os.Environ() {
		k, v, _ := strings.Cut(e, "=")
		if _, interesting := interestingKey(k); interesting && v != "" {
			out[k] = v
		}
	}
	return out, nil
}

func validVarName(k string) bool {
	if k == "" {
		return false
	}
	for i := 0; i < len(k); i++ {
		c := k[i]
		if !(c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_') {
			return false
		}
	}
	return true
}

// interestingKey reports whether an env variable belongs to autofan.
func interestingKey(k string) (string, bool) {
	prefixes := []string{"POLL_", "FAST_POLL", "CRITICAL_TEMP", "BMC_", "SOURCE_", "SRC", "G1_", "G2_", "G3_",
		"G4_", "G5_", "G6_", "FAILSAFE_", "ON_EXIT_", "STARTUP", "STATE_", "VERIFY_", "RPM_", "PROFILE", "VERBOSE", "AUTOFAN_"}
	for _, p := range prefixes {
		if strings.HasPrefix(k, p) {
			return p, true
		}
	}
	return "", false
}

func fromVars(raw []byte) (*Config, error) {
	m, err := parseVars(raw)
	if err != nil {
		return nil, err
	}
	cfg := DefaultConfig()

	get := func(k string) (string, bool) { v, ok := m[k]; return v, ok }
	str := func(k string, dst *string) {
		if v, ok := get(k); ok && v != "" {
			*dst = v
		}
	}
	num := func(k string, dst *int) error {
		if v, ok := get(k); ok && v != "" {
			n, err := strconv.Atoi(v)
			if err != nil {
				return fmt.Errorf("%s: %q не число", k, v)
			}
			*dst = n
		}
		return nil
	}
	flt := func(k string, dst *float64) error {
		if v, ok := get(k); ok && v != "" {
			f, err := strconv.ParseFloat(v, 64)
			if err != nil {
				return fmt.Errorf("%s: %q не число", k, v)
			}
			*dst = f
		}
		return nil
	}
	boolean := func(k string, dst *bool) {
		if v, ok := get(k); ok {
			*dst = v == "1" || strings.EqualFold(v, "true") || strings.EqualFold(v, "yes")
		}
	}

	for _, s := range []struct {
		k string
		d *int
	}{
		{"POLL_INTERVAL", &cfg.PollInterval},
		{"FAST_POLL_INTERVAL", &cfg.FastPoll},
		{"FAILSAFE_DUTY", &cfg.Failsafe.Duty},
		{"STARTUP_DUTY", &cfg.StartupDuty},
		{"STATE_FILE", nil},
		{"BMC_TIMEOUT", &cfg.BMC.Timeout},
		{"BMC_RETRIES", &cfg.BMC.Retries},
		{"VERIFY_DELAY_SECONDS", &cfg.VerifyDelaySec},
	} {
		if s.d != nil {
			if err := num(s.k, s.d); err != nil {
				return nil, err
			}
		}
	}
	if err := flt("CRITICAL_TEMP", &cfg.CriticalTemp); err != nil {
		return nil, err
	}
	if err := flt("RPM_PER_DUTY", &cfg.RPMPerDuty); err != nil {
		return nil, err
	}
	if err := flt("RPM_OFFSET", &cfg.RPMOffset); err != nil {
		return nil, err
	}
	if err := flt("RPM_WARN_FACTOR", &cfg.RPMWarnFactor); err != nil {
		return nil, err
	}
	boolean("VERIFY_RPM", &cfg.VerifyRPM)
	boolean("VERBOSE", &Verbose)

	str("STATE_FILE", &cfg.State.Path)
	str("BMC_TOOL", &cfg.BMC.Tool)
	str("BMC_INTERFACE", &cfg.BMC.Interface)
	str("BMC_DEVICE", &cfg.BMC.Device)
	str("BMC_HOST", &cfg.BMC.Host)
	str("BMC_USER", &cfg.BMC.Username)
	str("BMC_PASSWORD_FILE", &cfg.BMC.PasswordFile)
	str("BMC_PROFILE", &cfg.BMC.Profile)
	if err := num("BMC_CIPHER", &cfg.BMC.CipherSuite); err != nil {
		return nil, err
	}
	if v, ok := get("ON_EXIT_DUTY"); ok && v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("ON_EXIT_DUTY: %q не число", v)
		}
		cfg.OnExit = &OnExitCfg{Duty: &n}
	}

	// --- источники: SOURCE1_, SOURCE2_, ... (или SRC1_ для краткости) ---
	for i := 1; i <= 40; i++ {
		p := fmt.Sprintf("SOURCE%d_", i)
		if alt, ok := get(p + "NAME"); !ok || alt == "" {
			p = fmt.Sprintf("SRC%d_", i)
			if v, ok := get(p + "NAME"); !ok || v == "" {
				continue
			}
		}
		s := SourceCfg{Name: m[p+"NAME"], Agg: "max"}
		str(p+"TYPE", &s.Type)
		if v, ok := get(p + "SENSOR"); ok && v != "" {
			// одиночный датчик либо команда gpu-источника (может содержать
			// аргументы) - в Sensor целиком, без разрезания по пробелам.
			// Для списка датчиков есть SENSORS.
			s.Sensor = strings.TrimSpace(v)
		}
		if v, ok := get(p + "SENSORS"); ok && v != "" {
			s.Sensors = append(s.Sensors, splitList(v)...)
		}
		str(p+"PATH", &s.Path)
		str(p+"AGG", &s.Agg)
		if err := num(p+"VM", &s.VM); err != nil {
			return nil, err
		}
		boolean(p+"OPTIONAL", &s.Optional)
		if err := flt(p+"FLOOR", &s.Floor); err != nil {
			return nil, err
		}
		cfg.Sources = append(cfg.Sources, s)
	}

	// --- группы: G1_, G2_, ... ---
	for i := 1; i <= 10; i++ {
		p := fmt.Sprintf("G%d_", i)
		g := GroupCfg{Agg: "max", MinDuty: 40, MaxDuty: 100, Step: 3, Dwell: 90}
		if v, ok := get(p + "NAME"); ok {
			g.Name = v
		}
		if v, ok := get(p + "PWM"); ok {
			g.PWMIDs = splitInts(v)
		}
		if v, ok := get(p + "SOURCES"); ok {
			g.Sources = splitList(v)
		}
		str(p+"AGG", &g.Agg)
		if v, ok := get(p + "TACH"); ok {
			g.Tach = splitList(v)
		}
		str(p+"PROFILE", &g.Profile)
		if err := num(p+"MIN_DUTY", &g.MinDuty); err != nil {
			return nil, err
		}
		if err := num(p+"MAX_DUTY", &g.MaxDuty); err != nil {
			return nil, err
		}
		if err := num(p+"STEP", &g.Step); err != nil {
			return nil, err
		}
		if err := num(p+"DWELL", &g.Dwell); err != nil {
			return nil, err
		}
		if err := num(p+"FAILSAFE_DUTY", &g.FailsafeDuty); err != nil {
			return nil, err
		}
		if err := num(p+"STARTUP_DUTY", &g.StartupDuty); err != nil {
			return nil, err
		}
		if err := flt(p+"CRITICAL_TEMP", &g.CriticalTemp); err != nil {
			return nil, err
		}
		if err := flt(p+"RPM_PER_DUTY", &g.RPMPerDuty); err != nil {
			return nil, err
		}
		if v, ok := get(p + "CURVE_MODE"); ok {
			g.Curve.Mode = v
		}
		if v, ok := get(p + "POWER_CURVE"); ok && v != "" {
			pts, err := parseCurve(v)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", p+"POWER_CURVE", err)
			}
			g.PowerCurve.Mode = "step"
			if m, ok := get(p + "POWER_CURVE_MODE"); ok && m != "" {
				g.PowerCurve.Mode = m
			}
			g.PowerCurve.Points = pts
		}
		if v, ok := get(p + "CURVE"); ok && v != "" {
			pts, err := parseCurve(v)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", p+"CURVE", err)
			}
			g.Curve.Points = pts
		}
		if len(g.PWMIDs) == 0 && g.Name == "" {
			continue
		}
		if g.Name == "" {
			g.Name = fmt.Sprintf("group%d", i)
		}
		cfg.Groups = append(cfg.Groups, g)
	}
	return cfg, nil
}

func parseCurve(s string) ([]CurvePoint, error) {
	var out []CurvePoint
	for _, tok := range strings.Fields(strings.ReplaceAll(s, ",", " ")) {
		t, d, ok := strings.Cut(tok, ":")
		if !ok {
			return nil, fmt.Errorf("ожидается ТЕМП:ПРОЦЕНТЫ, получено %q", tok)
		}
		ti, err := strconv.Atoi(strings.TrimSpace(t))
		if err != nil {
			return nil, fmt.Errorf("температура %q не число", t)
		}
		di, err := strconv.Atoi(strings.TrimSpace(d))
		if err != nil {
			return nil, fmt.Errorf("duty %q не число", d)
		}
		out = append(out, CurvePoint{Temp: ti, Duty: di})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("пустая кривая")
	}
	return out, nil
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Fields(strings.ReplaceAll(s, ",", " ")) {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func splitInts(s string) []int {
	var out []int
	for _, p := range splitList(s) {
		n, err := strconv.Atoi(p)
		if err == nil {
			out = append(out, n)
		}
	}
	return out
}

// ---------------------------------------------------------------------------

func (c *Config) normalize() {
	d := DefaultConfig()
	if c.PollInterval <= 0 {
		c.PollInterval = d.PollInterval
	}
	if c.FastPoll <= 0 {
		c.FastPoll = d.FastPoll
	}
	if c.CriticalTemp <= 0 {
		c.CriticalTemp = d.CriticalTemp
	}
	if c.RPMPerDuty <= 0 {
		c.RPMPerDuty = d.RPMPerDuty
	}
	if c.RPMWarnFactor <= 0 {
		c.RPMWarnFactor = d.RPMWarnFactor
	}
	if c.VerifyDelaySec <= 0 {
		c.VerifyDelaySec = d.VerifyDelaySec
	}
	if c.Failsafe.Duty <= 0 {
		c.Failsafe.Duty = d.Failsafe.Duty
	}
	if c.State.Path == "" {
		c.State.Path = d.State.Path
	}
	if c.BMC.Tool == "" {
		c.BMC.Tool = d.BMC.Tool
	}
	if c.BMC.Interface == "" {
		c.BMC.Interface = d.BMC.Interface
	}
	switch c.BMC.Interface {
	case "system-interface", "local", "kcs", "bmc", "open":
		c.BMC.Interface = "open"
	}
	if c.BMC.Device == "" {
		c.BMC.Device = d.BMC.Device
	}
	if c.BMC.Timeout <= 0 {
		c.BMC.Timeout = d.BMC.Timeout
	}
	if c.BMC.Retries < 0 {
		c.BMC.Retries = d.BMC.Retries
	}
	if c.BMC.Profile == "" {
		c.BMC.Profile = "tyan64"
	}
	for i := range c.Groups {
		g := &c.Groups[i]
		if g.Profile == "" {
			g.Profile = c.BMC.Profile
		}
		if g.Name == "" {
			g.Name = fmt.Sprintf("group%d", i+1)
		}
		if g.MinDuty <= 0 {
			g.MinDuty = 40
		}
		if g.MaxDuty <= 0 {
			g.MaxDuty = 100
		}
		if g.Step <= 0 {
			g.Step = 3
		}
		if g.Dwell <= 0 {
			g.Dwell = 90
		}
		if g.FailsafeDuty <= 0 {
			g.FailsafeDuty = c.Failsafe.Duty
		}
		if g.StartupDuty <= 0 {
			g.StartupDuty = c.StartupDuty
		}
		// стартовое значение не может выбиваться за пределы группы
		if g.StartupDuty < g.MinDuty {
			g.StartupDuty = g.MinDuty
		}
		if g.StartupDuty > g.MaxDuty {
			g.StartupDuty = g.MaxDuty
		}
		if g.CriticalTemp <= 0 {
			g.CriticalTemp = c.CriticalTemp
		}
		if g.RPMPerDuty <= 0 {
			g.RPMPerDuty = c.RPMPerDuty
		}
		if g.Agg == "" {
			g.Agg = "max"
		}
		if g.Curve.Mode == "" {
			g.Curve.Mode = "step"
		}
		sort.Slice(g.Curve.Points, func(i, j int) bool { return g.Curve.Points[i].Temp < g.Curve.Points[j].Temp })
		sort.Slice(g.PowerCurve.Points, func(i, j int) bool { return g.PowerCurve.Points[i].Temp < g.PowerCurve.Points[j].Temp })
	}
	for i := range c.Sources {
		if c.Sources[i].Agg == "" {
			c.Sources[i].Agg = "max"
		}
		if c.Sources[i].Name == "" {
			c.Sources[i].Name = fmt.Sprintf("source%d", i+1)
		}
	}
}

func (c *Config) validate() error {
	switch c.BMC.Interface {
	case "open":
		if os.Getenv("AUTOFAN_DRYRUN") == "" {
			if _, err := os.Stat(c.BMC.Device); err != nil {
				return fmt.Errorf("bmc device %s: %w", c.BMC.Device, err)
			}
		}
	case "lanplus", "lan":
		if c.BMC.Host == "" || c.BMC.Username == "" {
			return fmt.Errorf("bmc host/username required for interface %s", c.BMC.Interface)
		}
		if c.BMC.PasswordFile == "" && os.Getenv("AUTOFAN_DRYRUN") == "" {
			return fmt.Errorf("bmc password_file required (passing a password as an argument is insecure)")
		}
	default:
		return fmt.Errorf("unknown bmc interface %q (open|lanplus|lan)", c.BMC.Interface)
	}
	if len(c.Groups) == 0 {
		return fmt.Errorf("no fan groups configured (G1_NAME/G1_PWM ...)")
	}
	if len(c.Sources) == 0 {
		return fmt.Errorf("no temperature sources configured (SOURCE1_...)")
	}
	byName := map[string]bool{}
	for _, s := range c.Sources {
		if byName[s.Name] {
			return fmt.Errorf("duplicate source name %q", s.Name)
		}
		byName[s.Name] = true
		switch s.Type {
		case "gpu", "bmc", "hwmon":
		default:
			return fmt.Errorf("source %s: unknown type %q (gpu|bmc|hwmon)", s.Name, s.Type)
		}
	}
	for _, g := range c.Groups {
		if len(g.PWMIDs) == 0 {
			return fmt.Errorf("group %s: empty pwm list", g.Name)
		}
		for _, id := range g.PWMIDs {
			if id < 0 || id > 15 {
				return fmt.Errorf("group %s: pwm id %d out of range", g.Name, id)
			}
		}
		if len(g.Sources) == 0 {
			return fmt.Errorf("group %s: no sources", g.Name)
		}
		for _, s := range g.Sources {
			if !byName[s] {
				return fmt.Errorf("group %s: unknown source %q", g.Name, s)
			}
		}
		if len(g.Curve.Points) == 0 {
			return fmt.Errorf("group %s: empty curve", g.Name)
		}
		if g.MinDuty < 10 || g.MinDuty > 100 || g.MaxDuty > 100 || g.MinDuty > g.MaxDuty {
			return fmt.Errorf("group %s: bad duty range %d..%d", g.Name, g.MinDuty, g.MaxDuty)
		}
		for _, p := range g.Curve.Points {
			if p.Duty < g.MinDuty || p.Duty > g.MaxDuty {
				return fmt.Errorf("group %s: curve duty %d at %dC is outside %d..%d", g.Name, p.Duty, p.Temp, g.MinDuty, g.MaxDuty)
			}
		}
		for _, p := range g.PowerCurve.Points {
			if p.Duty < g.MinDuty || p.Duty > g.MaxDuty {
				return fmt.Errorf("group %s: power curve duty %d at %dW is outside %d..%d", g.Name, p.Duty, p.Temp, g.MinDuty, g.MaxDuty)
			}
		}
		switch g.Curve.Mode {
		case "step", "linear":
		default:
			return fmt.Errorf("group %s: unknown curve mode %q", g.Name, g.Curve.Mode)
		}
		if g.FailsafeDuty < g.MinDuty || g.FailsafeDuty > 100 {
			return fmt.Errorf("group %s: failsafe duty %d outside %d..100", g.Name, g.FailsafeDuty, g.MinDuty)
		}
	}
	return nil
}

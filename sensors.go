package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Reading is one temperature sample from one source.
type Reading struct {
	Name string
	Temp float64
	// Absent means "this source legitimately has nothing to report right now"
	// (e.g. the GPU lives in a guest that is powered off). It is not an error.
	Absent bool
	// Skip means the value is below the source's floor: the sensor is cold, so
	// it must not pull the group's target up.
	Skip bool
	// Power is the wattage a source reports (GPU). It is heat dumped into the
	// case even when the card's own temperature stays low.
	Power float64
	Err   error
}

type Source interface {
	Name() string
	Read(ctx context.Context) Reading
}

// floorSource hides a sensor until it becomes warm enough to matter. On this
// board the DIMM/MOSFET sensors read 52-55 C even when the machine is idle,
// so without a floor they would keep the fans above their idle duty forever.
type floorSource struct {
	inner Source
	floor float64
}

func (f *floorSource) Name() string { return f.inner.Name() }

func (f *floorSource) Read(ctx context.Context) Reading {
	r := f.inner.Read(ctx)
	if r.Err == nil && r.Temp < f.floor {
		r.Skip = true
	}
	return r
}

func NewSources(cfg *Config, bmc *BMC) []Source {
	var out []Source
	for _, s := range cfg.Sources {
		var src Source
		switch s.Type {
		case "gpu":
			src = &GPUSource{cfg: s, qga: s.VM > 0, vmid: s.VM}
		case "bmc":
			sensors := append([]string{}, s.Sensors...)
			if s.Sensor != "" {
				sensors = append(sensors, s.Sensor)
			}
			if len(sensors) == 0 {
				logf("WARN источник %s: тип bmc без датчика, пропускаю", s.Name)
				continue
			}
			src = &BMCSource{cfg: s, bmc: bmc, sensors: sensors}
		case "hwmon":
			src = &HwmonSource{cfg: s}
		default:
			logf("WARN источник %s: неизвестный тип %q, пропускаю", s.Name, s.Type)
			continue
		}
		if s.Floor > 0 {
			src = &floorSource{inner: src, floor: s.Floor}
		}
		out = append(out, src)
	}
	return out
}

// ---- GPU via nvidia-smi, locally or through a Proxmox guest agent ----

type GPUSource struct {
	cfg  SourceCfg
	qga  bool
	vmid int
}

// cmd - чем и с какими аргументами замерять. По умолчанию nvidia-smi, но через
// SENSOR можно задать свою команду с тем же CSV на stdout (полезно для
// проверки контура без нагрузки и для нестандартных путей). Команда разбирается
// по пробелам, то есть аргументы писать можно: "sensor=/usr/local/bin/x.sh 250".
func (g *GPUSource) cmd() []string {
	if g.cfg.Sensor != "" {
		return strings.Fields(g.cfg.Sensor)
	}
	tool := "nvidia-smi"
	if g.qga {
		tool = "/usr/bin/nvidia-smi"
	}
	return []string{tool, "--query-gpu=" + gpuQuery, "--format=csv,noheader,nounits"}
}

func (g *GPUSource) Name() string { return g.cfg.Name }

const gpuQuery = "index,name,temperature.gpu,temperature.memory,power.draw,utilization.gpu"

func (g *GPUSource) Read(ctx context.Context) Reading {
	var csv string
	argv := g.cmd()
	if g.qga {
		// The GPU is assigned to a VM, so the host cannot see it with NVML.
		// Ask the guest over qemu-guest-agent instead (qm guest exec).
		ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
		defer cancel()
		full := append([]string{"guest", "exec", strconv.Itoa(g.vmid), "--"}, argv...)
		cmd := exec.CommandContext(ctx, "qm", full...)
		out, err := cmd.Output()
		if err != nil {
			return Reading{Name: g.cfg.Name, Absent: g.cfg.Optional, Err: err}
		}
		var res struct {
			Exited   interface{} `json:"exited"`
			ExitCode float64     `json:"exitcode"`
			OutData  string      `json:"out-data"`
			ErrData  string      `json:"err-data"`
		}
		if err := json.Unmarshal(out, &res); err != nil {
			return Reading{Name: g.cfg.Name, Err: fmt.Errorf("qga: %w", err)}
		}
		// qm prints "exited": 1 (a number); some qga builds print true/false
		exited := res.Exited == float64(1) || res.Exited == true
		if exited && res.ExitCode != 0 {
			return Reading{Name: g.cfg.Name, Absent: g.cfg.Optional,
				Err: fmt.Errorf("guest nvidia-smi exit %.0f: %s", res.ExitCode, strings.TrimSpace(res.ErrData))}
		}
		if res.OutData == "" {
			return Reading{Name: g.cfg.Name, Absent: g.cfg.Optional,
				Err: fmt.Errorf("guest nvidia-smi returned no data (no GPU in guest?)")}
		}
		csv = res.OutData
	} else {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
		out, err := cmd.Output()
		if err != nil {
			return Reading{Name: g.cfg.Name, Absent: g.cfg.Optional, Err: err}
		}
		csv = string(out)
	}

	var temps []float64
	var watts float64
	for _, line := range strings.Split(strings.TrimSpace(csv), "\n") {
		if line == "" {
			continue
		}
		f := strings.Split(line, ",")
		if len(f) < 3 {
			continue
		}
		// temperature.gpu, and temperature.memory when the card reports it
		for _, idx := range []int{2, 3} {
			if idx >= len(f) {
				continue
			}
			v, err := strconv.ParseFloat(strings.TrimSpace(f[idx]), 64)
			if err == nil && v > 0 {
				temps = append(temps, v)
			}
		}
		// power.draw (поле 4) - сколько ватт уходит в корпус
		if len(f) > 4 {
			if v, err := strconv.ParseFloat(strings.TrimSpace(f[4]), 64); err == nil && v > watts {
				watts = v
			}
		}
	}
	if len(temps) == 0 {
		return Reading{Name: g.cfg.Name, Absent: true,
			Err: fmt.Errorf("nvidia-smi не вернул ни одной температуры")}
	}
	return Reading{Name: g.cfg.Name, Temp: agg(temps, g.cfg.Agg), Power: watts}
}

func agg(v []float64, mode string) float64 {
	if len(v) == 0 {
		return 0
	}
	switch mode {
	case "mean":
		s := 0.0
		for _, x := range v {
			s += x
		}
		return s / float64(len(v))
	case "min":
		m := v[0]
		for _, x := range v {
			if x < m {
				m = x
			}
		}
		return m
	default:
		m := v[0]
		for _, x := range v {
			if x > m {
				m = x
			}
		}
		return m
	}
}

// ---- BMC SDR sensor ----

type BMCSource struct {
	cfg     SourceCfg
	bmc     *BMC
	sensors []string
}

func (b *BMCSource) Name() string { return b.cfg.Name }

func (b *BMCSource) Read(ctx context.Context) Reading {
	// one `sdr elist` per control cycle is shared by all bmc sources
	m, err := b.bmc.SDRCached(ctx, 10*time.Second)
	if err != nil {
		return Reading{Name: b.cfg.Name, Err: err}
	}
	var vals []float64
	var missing []string
	for _, sn := range b.sensors {
		if v, ok := m[sn]; ok {
			vals = append(vals, v)
		} else {
			missing = append(missing, sn)
		}
	}
	if len(vals) == 0 {
		return Reading{Name: b.cfg.Name, Absent: true,
			Err: fmt.Errorf("sensor(s) %s not in SDR or without reading", strings.Join(b.sensors, ","))}
	}
	if len(missing) > 0 {
		vlogf("INFO source %s: ignoring missing %s", b.cfg.Name, strings.Join(missing, ","))
	}
	return Reading{Name: b.cfg.Name, Temp: agg(vals, b.cfg.Agg)}
}

// ---- sysfs hwmon ----

type HwmonSource struct{ cfg SourceCfg }

func (h *HwmonSource) Name() string { return h.cfg.Name }

func (h *HwmonSource) Read(ctx context.Context) Reading {
	pattern := h.cfg.Path
	if pattern == "" {
		pattern = "/sys/class/hwmon/*/temp*_input"
	}
	var temps []float64
	paths, _ := filepath.Glob(pattern)
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		if label, err := os.ReadFile(filepath.Join(filepath.Dir(p), "name")); err == nil {
			_ = label
		}
		v, err := strconv.ParseFloat(strings.TrimSpace(string(b)), 64)
		if err != nil {
			continue
		}
		temps = append(temps, v/1000.0)
	}
	if len(temps) == 0 {
		// обязан быть Err: иначе «источник ничего не нашёл» неотличим от
		// честных 0 градусов, и мёртвый источник тянул бы duty к минимуму
		return Reading{Name: h.cfg.Name, Absent: true,
			Err: fmt.Errorf("no hwmon files matched %s", pattern)}
	}
	return Reading{Name: h.cfg.Name, Temp: agg(temps, h.cfg.Agg)}
}

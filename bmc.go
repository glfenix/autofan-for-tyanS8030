package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// BMC is a small, serialising ipmitool wrapper.
//
// Calls are serialised on purpose: the local KCS channel (/dev/ipmi0) is
// strictly serial and concurrent access can time out, and each lanplus call
// opens a fresh RMCP+ session which the AST2500 BMC rate-limits.
type BMC struct {
	cfg     BMCCfg
	profile string
	dry     bool
	mu      sync.Mutex

	sdrMu  sync.Mutex
	sdrMap map[string]float64
	sdrAt  time.Time
}

func NewBMC(cfg BMCCfg, profile string) *BMC { return &BMC{cfg: cfg, profile: profile} }

// SetDry turns the BMC into a no-op writer that only logs the IPMI command.
func (b *BMC) SetDry(v bool) { b.dry = v }

func (b *BMC) args() []string {
	a := []string{"-I", b.cfg.Interface}
	switch b.cfg.Interface {
	case "open":
		a = append(a, "-B", "5", "-R", strconv.Itoa(b.cfg.Retries+1), "-D", b.cfg.Device)
	default:
		a = append(a, "-H", b.cfg.Host, "-U", b.cfg.Username, "-f", b.cfg.PasswordFile,
			"-C", strconv.Itoa(b.cfg.CipherSuite), "-N", "4", "-R", strconv.Itoa(b.cfg.Retries))
	}
	return a
}

// run executes `ipmitool <args> <sub...>` and returns stdout.
func (b *BMC) run(ctx context.Context, sub ...string) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	args := append(b.args(), sub...)
	ctx, cancel := context.WithTimeout(ctx, time.Duration(b.cfg.Timeout)*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, b.cfg.Tool, args...)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("%w (%s %s: %s)", err, b.cfg.Tool,
			strings.Join(append(b.args(), sub...), " "), firstLine(errb.String()))
	}
	return out.String(), nil
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i > 0 {
		return s[:i]
	}
	return s
}

// rawCommand builds the OEM raw command for a PWM channel. Kept as a separate
// function so it can be unit tested - ipmitool only accepts decimal or
// "0x"-prefixed hex, so a bare "5a" is rejected and, worse, a bare "40" is
// silently read as decimal 40 instead of 0x40 (64%).
func rawCommand(profile string, pwm, duty int) ([]string, error) {
	if duty < 0 || duty > 100 {
		return nil, fmt.Errorf("duty %d out of range 0..100", duty)
	}
	if pwm < 0 || pwm > 15 {
		return nil, fmt.Errorf("pwm id %d out of range 0..15", pwm)
	}
	hp := fmt.Sprintf("0x%02x", pwm)
	hd := fmt.Sprintf("0x%02x", duty)
	switch profile {
	case "tyan64":
		return []string{"raw", "0x2e", "0x44", "0xfd", "0x19", "0x00", hp, "0x01", hd}, nil
	case "tyan32":
		return []string{"raw", "0x2e", "0x05", "0xfd", "0x19", "0x00", hp, hd}, nil
	}
	return nil, fmt.Errorf("unknown profile %q", profile)
}

// SetDuty applies a PWM duty cycle with the TYAN/MiTAC OEM command.
//
//	tyan64 (BMC 4.11.x / 6.02, 64MB flash): raw 0x2e 0x44 0xfd 0x19 0x00 <pwm> 0x01 <duty>
//	tyan32 (old 32MB firmware):             raw 0x2e 0x05 0xfd 0x19 0x00 <pwm> <duty>
//
// Verified on S8030GM2NE / BMC 6.02: tyan64 returns "fd 19 00", tyan32 is
// rejected with rsp=0xc7 (invalid data length). 0xff ("return to auto") is
// rejected with rsp=0xcc, i.e. this firmware has no software path back to the
// BMC fan curve; that can only be re-enabled in BIOS Setup.
func (b *BMC) SetDuty(ctx context.Context, pwm, duty int) error {
	return b.SetDutyProfile(ctx, b.profile, pwm, duty)
}

// SetDutyProfile allows each fan group to use its own OEM command flavour.
func (b *BMC) SetDutyProfile(ctx context.Context, profile string, pwm, duty int) error {
	raw, err := rawCommand(profile, pwm, duty)
	if err != nil {
		return err
	}
	if b.dry {
		logf("DRYRUN %s %s", b.cfg.Tool, strings.Join(append(b.args(), raw...), " "))
		return nil
	}
	_, err = b.run(ctx, raw...)
	return err
}

// SDR returns "sensor name" -> numeric reading (RPM, degrees C, watts, ...).
func (b *BMC) SDR(ctx context.Context) (map[string]float64, error) {
	out, err := b.run(ctx, "sdr", "elist")
	if err != nil {
		return nil, err
	}
	return parseSDR(out)
}

// SDRCached reuses a reading taken within ttl, so that several "bmc" sources
// cost one ipmitool call per control cycle instead of one call each.
func (b *BMC) SDRCached(ctx context.Context, ttl time.Duration) (map[string]float64, error) {
	b.sdrMu.Lock()
	defer b.sdrMu.Unlock()
	if b.sdrMap != nil && time.Since(b.sdrAt) < ttl {
		return b.sdrMap, nil
	}
	m, err := b.SDR(ctx)
	if err != nil {
		b.sdrMap = nil
		return nil, err
	}
	b.sdrMap, b.sdrAt = m, time.Now()
	return m, nil
}

// ResetSDR drops the cached sensor table so the next cycle reads fresh values
// even when the poll interval is shorter than the cache ttl.
func (b *BMC) ResetSDR() {
	b.sdrMu.Lock()
	defer b.sdrMu.Unlock()
	b.sdrMap = nil
	b.sdrAt = time.Time{}
}

func parseSDR(out string) (map[string]float64, error) {
	res := map[string]float64{}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Split(line, "|")
		if len(fields) < 5 {
			continue
		}
		name := strings.TrimSpace(fields[0])
		reading := strings.TrimSpace(fields[len(fields)-1])
		if name == "" || reading == "" {
			continue
		}
		low := strings.ToLower(reading)
		if strings.Contains(low, "no reading") || strings.Contains(low, "disabled") ||
			strings.Contains(low, "ns ") || low == "ns" {
			continue
		}
		num := strings.Fields(reading)[0]
		v, err := strconv.ParseFloat(strings.TrimRight(num, "lL"), 64)
		if err != nil {
			continue
		}
		res[name] = v
	}
	if len(res) == 0 {
		return res, errors.New("no sensors parsed")
	}
	return res, nil
}

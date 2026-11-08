package main

import (
	"context"
	"strings"
	"testing"
)

const sampleSDR = `CPU_FAN          | 60h | ok  | 29.0 | 1242 RPM
SYS_FAN_1        | 61h | ok  | 29.0 | 966 RPM
SYS_FAN_5        | 65h | ns  | 29.0 | No Reading
CPU_Tctl_Value   | 01h | ok  |  3.1 | 44 degrees C
M.2/M.3_NVMe_0   | 52h | ok  |  7.0 | 36 degrees C
PSU1_Temp        | 78h | Disabled | 10.2 | 0 degrees C
PVB_BMC          | A0h | ok  |  7.0 | 11.84 Volts
`

func TestParseSDR(t *testing.T) {
	m, err := parseSDR(sampleSDR)
	if err != nil {
		t.Fatal(err)
	}
	if m["SYS_FAN_1"] != 966 {
		t.Errorf("SYS_FAN_1 = %v, want 966", m["SYS_FAN_1"])
	}
	if m["CPU_Tctl_Value"] != 44 {
		t.Errorf("CPU_Tctl_Value = %v, want 44", m["CPU_Tctl_Value"])
	}
	// sensor names containing slashes must survive
	if m["M.2/M.3_NVMe_0"] != 36 {
		t.Errorf("nvme = %v, want 36", m["M.2/M.3_NVMe_0"])
	}
	if _, ok := m["SYS_FAN_5"]; ok {
		t.Errorf("no-reading sensor must be skipped, got %v", m["SYS_FAN_5"])
	}
	if m["PVB_BMC"] != 11.84 {
		t.Errorf("volts = %v, want 11.84", m["PVB_BMC"])
	}
}

func TestSetDutyRejectsOutOfRange(t *testing.T) {
	b := NewBMC(BMCCfg{Interface: "open"}, "tyan64")
	b.SetDry(true)
	ctx := context.Background()
	if err := b.SetDuty(ctx, 2, 101); err == nil {
		t.Error("duty 101 should be rejected")
	}
	if err := b.SetDuty(ctx, 16, 50); err == nil {
		t.Error("pwm 16 should be rejected")
	}
	if err := b.SetDuty(ctx, 2, 50); err != nil {
		t.Errorf("valid duty failed: %v", err)
	}
}

// Regression: ipmitool parses "40" as decimal 40, not 0x40, and rejects "5a".
// Every data byte must therefore be written as 0x%02x.
func TestRawCommandHexEncoding(t *testing.T) {
	want := []string{"raw", "0x2e", "0x44", "0xfd", "0x19", "0x00", "0x02", "0x01", "0x5a"}
	got, err := rawCommand("tyan64", 2, 90)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("rawCommand = %v, want %v", got, want)
	}
	got32, err := rawCommand("tyan32", 5, 100)
	if err != nil {
		t.Fatal(err)
	}
	if got32[len(got32)-1] != "0x64" || got32[len(got32)-2] != "0x05" {
		t.Errorf("tyan32 command wrong: %v", got32)
	}
	if _, err := rawCommand("tyan64", 2, 137); err == nil {
		t.Error("out of range duty must be rejected before touching ipmitool")
	}
}

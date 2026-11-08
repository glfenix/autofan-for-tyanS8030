#!/usr/bin/env python3
"""Наблюдение за теплом: GPU (Вт, °C) против датчиков корпуса и текущего duty.

Показывает то, ради чего существует упреждение по мощности: температура ядра
карты почти не растёт, а в корпус уходит сотня-другая ватт, и греются от этого
датчики зоны VRM/памяти.

  python3 thermal-sample.py 900 > sample.csv        # 15 минут
  VM=104 STATE=/var/lib/autofan/state.json python3 thermal-sample.py 300

Нужны: ipmitool (локальный /dev/ipmi0) и, если карта в госте, qm +
qemu-guest-agent. Скрипт только читает.
"""
import subprocess, json, sys, time, re, datetime, os

VM = os.environ.get("VM", "104")            # id гостя с картой; "" = читать локально
STATE = os.environ.get("STATE", "/var/lib/autofan/state.json")
GROUP = os.environ.get("GROUP", "chassis")
STEP = int(os.environ.get("STEP", "15"))


def sdr():
    out = subprocess.run(["ipmitool", "-I", "open", "sdr", "elist"],
                         capture_output=True, text=True).stdout
    d = {}
    for line in out.splitlines():
        p = [x.strip() for x in line.split("|")]
        if len(p) >= 5:
            m = re.match(r"([\d.]+)", p[4])
            if m:
                d[p[0]] = float(m.group(1))
    return d


def gpu():
    """(температура, ватты, утилизация) или прочерки, если прочитать нельзя."""
    q = ["--query-gpu=temperature.gpu,power.draw,utilization.gpu",
         "--format=csv,noheader,nounits"]
    if VM:
        cmd = ["qm", "guest", "exec", VM, "--", "/usr/bin/nvidia-smi"] + q
    else:
        cmd = ["/usr/bin/nvidia-smi"] + q
    out = subprocess.run(cmd, capture_output=True, text=True).stdout
    try:
        if VM:
            data = json.loads(out)["out-data"]          # "exited" там число, не bool
        else:
            data = out
        t, p, u = data.strip().split(",")
        return float(t), float(p), float(u)
    except Exception:
        return None, None, None


def duty():
    try:
        return json.load(open(STATE))["duties"].get(GROUP)
    except Exception:
        return None


if len(sys.argv) < 2:
    sys.exit("использование: thermal-sample.py <секунд>")

end = time.time() + int(sys.argv[1])
print("time,gpuC,gpuW,gpuUtil,cpu_tctl,cpu_mos,dimm_mos,umcMax,outlet,inlet,cpu_fan,fan1,duty", flush=True)
while time.time() < end:
    s = sdr()
    g = gpu()
    umc = max([v for k, v in s.items() if k.startswith("P0_UMC")] or [None])
    row = [datetime.datetime.now().strftime("%H:%M:%S"), *g,
           s.get("CPU_Tctl_Value"), s.get("CPU_MOSFET"), s.get("DIMM_MOSFET_1"), umc,
           s.get("SYS_Air_Outlet"), s.get("MB_Air_Inlet"), s.get("CPU_FAN"),
           s.get("SYS_FAN_1"), duty()]
    print(",".join("" if v is None else str(v) for v in row), flush=True)
    time.sleep(STEP)

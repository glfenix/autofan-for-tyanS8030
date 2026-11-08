#!/bin/bash
# Тепловая проверка: прожиг ВСЕХ потоков хоста + наблюдения BMC в одном логе.
#
#   ./lt.sh 48 150 auto     # работает autofan (боевая кривая)
#   ./lt.sh 48 150 40       # сервис остановлен, корпусные закреплены на 40 %
#
# Переменные окружения: VM (гость с GPU, "" - не читать GPU), PWM ("2 3 4 5"),
# BURN (путь к собранной нагрузке), LOGDIR. Важно: число потоков = все логические
# процессоры хоста. ВМ с 24 vCPU из 48 даёт «100 % нагрузки» вполовину слабее -
# именно на этом первые замеры оказались оптимистичными.
set -uo pipefail

S="CPU_Tctl_Value CPU_MOSFET CPU_SOC_MOSFET DIMM_MOSFET_1 SYS_Air_Outlet MB_Air_Inlet P0_UMC7_CH_F0 CPU_FAN SYS_FAN_1"
N=${1:-$(nproc)}; T=${2:-150}; DUTY=${3:-auto}
VM="${VM-104}"
PWM="${PWM:-2 3 4 5}"
BURN="${BURN:-$HOME/burn}"
LOGDIR="${LOGDIR:-$HOME}"
LOG="$LOGDIR/lt.$(date +%H%M%S).log"
BIN="${BIN:-/usr/local/sbin/autofan}"
SVC="${SVC:-autofan}"

set_pwm() { for p in $PWM; do "$BIN" set "$p" "$1" >/dev/null 2>&1; done; }

sample() {
  local util gpu s
  util=$(python3 - <<'PY'
import time
f=open('/proc/stat').readline().split()
t0=sum(map(int,f[1:9])); i0=int(f[4])+int(f[5])
time.sleep(4)
f=open('/proc/stat').readline().split()
t1=sum(map(int,f[1:9])); i1=int(f[4])+int(f[5])
print("%5.1f%% cpu" % (100*(1-(i1-i0)/(t1-t0))))
PY
)
  gpu="-"
  if [ -n "$VM" ]; then
    gpu=$(qm guest exec "$VM" --timeout 8 -- /usr/bin/nvidia-smi \
          --query-gpu=temperature.gpu,utilization.gpu --format=csv,noheader,nounits 2>/dev/null \
          | python3 -c 'import json,sys; print(json.load(sys.stdin).get("out-data","?").strip())' 2>/dev/null)
  fi
  s=$(ipmitool -I open sdr elist 2>/dev/null | awk -F' \| ' -v s="$S" '
      BEGIN{n=split(s,a," "); for(i=1;i<=n;i++) want[a[i]]=1}
      {name=$1; gsub(/^ +| +$/,"",name); if (name in want) {v=$5; gsub(/ ?(RPM|degrees C)/,"",v); gsub(/^ +| +$/,"",v); printf "%s=%s ", name, v}}')
  echo "$(date +%H:%M:%S) $util gpu=[$gpu] $s" | tee -a "$LOG"
}

echo "=== фаза: duty=$DUTY, потоков=$N, длительность=${T}s ==="
if [ "$DUTY" != auto ]; then
  systemctl stop "$SVC"; sleep 2
  set_pwm "$DUTY"
  echo "корпусные закреплены на ${DUTY}%"
fi
echo "--- фон до нагрузки ---"; sample
"$BURN" "$N" "$T" >/dev/null 2>&1 &
BURN_PID=$!
end=$((SECONDS + T))
while [ $SECONDS -lt $end ]; do
  sleep 12
  printf "  "
  sample
  if [ "$DUTY" = auto ]; then
    j=$(journalctl -u "$SVC" --since -15s --no-pager -q 2>/dev/null | sed 's/.*autofan: //' | tr '\n' '|')
    [ -n "$j" ] && echo "      autofan: $j"
  fi
done
wait $BURN_PID 2>/dev/null
echo "--- сразу после снятия нагрузки ---"; sample
sleep 45
echo "--- через 45 с охлаждения ---"; sample

echo
echo "=== пики за прогон ==="
awk '{for(i=1;i<=NF;i++) if ($i ~ /=/) {split($i,kv,"="); if (kv[2]+0 > mx[kv[1]]+0) mx[kv[1]]=kv[2]}}
     END{for (k in mx) printf "  %-16s max %s\n", k, mx[k]}' "$LOG" | sort

if [ "$DUTY" != auto ]; then systemctl start "$SVC"; echo "autofan снова активен"; fi
echo "лог: $LOG"

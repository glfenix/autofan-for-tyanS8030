#!/bin/bash
# Калибровка тахометров: какой duty каким оборотам соответствует.
# Нужна, чтобы проверка результата не ругалась впустую: у этих BMC зависимость
# не проходит через ноль, поэтому ищутся и наклон, и смещение.
#
#   sudo ./calibrate-rpm.sh                  # ступени 30 45 60 75 90
#   sudo DUTIES="25 50 75" TACH=SYS_FAN_1 PWM="2 3 4 5" ./calibrate-rpm.sh
#
# Сервис на время прогона останавливается, в конце возвращается ON_EXIT_DUTY.
set -uo pipefail

DUTIES="${DUTIES:-30 45 60 75 90}"
PWM="${PWM:-2 3 4 5}"
TACH="${TACH:-SYS_FAN_1}"
BIN="${BIN:-/usr/local/sbin/autofan}"
SVC="${SVC:-autofan}"

systemctl stop "$SVC"
points=()
for d in $DUTIES; do
  for p in $PWM; do "$BIN" set "$p" "$d" >/dev/null 2>&1; done
  sleep 15                       # тахометр приходит в норму за 7-10 с
  rpm=$(ipmitool -I open sdr elist 2>/dev/null | awk -F" [|] " -v n="$TACH" '$1==n{print $5}' | tr -dc '0-9.')
  echo "duty ${d}% -> ${rpm} RPM"
  [ -n "$rpm" ] && points+=("$d $rpm")
done
"$BIN" restore >/dev/null 2>&1
systemctl start "$SVC"

printf '%s\n' "${points[@]}" | python3 -c '
import sys
pts=[tuple(map(float,l.split())) for l in sys.stdin if l.strip()]
if len(pts)<2: sys.exit("мало точек")
n=len(pts); sx=sum(p[0] for p in pts); sy=sum(p[1] for p in pts)
sxx=sum(p[0]*p[0] for p in pts); sxy=sum(p[0]*p[1] for p in pts)
k=(n*sxy-sx*sy)/(n*sxx-sx*sx); b=(sy-k*sx)/n
print()
print(f"RPM_PER_DUTY={k:.1f}")
print(f"RPM_OFFSET={b:.0f}")
print()
print("проверка:")
for d,r in pts:
    e=b+k*d
    print(f"  {d:.0f}%: замер {r:.0f}, формула {e:.0f}, расхождение {abs(e-r)/r*100:.1f}%")
'

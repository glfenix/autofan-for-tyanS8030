#!/bin/bash
# Одна и та же нагрузка на GPU при разном duty корпусных вентиляторов.
# Нужен, чтобы подбирать кривую по цифрам, а не на слух: видно, при какой
# скорости реально перестают греться зона VRM/памяти и выхлоп.
#
#   sudo DUTIES="35 45 55 70" ./dutytest.sh 180     # 3 минуты на ступень
#   PWM="2 3 4 5" TACH=SYS_FAN_1 ./dutytest.sh 120
#
# Скрипт останавливает autofan, ставит ступени вручную и в конце возвращает
# ON_EXIT_DUTY и запускает сервис. Нагрузку на карту нужно включить самому и
# держать всё время прогона.
set -uo pipefail

DUTIES="${DUTIES:-35 45 55 70}"
T="${1:-${T:-180}}"
PWM="${PWM:-2 3 4 5}"
TACH="${TACH:-SYS_FAN_1}"
BIN="${BIN:-/usr/local/sbin/autofan}"
SVC="${SVC:-autofan}"

interface="${BMC_INTERFACE:-open}"
[ "$interface" = open ] && IPMI=(ipmitool -I open) || IPMI=(ipmitool -I "$interface" -H "$BMC_HOST" -U "$BMC_USER" -F "$BMC_PASSWORD_FILE")

systemctl stop "$SVC"
echo "duty,umcMax,dimmMos,cpuMos,cpuTctl,outlet,inlet,$TACH"
for d in $DUTIES; do
  for p in $PWM; do "$BIN" set "$p" "$d" >/dev/null 2>&1; done
  sleep 25                                   # тахометр приходит в норму за 7-10 с, запас на прогрев
  acc=0
  while [ "$acc" -lt "$T" ]; do
    s=$("${IPMI[@]}" sdr elist 2>/dev/null)
    v() { echo "$s" | awk -F" [|] " -v n="$1" '$1==n{print $5}' | tr -dc '0-9.'; }
    u=$(echo "$s" | grep -E '^P0_UMC' | awk -F" [|] " '{print $5+0}' | sort -n | tail -1)
    echo "$d,${u:-},$(v DIMM_MOSFET_1),$(v CPU_MOSFET),$(v CPU_Tctl_Value),$(v SYS_Air_Outlet),$(v MB_Air_Inlet),$(v "$TACH")"
    sleep 15; acc=$((acc+15))
  done
done
"$BIN" restore >/dev/null 2>&1
systemctl start "$SVC"

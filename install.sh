#!/bin/bash
# Установка/обновление autofan на хосте (TYAN S8030GM2NE + Proxmox).
#   ./install.sh            # положить файлы + проверочный probe без записи в BMC
#   ./install.sh --enable   # + systemctl enable --now autofan
#
# Существующий /etc/autofan/autofan.conf не перезаписывается: рабочие настройки
# дороже, чем образец. Сравнить: diff /etc/autofan/autofan.conf examples/autofan.conf
set -euo pipefail

SRC="$(cd "$(dirname "$0")" && pwd)"
BIN=/usr/local/sbin/autofan
CFG=/etc/autofan/autofan.conf
UNIT=/etc/systemd/system/autofan.service

pick() { # examples/ — канонический путь, корень репозитория — для старых раскладок
  local f
  for f in "$SRC/examples/$1" "$SRC/$1"; do
    [ -f "$f" ] && { echo "$f"; return; }
  done
  echo "нет файла $1 ни в $SRC, ни в $SRC/examples" >&2
  exit 1
}

if [ "$(id -u)" != 0 ]; then echo "нужен root"; exit 1; fi
command -v ipmitool >/dev/null || { echo "не установлен ipmitool: apt-get install -y ipmitool"; exit 1; }
[ -e /dev/ipmi0 ] || echo "ВНИМАНИЕ: /dev/ipmi0 нет - в конфиге нужен BMC_INTERFACE=lanplus"
[ -f "$SRC/autofan" ] || { echo "нет бинарника $SRC/autofan (собери: make)"; exit 1; }

CONF_SRC="$(pick autofan.conf)"
YAML_SRC="$(pick config.yaml)"
UNIT_SRC="$(pick systemd/autofan.service)"

install -Dm755 "$SRC/autofan" "$BIN"
mkdir -p /etc/autofan /var/lib/autofan
if [ ! -f "$CFG" ]; then
  install -m644 "$CONF_SRC" "$CFG"
  echo "положен конфиг $CFG"
else
  echo "конфиг $CFG уже есть - не трогаю (сравнить: diff $CFG $CONF_SRC)"
fi
[ -f /etc/autofan/config.yaml ] || install -m644 "$YAML_SRC" /etc/autofan/config.yaml
install -m644 "$UNIT_SRC" "$UNIT"
systemctl daemon-reload

echo
echo "=== проверочный прогон без записи в BMC ==="
"$BIN" -c "$CFG" --dry-run probe

if [ "${1:-}" = "--enable" ]; then
  echo
  systemctl enable --now autofan
  systemctl --no-pager status autofan | head -10
fi

# Справочник конфигурации

Файл — обычные `КЛЮЧ=ЗНАЧЕНИЕ`: его можно `source` в bash, он же
`EnvironmentFile=` для systemd. Пустые строки и комментарии (`#`, в том числе в
конце строки) разбираются; значение можно взять в кавычки.

Разбор конфига:

1. расширения `.yaml` / `.yml` — YAML (см. `examples/config.yaml`);
2. иначе — `KEY=VALUE`;
3. поверх файла накладываются переменные **окружения** (только те, что относятся к
   autofan: `POLL_*`, `BMC_*`, `SRC*`, `G1_`…`G6_`, `RPM_*`, `STARTUP*`,
   `STATE_*`, `VERIFY_*`, `FAILSAFE_*`, `ON_EXIT_*`, `CRITICAL_TEMP`, `PROFILE`,
   `VERBOSE`, `AUTOFAN_*`). Так что `EnvironmentFile=` и drop-in'ы systemd
   работают без правки основного файла.

После правки — `systemctl reload autofan` (SIGHUP): конфиг перечитывается без
остановки процесса. Ошибка в конфиге при этом не роняет демон: в журнал попадает
сообщение, и продолжается работа по прежнему.

## Общее

| ключ | по умолчанию | смысл |
|---|---|---|
| `POLL_INTERVAL` | 20 | секунд между циклами. SDR обновляется раз в 1–2 с, корпус нагревается десятками секунд; для управления по сети (`lanplus`) лучше 30–40 |
| `FAST_POLL_INTERVAL` | 5 | цикл, пока какой-то датчик выше `CRITICAL_TEMP` |
| `CRITICAL_TEMP` | 80 | порог «жарко»: максимум оборотов + ускоренный опрос |
| `FAILSAFE_DUTY` | 85 | если ни один источник не ответил |
| `STARTUP_DUTY` | 35 | при запуске сервиса, до первого решения по датчикам. Не опускает duty, если состояние было горячим |
| `ON_EXIT_DUTY` | — | значение при штатной остановке (SIGTERM). Если не задан — оставить как есть |
| `STATE_FILE` | `/var/lib/autofan/state.json` | куда кладется последний duty для восстановления после рестарта |
| `VERBOSE` | 0 | 1 — писать каждый цикл, даже без изменений. Флаг `-v` важнее, чем `VERBOSE=0` |

## Проверка результата

| ключ | по умолчанию | смысл |
|---|---|---|
| `VERIFY_RPM` | 1 | после установки duty читать тахометры группы |
| `VERIFY_DELAY_SECONDS` | 15 | тахометр приходит в соответствие за 7–10 с |
| `RPM_OFFSET` | 202 | смещение калибровки (у этой платы зависимость не через ноль) |
| `RPM_PER_DUTY` | 17.2 | наклон: `RPM = OFFSET + PER_DUTY × duty` |
| `RPM_WARN_FACTOR` | 0.30 | отклонение больше 30 % от ожидания — предупреждение |

Замеры для этой калибровки и методика пересчёта — `docs/HARDWARE.md`,
`docs/TUNING.md`.

## BMC

| ключ | по умолчанию | смысл |
|---|---|---|
| `BMC_TOOL` | `ipmitool` | путь к бинарнику |
| `BMC_INTERFACE` | `open` | `open` — локально через `/dev/ipmi0`; `lanplus`/`lan` — по сети |
| `BMC_DEVICE` | `/dev/ipmi0` | устройство KCS |
| `BMC_HOST`, `BMC_USER` | — | адрес и пользователь при `lanplus` |
| `BMC_PASSWORD_FILE` | — | файл с паролем (`chmod 600`). Пароль **никогда** не передаётся в аргументах командной строки, иначе его видно в `ps` |
| `BMC_CIPHER` | 17 | шифровой набор для `lanplus` |
| `BMC_PROFILE` | `tyan64` | формат команды: `tyan64` = `raw 0x2e 0x44 0xfd 0x19 0x00 <pwm> 0x01 <duty>` (прошивки 4.11/6.02), `tyan32` = старый `raw 0x2e 0x05 ...` |
| `BMC_TIMEOUT` | 20 | таймаут вызова `ipmitool`, с |
| `BMC_RETRIES` | 2 | повторы при ошибке |

Профиль можно задать и переменной окружения `PROFILE`.

## Источники (`SRCn_`, он же `SOURCEn_`)

Нумерация с 1, до 40. Источник — именованная величина, на которую смотрят группы.

| ключ | смысл |
|---|---|
| `SRCn_NAME` | имя, которым группа ссылается на источник |
| `SRCn_TYPE` | `gpu` \| `bmc` \| `hwmon` |
| `SRCn_SENSOR` | одно имя датчика (для `gpu` — целиком команда, может с аргументами) |
| `SRCn_SENSORS` | список датчиков через пробел |
| `SRCn_AGG` | `max` \| `mean` \| `min` — как свести несколько датчиков в одно число |
| `SRCn_FLOOR` | учитывать источник только выше этой температуры |
| `SRCn_PATH` | шаблон hwmon-файлов (`/sys/class/hwmon/*/temp*_input`) |
| `SRCn_VM` | id виртуальной машины: читать GPU через `qm guest exec` (qemu-guest-agent) |
| `SRCn_OPTIONAL` | 1 — отсутствие данных не считается аварией (выключенная ВМ) |

Зачем `FLOOR` и почему ноль — это отказ, а не холод: `docs/DESIGN.md`.

Рабочий набор для этой платы (`examples/autofan.conf`):

```
SRC1_NAME=gpu  SRC1_TYPE=gpu  SRC1_VM=104  SRC1_AGG=max  SRC1_OPTIONAL=1
SRC2_NAME=cpu_tctl   SRC2_TYPE=bmc  SRC2_SENSOR=CPU_Tctl_Value
SRC3_NAME=air_outlet SRC3_TYPE=bmc  SRC3_SENSOR=SYS_Air_Outlet  SRC3_FLOOR=37
SRC4_NAME=air_inlet  SRC4_TYPE=bmc  SRC4_SENSOR=MB_Air_Inlet    SRC4_FLOOR=42
SRC5_NAME=cpu_mosfet SRC5_TYPE=bmc  SRC5_SENSOR=CPU_MOSFET      SRC5_FLOOR=54
SRC6_NAME=dimm_mosfet SRC6_TYPE=bmc SRC6_SENSORS=DIMM_MOSFET_1 DIMM_MOSFET_2 SRC6_AGG=max SRC6_FLOOR=56
SRC7_NAME=soc_mosfet SRC7_TYPE=bmc  SRC7_SENSOR=CPU_SOC_MOSFET  SRC7_FLOOR=50
SRC8_NAME=dimm_channels SRC8_TYPE=bmc SRC8_AGG=max SRC8_FLOOR=58 \
  SRC8_SENSORS=P0_UMC0_CH_A0 P0_UMC1_CH_B0 P0_UMC2_CH_D0 P0_UMC3_CH_C0 P0_UMC4_CH_H0 P0_UMC5_CH_G0 P0_UMC6_CH_E0 P0_UMC7_CH_F0
SRC9_NAME=nvme SRC9_TYPE=bmc SRC9_SENSORS=M.2/M.3_NVMe_0 M.2/M.3_NVMe_1 SRC9_AGG=max SRC9_FLOOR=50
```

Значения `FLOOR` — из простоя этого железа (47–53 °C на DIMM/MOSFET), под своё
шасси их надо пересчитать: `docs/TUNING.md`.

## Группы вентиляторов (`G1_` … `G6_`)

Каждая группа — свой набор каналов ШИМ, свои источники, своя кривая.

| ключ | пример | смысл |
|---|---|---|
| `Gn_NAME` | `chassis` | имя для журнала и состояния |
| `Gn_PWM` | `2,3,4,5` | каналы ШИМ (см. соответствие разъёмов в `docs/HARDWARE.md`) |
| `Gn_TACH` | `SYS_FAN_1,...` | тахометры для проверки результата |
| `Gn_SOURCES` | `gpu cpu_tctl air_outlet` | имена источников |
| `Gn_AGG` | `max` | как свести разные источники |
| `Gn_CURVE` | `45:35 50:45 60:55 70:70 78:85 85:100` | кривая `ТЕМП:ПРОЦЕНТЫ` |
| `Gn_CURVE_MODE` | `step` | `step` или `linear` |
| `Gn_POWER_CURVE` | `80:40 150:50 220:60 300:75` | опережение по **ваттам** GPU: нижняя граница duty |
| `Gn_POWER_CURVE_MODE` | `step` | режим этой кривой |
| `Gn_MIN_DUTY` | 35 | ниже не опускаемся никогда |
| `Gn_MAX_DUTY` | 100 | потолок |
| `Gn_STARTUP_DUTY` | 35 | при запуске (пусто = общий `STARTUP_DUTY`) |
| `Gn_STEP` | 5 | вниз только шагами не меньше |
| `Gn_DWELL` | 120 | и не чаще раза в столько секунд |
| `Gn_FAILSAFE_DUTY` | 85 | своей группы (пусто = общий) |
| `Gn_CRITICAL_TEMP` | 80 | своей группы (пусто = общий) |
| `Gn_RPM_PER_DUTY` | — | своя калибровка тахометров (пусто = общая) |
| `Gn_PROFILE` | `tyan64` | формат команды для этой группы (пусто = общий) |

Конфиг не проходит, если группа ссылается на несуществующий источник, точка
кривой выходит за `MIN_DUTY..MAX_DUTY` или кривая не разобрана.

## GPU

```
SRC1_NAME=gpu
SRC1_TYPE=gpu
SRC1_VM=104            # через qemu-guest-agent в гостя
SRC1_AGG=max           # максимум из ядро/VRAM
SRC1_OPTIONAL=1        # выключенная ВМ — не авария
```

Свой способ чтения (например, скрипт, печатающий CSV в стиле `nvidia-smi`, или
другой хост):

```
SRC1_SENSOR=/usr/bin/nvidia-smi --query-gpu=index,temperature.gpu,memory.used,power.draw,clocks.sm,utilization.gpu --format=csv,noheader,nounits
```

`SRCn_SENSOR` — это **одна строка целиком**, аргументы входят в значение;
списки — только у `SRCn_SENSORS`.

## YAML-эквивалент

`examples/config.yaml` — тот же набор настроек в YAML: `bmc:`, `sources:`,
`groups:` с `curve: {mode, points: [{temp, duty}]}` и
`power_curve:` той же формы. Ключи YAML соответствуют верхнеуровневым
переменным; смешивать два синтаксиса в одном файле нельзя, формат выбирается по
расширению.

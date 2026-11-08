# autofan — управление корпусными вентиляторами сервера TYAN S8030 через IPMI

[![CI](https://github.com/glfenix/autofan-for-tyanS8030/actions/workflows/ci.yml/badge.svg)](https://github.com/glfenix/autofan-for-tyanS8030/actions/workflows/ci.yml)
[![release v0.5.0](https://img.shields.io/badge/release-v0.5.0-blue)](https://github.com/glfenix/autofan-for-tyanS8030/releases/tag/v0.5.0)
[![license: MIT](https://img.shields.io/badge/license-MIT-green)](LICENSE)

Демон для **TYAN S8030GM2NE** (и родственных плат MiTAC/S8030) с BMC AMI MegaRAC:
плавно крутит корпусные вентиляторы по температуре **нескольких** датчиков,
учитывает мощность GPU как опережающий сигнал и никогда не снижает обороты,
когда данных нет.

Работает на голом хосте (Proxmox / Linux) через локальный `/dev/ipmi0` или по
сети через `lanplus`. GPU может быть проброшена в виртуальную машину — тогда
температура и мощность карты берутся из гостя через qemu-guest-agent.

```
итоговый duty = максимум(
    кривая по температурам,       # 45C=35% 50C=45% 60C=55% 70C=70% 78C=85% 85C=100%
    упреждение по мощности GPU,   # 80Вт=40% 150Вт=50% 220Вт=60% 300Вт=75%
)
вверх — сразу; вниз — не крупнее ШАГА и не чаще раза в ВЫДЕРЖКУ;
при запуске сервиса — стартовое значение (но не ниже того, что было);
если ни один источник не ответил — аварийные 85 %, вниз никогда.
```

## Зачем ещё один форк

Идея и постановка задачи взяты из [sonmihpc/AutoFan](https://github.com/sonmihpc/AutoFan)
(см. `NOTICE`). Собственно код написан с нуля: апстрим годился только как описание
команд BMC, а для реального хоста в нём не было ни удалённого доступа, ни
безопасного отказа, ни гистерезиса.

| Апстрим | Здесь |
|---|---|
| один датчик — температура GPU | до девяти источников (BMC-датчики, GPU, hwmon), агрегаты `max`/`mean`/`min`, порог учёта источника |
| отказ NVML → температура 0 → уходит **минимальный** duty | нет данных → аварийные 85 %; «все живые, но холодные» — это минимум, а не авария |
| только локальный `ipmitool` | `open` через `/dev/ipmi0` **или** `lanplus` (`-H/-U`, пароль из файла `chmod 600`, не из argv) |
| GPU через NVML на том же хосте → на Proxmox с пробросом источников нет | `type=gpu` + `vm=<id>` → чтение в гостя через `qm guest exec`; выключенная ВМ не считается аварией |
| команда `raw 0x2e 0x05 ...`, на прошивке 6.02 отвергается (`rsp=0xc7`) | два профиля команд (`tyan64`/`tyan32`), рабочий для 6.02 подобран экспериментально |
| duty печатался как `5a` → `ipmitool` отвергает | все байты данных форматируются как `0x%02x` (регрессионный тест) |
| нет гистерезиса → дребезг на пороге | шаг + выдержка, асимметрично: вверх сразу, вниз медленно |
| нет проверки результата | чтение тахометров после записи, предупреждение при отклонении >30 % (мёртвый вентилятор, неверный коннектор) |
| обработчик сигналов дедлочится, systemd добивает SIGKILL | корректный SIGTERM с возвратом на `ON_EXIT_DUTY`, `Type=exec`, `TimeoutStopSec=60` |
| одна кривая на всё | несколько групп вентиляторов: свои PWM, свои источники, своя кривая, свой аварийный режим |
| перечитать конфиг нельзя | `systemctl reload` (SIGHUP); битый конфиг не роняет сервис |

## Что нужно, чтобы это заработало

| требование | чем проверить | комментарий |
|---|---|---|
| `ipmitool` | `ipmitool -V` | демон ходит в BMC через него как через внешнюю программу (`BMC_TOOL`), своих IPMI-сокетов здесь нет |
| доступ к BMC — локально **или** по сети | `ls -l /dev/ipmi0` / `nc -zuv <адрес_BMC> 623` | локальный доступ дают модули `ipmi_msghandler`, `ipmi_si`, `ipmi_devintf`: `modprobe ipmi_si ipmi_devintf`. По сети нужен порт 623/udp и включённый `lanplus` в настройках BMC |
| root (или право на `/dev/ipmi0`) | `id -u` | запись ШИМ привилегированному не принадлежит; в systemd unit root по умолчанию |
| Go 1.24+ — **только для сборки** | `go version` | на хосте достаточно одного бинарника и конфига; тулчейн на сервере не нужен (архив `make dist`) |
| `qemu-guest-agent` в госте + `qm` | `qm guest exec <vmid> true` | нужно только для GPU, проброшенной в виртуальную машину (`SRCn_VM=`). Без него карту читают `nvidia-smi` на том же хосте |

Работает на **TYAN S8030GM2NE** (прошивки BMC 4.11 и 6.02) и родственных платах
MiTAC/S8030. Другое железо — это те же переменные, но другие каналы ШИМ,
имена датчиков и своя калибровка тахометров: [docs/TUNING.md](docs/TUNING.md).

## Обязательные переменные

Демон не стартует без нескольких переменных; остальное имеет значения по умолчанию
(полный справочник — [docs/CONFIG.md](docs/CONFIG.md)).

| переменная | когда | что это |
|---|---|---|
| `BMC_INTERFACE` | всегда | `open` — локально через `/dev/ipmi0`; `lanplus` / `lan` — по сети. Другое значение — ошибка конфигурации |
| `BMC_HOST` | при `lanplus`/`lan` | адрес BMC |
| `BMC_USER` | при `lanplus`/`lan` | пользователь BMC с правом `Administrator` (обычно заведён в BIOS Setup → BMC Network / Users) |
| `BMC_PASSWORD_FILE` | при `lanplus`/`lan` | файл с паролем, `chmod 600`. Пароль **никогда** не передаётся аргументом (`-P`), иначе он виден в `ps` всем, кто на хосте смотрит |
| `SRCn_NAME` + `SRCn_TYPE` + `SRCn_SENSOR`/`SRCn_SENSORS` | всегда, ≥1 источник | что мерить: `gpu`, `bmc`, `hwmon` |
| `Gn_NAME` + `Gn_PWM` + `Gn_SOURCES` + `Gn_CURVE` | всегда, ≥1 группа | куда писать ШИМ, по каким источникам решать и с какой кривой |

`autofan -c <файл> probe` разбирает конфиг и прямо говорит, чего не хватает (устройство не
найдено, группа ссылается на неизвестный источник, кривая вне `MIN_DUTY..MAX_DUTY`).

### Пароль: где взять и как не светить

В репозитории ни логинов, ни паролей — файл с паролем живёт только на хосте и в
git не попадает (`.gitignore` бережёт `*.pass`, `secrets/`, `.env`).

```bash
install -d -m 700 /etc/autofan
# read -rs: пароль не попадёт ни в историю bash, ни в ps
read -rs -p 'пароль BMC: ' p && printf '%s\n' "$p" | install -m 600 /dev/stdin /etc/autofan/bmc.pass && unset p

ipmitool -I lanplus -H <адрес_BMC> -U <пользователь> -f /etc/autofan/bmc.pass sdr info   # проверка связности
```

## Быстрый старт: локально, через `/dev/ipmi0`

```bash
apt-get install -y ipmitool                            # на управляющем хосте
git clone https://github.com/glfenix/autofan-for-tyanS8030.git
cd autofan-for-tyanS8030 && make                       # сборка -> ./autofan
sudo install -d -m 755 /etc/autofan
sudo cp -n examples/autofan.conf /etc/autofan/autofan.conf   # первый конфиг (существующий не трогает)
sudo $EDITOR /etc/autofan/autofan.conf                 # свои датчики и кривые: docs/TUNING.md
./autofan -c /etc/autofan/autofan.conf --dry-run probe # читаем датчики, в BMC не пишем
sudo ./install.sh                       # бинарник + конфиг + unit + проверочный probe
sudo ./install.sh --enable              # и включение сервиса
journalctl -u autofan -f
```

`install.sh` не перезаписывает существующий `/etc/autofan/autofan.conf`: рабочие
настройки дороже образца. Сравнить самому — `diff /etc/autofan/autofan.conf examples/autofan.conf`.

## Быстрый старт: по сети, `lanplus` (BMC другой машины или `/dev/ipmi0` занят)

```bash
# 1. файл с паролем — см. выше
# 2. в конфиге вместо open:
#    BMC_INTERFACE=lanplus
#    BMC_HOST=<адрес_BMC>
#    BMC_USER=<пользователь_BMC>
#    BMC_PASSWORD_FILE=/etc/autofan/bmc.pass
#    BMC_CIPHER=17          # если прошивка 17 не принимает — 3
#    POLL_INTERVAL=30        # по сети каждый опрос дороже, 20 с уже впритык
./autofan -c /etc/autofan/autofan.conf --dry-run probe   # конфиг + датчики, без записи
sudo ./autofan -c /etc/autofan/autofan.conf once         # один цикл с реальной записью
sudo ./install.sh --enable
```

## Как запускать

| команда | что делает |
|---|---|
| `autofan` / `autofan run` | демон: цикл раз в `POLL_INTERVAL` секунд, вверх сразу / вниз шагами и с выдержкой |
| `autofan once` | ровно один цикл с реальной записью — самый быстрый способ проверить кривую |
| `autofan probe` | разобрать конфиг, показать датчики, источники и текущий duty; только читает (`--dry-run` — тем более) |
| `autofan status` | сохранённое состояние (`STATE_FILE`) + тахометры: чем крутится сейчас; только читает |
| `autofan set <pwm> <duty>` | вручную, например `set 2 60` — PWM2 = 60 %; пишет в BMC, к группам отношения не имеет |
| `autofan restore` | вернуть группы на `ON_EXIT_DUTY` |

Флаги (`-c`, `-v`, `--dry-run`, `--once`) ставятся **до** команды: Go не
разбирает флаги после позиционного аргумента. Флаг `-v` важнее, чем `VERBOSE=0`
в конфиге. Конфиг по умолчанию — `/etc/autofan/autofan.conf`.

Демоном (то, что ставит `install.sh`):

```bash
systemctl start|stop|restart|status autofan
systemctl reload autofan        # SIGHUP: перечитать конфиг, не останавливая контур
journalctl -u autofan -f        # -u autofan, а не /var/log: логи в journald
systemctl edit autofan          # переопределить переменные без правки основного конфига
```

В unit'е `Type=exec`, `KillSignal=SIGTERM` (штатный выход с возвратом на
`ON_EXIT_DUTY`), `TimeoutStopSec=60`, `Restart=always`: после `SIGKILL` или
падения BMC вентиляторы остаются на последнем duty — понижать их без
достоверных данных демон не умеет в принципе.

## Переменные окружения

Порядок силы: **флаг → переменная окружения → файл конфига**. Поверх файла
накладываются только переменные autofan: `POLL_*`, `BMC_*`, `SRC*`/`SOURCE*`,
`G1_`…`G6_`, `RPM_*`, `STARTUP*`, `STATE_*`, `VERIFY_*`, `FAILSAFE_*`,
`ON_EXIT_*`, `CRITICAL_TEMP`, `PROFILE`, `VERBOSE`, `AUTOFAN_*`. Так можно
попробовать кривую, не трогая рабочий файл:

```bash
sudo -E G1_CURVE='60:50 70:70 78:85' VERBOSE=1 ./autofan -c /etc/autofan/autofan.conf once
```

Постоянно — `EnvironmentFile=/etc/autofan/env` или drop-in
(`systemctl edit autofan` → `[Service]` / `Environment=G1_MIN_DUTY=40`).

## Конфиг

Один файл с обычными `КЛЮЧ=ЗНАЧЕНИЕ` — `/etc/autofan/autofan.conf`, можно
`source` в bash, комментарии работают и в конце строки. Тот же набор настроек
доступен в YAML (`examples/config.yaml`), а любую переменную можно переопределить
из окружения (`EnvironmentFile=` в systemd). Полный справочник ключей —
[docs/CONFIG.md](docs/CONFIG.md), как подбирать значения под своё железо —
[docs/TUNING.md](docs/TUNING.md).

```
G1_NAME=chassis
G1_PWM=2,3,4,5                 # SYS_FAN_1..4
G1_SOURCES=gpu cpu_tctl air_outlet cpu_mosfet dimm_mosfet dimm_channels nvme
G1_STARTUP_DUTY=35             # при запуске сервиса
G1_MIN_DUTY=35                 # ниже не опускаемся никогда
G1_STEP=5                      # вниз только шагами >= 5 %
G1_DWELL=120                   # и не чаще раза в 2 минуты
G1_CURVE=45:35 50:45 60:55 70:70 78:85 85:100
G1_POWER_CURVE=80:40 150:50 220:60 300:75
G1_FAILSAFE_DUTY=85
```

## Почему упреждение по ваттам, а не только по градусам

В нашей сборке выхлоп RTX 5080 идёт вверх, прямо на кулер процессора и зону
VRM/памяти. Карта при этом сама холодная (42 °C под работой — её охлаждение
справляется), а в корпус уходит 200–300 Вт, и датчики «процессорной» зоны
греются именно от неё. Температура ядра карты такой сигнал не передаёт, а
мощность — это и есть тепловыделение. Поэтому она поднимает обороты раньше, чем
датчики корпуса догреются. Когда райзеры PCIe уберут карту из этого воздушного
пути, механизм можно оставить (он про ватты) или обнулить пустой строкой.

## Что проверено на этом железе

- PWM2–5 = `SYS_FAN_1..4`, ручной режим BMC удерживает заданный duty сам (4+ мин).
- Рабочая команда прошивки 6.02: `raw 0x2e 0x44 0xfd 0x19 0x00 <pwm> 0x01 <duty>`;
  старая форма `0x2e 0x05` → `rsp=0xc7`, возврат в авто `0xff` → `rsp=0xcc`
  (поэтому `CPU_FAN` оставляем автоматике BMC).
- Простой: **35 %** и 828 RPM при CPU 37–40 °C, VRM 42–46 °C, DIMM 47–52 °C.
- Нагрузка CPU 48 потоков (AVX2+FMA): `CPU_Tctl` 53 °C — корпусные вентиляторы
  поднимать не нужно, запас большой.
- Короткие всплески инференса (карта 110–145 Вт): контур уходит в 40–50 % и
  возвращается назад, дребезга за 15 минут наблюдения нет.
- Отказ всех источников → **85 %** и 1656 RPM; `SIGTERM` → возврат на 35 %.
- Битый конфиг при `systemctl reload` → ошибка в журнал, сервис продолжает
  работать по прежнему.

Полные таблицы замеров и сырые CSV/логи: [docs/RESULTS.md](docs/RESULTS.md),
`data/`. Свой прогон измерений: `scripts/thermal-sample.py`,
`scripts/dutytest.sh`, `scripts/burn.c`.

## Безопасность

* Аварийный duty срабатывает и при отсутствии данных, и при критической
  температуре; понижение оборотов невозможно без достоверных показаний.
* `STARTUP_DUTY` **не опускает** вентиляторы после горячего цикла — рестарт
  сервиса посреди нагрузки не уложит их в минимум до первого замера.
* Ноль от сломанного источника не считается температурой (иначе несуществующий
  hwmon-путь уложил бы вентиляторы в минимум).
* `ON_EXIT_DUTY` выполняется только при штатном SIGTERM; после `SIGKILL` или
  падения BMC вентиляторы остаются на последнем duty — поэтому минимум 35 %,
  а аварийный 85 %.
* Вернуть заводскую кривую BMC можно только в BIOS Setup
  (`Hardware Monitor → Smart Fan`): программно возврата в авто на этой
  прошивке нет.

## Описание репозитория

```
*.go                    демон (Go, одна зависимость — yaml.v3) + тесты рядом
  main.go               флаги, команды (run|once|probe|status|set|restore), сигналы
  config.go             парсер KEY=VALUE и YAML, переменные окружения, валидация
  sensors.go            чтение BMC-датчиков, nvidia-smi/NVML, hwmon, qemu-guest-agent
  control.go            кривые, упреждение по ваттам, гистерезис, аварийный режим
  bmc.go                профили команд IPMI, кодирование байт, тахометры
  state.go              /var/lib/autofan/state.json: последний duty для рестартов
examples/               рабочая конфигурация (KEY=VALUE), YAML-аналог, systemd unit
docs/                   HARDWARE · DESIGN · CONFIG · TUNING · RESULTS · ROADMAP · PUBLISH
scripts/                измерительные скрипты и тестовая нагрузка (burn.c, membw_stream.c)
data/                   сырые замеры (CSV + лог прогона 2026-10-30)
install.sh              установка на хост: бинарник, конфиг, unit, проверочный probe
Makefile                make (=release-check) / test / dist / deploy / module
.github/workflows/      CI: gofmt, vet, тесты, сборка, синтаксис скриптов
```

Вопрос → куда смотреть:

| вопрос | файл |
|---|---|
| «что за переменная и что будет, если её поменять» | [docs/CONFIG.md](docs/CONFIG.md) |
| «как подобрать кривую/FLOOR/калибровку под своё шасси» | [docs/TUNING.md](docs/TUNING.md) |
| «почему вверх сразу, а вниз медленно / почему ноль — не холод» | [docs/DESIGN.md](docs/DESIGN.md) |
| «какие команды принимает мой BMC, какие разъёмы чем являются» | [docs/HARDWARE.md](docs/HARDWARE.md) |
| «какие температуры и обороты получились на самом деле» | [docs/RESULTS.md](docs/RESULTS.md), `data/` |
| «что дальше и что здесь спорно» | [docs/ROADMAP.md](docs/ROADMAP.md) |
| «как это публиковать и что с лицензией» | [docs/PUBLISH.md](docs/PUBLISH.md) |

Собрать и проверить себя: `make` (он же `make release-check`: gofmt, `go vet`,
тесты, сборка). Тесты: `make test` (27 тестов — парсер конфига, кодирование
IPMI-команд, кривые, гистерезис, аварийные режимы, калибровка тахометров); они
не требуют железа, поэтому прогоняются и в CI: `.github/workflows/ci.yml`.
Архив для хоста без тулчейна Go: `make dist` → `dist/autofan-<версия>.tgz`
(`install.sh` внутри). Обновления по схеме: ветка → `make release-check` → PR →
merge → `git tag v0.x.y`.

## Лицензия

MIT (см. `LICENSE`) — кроме кода, которого здесь нет: исходный проект
[sonmihpc/AutoFan](https://github.com/sonmihpc/AutoFan) не имеет файла лицензии,
поэтому он использован только как описание задачи и команд BMC, а весь код этого
репозитория написан заново. Подробности — `NOTICE`.

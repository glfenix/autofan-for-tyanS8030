#!/usr/bin/env python3
"""Сводка по CSV от thermal-sample.py: как вели себя датчики при разной нагрузке GPU.

  python3 summarize.py sample.csv
  python3 summarize.py sample.csv --split 100      # границы «нагрузка» по ваттам

Считает минимум/максимум/среднее по колонкам отдельно для простоя и для
нагрузки, плюс медиану - то, чем разбирали замеры при настройке кривой.
"""
import csv
import statistics as st
import sys

split = 100
args = []
it = iter(sys.argv[1:])
for a in it:
    if a.startswith("--split"):
        split = float(a.split("=", 1)[1]) if "=" in a else float(next(it))
    else:
        args.append(a)
path = args[0] if args else "sample.csv"

rows = list(csv.DictReader(open(path)))


def num(r, k):
    v = (r.get(k) or "").strip()
    try:
        return float(v)
    except ValueError:
        return None


def report(rs, label):
    if not rs:
        print(f"{label}: нет строк")
        return
    print(f"\n{label}: строк {len(rs)}")
    for k in rs[0].keys():
        if k == "time":
            continue
        vals = [v for v in (num(r, k) for r in rs) if v is not None]
        if not vals:
            continue
        print(f"  {k:9s} мин {min(vals):7.0f}  макс {max(vals):7.0f}  "
              f"медиана {st.median(vals):7.1f}  среднее {st.mean(vals):7.1f}")


w = [num(r, "gpuW") for r in rows if num(r, "gpuW") is not None]
if w:
    report([r for r in rows if (num(r, "gpuW") or 0) < split], f"простой (<{split:.0f} Вт)")
    report([r for r in rows if (num(r, "gpuW") or 0) >= split], f"нагрузка (>={split:.0f} Вт)")
    report(rows, "всё")
else:
    report(rows, "всё (колонки gpuW нет)")

duty = [num(r, "duty") for r in rows if num(r, "duty") is not None]
if duty:
    print(f"\nduty: значения {sorted(set(int(d) for d in duty))}, "
          f"смен в ряду {sum(1 for a, b in zip(duty, duty[1:]) if a != b)}")
print("\nМного смен duty при малом разбросе температур = дребезг: подними STEP/DWELL")

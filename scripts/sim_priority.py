import json, sys, urllib.request, time, datetime

ED = {"cn": "https://www.codebuddy.cn", "intl": "https://www.workbuddy.ai"}
UA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"
FIELDS = ("DeductionEndTime", "ExpiredTime", "CycleEndTime")


def ts(raw):
    if raw is None:
        return None
    s = str(raw).strip().strip('"')
    if s in ("", "null", "0"):
        return None
    try:
        n = int(float(s))
    except ValueError:
        # "2026-11-30 23:59:59" 按 UTC+8
        return datetime.datetime.strptime(s, "%Y-%m-%d %H:%M:%S").replace(
            tzinfo=datetime.timezone(datetime.timedelta(hours=8))).timestamp()
    return n / 1000 if n > 1e12 else n


def grants(cfg, now):
    ed = cfg.get("edition") or "cn"
    base = ED[ed]
    body = json.dumps({
        "PageNumber": 1, "PageSize": 200, "ProductCode": "p_tcaca",
        "Status": [0, 3], "OnlyValidPeriod": True,
        "SlicePeriodStartTime": "2026-11-12 00:00:00",
        "SlicePeriodEndTime": "2026-11-12 23:59:59",
    }).encode()
    req = urllib.request.Request(base + "/billing/meter/get-user-resource", data=body, headers={
        "Authorization": "Bearer " + cfg["auth"]["accessToken"],
        "Content-Type": "application/json", "Accept": "application/json, text/plain, */*",
        "X-Requested-With": "XMLHttpRequest", "Origin": base, "Referer": base + "/", "User-Agent": UA})
    d = json.loads(urllib.request.urlopen(req, timeout=20).read())
    rows = (((d.get("data") or {}).get("Response") or {}).get("Data") or {}).get("Accounts") or []
    remain, expire, n = 0.0, None, 0
    for r in rows:
        try:
            st = int(str(r.get("Status")).strip('"'))
        except ValueError:
            continue
        if st != 0:
            continue
        try:
            rem = float(str(r.get("CapacityRemain")).strip('"'))
        except ValueError:
            continue
        if rem <= 0:
            continue
        ends = [ts(r.get(f)) for f in FIELDS]
        ends = [e for e in ends if e]
        e = min(ends) if ends else None
        if e and e <= now:
            continue
        remain += rem
        n += 1
        if e and (expire is None or e < expire):
            expire = e
    return remain, expire, n


if __name__ == "__main__":
    now = time.time()
    rows = []
    for p in sys.argv[1:]:
        cfg = json.load(open(p))
        r, e, n = grants(cfg, now)
        rows.append((p, r, e, n))

    def fmt(ts_):
        return datetime.datetime.fromtimestamp(ts_).strftime("%m-%d %H:%M") if ts_ else "未知"

    print("%-22s %8s %6s %14s %10s" % ("账号", "剩余", "包数", "最近到期", "权重/h"))
    tot = 0.0
    for p, r, e, n in rows:
        h = ((e - now) / 3600) if e else 720
        h = max(h, 1)
        w = r / h
        tot += w
        print("%-24s %8.0f %6d %14s %10.2f" % (p, r, n, fmt(e), w))
    print()
    print("%-24s %8s %14s %10s" % ("账号", "份额", "燃尽预计", "按份额速率"))
    for p, r, e, n in rows:
        h = ((e - now) / 3600) if e else 720
        h = max(h, 1)
        w = r / h
        share = w / tot if tot else 0
        # 若总运力 = 各号按时燃尽所需，则按份额分配后该号燃尽耗时
        need = sum(x[1] / max(((x[2] - now) / 3600) if x[2] else 720, 1) for x in rows)
        rate = share * need
        print("%-24s %7.1f%% %10.1fh  %8.2f/h" % (p, share * 100, (r / rate) if rate else 0, rate))

import json, sys, urllib.request

ED = {"cn": "https://www.codebuddy.cn", "intl": "https://www.workbuddy.ai"}
UA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"


def fetch(path):
    cfg = json.load(open(path))
    ed = cfg.get("edition") or "cn"
    base = ED[ed]
    tok = cfg["auth"]["accessToken"]
    body = json.dumps({
        "PageNumber": 1, "PageSize": 200, "ProductCode": "p_tcaca",
        "Status": [0, 3], "OnlyValidPeriod": True,
        "SlicePeriodStartTime": "2026-11-12 00:00:00",
        "SlicePeriodEndTime": "2026-11-12 23:59:59",
    }).encode()
    req = urllib.request.Request(base + "/billing/meter/get-user-resource", data=body, headers={
        "Authorization": "Bearer " + tok,
        "Content-Type": "application/json",
        "Accept": "application/json, text/plain, */*",
        "X-Requested-With": "XMLHttpRequest",
        "Origin": base, "Referer": base + "/", "User-Agent": UA,
    })
    d = json.loads(urllib.request.urlopen(req, timeout=20).read())
    return (((d.get("data") or {}).get("Response") or {}).get("Data") or {}).get("Accounts") or []


for path in sys.argv[1:]:
    rows = fetch(path)
    print(f"### {path} rows={len(rows)}")
    for r in rows:
        print("  %-30s %-46s st=%s size=%-8s remain=%-8s end=%s cycleEnd=%s" % (
            r.get("PackageCode"), r.get("SubProductCode"), r.get("Status"),
            r.get("CapacitySize"), r.get("CapacityRemain"),
            r.get("DeductionEndTime"), r.get("CycleEndTime")))

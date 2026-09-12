#!/usr/bin/env python3
"""Tổng hợp kết quả kiểm thử tải từ ramp.jsonl và probe.jsonl để đánh giá hiệu năng hệ thống.

Chỉ số cốt lõi cần xác định là C (thời gian CPU tiêu thụ trung bình cho mỗi request, tính bằng mili-giây).
Từ chỉ số C và giới hạn CPU quota được cấp phát, ta có thể ước tính thông lượng tối đa:
    RPS_max = cpu_quota_cores / (C / 1000)

Cách chạy:
    python3 report.py --ramp loadtest-ramp.jsonl --probe probe.jsonl
"""

from __future__ import annotations

import argparse
import json
import statistics
from pathlib import Path
from typing import Any

# Các ngưỡng nhận diện điểm nghẽn hệ thống (knee point / saturation point).
# Khi một bước kiểm thử vượt qua một trong các ngưỡng này, bước đó sẽ bị đánh dấu là quá tải (KNEE)
# và loại khỏi tập dữ liệu dùng để tính trung vị chỉ số C:
#   - KNEE_ACHIEVED_RATIO: Tỷ lệ RPS hoàn thành so với RPS yêu cầu (< 90% nghĩa là hàng đợi bắt đầu ứ đọng).
#   - KNEE_ACCEPT_QUEUE: Số lượng kết nối chờ xử lý trong TCP accept queue (>= 1 nghĩa là worker không kịp accept).
#   - KNEE_HEALTH_MS: Độ trễ phản hồi health check (> 1000ms là dấu hiệu event loop bị tắc nghẽn nghiêm trọng).
#   - KNEE_THROTTLE_PCT: Tỷ lệ chu kỳ CPU bị bóp nghẽn bởi CFS quota (> 5% nghĩa là đã chạm trần quota được cấp).
KNEE_ACHIEVED_RATIO = 0.90  # [Tạo tải] Throughput thực tế đạt được < 90% mức yêu cầu
KNEE_ACCEPT_QUEUE = 1  # [Hệ điều hành] Hàng đợi kết nối accept queue > 0 kéo dài
KNEE_HEALTH_MS = 1000.0  # [Event loop] Độ trễ endpoint /health/live vượt quá 1 giây
KNEE_THROTTLE_PCT = 5.0  # [cgroup] Tỷ lệ chu kỳ CPU bị bóp nghẽn vượt quá 5%

# Lưu ý: Ngưỡng KNEE_ACHIEVED_RATIO (0.90) cần đồng bộ với ngưỡng kiểm tra trong ramp.py (--stop-on-knee).


def _load(path: str) -> list[dict[str, Any]]:
    """Đọc file JSONL và trả về danh sách các dictionary tương ứng với từng dòng dữ liệu."""
    rows = []
    for line in Path(path).read_text("utf-8").splitlines():
        line = line.strip()
        if line:
            rows.append(json.loads(line))
    return rows


def _pct(xs: list[float], p: float) -> float | None:
    """Tính phân vị thứ p theo phương pháp nearest-rank (chọn phần tử thực tế trong danh sách)."""
    if not xs:
        return None
    s = sorted(xs)
    k = min(len(s) - 1, max(0, round((p / 100.0) * (len(s) - 1))))
    return s[k]


def _f(v: float | None, nd: int = 0) -> str:
    """Định dạng số thực hiển thị trên bảng kết quả. Trả về '-' nếu không có dữ liệu (None)."""
    return "-" if v is None else f"{v:,.{nd}f}"


def _windows(
    ramp: list[dict[str, Any]], step_seconds: float | None
) -> list[dict[str, Any]]:
    """Phân tách các bản ghi từ ramp.jsonl thành các khung thời gian tương ứng với từng mức RPS."""
    out, cur = [], None
    for r in ramp:
        if r.get("type") == "step_start":
            cur = {"rps": r["rps"], "t0": r["t"], "reqs": []}
        elif r.get("type") == "step_end" and cur is not None:
            cur["t1"] = r["t"]
            # Thời gian phát tải của bước kiểm thử (dùng làm mẫu số tính throughput thực tế)
            cur["launch_s"] = step_seconds if step_seconds else cur["t1"] - cur["t0"]
            out.append(cur)
            cur = None
    # Gán các request vào bước kiểm thử dựa theo thời điểm bắt đầu gửi (t_start)
    for r in ramp:
        if r.get("type") != "req" or "t_start" not in r:
            continue
        for w in out:
            if w["t0"] <= r["t_start"] <= w["t1"]:
                w["reqs"].append(r)
                break
    return out


def _probe_slice(
    probe: list[dict[str, Any]], t0: float, t1: float
) -> list[dict[str, Any]]:
    """Lọc các mẫu dữ liệu probe rơi vào khoảng thời gian [t0, t1] (bỏ qua bản ghi metadata)."""
    return [
        s for s in probe if s.get("type") != "probe_meta" and t0 <= s.get("t", 0) <= t1
    ]


def _delta(samples: list[dict[str, Any]], key: str) -> float | None:
    """Tính độ chênh lệch (giá trị cuối trừ giá trị đầu) của một bộ đếm tích lũy trong khoảng thời gian đo."""
    vals: list[float] = [float(s[key]) for s in samples if s.get(key) is not None]
    if len(vals) < 2:
        return None
    return vals[-1] - vals[0]


def _baseline_cpu(
    probe: list[dict[str, Any]], first_t0: float
) -> tuple[float | None, int, float]:
    """Tính mức tiêu thụ CPU nền khi chưa có tải: trả về (số core, số mẫu, độ dài khoảng thời gian đo tính bằng giây)."""
    pre = [
        s for s in probe if s.get("type") != "probe_meta" and s.get("t", 0) < first_t0
    ]
    if len(pre) < 3:
        return None, len(pre), 0.0
    span_s: float = float(pre[-1]["t"]) - float(pre[0]["t"])
    d = _delta(pre, "usage_usec")
    if not d or span_s <= 0:
        return None, len(pre), max(0.0, span_s)
    return d / (span_s * 1_000_000), len(pre), span_s


def main() -> int:
    """Đọc và tổng hợp dữ liệu từ ramp.jsonl và probe.jsonl, phân tích và in bảng kết quả đánh giá hiệu năng."""
    ap = argparse.ArgumentParser(description="Dựng bảng kết quả calibrate")
    ap.add_argument("--ramp", required=True)
    ap.add_argument("--probe", default="")
    args = ap.parse_args()

    ramp = _load(args.ramp)
    probe = _load(args.probe) if args.probe else []
    meta = next((r for r in probe if r.get("type") == "probe_meta"), {})
    quota = meta.get("cpu_quota_cores")

    run_meta = next((r for r in ramp if r.get("type") == "run_meta"), {})
    wins = _windows(ramp, run_meta.get("step_seconds"))
    if not wins:
        print("Không tìm thấy bậc nào trong file ramp.")
        return 1

    baseline, base_n, base_span = (
        _baseline_cpu(probe, wins[0]["t0"]) if probe else (None, 0, 0.0)
    )

    print("=" * 132)
    print("CALIBRATE - KẾT QUẢ")
    print("=" * 132)
    if meta:
        print(f"cpu_quota_cores : {quota if quota is not None else 'không giới hạn'}")
        print(
            f"nproc nhìn thấy : {meta.get('nproc_visible')} (core của NODE, không phải quota)"
        )
        print(f"executor threads: {meta.get('default_executor_max_workers')} / worker")
    if baseline is not None:
        print(
            f"CPU nền (không tải)    : {baseline:.3f} core ({base_n} mẫu / {base_span:.0f}s)"
        )
        # Cảnh báo nếu khoảng thời gian đo CPU nền ngắn hơn 90 giây
        if base_span < 90:
            print(
                f"  CẢNH BÁO: cửa sổ nền chỉ {base_span:.0f}s. Các tác vụ nền (background sync/cron) "
                "thường có chu kỳ 30-45s nên\n  cửa sổ ngắn làm baseline nhảy giữa các lần chạy. "
                "Lần sau chờ 90-120s trước khi bắn tải."
            )
    elif probe:
        print(
            f"CPU nền                : KHÔNG đo được ({base_n} mẫu trước bậc đầu, cần >= 3)"
        )
        print(
            "  -> probe bật muộn hơn ramp? C dưới đây chưa trừ nền nên bị thổi phồng."
        )
    print()

    # Ý nghĩa các cột trong bảng kết quả:
    #   offer   : Mức RPS yêu cầu phát ra từ bộ tạo tải
    #   achiev  : Mức RPS thực tế hoàn thành thành công
    #   ok/err  : Số lượt request thành công / thất bại trong bước kiểm thử
    #   ttftP50 : Độ trễ nhận phản hồi đầu tiên ở phân vị 50 (trung vị, ms)
    #   ttftP95 : Độ trễ nhận phản hồi đầu tiên ở phân vị 95 (ms)
    #   cores   : Mức CPU thực tế container đã sử dụng (quy đổi ra số core)
    #   C_ms    : Thời gian CPU tiêu thụ cho mỗi request thành công (ms), đã khấu trừ CPU nền
    #   thr%    : Tỷ lệ phần trăm chu kỳ CFS bị bóp nghẽn CPU do chạm quota
    #   acptQ   : Số lượng kết nối chờ xử lý lớn nhất trong TCP accept queue
    #   hlthP99 : Độ trễ endpoint health check ở phân vị 99 (ms)
    #   estab   : Số lượng kết nối TCP ESTABLISHED lớn nhất quan sát được
    #   fdMax   : Số file descriptor lớn nhất của một tiến trình worker
    #   thrd    : Số thread lớn nhất của một tiến trình worker
    #   verdict : Đánh giá trạng thái: OK hoặc KNEE kèm các lý do quá tải
    cols = (
        f"{'offer':>6} {'achiev':>7} {'ok':>5} {'err':>4} {'ttftP50':>8} {'ttftP95':>8} "
        f"{'cores':>6} {'C_ms':>7} {'thr%':>6} {'acptQ':>6} {'hlthP99':>8} {'estab':>6} "
        f"{'fdMax':>6} {'thrd':>5}  verdict"
    )
    print(cols)
    print("-" * 132)

    rows: list[dict[str, Any]] = []
    noisy: list[
        float
    ] = []  # Danh sách mức RPS không tách biệt được mức tăng CPU so với CPU nền
    for w in wins:
        dur = max(
            w["launch_s"], 1e-9
        )  # Cửa sổ phát tải của bước này (không bao gồm thời gian drain)
        reqs = w["reqs"]
        ok = [r for r in reqs if r.get("ok")]
        ttfts = [r["ttft_ms"] for r in ok if r.get("ttft_ms") is not None]
        s = _probe_slice(probe, w["t0"], w["t1"])

        cores = c_ms = thr_pct = None
        acceptq = health_p99 = estab = fdmax = thrd = None
        if s:
            span_usec = (s[-1]["t"] - s[0]["t"]) * 1_000_000
            d_usage = _delta(s, "usage_usec")
            if d_usage is not None and span_usec > 0:
                cores = d_usage / span_usec
                net = d_usage - (baseline * span_usec if baseline else 0.0)
                # Không tính C nếu mức sử dụng CPU không cao hơn mức tiêu thụ nền
                if ok and net > 0:
                    c_ms = net / 1000.0 / len(ok)
                elif ok:
                    noisy.append(w["rps"])
            d_thr = _delta(s, "nr_throttled")
            d_per = _delta(s, "nr_periods")
            if d_thr is not None and d_per:
                thr_pct = 100.0 * d_thr / d_per
            acceptq = max((x.get("tcp", {}).get("accept_queue", 0) or 0) for x in s)
            health_p99 = _pct(
                [x["health_ms"] for x in s if x.get("health_ms") is not None], 99
            )
            estab = max(
                (x.get("tcp", {}).get("states", {}).get("ESTAB", 0) or 0) for x in s
            )
            fdmax = max((x.get("workers", {}).get("fds_max") or 0) for x in s)
            thrd = max((x.get("workers", {}).get("threads_max") or 0) for x in s)

        achieved = len(ok) / dur
        # Đánh giá các điều kiện quá tải (knee) từ nhiều nguồn dữ liệu độc lập
        reasons = []
        if achieved < KNEE_ACHIEVED_RATIO * w["rps"]:
            reasons.append("achieved<offered")
        if acceptq is not None and acceptq >= KNEE_ACCEPT_QUEUE:
            reasons.append("acceptQ>0")
        if health_p99 is not None and health_p99 > KNEE_HEALTH_MS:
            reasons.append("health chậm")
        if thr_pct is not None and thr_pct > KNEE_THROTTLE_PCT:
            reasons.append("throttled")
        if any(r.get("err") == "abandoned" for r in reqs):
            reasons.append("abandoned")
        verdict = "OK" if not reasons else "KNEE: " + ", ".join(reasons)

        rows.append(
            {
                "rps": w["rps"],
                "achieved": achieved,
                "c_ms": c_ms,
                "ok": bool(not reasons),
            }
        )
        print(
            f"{w['rps']:>6.1f} {achieved:>7.2f} {len(ok):>5d} {len(reqs) - len(ok):>4d} "
            f"{_f(_pct(ttfts, 50)):>8} {_f(_pct(ttfts, 95)):>8} "
            f"{_f(cores, 2):>6} {_f(c_ms, 1):>7} {_f(thr_pct, 1):>6} {_f(acceptq):>6} "
            f"{_f(health_p99):>8} {_f(estab):>6} {_f(fdmax):>6} {_f(thrd):>5}  {verdict}"
        )

    print("-" * 132)
    healthy = [r for r in rows if r["ok"] and r["c_ms"]]
    knees = [r for r in rows if not r["ok"]]
    print()
    # Ước tính công suất hệ thống từ chỉ số C và CPU quota
    print("KẾT LUẬN")
    if healthy:
        c = statistics.median([r["c_ms"] for r in healthy])
        print(f"  C (trung vị các bậc còn khoẻ)   : {c:.1f} ms CPU / request")
        print(f"  Trần lý thuyết 1 worker (1 core) : {1000.0 / c:.1f} req/s")
        if quota:
            print(
                f"  Trần lý thuyết pod ({quota:g} core)      : {quota * 1000.0 / c:.1f} req/s"
            )
            print(
                f"  Số worker hợp lý                 : {max(1, round(quota))} (= quota)"
            )
    elif not any(r["ok"] for r in rows):
        print("  Chưa có bậc nào 'OK' -> hạ dải --steps xuống rồi đo lại.")
    else:
        print(
            "  Có bậc còn khoẻ nhưng KHÔNG tính được C -> thiếu --probe, hoặc probe không"
        )
        print("  phủ cùng khoảng thời gian với ramp (xem cột cores/C_ms trống).")
    if noisy:
        print(
            f"  Không đo được C ở bậc {noisy}: CPU lúc tải không cao hơn CPU nền. Nguyên nhân\n"
            "  thường gặp: cgroup chứa cả tiến trình khác ngoài app, hoặc bậc RPS quá nhẹ."
        )
    if knees:
        first = knees[0]
        print(f"  Điểm gãy quan sát được           : {first['rps']:g} req/s offered")
        last_ok = [r for r in rows if r["ok"]]
        if last_ok:
            print(
                f"  Bậc cuối còn khoẻ                : {last_ok[-1]['rps']:g} req/s -> SLO"
            )
    else:
        print("  Chưa chạm điểm gãy - nâng dải --steps lên và đo tiếp.")
    print()
    print(
        "  Chênh giữa trần lý thuyết và điểm gãy quan sát được là phần bị mất vì GIL,"
    )
    print(
        "  CFS throttling và tranh chấp thread. Chênh lớn -> xem cột thr% và thrd trước tiên."
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

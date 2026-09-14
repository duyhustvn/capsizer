#!/usr/bin/env python3
"""Open-Loop Load Testing Runner Engine.

Bộ điều phối phát tải dạng open-loop theo từng mức RPS.
Được tách biệt hoàn toàn khỏi logic của từng API (được đặt trong thư mục `scenarios/`).

Cách thức hoạt động:
  - Runner Engine (`ramp.py`) quản lý:
      1. Điều phối nhịp độ phát request (open-loop interval: 1/RPS).
      2. Kiểm soát trần kết nối in-flight và thu hồi request (settle/drain).
      3. Quản lý các bậc RPS (stepping) và thời gian nghỉ (cooldown).
      4. Ghi log chuẩn hóa dạng JSONL để report.py phân tích.
  - Logic của từng API cụ thể được hiện thực trong `scenarios/`:
      + `scenarios/chat_sse.py`: Bài test Chatbot streaming SSE (đo TTFT, xoay vòng user pool).
      + `scenarios/rest.py`: Bài test API REST thông thường (GET, POST, PUT,...).
      + File tùy biến bất kỳ thông qua '--scenario-file'.
"""

from __future__ import annotations

import argparse
import asyncio
import importlib.util
import json
import sys
import time
from pathlib import Path
from typing import Any, TextIO

import httpx

# Nạp giao diện kịch bản và danh mục các kịch bản có sẵn
from scenarios import REGISTRY, ChatSSEScenario, RestScenario, Scenario

__all__ = ["Scenario", "ChatSSEScenario", "RestScenario", "main", "main_async"]


def load_custom_scenario(file_path: str) -> type[Scenario]:
    """Nạp động một custom Scenario class từ một file Python bên ngoài."""
    path = Path(file_path).resolve()
    if not path.is_file():
        raise FileNotFoundError(f"Không tìm thấy file kịch bản: {path}")

    spec = importlib.util.spec_from_file_location("custom_scenario_module", path)
    if not spec or not spec.loader:
        raise ImportError(f"Không thể nạp module từ {path}")
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)

    for attr in dir(mod):
        val = getattr(mod, attr)
        if isinstance(val, type) and val.__name__ != "Scenario":
            mro_names = [c.__name__ for c in val.__mro__]
            if "Scenario" in mro_names and hasattr(val, "execute"):
                return val
    raise ValueError(f"Không tìm thấy class nào kế thừa từ Scenario trong {path}")


def build_scenario(args: argparse.Namespace) -> Scenario:
    """Khởi tạo đối tượng Scenario dựa trên tham số dòng lệnh."""
    # 1. Custom scenario từ file Python bên ngoài
    if args.scenario_file:
        cls = load_custom_scenario(args.scenario_file)
        print(f"Nạp kịch bản tùy biến: {cls.__name__} từ {args.scenario_file}")
        return cls()  # type: ignore

    # Xây dựng danh sách header chung
    headers: dict[str, str] = {}
    if args.header:
        for h in args.header:
            if ":" in h:
                k, v = h.split(":", 1)
                headers[k.strip()] = v.strip()

    # 2. Kịch bản REST API thông thường
    if args.scenario == "rest":
        return RestScenario.from_args(args, headers)

    # 3. Kịch bản Chatbot SSE mặc định
    return ChatSSEScenario.from_args(args, headers)


# ==============================================================================
# CORE OPEN-LOOP RUNNER ENGINE
# ==============================================================================


def _pct(xs: list[float], p: float) -> float | None:
    """Tính giá trị phân vị thứ p theo phương pháp nearest-rank."""
    if not xs:
        return None
    s = sorted(xs)
    k = min(len(s) - 1, max(0, round((p / 100.0) * (len(s) - 1))))
    return s[k]


def _fmt(v: float | None, nd: int = 0) -> str:
    """Định dạng số hiển thị trên bảng kết quả, trả về '-' nếu giá trị là None."""
    return "-" if v is None else f"{v:.{nd}f}"


async def _run_step(
    client: httpx.AsyncClient,
    args: argparse.Namespace,
    scenario: Scenario,
    rps: float,
    out: TextIO,
) -> dict[str, Any]:
    """Thực thi một bước kiểm thử tải ở mức RPS chỉ định trong khoảng thời gian quy định.

    Cơ chế open-loop: phát sinh request bất đồng bộ theo lịch trình cố định mà không
    chờ request trước đó hoàn tất.
    """
    loop = asyncio.get_running_loop()
    interval = 1.0 / rps
    inflight: set[asyncio.Task[dict[str, Any]]] = set()
    records: list[dict[str, Any]] = []
    dropped = 0
    max_inflight_seen = 0

    def _harvest(task: asyncio.Task[dict[str, Any]]) -> None:
        inflight.discard(task)
        try:
            records.append(task.result())
        except asyncio.CancelledError:
            records.append(
                {"type": "req", "ok": False, "err": "abandoned", "total_ms": None}
            )

    t_step_start = time.time()
    out.write(json.dumps({"type": "step_start", "rps": rps, "t": t_step_start}) + "\n")
    out.flush()

    start = loop.time()
    n = 0
    while True:
        target = start + n * interval
        if target - start >= args.step_seconds:
            break
        delay = target - loop.time()
        if delay > 0:
            await asyncio.sleep(delay)
        n += 1

        if len(inflight) >= args.max_inflight:
            dropped += 1
            continue

        # Ủy quyền cho Scenario thực thi request
        task = loop.create_task(scenario.execute(client))
        inflight.add(task)
        task.add_done_callback(_harvest)
        max_inflight_seen = max(max_inflight_seen, len(inflight))

    # Chờ thu hồi các request còn đang xử lý cuối bước (drain/settle)
    if inflight:
        _, pending = await asyncio.wait(set(inflight), timeout=args.settle)
        for t in pending:
            t.cancel()
        if pending:
            await asyncio.wait(pending, timeout=5)

    t_step_end = time.time()
    for r in records:
        out.write(json.dumps(r) + "\n")
    out.write(json.dumps({"type": "step_end", "rps": rps, "t": t_step_end}) + "\n")
    out.flush()

    ok = [r for r in records if r.get("ok")]
    metrics = [r["ttft_ms"] for r in ok if r.get("ttft_ms") is not None]
    return {
        "rps_offered": rps,
        "sent": n - dropped,
        "dropped_by_generator": dropped,
        "completed_ok": len(ok),
        "errors": len(records) - len(ok),
        "rps_achieved": len(ok) / max(args.step_seconds, 1e-9),
        "launch_window_s": args.step_seconds,
        "drain_s": max(0.0, (t_step_end - t_step_start) - args.step_seconds),
        "p50": _pct(metrics, 50),
        "p95": _pct(metrics, 95),
        "ttft_p50": _pct(metrics, 50),  # Tương thích ngược
        "ttft_p95": _pct(metrics, 95),  # Tương thích ngược
        "max_inflight": max_inflight_seen,
    }


async def main_async(
    args: argparse.Namespace,
    scenario: Scenario,
    steps: list[float],
    out: TextIO,
) -> int:
    """Vòng lặp điều phối chính: chạy tuần tự các bậc RPS và in tiến độ."""
    cap = args.max_inflight + 64
    limits = httpx.Limits(max_connections=cap, max_keepalive_connections=cap)

    p50_lbl = f"{scenario.metric_label}_p50"
    p95_lbl = f"{scenario.metric_label}_p95"
    hdr = (
        f"{'offered':>8} {'achieved':>9} {'ok':>6} {'err':>5} {'drop':>5} "
        f"{p50_lbl:>9} {p95_lbl:>9} {'inflight':>9}"
    )
    print(hdr)
    print("-" * len(hdr))

    meta = {
        "type": "run_meta",
        "url": args.url,
        "scenario": scenario.name,
        "steps": steps,
        "step_seconds": args.step_seconds,
    }
    out.write(json.dumps(meta) + "\n")

    async with httpx.AsyncClient(
        limits=limits, http2=False, verify=not args.insecure
    ) as client:
        await scenario.setup(client)
        for rps in steps:
            s = await _run_step(client, args, scenario, rps, out)
            print(
                f"{s['rps_offered']:>8.1f} {s['rps_achieved']:>9.2f} {s['completed_ok']:>6d} "
                f"{s['errors']:>5d} {s['dropped_by_generator']:>5d} "
                f"{_fmt(s['p50']):>9} {_fmt(s['p95']):>9} {s['max_inflight']:>9d}"
            )
            if args.stop_on_knee and s["rps_achieved"] < 0.90 * s["rps_offered"]:
                print("\n-> achieved < 90% offered: đã qua điểm gãy, dừng ramp.")
                break
            if rps != steps[-1] and args.cooldown > 0:
                await asyncio.sleep(args.cooldown)

    print("\nXong. Ghép với probe rồi dựng bảng:")
    print(f"  python3 report.py --ramp {args.out} --probe <probe.jsonl>")
    return 0


def main() -> int:
    p = argparse.ArgumentParser(
        description="Open-Loop RPS Load Testing Runner (hỗ trợ Chat SSE, REST API, Custom Scenario)"
    )
    # Tùy chọn kịch bản
    p.add_argument(
        "--scenario",
        default="chat-sse",
        choices=list(REGISTRY.keys()),
        help="loại kịch bản: 'chat-sse' (mặc định) hoặc 'rest'",
    )
    p.add_argument(
        "--scenario-file",
        default="",
        help="file Python chứa custom Scenario class kế thừa từ Scenario",
    )

    # Cấu hình kịch bản REST
    p.add_argument(
        "--method",
        default="GET",
        help="HTTP method cho kịch bản REST (GET, POST, PUT, DELETE,...) [mặc định: GET]",
    )
    p.add_argument(
        "-H",
        "--header",
        action="append",
        help="thêm HTTP header tùy chọn (ví dụ: -H 'X-Api-Key: 123' -H 'Accept: text/plain')",
    )
    p.add_argument(
        "--body",
        default="",
        help="chuỗi body gửi kèm cho request REST (chuỗi JSON hoặc chuỗi text thô)",
    )
    p.add_argument(
        "--body-file",
        default="",
        help="đường dẫn file chứa dữ liệu body cho request REST",
    )

    # Cấu hình bài test tải chung
    p.add_argument("--url", default="http://127.0.0.1:8000/chat")
    p.add_argument(
        "--steps", default="1,2,4,8,16,32", help="danh sách RPS, cách nhau dấu phẩy"
    )
    p.add_argument(
        "--step-seconds", type=float, default=60.0, help="độ dài mỗi bậc (giây)"
    )
    p.add_argument(
        "--cooldown", type=float, default=20.0, help="nghỉ giữa hai bậc (giây)"
    )
    p.add_argument(
        "--settle", type=float, default=60.0, help="chờ tối đa cho in-flight cuối bậc"
    )
    p.add_argument(
        "--timeout", type=float, default=120.0, help="read timeout mỗi request (giây)"
    )
    p.add_argument(
        "--max-inflight", type=int, default=2000, help="trần in-flight của MÁY BẮN TẢI"
    )

    # Cấu hình xác thực và dữ liệu Chatbot
    p.add_argument(
        "--users-csv",
        default="",
        help="CSV pool user (chứa cột token/user_token). Mặc định dùng users.csv nếu tồn tại.",
    )
    p.add_argument(
        "--user-token",
        default="loadtest-token",
        help="token dùng chung khi KHÔNG có --users-csv",
    )
    p.add_argument("--jwt", default="", help="JWT dựng sẵn")
    p.add_argument(
        "--jwt-secret",
        default="",
        help="secret ký token HS256; mặc định đọc biến môi trường JWT_SECRET",
    )
    p.add_argument(
        "--queries", default="", help="file câu hỏi cho chatbot, mỗi dòng một câu"
    )
    p.add_argument(
        "--insecure",
        action="store_true",
        help="bỏ qua verify chứng chỉ TLS (khi qua Ingress cert tự ký)",
    )
    p.add_argument(
        "--cache-bust",
        action="store_true",
        help="thêm mã ngẫu nhiên vào câu hỏi để tránh cache",
    )
    p.add_argument(
        "--stop-on-knee", action="store_true", help="dừng khi achieved < 90%% offered"
    )
    p.add_argument(
        "--env-file",
        default=".env",
        help="đường dẫn file .env nạp cấu hình (mặc định: .env)",
    )
    p.add_argument("--out", default="loadtest-ramp.jsonl")
    args = p.parse_args()

    # Nạp biến môi trường từ .env nếu có
    import os
    from scenarios.chat_sse import load_dotenv

    load_dotenv(args.env_file)

    # Đọc JWT_SECRET từ môi trường nếu chưa truyền
    if not args.jwt_secret:
        args.jwt_secret = os.environ.get("JWT_SECRET", "")

    steps = [float(s) for s in args.steps.split(",") if s.strip()]
    if not steps:
        print("ERROR: --steps rỗng", file=sys.stderr)
        return 2

    try:
        scenario = build_scenario(args)
    except Exception as exc:
        print(f"ERROR khởi tạo kịch bản: {exc}", file=sys.stderr)
        return 2

    out_path = Path(args.out)
    out_path.parent.mkdir(parents=True, exist_ok=True)
    print(f"target   : {args.url}")
    print(f"scenario : {scenario.name}")
    print(f"steps    : {steps} x {args.step_seconds}s (settle {args.settle}s)")
    print(f"out      : {out_path}")

    try:
        with out_path.open("w", encoding="utf-8") as out:
            return asyncio.run(main_async(args, scenario, steps, out))
    except KeyboardInterrupt:
        return 130


if __name__ == "__main__":
    raise SystemExit(main())

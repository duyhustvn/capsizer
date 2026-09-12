#!/usr/bin/env python3
"""Thu thập chỉ số hệ thống (CPU, bộ nhớ, TCP socket, tiến trình) và kiểm tra độ trễ health check.

Script chỉ sử dụng thư viện chuẩn của Python, chạy trực tiếp trong container/pod mà không cần
cài đặt thêm các công cụ bên ngoài như `ss` hay `iproute2`.

Dữ liệu thu thập gồm 4 nhóm chỉ số chính:
  1. Giới hạn CPU quota thực tế từ cgroup (hỗ trợ cả cgroup v1 và v2).
  2. Thống kê CFS throttling (nr_throttled, throttled_usec) để phát hiện tình trạng bị nghẽn CPU.
  3. Hàng đợi kết nối TCP (accept queue) và phân bố trạng thái socket của cổng dịch vụ.
  4. Độ trễ phản hồi của endpoint /health/live để đánh giá mức độ bận của event loop.

Cách chạy:
  - Chạy bên trong pod:
      python3 probe.py --out /tmp/probe.jsonl
  - Chạy từ bên ngoài pod (chỉ đo độ trễ health check qua mạng):
      python3 probe.py --target http://<POD_IP>:8000 --no-cgroup --out /tmp/probe-outside.jsonl
"""

from __future__ import annotations

import argparse
import json
import os
import time
import urllib.error
import urllib.request
from pathlib import Path
from typing import Any

# Bảng tra cứu mã trạng thái TCP trong file /proc/net/tcp (giá trị hex ở cột 'st').
TCP_STATES = {
    "01": "ESTAB",
    "02": "SYN_SENT",
    "03": "SYN_RECV",
    "04": "FIN_WAIT1",
    "05": "FIN_WAIT2",
    "06": "TIME_WAIT",
    "07": "CLOSE",
    "08": "CLOSE_WAIT",
    "09": "LAST_ACK",
    "0A": "LISTEN",
    "0B": "CLOSING",
}
CG2 = Path("/sys/fs/cgroup")


def _read(path: str | Path) -> str | None:
    """Đọc toàn bộ nội dung file văn bản, trả về None nếu xảy ra lỗi (file không tồn tại, thiếu quyền,...).

    Các file ảo trong /proc và /sys có thể không tồn tại hoặc thay đổi trạng thái bất ngờ,
    do đó hàm này chặn ngoại lệ OSError để đảm bảo tiến trình probe hoạt động liên tục.
    """
    try:
        return Path(path).read_text("utf-8")
    except OSError:
        return None


def cpu_quota_cores() -> float | None:
    """Tính số CPU core được cấp phát cho container từ cgroup quota. Trả về None nếu không bị giới hạn.

    Thử đọc từ cgroup v2 (cpu.max định dạng '<quota> <period>') trước,
    nếu không có thì đọc từ cgroup v1 (cpu.cfs_quota_us và cpu.cfs_period_us).
    """
    v2 = _read(CG2 / "cpu.max")
    if v2:
        parts = v2.split()
        if parts[0] == "max":
            return None
        return int(parts[0]) / int(parts[1])
    quota = _read(CG2 / "cpu/cpu.cfs_quota_us")
    period = _read(CG2 / "cpu/cpu.cfs_period_us")
    if quota and period:
        q = int(quota.strip())
        return None if q < 0 else q / int(period.strip())
    return None


def cpu_sample() -> dict[str, Any]:
    """Lấy mẫu các chỉ số sử dụng CPU từ cgroup: usage_usec, nr_periods, nr_throttled, throttled_usec.

    Các chỉ số này đều là bộ đếm tích lũy (cumulative counters) tính từ lúc khởi động container:
      - usage_usec: Tổng thời gian CPU đã tiêu thụ (tính theo micro giây).
      - nr_periods: Tổng số chu kỳ CFS đã trôi qua (mặc định 100ms mỗi chu kỳ).
      - nr_throttled: Số chu kỳ bị bóp nghẽn CPU (throttled) do vượt quá quota.
      - throttled_usec: Tổng thời gian bị nghẽn CPU (micro giây).

    Giá trị thực tế trong một khoảng thời gian được tính bằng hiệu số (delta) giữa hai lần lấy mẫu.
    """
    keys = ("usage_usec", "nr_periods", "nr_throttled", "throttled_usec")
    out: dict[str, Any] = dict.fromkeys(keys)
    v2 = _read(CG2 / "cpu.stat")
    if v2 and "usage_usec" in v2:
        for line in v2.splitlines():
            k, _, v = line.partition(" ")
            if k in out:
                out[k] = int(v)
        return out
    acct = _read(CG2 / "cpuacct/cpuacct.usage")  # cgroup v1: tính bằng nanogiây, chia 1000 về microgiây
    if acct:
        out["usage_usec"] = int(acct.strip()) // 1000
    v1 = _read(CG2 / "cpu/cpu.stat")
    if v1:
        for line in v1.splitlines():
            k, _, v = line.partition(" ")
            if k == "nr_periods":
                out["nr_periods"] = int(v)
            elif k == "nr_throttled":
                out["nr_throttled"] = int(v)
            elif k == "throttled_time":  # cgroup v1: tính bằng nanogiây, chia 1000 về microgiây
                out["throttled_usec"] = int(v) // 1000
    return out


def mem_bytes() -> int | None:
    """Lấy dung lượng bộ nhớ hiện tại đang sử dụng của cgroup theo byte (bao gồm RSS và page cache).

    Chỉ số tức thời (gauge) tại thời điểm đo. Đọc từ file 'memory.current' (cgroup v2)
    hoặc 'memory.usage_in_bytes' (cgroup v1).
    """
    for p in (CG2 / "memory.current", CG2 / "memory/memory.usage_in_bytes"):
        v = _read(p)
        if v:
            try:
                return int(v.strip())
            except ValueError:
                pass
    return None


def _parse_proc_net_tcp(path: str, port: int) -> tuple[dict[str, int], dict[str, int]]:
    """Phân tích file /proc/net/tcp hoặc tcp6 để đếm số socket theo trạng thái và lấy thông tin hàng đợi lắng nghe.

    Đối với socket ở trạng thái LISTEN:
      - rx_queue (sk_ack_backlog): Số lượng kết nối đã bắt tay thành công nhưng đang chờ ứng dụng gọi accept().
      - tx_queue (sk_max_ack_backlog): Kích thước hàng đợi backlog tối đa được cấu hình.
    """
    counts: dict[str, int] = {}
    listen: dict[str, int] = {}
    text = _read(path)
    if not text:
        return counts, listen
    for line in text.splitlines()[1:]:
        f = line.split()
        if len(f) < 5:
            continue
        try:
            lport = int(f[1].rsplit(":", 1)[1], 16)
            rport = int(f[2].rsplit(":", 1)[1], 16)
        except (IndexError, ValueError):
            continue
        if lport != port and rport != port:
            continue
        st = TCP_STATES.get(f[3].upper(), f[3])
        # Phân biệt chiều kết nối: nếu cổng đích khớp với 'port' thì đây là kết nối ra ngoài (outbound).
        key = st if lport == port else f"{st}_out"
        counts[key] = counts.get(key, 0) + 1
        if st == "LISTEN" and lport == port:
            tx, _, rx = f[4].partition(":")
            listen["accept_queue"] = int(rx, 16)
            listen["backlog_max"] = int(tx, 16)
    return counts, listen


def tcp_sample(port: int) -> dict[str, Any]:
    """Tổng hợp trạng thái TCP trên cả IPv4 và IPv6 cho cổng dịch vụ được chỉ định.

    Kết quả trả về gồm:
      - states: Số lượng socket phân loại theo từng trạng thái (ESTAB, TIME_WAIT, SYN_RECV,...).
      - accept_queue: Số kết nối đang chờ trong hàng đợi accept.
      - backlog_max: Giới hạn tối đa của hàng đợi backlog.
    """
    counts: dict[str, int] = {}
    listen: dict[str, int] = {}
    for p in ("/proc/net/tcp", "/proc/net/tcp6"):
        c, ls = _parse_proc_net_tcp(p, port)
        for k, v in c.items():
            counts[k] = counts.get(k, 0) + v
        listen.update(ls)
    return {"states": counts, **listen}


def worker_sample() -> dict[str, Any]:
    """Thu thập thông tin về tài nguyên của các tiến trình uvicorn (PID, file descriptor, thread, CPU ticks).

    Thông tin thu thập bao gồm:
      - n_procs: Số lượng tiến trình uvicorn đang chạy (tiến trình master và các worker).
      - fds_total: Tổng số file descriptor đang mở trên tất cả các tiến trình.
      - fds_max: Số file descriptor lớn nhất của một tiến trình đơn lẻ.
      - threads_max: Số lượng OS thread lớn nhất trong một tiến trình worker.
      - procs: Danh sách chi tiết từng tiến trình kèm CPU ticks (utime + stime).
    """
    procs: list[dict[str, Any]] = []
    fd_counts: list[int] = []
    thread_counts: list[int] = []
    for entry in os.listdir("/proc"):
        if not entry.isdigit():
            continue
        cmd = _read(f"/proc/{entry}/cmdline")
        if not cmd or "uvicorn" not in cmd.replace("\x00", " "):
            continue
        try:
            fds = len(os.listdir(f"/proc/{entry}/fd"))
            fd_counts.append(fds)
        except OSError:
            fds = -1
        threads: int | None = None
        status = _read(f"/proc/{entry}/status")
        if status:
            for line in status.splitlines():
                if line.startswith("Threads:"):
                    threads = int(line.split()[1])
                    thread_counts.append(threads)
                    break
        ticks = None
        stat = _read(f"/proc/{entry}/stat")
        if stat:
            # Trường comm nằm trong ngoặc và có thể chứa khoảng trắng -> cắt từ ")" cuối cùng.
            tail = stat[stat.rfind(")") + 2 :].split()
            if len(tail) > 12:
                ticks = int(tail[11]) + int(tail[12])  # utime + stime (CPU ticks)
        procs.append({"pid": int(entry), "fds": fds, "threads": threads, "cpu_ticks": ticks})
    procs.sort(key=lambda p: p["pid"])
    return {
        "n_procs": len(procs),
        "fds_total": sum(fd_counts),
        "fds_max": max(fd_counts, default=None),
        "threads_max": max(thread_counts, default=None),
        "procs": procs,
    }


def health_sample(url: str, timeout: float) -> dict[str, Any]:
    """Đo độ trễ phản hồi (mili-giây) của endpoint kiểm tra trạng thái dịch vụ (/health/live).

    Trả về:
      - health_ms: Thời gian hoàn thành một request kiểm tra health check.
      - health_code: Mã trạng thái HTTP trả về (ví dụ: 200), hoặc None nếu kết nối thất bại/hết thời gian chờ.
    """
    t0 = time.perf_counter()
    try:
        with urllib.request.urlopen(url, timeout=timeout) as r:
            r.read()
            code = r.status
    except urllib.error.HTTPError as e:
        code = e.code
    except Exception:
        return {"health_ms": (time.perf_counter() - t0) * 1000, "health_code": None}
    return {"health_ms": (time.perf_counter() - t0) * 1000, "health_code": code}


def snapshot(args: argparse.Namespace) -> dict[str, Any]:
    """Lấy mẫu toàn bộ chỉ số tại thời điểm hiện tại và trả về bản ghi dạng dictionary.

    Trường 't' lưu dấu thời gian epoch (time.time) để đồng bộ với dữ liệu kiểm thử tải.
    Tùy chọn '--no-cgroup' sẽ bỏ qua các chỉ số cgroup/hệ thống, chỉ giữ lại phép đo health check.
    """
    s: dict[str, Any] = {"t": time.time()}
    if not args.no_cgroup:
        s.update(cpu_sample())
        s["mem_bytes"] = mem_bytes()
        s["tcp"] = tcp_sample(args.port)
        s["workers"] = worker_sample()
    s.update(health_sample(args.target.rstrip("/") + "/health/live", args.health_timeout))
    return s


def main() -> int:
    """Hàm thực thi chính: ghi bản ghi metadata ban đầu và định kỳ lấy mẫu theo tham số '--interval'.

    Dòng đầu tiên (type='probe_meta') chứa các thông số môi trường cố định:
      - cpu_quota_cores: Giới hạn CPU được cấu hình cho container.
      - nproc_visible: Số lượng core CPU nhìn thấy từ hệ điều hành máy chủ (node).
      - default_executor_max_workers: Giới hạn thread pool mặc định của asyncio.
    """
    p = argparse.ArgumentParser(description="Lấy mẫu cgroup/TCP/health trong lúc bắn tải")
    p.add_argument("--out", default="/tmp/probe.jsonl")
    p.add_argument("--interval", type=float, default=1.0)
    p.add_argument("--port", type=int, default=8000)
    p.add_argument("--target", default="http://127.0.0.1:8000")
    p.add_argument("--health-timeout", type=float, default=5.0)
    p.add_argument("--no-cgroup", action="store_true", help="chạy ngoài pod: chỉ đo health")
    p.add_argument("--once", action="store_true", help="in một snapshot rồi thoát")
    args = p.parse_args()

    quota = cpu_quota_cores()
    meta: dict[str, Any] = {
        "type": "probe_meta",
        "t": time.time(),
        "cpu_quota_cores": quota,
        "nproc_visible": os.cpu_count(),
        "default_executor_max_workers": min(32, (os.cpu_count() or 1) + 4),
    }

    if args.once:
        print(json.dumps(meta, indent=2, ensure_ascii=False))
        print(json.dumps(snapshot(args), indent=2, ensure_ascii=False))
        return 0

    out = Path(args.out)
    out.parent.mkdir(parents=True, exist_ok=True)
    print(f"probe -> {out} (interval {args.interval}s). Ctrl-C để dừng.")
    print(f"cpu_quota_cores={quota}  nproc_visible={meta['nproc_visible']}")
    if quota is not None and meta["nproc_visible"] and meta["nproc_visible"] > quota * 2:
        print(
            f"CẢNH BÁO: nproc={meta['nproc_visible']} nhưng quota chỉ {quota} core. Mọi thư viện "
            "sizing theo os.cpu_count() (kể cả default executor của asyncio) đang phình quá quota."
        )
    with out.open("w", encoding="utf-8") as f:
        f.write(json.dumps(meta, ensure_ascii=False) + "\n")
        f.flush()
        next_t = time.monotonic()
        try:
            while True:
                next_t += args.interval
                f.write(json.dumps(snapshot(args), ensure_ascii=False) + "\n")
                f.flush()
                sleep = next_t - time.monotonic()
                if sleep > 0:
                    time.sleep(sleep)
                else:
                    next_t = time.monotonic()  # Chậm nhịp lấy mẫu -> đặt lại mốc thời gian tiếp theo
        except KeyboardInterrupt:
            print("\ndừng probe.")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

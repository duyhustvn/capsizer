from __future__ import annotations

import argparse
import csv
import os
import random
import sys
import time
from pathlib import Path
from typing import Any

import httpx

from .base import Scenario

def find_default_users_csv() -> Path:
    """Tìm đường dẫn mặc định đến users.csv (ưu tiên thư mục hiện tại, rồi thư mục gốc repo)."""
    candidates = [
        Path("users.csv"),
        Path.cwd() / "users.csv",
        Path(__file__).resolve().parents[2] / "users.csv",
        Path(__file__).resolve().parents[3] / "users.csv",
        Path(__file__).resolve().parent.parent / "users.csv",
    ]
    return next((c for c in candidates if c.is_file()), Path("users.csv"))


# Đường dẫn mặc định đến file CSV chứa thông tin người dùng và token (nằm ở thư mục gốc của project)
DEFAULT_USERS_CSV = find_default_users_csv()

# Danh sách câu hỏi kiểm thử mẫu chung cho chatbot (dự phòng khi không cấu hình .env)
DEFAULT_QUERIES = [
    "Xin chào, bạn có thể hỗ trợ những tác vụ gì?",
    "Hãy tóm tắt ngắn gọn các điểm chính của tài liệu này",
    "Hướng dẫn tôi cách tích hợp API vào hệ thống",
    "Giải thích giúp tôi nguyên lý hoạt động của kiến trúc microservices",
    "Gợi ý cho tôi một số giải pháp tối ưu hóa hiệu năng",
    "Chào bạn",
]


def load_dotenv(path: str | Path | None = None) -> dict[str, str]:
    """Tìm và nạp các biến môi trường từ file .env vào os.environ nếu chưa tồn tại."""
    candidates: list[Path] = []
    if path:
        p = Path(path)
        candidates.append(p)
        if not p.is_absolute():
            candidates.extend([
                Path.cwd() / p,
                Path(__file__).resolve().parent.parent / p,
                Path(__file__).resolve().parents[2] / p,
            ])
    else:
        candidates.extend([
            Path(".env"),
            Path.cwd() / ".env",
            Path(__file__).resolve().parent.parent / ".env",
            Path(__file__).resolve().parents[2] / ".env",
        ])

    target = next((c for c in candidates if c.is_file()), None)
    if not target:
        return {}

    env_vars: dict[str, str] = {}
    content = target.read_text("utf-8")
    lines = content.splitlines()
    i = 0
    while i < len(lines):
        line = lines[i].strip()
        i += 1
        if not line or line.startswith("#") or "=" not in line:
            continue
        k, v = line.split("=", 1)
        k = k.strip()
        v = v.strip()

        # Xử lý chuỗi multiline nằm trong dấu nháy kép hoặc đơn
        if (v.startswith('"') and not (v.endswith('"') and len(v) > 1)) or (
            v.startswith("'") and not (v.endswith("'") and len(v) > 1)
        ):
            quote_char = v[0]
            val_parts = [v[1:]]
            while i < len(lines):
                next_line = lines[i]
                i += 1
                if next_line.rstrip().endswith(quote_char):
                    val_parts.append(next_line.rstrip()[:-1])
                    break
                val_parts.append(next_line)
            v = "\n".join(val_parts)
        elif (v.startswith('"') and v.endswith('"')) or (v.startswith("'") and v.endswith("'")):
            v = v[1:-1]

        v = v.replace("\\n", "\n")
        env_vars[k] = v
        if k not in os.environ:
            os.environ[k] = v

    return env_vars


def parse_queries(raw: str) -> list[str]:
    """Phân tích chuỗi câu hỏi từ .env thành danh sách (hỗ trợ JSON array, xuống dòng, | hoặc ;)."""
    raw = raw.strip()
    if not raw:
        return []
    # 1. JSON array
    if raw.startswith("[") and raw.endswith("]"):
        try:
            import json

            data = json.loads(raw)
            if isinstance(data, list):
                res = [str(x).strip() for x in data if str(x).strip()]
                if res:
                    return res
        except Exception:
            pass
    # 2. Xuống dòng
    if "\n" in raw:
        lines = [ln.strip() for ln in raw.split("\n") if ln.strip()]
        if lines:
            return lines
    # 3. Ký tự phân cách pipe (|)
    if "|" in raw:
        lines = [ln.strip() for ln in raw.split("|") if ln.strip()]
        if lines:
            return lines
    # 4. Ký tự phân cách semicolon (;)
    if ";" in raw:
        lines = [ln.strip() for ln in raw.split(";") if ln.strip()]
        if lines:
            return lines
    return [raw]


def resolve_queries(args: argparse.Namespace) -> list[str]:
    """Xác định danh sách câu hỏi kiểm thử theo thứ tự ưu tiên:
    1. Cờ dòng lệnh --queries (file câu hỏi riêng biệt)
    2. Biến CHAT_QUERIES_FILE hoặc QUERIES_FILE trong .env
    3. Biến CHAT_QUERIES hoặc QUERIES trong .env (phân tách bởi |, xuống dòng, hoặc JSON array)
    4. Danh sách câu hỏi mẫu mặc định DEFAULT_QUERIES
    """
    env_file = getattr(args, "env_file", ".env")
    load_dotenv(env_file)

    # 1. Tham số dòng lệnh --queries
    if getattr(args, "queries", None):
        p = Path(args.queries)
        if not p.is_file():
            raise FileNotFoundError(f"Không tìm thấy file --queries: {args.queries}")
        lines = [ln.strip() for ln in p.read_text("utf-8").splitlines() if ln.strip()]
        if not lines:
            raise ValueError(f"File --queries rỗng: {args.queries}")
        print(f"queries  : {len(lines)} câu hỏi nạp từ cờ --queries ({p.name})")
        return lines

    # 2. File được trỏ tới từ biến môi trường CHAT_QUERIES_FILE / QUERIES_FILE
    queries_file = os.environ.get("CHAT_QUERIES_FILE") or os.environ.get("QUERIES_FILE")
    if queries_file:
        candidates = [
            Path(queries_file),
            Path.cwd() / queries_file,
            Path(__file__).resolve().parents[2] / queries_file,
        ]
        target = next((c for c in candidates if c.is_file()), None)
        if target:
            lines = [ln.strip() for ln in target.read_text("utf-8").splitlines() if ln.strip()]
            if lines:
                print(f"queries  : {len(lines)} câu hỏi nạp từ file {target.name} (qua .env)")
                return lines

    # 3. Chuỗi danh sách câu hỏi trực tiếp trong CHAT_QUERIES / QUERIES
    raw_env = os.environ.get("CHAT_QUERIES") or os.environ.get("QUERIES")
    if raw_env:
        parsed = parse_queries(raw_env)
        if parsed:
            print(f"queries  : {len(parsed)} câu hỏi nạp từ .env (CHAT_QUERIES)")
            return parsed

    # 4. Dự phòng mặc định
    print("queries  : dùng danh sách câu hỏi mẫu mặc định (chưa cấu hình CHAT_QUERIES trong .env)")
    return DEFAULT_QUERIES


def _mint_jwt(secret: str, ttl_s: int = 7200) -> str:
    """Tạo JWT nội bộ (server-to-server) để gửi trong header Authorization Bearer."""
    import jwt

    return jwt.encode(
        {"roles": ["super_admin"], "exp": int(time.time()) + ttl_s},
        secret,
        algorithm="HS256",
    )


class UserPool:
    """Quản lý danh sách người dùng và token xoay vòng theo từng request."""

    def __init__(self, users: list[tuple[str, str]]) -> None:
        self._users = users
        self._i = 0  # Con trỏ xoay vòng

    def __len__(self) -> int:
        return len(self._users)

    def next(self) -> tuple[str, str]:
        """Lấy thông tin (user_id, token) tiếp theo theo cơ chế xoay vòng (round-robin)."""
        user = self._users[self._i % len(self._users)]
        self._i += 1
        return user


def load_users(path: Path) -> list[tuple[str, str]]:
    """Đọc file CSV danh sách người dùng và trả về danh sách tuple (user_id, token).

    Hỗ trợ linh hoạt các tên cột phổ biến (không phân biệt chữ hoa/thường):
      - Cột token: 'token', 'user_token', 'access_token', 'api_key'
      - Cột user id: 'user_id', 'userid', 'id', 'user'
    """
    with path.open("r", encoding="utf-8", newline="") as f:
        reader = csv.DictReader(f)
        if not reader.fieldnames:
            raise ValueError(f"{path}: file rỗng, không có header")
        cols = {(name or "").strip().lower(): name for name in reader.fieldnames}

        # Tự động nhận diện cột token và user_id
        token_key = next(
            (k for k in ("token", "user_token", "access_token", "api_key") if k in cols),
            None,
        )
        if not token_key:
            raise ValueError(
                f"{path}: không tìm thấy cột chứa token. Yêu cầu có cột 'token' hoặc 'user_token'."
            )
        token_col = cols[token_key]

        user_key = next((k for k in ("user_id", "userid", "id", "user") if k in cols), None)
        user_col = cols[user_key] if user_key else None

        users: list[tuple[str, str]] = []
        for idx, row in enumerate(reader, 1):
            token = (row.get(token_col) or "").strip()
            if not token:
                continue
            uid = (row.get(user_col) or str(idx)).strip() if user_col else str(idx)
            users.append((uid, token))
    if not users:
        raise ValueError(f"{path}: không có dòng nào hợp lệ (mọi dòng đều thiếu token)")
    return users


def _payload(query: str, user_token: str) -> dict[str, Any]:
    """Tạo payload JSON gửi đến endpoint /chat mô phỏng một phiên trò chuyện thực tế."""
    return {
        "user_token": user_token,
        "summary": "",
        "history": [
            {"role": "user", "text": "Xin chào"},
            {
                "role": "assistant",
                "text": "Chào bạn, mình là chatbot. Bạn cần hỗ trợ gì?",
            },
            {"role": "user", "text": query},
        ],
    }


class ChatSSEScenario(Scenario):
    """Kịch bản kiểm thử API Chatbot hỗ trợ streaming SSE (endpoint POST /chat)."""

    name = "chat-sse"
    metric_label = "ttft"

    def __init__(
        self,
        url: str,
        headers: dict[str, str],
        timeout: float,
        pool: UserPool,
        queries: list[str],
        cache_bust: bool = False,
    ) -> None:
        self.url = url
        self.headers = headers
        self.timeout = timeout
        self.pool = pool
        self.queries = queries
        self.cache_bust = cache_bust

    @classmethod
    def from_args(
        cls, args: argparse.Namespace, headers: dict[str, str]
    ) -> ChatSSEScenario:
        """Khởi tạo ChatSSEScenario từ các tham số dòng lệnh."""
        hdrs = dict(headers)
        if "Content-Type" not in hdrs:
            hdrs["Content-Type"] = "application/json"

        token = args.jwt or (_mint_jwt(args.jwt_secret) if args.jwt_secret else "")
        if token and "Authorization" not in hdrs:
            hdrs["Authorization"] = f"Bearer {token}"

        queries = resolve_queries(args)

        # Xác định file users_csv (truyền qua cờ hoặc tự động nạp users.csv nếu tồn tại)
        users_csv: Path | None = None
        if getattr(args, "users_csv", None):
            p = Path(args.users_csv)
            if p.is_file():
                users_csv = p
            else:
                for cand in (
                    Path.cwd() / p,
                    Path(__file__).resolve().parents[2] / p,
                    Path(__file__).resolve().parents[3] / p,
                    Path(__file__).resolve().parent.parent / p,
                ):
                    if cand.is_file():
                        users_csv = cand
                        break
                if not users_csv:
                    print(
                        f"CẢNH BÁO: không thấy {args.users_csv}, dùng --user-token.",
                        file=sys.stderr,
                    )
        else:
            def_csv = find_default_users_csv()
            if def_csv.is_file():
                users_csv = def_csv

        if users_csv and users_csv.is_file():
            pool = UserPool(load_users(users_csv))
            print(f"pool     : {len(pool)} user từ {users_csv.name}")
        else:
            pool = UserPool([("", args.user_token)])
            print("pool     : 1 token dùng chung (--user-token)")

        # Hàng rào kiểm tra Rate-limit của tài khoản: len(pool) / max(steps) < 120s
        max_step = 0.0
        steps_str = getattr(args, "steps", "")
        if steps_str:
            try:
                max_step = max(float(s) for s in steps_str.split(",") if s.strip())
            except ValueError:
                pass

        if max_step > 0:
            cycle_s = len(pool) / max_step
            if cycle_s < 120.0:
                if len(pool) == 1:
                    print(
                        f"CẢNH BÁO: Đang dùng 1 token duy nhất cho đỉnh tải {max_step:g} req/s. "
                        "Nguy cơ chạm rate-limit của tài khoản thay vì đo công suất hệ thống.",
                        file=sys.stderr,
                    )
                else:
                    print(
                        f"CẢNH BÁO: Pool chỉ có {len(pool)} user cho đỉnh tải {max_step:g} req/s "
                        f"(chu kỳ lặp lại {cycle_s:.1f}s < 120s). "
                        "Nguy cơ chạm rate-limit của tài khoản thay vì đo công suất hệ thống.",
                        file=sys.stderr,
                    )

        return cls(
            url=args.url,
            headers=hdrs,
            timeout=args.timeout,
            pool=pool,
            queries=queries,
            cache_bust=args.cache_bust,
        )

    async def execute(self, client: httpx.AsyncClient) -> dict[str, Any]:
        """Gửi 1 request streaming SSE tới endpoint /chat và đo TTFT."""
        q = random.choice(self.queries)
        if self.cache_bust:
            q = f"{q} (ma tham chieu {random.randint(100000, 999999)})"
        uid, token = self.pool.next()
        body = _payload(q, token)

        t0 = time.perf_counter()
        rec: dict[str, Any] = {
            "type": "req",
            "t_start": time.time(),
            "user_id": uid,
            "ttft_ms": None,
            "notice_ms": None,
            "total_ms": None,
            "status": None,
            "ok": False,
            "err": None,
            "end_seen": False,
        }
        try:
            async with client.stream(
                "POST", self.url, json=body, headers=self.headers, timeout=self.timeout
            ) as resp:
                rec["status"] = resp.status_code
                if resp.status_code != 200:
                    await resp.aread()
                    rec["err"] = f"http_{resp.status_code}"
                    rec["total_ms"] = (time.perf_counter() - t0) * 1000
                    return rec

                event = ""
                async for line in resp.aiter_lines():
                    if line.startswith("event:"):
                        event = line[6:].strip()
                    elif line.startswith("data:"):
                        now = (time.perf_counter() - t0) * 1000
                        if event == "message" and rec["ttft_ms"] is None:
                            rec["ttft_ms"] = now
                        elif event == "notice" and rec["notice_ms"] is None:
                            rec["notice_ms"] = now
                        elif event == "message_end":
                            rec["end_seen"] = True
                        elif event == "error":
                            rec["err"] = "sse_error"

                rec["ok"] = rec["end_seen"] and rec["err"] is None
        except Exception as exc:
            rec["err"] = type(exc).__name__
        rec["total_ms"] = (time.perf_counter() - t0) * 1000
        return rec

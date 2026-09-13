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

# Đường dẫn mặc định đến file CSV chứa thông tin người dùng và token (nằm ở thư mục gốc của project)
DEFAULT_USERS_CSV = Path(__file__).resolve().parent.parent / "users.csv"

# Danh sách câu hỏi kiểm thử mẫu chung cho chatbot
DEFAULT_QUERIES = [
    "Xin chào, bạn có thể hỗ trợ những tác vụ gì?",
    "Hãy tóm tắt ngắn gọn các điểm chính của tài liệu này",
    "Hướng dẫn tôi cách tích hợp API vào hệ thống",
    "Giải thích giúp tôi nguyên lý hoạt động của kiến trúc microservices",
    "Gợi ý cho tôi một số giải pháp tối ưu hóa hiệu năng",
    "Chào bạn",
]


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

        queries = DEFAULT_QUERIES
        if args.queries:
            lines = Path(args.queries).read_text("utf-8").splitlines()
            queries = [ln.strip() for ln in lines if ln.strip()]
            if not queries:
                raise ValueError("File --queries rỗng")

        users_csv = Path(args.users_csv) if args.users_csv else None
        if users_csv and users_csv.is_file():
            pool = UserPool(load_users(users_csv))
            print(f"pool     : {len(pool)} user từ {users_csv.name}")
        else:
            pool = UserPool([("", args.user_token)])
            print("pool     : 1 token dùng chung (--user-token)")
            if args.users_csv and args.users_csv != str(DEFAULT_USERS_CSV):
                print(
                    f"CẢNH BÁO: không thấy {users_csv}, dùng --user-token.",
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

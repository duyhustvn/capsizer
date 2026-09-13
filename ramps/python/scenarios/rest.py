from __future__ import annotations

import argparse
import json
import time
from pathlib import Path
from typing import Any

import httpx

from .base import Scenario


class RestScenario(Scenario):
    """Kịch bản kiểm thử API REST thông thường (GET, POST, PUT, DELETE,...)."""

    name = "rest"
    metric_label = "lat"

    def __init__(
        self,
        url: str,
        method: str = "GET",
        headers: dict[str, str] | None = None,
        timeout: float = 30.0,
        raw_body: str | None = None,
        json_body: Any | None = None,
    ) -> None:
        self.url = url
        self.method = method.upper()
        self.headers = headers or {}
        self.timeout = timeout
        self.raw_body = raw_body
        self.json_body = json_body

    @classmethod
    def from_args(cls, args: argparse.Namespace, headers: dict[str, str]) -> RestScenario:
        """Khởi tạo RestScenario từ các tham số dòng lệnh."""
        hdrs = dict(headers)
        json_body = None
        raw_body = None

        if getattr(args, "body", None):
            try:
                json_body = json.loads(args.body)
            except json.JSONDecodeError:
                raw_body = args.body
        elif getattr(args, "body_file", None):
            path = Path(args.body_file)
            content = path.read_text("utf-8")
            try:
                json_body = json.loads(content)
            except json.JSONDecodeError:
                raw_body = content

        if json_body is not None and "Content-Type" not in hdrs:
            hdrs["Content-Type"] = "application/json"

        token = getattr(args, "jwt", None) or (
            _mint_jwt(args.jwt_secret) if getattr(args, "jwt_secret", None) else ""
        )
        if token and "Authorization" not in hdrs:
            hdrs["Authorization"] = f"Bearer {token}"

        return cls(
            url=args.url,
            method=getattr(args, "method", "GET"),
            headers=hdrs,
            timeout=args.timeout,
            raw_body=raw_body,
            json_body=json_body,
        )

    async def execute(self, client: httpx.AsyncClient) -> dict[str, Any]:
        """Gửi 1 request REST thông thường và đo thời gian xử lý (latency)."""
        t0 = time.perf_counter()
        rec: dict[str, Any] = {
            "type": "req",
            "t_start": time.time(),
            "ttft_ms": None,
            "total_ms": None,
            "status": None,
            "ok": False,
            "err": None,
        }
        try:
            kwargs: dict[str, Any] = {
                "headers": self.headers,
                "timeout": self.timeout,
            }
            if self.json_body is not None:
                kwargs["json"] = self.json_body
            elif self.raw_body is not None:
                kwargs["content"] = self.raw_body

            resp = await client.request(self.method, self.url, **kwargs)
            rec["status"] = resp.status_code
            await resp.aread()  # Đọc hết body để giải phóng kết nối về connection pool

            elapsed = (time.perf_counter() - t0) * 1000
            rec["total_ms"] = elapsed
            # Gán ttft_ms = total_ms để đồng bộ hiển thị và tính toán trong report.py
            rec["ttft_ms"] = elapsed

            if 200 <= resp.status_code < 400:
                rec["ok"] = True
            else:
                rec["err"] = f"http_{resp.status_code}"
        except Exception as exc:
            rec["err"] = type(exc).__name__
            rec["total_ms"] = (time.perf_counter() - t0) * 1000
            rec["ttft_ms"] = rec["total_ms"]
        return rec


def _mint_jwt(secret: str, ttl_s: int = 7200) -> str:
    """Tạo JWT nội bộ dự phòng nếu người dùng truyền jwt_secret cho API REST."""
    try:
        import jwt

        return jwt.encode(
            {"roles": ["super_admin"], "exp": int(time.time()) + ttl_s},
            secret,
            algorithm="HS256",
        )
    except ImportError:
        return ""

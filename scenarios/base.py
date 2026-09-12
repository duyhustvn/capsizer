from __future__ import annotations

from typing import Any
import httpx


class Scenario:
    """Giao diện cơ sở (Interface/Protocol) cho mọi kịch bản kiểm thử API.

    Để tạo một bài test API mới, chỉ cần tạo một class kế thừa từ Scenario và hiện thực
    hàm execute(client).
    """

    name: str = "base"
    metric_label: str = "lat"  # Nhãn hiển thị cột độ trễ trên bảng theo dõi (vd: 'ttft', 'lat')

    async def setup(self, client: httpx.AsyncClient) -> None:
        """Khởi tạo tài nguyên trước khi bắt đầu bài test nếu cần (ví dụ: login lấy session, warm-up,...)."""
        pass

    async def execute(self, client: httpx.AsyncClient) -> dict[str, Any]:
        """Thực thi một request và trả về bản ghi kết quả chuẩn hóa dạng dict.

        Cấu trúc dict trả về cần chứa các trường chuẩn:
          - type: "req"
          - t_start: float (thời điểm bắt đầu gửi request, tính bằng time.time())
          - ok: bool (request thành công hay thất bại)
          - status: int | None (HTTP status code)
          - total_ms: float | None (tổng thời gian xử lý request tính bằng ms)
          - ttft_ms: float | None (thời gian nhận phản hồi đầu tiên hoặc latency)
          - err: str | None (mã lỗi nếu có)
        """
        raise NotImplementedError

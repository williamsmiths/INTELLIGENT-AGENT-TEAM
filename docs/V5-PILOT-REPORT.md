# Báo cáo pilot V5 (2026-11-17 → 2026-11-18)

Agent OpenBot thật (runtime kongCowork, không chạy Claude Code trực tiếp), `harnessd` thật, PostgreSQL thật, oracle ẩn chấm sau mỗi lượt. Model: Claude Haiku 5.5, effort medium, Claude Code 2.1.323. Ngân sách pilot 3 USD; đã dùng **1,71 USD** (gồm cả khoảng 0,43 USD cho 4 lượt khởi tạo Lead bằng Sonnet).

## 1. A/B/C trên hai codebase thật

Ba cấu hình theo `evals/README.md`. Mỗi ô có 3 lượt. Mỗi lượt bắt đầu từ cùng commit gốc, xóa context của agent trước khi chạy, cùng brief, cùng feedback gửi giữa chừng.

| Kịch bản | A — OpenBot | B — + Memory & Context | C — + Harness đủ | G — C bỏ cổng bản hiểu |
|---|---|---|---|---|
| Camera chống trùng (quy tắc ẩn trong product model) | 2/4 ·3 lượt | **4/4 ·3** | 4/4 ·3 | 4/4 ·3 |
| Camera biển số | 5/5 ·3 | 5/5 ·3 | 5/5 ·3 | 5/5 ·3 |
| IELTS điểm Writing (frontend) | 6/6 ·3 | 6/6 ·3 | 6/6 ·3 | 6/6 ·3 |
| Lượt đạt đủ quy tắc | 6/9 | 9/9 | 9/9 | 9/9 |
| Chi phí TB / lượt (USD, transcript) | 0,0199 | **0,0153** | 0,0287 | 0,0199 |
| Thời gian TB | 85 s | **56 s** | 103 s | 99 s |
| Token vào TB (gồm cache) | 555k | 507k | 1104k | 649k |
| Can thiệp của người | 0 | 0 | 0 | 0 |

Đọc kết quả:

- Chỉ kịch bản chống trùng phân biệt được các cấu hình. Ở kịch bản này A bỏ sót quy tắc ẩn ở cả 3 lượt (2/4), còn B, C và G đều đạt 4/4. Giá trị thật nằm ở **Memory & Context**: B đúng hơn A, rẻ hơn 23% và nhanh hơn 34%.
- Hai kịch bản còn lại không phân biệt được: cấu hình nào cũng đạt tối đa.
- C đắt hơn B khoảng 1,9 lần nhưng không đúng hơn. Khoảng 44% phần chênh đến từ **cổng bản hiểu** (C so với G). 66% chi phí của C phát sinh trước khi bản hiểu được chấp nhận. → Task 2 đổi mặc định: cổng chỉ bật ở DEEP (spec S2 C.1, AC-S2-040).
- C bản đầu có lỗi đã được chứng minh: agent gõ `cd … && kch understand`, harness không nhận ra lệnh `kch`, cổng chặn mọi lần ghi, 2 lượt hỏng (1/5 và 0/6). Sau khi sửa: 9/9. Các lượt hỏng được giữ làm bằng chứng (`.work/abc/*-C-[12].json`).
- Phép so sánh tương đương: 3 lượt B đối chứng trên daemon mới khớp B cũ (4/4, 5/5, 6/6, chi phí lệch dưới 8%). Memory tự động của OpenBot không xuất hiện trong prompt. Không lượt A nào gọi `kch`.

## 2. Đội V5 (Product / Design / Engineering / Quality Lead)

Bốn agent OpenBot thật; hai project riêng là `team-camera` (bản clone repo camera thật) và `team-clinic` (sản phẩm mới).

| Bước | Ai trả lời | Kết quả | Chi phí |
|---|---|---|---|
| Thảo luận ý tưởng sơ sài (DISCUSS) | Product Lead | Không sửa code, không mở task; tự nêu 5/6 khoảng trống ẩn (ưu tiên, vắng mặt, nhiều phòng, thông báo, reset theo ngày; thiếu: đặt lịch trước) | 0,005 |
| Chốt quyết định + nhờ làm prototype | Product → Design (`kch delegate`) | `prototypes/reception-queue.html`, có đủ quy tắc 70 tuổi / 6 tuổi / vắng mặt, trạng thái trống; task VERIFIED | 0,025 |
| Triển khai end-to-end + feedback giữa chừng + đổi tài khoản giữa task | Product → Engineering | Oracle ẩn **4/4**, test cũ vẫn pass, VERIFIED (scope + unit), không file nào ngoài phạm vi; ngân sách task cộng dồn qua tài khoản backup | 0,032 |
| Review độc lập | Quality Lead | Không sửa repo; NEEDS_REVISION vì một giá trị trong code (đã đổi theo feedback) lệch với quyết định AUTHORITATIVE chưa được cập nhật: đúng một khoảng hở quy trình (feedback chưa cập nhật product model) | 0,013 |
| Ghi nhớ sau khi xóa context | Product Lead | Nhắc lại đủ 3 quyết định của clinic, không lẫn thông tin camera | 0,004 |
| Hai project đồng thời | Design (clinic) ‖ pilot (camera) | Lease chồng thời gian; 0/76 context packet lọt project; thay đổi đúng repo | 0,023 |

## 3. Lỗi tìm ra nhờ chạy thật (đã sửa, có test)

1. `cd … && kch …` không được nhận là lệnh harness → agent kẹt ở cổng (C v1).
2. Role chưa có `kch delegate`; `kch status` không nói project của cuộc hội thoại.
3. Lead ghi `allowed_paths` tuyệt đối dạng thư mục → mọi lần ghi bị từ chối (E.1-13).
4. Ngưỡng "đội đã rảnh" của script đo quá ngắn (lỗi của bộ đo, không phải của harness).

## 4. Giới hạn còn lại (không sửa trong đợt này)

- **L14:** Bash của agent đã ghi danh vẫn đọc được `~/OpenBot/Shared`, vì OpenBot truyền thư mục này cho mọi phiên Claude. Muốn sửa tận gốc cần thay đổi OpenBot và chủ sản phẩm duyệt.
- Feedback của người dùng chưa tự trở thành đề xuất cập nhật product model (Quality Lead đã bắt được hệ quả).
- Việc nhận công việc đã VERIFIED (commit) do Owner làm tay; repo còn thay đổi chưa commit thì việc mới không bắt đầu được (đúng thiết kế, D.1).
- Không có route automation cho channel. Định tuyến trong channel chưa được kiểm bằng script; binding channel → project đã có test với dữ liệu mẫu đúng schema.
- Chi phí theo pha (C.7) không tách được trong một lượt: OpenBot ghi một dòng usage mỗi lượt, đóng dấu thời gian lúc bắt đầu lượt.
- Hook harness chỉ có cho Claude Code; backup model phải cùng provider Claude.
- Chưa đo với Sonnet: đội đang dùng Haiku để ở trong ngân sách pilot.
- Ba kịch bản A/B/C là ít; hai trong số đó không phân biệt được các cấu hình.

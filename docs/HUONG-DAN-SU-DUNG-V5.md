# Hướng dẫn dùng đội V5 trên kongCowork

Đội có bốn agent cố định trong mục **V5 Team** của sidebar: Product Lead, Design Lead, Engineering Lead và Quality Lead. Không cần tạo agent mới cho mỗi project: cùng bốn Lead làm việc ở mọi project, mỗi project có một channel riêng.

## 1. Tạo project mới (một lần, trên máy chạy kongCowork)

1. Đặt repo ở ngoài thư mục home (ví dụ `/var/tmp/kc-pilot/repos/my-app`). Repo phải là git và đã commit sạch.
2. Tạo channel trong kongCowork: tên `my-app · V5`, thành viên là bốn Lead, lead của channel là **Product Lead**, bật **isolated**. Có thể nhờ agent "New agent" tạo giúp.
3. Đăng ký project với harness, dùng lại bốn Lead và gắn channel:
   ```bash
   cd ~/Projects/kongcowork-harness
   python3 -I deploy/team/add_project.py my-app "My App" /var/tmp/kc-pilot/repos/my-app "go test ./..." --channel <id channel>
   ```
   `"go test ./..."` là lệnh kiểm tra bắt buộc cho mọi việc đội làm trong project này (thay bằng `npm test`, `pytest`… tùy repo). Chạy lại lệnh này không tạo bản sao.

## 2. Làm việc hằng ngày, tất cả trong channel của project

| Bạn muốn | Viết trong channel | Ai trả lời, làm gì |
|---|---|---|
| Bàn ý tưởng | "Mình muốn làm …" (không cần gọi tên) | Product Lead: nêu cách hiểu, khoảng trống, đề xuất, tối đa 3 câu hỏi. **Không sửa code** |
| Chốt quyết định | "Chốt: …" | Product Lead ghi vào product model |
| Thiết kế, prototype | "Nhờ Design làm prototype màn …" | Product giao cho Design Lead → file `.html` trong repo, có kiểm tra |
| Triển khai | "Triển khai … luôn nhé" | Product giao cho Engineering Lead → code, test, `kch verify` |
| Sửa lỗi nhỏ | "@Engineering Lead sửa …" | Agent được gọi tên trả lời |
| Review độc lập | "@Quality Lead review …" | Quality đọc code và product model, kết luận PASS / NEEDS_REVISION; không sửa code |
| Feedback giữa chừng | "Thực ra … giữ nguyên phần còn lại" | Agent đang giữ việc chỉ sửa phần bị ảnh hưởng |

Các agent tự hỏi ý nhau khi cần (`kch consult`), không bắt mọi việc đi qua cả bốn người.

## 3. Kiểm tra kết quả và nhận việc

- Việc xong khi agent báo **VERIFIED**: runner của harness đã chạy lệnh kiểm tra trên đúng trạng thái code.
- Xem thay đổi: `git -C <repo> status` và `git -C <repo> diff`.
- **Nhận việc = bạn commit** (agent không được commit hay deploy):
  ```bash
  git -C <repo> add -A && git -C <repo> commit -m "feat: …"
  ```
  Khi repo còn thay đổi chưa commit, việc mới trong cùng repo sẽ không bắt đầu (Product Lead sẽ nhắc).
- Quyết định có trust Candidate (giả định do agent đề xuất) cần bạn xác nhận. Quality Lead sẽ chỉ ra khi code lệch với một quyết định đã chốt.

## 4. Chi phí và giới hạn

- Mỗi task có trần chi phí (mặc định 0,4 USD), mỗi project có trần riêng, và cả hệ thống có trần chung. Khi vượt trần, agent dừng lại và tóm tắt việc đã làm.
- Model hiện tại là Claude Haiku 5.5, đủ cho các bài đã đo. Muốn đổi sang Sonnet thì đổi trong Agent Settings của bốn Lead; cấu hình Sonnet chưa được đo.
- Tin nhắn riêng cho một Lead (ngoài channel) không biết thuộc project nào. Hãy dùng channel của project.
- Agent đã ghi danh không dùng được MCP, web hay Computer Use (đây là chủ đích, do harness chặn).
- Agent vẫn có thể dùng lệnh `ls`/`cat` trong Bash để đọc thư mục `~/OpenBot/Shared` (giới hạn L14). Không để dữ liệu riêng của project trong thư mục đó.

## 5. Khi có sự cố

| Dấu hiệu | Việc cần làm |
|---|---|
| Mọi tool của agent bị từ chối kèm "harness không sẵn sàng" | `systemctl --user status kc-harness`; dịch vụ tự khởi động lại sau 5 giây |
| "cấu hình hook bị sửa" | `python3 -I deploy/team/add_project.py …` (chạy lại sẽ ghi danh lại) |
| Agent hỏi câu hỏi chặn (blocking) và dừng | Trả lời gõ trong channel **chưa** gỡ được trạng thái chờ của harness. Xem câu đang mở: `kch owner GET '/v1/questions?status=OPEN'`; trả lời: `kch owner POST /v1/questions/<id>/answer '{"option":"…"}'`. Trong pilot không phát sinh câu hỏi chặn nào (agent đi tiếp theo mặc định) |
| Muốn dừng toàn bộ đội | `systemctl --user stop kc-harness` (mọi tool của đội bị từ chối ngay) |

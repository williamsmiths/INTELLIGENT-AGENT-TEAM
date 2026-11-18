# Giao thức đo giá trị A / B / C

Mục đích: trả lời "agent có làm việc tốt hơn khi có harness không", bằng agent thật, model thật, codebase thật. Số test PASS của chính harness, JSON schema hợp lệ hay runtime giả lập **không** được dùng làm bằng chứng cho câu hỏi này.

## Ba cấu hình

| Cấu hình | `principal.mode` | Agent nhận được gì |
|---|---|---|
| A — kongCowork hiện tại | OBSERVE | Không gì thêm. Harness chỉ quan sát và đo. |
| B — + Memory & Context | CONTEXT | Context packet đúng project, đúng phiên bản ở mỗi lượt; tra cứu kiến thức. Không chặn, không ngân sách. |
| C — + Lightweight Harness | FULL | Như B, cộng: chặn ghi sai phạm vi, yêu cầu verify trước khi dừng, ngân sách, feedback điều hướng tại lần ghi kế tiếp. |

## Điều kiện giữ cố định giữa ba cấu hình

- Cùng model, cùng mức suy luận, cùng phiên bản CLI.
- Cùng repo và cùng commit gốc; mỗi lượt chạy trên một git worktree mới.
- Cùng brief (cố ý thiếu, như người dùng thật viết), cùng feedback giữa chừng, gửi tại cùng mốc (sau lần ghi file đầu tiên).
- Cùng Live Product Model trong database; A không được nạp nhưng model vẫn tồn tại để công bằng về dữ liệu.
- Bộ chấm (oracle) nằm ngoài worktree, agent không đọc được, chạy sau khi lượt kết thúc.

## Chỉ số (luôn ghi tử số / mẫu số; mẫu số 0 ghi N/A)

| Nhóm | Chỉ số | Cách đo |
|---|---|---|
| Hiểu yêu cầu | Số quy tắc nghiệp vụ không có trong brief nhưng có trong product model mà kết quả tuân thủ | Oracle test theo từng quy tắc |
| Phát hiện thiếu | Số khoảng trống đã gắn nhãn mà agent nêu ra (hỏi hoặc ghi giả định) | Đối chiếu câu trả lời cuối với danh sách nhãn, hai người chấm hoặc chấm tay có ghi lý do |
| Feedback | Feedback giữa chừng được áp dụng đúng và phần không liên quan giữ nguyên | Oracle test trước/sau |
| Chất lượng code | Oracle ẩn pass / tổng; file đổi ngoài phạm vi | Chạy oracle; `git diff --name-only` so với phạm vi task |
| Hiệu quả | Token vào/ra, chi phí, thời gian, số lượt, số tool call | Số do provider báo |
| Rework | Số lần verify FAIL; số dòng đổi sau lần báo xong đầu tiên | Evidence và git |
| Can thiệp | Số lần chủ phải nhắn thêm ngoài brief và feedback theo kịch bản | Đếm |

## Quy tắc báo cáo

- Mỗi cấu hình chạy ≥ 3 lượt trước khi so sánh; ít hơn thì ghi "giai thoại", không kết luận.
- Ghi rõ lượt nào chạy qua agent kongCowork thật và lượt nào chạy qua Claude Code CLI trực tiếp (cùng engine nhưng không qua runtime OpenBot).
- Ghi model, phiên bản CLI, commit gốc, thời điểm, và mọi lượt bị loại kèm lý do.
- Ít nhất hai project khác nghiệp vụ và kiến trúc; có lượt hai project chạy đồng thời để kiểm tra cách ly.
- Kết quả xấu cho harness được báo như kết quả tốt.

## Trạng thái

So sánh A/B/C theo giao thức trên: **đã chạy** (2026-11-17/18). Có 3 kịch bản trên 2 codebase thật (camera Go, IELTS frontend) với oracle ẩn, mỗi cấu hình 3 lượt. Kết quả, lượt bị loại và giới hạn ở `docs/V5-PILOT-REPORT.md`. Kịch bản và oracle là dữ liệu riêng của project nên không có trong bản công khai; `evals/abc/driver.py` và `batch.py` là bộ chạy.

### Thử cơ chế với agent thật (2026-11-17) — giai thoại, không phải kết quả đo

`evals/smoke/run_smoke.py`: `harnessd` thật, PostgreSQL thật, Claude Code 2.1.323 chạy trực tiếp (không qua runtime OpenBot), model `claude-haiku-5-5`, một lượt mỗi cấu hình, repo mẫu một file. Việc giao: tạo file lời chào đúng yêu cầu sản phẩm và cập nhật `README.md`; task chỉ cho phép `src/**`.

| | A (OBSERVE) | B (CONTEXT) | C (FULL) |
|---|---|---|---|
| Oracle | pass | pass | pass |
| File đổi ngoài phạm vi | `README.md` | không (verify lần đầu FAIL `scope`, agent tự hoàn tác) | không (ghi `README.md` bị từ chối trước) |
| Evidence do runner tạo | không | có, VERIFIED | có, VERIFIED |
| Lượt / chi phí / thời gian | 5 / 0,0147 USD / 17,4 s | 11 / 0,0180 USD / 27,3 s | 11 / 0,0212 USD / 28,5 s |

Đọc kết quả này thế nào:

- Nó chứng minh **cơ chế chạy được với agent thật**: context tới model, lệnh từ chối được tuân theo, agent tự gọi verify, evidence gắn đúng candidate.
- Nó **không** chứng minh agent hiểu yêu cầu tốt hơn: ở A agent vẫn làm đúng vì đọc được đáp án trong `tests/check.sh` nằm ngay trong repo. Fixture này để lộ oracle, vi phạm chính giao thức ở trên.
- Harness làm tăng chi phí khoảng 23–45% và thời gian khoảng 57–64% trên bài này. Thứ đổi lại là giữ đúng phạm vi và có evidence, không phải kết quả đúng hơn.
- Một lượt mỗi cấu hình: không kết luận được gì về xu hướng.

### Đo retrieval (AC-H-090)

`.work/retrieval-eval.json`, 34 truy vấn, 2 project, 20 mục mỗi project, chưa có embedding: recall@5 tiếng Việt có dấu 0,75; không dấu 1,00; tiếng Anh 0,50; code 1,00; p95 dưới 2 ms; không có kết quả lọt project. Sáu truy vấn trượt đều là diễn đạt lại hoặc hỏi tiếng Anh trên nội dung tiếng Việt — đây là nhóm cần cân nhắc vector search. Kho quá nhỏ để nói về độ trễ ở quy mô thật.

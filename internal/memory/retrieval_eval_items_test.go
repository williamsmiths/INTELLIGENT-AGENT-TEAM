package memory_test

// Fixture of the retrieval evaluation (AC-H-090): two projects with different
// domains that reuse the same keys. The items were written as realistic
// product knowledge first; the queries (retrieval_eval_queries_test.go) were
// written against the domain, not against what the search happens to return.

type evalItem struct {
	Key, Kind, Aspect, Title, Body string
}

// Project "alpha": an IELTS mock-test authoring and marking product.
var evalItemsIELTS = []evalItem{
	{"REQ-1", "requirement", "TO_BE", "Xóa nhóm câu hỏi phải xác nhận", "Khi giáo viên bấm xóa một nhóm câu hỏi trong Part, hệ thống hiện hộp thoại xác nhận và nêu số câu hỏi sẽ mất. Chỉ xóa sau khi người dùng đồng ý."},
	{"REQ-2", "requirement", "TO_BE", "Nút thêm nhóm câu hỏi nằm trong từng Part", "Mỗi Part của đề Reading và Listening có nút \"Thêm nhóm câu hỏi\" riêng ở cuối Part, không dùng nút chung ở đầu trang."},
	{"REQ-3", "requirement", "TO_BE", "Tự động lưu bản nháp đề thi", "Trình soạn đề tự lưu bản nháp mỗi 30 giây và khi người dùng rời trang; hiển thị thời điểm lưu gần nhất."},
	{"RULE-1", "rule", "TO_BE", "Quy đổi band điểm Listening", "Số câu đúng trên 40 được quy đổi sang band theo bảng chuẩn: 39–40 câu là band 9.0, 37–38 là 8.5, 35–36 là 8.0. Điểm tổng làm tròn đến 0.5 gần nhất."},
	{"RULE-2", "rule", "TO_BE", "Giới hạn thời gian làm bài Writing", "Bài Writing kéo dài 60 phút: Task 1 tối thiểu 150 từ, Task 2 tối thiểu 250 từ. Hết giờ hệ thống tự nộp bài."},
	{"UI-1", "ui", "AS_IS", "Thanh công cụ soạn đoạn văn Reading", "Thanh công cụ hiện có in đậm, in nghiêng, gạch chân và chèn ảnh; chưa có chèn bảng."},
	{"UI-2", "ui", "TO_BE", "Hiển thị bộ đếm từ cho bài Writing", "Ô soạn bài hiển thị số từ theo thời gian thực và cảnh báo màu cam khi chưa đạt số từ tối thiểu."},
	{"FLOW-1", "flow", "TO_BE", "Luồng chấm bài Speaking bằng AI", "Học viên ghi âm câu trả lời, hệ thống chuyển giọng nói thành văn bản, chấm theo bốn tiêu chí fluency, lexical resource, grammar, pronunciation rồi trả nhận xét."},
	{"FLOW-2", "flow", "TO_BE", "Nhập đề từ file Word", "Giáo viên tải file .docx, hệ thống tách đoạn văn và câu hỏi theo tiêu đề Questions 1–13, cho xem trước rồi mới lưu."},
	{"API-1", "api", "TO_BE", "POST /api/v1/exams/{examId}/parts/{partId}/question-groups", "Tạo nhóm câu hỏi mới trong một Part. Body gồm type (multiple_choice, matching_headings, gap_fill), instructions và danh sách câu hỏi. Trả 201 kèm id nhóm."},
	{"API-2", "api", "TO_BE", "DELETE /api/v1/question-groups/{groupId}", "Xóa nhóm câu hỏi. Yêu cầu tham số confirm=true, nếu thiếu trả 409. Handler: deleteQuestionGroup trong src/server/routes/question-groups.ts."},
	{"DATA-1", "data", "AS_IS", "Bảng question_group", "Cột: id, part_id, type, instructions, order_index, created_at. Khóa ngoại part_id trỏ tới exam_part với ON DELETE CASCADE."},
	{"ARCH-1", "architecture", "AS_IS", "Trình soạn đề dùng TipTap", "Editor xây trên TipTap/ProseMirror; mỗi loại câu hỏi là một node extension riêng trong src/features/editor/extensions/."},
	{"DEC-1", "decision", "TO_BE", "Không cho sửa đề đã phát hành", "Đề đã phát hành cho lớp chỉ được nhân bản thành phiên bản mới; không sửa trực tiếp để giữ nguyên kết quả của học viên đã làm."},
	{"CODE-1", "architecture", "AS_IS", "Hàm calculateBandScore", "calculateBandScore(rawCorrect: number, skill: Skill) trong src/lib/scoring/band-score.ts tra bảng BAND_TABLE và trả band dạng số thực."},
	{"CODE-2", "architecture", "AS_IS", "Component QuestionGroupEditor", "src/features/editor/components/QuestionGroupEditor.tsx render danh sách câu hỏi của một nhóm, dùng hook useQuestionGroupMutations để thêm, sửa, xóa."},
	{"DEP-1", "dependency", "AS_IS", "Dịch vụ chuyển giọng nói thành văn bản", "Dùng Whisper qua hàng đợi BullMQ; file ghi âm lưu ở S3 bucket ielts-speaking-audio, giữ 90 ngày."},
	{"GOAL-1", "goal", "TO_BE", "Giáo viên soạn một đề Reading hoàn chỉnh trong 20 phút", "Mục tiêu sản phẩm quý này: giảm thời gian soạn đề bằng nhập từ file Word và gợi ý câu hỏi."},
	{"EN-1", "decision", "TO_BE", "Students cannot retake a mock test within 24 hours", "A student may start the same mock test again only after 24 hours, so that band history stays meaningful. Teachers can override per class."},
	{"EN-2", "rule", "TO_BE", "Listening audio plays only once", "During a Listening section the audio cannot be paused or rewound; the player hides the seek bar and resumes from the last position after a network drop."},
}

// Project "beta": a warehouse and order-fulfilment service.
var evalItemsWarehouse = []evalItem{
	{"REQ-1", "requirement", "TO_BE", "Xóa phiếu nhập kho phải xác nhận", "Khi thủ kho xóa một phiếu nhập chưa duyệt, hệ thống yêu cầu xác nhận và ghi lý do. Phiếu đã duyệt không được xóa, chỉ được hủy bằng phiếu điều chỉnh."},
	{"REQ-2", "requirement", "TO_BE", "Xuất kho theo nguyên tắc nhập trước xuất trước", "Khi tạo phiếu xuất, hệ thống gợi ý lô có ngày nhập sớm nhất (FIFO). Hàng có hạn dùng thì ưu tiên lô hết hạn sớm nhất."},
	{"REQ-3", "requirement", "TO_BE", "Cảnh báo tồn kho dưới mức tối thiểu", "Mỗi mã hàng có mức tồn tối thiểu theo từng kho. Khi tồn khả dụng thấp hơn mức này, gửi cảnh báo cho quản lý kho và đề xuất số lượng đặt thêm."},
	{"RULE-1", "rule", "TO_BE", "Tồn khả dụng không được âm", "Tồn khả dụng = tồn thực tế − hàng đã giữ cho đơn. Không cho xác nhận đơn hàng nếu làm tồn khả dụng nhỏ hơn 0."},
	{"RULE-2", "rule", "TO_BE", "Kiểm kê định kỳ khóa vị trí kệ", "Trong lúc kiểm kê một vị trí kệ, mọi lệnh nhập xuất tại vị trí đó bị tạm khóa cho đến khi chốt chênh lệch."},
	{"UI-1", "ui", "AS_IS", "Màn hình quét mã vạch trên thiết bị cầm tay", "Nhân viên quét mã vạch kiện hàng rồi quét vị trí kệ; màn hình hiện số lượng còn phải cất và phát tiếng báo khi quét sai vị trí."},
	{"UI-2", "ui", "TO_BE", "Bảng điều khiển tồn kho theo thời gian thực", "Trang tổng quan hiển thị tồn theo kho, hàng sắp hết hạn trong 30 ngày và các đơn đang chờ giữ hàng."},
	{"FLOW-1", "flow", "TO_BE", "Luồng xử lý đơn hàng trả về", "Hàng trả về được nhận tại khu kiểm tra, phân loại còn bán được hoặc hỏng, rồi nhập lại kho hoặc chuyển sang kho hàng lỗi kèm biên bản."},
	{"FLOW-2", "flow", "TO_BE", "Chuyển kho nội bộ", "Phiếu chuyển kho tạo một lệnh xuất ở kho đi và một lệnh nhập chờ ở kho đến; hàng ở trạng thái đang vận chuyển cho tới khi kho đến xác nhận nhận đủ."},
	{"API-1", "api", "TO_BE", "POST /api/v1/warehouses/{warehouseId}/receipts", "Tạo phiếu nhập kho. Body gồm supplier_id, danh sách dòng hàng (sku, quantity, lot_number, expiry_date). Trả 201 kèm mã phiếu."},
	{"API-2", "api", "TO_BE", "POST /api/v1/orders/{orderId}/allocate", "Giữ hàng cho đơn. Handler AllocateOrder trong internal/allocation/service.go chọn lô theo FIFO và trả 409 khi không đủ tồn."},
	{"DATA-1", "data", "AS_IS", "Bảng stock_movement", "Cột: id, sku, warehouse_id, bin_location, quantity_delta, reason, ref_type, ref_id, created_at. Chỉ thêm, không sửa; tồn kho là tổng quantity_delta."},
	{"ARCH-1", "architecture", "AS_IS", "Tồn kho tính từ sổ cái di chuyển", "Không lưu số tồn trực tiếp; bảng stock_balance là materialized view làm mới sau mỗi giao dịch, nguồn sự thật là stock_movement."},
	{"DEC-1", "decision", "TO_BE", "Không cho sửa phiếu đã duyệt", "Phiếu nhập và phiếu xuất đã duyệt là bất biến; sai sót được sửa bằng phiếu điều chỉnh để giữ dấu vết kiểm toán."},
	{"CODE-1", "architecture", "AS_IS", "Hàm ReserveStock", "ReserveStock(ctx, tx, orderID, lines) trong internal/allocation/reserve.go khóa các dòng stock_balance bằng SELECT FOR UPDATE theo thứ tự sku để tránh deadlock."},
	{"CODE-2", "architecture", "AS_IS", "Worker đồng bộ đơn hàng từ sàn", "cmd/sync-worker/main.go chạy job SyncMarketplaceOrders mỗi 5 phút, gọi API Shopee và Lazada, ghi vào bảng inbound_order với khóa idempotency."},
	{"DEP-1", "dependency", "AS_IS", "Tích hợp hãng vận chuyển", "Tạo vận đơn qua API của GHN và GHTK; webhook trạng thái giao hàng nhận ở /webhooks/carriers và xác thực bằng chữ ký HMAC."},
	{"GOAL-1", "goal", "TO_BE", "Giảm sai lệch tồn kho xuống dưới 0,5%", "Mục tiêu năm: chênh lệch giữa tồn hệ thống và tồn kiểm kê thực tế dưới 0,5% giá trị hàng."},
	{"EN-1", "decision", "TO_BE", "Pick lists are grouped by zone", "The picking list for a wave is sorted by warehouse zone and bin location so that a picker walks each aisle only once."},
	{"EN-2", "rule", "TO_BE", "Negative adjustments need a second approver", "A stock adjustment that reduces quantity by more than 50 units or 5,000,000 VND must be approved by a second user with the inventory manager role."},
}

package memory_test

// Query groups of the retrieval evaluation (AC-H-090).
const (
	groupViAccent = "vi_diacritics"
	groupViPlain  = "vi_no_diacritics"
	groupEnglish  = "english"
	groupCode     = "code"
)

// evalQuery is one labelled query: the keys a person would expect back, in the
// project named ("ielts" or "warehouse"). Several are paraphrases or use the
// other language on purpose; lexical search is expected to miss some of them.
type evalQuery struct {
	Project, Group, Q string
	Want              []string
}

var evalQueries = []evalQuery{
	// Vietnamese with diacritics.
	{"ielts", groupViAccent, "xóa nhóm câu hỏi cần xác nhận", []string{"REQ-1", "API-2"}},
	{"ielts", groupViAccent, "bảng quy đổi điểm nghe sang band", []string{"RULE-1"}},
	{"ielts", groupViAccent, "tự động lưu bản nháp", []string{"REQ-3"}},
	{"ielts", groupViAccent, "chấm bài nói bằng trí tuệ nhân tạo", []string{"FLOW-1"}},
	{"warehouse", groupViAccent, "xóa phiếu nhập kho", []string{"REQ-1"}},
	{"warehouse", groupViAccent, "cảnh báo khi hàng sắp hết", []string{"REQ-3"}},
	{"warehouse", groupViAccent, "nhập trước xuất trước", []string{"REQ-2"}},
	{"warehouse", groupViAccent, "quy trình hàng trả về", []string{"FLOW-1"}},

	// Vietnamese typed without diacritics.
	{"ielts", groupViPlain, "nut them nhom cau hoi trong part", []string{"REQ-2"}},
	{"ielts", groupViPlain, "gioi han thoi gian bai writing", []string{"RULE-2"}},
	{"ielts", groupViPlain, "nhap de tu file word", []string{"FLOW-2"}},
	{"ielts", groupViPlain, "khong sua de da phat hanh", []string{"DEC-1"}},
	{"warehouse", groupViPlain, "ton kha dung khong duoc am", []string{"RULE-1"}},
	{"warehouse", groupViPlain, "kiem ke khoa vi tri ke", []string{"RULE-2"}},
	{"warehouse", groupViPlain, "chuyen kho noi bo", []string{"FLOW-2"}},
	{"warehouse", groupViPlain, "tich hop don vi van chuyen", []string{"DEP-1"}},

	// English.
	{"ielts", groupEnglish, "retake mock test after 24 hours", []string{"EN-1"}},
	{"ielts", groupEnglish, "listening audio cannot be paused", []string{"EN-2"}},
	{"ielts", groupEnglish, "word count warning for writing essay", []string{"UI-2"}},
	{"ielts", groupEnglish, "speech to text service", []string{"DEP-1"}},
	{"warehouse", groupEnglish, "pick list sorted by zone", []string{"EN-1"}},
	{"warehouse", groupEnglish, "second approver for stock adjustment", []string{"EN-2"}},
	{"warehouse", groupEnglish, "barcode scanning on handheld", []string{"UI-1"}},
	{"warehouse", groupEnglish, "low stock alert", []string{"REQ-3"}},

	// Code-like: symbol names, file paths, routes, table names.
	{"ielts", groupCode, "calculateBandScore", []string{"CODE-1"}},
	{"ielts", groupCode, "src/features/editor/components/QuestionGroupEditor.tsx", []string{"CODE-2"}},
	{"ielts", groupCode, "question_group", []string{"DATA-1"}},
	{"ielts", groupCode, "useQuestionGroupMutations", []string{"CODE-2"}},
	{"ielts", groupCode, "DELETE /api/v1/question-groups/{groupId}", []string{"API-2"}},
	{"warehouse", groupCode, "ReserveStock", []string{"CODE-1"}},
	{"warehouse", groupCode, "internal/allocation/service.go", []string{"API-2"}},
	{"warehouse", groupCode, "stock_movement", []string{"DATA-1", "ARCH-1"}},
	{"warehouse", groupCode, "SyncMarketplaceOrders", []string{"CODE-2"}},
	{"warehouse", groupCode, "/webhooks/carriers", []string{"DEP-1"}},
}

// Report of the evaluation, written to .work/retrieval-eval.json.
type evalGroupReport struct {
	Group              string         `json:"group"`
	Queries            int            `json:"queries"`
	RecallAt5          float64        `json:"recall_at_5"`
	MRR                float64        `json:"mrr"`
	P50Ms              float64        `json:"latency_p50_ms"`
	P95Ms              float64        `json:"latency_p95_ms"`
	PacketCharsAvg     float64        `json:"packet_chars_avg"`
	FirstRelevantStage map[string]int `json:"first_relevant_stage"`
	latencies          []float64
}

type evalQueryReport struct {
	Project   string   `json:"project"`
	Group     string   `json:"group"`
	Q         string   `json:"q"`
	Want      []string `json:"want"`
	Top5      []string `json:"top5"`
	RecallAt5 float64  `json:"recall_at_5"`
	RR        float64  `json:"reciprocal_rank"`
	Stage     string   `json:"first_relevant_stage"`
}

type evalReport struct {
	Spec             string             `json:"spec"`
	GeneratedAt      string             `json:"generated_at"`
	Method           map[string]string  `json:"method"`
	ItemsPerProject  map[string]int     `json:"items_per_project"`
	Groups           []*evalGroupReport `json:"groups"`
	Overall          *evalGroupReport   `json:"overall"`
	CrossProjectHits int                `json:"cross_project_hits"`
	Queries          []evalQueryReport  `json:"queries"`
}

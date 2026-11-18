package intel

import (
	"fmt"
	"strings"
)

// Texts shown to the model (C.2, C.4, C.5): Vietnamese, short, bounded, and
// built only from the task's own project.

// maxListed bounds the items listed in one message.
const maxListed = 12

// UnderstandCommand is the exact command an agent uses for a full understanding.
const UnderstandCommand = "kch understand <<'END'\n" +
	`{"goal":"…","preserve":["KEY"],"affected":["KEY hoặc đường dẫn"],` +
	`"not_applicable":[{"key":"KEY","reason":"lý do ≥ 15 ký tự"}],` +
	`"assumptions":[{"text":"…","basis":"inferred","evidence":"…"}],` +
	`"questions":[{"text":"…","options":["A","B"],"default":"A","impact":"low","blocking":false}]}` +
	"\nEND"

// DeltaCommand is the exact command for a delta (C.5).
const DeltaCommand = "kch understand <<'END'\n" +
	`{"delta":{"changed":["KEY hoặc đường dẫn"],"preserved":["KEY"]}}` + "\nEND"

var ruleText = map[string]string{
	"U1":        "key được ghim → preserve hoặc affected",
	"U2":        "mục liên quan → preserve, affected hoặc not_applicable",
	"U3":        "mục chưa chắc chắn → nhắc key trong assumptions, questions hoặc not_applicable",
	"U4":        "câu hỏi cần ≥ 2 options và default là một option",
	"U4-high":   "tối đa 3 câu impact high",
	"U5-brief":  "task chỉ có brief → cần ít nhất một giả định hoặc câu hỏi",
	"U5-pinned": "not_applicable không dùng cho key đang ghim",
	"U5-reason": "lý do not_applicable cần ≥ 15 ký tự",
	"DELTA":     "key được ghim → changed hoặc preserved",
}

// ListMissing renders the unaddressed items, one per line, bounded.
func ListMissing(m []Missing) string {
	var b strings.Builder
	for i, x := range m {
		if i == maxListed {
			fmt.Fprintf(&b, "- … (+%d mục)\n", len(m)-maxListed)
			break
		}
		title := ""
		if x.Title != "" {
			title = " (" + clip(x.Title, 80) + ")"
		}
		fmt.Fprintf(&b, "- %s%s: %s\n", clip(x.Key, 60), title, ruleText[x.Rule])
	}
	return b.String()
}

// GateItems is what a full understanding must address, before anything was submitted.
func GateItems(s Sets) []Missing {
	var m []Missing
	for _, r := range s.Pinned {
		m = append(m, Missing{Key: r.Key, Title: r.Title, Rule: "U1"})
	}
	for _, r := range s.Related {
		m = append(m, Missing{Key: r.Key, Title: r.Title, Rule: "U2"})
	}
	for _, r := range s.Uncertain {
		m = append(m, Missing{Key: r.Key, Title: r.Title, Rule: "U3"})
	}
	if s.BriefOnly() {
		m = append(m, Missing{Key: "assumptions", Title: "task chỉ có brief", Rule: "U5-brief"})
	}
	return m
}

// GateDeny is the pre-tool refusal of a write before the understanding (AC-S2-031).
func GateDeny(taskKey, mode string, s Sets) string {
	return fmt.Sprintf("Task %s (mode %s) cần bản hiểu trước lần ghi đầu vào repo. Phải xử lý:\n%s"+
		"Nộp bằng MỘT lệnh Bash riêng (JSON qua heredoc):\n%s\n"+
		"Gửi bản hiểu bằng MỘT lệnh Bash riêng: kch understand <<'END' {json} END (không cd, không &&)\n",
		taskKey, mode, ListMissing(GateItems(s)), UnderstandCommand)
}

// DeltaDeny is the pre-tool refusal of a write before the delta (AC-S2-035).
func DeltaDeny(taskKey string, revision int, s Sets, notice string) string {
	var m []Missing
	for _, r := range s.Pinned {
		m = append(m, Missing{Key: r.Key, Title: r.Title, Rule: "DELTA"})
	}
	head := fmt.Sprintf("Task %s đã đổi (rev %d).", taskKey, revision)
	if notice != "" {
		head += " Thay đổi:\n" + notice + "\n"
	}
	return head + "Trước khi ghi tiếp, nộp delta; mọi key được ghim phải nằm trong changed hoặc preserved:\n" +
		ListMissing(m) + "Nộp bằng MỘT lệnh Bash riêng:\n" + DeltaCommand
}

// Rejected is the 422 answer of a submission that misses items (AC-S2-031).
func Rejected(m []Missing, left int, delta bool) string {
	cmd := UnderstandCommand
	if delta {
		cmd = DeltaCommand
	}
	return fmt.Sprintf("Bản hiểu chưa xử lý đủ (%d mục):\n%sCòn %d lần nộp trước khi bản thiếu được nhận kèm danh sách chưa xử lý (chủ sản phẩm sẽ thấy). Nộp lại:\n%s",
		len(m), ListMissing(m), left, cmd)
}

// Steer is the convergence hint after two identical failing verifies (C.4).
func Steer(failing []string, bonus bool) string {
	s := "Hai lần verify liên tiếp thất bại giống hệt nhau (" + strings.Join(failing, ", ") + "): đừng sửa tiếp theo cùng giả thuyết. " +
		"Xét lại lần lượt từng tầng: yêu cầu (đọc lại requirement được ghim), context (kch context, kch search), kiến trúc (chỗ đổi có đúng module không), " +
		"test (check đang kiểm điều gì), cài đặt. Lần thứ ba giống hệt sẽ dừng task chờ chủ sản phẩm."
	if bonus {
		s += " Được cấp thêm 1 lượt tham vấn: kch consult <product|engineering|design|quality> <câu hỏi>."
	}
	return s
}

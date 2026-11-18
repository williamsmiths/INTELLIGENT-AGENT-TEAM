# Context — kongCowork Intelligence Harness

- **Ai xây:** chủ sản phẩm kongCowork (fork OpenBot, `~/Projects/openbot`, v0.70.0). Người dùng duy nhất hiện tại: chủ (PM).
- **Hệ thống:** dịch vụ Go độc lập (`harnessd`) chạy cùng máy với kongCowork, bổ sung năng lực hiểu – nhớ – kiểm soát – kiểm chứng cho agent. Không thay runtime agent của OpenBot.
- **Nguồn đặc tả:** Master Prompt V5.0 (ưu tiên) + bộ Independent Agent Workspace V4.1 (kế thừa phần không mâu thuẫn).
- **Stack:** Go 1.27, chi, pgx v5, goose, MCP Go SDK, PostgreSQL 17 + pgvector/pg_trgm/unaccent (container `kc-harness-pg`, 127.0.0.1:5439), modernc sqlite (đọc `openbot.db` chỉ-đọc).
- **Quy mô S1:** 1 máy, 1 chủ, ~10 agent, 1–vài project. Không multi-tenant.
- **Ràng buộc:** hạn chế sửa lõi OpenBot (fork merge upstream đều); không hai scheduler, không hai hệ task, không hai nguồn chính thức cho cùng một trách nhiệm.

## Quyền sở hữu trạng thái (ADR-0001, tóm tắt)

| Trách nhiệm | Chủ sở hữu | Bên kia làm gì |
|---|---|---|
| Hồ sơ agent, chat, turn, session, hàng đợi tin, lịch/routine, tài khoản provider, số đo token thô | OpenBot (`openbot.db`) | Harness chỉ đọc, không ghi SQLite |
| Kiến thức dự án, living spec, blackboard, kinh nghiệm | Harness (PostgreSQL) | Agent đọc/ghi qua hook, REST, MCP |
| Work item (goal/task) như **phạm vi kiến thức + quyền + ngân sách** | Harness | Không có hàng đợi chạy: giao việc = gửi tin qua mailbox của OpenBot |
| Lease, checkpoint, evidence, sổ ngân sách cộng dồn | Harness | Sổ ngân sách là dẫn xuất từ số đo thô của OpenBot, khóa theo `record_id` |
| Khởi chạy/điều hướng lượt chạy của agent | OpenBot | Harness yêu cầu qua automation API; không tự spawn model |

## Điểm tích hợp runtime (đã audit, không sửa lõi)

| Thời điểm (V5 §3) | Cơ chế | Phủ |
|---|---|---|
| Trước khi gọi model / dựng context | Claude hook `UserPromptSubmit`, `SessionStart` trong `<workspace>/.claude/settings.json` | Claude, Full access |
| Trước/sau tool, khi đổi code | Claude hook `PreToolUse` / `PostToolUse` | Claude, Full access |
| Hoàn thành / dừng | Claude hook `Stop`; `orchestration_events: turn.completed{status}` | Hook: Claude; event: mọi provider |
| Token usage | `agent_usage_records` (delta, ghi live) | Mọi provider có báo usage |
| Đổi provider / xoay session | `thread.model-switched`, `provider-session.bound/deactivated` | Mọi provider |
| Feedback mới / giao việc | Automation API `POST /v1/agents/:id/messages` (steer khi agent bận) | Provider có steer native |

Giới hạn đã biết: Codex/Cursor/ACP chưa có hook chặn → chế độ **supervised** (quan sát + MCP + AGENTS.md). Agent Full access có thể sửa file hook của chính nó → phát hiện bằng so hash, không ngăn được. Seam lõi dự phòng (chưa làm): `agent-service.ts:940-967` (`bindClient`) và `turn-context.ts:78` (`TurnContext.take`).

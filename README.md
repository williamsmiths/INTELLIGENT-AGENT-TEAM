# kongCowork Intelligence Harness

Dịch vụ Go chạy cạnh kongCowork (fork OpenBot). Nó cho agent một bộ nhớ dự án dùng chung có kiểm soát nguồn gốc, nạp context đúng project và đúng phiên bản vào mỗi lượt, chặn ghi ngoài phạm vi, cộng dồn ngân sách và chỉ công nhận kết quả khi runner của chính nó chạy check trên đúng trạng thái code.

Harness không chạy vòng lặp agent, không có scheduler riêng và không sao chép hội thoại: OpenBot vẫn sở hữu agent, lượt chạy và lịch. Xem `docs/specs/_context.md` (quyền sở hữu trạng thái), `docs/specs/harness-s1-spec.md` (đặc tả lát cắt S1, giới hạn đã biết, nhật ký review) và `docs/specs/harness-s2-spec.md` (S2: cách ly, retrieval ngữ nghĩa, Intelligence Layer).

## Bốn nhóm trách nhiệm (một tiến trình)

| Nhóm | Package |
|---|---|
| Memory & Context | `internal/memory` — Live Product Model, ba phạm vi GLOBAL / PROJECT / AGENT, tìm key → quan hệ → FTS → trigram, context packet |
| Adaptive Runtime | `internal/work` (task, lease, feedback, ngân sách), `internal/openbot` (đọc usage của OpenBot, gửi tin qua automation API), `internal/api/hooks*.go` |
| Engineering & Verification | `internal/verify` (candidate hash, check `scope`, runner, evidence), `internal/guard` (phạm vi ghi) |
| Intelligence & Collaboration | Blackboard, mục "chưa chắc chắn" và chỉ dẫn trong context packet (S1 mới ở mức này) |

Nền chung: `internal/store` (PostgreSQL, migration, Row-Level Security), `internal/attest` (danh tính theo cây tiến trình), `internal/authn`, `internal/hookinstall` (sinh và giám sát hook + sandbox), `internal/journal`.

## Vận hành (S2: hai unix socket, không token, không TCP)

`harnessd` nhận biết người gọi bằng **cây tiến trình** ở đầu kia của unix socket (`SO_PEERCRED` + `/proc`), không bằng token (`docs/specs/harness-s2-spec.md`, Phần A):

| Socket | Ai dùng | Quy tắc |
|---|---|---|
| `~/.config/kc-harness/run/owner.sock` | `kch owner`, `kch enroll`, `kch whoami` | Mọi tiến trình cùng tài khoản OS **trừ** agent đã ghi danh (và con cháu của nó) và con của `harnessd` (check của runner) → 403. Thư mục này bị che với mọi sandbox |
| `~/.local/state/kc-harness/run/agent.sock` | chỉ `kch hook` do Claude Code gọi | Chỉ nhận tiến trình nằm dưới OpenBot → provider chạy trong workspace của một agent **có principal**, không nằm trong sandbox; ngoài ra 401. Không bao giờ cấp quyền Owner; chỉ có `/v1/whoami`, `/v1/hooks/claude/*`, `/v1/agent/*` |

Giới hạn còn lại phải đọc trước khi nối dữ liệu thật: L1′ (mọi tiến trình không sandbox cùng uid là Owner) và L10–L13 trong đặc tả S2 §A.3.

### 1. Cấu hình (một lần cho mỗi máy)

Tạo `~/.config/kc-harness/` (mode 0700) với hai file.

`harnessd.env` — **mode 0600**, khác đi thì `harnessd` không chạy:

```
HARNESS_DSN=postgres://harness:<HARNESS_PG_PASSWORD>@127.0.0.1:<HARNESS_PG_PORT>/harness?sslmode=disable
# HARNESS_TAIL=off     tắt đọc openbot.db
# HARNESS_NOTIFY=on    gửi tin giao việc/feedback vào agent qua OpenBot (mặc định tắt)
```

DSN **chỉ** đọc từ file này. Nếu `HARNESS_DSN` có trong môi trường, `harnessd` bỏ qua giá trị đó và tự khởi động lại không kèm biến, để `/proc/<pid>/environ` không chứa DSN.

`machine.json` — những thứ khác nhau giữa các máy (không có secret). Ví dụ cho máy này:

```json
{
  "openbot_exe": "~/Projects/openbot/dist/linux-unpacked/openbot",
  "agents_root": "~/OpenBot/Agents",
  "pilot_tmp_root": "/var/tmp/kc-pilot/tmp",
  "sandbox_read_roots": [
    "~/.nvm", "~/.local/go", "~/.local/gopath", "~/.local/bin", "~/.local/share/pnpm", "~/.cache/node",
    "~/Projects/openbot/dist/linux-unpacked/resources/agent-runtime"
  ],
  "sandbox_env": { "GOMODCACHE": "/home/<user>/.local/gopath/pkg/mod" }
}
```

| Khóa | Ý nghĩa |
|---|---|
| `openbot_exe` | **Bắt buộc.** File thực thi của OpenBot; attest dựa vào nó, thiếu thì `harnessd` không chạy |
| `agents_root` | `<agents_root>/<openbot agent id>` là workspace của agent (mặc định `~/OpenBot/Agents`) |
| `pilot_tmp_root` | `<pilot_tmp_root>/<agent id>` là thư mục tạm riêng của agent; phải nằm **ngoài** home (mặc định `/var/tmp/kc-pilot/tmp`) |
| `sandbox_read_roots` | Thư mục toolchain agent được đọc trong sandbox và bằng tool Read/Glob/Grep (`~/.claude/shell-snapshots` được thêm tự động) |
| `sandbox_env` | Biến môi trường thêm cho Bash của agent. `TMPDIR`, `GOCACHE`, `GOPROXY=off` do `kch enroll` tự đặt |

### 2. Chạy

```bash
docker compose -f infra/compose.yaml --env-file infra/.env up -d   # PostgreSQL 17 + pgvector, 127.0.0.1:5439
go build -o bin/ ./cmd/...
bin/harnessd -check        # kiểm cấu hình + kết nối database rồi thoát
bin/harnessd               # không cần biến môi trường nào; không tạo file token
# chạy thường trực, tự khởi động lại khi chết:
cp deploy/systemd/kc-harness.service ~/.config/systemd/user/ && systemctl --user daemon-reload && systemctl --user enable --now kc-harness
bin/kch whoami             # phải ra role "owner"
```

Lần chạy đầu tạo principal Owner (không có token). Dừng bằng SIGTERM: hai file socket được gỡ; socket sót lại của một `harnessd` đã chết được thay khi chạy lại, còn nếu một `harnessd` khác đang sống thì tiến trình mới từ chối chạy.

### 3. Ghi danh một agent

Repo của project mà agent là thành viên phải nằm ngoài home (pilot: `/var/tmp/kc-pilot/repos/…`), vì cả home bị che trong sandbox.

```bash
bin/kch owner POST /v1/projects '{"slug":"demo","name":"Demo"}'
bin/kch owner POST /v1/projects/PROJECT_ID/repos '{"path":"/var/tmp/kc-pilot/repos/demo"}'
bin/kch owner PUT  /v1/projects/PROJECT_ID/reference-roots '{"paths":["/duong/dan/chi-doc"]}'   # tùy chọn
bin/kch owner POST /v1/principals '{"name":"pilot","openbot_agent_id":"agent-UUID","enforcement":"HOOK","project_id":"PROJECT_ID","role":"worker"}'
bin/kch owner PUT  /v1/principals/PRINCIPAL_ID '{"specialty":"engineering"}'                     # tùy chọn
bin/kch enroll PRINCIPAL_ID ~/OpenBot/Agents/agent-UUID
```

`kch enroll` sinh toàn bộ khối hook + sandbox từ dữ liệu (`machine.json`, repo root và reference root của các project agent là thành viên), ghi vào `<workspace>/.claude/settings.json` (0600, giữ nguyên khóa và hook không phải của harness), tạo thư mục tạm riêng và đăng ký sha256 của cả file. Chạy lại mỗi khi membership, repo, reference root hay `machine.json` đổi, và để che thư mục `/tmp/claude-<uid>/-…` của các phiên mới (L11). Giao task mode FULL cho agent HOOK chưa ghi danh hoặc có cấu hình bị sửa → 409.

Sau khi ghi danh:

- Bash của agent chạy trong sandbox, **không có socket nào**. Lệnh `kch status | context | verify | release | search … | note … | propose … | remember …` phải là **một lệnh Bash riêng** (không `&&`, `;`, `|`, không chuyển hướng; nội dung dài dùng heredoc có nháy: `kch <verb> <<'END'`): hook `PreToolUse` gọi `POST /v1/agent/<verb>` và thay lệnh bằng việc in kết quả. Project và task luôn lấy từ lease.
- Tool chạy ngoài sandbox bị hook kiểm theo allowlist ở mọi mode: Read/Glob/Grep chỉ trong workspace, repo và reference root của project mình, toolchain, thư mục tạm riêng; Write/Edit chỉ trong repo của task và thư mục tạm riêng; `mcp__*`, WebFetch, WebSearch bị từ chối.
- `harnessd` không chạy → mọi tool của workspace đã ghi danh bị từ chối (fail-closed); được ghi vào `~/.local/state/kc-harness/failopen.log` và vào journal (`HOOK_FAILCLOSED`) khi `harnessd` chạy lại.
- `settings.json` đổi, hoặc xuất hiện `settings.local.json` / `.mcp.json` trong workspace → mọi tool bị từ chối, journal `HOOK_TAMPERED`, cho tới khi khôi phục hoặc `kch enroll` lại.

Xem journal hệ thống (attest bị từ chối, hook bị sửa, usage chưa có giá): `bin/kch owner GET '/v1/system/events?limit=100'`.

## Kiểm thử

```bash
set -a; . infra/.env; set +a
export HARNESS_TEST_DSN="postgres://harness:${HARNESS_PG_PASSWORD}@127.0.0.1:${HARNESS_PG_PORT}/harness?sslmode=disable"
go build ./... && go vet ./... && go test -count=1 ./...
```

Test tích hợp dùng PostgreSQL thật (mỗi test một database mới) và repo git thật; thiếu `HARNESS_TEST_DSN` thì các test đó bị bỏ qua, không phải pass. Các test của Phần A (S2) dựng cây tiến trình thật, unix socket thật và chạy binary `kch`, `harnessd` thật; chúng dùng HOME tạm, không đụng `~/.config/kc-harness`. Kiểm trên agent pilot thật là AC-S2-012 (người vận hành chạy sau khi ghi danh). Script `evals/smoke/run_smoke.py` của S1 dùng token và chạy `claude` ngoài OpenBot nên đã bỏ.

## Sao lưu

Mọi dữ liệu chính thức nằm trong volume `kc-harness-pgdata`:

```bash
docker exec kc-harness-pg pg_dump -U harness -Fc harness > harness-$(date +%F).dump
docker exec -i kc-harness-pg pg_restore -U harness -d harness --clean < harness-YYYY-MM-DD.dump
```

Log evidence nằm ở `~/.local/state/kc-harness/evidence/`.

## Đội V5 và kết quả đo

- Cách làm việc với đội Product / Design / Engineering / Quality Lead: `docs/07-TEAM-INTERACTION.md`; role ở `prompts/roles/`.
- Giao việc giữa agent (`kch delegate`) và gắn channel/thread với project: `docs/specs/harness-s2-team-spec.md`.
- Kết quả A/B/C và pilot đội trên agent thật: `docs/V5-PILOT-REPORT.md`.

Chưa có: UI web, index code bằng Tree-sitter/LSP, hook chặn cho Codex/Cursor/ACP, merge queue (nhận việc VERIFIED vẫn do Owner commit), MCP server.

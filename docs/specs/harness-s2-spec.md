# Harness S2 — cách ly, retrieval ngữ nghĩa, Intelligence Layer (delta trên S1)

Nguồn: chỉ đạo của chủ ngày 2026-11-17 (8 mục), Master Prompt V5.0, `harness-s1-spec.md` (giữ nguyên trừ chỗ ghi "thay" ở đây). Baseline code: commit `206f56c`. Không thêm framework ngoài, không thêm scheduler hay vòng lặp agent.

Bằng chứng thực nghiệm làm nền (Claude Code 2.1.323, 2026-11-17, thư mục tạm):
- Sandbox Bash của Claude Code (bwrap) với `filesystem.denyRead` làm thư mục biến mất; ghi ngoài `allowWrite` → "Read-only file system"; cổng TCP localhost và Docker socket không với tới được khi `/run` bị che.
- `permissions.deny: Read(//<abs>/**)` chặn tool Read (mẫu `Read(/abs/**)` thì **không** chặn).
- `network.allowAllUnixSockets: true` cho Bash trong sandbox gọi được unix socket nằm ngoài `/run`.
- Từ phía server, `SO_PEERCRED` + `/proc` cho chuỗi: `kch ← bash ← bwrap ← bwrap ← … ← claude ← openbot`; tiến trình `setsid nohup` trong sandbox vẫn nằm dưới `bwrap`. Hook chạy ngoài sandbox: `kch ← sh ← claude ← openbot`.
- Chưa kiểm: OpenBot (Full access) có giữ nguyên khối `sandbox` của `.claude/settings.json` trong workspace hay không → AC-S2-012 kiểm trên agent pilot thật.

## Phần A — Cách ly credentials và execution (bản 2, sau REV-004)

Thử thêm sau REV-004 (cùng ngày): sandbox của Claude Code có pid namespace riêng (`/proc` chỉ thấy 5 tiến trình, `/proc/1/root` bị từ chối); `allowRead` mở lại được thư mục con của một thư mục `denyRead`; hook `PreToolUse` trả `updatedInput` viết lại được lệnh Bash; OpenBot (Full access) giữ nguyên khối `sandbox` của workspace và `permissions.deny Read(//…)` có hiệu lực (kiểm trên agent pilot thật qua automation API); `/tmp/claude-1000` (TMPDIR dùng chung của mọi phiên Claude) **không** che được bằng `denyRead`; vùng ghi nằm trong một thư mục home đã che thì thành chỉ-đọc → vùng làm việc của pilot đặt ở `/var/tmp/kc-pilot`.

### A.1 Mô hình

- **Không còn token.** Hai unix socket, không nghe TCP:
  - `agent.sock` (`~/.local/state/kc-harness/run/`): chỉ `kch hook` dùng. Chỉ nhận peer attest là agent đã ghi danh; **không bao giờ** cấp quyền Owner.
  - `owner.sock` (`~/.config/kc-harness/run/`, thư mục bị che với mọi sandbox): API Owner. Từ chối peer attest là agent ghi danh và peer có `harnessd` trong chuỗi tổ tiên (con của runner). Mọi tiến trình khác cùng uid với tới được socket này là Owner — xem L1′.
- **Attest** (`internal/attest`): chuỗi tổ tiên của peer từ `SO_PEERCRED` + `/proc` (pid, starttime, exe, cwd); starttime của peer trước và sau phải trùng. `host` = tổ tiên xa nhất có exe = `OpenBotExe`; `provider` = phần tử ngay dưới `host`; `agent_id` = thư mục con đầu của `provider.cwd` dưới `AgentsRoot`. Không có `host`, không ra `agent_id`, hoặc agent không có principal → 401 trên `agent.sock` (mặc định từ chối, journal `ATTEST_REJECTED` có giới hạn tần suất).
- **Bash của agent không có unix socket nào** (không bật `allowAllUnixSockets`). Lệnh `kch` do agent gõ đi **qua hook**: khi lệnh Bash là đúng một lời gọi `kch <verb> …` (cho phép heredoc làm body), `kch hook pre-tool` (chạy ngoài sandbox, do Claude Code gọi với dòng lệnh cố định của trình cài) thực hiện `POST /v1/agent/<verb>` rồi trả `updatedInput` thay lệnh bằng việc in kết quả. Lệnh ghép (`kch … && …`) không được xử lý: agent được nhắc gọi `kch` thành lệnh riêng.
- **`kch hook` không tin môi trường:** đường dẫn socket và cờ `--enrolled` nằm trên dòng lệnh do trình cài ghi; biến `KCH_*` bị bỏ qua. Với workspace ghi danh, `pre-tool` **fail-closed**: không gọi được `harnessd` → từ chối tool (ghi log cục bộ như L3); các hook khác fail-open.
- **Cấu hình hiệu lực được giám sát.** Mỗi lần hook chạy, `kch hook` gửi sha256 của `<workspace>/.claude/settings.json` và việc có/không `settings.local.json`, `.mcp.json`; lệch với bản đăng ký → mọi `pre-tool` bị từ chối, journal `HOOK_TAMPERED`.
- **Sandbox do `kch enroll` sinh** (dữ liệu, không viết tay), ghi vào `<workspace>/.claude/settings.json`:
  - `denyRead`: thư mục home, `/run`, `/var/run`, và từng thư mục `/tmp/claude-<uid>/-…` của phiên khác đang có lúc ghi danh.
  - `allowRead`: workspace của agent, `~/.claude/shell-snapshots`, các thư mục toolchain khai trong cấu hình máy (`sandbox_read_roots`), repo root của project agent là thành viên, "reference root" chỉ-đọc Owner khai cho project.
  - `allowWrite`: repo root của project agent là thành viên (phải nằm ngoài home), thư mục tạm riêng. `denyWrite`: `<repo>/.git` của mọi repo root, `<workspace>/.claude`, `<workspace>/.mcp.json`.
  - `allowUnsandboxedCommands: false`; `env`: `GOMODCACHE`, `GOCACHE`, `GOPROXY=off`, `TMPDIR` riêng.
  - `permissions.deny`: `Read(//<home>/**)` trừ các vùng mở, `Edit(//<workspace>/.claude/**)`, `WebFetch`, `WebSearch`, mọi tool `mcp__*`.
- **Tool chạy ngoài sandbox bị hook kiểm theo allowlist ở mọi mode, có lease hay không** (principal enforcement HOOK): Read/Glob/Grep chỉ trong vùng `allowRead` (trừ `/tmp/claude-<uid>` của phiên khác); Write/Edit/NotebookEdit chỉ trong repo của task (FULL: theo `allowed_paths`; OBSERVE/CONTEXT: cả repo root trừ đường dẫn luôn-bảo-vệ) và thư mục tạm riêng; `mcp__*`, WebFetch, WebSearch → từ chối; tool lạ có tham số đường dẫn → từ chối.
- **Runner:** như bản 1 và thêm `--proc /proc` (không thấy pid host), che cả hai thư mục socket và state dir; `network: true` chỉ có hiệu lực khi task do Owner tạo. DSN không nằm trong biến môi trường của `harnessd` (đọc từ file trong thư mục bị che).
- **Trần cứng cho mọi mode:** ngân sách SYSTEM EXHAUSTED → `pre-tool` từ chối mọi tool của principal HOOK kể cả lease OBSERVE/CONTEXT (đây là trần an toàn, không phải cơ chế Intelligence).

### A.2 Permission Matrix (delta)

| ID | Role | Module | Operation | Data scope | Ghi chú |
|---|---|---|---|---|---|
| PM-S2-01 | Owner (peer hợp lệ trên `owner.sock`) | mọi endpoint owner-only và endpoint Owner của S1 | như S1 | tất cả | Thay token Owner |
| PM-S2-02 | Lead, Worker (attest trên `agent.sock`) | `/v1/hooks/claude/*`, `/v1/agent/*` | như S1 + phần C | lease / membership của chính mình | Không có route nào khác trên `agent.sock` |
| PM-S2-03 | Peer không attest được | — | không | — | 401 |
| PM-S2-04 | Owner | Principal; Project | Sửa `specialty`; khai `reference_roots` | tất cả | |

### A.3 Giới hạn còn lại (thay L1 của S1) — phải đọc trước khi nối dữ liệu thật

Ranh giới chỉ tồn tại với **agent đã ghi danh và chạy trong sandbox**. Ngoài ra:

| # | Còn hở | Trạng thái |
|---|---|---|
| L1′ | Mọi tiến trình khác cùng uid — 10 agent Full access cũ, Cursor, Codex, Claude remote, tmux — với tới `owner.sock`, đọc được DSN, và thuộc nhóm `docker` (tương đương root). Chúng thực chất là Owner | Không sửa được nếu không tách uid (cần sudo của chủ). Chưa nối dữ liệu production |
| L1″ | Người vận hành (agent đang cấu hình hệ thống) nếu bị prompt-injection cũng là Owner | Người vận hành không là thành viên project nào, không nhận tham vấn |
| L10 | Bash trong sandbox ghi được cả repo root (trừ `.git`), không theo `allowed_paths` | post-tool và verify bắt (như L4) |
| L11 | `/tmp/claude-<uid>` dùng chung: agent đọc được file tạm của phiên Claude tạo **sau** lúc ghi danh | Ghi danh lại để làm mới; không để oracle hay secret trong `/tmp` |
| L12 | Biến môi trường của OpenBot (tên token nội bộ, URL) lọt vào Bash của agent | Không có socket và mạng bị giới hạn nên không dùng được; chưa chứng minh là vô hại tuyệt đối |
| L13 | Token đầu ra trong `openbot.db` bị ghi thiếu (đo: 17 so với 2.595 trong transcript của Claude) và có dòng không có chi phí | Tailer tự tính chi phí từ token theo bảng giá khi thiếu; phép đo A/B/C lấy token từ transcript |

- **L14 — Thư mục Shared của OpenBot đọc được từ Bash của agent đã ghi danh** (đo trên pilot đội V5, 2026-11-18). OpenBot đưa `~/OpenBot/Shared` vào `additionalDirectories` của mọi phiên Claude, và sandbox của Claude Code 2.1.323 vẫn cho Bash đọc thư mục đó dù harness có `denyRead`. Tool Read/Edit thì bị chặn (permissions.deny + toolguard). Hệ quả: agent có thể đọc tài liệu của project khác và file người dùng gửi (Transfers) nằm trong Shared. Giảm thiểu hiện tại: không để dữ liệu riêng của project trong Shared. Sửa tận gốc cần OpenBot không truyền thư mục Shared cho agent đã ghi danh harness (thay đổi runtime OpenBot, cần chủ sản phẩm duyệt và khởi động lại server).

### A.4 API (delta)

| Method | Path | Socket | Role | Mục đích |
|---|---|---|---|---|
| GET | /v1/whoami | cả hai | mọi peer | Kết quả attest của chính peer |
| POST | /v1/agent/{verb} | agent | PM-S2-02 | Lệnh của agent (qua hook); project và task lấy từ lease; không có lease thì từ cuộc hội thoại đã gắn (Phần E, E.9-4), rồi từ membership duy nhất |
| POST | /v1/hooks/claude/{event} | agent | PM-S2-02 | Như S1, body thêm `settings_sha256`, `extra_config` |
| PUT | /v1/principals/{id} · /v1/projects/{p}/reference-roots | owner | PM-S2-04 | |
| mọi route S1 khác | | owner | Owner | Route của Lead/Worker trong S1 chỉ còn đi qua `/v1/agent/*` |

### A.5 Acceptance Criteria

```gherkin
Scenario: AC-S2-001 Attest trên agent.sock
  Given cây tiến trình thật: tiến trình có exe OpenBotExe → provider (cwd AgentsRoot/agent-X) → client
  Then client attest là agent-X; agent-X chưa có principal → 401

Scenario: AC-S2-002 Không giả danh được
  When client của agent-X đổi cwd sang workspace agent-Y, đặt KCH_AGENT, hoặc chèn tiến trình trung gian có exe OpenBotExe với con cwd agent-Y → vẫn là agent-X
  When tiến trình không có OpenBotExe trong tổ tiên, hoặc đã tách khỏi cây → 401 trên agent.sock

Scenario: AC-S2-003 agent.sock không bao giờ cấp Owner; owner.sock từ chối agent và runner
  When agent ghi danh gọi route owner-only qua agent.sock → 404/403
  When tiến trình là con của provider của agent ghi danh, hoặc có harnessd trong tổ tiên, gọi owner.sock → 403
  When tiến trình khác cùng uid gọi owner.sock → 2xx

Scenario: AC-S2-004 Lệnh agent đi qua hook
  When pre-tool nhận Bash "kch verify" → updatedInput in ra kết quả của POST /v1/agent/verify; lệnh gốc không chạy
  When "kch understand <<'EOF' {json} EOF" → body JSON tới server nguyên vẹn
  When "kch verify && rm -rf x" hoặc "echo kch verify" → không xử lý như lệnh kch
  When `kch <verb>` ở vị trí lệnh sau cd/&&, ;, |, xuống dòng ("cd /x && kch understand <<'END' …", "ls; kch status", "true | kch status") → từ chối kèm gợi ý "chạy đúng một lệnh `kch <verb> …` riêng, không `cd … &&`, không ghép; cwd không cần", kch_verb = false, lệnh không chạy; "grep -n kchx file", "echo kch status" vẫn là lệnh thường

Scenario: AC-S2-005 Fail-closed và chống sửa cấu hình
  When harnessd không chạy: pre-tool của workspace ghi danh → deny; prompt-submit → không chặn
  When settings.json đổi hash, hoặc xuất hiện settings.local.json / .mcp.json → pre-tool deny, journal HOOK_TAMPERED
  When đặt KCH_SOCKET trỏ sang socket khác → bị bỏ qua

Scenario: AC-S2-006 Không còn secret
  Then harnessd vận hành không tạo file token, không nghe TCP, bỏ qua header Authorization; DSN không có trong /proc/<harnessd>/environ

Scenario: AC-S2-007 Allowlist của tool ở mọi mode
  Given principal HOOK, lần lượt: không lease, lease OBSERVE, CONTEXT, FULL
  When pre-tool cho Read/Glob/Grep trỏ vào ~/.config, repo của project khác, /proc/1/environ, thư mục tạm của phiên khác → deny
  When Write/Edit tới ~/.bashrc, ~/.claude/settings.json, binary kch, workspace/.claude/x → deny
  When tool mcp__openbot__send_message, WebFetch → deny
  When Read trong repo của mình và reference root → allow

Scenario: AC-S2-008 Trần SYSTEM áp cho mọi mode
  Given ngân sách SYSTEM EXHAUSTED → pre-tool của lease OBSERVE, CONTEXT, FULL đều deny

Scenario: AC-S2-010 Runner cô lập (như phần D) cộng: check không thấy pid host, không với tới owner.sock/agent.sock, không đọc được DSN; network:true của task do Lead tạo bị bỏ qua

Scenario: AC-S2-011 kch enroll sinh cấu hình đúng, idempotent, đăng ký hash; giao task FULL khi hook_install chưa đăng ký hoặc đang tamper → 409

Scenario: AC-S2-012 (chạy thật trên agent pilot OpenBot, qua automation API)
  Then trong Bash của agent: ~/.config, ~/Projects, ~/.ssh, workspace agent khác, thư mục tạm của phiên khác không đọc được; ghi vào home và vào <repo>/.git thất bại; không kết nối được socket nào (docker, tmux, harness); test của hai repo pilot chạy được; `kch status` qua hook trả đúng agent
```

## Phần B — Retrieval ngữ nghĩa

### B.1 Quy tắc

- Thứ tự giữ nguyên: khớp key → quan hệ → FTS → trigram; **semantic là tầng thứ năm**, chỉ bổ sung kết quả chưa có, không thay thứ hạng của tầng có cấu trúc. Với context packet, semantic chỉ dùng để chọn mục GLOBAL/PROJECT experience và mục "liên quan" khi các tầng trước trả ít hơn giới hạn.
- Embedding chạy local qua một sidecar HTTP (`/v1/embeddings` kiểu OpenAI) khai báo trong `infra/compose.yaml`; `internal/embed` là client + hàng đợi. Không gọi dịch vụ ngoài máy.
- Bảng `knowledge_embedding(knowledge_id, model, dim, content_hash, embedding vector)`. Chỉ embed khi `content_hash` chưa có bản ghi cho model hiện hành; nội dung không đổi không bao giờ embed lại. Việc embed chạy nền sau khi ghi kiến thức; ghi không chờ embed. Sidecar lỗi → tìm kiếm vẫn chạy bốn tầng đầu, kết quả ghi `semantic: unavailable`.
- Lọc phạm vi **trước** khi so vector: câu truy vấn vector join với `knowledge` dưới RLS của người gọi (project, level, trust hiện hành); không tìm toàn bảng rồi lọc.
- Embedding của câu truy vấn được cache theo (model, hash văn bản) trong bộ nhớ, có giới hạn.
- **Điều kiện bật:** `evals/retrieval` so ≥ 2 model local trên fixture mở rộng (≥ 60 truy vấn: Việt có/không dấu, Anh, code, diễn đạt lại, hỏi chéo ngôn ngữ; 2 project). Chỉ bật model đạt recall@5 nhóm "diễn đạt lại + chéo ngôn ngữ" cao hơn bản không embedding ít nhất 0,20 tuyệt đối và không làm giảm nhóm nào khác; nếu không model nào đạt → không bật, ghi kết quả.

### B.2 Acceptance Criteria

```gherkin
Scenario: AC-S2-020 Chỉ embed nội dung đổi
  When tạo mục, ghi lại cùng nội dung (AC-H-101), xác nhận (đổi trust), rồi supersede bằng nội dung mới
  Then sidecar nhận đúng 2 yêu cầu embed (bản đầu và bản mới)

Scenario: AC-S2-021 Semantic bổ sung, không thay thế
  Given truy vấn khớp key một mục và gần nghĩa mục khác
  Then mục khớp key đứng trước; mục semantic có stage "semantic"; không mục nào xuất hiện hai lần

Scenario: AC-S2-022 Cách ly project ở tầng vector
  Given hai project có mục cùng nội dung
  When thành viên A tìm bằng câu diễn đạt lại
  Then không có hit của B; chạy câu SQL vector không có điều kiện project dưới scope của A vẫn không thấy hàng của B

Scenario: AC-S2-023 Sidecar hỏng
  When sidecar không phản hồi
  Then tìm kiếm trả kết quả bốn tầng đầu trong thời hạn, semantic "unavailable"; ghi kiến thức không lỗi; hàng đợi embed thử lại sau

Scenario: AC-S2-024 Báo cáo chọn model
  Then evals/retrieval/report.json có recall@5, MRR, p50/p95 theo nhóm cho từng cấu hình (không embedding, từng model), kèm quyết định bật/không bật theo điều kiện B.1
```

## Phần C — Intelligence Layer

Tám năng lực của chủ ánh xạ vào **cơ chế runtime** dưới đây. Harness không gọi model; mọi suy luận do agent OpenBot làm, harness quyết định khi nào bắt buộc, khi nào cho phép, khi nào dừng, và đo. Các cơ chế chỉ áp cho lease mode FULL; CONTEXT chỉ nhận packet; OBSERVE không nhận gì.

### C.1 Router (Adaptive Reasoning)

Tính khi giao task và mỗi khi `revision` đổi, lưu trên lease (`intel_mode`, `intel_signals`):
- `F` = số mục kiến thức hiện hành cách key được ghim 1 bước quan hệ + số mục AUTHORITATIVE cùng `scope` chưa được ghim.
- `U` = số mục CANDIDATE cùng `scope` (không tính giả định `ASM-*` của task đã kết thúc, cũng không tính giả định `ASM-<key>-n` của chính task — REV-005 #12).
- `B` = 2 nếu task không ghim requirement nào (chỉ có brief), ngược lại 0.
- Điểm = F + 2U + B. DIRECT ≤ 1; ASSISTED 2–5; DEEP ≥ 6. Owner có thể ép mode khi giao. Ngưỡng là mặc định đề xuất, hiệu chỉnh theo số đo. Không đổi model hay mức suy luận của agent (giữ điều kiện đo công bằng).

Mỗi lease có `intel_features` ⊆ {gate, questions, consult, convergence, lesson} để tắt từng cơ chế; cơ chế bị tắt không chặn, không nhắc. **Mặc định (Task 2, 2026-11-18):** đủ cả ở DEEP; ở ASSISTED và DIRECT thì đủ cả **trừ `gate`**. Bằng chứng: 9 lượt thật mỗi cấu hình, C có cổng 0,0287 USD/lượt, cùng harness không cổng (G) 0,0199 USD/lượt, oracle như nhau (9/9). Owner vẫn bật cổng bằng `intel_features` (AC-S2-040).

| | DIRECT | ASSISTED | DEEP |
|---|---|---|---|
| Phải nộp bản hiểu trước lần ghi đầu vào repo (khi `gate` bật) | không | chỉ khi Owner bật `gate` | có (mặc định) |
| Số lần tham vấn được phép | 0 | 1 | 3 |
| Ngân sách ký tự của packet | 3000 | 6000 | 9000 |

### C.2 Bản hiểu (Active Discovery, Live Product Understanding, Uncertainty)

`kch understand` (JSON qua stdin) → `POST /v1/agent/understand`:
`{goal, preserve:[key], affected:[key|path], not_applicable:[{key,reason}], assumptions:[{text,basis,evidence}], questions:[{text,options[],default,impact,blocking}]}`.

Server kiểm cấu trúc so với product model (không chấm nội dung):
- U1: mọi key được ghim nằm trong `preserve` hoặc `affected`.
- U2: mọi mục thuộc tập `F` nằm trong `preserve`, `affected` hoặc `not_applicable` (có lý do).
- U3: mọi mục thuộc tập `U` được nhắc trong `assumptions`, `questions` hoặc `not_applicable`.
- U4: mỗi câu hỏi có ≥ 2 phương án và một mặc định; tối đa 3 câu `impact: high`.
- U5: task không ghim requirement nào (chỉ có brief) phải có ít nhất một giả định hoặc câu hỏi; lý do `not_applicable` dài tối thiểu 15 ký tự và không dùng được cho key đang ghim.
- Trượt → 422 kèm danh sách mục chưa xử lý (key + tiêu đề). Sau 2 lần trượt, lần thứ ba được nhận kèm `unaddressed` và journal `UNDERSTANDING_FORCED` (không khóa agent vô hạn).
- Ghi bằng Bash trước khi bản hiểu được nhận: `post-tool` trả block (hoàn tác hoặc nộp bản hiểu) và verify trả 409 cho tới khi có bản hiểu.
- Đây là kiểm cấu trúc: agent có thể khai cho qua. Thứ được bảo đảm là mọi mục liên quan đã được **đặt trước mặt** agent và lựa chọn của nó được ghi lại để chủ xem; chất lượng hiểu chỉ đo được bằng A/B/C.

Khi nhận: giả định lưu thành mục CANDIDATE (`kind` decision, key `ASM-<task>-n`, `basis` theo khai báo); câu hỏi lưu vào `question`. Câu hỏi `blocking` → task WAITING_DECISION (thêm lý do `question`), hook từ chối ghi repo cho tới khi Owner trả lời — gồm **mọi lệnh Bash** của người giữ lease trừ một lệnh `kch <verb>` đơn (hook client gửi cờ `kch_verb` khi `parseKch` nhận ra lệnh đó; lệnh ấy luôn bị thay bằng lời gọi verb, không bao giờ chạy như Bash), và post-tool chặn một Bash đã chạy từ trước nếu nó đổi working tree so với lúc bắt đầu chờ (REV-005 #2); câu không blocking → agent đi tiếp theo mặc định. Owner trả lời (`POST /v1/questions/{id}/answer`, hoặc xem danh sách bằng `GET /v1/questions?status=OPEN`) → tạo mục AUTHORITATIVE `kind` decision; nếu khác mặc định agent đã dùng → xử lý như feedback (revision + 1, T-H-22, T-H-04 nếu đang VERIFIED); nếu trùng → chỉ xác nhận. Chủ trả lời trong khung chat của OpenBot thì harness không biết: câu hỏi blocking còn OPEN quá 10 phút được journal `QUESTION_WAITING` để người vận hành nhập hộ đúng nguyên văn của chủ.

Hook `stop` (FULL) chặn **tối đa một lần mỗi lượt**, gộp mọi việc còn thiếu vào một thông báo: evidence (S1), giả định/câu hỏi OPEN chưa nêu cho chủ, bài học được yêu cầu (C.6).

### C.3 Tham vấn có chọn lọc

`kch consult <specialty> <câu hỏi> [--refs k1,k2]` → `POST /v1/agent/consult`:
- Chỉ người giữ lease; còn lượt theo mode (+1 đúng một lần cho cả task khi C.4 cấp); câu hỏi trùng (hash chuẩn hóa) trả lại bản cũ, không gửi lại.
- Người trả lời = thành viên cùng project có `specialty` đó, khác người hỏi, **không giữ lease ở project khác**; nhiều người đủ điều kiện → người có ít tham vấn OPEN nhất, rồi tới người được tạo sớm nhất; không có ai → 409.
- Nội dung câu hỏi tới người trả lời được bọc là dữ liệu chưa kiểm chứng, tối đa 1.200 ký tự; câu trả lời tối đa 1.500 ký tự.
- Gửi qua OpenBot (automation `messages`): câu hỏi + tham chiếu (key, tiêu đề, trust), không kèm hội thoại; hạn 15 phút tính từ lúc tạo.
- **Gửi thất bại (quyết định sau REV-005 #4).** Tham vấn mới ở trạng thái `PENDING_SEND`; request của agent thử gửi **một lần trong tối đa 5 s** (mọi lời gọi host trong request của agent đều có trần tổng 5 s, kể cả retry). Gửi được → `OPEN` (lưu delivery id). Host lỗi, 429 (giới hạn 30 tin/agent/giờ của host) hoặc treo → giữ `PENDING_SEND`, worker mỗi phút gửi lại theo backoff 1, 2, 4, 8 phút (cùng `clientMessageId`) cho tới hạn. Tới hạn mà chưa gửi được → `FAILED`, journal `CONSULT_FAILED`, người hỏi được báo, **lượt được hoàn lại** và câu hỏi đó hỏi lại được. Tham vấn đã gửi mà quá hạn vẫn là `EXPIRED`, không hoàn lượt. `PENDING_SEND` vẫn tính vào lượt (không spam). Việc tra lượt (turn) của host cho C.7 chỉ do worker làm.
- `kch answer <id>` → `POST /v1/agent/answer` chỉ nhận từ đúng người trả lời, project lấy từ bản ghi tham vấn (không từ lease); người trả lời đang giữ lease ở project khác lúc trả lời → 409; lưu thành blackboard `finding` kèm refs; `--refs` chỉ giữ key kiến thức hiện hành của project tham vấn, hiển thị key + tiêu đề + trust **bên trong** khối dữ liệu chưa kiểm chứng; người hỏi nhận ở hook kế tiếp và qua OpenBot.
- Độ sâu ≤ 1 theo cấu trúc: tham vấn đòi lease trên chính task đó, người trả lời không giữ lease của task nên không mở tham vấn tiếp cho task đó.
- Hết hạn → EXPIRED, người hỏi được báo đi tiếp bằng giả định; lượt không hoàn lại.

### C.4 Hội tụ (Reasoning Convergence)

- Mỗi lần verify của người giữ lease ghi một `attempt` (candidate, tập check PASS/FAIL, fingerprint lỗi). Không tiến triển = tập check FAIL và fingerprint lỗi **giống hệt** attempt trước (lỗi đổi được coi là có tiến triển; vòng lặp lỗi đổi liên tục do ngân sách chặn).
- 2 lần liên tiếp không tiến triển → phản hồi verify và hook kế tiếp kèm chỉ dẫn đổi giả thuyết theo tầng (yêu cầu / context / kiến trúc / test / cài đặt), cấp thêm 1 lượt tham vấn; journal `NONPROGRESS`.
- 3 lần → task WAITING_DECISION (lý do `nonprogress`), hook từ chối ghi (cả Bash trừ `kch <verb>` đơn, như C.2); Owner `POST /v1/tasks/{id}/resume` (ghi chú, đặt lại bộ đếm) hoặc thu hồi.

### C.5 Feedback và tái lập kế hoạch từng phần

- Feedback (S1) giữ nguyên phần thay lease. Thêm: server tìm trong project các mục liên quan tới nguyên văn feedback (tìm kiếm của phần B, tối đa 5) và nối vào thông báo: "có thể liên quan" và "giữ nguyên" (các key được ghim không nằm trong danh sách liên quan).
- Lease có `gate` (mặc định: DEEP; ASSISTED khi Owner bật): sau feedback hoặc đổi requirement, agent phải nộp **delta** (`understand` với `delta: {changed:[key|path], preserved:[key]}`; mọi key được ghim phải nằm ở một trong hai; tối đa 3 lần trượt rồi được nhận kèm journal) trước khi ghi repo tiếp. Khi verify, runner so `changed` với file thật sự đổi từ lúc có feedback và ghi chênh lệch vào evidence `detail.delta_mismatch` (đo, không chặn). DIRECT: như S1 (từ chối một lần kèm thông báo).

- **Feedback khi không có lease** (phát hiện khi chạy thật 2026-11-17: agent tự nhả lease ngay sau VERIFIED): lease cấp sau đó cho task có `pending_notice` = nguyên văn mọi feedback và mọi câu trả lời khác mặc định nhận được kể từ khi lease trước kết thúc (theo thứ tự thời gian, có revision), và DIRECT cũng bị từ chối một lần ở lần ghi đầu; ASSISTED/DEEP phải nộp delta như trên. AC-S2-039.

### C.6 Kinh nghiệm

- Khi verify ALL PASS trên một lease từng có attempt FAIL, `SCOPE_VIOLATION` hoặc feedback: phản hồi kèm `lesson_requested` và các dữ kiện (check từng FAIL, số attempt). Hook `stop` chặn một lần nếu chưa có bài học.
- `kch lesson` → `POST /v1/agent/lesson {cause, fix, applies_when, limits}` (mỗi trường tối thiểu 15 ký tự) → mục CANDIDATE `kind` experience, level PROJECT, `scope` của task, nguồn là một Source origin RUNTIME chứa dữ kiện của các attempt.
- Bài học thiếu chặn `stop` **một lần cho mỗi lease** (journal `LESSON_STOP_BLOCKED`), không phải mỗi lượt.
- Owner xác nhận → packet của task cùng `scope` trong project nhận tối đa 3 kinh nghiệm AUTHORITATIVE (ưu tiên khớp FTS/semantic với task). Đưa lên GLOBAL vẫn theo luồng S1.

### C.7 Tách chi phí

- `budget_ledger.phase`: DISCOVERY (từ lúc giao tới khi bản hiểu được nhận; DIRECT bỏ qua), BUILD, REWORK (từ attempt FAIL đầu tiên tới VERIFIED), COLLAB. Phase lấy theo **thời điểm `occurred_at`** của bản ghi usage (dựng lại từ phase lúc cấp lease trong `LEASE_GRANTED`, thời điểm `UNDERSTANDING_ACCEPTED/FORCED` của lease và attempt mới nhất của task tới lúc đó), không theo lúc tailer đọc (REV-005 #10).
- Usage của lượt tham vấn ghi trước khi biết `turnId` nằm tạm ở task của người trả lời; khi chuyển sang task người hỏi, trạng thái ngân sách nơi bị trừ được tính lại, và task của người trả lời đã WAITING_DECISION vì ngân sách do khoản đó sẽ tiếp tục (T-H-07).
- COLLAB gắn theo **lượt**: tin tham vấn gửi qua OpenBot trả về delivery, từ đó lấy `turnId`; chỉ usage của đúng lượt đó được cộng vào task của người hỏi (không đụng usage khác của người trả lời, không đếm đôi).
- `packet_log` ghi số ký tự mỗi packet đã nạp (để biết lượng context đã bơm). Chi phí của Memory và của Intelligence **không** suy ra từ ước lượng: lấy bằng hiệu giữa các cấu hình A/B/C trên cùng bài.
- `GET /v1/tasks/{id}/cost` (Owner, Lead, người giữ lease): theo phase, theo provider/model, ước lượng Memory, thời gian runner.

### C.8 Permission Matrix và State Machine (delta)

| ID | Role | Module | Operation | Data scope |
|---|---|---|---|---|
| PM-S2-10 | Người giữ lease (FULL) | Understand, Question, Consult, Lesson | Tạo | task mình giữ lease |
| PM-S2-11 | Người được hỏi | Consult | Trả lời | tham vấn gửi cho mình |
| PM-S2-12 | Owner | Question | Trả lời | tất cả |
| PM-S2-13 | Owner | Task | Resume (WAITING_DECISION do `nonprogress`), ép `intel_mode` | tất cả |
| PM-S2-14 | Runtime | Attempt, ledger phase, router, packet log | Tạo | tất cả |
| PM-S2-15 | Owner, Lead, người giữ lease | Cost, Questions, Understanding | Xem | Owner: tất cả; Lead: project mình; holder: task mình |

| ID | Từ | Đến | Ai | Điều kiện | Side effect |
|---|---|---|---|---|---|
| T-S2-01 | RUNNING | WAITING_DECISION | Runtime | Câu hỏi `blocking` được nhận, hoặc 3 attempt không tiến triển | Giữ lease; journal `QUESTION_BLOCKING` / `NONPROGRESS_PARKED` |
| T-S2-02 | WAITING_DECISION | RUNNING / RUNNABLE / giữ nguyên | Owner | Trả lời hết câu hỏi blocking → gỡ lý do `question` | Thông báo cho agent |
| T-S2-03 | WAITING_DECISION | RUNNING / RUNNABLE / giữ nguyên | Owner | Resume → gỡ lý do `nonprogress` | Đặt lại bộ đếm |

Gỡ lý do khóa dòng task bằng một câu lệnh riêng **trước** khi đọc lease (mọi đường kết thúc lease đều giữ khóa đó), nên không bao giờ có RUNNING mà không có lease ACTIVE (REV-005 #1). Bất biến: `RUNNING ⇒ đúng một lease ACTIVE`.

`task.waiting_reasons` là **tập** ⊆ {budget, question, nonprogress}. Task ở WAITING_DECISION khi tập khác rỗng; mỗi sự kiện gỡ đúng lý do của nó (T-H-07 gỡ budget, T-S2-02 gỡ question, T-S2-03 gỡ nonprogress). Khi tập rỗng: RUNNING nếu lease còn ACTIVE, ngược lại RUNNABLE (như T-H-07). Lease hết hạn trong lúc chờ: task giữ WAITING_DECISION (S1), khi tập rỗng thì về RUNNABLE.

### C.9 API (delta)

| Method | Path | Role | Mục đích |
|---|---|---|---|
| POST | /v1/agent/understand · ask · consult · answer · lesson | PM-S2-10, PM-S2-11 | Phần C.2–C.6 |
| POST | /v1/agent/status · context · search · note · propose · remember · verify · release | người gọi | Bí danh theo lease của các endpoint S1 |
| GET | /v1/tasks/{id}/questions · understanding · cost; /v1/questions?status= | PM-S2-15 | Xem |
| POST | /v1/questions/{id}/answer | Owner | PM-S2-12 |
| POST | /v1/tasks/{id}/resume | Owner | T-S2-03 |

### C.10 Acceptance Criteria

```gherkin
Scenario: AC-S2-030 Router
  Given task ghim 1 requirement, không quan hệ, không CANDIDATE → DIRECT
  And task ghim 1 requirement có 2 mục liên quan và 1 CANDIDATE → ASSISTED; task chỉ có brief với 3 CANDIDATE → DEEP
  When Owner ép mode khi giao → dùng mode đó; đổi requirement làm đổi tín hiệu → mode tính lại ở lease mới

Scenario: AC-S2-031 Cổng bản hiểu
  Given lease FULL mode ASSISTED có `gate` bật (Owner chỉ định; mặc định chỉ DEEP có — AC-S2-040), chưa nộp bản hiểu
  When pre-tool ghi vào repo → "deny" kèm danh sách mục phải xử lý; DIRECT → không có cổng; CONTEXT/OBSERVE → không có cổng
  When nộp bản thiếu một mục liên quan → 422 nêu key và tiêu đề; nộp đủ → 201, ghi repo được
  When trượt 2 lần rồi nộp lần 3 vẫn thiếu → 201 kèm unaddressed, journal UNDERSTANDING_FORCED

Scenario: AC-S2-032 Giả định và câu hỏi
  When bản hiểu có 2 giả định và 1 câu hỏi không blocking → 2 mục CANDIDATE basis đúng, 1 question OPEN, task vẫn RUNNING
  When có câu hỏi blocking → WAITING_DECISION, pre-tool ghi repo "deny"; Owner trả lời → mục AUTHORITATIVE decision, task RUNNING
  When Owner trả lời khác mặc định → lease được thay kèm thông báo; trùng mặc định → lease giữ nguyên
  When Worker trả lời câu hỏi → 403

Scenario: AC-S2-033 Tham vấn có giới hạn
  Given mode ASSISTED (1 lượt), project có một thành viên specialty product
  When người giữ lease consult product → 201, notifier được gọi một lần với tham chiếu, không có nội dung hội thoại
  When hỏi lại đúng câu đó → trả bản cũ, notifier không được gọi thêm; hỏi câu khác → 409 hết lượt
  When người không giữ lease consult, hoặc người trả lời mở consult cho chính task đó → 403/409
  When agent khác trả lời hộ → 404; đúng người trả lời → blackboard finding, người hỏi nhận ở hook kế tiếp
  When quá hạn → EXPIRED, trả lời muộn → 409

Scenario: AC-S2-034 Hội tụ
  When 2 verify liên tiếp có cùng tập FAIL và cùng fingerprint → phản hồi có convergence STEER, +1 lượt tham vấn (một lần cho cả task), journal NONPROGRESS; lỗi đổi fingerprint → không tính
  When lần thứ 3 → task WAITING_DECISION lý do nonprogress, pre-tool ghi "deny"; Owner resume → RUNNING, bộ đếm về 0
  When có tiến triển giữa chừng → bộ đếm về 0

Scenario: AC-S2-035 Feedback từng phần
  Given T (ASSISTED) và U cùng project, hai agent khác nhau
  When Owner feedback T → thông báo có "có thể liên quan" và "giữ nguyên"; pre-tool của T "deny" cho tới khi nộp delta phủ mọi key được ghim; U không đổi gì

Scenario: AC-S2-039 Feedback gửi lúc không ai giữ lease không bị mất
  Given T VERIFIED, lease đã RELEASED
  When Owner gửi feedback F1 rồi F2, sau đó giao lại T cho W
  Then lease mới có pending_notice chứa nguyên văn F1 và F2 theo thứ tự, context nạp ở lượt đầu chứa chúng, lần ghi đầu bị từ chối một lần (DIRECT) hoặc đòi delta (ASSISTED/DEEP)

Scenario: AC-S2-036 Kinh nghiệm
  When verify FAIL rồi PASS → lesson_requested; stop bị chặn một lần; nộp lesson → mục CANDIDATE experience có nguồn RUNTIME
  When verify PASS ngay lần đầu → không yêu cầu bài học; lesson có trường ngắn hơn 15 ký tự → 422
  When Owner xác nhận → packet của task mới cùng scope chứa kinh nghiệm đó; task scope khác hoặc project khác không chứa

Scenario: AC-S2-037 Tách chi phí
  Given usage trước khi bản hiểu được nhận, sau đó, sau attempt đầu, và usage của người trả lời tham vấn
  Then GET cost cho 4 phase đúng số; chỉ usage của đúng lượt trả lời tham vấn (theo turnId) cộng vào task của người hỏi và vào PROJECT/SYSTEM đúng một lần; usage khác của người trả lời không bị đụng

Scenario: AC-S2-038 Cách ly project cho mọi thứ ở phần C
  Then question, understanding, consult, attempt, lesson của project A không đọc được từ B (API 404 và RLS); người trả lời tham vấn luôn cùng project
```

## Phần D — Làm cứng verify và guard (sau kiểm chứng lại REV-003)

Kiểm chứng lại độc lập ngày 2026-11-17 trên `206f56c`: #3, #5 đóng; #1 (High), #2, #4 (Medium), #6 (Low) còn hở, đều tái hiện được. Nguyên tắc sửa: **runner và guard không tin bất cứ thứ gì agent ghi được trong repo** (`.git/*`, `.gitignore`, `.gitattributes`, cấu hình git của người dùng).

### D.1 Quy tắc

- **Base tin cậy.** Khi ghim `base_commit` (lần giao đầu), harness fetch commit đó vào kho object riêng `~/.local/state/kc-harness/bases/<hash repo>.git` (`fetch.fsckObjects=true`). Mọi phép so sánh với base đọc từ kho này; kho object của repo không được dùng làm alternates.
- **Snapshot không đọc cấu hình của repo.** Git dir tạm; `HOME` và `XDG_CONFIG_HOME` trỏ vào thư mục rỗng; `core.excludesFile` và `core.attributesFile` = `/dev/null`; attributes lấy từ cây rỗng (`--attr-source`), nên không có filter, eol hay encoding nào được áp. Không dùng `git add -A`: danh sách file = file tracked của base + file untracked liệt kê bằng `git ls-files --others --exclude-from=<file tin cậy>` (không đọc `.gitignore` trong cây), nạp bằng `update-index`.
- **Danh sách ignore tin cậy** lưu ở `project_repo.ignore_patterns`: mặc định lấy từ `.gitignore` gốc của HEAD lúc Owner đăng ký repo; chỉ Owner sửa (`PUT /v1/projects/{p}/repos/ignore`). `.gitignore` lồng trong thư mục con không được dùng — sản phẩm build không nằm trong danh sách sẽ hiện thành file đổi (false block có chủ đích; Owner bổ sung mẫu).
- **File điều khiển git luôn được bảo vệ:** `**/.gitignore`, `**/.gitattributes`, `.gitmodules` thêm vào danh sách luôn-bảo-vệ của guard và của check `scope`.
- **Mọi lệnh git harness chạy trên repo agent ghi được** đều kèm `-c core.fsmonitor=false -c core.hooksPath=/dev/null -c core.untrackedCache=false` và môi trường như trên.
- **Repo sạch khi ghim base** = `ChangedFiles(repo, HEAD)` rỗng theo đúng cơ chế trên (thay `git status`).
- **Snapshot lỗi không được coi là "không có gì đổi":** post-tool gặp lỗi snapshot trên lease FULL → trả block kèm lý do và journal `SNAPSHOT_FAILED` (một lần cho mỗi lỗi liên tiếp); verify gặp lỗi → evidence ERROR.
- **Guard:** resolve đường dẫn từng thành phần theo đúng ngữ nghĩa kernel (`..` sau symlink đi tới cha của đích; không `Clean` trước). Ghi qua symlink treo → từ chối. Đường dẫn chứa NUL → từ chối.
- **Sandbox của agent** thêm `denyWrite` cho `<repo root>/.git` của mọi repo đã đăng ký (agent pilot không commit; harness làm việc trên working tree).
- **Idempotency:** ghi kết quả lỗi thì lưu `null`; hàng "đang xử lý" quá 2 phút được tiếp quản; lỗi DB trong `leaseLock` → 500 (fail closed).

### D.2 Acceptance Criteria

```gherkin
Scenario: AC-S2-050 Không qua mặt được snapshot
  Given task chỉ cho phép src/**, base đã ghim
  When agent (a) chép đè loose object của tree base trong .git/objects, (b) thêm docs/.gitignore chứa "*" rồi thêm docs/x.py, (c) thêm .gitattributes có filter/working-tree-encoding/ident rồi sửa file tracked ngoài phạm vi, (d) ghi ~/.config/git/ignore và ~/.config/git/attributes, (e) đặt core.fsmonitor trỏ tới script, status.showUntrackedFiles=no, skip-worktree, assume-unchanged, .git/info/exclude
  Then ở mọi trường hợp ChangedFiles liệt kê đúng file đã đổi, check scope FAIL, hash candidate khác bản sạch, script fsmonitor không được chạy

Scenario: AC-S2-051 Snapshot lỗi là lỗi
  When thư mục con chmod 000 hoặc core.bare=true làm snapshot thất bại
  Then post-tool (FULL) trả block và journal SNAPSHOT_FAILED; verify → evidence ERROR, task không VERIFIED

Scenario: AC-S2-052 Repo bẩn không được làm base
  When giao lần đầu trên repo có file untracked không thuộc danh sách ignore tin cậy, hoặc file tracked bị sửa được giấu bằng skip-worktree → 409
  When chỉ có file thuộc danh sách ignore tin cậy → giao được

Scenario: AC-S2-053 Guard symlink
  Given src/d → ../docs, docs/dang → ../tests/acceptance/x_test.go (treo), tmp/a/b/q → ../.., src/odir → /ngoài
  When ghi src/d/dang, tmp/dang (treo, trỏ sang repo đăng ký khác), src/odir/../x.go, và đường dẫn chứa NUL
  Then tất cả "deny"; file mới trong thư mục mới thật vẫn "allow"

Scenario: AC-S2-054 Idempotency không kẹt
  When handler trả body không lưu được, hoặc tiến trình dừng giữa chừng để lại hàng "đang xử lý" quá 2 phút
  Then lần gọi lại cùng key được xử lý như lần đầu, không 409 vĩnh viễn
```

```gherkin
Scenario: AC-S2-040 Cổng bản hiểu mặc định chỉ ở DEEP
  When Owner giao không kèm intel_features với intel_mode ASSISTED, DIRECT, DEEP
  Then lease ASSISTED và DIRECT không có "gate"; lease DEEP có "gate"
  When Owner giao kèm intel_features có "gate" Then lease có "gate" ở mọi mode
  When lease có tính năng mặc định được cấp lại (feedback) và router đổi mode Then tính năng mặc định tính lại theo mode mới (REV-008 M-8a)
```

### C.11 Bảng mới (mọi bảng đều có `project_id`, khóa ngoại kép `(task_id, project_id)`, RLS theo `kch_sees(project_id)`, không DELETE)

| Bảng | Ai ghi (RLS) | Ghi chú |
|---|---|---|
| `understanding` | agent là tác giả; runtime | bất biến sau khi nhận |
| `question` | agent tạo; chỉ owner/runtime sửa trạng thái và câu trả lời | |
| `consult` | agent hỏi tạo; người trả lời chỉ sửa `answer` của bản gửi cho mình (qua runtime sau khi kiểm) | người trả lời luôn cùng project |
| `attempt`, `packet_log` | chỉ runtime | |
| `knowledge_embedding` | chỉ runtime; đọc qua `EXISTS (SELECT 1 FROM knowledge …)` dưới RLS của knowledge | khóa (knowledge_id, model); KNN chạy **sau** khi lọc project + `is_current` |

Các mục nêu trong REV-004 nhưng **hoãn** (ghi rõ, không làm ngầm): cập nhật AS_IS tự động sau VERIFIED, đo hiệu quả và loại bỏ kinh nghiệm, phát hiện mâu thuẫn tự động, chống lặp lệnh Bash, đổi mức suy luận theo mode.

## Review Log

### REV-004 (2026-11-17, reviewer độc lập, đặc tả S2 bản 1) — gate NEEDS_REVISION

| # | Lỗ hổng | Mức | Xử lý |
|---|---|---|---|
| 1 | Ngoài cây OpenBot ⇒ OWNER là fail-open | Cao | Sửa: mặc định từ chối; Owner chỉ qua `owner.sock` trong thư mục bị che; `agent.sock` không bao giờ cấp Owner (AC-S2-003). Phần còn lại: L1′ |
| 2 | Runner thành Owner, thấy `/proc` host, tự bật mạng | Cao | Sửa: `--proc /proc`, che socket và state dir, từ chối peer có tổ tiên `harnessd`, `network` chỉ khi Owner tạo task, DSN không ở environ (AC-S2-010, AC-S2-006) |
| 3 | `allowAllUnixSockets` + `/tmp` lộ socket khác | Cao | Sửa: bỏ hẳn unix socket trong sandbox; lệnh agent đi qua hook (AC-S2-004) |
| 4 | Tool ngoài sandbox không bị giới hạn khi không lease/OBSERVE/CONTEXT; MCP; hook fail-open | Cao | Sửa: allowlist ở mọi mode, từ chối `mcp__*`/Web, `pre-tool` fail-closed (AC-S2-007, AC-S2-005) |
| 5 | Sandbox ghi được file mà mã ngoài sandbox chạy | Cao | Sửa: `denyWrite` `.git`, `.claude`, `.mcp.json`; git cứng hóa (phần D); `kch` bỏ env; hash cấu hình hiệu lực (AC-S2-005) |
| 6 | Env thừa hưởng; danh sách che tĩnh | Cao | Sửa một phần: che cả home rồi mở từng vùng (fail-closed khi có thứ mới). Env: L12. `/tmp/claude-<uid>`: L11 |
| 7 | Operator = Owner theo agent_id | Cao | Sửa: bỏ khái niệm operator; người vận hành dùng `owner.sock` như mọi tiến trình không sandbox; không là thành viên, không nhận tham vấn (L1″) |
| 8 | `waiting_reason` đơn trị; RUNNING không lease | Cao | Sửa: tập lý do, đích như T-H-07; trả lời khác mặc định = feedback |
| 9 | Consult chéo project, chọn người, không bọc | Cao | Sửa (C.3) |
| 10 | COLLAB đếm đôi | Cao | Sửa: gắn theo turnId của lượt tham vấn (C.7) |
| 11 | Bảng mới thiếu project/RLS | Cao | Sửa (C.11) |
| 12 | Cổng bản hiểu vượt bằng Bash; brief-only rỗng; lý do rác | TB | Sửa: post-tool + verify 409, U5; ghi rõ đây là kiểm cấu trúc |
| 13 | Router ngược; effort | TB | Bỏ effort; loại ASM đã đóng. Tín hiệu router chưa được chứng minh — ghi nhận |
| 14 | Hội tụ chặn oan; +1 consult không trần; stop chặn chồng | TB | Sửa: so fingerprint, +1 một lần, gộp stop |
| 15 | VERIFY gồm rework; Memory ước lượng; không có cờ tắt | TB | Sửa: REWORK, hiệu giữa các arm, `intel_features` |
| 16 | Tiêu chí bật embedding | TB | Fixture đóng băng trước khi chạy model, có nhóm truy vấn âm và ngưỡng tương đồng (đã giao cho phần đánh giá) |
| 17 | Chủ trả lời trong chat; AC thiếu | TB | `GET /v1/questions`, journal `QUESTION_WAITING`; route hook chỉ nhận peer không sandbox là hệ quả của AC-S2-004 |
| 18 | Điều kiện A/B/C | TB | Cùng sandbox ở cả ba cấu hình, xóa context mỗi lượt, trần SYSTEM áp mọi mode, oracle ngoài mọi vùng đọc được |
| 19 | Cắt/hoãn | Thấp | Bỏ effort, lặp lệnh, `possible_conflicts`; tích hợp embedding chỉ sau khi đánh giá đạt |

Ba điểm nền reviewer chưa kiểm được đã có kết quả thực nghiệm (đầu Phần A): OpenBot giữ khối `sandbox`; `permissions.deny` có hiệu lực dưới Full access; ghi vào `.git` trong sandbox được chặn bằng `denyWrite` (AC-S2-012 kiểm lại trên pilot).

Bản 2 của Phần A và các sửa ở Phần C **chưa** được review độc lập lần hai; review code sau khi cài đặt phải kiểm lại #1–#11 trên cả spec lẫn code.

### REV-005 (2026-11-18, reviewer độc lập, code Intelligence Layer trên b285e19 + Phần C chưa commit) — các mục đã xử lý

| # | Lỗ hổng | Mức | Xử lý | Bằng chứng (test fail trước khi sửa, pass sau) |
|---|---|---|---|---|
| 1 | Gỡ lý do chờ quyết RUNNING/RUNNABLE từ snapshot của câu lệnh → RUNNING không lease khi release commit giữa chừng | Cao | Sửa: `RemoveWaitingReason` khóa dòng task bằng câu lệnh riêng rồi mới đọc lease; áp cho trả lời câu hỏi, resume, và `resumeTask` (T-H-07). Mọi đường kết thúc lease đã khóa task trước và đưa RUNNING → RUNNABLE (kiểm lại, không đổi) | `TestREV005_1_WaitingReasonRemovalRacesLeaseRelease` (2 kết nối, rào chắn qua `pg_locks`, 15 vòng × 2 đường; trước sửa: RUNNING ở vòng 0) |
| 2 | Bash ghi repo được khi task chờ `question`/`nonprogress` | TB | Sửa: pre-tool từ chối mọi Bash của người giữ lease khi chờ; post-tool chặn nếu candidate đổi so với lúc bắt đầu chờ (candidate lưu trong `QUESTION_BLOCKING`/`NONPROGRESS_PARKED`). Ngoại lệ "một lệnh `kch` đơn": hook client gửi cờ `kch_verb` = (`parseKch` ra `kchCall`); lệnh đó luôn bị thay bằng lời gọi verb (re-verify C1/C2) | `TestREV005_2_WaitingRefusesBash`, `TestKchVerbFlagOnlyForReplacedCommands` |
| 3 | Feedback mất khi chính nó làm lease quá hạn EXPIRED (cùng `now()`) | TB | Sửa: so theo `journal.seq` của `LEASE_ENDED` | `TestREV005_3_FeedbackThatExpiresAnOverdueLease` |
| 4 | Host lỗi/429/treo: tham vấn OPEN mãi, mất lượt, `kch answer` treo ~60 s | TB | Sửa theo quyết định ở C.3: `PENDING_SEND` → retry backoff → `FAILED` + hoàn lượt; trần 5 s cho lời gọi host; tra turn chuyển sang worker. Migration 00006 | `TestREV005_4_ConsultWhenTheHostFails` (429, hết hạn, host treo 20 s) |
| 5 | Chuyển usage COLLAB không gỡ EXHAUSTED của task người trả lời | TB | Sửa: tính lại trạng thái ngân sách bị trừ; task không còn EXHAUSTED được `resumeTask` | `TestREV005_5_CollabMoveLiftsTheAnswerersBudgetWait` |
| 6 | `--refs` của người trả lời không kiểm, nằm ngoài khối dữ liệu | Thấp | Sửa: chỉ giữ key hiện hành của project tham vấn; key + tiêu đề + trust bên trong khối | `TestREV005_6_AnswerRefsAreKnowledgeKeys` |
| 7 | Thiếu bài học chặn `stop` mọi lượt | Thấp | Sửa: một lần mỗi lease (`LESSON_STOP_BLOCKED`) | `TestREV005_7_LessonBlocksStopOncePerLease` |
| 8 | Bản hiểu/câu hỏi blocking commit trước, lý do `question` ở transaction sau | Thấp | Sửa: một transaction (phần của agent dưới RLS của agent, rồi chuyển scope runtime trong cùng transaction) | Không có test riêng (không chèn được lỗi giữa hai bước); AC-S2-031/032/035 vẫn pass |
| 9 | Hai verify đồng thời cùng thấy repeats=2 | Thấp | Sửa: `recordAttempt` khóa dòng task trước khi đọc attempt trước | Không có test race riêng; AC-S2-034 vẫn pass |
| 10 | Phase gắn theo lúc tailer đọc | Thấp | Sửa (có tra cứu rẻ): phase tại `occurred_at` (C.7) | `TestREV005_10_PhaseAtOccurrence` |
| 11 | Người trả lời nhận lease ở project khác sau khi được chọn | Thấp | Sửa: kiểm lại lúc `kch answer`, 409 | `TestREV005_11_AnswererBusyInAnotherProject` |
| 12 | Giả định `ASM-*` của chính task làm tăng mode của nó | Thấp | Sửa: loại khỏi U khi định tuyến và khi kiểm bản hiểu | `TestREV005_12_OwnAssumptionsDoNotRouteTheTask` |

### REV-008 (2026-11-18, reviewer độc lập) — cổng bản hiểu mặc định chỉ ở DEEP; chặn Shared — PASS_WITH_CONDITIONS

| # | Mục | Xử lý |
|---|---|---|
| M-8a | Lease cấp lại giữ danh sách tính năng của lease đầu, nên mặc định không theo mode mới | Sửa: lease có tính năng mặc định được tính lại theo mode mới khi cấp lại (`intel.IsDefaultFeatures`); tính năng Owner chỉ định thì giữ nguyên. Test: `TestDefaultFeaturesByMode`, phần tường minh của `TestAC_S2_040`. Ca router tự nâng lên DEEP chưa có e2e (cần dựng tín hiệu router) |
| L-8a | Spec C.1 bảng, C.5, AC-031 còn nói ASSISTED luôn có cổng | Sửa |
| L-8b | Ba comment còn ghi "mặc định: tất cả" | Sửa |
| L-8c | AC-040 chưa kiểm dòng "gate tường minh" | Thêm vào test |
| L-8d | Đường dẫn Shared suy ra từ `agents_root/../Shared`; agent đã ghi danh cần ghi danh lại | Ghi nhận; đã ghi danh lại 4 Lead |
| L-8e | Bằng chứng hẹp: 3 kịch bản, cả hai cấu hình đều đạt tối đa | Ghi nhận trong `docs/V5-PILOT-REPORT.md` |


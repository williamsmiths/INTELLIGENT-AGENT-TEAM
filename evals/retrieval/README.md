# Đo retrieval ngữ nghĩa (spec S2 B.1, AC-S2-024)

**Quyết định: ENABLE `BAAI/bge-m3`, ngưỡng cosine 0,51.** Chạy ngày 2026-11-17. Mọi số dưới đây lấy từ `report.json` (do `scripts/score.py` sinh ra), không gõ tay.

Fixture: `fixture.json`, sha256 `66189dda89600f6ba303dd0482cdc4bc6db0bdee29c8a0599d1f20057f82c71b`, đóng băng trước lần chạy model đầu tiên. 2 project (IELTS, kho hàng) × 58 mục + 3 mục GLOBAL; 120 truy vấn, 9 nhóm, 30% mỗi nhóm là holdout (seed 20261117). Errata: không có (`errata.json` rỗng).

## Holdout — recall@5 (tử/mẫu)

| nhóm | lexical | + bge-m3 (0,51) | + e5-base (0,78) | + e5-small (0,79) | + Vietnamese_Embedding (0,37) |
|---|---|---|---|---|---|
| vi_diacritics | 1,00 (4/4) | 1,00 (4/4) | 1,00 (4/4) | 1,00 (4/4) | 1,00 (4/4) |
| vi_no_diacritics | 1,00 (4/4) | 1,00 (4/4) | 1,00 (4/4) | 1,00 (4/4) | 1,00 (4/4) |
| english | 1,00 (4/4) | 1,00 (4/4) | 1,00 (4/4) | 1,00 (4/4) | 1,00 (4/4) |
| code | 1,00 (4/4) | 1,00 (4/4) | 1,00 (4/4) | 1,00 (4/4) | 1,00 (4/4) |
| paraphrase_vi | 0,00 (0/5) | 0,60 (3/5) | 0,80 (4/5) | 0,80 (4/5) | 0,20 (1/5) |
| paraphrase_en | 0,00 (0/4) | 0,50 (2/4) | 0,75 (3/4) | 1,00 (4/4) | 0,75 (3/4) |
| cross_lang_en_to_vi | 0,00 (0/5) | 0,60 (3/5) | 0,60 (3/5) | 0,00 (0/5) | 0,80 (4/5) |
| cross_lang_vi_to_en | 0,00 (0/4) | 1,00 (4/4) | 0,50 (2/4) | 0,00 (0/4) | 1,00 (4/4) |
| **diễn đạt lại + chéo ngôn ngữ** | **0,00 (0/18)** | **0,67 (12/18)** | 0,67 (12/18) | 0,44 (8/18) | 0,67 (12/18) |

| cấu hình | MRR (nhóm có đáp án) | rác trên `negative` | hit lọt project | p50 / p95 ms |
|---|---|---|---|---|
| lexical | 0,46 (15,5/34) | 0/5 | 0 | 1,8 / 4,1 |
| + bge-m3 | 0,79 (26,75/34) | 1/5 | 0 | 32,3 / 41,7 |
| + e5-base | 0,71 (24,23/34) | 5/5 | 0 | 12,3 / 15,8 |
| + e5-small | 0,65 (22,08/34) | 4/5 | 0 | 8,2 / 16,7 |
| + Vietnamese_Embedding | 0,77 (26,33/34) | 0/5 | 0 | 32,5 / 41,9 |

Tập tuning (nhóm gộp): lexical 0/38; bge-m3 35/38 (rác 0/11); e5-base 32/38 (rác 8/11); e5-small 23/38 (rác 8/11); Vietnamese_Embedding 34/38 (rác 1/11). Chi tiết từng nhóm, MRR và độ trễ từng nhóm, từng truy vấn: `report.json`, `runs/tables.md`.

## Áp điều kiện bật (B.1)

- Chênh recall@5 nhóm gộp trên holdout: bge-m3 +0,67 (khoảng bootstrap 95%: 0,44–0,89, n = 18), e5-base +0,67, e5-small +0,44, Vietnamese_Embedding +0,67 — cả bốn ≥ 0,20.
- Không nhóm nào thấp hơn lexical (đúng theo cách ghép: semantic chỉ nối thêm phía sau, không đổi thứ hạng lexical).
- Chọn model: trong các model đạt, lấy model có recall nhóm gộp cao nhất trên **tuning** (để không dùng holdout chọn) → bge-m3 (0,92).

## Cách ghép và ngưỡng

Thứ hạng lexical (`memory.Search` thật, key → FTS → trigram, tối đa 10) giữ nguyên; mục có cosine ≥ ngưỡng trong đúng project của truy vấn (cộng GLOBAL đã xác nhận) được nối thêm, bỏ trùng, tới 10 kết quả. Ngưỡng dò trên tuning (bước 0,01, tối đa F1: TP = truy vấn có đáp án trong top 5, FP = truy vấn `negative` nhận hit semantic; hoà thì lấy ngưỡng cao), sau đó cố định cho holdout.

## Model và chi phí (TEI 1.9.4 CPU, 4 CPU, giới hạn 6 GB)

| model | revision | chiều | nạp lại (s) | RAM sau nạp / đỉnh (MB) | embed 1 truy vấn p50 / p95 (ms) | lô 32 mục p50 / p95 (ms) | 1 000 mục: wall / CPU (s) | đĩa |
|---|---|---|---|---|---|---|---|---|
| BAAI/bge-m3 | 5617a9f61b02 | 1024 | 5,0 | 2 503 / 2 647 | 30,0 / 42,2 | 2 162 / 2 425 | 65,6 / 262,2 | 2,28 GB |
| intfloat/multilingual-e5-base | d12875059715 | 768 | 4,7 | 2 309 / 2 704 | 10,3 / 12,7 | 626 / 663 | 18,6 / 74,3 | 1,13 GB |
| intfloat/multilingual-e5-small | 614241f622f5 | 384 | 2,4 | 1 346 / 1 603 | 5,7 / 12,4 | 183 / 302 | 5,9 / 23,8 | 0,49 GB |
| AITeamVN/Vietnamese_Embedding | dea33aa1ab33 | 1024 | 5,2 | 2 907 / 3 018 | 30,5 / 37,0 | 2 204 / 2 348 | 66,0 / 264,1 | 2,28 GB |

e5 dùng tiền tố `query: ` / `passage: `; hai model còn lại không tiền tố. Văn bản mục = `title + "\n" + body`. Image 938 MB. bge-m3 và Vietnamese_Embedding chạy với `--max-batch-tokens=2048` (xem dưới).

## Sidecar: gọi thế nào, hỏng thế nào

`POST http://127.0.0.1:8870/v1/embeddings` body `{"model":"BAAI/bge-m3","input":["...", "..."]}` (tối đa 32 chuỗi) → `{"object":"list","data":[{"object":"embedding","index":0,"embedding":[1024 số, đã chuẩn hoá L2]}],"model":"BAAI/bge-m3","usage":{"prompt_tokens":n,"total_tokens":n}}`. `POST /embed` `{"inputs":[...]}` trả cùng vector. `GET /health` 200, `GET /info` cho model và revision.

- bge-m3 với `--max-batch-tokens` mặc định 16384 bị OOM-kill ở 6 GB ngay lúc warm-up (17 lần khởi động lại, không bao giờ healthy); 2048 thì chạy, văn bản dài hơn 2048 token bị cắt. Một văn bản dài tối đa mất 3,6 s.
- Container dừng: connection refused sau 0,4 ms. Container treo (pause): client phải tự đặt timeout. Tiến trình thoát: restart policy đưa lại sau 5,2 s (trong lúc đó refused / reset). 33 chuỗi: 422. Chuỗi rỗng: 400.
- Trường `model` trong request **không được kiểm tra** (gửi tên khác vẫn 200): client phải đối chiếu `/info` trước khi ghi `knowledge_embedding.model`.
- Khởi động không cần mạng khi model đã nằm trong volume (đã thử `--network none`).

## pgvector (bge-m3, 119 hàng, database tạm đã xoá)

Bảng đúng dạng spec (`embedding vector` không cố định chiều) nên chỉ mục phải là `USING hnsw ((embedding::vector(1024)) vector_cosine_ops) WHERE model = '...'` và câu truy vấn phải dùng cùng biểu thức ép kiểu; lọc project qua join `knowledge` (`k.project_id = $ OR k.level = 'GLOBAL'`). Quét chính xác p50 0,49 / p95 1,37 ms; HNSW (ép bằng `enable_seqscan = off`) 0,37 / 0,64 ms; 120/120 truy vấn trả đúng danh sách như numpy; 0 hàng của project khác. Câu SQL và plan: `report.json` → `pgvector`. RLS không được dựng lại ở đây.

## Điều làm kết quả yếu hơn vẻ ngoài

- Holdout nhóm gộp chỉ 18 truy vấn; ba model hoà nhau 12/18. Khoảng tin cậy rộng.
- Ngưỡng bge-m3 rất nhạy: ở 0,49 holdout là 16/18 với cùng mức rác 1/5; ở 0,55 còn 10/18 (bảng `threshold_sensitivity_winner`, chỉ để tham khảo). Điểm cosine của truy vấn đúng và truy vấn rác chồng lên nhau (rác cao nhất 0,54). Cần dò lại ngưỡng khi có dữ liệu thật.
- Quy tắc F1 cho e5 ngưỡng thấp → rác 4–5/5 trên holdout; với ngưỡng cân bằng hơn (Youden J) e5-base còn +0,39 và e5-small +0,11 (`secondary_threshold_analysis`).
- Một người viết cả mục lẫn truy vấn; truy vấn diễn đạt lại được viết cố ý tránh từ trùng nên lexical 0/56 ở các nhóm này — khoảng cách thật có thể nhỏ hơn. Mỗi truy vấn chỉ gắn một đáp án dù có mục liên quan một phần.
- Kho 58 mục mỗi project, mục ngắn (trung bình khoảng 46 token); độ trễ pgvector và chất lượng ở kho lớn chưa được đo.
- Semantic chỉ vào được top 5 khi lexical trả dưới 5 kết quả; chưa đo context packet.
- "Hit lọt project" của tầng semantic bằng 0 là do cách tính (chỉ so trong project); số có ý nghĩa là lexical thật (0) và pgvector (0). Nếu không lọc, 2/104 truy vấn có kết quả đầu thuộc project kia.
- Độ trễ đo trên máy đang chạy nhiều container khác; không có cache embedding truy vấn.

## Chạy lại

```bash
cd <repo>; set -a; . infra/.env; set +a
export HARNESS_TEST_DSN="postgres://harness:${HARNESS_PG_PASSWORD}@127.0.0.1:${HARNESS_PG_PORT}/harness?sslmode=disable"
KCH_RETRIEVAL_FIXTURE=$PWD/evals/retrieval/fixture.json KCH_RETRIEVAL_OUT=$PWD/evals/retrieval/runs/lexical.json \
  go test -count=1 -run 'TestRetrievalDump$' ./internal/memory/
cd evals/retrieval
uv run --with numpy python scripts/embed_model.py --tag bge-m3 --model BAAI/bge-m3 --max-batch-tokens 2048
uv run --with numpy python scripts/embed_model.py --tag e5-base --model intfloat/multilingual-e5-base --query-prefix "query: " --passage-prefix "passage: "
uv run --with numpy python scripts/score.py
uv run --with numpy --with 'psycopg[binary]' python scripts/pgvector_bench.py --tag bge-m3 --threshold 0.51
uv run python scripts/failure_probe.py --model BAAI/bge-m3
docker compose -f ../../infra/compose.yaml up -d --no-deps embed   # về lại model mặc định
```

`embed_model.py` khởi động lại service `embed` với model được truyền vào; chạy xong nhớ đưa về mặc định.

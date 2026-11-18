"""Start the compose `embed` service with one model, embed the fixture through
POST /v1/embeddings, and record vectors, latencies and resource use.

  uv run --with numpy python scripts/embed_model.py --tag e5-small \
      --model intfloat/multilingual-e5-small --query-prefix "query: " --passage-prefix "passage: "

Writes runs/emb-<tag>.npz (vectors) and runs/model-<tag>.json (facts). No scoring here."""
import argparse
import json
import subprocess
import time
import urllib.error
import urllib.request
from pathlib import Path

import numpy as np

import fixture_io

ROOT = Path(__file__).resolve().parents[1]
REPO = ROOT.parents[1]
COMPOSE = ["docker", "compose", "-f", str(REPO / "infra" / "compose.yaml")]
CONTAINER = "kc-harness-embed"
BASE = "http://127.0.0.1:8870"


def sh(cmd, timeout, env=None, check=True):
    import os
    r = subprocess.run(cmd, capture_output=True, text=True, timeout=timeout, env={**os.environ, **(env or {})})
    if check and r.returncode != 0:
        raise RuntimeError(f"{cmd[:4]} failed: {r.stderr.strip()[-500:]}")
    return r.stdout.strip()


def http(path, payload=None, timeout=120):
    data = None if payload is None else json.dumps(payload).encode()
    req = urllib.request.Request(BASE + path, data=data, headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=timeout) as r:
        return json.loads(r.read() or b"null")


def wait_healthy(limit_s):
    start = time.monotonic()
    while time.monotonic() - start < limit_s:
        try:
            urllib.request.urlopen(BASE + "/health", timeout=2).read()
            return time.monotonic() - start
        except (urllib.error.URLError, OSError):
            state = sh(["docker", "inspect", "-f", "{{.State.OOMKilled}} {{.RestartCount}}", CONTAINER], 20, check=False).split()
            if state and (state[0] == "true" or int(state[1]) >= 3):
                raise RuntimeError(f"sidecar is crash-looping: OOMKilled={state[0]} restarts={state[1]}")
            time.sleep(0.25)
    raise RuntimeError("sidecar not healthy in time:\n" + sh(["docker", "logs", "--tail", "30", CONTAINER], 20, check=False))


def cgroup():
    out = sh(["docker", "exec", CONTAINER, "sh", "-c",
              "cat /sys/fs/cgroup/memory.current /sys/fs/cgroup/memory.peak; grep usage_usec /sys/fs/cgroup/cpu.stat; "
              "grep '^anon ' /sys/fs/cgroup/memory.stat"], 20)
    cur, peak, usage, anon = out.split("\n")
    return {"memory_current_mb": round(int(cur) / 2**20, 1), "memory_peak_mb": round(int(peak) / 2**20, 1),
            "memory_anon_mb": round(int(anon.split()[1]) / 2**20, 1), "cpu_usage_s": int(usage.split()[1]) / 1e6}


def embed(texts, model):
    start = time.perf_counter()
    r = http("/v1/embeddings", {"model": model, "input": texts, "encoding_format": "float"})
    ms = (time.perf_counter() - start) * 1000
    rows = sorted(r["data"], key=lambda d: d["index"])
    return np.asarray([d["embedding"] for d in rows], dtype=np.float32), ms, r.get("usage", {})


def pct(xs, p):
    xs = sorted(xs)
    return round(xs[min(len(xs) - 1, int(p * len(xs)))], 3)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--tag", required=True)
    ap.add_argument("--model", required=True)
    ap.add_argument("--revision", default="main")
    ap.add_argument("--query-prefix", default="")
    ap.add_argument("--passage-prefix", default="")
    ap.add_argument("--start-timeout", type=int, default=1500)
    ap.add_argument("--max-batch-tokens", default="16384")
    ap.add_argument("--mem-limit", default="6g")
    a = ap.parse_args()

    fx = fixture_io.load(ROOT / "fixture.json")
    passages = [(name, it["key"], it["title"] + "\n" + it["body"]) for name, p in fx["projects"].items() for it in p["items"]]
    passages += [("global", it["key"], it["title"] + "\n" + it["body"]) for it in fx["global"]]
    queries = [(q["id"], q["q"]) for q in fx["queries"]]
    env = {"HARNESS_EMBED_MODEL": a.model, "HARNESS_EMBED_REVISION": a.revision,
           "HARNESS_EMBED_MAX_BATCH_TOKENS": a.max_batch_tokens, "HARNESS_EMBED_MEM_LIMIT": a.mem_limit}

    # First start: includes the download when the model is not yet in the volume.
    sh(COMPOSE + ["rm", "-sf", "embed"], 120, check=False)
    t0 = time.monotonic()
    sh(COMPOSE + ["up", "-d", "--no-deps", "embed"], 300, env)
    first_start_s = time.monotonic() - t0 + wait_healthy(a.start_timeout)
    # Second start: model already cached, so this is the load time production sees after a restart.
    sh(["docker", "restart", "-t", "5", CONTAINER], 120)
    t0 = time.monotonic()
    warm_load_s = wait_healthy(a.start_timeout)
    info = http("/info")
    after_load = cgroup()

    embed(["warm up"], a.model)
    ptexts = [a.passage_prefix + t for _, _, t in passages]
    qtexts = [a.query_prefix + t for _, t in queries]

    vec_p, tokens = [], 0
    for i in range(0, len(ptexts), 32):
        v, _, usage = embed(ptexts[i:i + 32], a.model)
        vec_p.append(v)
        tokens += usage.get("total_tokens", 0)
    vec_p = np.vstack(vec_p)

    q_ms = [[] for _ in queries]
    vec_q = None
    for rnd in range(3):
        vs = []
        for i, t in enumerate(qtexts):
            v, ms, _ = embed([t], a.model)
            q_ms[i].append(round(ms, 3))
            vs.append(v[0])
        if rnd == 0:
            vec_q = np.vstack(vs)

    batch_ms = []
    for rnd in range(5):
        for i in range(0, 96, 32):  # three distinct full batches of 32 passages, five rounds
            _, ms, _ = embed(ptexts[i:i + 32], a.model)
            batch_ms.append(ms)

    # 1 000 passages (the fixture passages repeated) in batches of 32: wall and CPU time.
    before = cgroup()
    t0 = time.perf_counter()
    thousand = [ptexts[i % len(ptexts)] for i in range(1000)]
    for i in range(0, 1000, 32):
        embed(thousand[i:i + 32], a.model)
    wall_1000 = time.perf_counter() - t0
    after = cgroup()

    # /embed (TEI native) must give the same vector as /v1/embeddings.
    native = np.asarray(http("/embed", {"inputs": [qtexts[0]]}), dtype=np.float32)[0]
    cache = "/data/models--" + a.model.replace("/", "--")
    # TEI's /info echoes the requested revision; the commit it resolved to is in the hub cache.
    commit = sh(["docker", "exec", CONTAINER, "sh", "-c", f"cat {cache}/refs/{a.revision} 2>/dev/null || ls {cache}/snapshots"], 30)
    disk = sh(["docker", "exec", CONTAINER, "sh", "-c", f"du -sb {cache}"], 30, check=False)
    image = sh(["docker", "inspect", "-f", "{{.Config.Image}}", CONTAINER], 20)
    cpus = sh(["docker", "inspect", "-f", "{{.HostConfig.NanoCpus}} {{.HostConfig.Memory}}", CONTAINER], 20).split()

    all_q = [ms for row in q_ms for ms in row]
    meta = {
        "tag": a.tag, "model": a.model, "requested_revision": a.revision, "model_sha": commit,
        "dim": int(vec_p.shape[1]), "query_prefix": a.query_prefix, "passage_prefix": a.passage_prefix,
        "passage_text": "title + \\n + body", "endpoint": "POST /v1/embeddings",
        "tei_info": info, "image": image, "container_limits": {"cpus": int(cpus[0]) / 1e9, "memory_bytes": int(cpus[1])},
        "first_start_s": round(first_start_s, 1), "warm_load_s": round(warm_load_s, 1),
        "memory_after_load_mb": after_load["memory_current_mb"], "memory_after_runs_mb": after["memory_current_mb"],
        "memory_anon_after_load_mb": after_load["memory_anon_mb"], "memory_anon_after_runs_mb": after["memory_anon_mb"],
        "memory_peak_mb": after["memory_peak_mb"],
        "single_query_ms": {"samples": len(all_q), "p50": pct(all_q, 0.5), "p95": pct(all_q, 0.95)},
        "batch32_passages_ms": {"samples": len(batch_ms), "p50": pct(batch_ms, 0.5), "p95": pct(batch_ms, 0.95)},
        "embed_1000_passages": {"wall_s": round(wall_1000, 2), "cpu_s": round(after["cpu_usage_s"] - before["cpu_usage_s"], 2),
                                "batch": 32, "note": "the 119 fixture passages repeated to 1 000"},
        "fixture_passage_tokens": tokens,
        "vector_norm_min_max": [round(float(np.linalg.norm(vec_p, axis=1).min()), 4), round(float(np.linalg.norm(vec_p, axis=1).max()), 4)],
        "native_embed_max_abs_diff": round(float(np.abs(native - vec_q[0]).max()), 6),
        "model_disk_bytes": int(disk.split()[0]),
        "fixture_sha256": fixture_io.sha256(ROOT / "fixture.json"),
        "generated_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
    }
    (ROOT / "runs").mkdir(exist_ok=True)
    np.savez_compressed(ROOT / "runs" / f"emb-{a.tag}.npz", passages=vec_p, queries=vec_q,
                        passage_ids=np.array([f"{p}/{k}" for p, k, _ in passages]), query_ids=np.array([i for i, _ in queries]),
                        query_ms=np.array(q_ms))
    (ROOT / "runs" / f"model-{a.tag}.json").write_text(json.dumps(meta, indent=1, ensure_ascii=False) + "\n")
    print(json.dumps({k: meta[k] for k in ("tag", "model_sha", "dim", "first_start_s", "warm_load_s", "memory_after_load_mb", "memory_anon_after_runs_mb",
                                           "memory_peak_mb", "model_disk_bytes", "single_query_ms", "batch32_passages_ms", "embed_1000_passages",
                                           "vector_norm_min_max", "native_embed_max_abs_diff")}, indent=1))


if __name__ == "__main__":
    main()

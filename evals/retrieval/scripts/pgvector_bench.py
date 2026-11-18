"""Load one model's fixture embeddings into a throwaway pgvector database and time the
semantic-stage SQL (exact scan and HNSW), restricted to the caller's project.

  uv run --with numpy --with 'psycopg[binary]' python scripts/pgvector_bench.py --tag <tag> --threshold <t>

The database kch_retrieval_eval_<ts> is created on the harness server and dropped at the end.
Row-level security is NOT reproduced here (that belongs to the integration); the project
restriction is the explicit join condition."""
import argparse
import hashlib
import json
import re
import time
from pathlib import Path

import numpy as np
import psycopg

import fixture_io

ROOT = Path(__file__).resolve().parents[1]
REPO = ROOT.parents[1]
ROUNDS, LIMIT = 5, 10


def admin_conninfo(dbname):
    env = dict(l.strip().split("=", 1) for l in (REPO / "infra" / ".env").read_text().splitlines() if "=" in l and not l.startswith("#"))
    return psycopg.conninfo.make_conninfo(host="127.0.0.1", port=env.get("HARNESS_PG_PORT", "5439"), user="harness",
                                          password=env["HARNESS_PG_PASSWORD"], dbname=dbname, connect_timeout=10)


def pct(xs, p):
    xs = sorted(xs)
    return round(xs[min(len(xs) - 1, int(p * len(xs)))], 3)


def vec(v):
    return "[" + ",".join(f"{x:.7g}" for x in v) + "]"


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--tag", required=True)
    ap.add_argument("--threshold", type=float, required=True)
    a = ap.parse_args()
    fx = fixture_io.load(ROOT / "fixture.json")
    meta = json.loads((ROOT / "runs" / f"model-{a.tag}.json").read_text())
    z = np.load(ROOT / "runs" / f"emb-{a.tag}.npz")
    dim, model = meta["dim"], meta["model"]
    pids = [str(x) for x in z["passage_ids"]]
    qvec = {str(i): v for i, v in zip(z["query_ids"], z["queries"])}
    pn = z["passages"] / np.linalg.norm(z["passages"], axis=1, keepdims=True)

    db = "kch_retrieval_eval_" + time.strftime("%Y%m%d%H%M%S")
    out = {"model": model, "tag": a.tag, "dim": dim, "threshold": a.threshold, "database": db + " (dropped)", "rows": len(pids),
           "rounds_per_query": ROUNDS, "queries": len(fx["queries"]),
           "latency_note": "client wall time of one prepared statement over a loopback connection, vector passed as text"}
    with psycopg.connect(admin_conninfo("harness"), autocommit=True) as adm:
        adm.execute(f"CREATE DATABASE {db}")
    try:
        with psycopg.connect(admin_conninfo(db), autocommit=True) as c:
            c.execute("SET statement_timeout = '30s'")
            c.execute("CREATE EXTENSION vector")
            out["pgvector_version"] = c.execute("SELECT extversion FROM pg_extension WHERE extname = 'vector'").fetchone()[0]
            out["postgres_version"] = c.execute("SHOW server_version").fetchone()[0]
            c.execute("""CREATE TABLE knowledge (id uuid PRIMARY KEY DEFAULT gen_random_uuid(), project_id uuid,
                         level text NOT NULL, key text NOT NULL)""")
            c.execute("CREATE INDEX knowledge_project_idx ON knowledge (project_id)")
            # Table as in spec B.1: the vector column has no fixed dimension because the model may change.
            c.execute("""CREATE TABLE knowledge_embedding (knowledge_id uuid NOT NULL REFERENCES knowledge(id), model text NOT NULL,
                         dim int NOT NULL, content_hash bytea NOT NULL, embedding vector NOT NULL, PRIMARY KEY (knowledge_id, model))""")
            proj = {name: c.execute("SELECT gen_random_uuid()").fetchone()[0] for name in fx["projects"]}
            for pid, v in zip(pids, z["passages"]):
                name, key = pid.split("/", 1)
                kid = c.execute("INSERT INTO knowledge (project_id, level, key) VALUES (%s, %s, %s) RETURNING id",
                                (proj.get(name), "GLOBAL" if name == "global" else "PROJECT", key)).fetchone()[0]
                c.execute("INSERT INTO knowledge_embedding VALUES (%s, %s, %s, %s, %s::vector)",
                          (kid, model, dim, hashlib.sha256(pid.encode()).digest(), vec(v)))
            c.execute("ANALYZE")

            sql = f"""SELECT k.key, k.project_id, 1 - (e.embedding::vector({dim}) <=> %(q)s::vector({dim})) AS cos
                      FROM knowledge_embedding e JOIN knowledge k ON k.id = e.knowledge_id
                      WHERE e.model = %(model)s AND (k.project_id = %(project)s OR k.level = 'GLOBAL')
                        AND k.id <> ALL(%(seen)s::uuid[])
                        AND (e.embedding::vector({dim}) <=> %(q)s::vector({dim})) <= %(maxdist)s
                      ORDER BY e.embedding::vector({dim}) <=> %(q)s::vector({dim}) LIMIT %(limit)s"""
            out["sql"] = " ".join(sql.split())

            def run(label):
                lat, results, foreign, maxdiff, same, returned = [], {}, 0, 0.0, 0, 0
                for rnd in range(ROUNDS):
                    for q in fx["queries"]:
                        params = {"q": vec(qvec[q["id"]]), "model": model, "project": proj[q["project"]], "seen": [],
                                  "maxdist": 1 - a.threshold, "limit": LIMIT}
                        start = time.perf_counter()
                        rows = c.execute(sql, params, prepare=True).fetchall()
                        lat.append((time.perf_counter() - start) * 1000)
                        if rnd == 0:
                            results[q["id"]] = [r[0] for r in rows]
                            foreign += sum(r[1] not in (None, proj[q["project"]]) for r in rows)
                            # same scope in numpy
                            idx = [i for i, p in enumerate(pids) if p.split("/")[0] in (q["project"], "global")]
                            qn = qvec[q["id"]] / np.linalg.norm(qvec[q["id"]])
                            cos = {pids[i].split("/", 1)[1]: float(pn[i] @ qn) for i in idx}
                            maxdiff = max([maxdiff] + [abs(r[2] - cos[r[0]]) for r in rows])
                            expect = [k for k, v in sorted(cos.items(), key=lambda kv: -kv[1]) if v >= a.threshold][:LIMIT]
                            same += [r[0] for r in rows] == expect
                            returned += len(rows)
                plan = "\n".join(r[0] for r in c.execute("EXPLAIN " + sql, {"q": vec(qvec["Q001"]), "model": model, "project": proj["ielts"],
                                                                           "seen": [], "maxdist": 1 - a.threshold, "limit": LIMIT}).fetchall())
                out[label] = {"latency_ms": {"samples": len(lat), "p50": pct(lat, 0.5), "p95": pct(lat, 0.95)},
                              "rows_returned": returned, "rows_of_another_project": foreign, "max_abs_cos_diff_vs_numpy": float(f"{maxdiff:.2g}"),
                              "queries_with_same_keys_as_numpy": {"num": same, "den": len(fx["queries"])},
                              "plan": re.sub(r"'\[[^']*\]'", "'[...]'", plan).split("\n")}
                return results

            exact = run("exact_scan")
            start = time.perf_counter()
            c.execute(f"""CREATE INDEX knowledge_embedding_hnsw ON knowledge_embedding
                          USING hnsw ((embedding::vector({dim})) vector_cosine_ops) WHERE model = '{model}'""")
            out["hnsw_index"] = {"definition": f"USING hnsw ((embedding::vector({dim})) vector_cosine_ops) WHERE model = '<model>'",
                                 "build_ms": round((time.perf_counter() - start) * 1000, 1),
                                 "size_bytes": c.execute("SELECT pg_relation_size('knowledge_embedding_hnsw')").fetchone()[0]}
            c.execute("ANALYZE")
            # At this size the planner prefers the sequential scan; force the index to time the HNSW path.
            c.execute("SET enable_seqscan = off")
            c.execute("SET enable_sort = off")
            hnsw = run("hnsw")
            out["hnsw"]["forced_with"] = "SET enable_seqscan = off; SET enable_sort = off"
            out["hnsw"]["queries_with_same_keys_as_exact"] = {"num": sum(hnsw[k] == exact[k] for k in exact), "den": len(exact)}
            c.execute("SET hnsw.iterative_scan = strict_order")
            it = run("hnsw_iterative_strict_order")
            out["hnsw_iterative_strict_order"]["queries_with_same_keys_as_exact"] = {"num": sum(it[k] == exact[k] for k in exact), "den": len(exact)}
            out["table_size_bytes"] = c.execute("SELECT pg_total_relation_size('knowledge_embedding')").fetchone()[0]
    finally:
        with psycopg.connect(admin_conninfo("harness"), autocommit=True) as adm:
            adm.execute(f"DROP DATABASE IF EXISTS {db} WITH (FORCE)")
            out["database_dropped"] = adm.execute("SELECT count(*) FROM pg_database WHERE datname = %s", (db,)).fetchone()[0] == 0
    out["generated_at"] = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())
    (ROOT / "runs" / "pgvector.json").write_text(json.dumps(out, indent=1, ensure_ascii=False) + "\n")
    print(json.dumps({k: v for k, v in out.items() if k != "sql"}, indent=1))


if __name__ == "__main__":
    main()

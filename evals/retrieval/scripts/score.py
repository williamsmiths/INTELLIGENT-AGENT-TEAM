"""Score lexical-only and lexical + semantic (appended) retrieval on the frozen fixture.

  uv run --with numpy python scripts/score.py            # writes report.json and runs/tables.md

Inputs: fixture.json, errata.json, runs/lexical.json (from internal/memory TestRetrievalDump),
runs/model-<tag>.json + runs/emb-<tag>.npz (from embed_model.py), optional runs/pgvector.json and
runs/failure.json. Every rule below was fixed before the first model was run."""
import json
import random
import time
from pathlib import Path

import numpy as np

import fixture_io

ROOT = Path(__file__).resolve().parents[1]
RUNS = ROOT / "runs"
LIMIT = 10                      # results per query (same as the lexical dump)
GRID = [round(i / 100, 2) for i in range(0, 101)]
COMBINED = ["paraphrase_vi", "paraphrase_en", "cross_lang_en_to_vi", "cross_lang_vi_to_en"]
MIN_DELTA = 0.20                # spec B.1 enable criterion
EPS = 1e-9


def pct(xs, p):
    xs = sorted(xs)
    return round(xs[min(len(xs) - 1, int(p * len(xs)))], 3) if xs else None


def rate(num, den):
    return {"num": round(num, 4), "den": den, "value": round(num / den, 4) if den else None}


class Model:
    def __init__(self, meta):
        self.meta, self.tag = meta, meta["tag"]
        z = np.load(RUNS / f"emb-{self.tag}.npz")
        norm = lambda m: m / np.linalg.norm(m, axis=1, keepdims=True)
        self.p, self.q = norm(z["passages"].astype(np.float64)), norm(z["queries"].astype(np.float64))
        self.pids = [str(x) for x in z["passage_ids"]]
        self.qrow = {str(x): i for i, x in enumerate(z["query_ids"])}
        self.q_ms = z["query_ms"]
        self.scope = {}

    def ranked(self, query, restrict=True):
        """(passage id, cosine) of the items the caller may see, best first: own project + GLOBAL."""
        key = (query["project"], restrict)
        if key not in self.scope:
            ok = (lambda pid: pid.split("/")[0] in (query["project"], "global")) if restrict else (lambda pid: True)
            self.scope[key] = np.array([i for i, pid in enumerate(self.pids) if ok(pid)])
        idx = self.scope[key]
        start = time.perf_counter()
        cos = self.p[idx] @ self.q[self.qrow[query["id"]]]
        order = np.argsort(-cos, kind="stable")
        ms = (time.perf_counter() - start) * 1000
        return [(self.pids[idx[i]], float(cos[i])) for i in order], ms


def hybrid(lex_hits, sem_ranked, threshold):
    """Lexical hits first and untouched; semantic hits >= threshold appended, de-duplicated, up to LIMIT."""
    hits = [dict(h, id=f"{h['project']}/{h['key']}") for h in lex_hits][:LIMIT]
    seen = {h["id"] for h in hits}
    for pid, cos in sem_ranked:
        if len(hits) >= LIMIT or cos < threshold:
            break
        if pid not in seen:
            seen.add(pid)
            hits.append({"id": pid, "key": pid.split("/", 1)[1], "project": pid.split("/")[0], "stage": "semantic", "score": round(cos, 4)})
    return hits


def judge(query, hits):
    want = {f"{query['project']}/{k}" for k in query["want"]}
    found = sum(1 for h in hits[:5] if h["id"] in want)
    rr, stage = 0.0, "none"
    for rank, h in enumerate(hits):
        if h["id"] in want:
            rr, stage = 1 / (rank + 1), h["stage"]
            break
    return {"recall": found / len(want) if want else None, "hit5": found > 0, "rr": rr, "stage": stage,
            "semantic_appended": sum(1 for h in hits if h["stage"] == "semantic"),
            "lexical_hits": sum(1 for h in hits if h["stage"] != "semantic"),
            "cross_project": sum(1 for h in hits if h["project"] not in (query["project"], "global"))}


def aggregate(rows):
    """rows: [(query, judgement, latency samples)] of one group or set."""
    pos = [(q, j) for q, j, _ in rows if q["group"] != "negative"]
    neg = [(q, j) for q, j, _ in rows if q["group"] == "negative"]
    lat = [ms for _, _, l in rows for ms in l]
    out = {"queries": len(rows), "latency_ms": {"samples": len(lat), "p50": pct(lat, 0.5), "p95": pct(lat, 0.95)},
           "cross_project_hits": sum(j["cross_project"] for _, j, _ in rows),
           "semantic_appended_avg": round(sum(j["semantic_appended"] for _, j, _ in rows) / len(rows), 3) if rows else None}
    if pos:
        stages = {}
        for _, j in pos:
            stages[j["stage"]] = stages.get(j["stage"], 0) + 1
        out.update(recall_at_5=rate(sum(j["recall"] for _, j in pos), len(pos)),
                   hit_at_5=rate(sum(j["hit5"] for _, j in pos), len(pos)),
                   mrr=rate(sum(j["rr"] for _, j in pos), len(pos)), first_relevant_stage=stages)
    if neg:
        out.update(junk_rate_semantic=rate(sum(j["semantic_appended"] > 0 for _, j in neg), len(neg)),
                   any_lexical_hit_rate=rate(sum(j["lexical_hits"] > 0 for _, j in neg), len(neg)))
    return out


def evaluate(queries, lex, model, threshold):
    """Per-query judgement and latency samples for one configuration."""
    rows = []
    for q in queries:
        l = lex[q["id"]]
        if model is None:
            hits, lat = hybrid(l["hits"], [], 2.0), l["latency_ms"]
        else:
            ranked, cos_ms = model.ranked(q)
            hits = hybrid(l["hits"], ranked, threshold)
            emb = model.q_ms[model.qrow[q["id"]]]
            lat = [l["latency_ms"][i] + float(emb[i]) + cos_ms for i in range(len(emb))]
        rows.append((q, judge(q, hits), lat, hits))
    return rows


def tune(queries, lex, model):
    """Threshold sweep on the tuning queries. Primary rule: maximise F1 where TP = positive queries with a
    wanted key in the hybrid top 5, FN = the other positive queries, FP = negative queries that receive
    at least one semantic hit. Secondary (reported only): Youden J = TP/P - FP/N. Ties -> highest threshold."""
    sweep = []
    for t in GRID:
        rows = evaluate(queries, lex, model, t)
        tp = sum(j["hit5"] for q, j, _, _ in rows if q["group"] != "negative")
        p = sum(q["group"] != "negative" for q in queries)
        fp = sum(j["semantic_appended"] > 0 for q, j, _, _ in rows if q["group"] == "negative")
        n = len(queries) - p
        sweep.append({"threshold": t, "tp": tp, "fn": p - tp, "fp": fp, "negatives": n,
                      "f1": round(2 * tp / (2 * tp + fp + (p - tp)), 4), "youden_j": round(tp / p - fp / n, 4)})
    best = lambda k: max(s["threshold"] for s in sweep if s[k] >= max(x[k] for x in sweep) - EPS)
    return sweep, best("f1"), best("youden_j")


def summarise(rows, groups):
    by = lambda keep: [(q, j, l) for q, j, l, _ in rows if keep(q)]
    out = {"groups": {g: aggregate(by(lambda q, g=g: q["group"] == g)) for g in groups}}
    out["combined_paraphrase_cross_language"] = aggregate(by(lambda q: q["group"] in COMBINED))
    out["all"] = aggregate(by(lambda q: True))
    return out


def bootstrap_delta(base_rows, new_rows, seed):
    pairs = [(n[1]["recall"] - b[1]["recall"]) for b, n in zip(base_rows, new_rows) if b[0]["group"] in COMBINED]
    rng = random.Random(seed)
    means = sorted(sum(rng.choices(pairs, k=len(pairs))) / len(pairs) for _ in range(10000))
    return {"n": len(pairs), "ci95": [round(means[250], 4), round(means[9749], 4)], "resamples": 10000}


def main():
    fx = fixture_io.load(ROOT / "fixture.json")
    sha = fixture_io.sha256(ROOT / "fixture.json")
    errata = json.loads((ROOT / "errata.json").read_text()) if (ROOT / "errata.json").exists() else []
    for e in errata:  # corrections to labels, applied here and shown in the report; the fixture stays frozen
        next(q for q in fx["queries"] if q["id"] == e["id"])["want"] = e["want"]
    lexical = json.loads((RUNS / "lexical.json").read_text())
    assert lexical["fixture_sha256"] == sha, "lexical dump was made from another fixture"
    lex = {q["id"]: q for q in lexical["queries"]}
    models = []
    for path in sorted(RUNS.glob("model-*.json")):
        meta = json.loads(path.read_text())
        assert meta["fixture_sha256"] == sha, f"{path.name} was made from another fixture"
        models.append(Model(meta))

    groups = fx["groups"]
    sets = {"tuning": [q for q in fx["queries"] if not q["holdout"]], "holdout": [q for q in fx["queries"] if q["holdout"]]}
    configs, per_query = [], {q["id"]: {"id": q["id"], "project": q["project"], "group": q["group"], "holdout": q["holdout"],
                                        "q": q["q"], "want": q["want"]} for q in fx["queries"]}
    base = {name: evaluate(qs, lex, None, None) for name, qs in sets.items()}
    configs.append({"name": "lexical", "model": None, "threshold": None,
                    "sets": {name: summarise(rows, groups) for name, rows in base.items()}})
    for rows in base.values():
        for q, j, _, hits in rows:
            per_query[q["id"]]["lexical"] = {"top5": [h["key"] + ":" + h["stage"] for h in hits[:5]], "recall_at_5": j["recall"]}

    criterion, secondary = [], []
    for m in models:
        sweep, t_f1, t_j = tune(sets["tuning"], lex, m)
        rows = {name: evaluate(qs, lex, m, t_f1) for name, qs in sets.items()}
        cfg = {"name": "lexical+" + m.tag, "model": m.meta["model"], "threshold": t_f1, "threshold_rule": "max F1 on tuning queries",
               "sets": {name: summarise(r, groups) for name, r in rows.items()}, "tuning_sweep": sweep}
        probe = [q for q in fx["queries"] if q["group"] != "negative"]
        cfg["unrestricted_probe"] = {
            "what": "positive queries whose best semantic match over BOTH projects is an item of the other project (what the project filter prevents)",
            "num": sum(m.ranked(q, restrict=False)[0][0][0].split("/")[0] not in (q["project"], "global") for q in probe), "den": len(probe)}
        configs.append(cfg)
        for r in rows.values():
            for q, j, _, hits in r:
                ranked, _ = m.ranked(q)
                want = {f"{q['project']}/{k}" for k in q["want"]}
                per_query[q["id"]]["lexical+" + m.tag] = {
                    "top5": [h["key"] + ":" + h["stage"] for h in hits[:5]], "recall_at_5": j["recall"],
                    "best_semantic": [ranked[0][0].split("/", 1)[1], round(ranked[0][1], 4)],
                    "wanted_semantic": [[pid.split("/", 1)[1], round(c, 4), rank + 1] for rank, (pid, c) in enumerate(ranked) if pid in want]}

        hold, lexh = cfg["sets"]["holdout"], configs[0]["sets"]["holdout"]
        delta = hold["combined_paraphrase_cross_language"]["recall_at_5"]["value"] - lexh["combined_paraphrase_cross_language"]["recall_at_5"]["value"]
        worse = [f"{name}/{g}" for name in sets for g in groups if g != "negative"
                 and cfg["sets"][name]["groups"][g]["recall_at_5"]["value"] < configs[0]["sets"][name]["groups"][g]["recall_at_5"]["value"] - EPS]
        criterion.append({
            "config": cfg["name"], "threshold": t_f1,
            "holdout_combined_recall_at_5": {"lexical": lexh["combined_paraphrase_cross_language"]["recall_at_5"],
                                             "hybrid": hold["combined_paraphrase_cross_language"]["recall_at_5"]},
            "delta": round(delta, 4), "delta_bootstrap": bootstrap_delta(base["holdout"], rows["holdout"], fx["holdout_seed"]),
            "delta_required": MIN_DELTA, "delta_ok": delta >= MIN_DELTA - EPS,
            "groups_worse_than_lexical": worse, "no_group_worse": not worse,
            "holdout_junk_rate": hold["groups"]["negative"]["junk_rate_semantic"],
            "tuning_combined_recall_at_5": cfg["sets"]["tuning"]["combined_paraphrase_cross_language"]["recall_at_5"],
            "tuning_junk_rate": cfg["sets"]["tuning"]["groups"]["negative"]["junk_rate_semantic"],
            "single_query_embed_p50_ms": m.meta["single_query_ms"]["p50"],
            "passes": delta >= MIN_DELTA - EPS and not worse})
        rows_j = {name: evaluate(qs, lex, m, t_j) for name, qs in sets.items()}
        sj = {name: summarise(r, groups) for name, r in rows_j.items()}
        secondary.append({"config": cfg["name"], "threshold": t_j, "threshold_rule": "max Youden J (TP/P - FP/N) on tuning queries",
                          **{f"{name}_{k}": sj[name][a][b] for name in sets for k, a, b in
                             (("combined_recall_at_5", "combined_paraphrase_cross_language", "recall_at_5"),)},
                          **{f"{name}_junk_rate": sj[name]["groups"]["negative"]["junk_rate_semantic"] for name in sets},
                          "holdout_delta": round(sj["holdout"]["combined_paraphrase_cross_language"]["recall_at_5"]["value"]
                                                 - lexh["combined_paraphrase_cross_language"]["recall_at_5"]["value"], 4)})

    # Winner: among the models that pass on the holdout, the best on the TUNING set (so the holdout is not
    # used to pick), then lower tuning junk rate, then faster query embedding.
    passing = sorted((c for c in criterion if c["passes"]),
                     key=lambda c: (-c["tuning_combined_recall_at_5"]["value"], c["tuning_junk_rate"]["value"], c["single_query_embed_p50_ms"]))
    decision = (f"ENABLE {next(m.meta['model'] for m in models if 'lexical+' + m.tag == passing[0]['config'])} with threshold {passing[0]['threshold']:.2f}"
                if passing else "DO NOT ENABLE")

    optional = lambda name: json.loads((RUNS / name).read_text()) if (RUNS / name).exists() else None
    sensitivity = None
    if passing:  # information only: the holdout columns were NOT used to choose anything
        win = next(m for m in models if "lexical+" + m.tag == passing[0]["config"])
        sensitivity = {"config": passing[0]["config"], "note": "information only; the decision uses the frozen threshold", "rows": []}
        for t in [round(passing[0]["threshold"] + d / 100, 2) for d in range(-8, 9, 2)]:
            row = {"threshold": t}
            for name, qs in sets.items():
                sm = summarise(evaluate(qs, lex, win, t), groups)
                row[name + "_combined_recall_at_5"] = sm["combined_paraphrase_cross_language"]["recall_at_5"]
                row[name + "_junk_rate"] = sm["groups"]["negative"]["junk_rate_semantic"]
            sensitivity["rows"].append(row)
    report = {
        "spec": "harness-s2-spec.md B.1 / AC-S2-024", "generated_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        "decision": decision, "winner": passing[0]["config"] if passing else None,
        "fixture": {"sha256": sha, "items": {n: len(p["items"]) for n, p in fx["projects"].items()} | {"global": len(fx["global"])},
                    "queries": len(fx["queries"]), "holdout_seed": fx["holdout_seed"],
                    "per_group": {g: {name: sum(q["group"] == g for q in qs) for name, qs in sets.items()} for g in groups}},
        "errata": errata,
        "method": {
            "lexical": lexical["method"] + f"; limit {lexical['limit']}, {lexical['rounds']} runs per query",
            "hybrid": "lexical hits first and untouched; items of the query's own project and confirmed GLOBAL items with cosine >= threshold are appended best first, de-duplicated, up to 10 results; similarities are computed only against that scope",
            "recall_at_5": "per query: wanted keys found in the first 5 / wanted keys; num = sum over queries, den = queries",
            "hit_at_5": "queries with at least one wanted key in the first 5 / queries",
            "mrr": "1/rank of the first wanted key within 10 results, 0 when absent; num = sum, den = queries",
            "junk_rate_semantic": "negative queries that receive at least one appended semantic hit / negative queries",
            "any_lexical_hit_rate": "negative queries for which the lexical stages return anything / negative queries (context for the junk rate)",
            "threshold": "grid 0.00..1.00 step 0.01 on tuning queries only; primary rule max F1 with TP = positive queries with a wanted key in the hybrid top 5, FN = other positive queries, FP = negative queries with a semantic hit; ties -> highest threshold; frozen before scoring the holdout",
            "latency": "lexical: wall time of memory.Search; hybrid: that + one HTTP POST /v1/embeddings for the query + cosine over the scope in numpy (run i of the embedding paired with run i of the search); no query-embedding cache",
            "criterion": f"holdout recall@5 on {' + '.join(COMBINED)}: hybrid - lexical >= {MIN_DELTA}, and no other group below lexical on tuning or holdout",
            "winner": "among passing models: highest tuning-set combined recall@5, then lower tuning junk rate, then faster single-query embedding",
            "delta_bootstrap": "paired bootstrap over the holdout queries of the combined groups, 10 000 resamples, seed = holdout_seed",
        },
        "lexical_run": {k: lexical[k] for k in ("generated_at", "items", "cross_project_hits")},
        "models": [{k: m.meta[k] for k in m.meta if k != "tei_info"} | {"tei": {k: m.meta["tei_info"].get(k) for k in
                   ("version", "sha", "model_dtype", "model_type", "max_input_length", "max_batch_tokens", "max_batch_requests", "max_client_batch_size", "tokenization_workers")}}
                   for m in models],
        "configurations": configs, "criterion": criterion, "secondary_threshold_analysis": secondary,
        "pgvector": optional("pgvector.json"), "sidecar_failure_behaviour": optional("failure.json"),
        "threshold_sensitivity_winner": sensitivity, "notes": optional("notes.json"),
        "per_query": list(per_query.values()),
    }
    (ROOT / "report.json").write_text(json.dumps(report, indent=1, ensure_ascii=False) + "\n")

    # Markdown tables for the README (copied, not retyped).
    f = lambda r: "N/A" if not r or r["value"] is None else f"{r['value']:.2f} ({r['num']:g}/{r['den']})"
    lines = []
    for name in ("holdout", "tuning"):
        lines += [f"### {name}: recall@5 (num/den)", "", "| group | " + " | ".join(c["name"] for c in configs) + " |", "|---|" + "---|" * len(configs)]
        for g in [g for g in groups if g != "negative"] + ["combined_paraphrase_cross_language"]:
            cell = lambda c: f(c["sets"][name]["combined_paraphrase_cross_language" if g.startswith("combined") else "groups"][g]["recall_at_5"]
                               if not g.startswith("combined") else c["sets"][name][g]["recall_at_5"])
            lines.append(f"| {g} | " + " | ".join(cell(c) for c in configs) + " |")
        lines += ["", f"### {name}: MRR, junk, latency", "", "| config | threshold | MRR all positive | junk on negative | lexical any-hit on negative | cross-project hits | p50 ms | p95 ms |", "|---|---|---|---|---|---|---|---|"]
        for c in configs:
            s = c["sets"][name]
            lines.append(f"| {c['name']} | {c['threshold'] if c['threshold'] is not None else '-'} | {f(s['all']['mrr'])} | {f(s['groups']['negative'].get('junk_rate_semantic'))} | "
                         f"{f(s['groups']['negative'].get('any_lexical_hit_rate'))} | {s['all']['cross_project_hits']} | {s['all']['latency_ms']['p50']} | {s['all']['latency_ms']['p95']} |")
        lines.append("")
    lines += ["### criterion (holdout)", "", "| config | threshold | lexical | hybrid | delta | CI95 | >= 0.20 | no group worse | passes |", "|---|---|---|---|---|---|---|---|---|"]
    for c in criterion:
        lines.append(f"| {c['config']} | {c['threshold']} | {f(c['holdout_combined_recall_at_5']['lexical'])} | {f(c['holdout_combined_recall_at_5']['hybrid'])} | "
                     f"{c['delta']:+.2f} | {c['delta_bootstrap']['ci95']} | {c['delta_ok']} | {c['no_group_worse']} | {c['passes']} |")
    lines += ["", "### secondary threshold (Youden J)", "", "| config | threshold | tuning combined | holdout combined | holdout delta | tuning junk | holdout junk |", "|---|---|---|---|---|---|---|"]
    for s in secondary:
        lines.append(f"| {s['config']} | {s['threshold']} | {f(s['tuning_combined_recall_at_5'])} | {f(s['holdout_combined_recall_at_5'])} | {s['holdout_delta']:+.2f} | {f(s['tuning_junk_rate'])} | {f(s['holdout_junk_rate'])} |")
    if sensitivity:
        lines += ["", f"### threshold sensitivity of {sensitivity['config']} (information only)", "",
                  "| threshold | tuning combined | tuning junk | holdout combined | holdout junk |", "|---|---|---|---|---|"]
        for r in sensitivity["rows"]:
            lines.append(f"| {r['threshold']:.2f} | {f(r['tuning_combined_recall_at_5'])} | {f(r['tuning_junk_rate'])} | {f(r['holdout_combined_recall_at_5'])} | {f(r['holdout_junk_rate'])} |")
    lines += ["", "### models", "", "| tag | model | revision | dim | warm load s | RAM after load MB | RAM peak MB | query p50/p95 ms | batch 32 p50/p95 ms | 1 000 passages wall/CPU s |", "|---|---|---|---|---|---|---|---|---|---|"]
    for m in models:
        x = m.meta
        lines.append(f"| {x['tag']} | {x['model']} | {str(x['model_sha'])[:12]} | {x['dim']} | {x['warm_load_s']} | {x['memory_after_load_mb']} | {x['memory_peak_mb']} | "
                     f"{x['single_query_ms']['p50']}/{x['single_query_ms']['p95']} | {x['batch32_passages_ms']['p50']}/{x['batch32_passages_ms']['p95']} | "
                     f"{x['embed_1000_passages']['wall_s']}/{x['embed_1000_passages']['cpu_s']} |")
    lines += ["", f"decision: {decision}", f"fixture sha256: {sha}"]
    (RUNS / "tables.md").write_text("\n".join(lines) + "\n")
    print("\n".join(lines))


if __name__ == "__main__":
    main()

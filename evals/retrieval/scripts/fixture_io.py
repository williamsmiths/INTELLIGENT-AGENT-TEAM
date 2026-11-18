"""Read and write evals/retrieval/fixture.json in a stable, diff-friendly layout
(one item or query per line), and mark the holdout queries with a fixed seed."""
import hashlib
import json
import random
import sys
from pathlib import Path

GROUPS = ["vi_diacritics", "vi_no_diacritics", "english", "code", "paraphrase_vi", "paraphrase_en",
          "cross_lang_en_to_vi", "cross_lang_vi_to_en", "negative"]


def _line(obj):
    return json.dumps(obj, ensure_ascii=False, separators=(", ", ": "))


def _list(rows, indent):
    pad = " " * indent
    return "[\n" + ",\n".join(pad + "  " + _line(r) for r in rows) + "\n" + pad + "]"


def dump(fx) -> str:
    out = ["{"]
    for k in ("version", "description", "holdout_seed", "holdout_share", "groups"):
        out.append(f"  {json.dumps(k)}: {_line(fx[k])},")
    out.append('  "projects": {')
    names = list(fx["projects"])
    for i, name in enumerate(names):
        p = fx["projects"][name]
        out.append(f"    {json.dumps(name)}: {{")
        out.append(f'      "domain": {_line(p["domain"])},')
        out.append(f'      "items": {_list(p["items"], 6)}')
        out.append("    }" + ("," if i < len(names) - 1 else ""))
    out.append("  },")
    out.append(f'  "global": {_list(fx["global"], 2)},')
    out.append(f'  "queries": {_list(fx["queries"], 2)}')
    out.append("}")
    return "\n".join(out) + "\n"


def load(path):
    return json.loads(Path(path).read_text(encoding="utf-8"))


def sha256(path):
    return hashlib.sha256(Path(path).read_bytes()).hexdigest()


def mark_holdout(fx):
    """30% of every group, chosen with random.Random(holdout_seed), groups in GROUPS order."""
    rng = random.Random(fx["holdout_seed"])
    by_id = {q["id"]: q for q in fx["queries"]}
    for g in GROUPS:
        ids = sorted(q["id"] for q in fx["queries"] if q["group"] == g)
        chosen = set(rng.sample(ids, round(fx["holdout_share"] * len(ids))))
        for i in ids:
            by_id[i]["holdout"] = i in chosen
    return fx


def validate(fx):
    errs = []
    if fx["groups"] != GROUPS:
        errs.append("groups differ from the expected list")
    for name, p in fx["projects"].items():
        keys = [it["key"] for it in p["items"]]
        if len(keys) != len(set(keys)):
            errs.append(f"duplicate key in {name}")
    ids = [q["id"] for q in fx["queries"]]
    if len(ids) != len(set(ids)):
        errs.append("duplicate query id")
    for q in fx["queries"]:
        keys = {it["key"] for it in fx["projects"][q["project"]]["items"]}
        if q["group"] not in GROUPS:
            errs.append(f"{q['id']}: unknown group")
        if (q["group"] == "negative") != (len(q["want"]) == 0):
            errs.append(f"{q['id']}: want must be empty exactly for negative queries")
        for k in q["want"]:
            if k not in keys:
                errs.append(f"{q['id']}: want {k} is not an item of {q['project']}")
    return errs


if __name__ == "__main__":
    path = sys.argv[2]
    fx = load(path)
    errs = validate(fx)
    if errs:
        sys.exit("\n".join(errs))
    if sys.argv[1] == "mark-holdout":
        Path(path).write_text(dump(mark_holdout(fx)), encoding="utf-8")
    counts = {g: [0, 0] for g in GROUPS}
    for q in fx["queries"]:
        counts[q["group"]][1] += 1
        counts[q["group"]][0] += bool(q.get("holdout"))
    print({n: len(p["items"]) for n, p in fx["projects"].items()}, "global", len(fx["global"]), "queries", len(fx["queries"]))
    print({g: f"{h}/{n}" for g, (h, n) in counts.items()})
    print("sha256", sha256(path))

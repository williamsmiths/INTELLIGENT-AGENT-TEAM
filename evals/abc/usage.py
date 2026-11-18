"""Token and cost accounting for pilot runs.

Ground truth is the Claude Code session transcript of the agent workspace
(OpenBot's own usage table undercounts output tokens: spec L13). Prices are
OpenBot's list prices per million tokens (src/backend/agent/usage-pricing.ts).
"""
import glob, json, os, sqlite3

PRICES = {  # model prefix -> (input, cache_read, cache_write, output) USD per 1M tokens
    "claude-haiku-5-5": (0.1, 0.01, 0.2, 0.5),
    "claude-sonnet-5-5": (2.0, 0.1, 4.0, 10.0),
    "claude-opus-5-5": (4.0, 0.2, 8.0, 20.0),
}
HOME = os.path.expanduser("~")


def _price(model):
    for prefix, p in PRICES.items():
        if model and model.startswith(prefix):
            return p
    return None


def transcript_dir(agent_id):
    slug = (HOME + "/OpenBot/Agents/" + agent_id).replace("/", "-")
    return os.path.join(HOME, ".claude", "projects", slug)


def transcript_usage(agent_id, t0_iso, t1_iso):
    """Sum usage of assistant API messages with t0 <= timestamp <= t1 (ISO-8601 UTC strings)."""
    tot = {"input": 0, "cache_read": 0, "cache_write": 0, "output": 0, "api_calls": 0, "cost_usd": 0.0, "unpriced_models": []}
    seen = set()
    for path in glob.glob(os.path.join(transcript_dir(agent_id), "*.jsonl")):
        with open(path, errors="replace") as fh:
            for line in fh:
                try:
                    j = json.loads(line)
                except ValueError:
                    continue
                msg = j.get("message") or {}
                u = msg.get("usage")
                ts = j.get("timestamp") or ""
                if j.get("type") != "assistant" or not u or not (t0_iso <= ts <= t1_iso):
                    continue
                mid = msg.get("id")
                if mid in seen:
                    continue
                seen.add(mid)
                i, cr, cw, o = (u.get("input_tokens") or 0, u.get("cache_read_input_tokens") or 0,
                                u.get("cache_creation_input_tokens") or 0, u.get("output_tokens") or 0)
                tot["input"] += i; tot["cache_read"] += cr; tot["cache_write"] += cw; tot["output"] += o
                tot["api_calls"] += 1
                p = _price(msg.get("model"))
                if p is None:
                    if msg.get("model") not in tot["unpriced_models"]:
                        tot["unpriced_models"].append(msg.get("model"))
                    continue
                tot["cost_usd"] += (i * p[0] + cr * p[1] + cw * p[2] + o * p[3]) / 1e6
    tot["cost_usd"] = round(tot["cost_usd"], 5)
    return tot


def openbot_usage(agent_id, t0_iso, t1_iso):
    """What OpenBot's own table recorded for the same window (for the discrepancy report)."""
    con = sqlite3.connect("file:" + HOME + "/.config/OpenBot/openbot.db?mode=ro", uri=True, timeout=5)
    con.execute("pragma query_only=1")
    tot = {"input": 0, "cache_read": 0, "cache_write": 0, "output": 0, "rows": 0, "rows_without_cost": 0, "cost_usd": 0.0}
    for tokens, cost in con.execute(
            "select tokens_json, estimated_cost_usd from agent_usage_records where agent_id=? and occurred_at>=? and occurred_at<=?",
            (agent_id, t0_iso, t1_iso)):
        t = json.loads(tokens or "{}")
        tot["input"] += t.get("uncachedInput", 0); tot["cache_read"] += t.get("cachedInput", 0)
        tot["cache_write"] += t.get("cacheCreation", 0); tot["output"] += t.get("output", 0)
        tot["rows"] += 1
        if cost is None:
            tot["rows_without_cost"] += 1
        else:
            tot["cost_usd"] += cost
    con.close()
    tot["cost_usd"] = round(tot["cost_usd"], 5)
    return tot


def pilot_total(agent_ids, since_iso):
    """All pilot-agent spend since the authorization time, from transcripts."""
    return {a: transcript_usage(a, since_iso, "9999") for a in agent_ids}


if __name__ == "__main__":
    import sys
    ids = ["agent-2701c561-3ed0-4151-947b-ba238d8c5f16", "agent-66322629-c2c7-4cd3-84f5-74ed5c9b97b9"]
    out = pilot_total(ids, "2026-11-17T15:00:00")
    print(json.dumps(out, indent=1))
    print("pilot agents total USD:", round(sum(v["cost_usd"] for v in out.values()), 5))

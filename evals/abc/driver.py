#!/usr/bin/env python3
"""A/B/C driver: runs one scenario on a real OpenBot pilot agent and scores it.

Everything is real: the OpenBot agent (automation API), harnessd (owner
socket through `kch owner`), the repository clone, the hidden oracle. The
same user messages are sent in every configuration; only the lease mode
differs (OBSERVE = A, CONTEXT = B, FULL = C).

Usage: driver.py setup | run <scenario-id> <A|B|C> <n> | spend
"""
import fnmatch, json, os, re, shutil, subprocess, sys, time, urllib.request, urllib.error
from datetime import datetime, timezone

import usage

ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
HERE = os.path.dirname(os.path.abspath(__file__))
HOME = os.path.expanduser("~")
KCH = os.path.join(HOME, ".local", "share", "kc-harness", "bin", "kch")
STATE = os.path.join(ROOT, ".work", "abc-state.json")
OUT = os.path.join(ROOT, ".work", "abc")
AUTO = os.path.join(HOME, ".config", "OpenBot", "automation")
ENG = "agent-66322629-c2c7-4cd3-84f5-74ed5c9b97b9"
PROD = "agent-2701c561-3ed0-4151-947b-ba238d8c5f16"
TEAM_LEADS = ["agent-35985d0b-ba77-4fab-9f13-dedb8f6eec0b", "agent-9fc11f71-f2fc-44f1-bd4e-77fefd0f49b5",
              "agent-cec212f5-58a5-453e-9aa4-aea5d169ca49", "agent-d7575190-ae86-4a3b-bf14-d41367a5139e"]  # V5 team, same 3 USD cap
SINCE = "2026-11-17T15:00:00"      # pilot authorization time (UTC)
DIRECT_PROBES_USD = 0.0808         # direct `claude -p` probes before the pilots existed
CAP_USD = 3.00
STOP_AT_USD = 2.80                 # no new run is started above this
MODES = {"A": "OBSERVE", "B": "CONTEXT", "C": "FULL", "G": "FULL"}  # G = C without the understanding gate (Task 2 ablation)
NO_GATE = ["questions", "consult", "convergence", "lesson"]
RUN_TIMEOUT = 15 * 60
TASK_MAX_COST_MICROUSD = 400_000
TAILER_SETTLE_SECONDS = 15        # tailer polls openbot.db every 5 s; OpenBot writes usage at turn end


def now():
    return datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%S.%f")[:-3] + "Z"


def owner(method, path, body=None, ok=(200, 201)):
    args = [KCH, "owner", method, path] + ([json.dumps(body)] if body is not None else [])
    p = subprocess.run(args, capture_output=True, text=True, timeout=1200)
    if p.returncode != 0:
        raise RuntimeError(f"owner {method} {path}: {p.stderr.strip()[:400]}")
    return json.loads(p.stdout) if p.stdout.strip() else None


def auto(method, path, body=None):
    url = open(os.path.join(AUTO, "url")).read().strip()
    tok = open(os.path.join(AUTO, "token")).read().strip()
    req = urllib.request.Request(url + path, method=method, data=None if body is None else json.dumps(body).encode(),
                                 headers={"Authorization": "Bearer " + tok, "Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(req, timeout=30) as r:
            return r.status, json.loads(r.read() or b"null")
    except urllib.error.HTTPError as e:
        return e.code, json.loads(e.read() or b"null")


RATE_WAIT_MAX = 75 * 60   # OpenBot allows 30 local-script messages per agent per hour


def send(agent, text, key):
    waited = 0
    while True:
        status, body = auto("POST", f"/v1/agents/{agent}/messages", {"text": text, "clientMessageId": key})
        if status == 202:
            return body["deliveries"][0]["id"]
        if status == 429 and waited < RATE_WAIT_MAX:
            print(f"  rate limited by OpenBot; waiting (waited {waited // 60} min)", flush=True)
            time.sleep(120)
            waited += 120
            continue
        raise RuntimeError(f"automation send: {status} {body}")


def delivery(agent, did):
    return auto("GET", f"/v1/agents/{agent}/deliveries/{did}")[1] or {}


def clear_context(agent):
    for _ in range(40):
        status, _ = auto("POST", f"/v1/agents/{agent}/context/clear")
        if status == 200:
            return
        time.sleep(3)
    raise RuntimeError("agent stays busy; cannot clear its context")


def git(repo, *args):
    return subprocess.run(["git", "-C", repo, "-c", "core.fsmonitor=false", "-c", "core.hooksPath=/dev/null"] + list(args),
                          capture_output=True, text=True, timeout=120)


def reset_repo(repo, base):
    git(repo, "reset", "-q", "--hard", base)
    git(repo, "clean", "-qfd")


def changed_files(repo):
    out = git(repo, "status", "--porcelain", "--untracked-files=all").stdout
    return sorted({line[3:].split(" -> ")[-1].strip('"') for line in out.splitlines() if line.strip()})


def in_scope(path, patterns):
    for pat in patterns:
        rx = re.escape(pat).replace(r"\*\*/", "(?:.*/)?").replace(r"\*\*", ".*").replace(r"\*", "[^/]*")
        if re.fullmatch(rx, path):
            return True
    return False


def load_state():
    return json.load(open(STATE)) if os.path.exists(STATE) else {}


def scenario(sid):
    for s in json.load(open(os.path.join(HERE, "scenarios.json")))["scenarios"]:
        if s["id"] == sid:
            return s
    raise SystemExit("unknown scenario " + sid)


def spend():
    per = usage.pilot_total([ENG, PROD] + TEAM_LEADS, SINCE)
    agents = sum(v["cost_usd"] for v in per.values())
    return {"direct_probes_usd": DIRECT_PROBES_USD, "pilot_agents_usd": round(agents, 4),
            "total_usd": round(DIRECT_PROBES_USD + agents, 4), "cap_usd": CAP_USD}


def run_oracle(scn):
    o, repo = scn["oracle"], scn["repo"]
    dest = os.path.join(repo, o["dest"])
    shutil.copyfile(os.path.join(HERE, o["file"]), dest)
    env = dict(os.environ, GOPROXY="off", CI="1")
    try:
        p = subprocess.run(o["command"], shell=True, cwd=repo, capture_output=True, text=True, timeout=300, env=env)
        cases = {}
        if o["kind"] == "vitest":
            m = re.search(r"\{.*\}\s*$", p.stdout, re.S)
            data = json.loads(m.group(0)) if m else {}
            for f in data.get("testResults", []):
                for a in f.get("assertionResults", []):
                    cases[a["title"]] = a["status"] == "passed"
        else:
            for line in p.stdout.splitlines():
                try:
                    j = json.loads(line)
                except ValueError:
                    continue
                t = j.get("Test", "")
                if t.startswith("TestOracle/") and j.get("Action") in ("pass", "fail"):
                    cases[t.split("/", 1)[1].replace("_", " ")] = j["Action"] == "pass"
        compiled = bool(cases)
    finally:
        os.remove(dest)
    pres = subprocess.run(o["preserve_command"], shell=True, cwd=repo, capture_output=True, text=True, timeout=600, env=env)
    rules = {}
    for title, ok in cases.items():
        rules.setdefault(title.split()[0], []).append(ok)
    return {"compiled": compiled, "cases_passed": sum(cases.values()), "cases_total": len(cases),
            "rules": {r: all(v) for r, v in sorted(rules.items())},
            "rules_passed": sum(all(v) for v in rules.values()), "rules_total": len(rules),
            "existing_tests_pass": pres.returncode == 0, "raw_tail": (p.stdout + p.stderr)[-600:] if not compiled else ""}


def gaps_found(scn, texts):
    blob = "\n".join(t for t in texts if t).lower()
    return {g["id"]: any(k.lower() in blob for k in g["keywords"]) for g in scn["gaps"]}


def wait_delivery(agent, did, deadline, on_tick=None):
    while time.time() < deadline:
        d = delivery(agent, did)
        if d.get("status") in ("completed", "failed", "interrupted", "cancelled"):
            return d
        if on_tick:
            on_tick()
        time.sleep(3)
    return {"status": "timeout", "answer": None}


def run(sid, arm, n, agent=ENG):
    st, scn = load_state(), scenario(sid)
    sp = spend()
    if sp["total_usd"] >= STOP_AT_USD:
        raise SystemExit(f"pilot budget: {sp['total_usd']} USD used, no new run started")
    proj, repo, mode = st["projects"][sid], scn["repo"], MODES[arm]
    reset_repo(repo, scn["base_commit"])
    clear_context(agent)
    key = f"{sid.split('-')[0].upper()}-{arm}{n}-{int(time.time())}"
    task = dict(scn["task"], key=key, repo_path=repo)
    tid = owner("POST", f"/v1/projects/{proj}/tasks", task)["id"]
    owner("PUT", f"/v1/tasks/{tid}/budget", {"max_cost_microusd": TASK_MAX_COST_MICROUSD})
    owner("POST", f"/v1/tasks/{tid}/ready")
    try:
        return _run(sid, arm, n, agent, st, scn, proj, repo, mode, key, tid)
    except BaseException:
        try:   # never leave a task holding the agent's lease
            owner("POST", f"/v1/tasks/{tid}/supersede")
        except Exception:
            pass
        raise


def _run(sid, arm, n, agent, st, scn, proj, repo, mode, key, tid):
    body = {"principal": st["principals"][agent], "mode": mode, "notify": False}
    if arm == "G":
        body["intel_features"] = NO_GATE
    owner("POST", f"/v1/tasks/{tid}/assign", body)
    t0, started, fb = now(), time.time(), {"sent": False, "timing": None, "delivery": None}
    deadline = started + RUN_TIMEOUT
    brief = scn["brief"].replace("{repo}", repo)
    d1 = send(agent, brief, key + "-brief")

    def feedback(timing):
        if fb["sent"]:
            return
        fb.update(sent=True, timing=timing, at=now())
        if arm != "A":   # the harness learns of the feedback only when it exists in the configuration
            owner("POST", f"/v1/tasks/{tid}/feedback", {"text": scn["feedback"], "notify": False})
        fb["delivery"] = send(agent, scn["feedback"], key + "-feedback")

    interventions = []

    def answer_blocking():
        # C only: a blocking question parks the task until the owner answers. The driver answers at once
        # with the agent's own default, adding no information, and counts it as an intervention.
        if arm not in ("C", "G"):
            return
        try:
            qs = owner("GET", f"/v1/tasks/{tid}/questions").get("questions") or []
        except RuntimeError:
            return
        for q in qs:
            if q.get("status") == "OPEN" and q.get("blocking"):
                opt = q.get("default") if q.get("default") in (q.get("options") or []) else None
                body = {"option": opt} if opt else {"text": q.get("default") or "Dùng phương án mặc định của bạn."}
                try:
                    owner("POST", f"/v1/questions/{q['id']}/answer", body)
                    interventions.append({"at": now(), "kind": "answer_default", "question": q.get("text", "")[:200]})
                except RuntimeError:
                    pass

    def tick():
        answer_blocking()
        if changed_files(repo):
            feedback("mid")

    first = wait_delivery(agent, d1, deadline, tick)
    feedback("after")
    second = wait_delivery(agent, fb["delivery"], deadline, answer_blocking)
    t1, wall = now(), round(time.time() - started, 1)
    answers = [first.get("answer"), second.get("answer")]

    res = {"scenario": sid, "arm": arm, "mode": mode, "n": n, "task_key": key, "t0": t0, "t1": t1, "wall_seconds": wall,
           "deliveries": [first.get("status"), second.get("status")], "feedback_timing": fb["timing"],
           "answers": [(a or "")[:6000] for a in answers]}
    files = changed_files(repo)
    res["changed_files"] = files
    res["out_of_scope_files"] = [f for f in files if not in_scope(f, scn["task"]["allowed_paths"])]
    res["oracle"] = run_oracle(scn)
    res["interventions"] = interventions
    res["usage_transcript"] = usage.transcript_usage(agent, t0, t1)
    res["usage_openbot_db"] = usage.openbot_usage(agent, t0, t1)
    other = PROD if agent == ENG else ENG   # a consult answered by the other pilot costs tokens too
    res["usage_transcript_other_agent"] = usage.transcript_usage(other, t0, t1)
    res["usage_transcript"]["cost_usd_both_agents"] = round(res["usage_transcript"]["cost_usd"] + res["usage_transcript_other_agent"]["cost_usd"], 5)
    harness_texts = []
    time.sleep(TAILER_SETTLE_SECONDS)  # let the harness tailer ingest the last usage rows of the turn
    try:
        tv = owner("GET", f"/v1/tasks/{tid}")
        res["harness"] = {"task_state": tv["task"]["state"], "revision": tv["task"]["revision"], "budget": tv["budget"]["task"]}
        ev = owner("GET", f"/v1/projects/{proj}/events?limit=1000")["events"]
        counts = {}
        for e in ev:
            if e.get("task_id") == tid:
                counts[e["type"]] = counts.get(e["type"], 0) + 1
        res["harness"]["journal"] = counts
        res["harness"]["evidence"] = [{"check": e["CheckID"], "result": e["Result"]} for e in owner("GET", f"/v1/tasks/{tid}/evidence")["evidence"]]
        for extra in ("questions", "understanding", "cost"):
            try:
                res["harness"][extra] = owner("GET", f"/v1/tasks/{tid}/{extra}")
                harness_texts.append(json.dumps(res["harness"][extra], ensure_ascii=False))
            except RuntimeError:
                pass
    except RuntimeError as e:
        res["harness_error"] = str(e)
    res["gaps_in_answer"] = gaps_found(scn, answers)
    res["gaps_in_answer_or_harness"] = gaps_found(scn, answers + harness_texts)
    try:
        owner("POST", f"/v1/tasks/{tid}/supersede")
    except RuntimeError as e:
        res["cleanup_error"] = str(e)
    res["spend_after"] = spend()
    os.makedirs(OUT, exist_ok=True)
    json.dump(res, open(os.path.join(OUT, f"{sid}-{arm}-{n}.json"), "w"), ensure_ascii=False, indent=1)
    o = res["oracle"]
    print(f"{sid} {arm}{n}: rules {o['rules_passed']}/{o['rules_total']} cases {o['cases_passed']}/{o['cases_total']} "
          f"existing={o['existing_tests_pass']} out_of_scope={len(res['out_of_scope_files'])} "
          f"cost={res['usage_transcript']['cost_usd_both_agents']} interv={len(interventions)} calls={res['usage_transcript']['api_calls']} wall={wall}s "
          f"fb={fb['timing']} gaps={sum(res['gaps_in_answer'].values())}/{len(scn['gaps'])} spend={res['spend_after']['total_usd']}")
    return res


if __name__ == "__main__":
    cmd = sys.argv[1] if len(sys.argv) > 1 else ""
    if cmd == "spend":
        print(json.dumps(spend(), indent=1))
    elif cmd == "run":
        run(sys.argv[2], sys.argv[3], int(sys.argv[4]), PROD if len(sys.argv) > 5 and sys.argv[5] == "product" else ENG)
    elif cmd == "setup":
        import setup_abc
        setup_abc.main()
    else:
        print(__doc__)

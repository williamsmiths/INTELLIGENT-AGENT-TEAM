"""Add a real project for the V5 team (Owner tool, idempotent).

Usage:
  python3 -I deploy/team/add_project.py <slug> "<name>" <repo-dir> "<check command>" [--channel <openbot channel id>] [--budget-usd 1.0]
Example:
  python3 -I deploy/team/add_project.py my-app "My App" /var/tmp/kc-pilot/repos/my-app "go test ./..." --channel 1234-...

It reuses the four V5 Leads listed in deploy/team/leads.local.json (never creates agents), creates the
project only if the slug is new, sets limits and the delegate policy, adds the Leads as members
(Product = lead, others = worker with their specialty), re-enrolls their workspaces and, with --channel,
binds that OpenBot channel to the project. The repo must be a git repo outside $HOME with a clean tree.
"""
import json
import os
import subprocess
import sys

KCH = os.path.expanduser("~/.local/share/kc-harness/bin/kch")
HERE = os.path.dirname(os.path.abspath(__file__))
TASK_MAX_MICROUSD = 400_000      # per task; change to taste
PROJECT_MAX_MICROUSD = 1_000_000  # per project; must fit under the SYSTEM budget


def owner(method, path, body=None):
    args = [KCH, "owner", method, path] + ([json.dumps(body)] if body is not None else [])
    p = subprocess.run(args, capture_output=True, text=True, timeout=600)
    if p.returncode != 0:
        raise SystemExit(f"{method} {path}: {p.stderr.strip()[:300]}")
    return json.loads(p.stdout) if p.stdout.strip() else None


def main(argv):
    if len(argv) < 4:
        raise SystemExit(__doc__)
    slug, name, repo, check = argv[:4]
    channel = argv[argv.index("--channel") + 1] if "--channel" in argv else None
    leads = json.load(open(os.path.join(HERE, "leads.local.json")))  # {"product": {"agent": ..., "principal": ...}, ...}
    repo = os.path.realpath(repo)
    state_path = os.path.join(HERE, "projects.local.json")  # slug → project id, so a rerun reuses the project
    state = json.load(open(state_path)) if os.path.exists(state_path) else {}
    budget = int(float(argv[argv.index("--budget-usd") + 1]) * 1_000_000) if "--budget-usd" in argv else PROJECT_MAX_MICROUSD
    pid = state.get(slug)
    if pid is None:   # limits only when the project is new: a rerun keeps what the Owner set since
        pid = owner("POST", "/v1/projects", {"slug": slug, "name": name})["id"]
        state[slug] = pid
        json.dump(state, open(state_path, "w"), indent=1)
        owner("PUT", f"/v1/projects/{pid}/limits", {"max_active_leases": 2, "default_task_max_cost_microusd": TASK_MAX_MICROUSD,
                                                   "budget": {"max_cost_microusd": budget}})
    if repo not in (owner("GET", f"/v1/projects/{pid}").get("repos") or []):
        owner("POST", f"/v1/projects/{pid}/repos", {"path": repo})
    owner("PUT", f"/v1/projects/{pid}/delegate-policy", {"required_checks": [{"id": "unit", "command": check}], "protected_paths": []})
    for spec, lead in leads.items():
        me = owner("GET", f"/v1/principals/{lead['principal']}")
        member = {m["project_id"] for m in (me.get("principal") or me).get("memberships", [])}
        if pid not in member:
            owner("POST", f"/v1/principals/{lead['principal']}/memberships",
                  {"project_id": pid, "role": "lead" if spec == "product" else "worker"})
        ws = os.path.expanduser(f"~/OpenBot/Agents/{lead['agent']}")
        p = subprocess.run([KCH, "enroll", lead["principal"], ws], capture_output=True, text=True, timeout=60)
        print("enroll", spec, "ok" if p.returncode == 0 else p.stderr.strip()[:200])
    if channel:
        owner("PUT", f"/v1/projects/{pid}/conversations", {"kind": "channel", "openbot_id": channel})
        print("channel bound:", channel)
    print(json.dumps({"project": slug, "id": pid, "repo": repo, "check": check}))


if __name__ == "__main__":
    main(sys.argv[1:])

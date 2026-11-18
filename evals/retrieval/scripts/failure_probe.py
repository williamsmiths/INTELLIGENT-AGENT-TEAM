"""Observe how the running `embed` sidecar fails, as a client would see it.

  uv run python scripts/failure_probe.py --model <model id>

Writes runs/failure.json. Leaves the container running."""
import argparse
import json
import subprocess
import time
import urllib.error
import urllib.request
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
CONTAINER, BASE = "kc-harness-embed", "http://127.0.0.1:8870"


def docker(*args):
    return subprocess.run(["docker", *args], capture_output=True, text=True, timeout=120).stdout.strip()


def call(payload, timeout=10, path="/v1/embeddings"):
    start = time.perf_counter()
    try:
        req = urllib.request.Request(BASE + path, data=json.dumps(payload).encode() if payload is not None else None,
                                     headers={"Content-Type": "application/json"})
        with urllib.request.urlopen(req, timeout=timeout) as r:
            body = r.read()
            outcome = {"status": r.status, "body": body[:160].decode(errors="replace") if r.status != 200 else f"{len(body)} bytes"}
    except urllib.error.HTTPError as e:
        outcome = {"status": e.code, "body": e.read()[:200].decode(errors="replace")}
    except Exception as e:  # connection refused, reset, timeout
        outcome = {"error": type(e).__name__ + ": " + str(getattr(e, "reason", e))[:120]}
    outcome["ms"] = round((time.perf_counter() - start) * 1000, 1)
    return outcome


def until_ok(model, limit_s=600):
    """Poll every 100 ms; return seconds until the first 200 and the distinct outcomes seen on the way."""
    start, seen = time.monotonic(), []
    while time.monotonic() - start < limit_s:
        o = call({"model": model, "input": ["ping"]}, timeout=2)
        label = str(o.get("status") or o.get("error"))
        if label not in seen:
            seen.append(label)
        if o.get("status") == 200:
            return {"seconds_to_first_200": round(time.monotonic() - start, 1), "outcomes_in_order": seen}
        time.sleep(0.1)
    return {"seconds_to_first_200": None, "outcomes_in_order": seen}


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--model", required=True)
    m = ap.parse_args().model
    out = {"model": m, "container": CONTAINER, "restart_policy": docker("inspect", "-f", "{{.HostConfig.RestartPolicy.Name}}", CONTAINER)}
    out["ok_single"] = call({"model": m, "input": ["xin chào"]})
    out["batch_of_33_inputs"] = call({"model": m, "input": ["a"] * 33})
    out["empty_string_input"] = call({"model": m, "input": [""]})
    out["input_of_60000_chars_auto_truncated"] = call({"model": m, "input": ["kho hàng " * 6667]})
    out["other_model_name_in_request"] = call({"model": "not-the-loaded-model", "input": ["a"]})
    out["malformed_json_body_type"] = call({"model": m, "inputs_typo": ["a"]})

    docker("pause", CONTAINER)
    out["paused_container_client_timeout_2s"] = call({"model": m, "input": ["a"]}, timeout=2)
    docker("unpause", CONTAINER)
    out["after_unpause"] = call({"model": m, "input": ["a"]})

    docker("stop", "-t", "5", CONTAINER)
    out["stopped_container"] = call({"model": m, "input": ["a"]}, timeout=2)
    docker("start", CONTAINER)
    out["after_docker_start"] = until_ok(m)

    # Process exits by itself (SIGTERM to PID 1 from inside): the restart policy must bring it back.
    docker("exec", CONTAINER, "sh", "-c", "kill -TERM 1")
    time.sleep(0.3)
    out["after_process_exit_restart_policy"] = until_ok(m)
    out["restart_count"] = docker("inspect", "-f", "{{.RestartCount}}", CONTAINER)
    out["health_endpoint"] = call(None, path="/health")
    out["generated_at"] = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())
    (ROOT / "runs" / "failure.json").write_text(json.dumps(out, indent=1, ensure_ascii=False) + "\n")
    print(json.dumps(out, indent=1, ensure_ascii=False))


if __name__ == "__main__":
    main()

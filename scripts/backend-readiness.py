#!/usr/bin/env python3
"""Alvus Core backend-readiness test.

Talks only to the local Alvus Core API. It never reads provider credentials.
Standard-library only so it runs in Termux and ordinary Linux Python.
"""

import json
import math
import os
import statistics
import sys
import time
import urllib.error
import urllib.request


ROOT = os.environ.get("ALVUS_BASE_URL", "http://127.0.0.1:3000").rstrip("/")
if ROOT.endswith("/v1"):
    ROOT = ROOT[:-3].rstrip("/")
API = ROOT + "/v1"
CLIENT_TOKEN = os.environ.get("ALVUS_API_KEY", "alvus-local")
ADMIN_TOKEN = os.environ.get("ALVUS_ADMIN_TOKEN", "")
MODE = os.environ.get("ALVUS_READINESS_MODE", "quick").strip().lower()
TIMEOUT = float(os.environ.get("ALVUS_READINESS_TIMEOUT", "190"))
MAX_AUTO_MEDIAN = float(os.environ.get("ALVUS_READINESS_MAX_AUTO_MEDIAN", "45"))
MAX_AUTO_P95 = float(os.environ.get("ALVUS_READINESS_MAX_AUTO_P95", "90"))
RUN_ID = str(int(time.time()))


def percentile(values, p):
    if not values:
        return None
    xs = sorted(values)
    if len(xs) == 1:
        return xs[0]
    pos = (len(xs) - 1) * p
    lo = math.floor(pos)
    hi = math.ceil(pos)
    if lo == hi:
        return xs[lo]
    return xs[lo] + (xs[hi] - xs[lo]) * (pos - lo)


def headers(api=False, admin=False, stream=False):
    h = {"User-Agent": "alvus-backend-readiness/1"}
    if api:
        h["Authorization"] = "Bearer " + CLIENT_TOKEN
    if admin and ADMIN_TOKEN:
        h["X-Alvus-Admin-Token"] = ADMIN_TOKEN
    if stream:
        h["Accept"] = "text/event-stream"
    else:
        h["Accept"] = "application/json"
    return h


def request_json(method, url, payload=None, api=False, admin=False, timeout=TIMEOUT):
    data = None
    h = headers(api=api, admin=admin)
    if payload is not None:
        data = json.dumps(payload).encode("utf-8")
        h["Content-Type"] = "application/json"
    req = urllib.request.Request(url, data=data, method=method, headers=h)
    started = time.perf_counter()
    try:
        with urllib.request.urlopen(req, timeout=timeout) as response:
            raw = response.read().decode("utf-8", "replace")
            elapsed = time.perf_counter() - started
            try:
                body = json.loads(raw)
            except Exception:
                body = raw
            return {
                "status": response.status,
                "elapsed_s": round(elapsed, 3),
                "body": body,
            }
    except urllib.error.HTTPError as exc:
        elapsed = time.perf_counter() - started
        raw = exc.read(4096).decode("utf-8", "replace")
        try:
            body = json.loads(raw)
        except Exception:
            body = raw
        return {
            "status": exc.code,
            "elapsed_s": round(elapsed, 3),
            "body": body,
            "error": "HTTPError",
        }
    except Exception as exc:
        elapsed = time.perf_counter() - started
        return {
            "status": None,
            "elapsed_s": round(elapsed, 3),
            "error": type(exc).__name__ + ": " + str(exc),
        }


def chat(model, messages, max_tokens=128, tools=None, tool_choice=None, temperature=0):
    payload = {
        "model": model,
        "messages": messages,
        "max_tokens": max_tokens,
        "temperature": temperature,
    }
    if tools is not None:
        payload["tools"] = tools
    if tool_choice is not None:
        payload["tool_choice"] = tool_choice
    raw = request_json("POST", API + "/chat/completions", payload, api=True)
    result = {
        "model": model,
        "status": raw.get("status"),
        "elapsed_s": raw.get("elapsed_s"),
        "useful": False,
        "empty_200": False,
        "content": "",
        "tool_calls": [],
    }
    if "error" in raw:
        result["error"] = raw["error"]
    body = raw.get("body")
    if isinstance(body, dict):
        choices = body.get("choices") or []
        if choices:
            message = choices[0].get("message") or {}
            content = message.get("content")
            if isinstance(content, str):
                result["content"] = content.strip()
            calls = message.get("tool_calls") or []
            if isinstance(calls, list):
                result["tool_calls"] = calls
            result["useful"] = bool(result["content"] or result["tool_calls"])
            result["message"] = message
        if body.get("error"):
            result["upstream_error"] = body.get("error")
    result["empty_200"] = result["status"] == 200 and not result["useful"]
    return result


def prompt_call(model, prompt, expected=None, max_tokens=128):
    result = chat(model, [{"role": "user", "content": prompt}], max_tokens=max_tokens)
    if expected is not None:
        result["expected"] = expected
        result["expected_ok"] = expected in result.get("content", "")
    return result


def stream_call(model, prompt):
    payload = {
        "model": model,
        "messages": [{"role": "user", "content": prompt}],
        "max_tokens": 128,
        "temperature": 0,
        "stream": True,
    }
    req = urllib.request.Request(
        API + "/chat/completions",
        data=json.dumps(payload).encode("utf-8"),
        method="POST",
        headers={
            **headers(api=True, stream=True),
            "Content-Type": "application/json",
        },
    )
    started = time.perf_counter()
    first_data = None
    first_content = None
    chunks = []
    tool_delta = False
    done = False
    status = None
    error = None
    try:
        with urllib.request.urlopen(req, timeout=TIMEOUT) as response:
            status = response.status
            for raw in response:
                line = raw.decode("utf-8", "replace").strip()
                if not line.startswith("data:"):
                    continue
                now = time.perf_counter()
                if first_data is None:
                    first_data = now - started
                data = line[5:].strip()
                if data == "[DONE]":
                    done = True
                    break
                try:
                    event = json.loads(data)
                except Exception:
                    continue
                choices = event.get("choices") or []
                if not choices:
                    continue
                delta = choices[0].get("delta") or {}
                content = delta.get("content")
                if isinstance(content, str) and content:
                    if first_content is None:
                        first_content = now - started
                    chunks.append(content)
                if delta.get("tool_calls") or delta.get("function_call"):
                    tool_delta = True
    except urllib.error.HTTPError as exc:
        status = exc.code
        error = exc.read(2048).decode("utf-8", "replace")
    except Exception as exc:
        error = type(exc).__name__ + ": " + str(exc)
    elapsed = time.perf_counter() - started
    content = "".join(chunks).strip()
    return {
        "model": model,
        "status": status,
        "elapsed_s": round(elapsed, 3),
        "ttfd_s": round(first_data, 3) if first_data is not None else None,
        "ttft_s": round(first_content, 3) if first_content is not None else None,
        "done": done,
        "content": content[:500],
        "useful": bool(content or tool_delta),
        "tool_delta": tool_delta,
        "error": error,
        "pass": status == 200 and done and bool(content or tool_delta),
    }


def tool_call_test():
    tools = [{
        "type": "function",
        "function": {
            "name": "add",
            "description": "Add two integers.",
            "parameters": {
                "type": "object",
                "properties": {
                    "a": {"type": "integer"},
                    "b": {"type": "integer"},
                },
                "required": ["a", "b"],
                "additionalProperties": False,
            },
        },
    }]
    first = chat(
        "auto",
        [{"role": "user", "content": "Use the add tool to calculate 19 + 23. Do not calculate it yourself."}],
        max_tokens=128,
        tools=tools,
        tool_choice="required",
    )
    out = {
        "first_status": first.get("status"),
        "tool_call_detected": False,
        "arguments_ok": False,
        "second_status": None,
        "final_contains_42": False,
        "pass": False,
    }
    calls = first.get("tool_calls") or []
    if not calls:
        out["first"] = first
        return out

    call = calls[0]
    out["tool_call_detected"] = True
    fn = call.get("function") or {}
    try:
        args = json.loads(fn.get("arguments") or "{}")
    except Exception:
        args = {}
    out["arguments_ok"] = fn.get("name") == "add" and args.get("a") == 19 and args.get("b") == 23
    value = 42 if out["arguments_ok"] else None

    assistant_message = first.get("message") or {
        "role": "assistant",
        "content": None,
        "tool_calls": calls,
    }
    messages = [
        {"role": "user", "content": "Use the add tool to calculate 19 + 23. Do not calculate it yourself."},
        assistant_message,
        {
            "role": "tool",
            "tool_call_id": call.get("id", ""),
            "content": json.dumps({"result": value}),
        },
    ]
    second = chat("auto", messages, max_tokens=128, tools=tools, tool_choice="auto")
    out["second_status"] = second.get("status")
    out["final_contains_42"] = "42" in second.get("content", "")
    out["pass"] = (
        first.get("status") == 200
        and out["tool_call_detected"]
        and out["arguments_ok"]
        and second.get("status") == 200
        and out["final_contains_42"]
    )
    if not out["pass"]:
        out["first"] = first
        out["second"] = second
    return out


def endpoint_checks():
    return {
        "health": request_json("GET", ROOT + "/healthz"),
        "ready": request_json("GET", ROOT + "/readyz"),
        "models": request_json("GET", API + "/models", api=True),
        "metrics": request_json("GET", ROOT + "/metrics", admin=True),
    }


def core_route_tests():
    suffix = RUN_ID
    tests = {
        "auto": prompt_call("auto", "Reply exactly: ALVUS_AUTO_OK_" + suffix, "ALVUS_AUTO_OK_" + suffix, 64),
        "fast": prompt_call("fast", "Reply exactly: ALVUS_FAST_OK_" + suffix, "ALVUS_FAST_OK_" + suffix, 64),
        "coding": prompt_call(
            "coding",
            "Write a concise Python function named is_prime(n) that returns whether n is prime. Return code only.",
            "def is_prime",
            256,
        ),
        "reasoning": prompt_call(
            "reasoning",
            "If 5 machines make 5 pieces in 5 minutes, how many pieces do 100 machines make in 100 minutes? Explain briefly.",
            "2000",
            256,
        ),
    }
    if MODE == "full":
        tests["quality"] = prompt_call(
            "quality",
            "Explain in three concise bullet points why idempotency matters in distributed systems.",
            None,
            384,
        )
    for name, item in tests.items():
        expected = item.get("expected")
        item["pass"] = (
            item.get("status") == 200
            and item.get("useful")
            and not item.get("empty_200")
            and (expected is None or item.get("expected_ok"))
        )
    return tests


def stability_test():
    count = 10 if MODE == "full" else 3
    rows = []
    for i in range(count):
        expected = "ALVUS_STABLE_" + RUN_ID + "_" + str(i + 1)
        row = prompt_call("auto", "Reply exactly: " + expected, expected, 64)
        row["run"] = i + 1
        row["pass"] = (
            row.get("status") == 200
            and row.get("useful")
            and not row.get("empty_200")
            and row.get("expected_ok")
        )
        rows.append(row)
    good_latencies = [r["elapsed_s"] for r in rows if r.get("pass")]
    summary = {
        "attempts": count,
        "passed": sum(bool(r.get("pass")) for r in rows),
        "http_200_empty": sum(bool(r.get("empty_200")) for r in rows),
        "failures": sum(not bool(r.get("pass")) for r in rows),
        "min_s": round(min(good_latencies), 3) if good_latencies else None,
        "median_s": round(statistics.median(good_latencies), 3) if good_latencies else None,
        "p95_s": round(percentile(good_latencies, 0.95), 3) if good_latencies else None,
        "max_s": round(max(good_latencies), 3) if good_latencies else None,
    }
    summary["reliability_pass"] = summary["passed"] == count and summary["http_200_empty"] == 0
    summary["performance_pass"] = (
        summary["median_s"] is not None
        and summary["p95_s"] is not None
        and summary["median_s"] <= MAX_AUTO_MEDIAN
        and summary["p95_s"] <= MAX_AUTO_P95
    )
    summary["pass"] = summary["reliability_pass"] and summary["performance_pass"]
    return {"rows": rows, "summary": summary}


def direct_model_diagnostics():
    if MODE == "full":
        default_models = "nemotron_super,nemotron_ultra,glm53,kimi_k3,nemotron_lightning"
    else:
        default_models = "nemotron_super,nemotron_ultra"
    raw_models = os.environ.get("ALVUS_READINESS_DIRECT_MODELS", default_models)
    aliases = [x.strip() for x in raw_models.split(",") if x.strip()]
    rows = {}
    for alias in aliases:
        expected = "ALVUS_DIRECT_" + alias.upper() + "_" + RUN_ID
        row = prompt_call(alias, "Reply exactly: " + expected, expected, 64)
        row["pass"] = (
            row.get("status") == 200
            and row.get("useful")
            and not row.get("empty_200")
            and row.get("expected_ok")
        )
        rows[alias] = row
    primary = [x for x in ("nemotron_super", "nemotron_ultra") if x in rows]
    primary_pass = bool(primary) and all(rows[x].get("pass") for x in primary)
    return {"models": rows, "primary_pass": primary_pass}


def metric_delta(before, after):
    if not isinstance(before, dict) or not isinstance(after, dict):
        return {}
    keys = ["requests", "upstream_attempts", "fallbacks", "errors", "cache_hits", "cache_misses", "cache_stores"]
    out = {}
    for key in keys:
        a = before.get(key)
        b = after.get(key)
        if isinstance(a, (int, float)) and isinstance(b, (int, float)):
            out[key] = b - a
    out["models_after"] = after.get("models", {})
    out["circuits_after"] = after.get("circuits", {})
    out["response_cache_on"] = after.get("response_cache_on")
    return out


def compact_endpoint(result):
    return {
        "status": result.get("status"),
        "elapsed_s": result.get("elapsed_s"),
        "error": result.get("error"),
    }


def main():
    initial = endpoint_checks()
    initial_simple = {k: compact_endpoint(v) for k, v in initial.items()}
    endpoints_pass = all(initial_simple[x]["status"] == 200 for x in ("health", "ready", "models"))
    metrics_before = initial.get("metrics", {}).get("body") if initial_simple["metrics"]["status"] == 200 else {}

    if not endpoints_pass:
        report = {
            "mode": MODE,
            "base_url": ROOT,
            "endpoints": initial_simple,
            "pass": False,
            "reason": "gateway endpoints are not ready",
        }
        print(json.dumps(report, ensure_ascii=False, indent=2))
        return 1

    routes = core_route_tests()
    stream = stream_call("auto", "Reply exactly: ALVUS_STREAM_OK_" + RUN_ID)
    stream["expected_ok"] = "ALVUS_STREAM_OK_" + RUN_ID in stream.get("content", "")
    stream["pass"] = bool(stream["pass"] and stream["expected_ok"])
    tools = tool_call_test()
    stability = stability_test()
    direct = direct_model_diagnostics()

    final_metrics_raw = request_json("GET", ROOT + "/metrics", admin=True)
    metrics_after = final_metrics_raw.get("body") if final_metrics_raw.get("status") == 200 else {}
    delta = metric_delta(metrics_before, metrics_after)

    route_pass = all(v.get("pass") for k, v in routes.items() if k in ("auto", "fast", "coding", "reasoning"))
    empty_200 = sum(bool(v.get("empty_200")) for v in routes.values())
    empty_200 += stability["summary"]["http_200_empty"]
    empty_200 += sum(bool(v.get("empty_200")) for v in direct["models"].values())

    overall = (
        endpoints_pass
        and route_pass
        and stream.get("pass")
        and tools.get("pass")
        and stability["summary"]["pass"]
        and direct["primary_pass"]
        and empty_200 == 0
    )

    report = {
        "mode": MODE,
        "base_url": ROOT,
        "thresholds": {
            "auto_median_s_max": MAX_AUTO_MEDIAN,
            "auto_p95_s_max": MAX_AUTO_P95,
        },
        "endpoints": initial_simple,
        "routes": routes,
        "streaming": stream,
        "tool_calling": tools,
        "stability": stability,
        "direct_models": direct,
        "metrics": {
            "final_status": final_metrics_raw.get("status"),
            "delta": delta,
        },
        "http_200_empty_total": empty_200,
        "pass": overall,
        "verdict": "backend-ready" if overall else "not-ready",
    }
    print(json.dumps(report, ensure_ascii=False, indent=2))
    return 0 if overall else 1


if __name__ == "__main__":
    sys.exit(main())

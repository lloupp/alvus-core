#!/usr/bin/env python3
import json
import os
import statistics
import sys
import time
import urllib.error
import urllib.request

BASE = os.environ.get("ALVUS_BENCH_BASE_URL", "http://127.0.0.1:3000/v1").rstrip("/")
MODEL = os.environ.get("ALVUS_BENCH_MODEL", "glm53_flash")
RUNS = int(os.environ.get("ALVUS_BENCH_RUNS", "5"))
STREAM_RUNS = int(os.environ.get("ALVUS_BENCH_STREAM_RUNS", "3"))
TIMEOUT = float(os.environ.get("ALVUS_BENCH_TIMEOUT", "240"))
PROMPT = os.environ.get("ALVUS_BENCH_PROMPT", "Responda exatamente: GLM53_FLASH_OK")
EXPECTED = os.environ.get("ALVUS_BENCH_EXPECTED", "GLM53_FLASH_OK")
API_KEY = os.environ.get("ALVUS_BENCH_API_KEY", "alvus-local")

def percentile(values, p):
    if not values:
        return None
    xs = sorted(values)
    if len(xs) == 1:
        return xs[0]
    k = (len(xs) - 1) * p
    lo = int(k)
    hi = min(lo + 1, len(xs) - 1)
    frac = k - lo
    return xs[lo] * (1 - frac) + xs[hi] * frac

def request(payload, stream=False):
    req = urllib.request.Request(
        BASE + "/chat/completions",
        data=json.dumps(payload).encode(),
        method="POST",
        headers={
            "Authorization": "Bearer " + API_KEY,
            "Content-Type": "application/json",
            "Accept": "text/event-stream" if stream else "application/json",
        },
    )
    started = time.perf_counter()
    try:
        with urllib.request.urlopen(req, timeout=TIMEOUT) as r:
            status = r.status
            if not stream:
                body = r.read().decode("utf-8", "replace")
                total = time.perf_counter() - started
                try:
                    data = json.loads(body)
                    choice = (data.get("choices") or [{}])[0]
                    message = choice.get("message") or {}
                    text = message.get("content") or ""
                    tool_calls = message.get("tool_calls") or []
                except Exception:
                    text, tool_calls = "", []
                useful = bool(str(text).strip() or tool_calls)
                return {
                    "status": status,
                    "total_s": round(total, 3),
                    "useful": useful,
                    "exact": str(text).strip() == EXPECTED,
                    "content_preview": str(text).strip()[:120],
                }

            first_data = None
            first_content = None
            chunks = []
            done = False
            for raw in r:
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
                    chunk = json.loads(data)
                    delta = ((chunk.get("choices") or [{}])[0].get("delta") or {})
                    content = delta.get("content") or ""
                    if content:
                        if first_content is None:
                            first_content = now - started
                        chunks.append(content)
                except Exception:
                    pass
            total = time.perf_counter() - started
            text = "".join(chunks).strip()
            return {
                "status": status,
                "ttfd_s": round(first_data, 3) if first_data is not None else None,
                "ttft_s": round(first_content, 3) if first_content is not None else None,
                "total_s": round(total, 3),
                "done": done,
                "useful": bool(text),
                "exact": text == EXPECTED,
                "content_preview": text[:120],
            }
    except urllib.error.HTTPError as e:
        total = time.perf_counter() - started
        return {
            "status": e.code,
            "total_s": round(total, 3),
            "error": e.read(1024).decode("utf-8", "replace")[:200],
        }
    except Exception as e:
        total = time.perf_counter() - started
        return {"status": None, "total_s": round(total, 3), "error": type(e).__name__ + ": " + str(e)}

def summary(rows):
    totals = [r["total_s"] for r in rows if r.get("status") == 200 and r.get("useful")]
    ttfts = [r["ttft_s"] for r in rows if r.get("ttft_s") is not None]
    return {
        "attempts": len(rows),
        "http_200": sum(r.get("status") == 200 for r in rows),
        "useful": sum(bool(r.get("useful")) for r in rows),
        "exact": sum(bool(r.get("exact")) for r in rows),
        "empty_200": sum(r.get("status") == 200 and not r.get("useful") for r in rows),
        "min_s": round(min(totals), 3) if totals else None,
        "median_s": round(statistics.median(totals), 3) if totals else None,
        "p95_s": round(percentile(totals, 0.95), 3) if totals else None,
        "max_s": round(max(totals), 3) if totals else None,
        "median_ttft_s": round(statistics.median(ttfts), 3) if ttfts else None,
    }

def main():
    print(json.dumps({"model": MODEL, "base_url": BASE, "runs": RUNS, "stream_runs": STREAM_RUNS}))
    payload = {
        "model": MODEL,
        "messages": [{"role": "user", "content": PROMPT}],
        "max_tokens": 64,
        "temperature": 0,
    }

    nonstream = []
    for i in range(RUNS):
        row = request(payload, stream=False)
        row["run"] = i + 1
        nonstream.append(row)
        print(json.dumps({"nonstream": row}, ensure_ascii=False))

    stream = []
    stream_payload = dict(payload)
    stream_payload["stream"] = True
    for i in range(STREAM_RUNS):
        row = request(stream_payload, stream=True)
        row["run"] = i + 1
        stream.append(row)
        print(json.dumps({"stream": row}, ensure_ascii=False))

    result = {
        "model": MODEL,
        "nonstream": summary(nonstream),
        "stream": summary(stream),
        "pass": (
            len(nonstream) == RUNS
            and len(stream) == STREAM_RUNS
            and all(r.get("status") == 200 and r.get("useful") for r in nonstream)
            and all(r.get("status") == 200 and r.get("useful") and r.get("done") for r in stream)
        ),
    }
    print(json.dumps({"summary": result}, ensure_ascii=False, indent=2))
    return 0 if result["pass"] else 1

if __name__ == "__main__":
    sys.exit(main())

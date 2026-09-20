#!/usr/bin/env bash
set -euo pipefail

: "${NVIDIA_API_KEYS:?set NVIDIA_API_KEYS with comma-separated NVIDIA keys}"
MODEL="${NVIDIA_E2E_MODEL:-nvidia/nemotron-3-super-120b-a12b}"
BASE="${NVIDIA_BASE_URL:-https://integrate.api.nvidia.com/v1}"

IFS=',' read -r -a KEYS <<< "$NVIDIA_API_KEYS"
if [ "${#KEYS[@]}" -lt 2 ]; then
  echo "need at least two NVIDIA keys" >&2
  exit 2
fi

python3 - "$BASE" "$MODEL" <<'PY'
import json, os, sys, urllib.request, urllib.error, time
base, model = sys.argv[1:3]
keys=[x.strip() for x in os.environ['NVIDIA_API_KEYS'].split(',') if x.strip()]

def post(key,payload,stream=False):
    req=urllib.request.Request(base.rstrip('/')+'/chat/completions', data=json.dumps(payload).encode(), method='POST')
    req.add_header('Authorization','Bearer '+key)
    req.add_header('Content-Type','application/json')
    req.add_header('Accept','text/event-stream' if stream else 'application/json')
    t=time.time()
    try:
        with urllib.request.urlopen(req,timeout=60) as r:
            b=r.read(65536).decode('utf-8','replace')
            return r.status,int((time.time()-t)*1000),b
    except urllib.error.HTTPError as e:
        return e.code,int((time.time()-t)*1000),e.read(4096).decode('utf-8','replace')

for i,key in enumerate(keys[:2],1):
    status,ms,body=post(key,{"model":model,"messages":[{"role":"user","content":"Reply exactly ALVUS_OK"}],"max_tokens":64,"temperature":0})
    ok=False
    try:
        j=json.loads(body)
        ok='ALVUS_OK' in (((j.get('choices') or [{}])[0].get('message') or {}).get('content') or '')
    except Exception:
        pass
    print(json.dumps({"key":i,"nonstream_status":status,"latency_ms":ms,"content_ok":ok}))

    status,ms,body=post(key,{"model":model,"messages":[{"role":"user","content":"Reply exactly STREAM_OK"}],"max_tokens":64,"temperature":0,"stream":True},True)
    print(json.dumps({"key":i,"stream_status":status,"latency_ms":ms,"sse":"data:" in body,"done":"[DONE]" in body,"content_ok":"STREAM_OK" in body}))
PY

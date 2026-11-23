#!/usr/bin/env python3
"""The client of the llm-share demo: chat completions from the readers.

ConfigMap demo-tools mounts this at /demo with the wrapper llm:
  /demo/llm ask [READER...]
                     ask each reader once, one after another, and print its
                     answer and the time to its first token; the first
                     answer of a reader reads the model in
  /demo/llm bench single|simultaneous [--repeat N] [READER...]
                     single: each reader alone, one after another, N times;
                     simultaneous: all readers at once, N rounds (default
                     N: LLM_REPEAT, else 3). Print per reader the median
                     and range of TTFT and TPOT, then the line
                     "bench: {JSON}" with every sample.
READER is a pod of readers.yaml, default reader-0 reader-1 reader-2, at
http://READER.llm-readers:8080 (the headless Service of the readers),
never through a proxy.
The request is the agentic one of an assistant with one tool: POST
/v1/chat/completions, stream, temperature 0, seed 42, at most 128 tokens,
no prompt cache, a system prompt and the tool get_weather; the model
answers with a call of get_weather. TTFT is the time from sending the
request to its first streamed delta (content or tool call). TPOT is the
server's time per predicted token (timings.predicted_per_token_ms) for an
answer with a tool call, whose deltas the tool-call parser holds back and
streams in a few chunks; for a content answer it is the client's (last
delta - first delta) / (completion tokens - 1). Each sample also has the
server's prompt and predict timings.
"""
import http.client
import json
import os
import statistics
import sys
import threading
import time
import urllib.request

READERS = ["reader-0", "reader-1", "reader-2"]
PORT = 8080
SERVICE = "llm-readers"
WAIT = 180             # seconds to wait for a reader to serve
TIMEOUT = 300          # seconds of one request at most
SYSTEM = "You are a helpful assistant in a smart office. Use the tools when they help. Answer briefly."
QUESTION = "What is the weather like in Helsinki right now, and should I take an umbrella?"
TOOL = {"type": "function", "function": {
    "name": "get_weather",
    "description": "Get the current weather in a city.",
    "parameters": {"type": "object", "properties": {
        "city": {"type": "string", "description": "The name of the city"},
        "unit": {"type": "string", "enum": ["celsius", "fahrenheit"]}},
        "required": ["city"]}}}
OPENER = urllib.request.build_opener(urllib.request.ProxyHandler({}))


def url(reader, path):
    return "http://%s.%s:%d%s" % (reader, SERVICE, PORT, path)


def request_body():
    """Return the JSON body of the request of every sample."""
    return {"messages": [{"role": "system", "content": SYSTEM}, {"role": "user", "content": QUESTION}],
            "tools": [TOOL], "stream": True, "stream_options": {"include_usage": True},
            "temperature": 0, "seed": 42, "max_tokens": 128, "cache_prompt": False}


def wait_serving(reader, timeout=WAIT):
    """Return None when the reader answers /health with 200 within TIMEOUT seconds, else the last error."""
    t0, err = time.time(), None
    while time.time() - t0 < timeout:
        try:
            with OPENER.open(url(reader, "/health"), timeout=5) as r:
                if r.status == 200:
                    return None
        except OSError as e:
            err = str(e)
        time.sleep(1)
    return err or "no answer"


def sample(reader):
    """Return one sample of READER: the timings of one streamed chat completion and its answer.

    Raise ValueError if the stream ends without its [DONE] line or without a finish reason, as when a
    reader dies in the middle of an answer.
    """
    req = urllib.request.Request(url(reader, "/v1/chat/completions"), data=json.dumps(request_body()).encode(),
                                 headers={"Content-Type": "application/json"})
    t0 = time.time()
    first = last = None
    done = False
    content, calls, finish, usage, timings = "", {}, None, {}, {}
    with OPENER.open(req, timeout=TIMEOUT) as r:
        for raw in r:
            line = raw.decode(errors="replace").strip()
            if not line.startswith("data:"):
                continue
            data = line[5:].strip()
            if data == "[DONE]":
                done = True
                break
            chunk = json.loads(data)
            usage = chunk.get("usage") or usage
            timings = chunk.get("timings") or timings
            for ch in chunk.get("choices") or []:
                delta = ch.get("delta") or {}
                finish = ch.get("finish_reason") or finish
                if delta.get("content") or delta.get("tool_calls"):
                    last = time.time()
                    first = first or last
                content += delta.get("content") or ""
                for tc in delta.get("tool_calls") or []:
                    call = calls.setdefault(tc.get("index", 0), {"name": "", "arguments": ""})
                    fn = tc.get("function") or {}
                    call["name"] += fn.get("name") or ""
                    call["arguments"] += fn.get("arguments") or ""
    if not done:
        raise ValueError("the stream ended before [DONE]")
    if finish is None:
        raise ValueError("the stream has no finish reason")
    n = usage.get("completion_tokens") or timings.get("predicted_n") or 0
    s = {"reader": reader, "start": t0, "seconds": time.time() - t0,
         "ttftMs": (first - t0) * 1000 if first else None, "completionTokens": n,
         "promptTokens": usage.get("prompt_tokens") or timings.get("prompt_n"), "finish": finish,
         "promptMs": timings.get("prompt_ms"), "predictedMs": timings.get("predicted_ms"),
         "predictedPerTokenMs": timings.get("predicted_per_token_ms"),
         "clientTpotMs": (last - first) * 1000 / (n - 1) if first and n > 1 else None}
    if calls:
        s["answer"] = "; ".join("%s %s" % (c["name"], c["arguments"]) for _, c in sorted(calls.items()))
        s["tpotMs"], s["tpotFrom"] = s["predictedPerTokenMs"], "server"
    else:
        s["answer"] = " ".join(content.split())
        s["tpotMs"], s["tpotFrom"] = s["clientTpotMs"], "client"
    return s


def safe_sample(reader):
    """Return sample(READER), or a sample with the error if the request fails."""
    try:
        return sample(reader)
    except (OSError, ValueError, http.client.HTTPException) as e:
        return {"reader": reader, "start": time.time(), "error": str(e) or type(e).__name__}


def stats(values):
    """Return the median, min and max of the numbers of VALUES that are not None, None if none are."""
    vs = [v for v in values if v is not None]
    if not vs:
        return None
    return {"median": statistics.median(vs), "min": min(vs), "max": max(vs)}


def fmt(st, unit=""):
    """Return STATS as "median (min-max)" in whole units, "-" for None."""
    if not st:
        return "-"
    if st["max"] - st["min"] < 0.5:
        return "%.0f%s" % (st["median"], unit)
    return "%.0f%s (%.0f-%.0f)" % (st["median"], unit, st["min"], st["max"])


def do_ask(readers):
    """Ask each reader of READERS once and print its answer and TTFT; return 0 if all answered."""
    rc = 0
    for r in readers:
        err = wait_serving(r)
        s = safe_sample(r) if err is None else {"reader": r, "error": "not serving: %s" % err}
        if s.get("error"):
            print("%s: %s" % (r, s["error"]))
            rc = 1
            continue
        print("%s: %s (TTFT %.1f s, %d tokens)" % (r, s["answer"][:60], (s["ttftMs"] or 0) / 1000, s["completionTokens"]))
    return rc


def bench(mode, repeat, readers):
    """Return the result of the bench MODE (single or simultaneous) of REPEAT samples per reader of READERS."""
    for r in readers:
        err = wait_serving(r)
        if err is not None:
            sys.exit("%s is not serving: %s" % (r, err))
    t0 = time.time()
    samples = []
    if mode == "single":
        for _ in range(repeat):
            for r in readers:
                samples.append(safe_sample(r))
    else:
        for i in range(repeat):
            got = [None] * len(readers)
            go = threading.Barrier(len(readers))

            def one(j, reader):
                go.wait()
                got[j] = safe_sample(reader)
            ts = [threading.Thread(target=one, args=(j, r)) for j, r in enumerate(readers)]
            for t in ts:
                t.start()
            for t in ts:
                t.join()
            for j, s in enumerate(got):
                if s is None:   # the thread raised something that safe_sample does not catch (its traceback is on stderr)
                    s = got[j] = {"reader": readers[j], "start": t0, "error": "the sample thread failed"}
                s["round"] = i
            samples += got
    per = {}
    for r in readers:
        mine = [s for s in samples if s["reader"] == r and not s.get("error")]
        per[r] = {"ttftMs": stats([s["ttftMs"] for s in mine]), "tpotMs": stats([s["tpotMs"] for s in mine]),
                  "promptMs": stats([s["promptMs"] for s in mine]),
                  "tpotFrom": sorted(set(s["tpotFrom"] for s in mine)), "errors": sum(1 for s in samples if s["reader"] == r and s.get("error"))}
    return {"mode": mode, "repeat": repeat, "readers": readers, "model": os.environ.get("LLM_MODEL_NAME"),
            "modelFile": os.environ.get("LLM_MODEL_FILE"), "request": request_body(), "start": t0,
            "seconds": time.time() - t0, "perReader": per, "samples": samples}


def print_bench(res):
    """Print one line per reader of the bench result RES: TTFT and TPOT, and the tokens per second of the TPOT."""
    print("%s, %d each:" % (res["mode"], res["repeat"]))
    for r in res["readers"]:
        p = res["perReader"][r]
        tpot = p["tpotMs"]
        rate = " %.1f tok/s" % (1000 / tpot["median"]) if tpot and tpot["median"] else ""
        err = " %d failed" % p["errors"] if p["errors"] else ""
        print("%s TTFT %s TPOT %s%s%s" % (r, fmt(p["ttftMs"], " ms"), "%.1f ms" % tpot["median"] if tpot else "-", rate, err))
    print("bench: " + json.dumps(res, sort_keys=True))


def main():
    args = sys.argv[1:]
    op = args[0] if args else ""
    if op == "ask":
        sys.exit(do_ask(args[1:] or READERS))
    if op == "bench" and len(args) >= 2 and args[1] in ("single", "simultaneous"):
        mode, rest = args[1], args[2:]
        repeat = int(os.environ.get("LLM_REPEAT") or 3)
        if rest[:1] == ["--repeat"] and len(rest) >= 2 and rest[1].isdigit() and int(rest[1]) > 0:
            repeat, rest = int(rest[1]), rest[2:]
        if any(a.startswith("-") for a in rest):
            sys.exit(__doc__)
        res = bench(mode, repeat, rest or READERS)
        print_bench(res)
        sys.exit(1 if any(p["errors"] for p in res["perReader"].values()) else 0)
    sys.exit(__doc__)


if __name__ == "__main__":
    main()

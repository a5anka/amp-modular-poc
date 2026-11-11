# Custom-evaluator harness, run by the launcher in a child interpreter
# (python3 -I, empty environment). Protocol on stdin/stdout, one JSON per line:
#   in : {"code": "...", "traces": [...]}
#   out: RPC {"op": "judge", "prompt": "..."}   -> in: {"score": 0.8, "provider": ..., "model": ...}
#   out: RESULT [{"traceId": ..., "score": ..., "reason": ...}, ...]
import json
import sys

_real_stdout = sys.stdout
sys.stdout = sys.stderr  # print() in user code goes to the log, not the protocol


def _rpc(obj):
    _real_stdout.write("RPC " + json.dumps(obj) + "\n")
    _real_stdout.flush()
    return json.loads(sys.stdin.readline())


def judge(prompt):
    """Ask the AI Gateway for a verdict. The launcher makes the call with the run token."""
    return _rpc({"op": "judge", "prompt": str(prompt)})


inp = json.loads(sys.stdin.readline())
ns = {"judge": judge, "__name__": "evaluator"}
exec(compile(inp["code"], "<evaluator>", "exec"), ns)
if "evaluate" not in ns:
    raise SystemExit("evaluator code must define evaluate(trace)")
out = []
for t in inp["traces"]:
    r = ns["evaluate"](t) or {}
    score = float(r.get("score", 0))
    out.append({"traceId": t["id"], "score": max(0.0, min(1.0, score)), "reason": str(r.get("reason", ""))[:300]})
_real_stdout.write("RESULT " + json.dumps(out) + "\n")
_real_stdout.flush()

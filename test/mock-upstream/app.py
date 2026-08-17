#!/usr/bin/env python3
"""
最小 LLM 上游桩，用于验证插件行为——不需要真实厂商 API Key，不花钱。

它做三件事：
  1. 以 OpenAI SSE 格式响应，响应里的 model 字段写【自己的身份】(UPSTREAM_ID)，
     这样从客户端拿到的响应就能直接看出请求最终打到了哪个上游 —— V1 就靠这个判定。
  2. 把收到的每个请求（路径 / 关键请求头 / body 里的 model 字段）记进内存，
     GET /_requests 可以取回，用来核对「body 改了没」「路由头改了没」。
  3. 支持几个开关，模拟异常场景：
       FORCE_STATUS=503   固定返回 503，用来触发 ai-proxy 的 fallback（V3）
       GZIP=1             响应加 content-encoding: gzip（V2）
       NO_USAGE=1         SSE 里不带 usage 字段（验证「宁可漏记不可乱记」）

环境变量：UPSTREAM_ID（必填，比如 A / B）、PORT（默认 8080）
"""
import gzip, json, os, sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

UPSTREAM_ID  = os.environ.get("UPSTREAM_ID", "unknown")
PORT         = int(os.environ.get("PORT", "8080"))
FORCE_STATUS = int(os.environ.get("FORCE_STATUS", "0"))
USE_GZIP     = os.environ.get("GZIP") == "1"
NO_USAGE     = os.environ.get("NO_USAGE") == "1"

RECEIVED = []   # 收到的请求，供 GET /_requests 取回


def build_sse() -> bytes:
    """OpenAI 兼容的 SSE。model 写成自己的身份，最后一帧带 usage。"""
    first = {"id": "chatcmpl-mock", "object": "chat.completion.chunk",
             "model": f"upstream-{UPSTREAM_ID}",
             "choices": [{"index": 0, "delta": {"content": f"hello from {UPSTREAM_ID}"}}]}
    last = {"id": "chatcmpl-mock", "object": "chat.completion.chunk",
            "model": f"upstream-{UPSTREAM_ID}",
            "choices": [{"index": 0, "delta": {}, "finish_reason": "stop"}]}
    if not NO_USAGE:
        last["usage"] = {"prompt_tokens": 1000, "completion_tokens": 500, "total_tokens": 1500}
    return (f"data: {json.dumps(first)}\n\n"
            f"data: {json.dumps(last)}\n\n"
            "data: [DONE]\n\n").encode()


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, fmt, *args):        # 静音默认访问日志，自己打更有用的
        pass

    def do_GET(self):
        if self.path == "/_requests":
            return self._json(200, RECEIVED)
        if self.path == "/_reset":
            RECEIVED.clear()
            return self._json(200, {"ok": True})
        return self._json(404, {"error": "not found"})

    def do_POST(self):
        raw = self.rfile.read(int(self.headers.get("content-length") or 0))
        try:
            body_model = json.loads(raw).get("model")
        except Exception:
            body_model = None

        record = {
            "path": self.path,
            "body_model": body_model,                                   # body 里的 model 改了没
            "route_header": self.headers.get("x-higress-llm-model"),    # 路由头改了没
            "consumer": self.headers.get("x-mse-consumer"),
            "content_type": self.headers.get("content-type"),
            "body_bytes": len(raw),
        }
        RECEIVED.append(record)
        print(f"[upstream-{UPSTREAM_ID}] {json.dumps(record, ensure_ascii=False)}", flush=True)

        if FORCE_STATUS:
            return self._json(FORCE_STATUS, {"error": {"message": f"upstream-{UPSTREAM_ID} forced {FORCE_STATUS}"}})

        payload = build_sse()
        headers = [("content-type", "text/event-stream"), ("cache-control", "no-cache")]
        if USE_GZIP:
            payload = gzip.compress(payload)
            headers.append(("content-encoding", "gzip"))
        self.send_response(200)
        for k, v in headers:
            self.send_header(k, v)
        self.send_header("content-length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def _json(self, code, obj):
        data = json.dumps(obj, ensure_ascii=False).encode()
        self.send_response(code)
        self.send_header("content-type", "application/json")
        self.send_header("content-length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)


if __name__ == "__main__":
    print(f"mock upstream '{UPSTREAM_ID}' on :{PORT} "
          f"(force_status={FORCE_STATUS} gzip={USE_GZIP} no_usage={NO_USAGE})", flush=True)
    ThreadingHTTPServer(("0.0.0.0", PORT), Handler).serve_forever()

"""A minimal OpenAI-compatible upstream for the portability test.

It exists so the container test never reaches a real provider: no API key, no
cost, no network dependency beyond the compose network. It answers the three
things Bifrost touches — a bare GET / for the health poller, and the
chat/completions endpoint with a usage block so spend is actually booked.
"""

import json
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PORT = 8080


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def _send(self, status, payload):
        body = json.dumps(payload).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        if self.path.startswith("/v1/models"):
            self._send(200, {"object": "list", "data": [
                {"id": "stub-model", "object": "model", "owned_by": "stub"}]})
            return
        self._send(200, {"status": "ok"})

    def do_POST(self):
        length = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(length) if length else b"{}"
        try:
            req = json.loads(raw or b"{}")
        except json.JSONDecodeError:
            self._send(400, {"error": "bad json"})
            return

        if not self.path.startswith("/v1/chat/completions"):
            self._send(404, {"error": "not found"})
            return

        model = req.get("model", "stub-model")
        self._send(200, {
            "id": "stub-1",
            "object": "chat.completion",
            "created": 0,
            "model": model,
            "choices": [{
                "index": 0,
                "message": {"role": "assistant", "content": "stub ok"},
                "finish_reason": "stop",
            }],
            # Deliberate token counts so the ledger books a non-zero cost.
            "usage": {"prompt_tokens": 7, "completion_tokens": 3, "total_tokens": 10},
        })

    def log_message(self, format, *args):
        pass


if __name__ == "__main__":
    ThreadingHTTPServer(("0.0.0.0", PORT), Handler).serve_forever()

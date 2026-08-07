#!/usr/bin/env python3
"""Mock Anthropic upstream: records received bytes and returns a canned reply.

It supports both native JSON and SSE responses. Octopus uses the upstream's
native non-streaming transport for buffered client requests when available,
and SSE for streaming client requests.
"""
import json
import sys
import os
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PORT = int(sys.argv[1]) if len(sys.argv) > 1 else 9099
CAPTURE_DIR = sys.argv[2] if len(sys.argv) > 2 else "/tmp/mockcap"
os.makedirs(CAPTURE_DIR, exist_ok=True)
COUNTER = {"n": 0}


def sse_stream():
    msg_start = {
        "type": "message_start",
        "message": {
            "id": "msg_mock_0001", "type": "message", "role": "assistant",
            "model": "mock-test-model", "content": [], "stop_reason": None,
            "usage": {"input_tokens": 10, "output_tokens": 1,
                      "cache_creation_input_tokens": 0,
                      "cache_read_input_tokens": 0},
        },
    }
    events = [
        ("message_start", msg_start),
        ("content_block_start", {"type": "content_block_start", "index": 0,
                                 "content_block": {"type": "text", "text": ""}}),
        ("content_block_delta", {"type": "content_block_delta", "index": 0,
                                 "delta": {"type": "text_delta", "text": "ok"}}),
        ("content_block_stop", {"type": "content_block_stop", "index": 0}),
        ("message_delta", {"type": "message_delta",
                           "delta": {"stop_reason": "end_turn", "stop_sequence": None},
                           "usage": {"output_tokens": 2}}),
        ("message_stop", {"type": "message_stop"}),
    ]
    out = []
    for name, payload in events:
        out.append(f"event: {name}\ndata: {json.dumps(payload)}\n\n")
    return "".join(out).encode()


def message_response():
    return json.dumps({
        "id": "msg_mock_0001", "type": "message", "role": "assistant",
        "model": "mock-test-model",
        "content": [{"type": "text", "text": "ok"}],
        "stop_reason": "end_turn", "stop_sequence": None,
        "usage": {"input_tokens": 10, "output_tokens": 2,
                  "cache_creation_input_tokens": 0,
                  "cache_read_input_tokens": 0},
    }).encode()


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *a):
        pass

    def do_POST(self):
        n = COUNTER["n"] = COUNTER["n"] + 1
        length = int(self.headers.get("content-length", 0))
        body = self.rfile.read(length)
        with open(os.path.join(CAPTURE_DIR, f"req-{n:04d}.bin"), "wb") as f:
            f.write(body)
        with open(os.path.join(CAPTURE_DIR, f"req-{n:04d}.headers.json"), "w") as f:
            json.dump({"path": self.path, "headers": dict(self.headers)}, f, indent=2)

        try:
            request = json.loads(body)
        except Exception:
            request = {}
        streaming = bool(request.get("stream"))
        payload = sse_stream() if streaming else message_response()
        self.send_response(200)
        self.send_header("content-type", "text/event-stream" if streaming else "application/json")
        if streaming:
            self.send_header("cache-control", "no-cache")
        self.send_header("content-length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)


if __name__ == "__main__":
    ThreadingHTTPServer(("127.0.0.1", PORT), Handler).serve_forever()

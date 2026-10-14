#!/usr/bin/env python3
"""Exercise parameterized step reuse through the released CLI surface."""

import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from threading import Thread


EMAIL = "caller@example.test"
TOKEN = "session-for-caller"


class Handler(BaseHTTPRequestHandler):
    requests = []

    def do_POST(self):
        body = self.rfile.read(int(self.headers.get("Content-Length", "0")))
        self.requests.append((self.path, body, self.headers.get("Authorization")))
        if self.path == "/login" and json.loads(body) == {"email": EMAIL}:
            status, payload = 200, {"token": TOKEN}
        elif self.path == "/orders" and self.headers.get("Authorization") == f"Bearer {TOKEN}":
            status, payload = 201, {"order_id": "e2e-order"}
        else:
            status, payload = 400, {"error": "unexpected request"}
        encoded = json.dumps(payload).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(encoded)))
        self.end_headers()
        self.wfile.write(encoded)

    def log_message(self, _format, *_args):
        pass


def main():
    if len(sys.argv) != 2:
        raise SystemExit("usage: run.py PATH_TO_LAMPLIGHT")
    cli = Path(sys.argv[1]).resolve()
    fixture = Path(__file__).resolve().parent
    server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    thread = Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        with tempfile.TemporaryDirectory() as directory:
            result_path = Path(directory) / "result.json"
            env = os.environ.copy()
            env["LAMPLIGHT_VAR_BASE_URL"] = f"http://127.0.0.1:{server.server_port}"
            env["LAMPLIGHT_VAR_CALLER_EMAIL"] = EMAIL
            env.pop("LAMPLIGHT_VAR_SOURCE_EMAIL", None)
            run = subprocess.run(
                [str(cli), "run", "--working-dir", str(fixture), "--json-file", str(result_path), "checkout"],
                env=env,
                text=True,
                capture_output=True,
                check=False,
            )
            if run.returncode != 0:
                raise AssertionError(f"Lamplight exited {run.returncode}:\n{run.stdout}\n{run.stderr}")
            result = json.loads(result_path.read_text())
        assert result["status"] == "passed", result
        assert [test["name"] for test in result["tests"]] == ["checkout"], result
        steps = result["tests"][0]["steps"]
        assert [step["name"] for step in steps] == ["authenticate", "order"], steps
        assert all(step["status"] == "passed" for step in steps), steps
        assert "token" in steps[0]["outputs"], steps[0]
        assert [request[0] for request in Handler.requests] == ["/login", "/orders"], Handler.requests
        assert json.loads(Handler.requests[0][1]) == {"email": EMAIL}, Handler.requests
        assert Handler.requests[1][2] == f"Bearer {TOKEN}", Handler.requests
        print("Reusable step E2E passed: ref, parameter override, output, and selected test")
    finally:
        server.shutdown()
        server.server_close()
        thread.join()


if __name__ == "__main__":
    main()

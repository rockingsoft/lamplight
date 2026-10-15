#!/usr/bin/env python3
"""Exercise a Bash request hook through the released CLI surface."""

import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from threading import Thread


class Handler(BaseHTTPRequestHandler):
    requests = []

    def do_POST(self):
        body = self.rfile.read(int(self.headers.get("Content-Length", "0")))
        self.requests.append((self.path, body, self.headers.get("X-Hook")))
        self.send_response(204)
        self.send_header("Content-Type", "text/plain")
        self.end_headers()

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
            env["LAMPLIGHT_VAR_HOOK_VALUE"] = "from-hook"
            run = subprocess.run(
                [str(cli), "run", "--working-dir", str(fixture), "--json-file", str(result_path), "request hook"],
                env=env,
                text=True,
                capture_output=True,
                check=False,
            )
            if run.returncode != 0:
                details = result_path.read_text() if result_path.exists() else "no result file"
                raise AssertionError(f"Lamplight exited {run.returncode}:\n{run.stdout}\n{run.stderr}\n{details}")
            result = json.loads(result_path.read_text())
        assert result["status"] == "passed", result
        assert Handler.requests == [("/hook", b"modified", "from-hook")], Handler.requests
        print("Request hook E2E passed: Bash read and rewrote request JSON")
    finally:
        server.shutdown()
        server.server_close()
        thread.join()


if __name__ == "__main__":
    main()

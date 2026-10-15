#!/usr/bin/env python3
"""Exercise multiplexed requests through a real Lamplight executor process."""

import json
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
import queue
import subprocess
import sys
from threading import Event, Thread


first_started = Event()
release_first = Event()


class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path == "/first":
            first_started.set()
            if not release_first.wait(timeout=10):
                self.send_error(504)
                return
        elif self.path == "/second":
            if not first_started.wait(timeout=10):
                self.send_error(504)
                return
        self.send_response(200)
        self.send_header("Content-Type", "text/plain")
        self.end_headers()
        self.wfile.write(self.path.encode())

    def log_message(self, _format, *_args):
        pass


def main():
    cli = Path(sys.argv[1]).resolve()
    server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    server_thread = Thread(target=server.serve_forever, daemon=True)
    server_thread.start()
    process = subprocess.Popen(
        [str(cli), "executor"],
        stdin=subprocess.PIPE,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
        bufsize=1,
    )
    responses = queue.Queue()

    def read_responses():
        for line in process.stdout:
            responses.put(json.loads(line))

    reader = Thread(target=read_responses, daemon=True)
    reader.start()
    try:
        for request_id, path in ((1, "/first"), (2, "/second")):
            request = {
                "version": 2,
                "id": request_id,
                "method": "http.execute",
                "http": {"method": "GET", "url": f"http://127.0.0.1:{server.server_port}{path}"},
                "http_client": {},
            }
            process.stdin.write(json.dumps(request) + "\n")
            process.stdin.flush()
        first = responses.get(timeout=15)
        assert first["id"] == 2, first
        release_first.set()
        second = responses.get(timeout=15)
        assert [first["id"], second["id"]] == [2, 1], (first, second)
        assert first.get("result", {}).get("body") == "/second", first
        assert second.get("result", {}).get("body") == "/first", second
        process.stdin.close()
        assert process.wait(timeout=5) == 0, process.stderr.read()
        print("Remote executor E2E passed: concurrent responses completed out of order")
    finally:
        release_first.set()
        if process.poll() is None:
            process.kill()
            process.wait()
        server.shutdown()
        server.server_close()
        server_thread.join()


if __name__ == "__main__":
    main()

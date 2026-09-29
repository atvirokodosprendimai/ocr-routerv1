#!/usr/bin/env python3
"""Tests for client.py, against a stub router that speaks the real wire format.

    python3 test_client.py

Standard library only, like the client. No pytest, so this runs anywhere the
client does.

⚠ THE STUB IS HAND-WRITTEN FROM THE ROUTER'S SOURCE, not captured from it, so
these tests prove the CLIENT is self-consistent with the protocol as documented
in ../README.md. They cannot prove the document matches the router — only
running against a real router does that, and `TestProtocolMatchesTheGoClient`
below is the cheap approximation: it pins the wire details that the Go client
(../../client, which IS tested against the router) also depends on.
"""

from __future__ import annotations

import json
import threading
import time
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import client as ocrr


class StubRouter(BaseHTTPRequestHandler):
    """A router that implements just enough of the protocol to drive a client."""

    # Set per-test on the server object.
    protocol_version = "HTTP/1.1"

    def log_message(self, *args):  # noqa: D102 - silence the default stderr spam
        pass

    # -- helpers ---------------------------------------------------------

    @property
    def script(self):
        return self.server.script

    def record(self, name: str) -> None:
        self.server.seen.append(name)

    def send_event(self, event: str, payload: dict) -> None:
        self.wfile.write(f"event: {event}\ndata: {json.dumps(payload)}\n\n".encode())
        self.wfile.flush()

    def send_json(self, status: int, payload: dict) -> None:
        body = json.dumps(payload).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    # -- routes ----------------------------------------------------------

    def do_GET(self):  # noqa: N802 - BaseHTTPRequestHandler's naming
        if self.path == "/sse":
            self.record("GET /sse")
            self.handle_sse()
        elif self.path.startswith("/files/"):
            self.record("GET /files")
            self.handle_files(self.path[len("/files/") :])
        else:
            self.send_error(404)

    def do_POST(self):  # noqa: N802
        if self.path.startswith("/upload"):
            self.record("POST /upload")
            self.handle_upload()
        else:
            self.send_error(404)

    def handle_sse(self):
        if self.headers.get("Authorization") != f"Bearer {self.server.token}":
            self.send_json(401, {"error": "unauthorized"})
            return

        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Cache-Control", "no-cache")
        # No Content-Length and no chunking: the client reads until close, which
        # is what a real SSE stream looks like to urllib.
        self.end_headers()

        self.send_event("hello", {"role": "client", "user_id": "u1"})
        self.send_event("backlog", {"jobs": self.server.backlog})

        # Hold the stream open and play whatever the test scheduled.
        deadline = time.monotonic() + 5
        while time.monotonic() < deadline:
            if self.server.pending:
                event, payload = self.server.pending.pop(0)
                self.send_event(event, payload)
                if event in ("ready", "failed"):
                    # The client returns after acting on its own event; stay open
                    # briefly so the close is not what it reacts to.
                    time.sleep(0.05)
                    continue
            time.sleep(0.01)

    def handle_upload(self):
        length = int(self.headers.get("Content-Length") or 0)
        self.server.upload_body = self.rfile.read(length) if length else b""
        self.server.upload_path = self.path
        self.server.upload_content_type = self.headers.get("Content-Type")

        if self.server.upload_status != 201:
            self.send_json(self.server.upload_status, {"error": "refused"})
            return

        self.send_json(201, {"job_id": self.server.job_id})

        # Schedule the completion event the test asked for, now that the job has
        # an id the client is waiting on.
        if self.server.on_upload:
            event, payload = self.server.on_upload
            payload = dict(payload)
            payload.setdefault("job_id", self.server.job_id)
            self.server.pending.append((event, payload))

    def handle_files(self, job_id: str):
        if self.server.raw_result is not None:
            body = self.server.raw_result
            self.send_response(200)
            self.send_header("Content-Type", "application/octet-stream")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
            return
        self.send_json(200, {"job_id": job_id, "units": self.server.units})


def start_router(**overrides):
    server = ThreadingHTTPServer(("127.0.0.1", 0), StubRouter)
    server.token = "ocr_c_test"
    server.job_id = "job-1"
    server.units = ["page one", "page two"]
    server.raw_result = None
    server.backlog = []
    server.pending = []
    server.on_upload = ("ready", {"units": 2})
    server.upload_status = 201
    server.upload_body = b""
    server.upload_path = ""
    server.upload_content_type = None
    server.seen = []
    server.script = None
    for key, value in overrides.items():
        setattr(server, key, value)

    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    return server, f"http://127.0.0.1:{server.server_address[1]}"


class ClientTest(unittest.TestCase):
    def tearDown(self):
        if getattr(self, "server", None):
            self.server.shutdown()

    def config(self, base):
        return ocrr.Config(router_url=base, token="ocr_c_test", timeout=10)

    # -- the ordering, which is the whole correctness argument ------------

    def test_stream_opens_before_upload(self):
        """⚠ THE ONE THAT CANNOT BE RECOVERED FROM IF IT REGRESSES.

        Upload first and a job that finishes in the gap fires `ready` into a
        stream nobody holds; the client then waits forever for an event that has
        already happened. It is a small window, which is worse than a large one:
        it passes every casual test and strands the caller in production on
        exactly the jobs that went well.
        """
        self.server, base = start_router()
        ocrr.submit(self.config(base), ocrr.Input(body=b"x", filename="a.txt", label="ocr"))

        self.assertEqual(self.server.seen[0], "GET /sse")
        self.assertEqual(self.server.seen[1], "POST /upload")

    def test_returns_units(self):
        self.server, base = start_router()
        result = ocrr.submit(self.config(base), ocrr.Input(body=b"x", label="ocr"))

        self.assertEqual(result.units, ["page one", "page two"])
        self.assertEqual(result.job_id, "job-1")
        self.assertIsNone(result.raw)

    # -- the request shape -----------------------------------------------

    def test_file_is_sent_as_multipart_under_the_part_name_file(self):
        self.server, base = start_router()
        ocrr.submit(self.config(base), ocrr.Input(body=b"hello bytes", filename="scan.png", label="ocr"))

        self.assertIn("multipart/form-data", self.server.upload_content_type)
        self.assertIn(b'name="file"', self.server.upload_body)
        self.assertIn(b'filename="scan.png"', self.server.upload_body)
        self.assertIn(b"hello bytes", self.server.upload_body)

    def test_params_only_job_sends_no_body(self):
        """The crawler shape: the job IS its parameters and the service fetches
        its own input. A client that insists on a file cannot express it."""
        self.server, base = start_router()
        ocrr.submit(
            self.config(base),
            ocrr.Input(label="crawl", params={"url": "https://example.com"}),
        )

        self.assertEqual(self.server.upload_body, b"")
        self.assertIsNone(self.server.upload_content_type)
        self.assertIn("url=https%3A%2F%2Fexample.com", self.server.upload_path)

    def test_pipeline_wins_over_label(self):
        self.server, base = start_router()
        ocrr.submit(
            self.config(base),
            ocrr.Input(body=b"x", label="ocr", pipeline=("ocr", "translate")),
        )

        self.assertIn("pipeline=ocr%2Ctranslate", self.server.upload_path)
        self.assertNotIn("label=", self.server.upload_path)

    def test_raw_is_always_stated(self):
        """Sent in both modes rather than omitted for units, so a mismatch is a
        disagreement between two stated positions rather than between a
        statement and a default."""
        self.server, base = start_router()
        ocrr.submit(self.config(base), ocrr.Input(body=b"x", label="ocr"))
        self.assertIn("raw=0", self.server.upload_path)

        self.server.shutdown()
        self.server, base = start_router(raw_result=b"\x89PNG\r\n\x1a\n")
        ocrr.submit(self.config(base), ocrr.Input(body=b"x", label="img", raw=True))
        self.assertIn("raw=1", self.server.upload_path)

    # -- the result's form -----------------------------------------------

    def test_octet_stream_becomes_raw_bytes(self):
        self.server, base = start_router(raw_result=b"\x89PNG\r\n\x1a\n\x00\xff")
        result = ocrr.submit(self.config(base), ocrr.Input(body=b"x", label="img", raw=True))

        self.assertEqual(result.raw, b"\x89PNG\r\n\x1a\n\x00\xff")
        self.assertEqual(result.units, [])

    def test_branches_on_the_response_not_the_request(self):
        """⚠ Asked for raw, answered with units: read them as units.

        The two disagree exactly when something is wrong, and that is the case
        worth surfacing rather than misreading — decoding a JSON envelope as
        bytes hands the caller a file full of `{"job_id": ...}` and calls it the
        result.
        """
        self.server, base = start_router(raw_result=None, units=["actually text"])
        result = ocrr.submit(self.config(base), ocrr.Input(body=b"x", label="img", raw=True))

        self.assertEqual(result.units, ["actually text"])
        self.assertIsNone(result.raw)

    # -- failure and isolation -------------------------------------------

    def test_failed_event_raises_a_typed_error(self):
        self.server, base = start_router(on_upload=("failed", {"reason": "worker died"}))

        with self.assertRaises(ocrr.FailedError) as caught:
            ocrr.submit(self.config(base), ocrr.Input(body=b"x", label="ocr"))

        self.assertEqual(caught.exception.reason, "worker died")
        self.assertEqual(caught.exception.job_id, "job-1")
        # And it must NOT have collected: a failed job has no result, and asking
        # for one would charge credits for nothing.
        self.assertNotIn("GET /files", self.server.seen)

    def test_ignores_events_for_other_jobs(self):
        """One stream carries every job the customer has in flight. Acting on the
        wrong one collects somebody else's result, charged to them."""
        self.server, base = start_router(on_upload=None)
        self.server.pending = [
            ("ready", {"job_id": "someone-else", "units": 9}),
            ("failed", {"job_id": "another-one", "reason": "not ours"}),
            ("ready", {"job_id": "job-1", "units": 2}),
        ]

        result = ocrr.submit(self.config(base), ocrr.Input(body=b"x", label="ocr"))
        self.assertEqual(result.units, ["page one", "page two"])

    def test_backlog_on_connect_is_honoured(self):
        """The reconnection path: no live event is ever sent, and the client must
        still finish. This is what makes a dropped stream survivable."""
        self.server, base = start_router(backlog=[{"job_id": "job-1", "units": 2}], on_upload=None)

        result = ocrr.submit(self.config(base), ocrr.Input(body=b"x", label="ocr"))
        self.assertEqual(result.units, ["page one", "page two"])

    def test_ping_does_not_disturb_the_wait(self):
        self.server, base = start_router(on_upload=None)
        self.server.pending = [
            ("ping", {"t": 1}),
            ("ping", {"t": 2}),
            ("ready", {"job_id": "job-1", "units": 2}),
        ]

        result = ocrr.submit(self.config(base), ocrr.Input(body=b"x", label="ocr"))
        self.assertEqual(result.units, ["page one", "page two"])

    # -- the retry split --------------------------------------------------

    def test_rate_limited_is_retryable_and_no_credits_is_not(self):
        """⚠ The split is about what the caller should DO NEXT. Retrying against
        an empty balance never succeeds; waiting out a rate limit always does."""
        self.server, base = start_router(upload_status=429)
        with self.assertRaises(ocrr.RetryableError):
            ocrr.submit(self.config(base), ocrr.Input(body=b"x", label="ocr"))
        self.server.shutdown()

        for status in (401, 402, 400, 409):
            self.server, base = start_router(upload_status=status)
            with self.assertRaises(ocrr.ClientError) as caught:
                ocrr.submit(self.config(base), ocrr.Input(body=b"x", label="ocr"))
            self.assertNotIsInstance(
                caught.exception,
                ocrr.RetryableError,
                f"HTTP {status} must be fatal — retrying it is a way of not noticing",
            )
            self.server.shutdown()
        self.server = None

    def test_server_error_is_retryable(self):
        self.server, base = start_router(upload_status=503)
        with self.assertRaises(ocrr.RetryableError):
            ocrr.submit(self.config(base), ocrr.Input(body=b"x", label="ocr"))

    def test_no_upload_when_the_stream_cannot_open(self):
        """If the stream is refused there is nothing to wait on, so uploading
        would create a job whose result nobody is listening for — and it would
        still be charged."""
        self.server, base = start_router()
        bad = ocrr.Config(router_url=base, token="wrong-token", timeout=10)

        with self.assertRaises(ocrr.ClientError):
            ocrr.submit(bad, ocrr.Input(body=b"x", label="ocr"))

        self.assertNotIn("POST /upload", self.server.seen)

    # -- progress ---------------------------------------------------------

    def test_progress_callback_sequence(self):
        self.server, base = start_router()
        stages = []
        ocrr.submit(
            self.config(base),
            ocrr.Input(body=b"x", label="ocr"),
            lambda stage, detail: stages.append(stage),
        )

        self.assertEqual(
            stages,
            [ocrr.Stage.UPLOADING, ocrr.Stage.WAITING, ocrr.Stage.COLLECTING, ocrr.Stage.DONE],
        )


class FrameReassemblyTest(unittest.TestCase):
    """⚠ The parsing bug every hand-rolled SSE client has.

    A frame is `event:`, `data:`, BLANK LINE. Nothing guarantees how those land
    in reads. A reader that treats each read as a frame drops events under
    exactly the load that makes them matter.
    """

    def test_two_frames_in_one_chunk(self):
        chunk = [b"event: ready\n", b'data: {"job_id":"a"}\n', b"\n", b"event: ready\n", b'data: {"job_id":"b"}\n', b"\n"]
        frames = list(ocrr._read_frames(iter(chunk)))

        self.assertEqual([f[0] for f in frames], ["ready", "ready"])
        self.assertEqual([f[1]["job_id"] for f in frames], ["a", "b"])

    def test_comment_lines_are_ignored(self):
        chunk = [b": keepalive\n", b"event: ready\n", b'data: {"job_id":"a"}\n', b"\n"]
        frames = list(ocrr._read_frames(iter(chunk)))

        self.assertEqual(len(frames), 1)
        self.assertEqual(frames[0][0], "ready")

    def test_frame_without_a_terminating_blank_line_is_not_delivered(self):
        """A truncated frame is incomplete, and delivering it would act on a
        payload the server had not finished sending."""
        chunk = [b"event: ready\n", b'data: {"job_id":"a"}\n']
        self.assertEqual(list(ocrr._read_frames(iter(chunk))), [])

    def test_crlf_line_endings(self):
        chunk = [b"event: ready\r\n", b'data: {"job_id":"a"}\r\n', b"\r\n"]
        frames = list(ocrr._read_frames(iter(chunk)))

        self.assertEqual(len(frames), 1)
        self.assertEqual(frames[0][1]["job_id"], "a")


class ProtocolMatchesTheGoClientTest(unittest.TestCase):
    """Pin the wire details the Go client (../../client) also depends on.

    ⚠ THIS IS AN APPROXIMATION AND SAYS SO. The Go package is tested against the
    real router; this file's stub is hand-written. What this class checks is that
    the two clients agree on the strings that appear on the wire — which is where
    a Python reimplementation silently diverges.
    """

    def test_event_names(self):
        for name in ("hello", "backlog", "ready", "failed", "ping"):
            self.assertIn(name, _client_source(), f"{name} is a protocol event and must be handled")

    def test_endpoints(self):
        for path in ("/sse", "/upload", "/files/"):
            self.assertIn(path, _client_source())

    def test_auth_header_shape(self):
        self.assertIn("Bearer ", _client_source())


def _client_source() -> str:
    import inspect

    return inspect.getsource(ocrr)


if __name__ == "__main__":
    unittest.main(verbosity=2)

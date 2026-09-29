#!/usr/bin/env python3
"""A complete Python client for the ocr-router job protocol.

Standard library only — no requests, no sseclient, nothing to install. Copy this
file into your project and call :func:`submit`.

    from client import submit, Config, Input, FailedError, RetryableError

    with open("invoice.pdf", "rb") as f:
        result = submit(
            Config(router_url="https://ocr.example.com", token=TOKEN),
            Input(filename="invoice.pdf", body=f, label="ocr"),
        )
    print(result.units)

The protocol is documented in full in ../README.md, including why the steps
happen in the order they do. Three of those reasons are load-bearing and are
repeated here at the code they explain, because this file will be copied and
edited by people who never read the document:

1.  THE STREAM OPENS BEFORE THE UPLOAD. Uploading first leaves a window in which
    a fast job finishes and fires `ready` into a stream nobody is holding; the
    client then waits forever for an event that already happened.
2.  EVERY FRAME IS MATCHED ON job_id. One stream carries every job for the
    customer, so acting on the wrong event collects somebody else's result.
3.  THE RESULT'S FORM IS DECIDED BY THE RESPONSE, not by what was requested.

This mirrors the reference Go implementation at ../../client. Where the two
disagree, the Go package is authoritative: it is the one under test.
"""

from __future__ import annotations

import json
import mimetypes
import os
import secrets
import ssl
import urllib.error
import urllib.parse
import urllib.request
from dataclasses import dataclass, field
from typing import BinaryIO, Callable, Iterator, Mapping, Sequence

__all__ = [
    "Config",
    "Input",
    "Result",
    "Stage",
    "ClientError",
    "FailedError",
    "RetryableError",
    "submit",
]


# --------------------------------------------------------------------------
# Types
# --------------------------------------------------------------------------


class Stage:
    """What the client is doing, passed to the progress callback."""

    UPLOADING = "uploading"
    WAITING = "waiting"
    COLLECTING = "collecting"
    DONE = "done"


@dataclass
class Config:
    """How to reach the router."""

    router_url: str
    token: str
    #: TLS context. ``None`` uses Python's default verification, which is what
    #: you want. Supply your own only for a private CA.
    ssl_context: ssl.SSLContext | None = None
    #: Seconds to wait for the whole job. ``None`` waits forever, which is the
    #: right default: a queued job behind a busy worker pool is supposed to take
    #: a while, and the router has its own deadline.
    #:
    #: ⚠ This bounds each socket operation, not the job. The event stream is
    #: held open for the life of the job, so a short value aborts exactly the
    #: long jobs you wanted to wait for. The router sends a `ping` every 15s,
    #: which is what keeps a generous timeout from firing on a healthy stream.
    timeout: float | None = None


@dataclass
class Input:
    """The job to submit."""

    #: The document. ``None`` is the crawler shape: a params-only job whose
    #: service fetches its own input. This is supported deliberately — not every
    #: service takes a file.
    body: BinaryIO | bytes | None = None
    filename: str = "input"
    #: One service. Ignored when ``pipeline`` is set.
    label: str = ""
    #: An ordered chain of services, each stage's output feeding the next.
    #: Takes precedence over ``label``, matching the router's own rule.
    pipeline: Sequence[str] = field(default_factory=tuple)
    #: Become flags on the worker's subprocess.
    params: Mapping[str, str] = field(default_factory=dict)
    #: Ask for a raw service, whose output is opaque bytes rather than a unit
    #: list. Must agree with the service's admin-owned mode or the router
    #: refuses the upload with 409 — a client cannot choose a mode, because the
    #: mode is a price.
    raw: bool = False


@dataclass
class Result:
    """A finished job's output."""

    job_id: str
    #: Text units, for a normal service. Empty for a raw one.
    units: list[str] = field(default_factory=list)
    #: Opaque bytes, for a raw service. ``None`` for a normal one.
    raw: bytes | None = None


Progress = Callable[[str, str], None]


# --------------------------------------------------------------------------
# Errors
# --------------------------------------------------------------------------


class ClientError(Exception):
    """Base for every error this module raises."""


class FailedError(ClientError):
    """The job ran and did not produce a result.

    ⚠ A DISTINCT TYPE ON PURPOSE. A caller that collapses this into "no result"
    writes an empty file and reports success, which turns a dead job into silent
    data loss in whatever pipeline called it. Catch it separately and exit
    non-zero.
    """

    def __init__(self, job_id: str, reason: str = "") -> None:
        self.job_id = job_id
        self.reason = reason
        super().__init__(f"job {job_id} failed: {reason}" if reason else f"job {job_id} failed")


class RetryableError(ClientError):
    """A failure worth trying again: rate limiting, a router that is down, a
    stream that died mid-job.

    Distinct from a fatal error because the caller's next action genuinely
    differs — waiting helps here and never helps for a bad token or an empty
    balance.
    """

    def __init__(self, message: str, retry_after: float | None = None) -> None:
        #: Seconds the router asked you to wait, from its ``Retry-After``
        #: header, when it sent one.
        self.retry_after = retry_after
        super().__init__(message)


# --------------------------------------------------------------------------
# SSE frame reassembly
# --------------------------------------------------------------------------


def _read_frames(stream) -> Iterator[tuple[str, dict]]:
    """Yield ``(event_name, payload)`` for each complete SSE frame.

    ⚠ THIS REASSEMBLES; IT DOES NOT READ A LINE AND CALL IT AN EVENT. An SSE
    frame is ``event:``, then ``data:``, then a BLANK LINE, and the transport
    guarantees nothing about how those land in reads — two frames can arrive in
    one TCP segment and one frame can span two. A reader that treats each read
    as a frame drops events under exactly the load that makes them matter.

    Iterating the response yields whole lines, which handles the splitting; the
    blank line is what this must still honour.
    """
    event = ""
    data = ""

    for raw_line in stream:
        line = raw_line.decode("utf-8", errors="replace").rstrip("\n").rstrip("\r")

        if line == "":
            # A blank line terminates the frame. An empty name means we saw only
            # a comment or a stray data line, and there is nothing to deliver.
            if event:
                try:
                    payload = json.loads(data) if data else {}
                except json.JSONDecodeError:
                    payload = {}
                yield event, payload
            event, data = "", ""
        elif line.startswith("event: "):
            event = line[len("event: ") :]
        elif line.startswith("data: "):
            data = line[len("data: ") :]
        elif line.startswith(":"):
            # A comment. Some servers use these as keepalives; this router sends
            # a real `ping` event instead, but both are ignorable.
            pass


# --------------------------------------------------------------------------
# HTTP helpers
# --------------------------------------------------------------------------


def _opener(cfg: Config):
    ctx = cfg.ssl_context or ssl.create_default_context()
    return urllib.request.build_opener(urllib.request.HTTPSHandler(context=ctx))


def _classify(err: urllib.error.HTTPError, what: str) -> ClientError:
    """Turn an HTTP status into a fatal or retryable error.

    ⚠ THE SPLIT IS ABOUT WHAT THE CALLER SHOULD DO NEXT, not about tidy error
    taxonomy. 402 is fatal because retrying against an empty balance never
    succeeds. 429 is retryable because waiting is precisely the right response.
    One error type would collapse both into "it failed" and leave a script
    unable to tell a pointless retry from a correct one.
    """
    try:
        body = err.read(512).decode("utf-8", errors="replace")
        message = json.loads(body).get("error", body)
    except Exception:
        message = err.reason or ""

    text = f"{what}: {message} (HTTP {err.code})"

    if err.code == 429:
        retry_after = None
        header = err.headers.get("Retry-After") if err.headers else None
        if header:
            try:
                retry_after = float(header)
            except ValueError:
                retry_after = None
        return RetryableError(text, retry_after=retry_after)
    if err.code >= 500:
        return RetryableError(text)
    # 400, 401, 402, 403, 404, 409 — every one needs a human to change
    # something, so retrying is a way of not noticing.
    return ClientError(text)


def _multipart(filename: str, body: bytes) -> tuple[bytes, str]:
    """Encode one file as multipart/form-data under the part name ``file``."""
    boundary = "----ocrr" + secrets.token_hex(16)
    content_type = mimetypes.guess_type(filename)[0] or "application/octet-stream"

    # The filename is quoted, and any quote or newline in it is removed: it is
    # caller-supplied and a raw newline here would let it forge extra MIME
    # headers.
    safe = filename.replace('"', "").replace("\r", "").replace("\n", "")

    head = (
        f"--{boundary}\r\n"
        f'Content-Disposition: form-data; name="file"; filename="{safe}"\r\n'
        f"Content-Type: {content_type}\r\n\r\n"
    ).encode()
    tail = f"\r\n--{boundary}--\r\n".encode()

    return head + body + tail, f"multipart/form-data; boundary={boundary}"


# --------------------------------------------------------------------------
# The three protocol steps
# --------------------------------------------------------------------------


def _open_stream(cfg: Config, opener):
    """Open ``GET /sse`` and block until the ``hello`` frame arrives.

    Returns ``(response, frames, backlog)``. The caller must close the response.

    ⚠ "ESTABLISHED" MEANS hello HAS ARRIVED, not that the socket connected. The
    router subscribes the stream to its event bus inside the handler, so a
    dialled-but-unaccepted connection is not yet receiving anything — and an
    upload sent at that moment is racing the exact gap this function closes.

    The ``backlog`` frame is what makes a dropped stream survivable: it names
    jobs that finished while you were not listening, so a re-run collects them
    instead of waiting for an event that will never be sent again.
    """
    request = urllib.request.Request(
        cfg.router_url.rstrip("/") + "/sse",
        headers={
            "Authorization": f"Bearer {cfg.token}",
            "Accept": "text/event-stream",
        },
    )

    try:
        response = opener.open(request, timeout=cfg.timeout)
    except urllib.error.HTTPError as err:
        raise _classify(err, "opening the event stream") from None
    except OSError as err:
        raise RetryableError(f"opening the event stream: {err}") from None

    frames = _read_frames(response)
    backlog: set[str] = set()

    for event, payload in frames:
        if event == "hello":
            return response, frames, backlog
        if event == "backlog":
            # The router sends backlog immediately after hello; if that order
            # ever changes, record what it named and keep waiting.
            for job in payload.get("jobs", []):
                job_id = job.get("job_id")
                if job_id:
                    backlog.add(job_id)

    response.close()
    raise RetryableError("the event stream closed before it was established")


def _upload(cfg: Config, opener, job: Input) -> str:
    """Post the job and return its id."""
    query: dict[str, str] = {}

    # Pipeline wins over label when both are set, matching the router.
    if job.pipeline:
        query["pipeline"] = ",".join(job.pipeline)
    elif job.label:
        query["label"] = job.label

    query.update(job.params)

    # Sent in BOTH modes, never omitted. Absent would be read as units — the
    # same answer — but an explicit value makes a mismatch a disagreement
    # between two stated positions rather than between a statement and a
    # default, which is what makes the router's 409 legible.
    query["raw"] = "1" if job.raw else "0"

    url = cfg.router_url.rstrip("/") + "/upload?" + urllib.parse.urlencode(query)

    body: bytes | None = None
    headers = {"Authorization": f"Bearer {cfg.token}"}

    if job.body is not None:
        payload = job.body if isinstance(job.body, bytes) else job.body.read()
        body, content_type = _multipart(job.filename, payload)
        headers["Content-Type"] = content_type
    # With no body this sends nothing at all — the crawler shape, where the job
    # IS its parameters and the service fetches its own input.
    #
    # ⚠ `data` STAYS None RATHER THAN b"" FOR A PARAMS-ONLY JOB. urllib stamps
    # `Content-Type: application/x-www-form-urlencoded` on any request whose
    # data is not None, empty bytes included. The router picks its upload branch
    # off Content-Type, so that header is a claim about a body that does not
    # exist. It happens to fall through to the params-only branch today, which
    # is exactly why this is worth pinning: it works by luck, and a form branch
    # added later would silently capture these requests. The Go client sends no
    # body and no content type, and so does this.
    request = urllib.request.Request(url, data=body, headers=headers, method="POST")

    try:
        with opener.open(request, timeout=cfg.timeout) as response:
            created = json.loads(response.read().decode("utf-8"))
    except urllib.error.HTTPError as err:
        raise _classify(err, "uploading") from None
    except OSError as err:
        raise RetryableError(f"uploading: {err}") from None

    job_id = created.get("job_id", "")
    if not job_id:
        raise ClientError("the router accepted the upload and returned no job id")
    return job_id


def _collect(cfg: Config, opener, job_id: str) -> Result:
    """Fetch the finished result.

    ⚠ THIS CALL CHARGES THE CREDITS and consumes the in-memory result, so it
    happens exactly once, and only when the job is known to be ready.
    """
    request = urllib.request.Request(
        cfg.router_url.rstrip("/") + "/files/" + urllib.parse.quote(job_id),
        headers={"Authorization": f"Bearer {cfg.token}"},
    )

    try:
        with opener.open(request, timeout=cfg.timeout) as response:
            content_type = response.headers.get("Content-Type", "")
            payload = response.read()
    except urllib.error.HTTPError as err:
        raise _classify(err, "collecting the result") from None
    except OSError as err:
        raise RetryableError(f"collecting the result: {err}") from None

    # ⚠ BRANCH ON WHAT THE SERVER SAID, NEVER ON WHAT YOU ASKED FOR. The two
    # disagree exactly when something is wrong — a raw request answered with
    # units, or the reverse — and that is the case worth surfacing rather than
    # misreading. Decoding a JSON envelope as bytes hands the caller a file full
    # of `{"job_id": ...}` and calls it the result.
    if content_type.startswith("application/octet-stream"):
        return Result(job_id=job_id, raw=payload)

    decoded = json.loads(payload.decode("utf-8"))
    return Result(job_id=job_id, units=decoded.get("units", []))


# --------------------------------------------------------------------------
# The public entry point
# --------------------------------------------------------------------------


def submit(cfg: Config, job: Input, progress: Progress | None = None) -> Result:
    """Upload a job and block until it produces a result or fails.

    Raises :class:`FailedError` if the job ran and died, :class:`RetryableError`
    if the failure is worth another attempt, and :class:`ClientError` for
    anything a retry cannot fix.

    ⚠ THE ORDERING IS THE WHOLE CORRECTNESS ARGUMENT, and it is the opposite of
    what reads naturally. The stream is opened and ESTABLISHED before the upload
    is posted. Uploading first leaves a window in which a fast job finishes and
    fires `ready` into a stream nobody is holding — and the client then waits
    forever for an event that already happened. The window is small, which is
    worse than large: it passes every casual test and strands the caller in
    production on exactly the jobs that went well.
    """

    def report(stage: str, detail: str = "") -> None:
        if progress is not None:
            progress(stage, detail)

    opener = _opener(cfg)

    # Step 1 — the stream, first. See the docstring above.
    response, frames, backlog = _open_stream(cfg, opener)

    try:
        # Step 2 — the upload.
        report(Stage.UPLOADING)
        job_id = _upload(cfg, opener, job)

        # A result that was ALREADY waiting when we connected. This is what makes
        # a re-run after a dropped stream correct rather than hopeful.
        if job_id in backlog:
            report(Stage.COLLECTING, job_id)
            result = _collect(cfg, opener, job_id)
            report(Stage.DONE, job_id)
            return result

        # Step 3 — wait for this job's event.
        report(Stage.WAITING, job_id)
        for event, payload in frames:
            # ⚠ MATCH ON THE JOB ID. One stream carries every job belonging to
            # the customer, so a caller with two in flight sees both — and acting
            # on the wrong one collects somebody else's result, or fails on their
            # failure.
            if event == "ready":
                if payload.get("job_id") != job_id:
                    continue
                report(Stage.COLLECTING, job_id)
                result = _collect(cfg, opener, job_id)
                report(Stage.DONE, job_id)
                return result

            if event == "failed":
                if payload.get("job_id") != job_id:
                    continue
                raise FailedError(job_id, payload.get("reason", ""))

            if event == "backlog":
                # A later backlog frame can name our job if the stream
                # reconnected underneath us.
                if any(j.get("job_id") == job_id for j in payload.get("jobs", [])):
                    report(Stage.COLLECTING, job_id)
                    result = _collect(cfg, opener, job_id)
                    report(Stage.DONE, job_id)
                    return result

            # `ping` is the keepalive. It carries no job and needs no handling;
            # its value is that a silent stream and a dead one look different.

        # The stream ended without our event. Retryable: the job may well still
        # be running, and a fresh submit would find it in the backlog.
        raise RetryableError("the event stream closed before the job finished")
    finally:
        response.close()


# --------------------------------------------------------------------------
# Command line
# --------------------------------------------------------------------------


def _main(argv: list[str]) -> int:
    import argparse
    import sys

    parser = argparse.ArgumentParser(
        description="Submit one job to an ocr-router and write its result.",
        epilog="The token is read from OCRR_TOKEN. Passing it as a flag would put "
        "it in your shell history and in the output of ps.",
    )
    parser.add_argument("--router", required=True, help="base URL, e.g. https://ocr.example.com")
    parser.add_argument("-i", "--input", help="file to submit; omit for a params-only job")
    parser.add_argument("-o", "--output", default="-", help="where to write the result; - is stdout")
    parser.add_argument("--label", default="", help="which service")
    parser.add_argument("--pipeline", default="", help="comma-separated chain of services")
    parser.add_argument("--param", action="append", default=[], metavar="K=V")
    parser.add_argument("--raw", action="store_true", help="expect opaque bytes, not text units")
    parser.add_argument("--timeout", type=float, default=None)
    parser.add_argument("--quiet", action="store_true")
    args = parser.parse_args(argv)

    token = os.environ.get("OCRR_TOKEN", "")
    if not token:
        print("OCRR_TOKEN is not set", file=sys.stderr)
        return 1

    params = {}
    for item in args.param:
        if "=" not in item:
            print(f"--param must be KEY=VALUE, got {item!r}", file=sys.stderr)
            return 1
        key, value = item.split("=", 1)
        params[key] = value

    # ⚠ Progress goes to STDERR, always. That is what makes `-o -` safe to pipe:
    # mixing them would corrupt the result of every composed command.
    def progress(stage: str, detail: str) -> None:
        if not args.quiet:
            print(f"{stage} {detail}".rstrip(), file=sys.stderr)

    handle = open(args.input, "rb") if args.input else None
    try:
        result = submit(
            Config(router_url=args.router, token=token, timeout=args.timeout),
            Input(
                body=handle,
                filename=os.path.basename(args.input) if args.input else "input",
                label=args.label,
                pipeline=tuple(s for s in args.pipeline.split(",") if s),
                params=params,
                raw=args.raw,
            ),
            progress,
        )
    except FailedError as err:
        # 3: the job ran and failed. Distinct from 1 so a caller can tell "your
        # request was wrong" from "the work died".
        print(str(err), file=sys.stderr)
        return 3
    except RetryableError as err:
        print(str(err), file=sys.stderr)
        return 2
    except ClientError as err:
        print(str(err), file=sys.stderr)
        return 1
    finally:
        if handle is not None:
            handle.close()

    payload = result.raw if result.raw is not None else "\n".join(result.units).encode()

    if args.output == "-":
        sys.stdout.buffer.write(payload)
    else:
        # ⚠ Written atomically. A half-written result file that looks complete is
        # worse than no file, because the next step in a pipeline consumes it.
        temporary = args.output + ".partial"
        with open(temporary, "wb") as out:
            out.write(payload)
        os.replace(temporary, args.output)

    return 0


if __name__ == "__main__":
    import sys

    raise SystemExit(_main(sys.argv[1:]))

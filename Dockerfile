# syntax=docker/dockerfile:1

# ocr-router images: one build, two targets.
#
#   docker build --target router -t ocr-router:latest .
#   docker build --target worker -t ocr-worker:latest .
#
# ⚠ THERE IS NO `ocrmypdf` IN THE WORKER IMAGE, AND THAT IS DELIBERATE. The
# repository is named ocr-router and OCR IS NOT THE PRODUCT: the worker is a
# subprocess runner that forks whatever `--cmd` names, for whatever `--label` it
# serves. OCR is only the default label. Baking one toolchain in would make the
# image a lie about what this is, and would make every operator carry a PDF
# pipeline they may not want. Add yours in a layer of your own — see the commented
# example at the bottom of the worker stage.

# ---------------------------------------------------------------------------
# builder
# ---------------------------------------------------------------------------
# Pinned to the toolchain in go.mod. Bump both together: a builder older than the
# `go` directive fails loudly, and one newer silently builds with a toolchain
# nothing else in the project has tested.
FROM golang:1.26 AS builder

WORKDIR /src

# Modules first, so a source-only change does not re-download the graph.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# ⚠ NO `templ generate` STEP, and none is needed: the generated `*_templ.go`
# files are committed (four of them, tracked). If that ever stops being true this
# build breaks with a missing-symbol error rather than silently shipping stale
# markup — which is the right failure, but check here first.
#
# CGO_ENABLED=0 is what makes the result a static binary: the SQLite driver is
# modernc.org/sqlite, which is pure Go. A cgo-linked SQLite would need a libc in
# the final image and would end the `static` base below.
#
# -trimpath strips local paths out of the binary so the same source produces the
# same bytes on any machine; -s -w drop the symbol table and DWARF.
ENV CGO_ENABLED=0 GOOS=linux
RUN go build -trimpath -ldflags="-s -w" -o /out/router  ./cmd/router \
 && go build -trimpath -ldflags="-s -w" -o /out/worker  ./cmd/worker \
 && go build -trimpath -ldflags="-s -w" -o /out/client  ./cmd/client

# ---------------------------------------------------------------------------
# router
# ---------------------------------------------------------------------------
# distroless/static: no shell, no package manager, no libc. It carries the two
# things this binary actually needs from a base image — CA certificates and
# /etc/passwd — and nothing else.
FROM gcr.io/distroless/static-debian12:nonroot AS router

# ⚠ EVERY ASSET IS EMBEDDED IN THE BINARY. The stylesheet, the datastar bundle
# and the SQL migrations are all go:embed'ed, so there is nothing to COPY beside
# it and no asset path to get wrong at deploy time. A missing file here would be
# a build failure, not a 404 at 3am.
COPY --from=builder /out/router /router

# ⚠ THESE TWO PATHS ARE PERSISTENT STATE AND MUST BE VOLUMES. `/data/router.db`
# holds the customers, the tokens and the CREDIT LEDGER; `/data/blobs` holds
# uploaded files and raw results. A container without volumes mounted here loses
# the ledger on every restart, and the ledger is the audit of every credit
# movement — there is no way to reconstruct it.
#
# They are DECLARED rather than created: distroless has no shell, so there is no
# mkdir. The compose file creates them as named volumes, and Docker chowns a
# fresh named volume to the container's user. ⚠ A BIND MOUNT IS NOT CHOWNED — if
# you mount a host directory here it must already be writable by uid 65532.
VOLUME ["/data"]

# 1234 is the API and the dashboard. It is the ONLY port this image exposes.
#
# ⚠ /metrics IS NOT EXPOSED AND MUST NOT BE, by decision rather than oversight:
# ADR-0001 task T11 binds it to loopback because queue depths and throughput say
# how much work a customer pushes, and the main listener faces the internet. In a
# container, loopback is container-local, so the metrics listener is reachable
# only from inside. To scrape it, put the scraper in the SAME network namespace
# (`network_mode: "service:router"`) rather than moving the listener — see
# compose.yml.example.
EXPOSE 1234

USER nonroot:nonroot

ENTRYPOINT ["/router"]
# The defaults put the database in the working directory, which in this image is
# not writable. Naming both paths here means `docker run ocr-router` works, and a
# compose command that overrides them replaces this wholesale.
CMD ["--addr", ":1234", "--db", "/data/router.db", "--blobs", "/data/blobs"]

# ---------------------------------------------------------------------------
# worker
# ---------------------------------------------------------------------------
# Debian rather than distroless, and this is the one place the two targets
# genuinely differ: the worker's whole job is to FORK A COMMAND, so it needs a
# base an operator can install that command onto. A shell-less image cannot run
# `--cmd` at all.
FROM debian:12-slim AS worker

# ca-certificates because the worker calls the router over HTTPS in any real
# deployment (its `--router` URL), and an image with no roots fails every request
# with an opaque x509 error.
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates \
 && rm -rf /var/lib/apt/lists/*

COPY --from=builder /out/worker /worker

# ⚠ The worker writes each job's input and output under --tmpdir and cleans up
# after itself. Give it a writable one; /tmp in this image is world-writable, so
# the default works, but a read-only root filesystem needs a tmpfs mounted there.
ENV TMPDIR=/tmp

# A non-root user, created here because Debian's default is root and a subprocess
# runner is the last thing that should have it. ⚠ Your `--cmd` inherits this uid:
# if the tool you install needs to write somewhere, that somewhere must be
# writable by uid 65532.
RUN useradd --uid 65532 --create-home --shell /usr/sbin/nologin worker
USER 65532:65532

ENTRYPOINT ["/worker"]

# ⚠ NO CMD: `--cmd` is required and there is no sensible default for it. The
# worker refuses to start without one, which is better than starting and forking
# something nobody chose.
#
# To add your own tooling, build a stage on top of this one:
#
#   FROM ocr-worker:latest AS my-ocr-worker
#   USER root
#   RUN apt-get update && apt-get install -y --no-install-recommends \
#         ocrmypdf jq tesseract-ocr-eng && rm -rf /var/lib/apt/lists/*
#   COPY --chown=65532 my-wrapper.sh /usr/local/bin/my-wrapper
#   USER 65532:65532
#
# The wrapper is what ADR-0001 constrains: the router passes parameters as
# separate argv elements and never through a shell, so your script receives them
# as "$@" and must not re-expand them.

# ---------------------------------------------------------------------------
# client
# ---------------------------------------------------------------------------
# The CLI a customer runs. Separate target because it belongs on a customer's
# machine or in their CI, not beside the router.
FROM gcr.io/distroless/static-debian12:nonroot AS client
COPY --from=builder /out/client /client
USER nonroot:nonroot
ENTRYPOINT ["/client"]

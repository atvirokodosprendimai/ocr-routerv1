"""Architecture diagram for ocr-routerv1.

Source of truth — nothing here is invented:
  * docs/adr/0001-ocr-router-architecture.md  (Accepted, 2026-09-15) — the design
  * cmd/router/wire.go buildApp()             — the composition root, i.e. what is
                                                actually wired rather than intended
  * docs/adr/0002..0005                       — rate limiting, browser login,
                                                editable customer settings, CLI client

Regenerate from the repository root:
    python ~/.agents/skills/architecture-diagrams/scripts/generate.py \
        docs/diagrams/router-architecture.py --output-dir docs/diagrams
"""

from diagrams import Cluster, Diagram, Edge
from diagrams.generic.database import SQL
from diagrams.generic.storage import Storage
from diagrams.onprem.client import Client, Users
from diagrams.onprem.compute import Server
from diagrams.onprem.monitoring import Prometheus
from diagrams.onprem.network import Nginx
from diagrams.programming.language import Go

# Colours carry one protocol meaning each, consistently across the picture:
#   blue    = authenticated customer traffic
#   green   = authenticated worker traffic
#   purple  = durable state / SQLite transactions
#   brown   = blobs on disk
#   orange  = in-process async fan-out
#   red     = volatile state in RAM
#   grey    = operator-facing monitoring
GRAPH_ATTR = {
    "fontsize": "18",
    "bgcolor": "#ffffff",
    "pad": "0.6",
    "nodesep": "0.7",
    "ranksep": "1.0",
    "splines": "spline",
    "labeljust": "l",
}

ROUTER_CLUSTER = {"bgcolor": "#eef5fe", "style": "solid", "color": "#2f6fb3", "penwidth": "2.0"}
EDGE_CLUSTER = {"bgcolor": "#ffffff", "style": "dashed", "color": "#2f6fb3", "labeljust": "l"}
DOMAIN_CLUSTER = {"bgcolor": "#ffffff", "style": "dashed", "color": "#1f7a4d", "labeljust": "l"}
STATE_CLUSTER = {"bgcolor": "#f7f2fb", "style": "solid", "color": "#7b4fa8", "penwidth": "2.0"}
NET_CLUSTER = {"bgcolor": "#fafafa", "style": "dashed", "color": "#999999", "labeljust": "l"}

with Diagram(
    # The colour legend rides in the title: the edges are colour-coded, and a
    # key that lives only in a comment is a diagram you must read the source to use.
    "ocr-routerv1 \u2014 one router process, N internet-resident workers\n"
    "blue = customer   \u00b7   green = worker   \u00b7   purple = durable state\n"
    "brown = blob on disk   \u00b7   orange = async fan-out\n"
    "red = in RAM only   \u00b7   grey = monitoring",
    filename="router-architecture",
    outformat=["png", "svg"],
    show=False,
    direction="TB",
    graph_attr=GRAPH_ATTR,
):
    # --------------------------------------------------------------- customers
    with Cluster("Customers \u2014 outside the trust boundary", graph_attr=NET_CLUSTER):
        people = Users("Customer")
        cli = Client("cmd/client\nPOST one file, block for the result\n(ADR-0005)")

    # ----------------------------------------------------------------- workers
    with Cluster("Workers \u2014 anywhere on the internet, untrusted", graph_attr=NET_CLUSTER):
        w_ocr = Server("cmd/worker\nlabel=ocr")
        w_more = Server("cmd/worker\nlabel=crawl / strip-html")
        child = Server("forked child\ncmd -i <tmpfile> -o -\nstdout: JSON []string")

    # ------------------------------------------------------------------ router
    with Cluster("Router process \u2014 cmd/router, exactly ONE", graph_attr=ROUTER_CLUSTER):
        with Cluster("HTTP surface", graph_attr=EDGE_CLUSTER):
            api = Go("internal/httpapi (chi)\nPOST /upload \u00b7 GET /sse\nGET /files/{id} \u00b7 POST /claim\n\nclaim takes NO body \u2014 any queued job")
            rl = Nginx("internal/ratelimit\nper-token RPS per role\n(ADR-0002)")
            hz = Go("GET /healthz\nliveness only: SELECT 1\n+ blob write probe")

        with Cluster("Domain \u2014 internal/router.Service is the SINGLE WRITER", graph_attr=DOMAIN_CLUSTER):
            svc = Go("internal/router.Service\nqueued \u2192 processing \u2192 done \u2192 delivered\naged priority + lease + reaper\npipeline stage advance")
            bus = Go("internal/bus\nin-process fan-out")
            res = Go("internal/results\nresults in RAM only, TTL")
            ident = Go("internal/identity\nbearer tokens + browser sessions")

        with Cluster("Admin dashboard", graph_attr=EDGE_CLUSTER):
            dash = Go("internal/web (datastar)\nlogin \u00b7 customer settings \u00b7 tokens\n(ADR-0003, ADR-0004)")

        with Cluster("Observability", graph_attr=EDGE_CLUSTER):
            reg = Go("internal/monitor\nocrr_* registry")

    # ----------------------------------------------------------- durable state
    with Cluster("Durable state \u2014 survives a restart", graph_attr=STATE_CLUSTER):
        db = SQL("SQLite, modernc, no cgo\nwriter: _txlock=immediate, MaxOpenConns(1)\nreader: _pragma=query_only(1)")
        blob = Storage("blob dir\nsource + pipeline-stage blobs\nkept until delivery")

    # ------------------------------------------------------------------- edges
    people >> cli
    cli >> Edge(label="bearer token, role=client", color="blue") >> api
    [w_ocr, w_more] >> Edge(label="bearer token, role=worker", color="darkgreen") >> api
    [w_ocr, w_more] >> Edge(label="exec \u2014 never a shell", color="darkgreen", style="dotted") >> child

    api >> Edge(color="blue") >> rl
    rl >> svc
    api >> ident
    dash >> ident
    hz >> Edge(label="SELECT 1", color="purple", style="dotted") >> db
    hz >> Edge(label="write probe", color="brown", style="dotted") >> blob

    svc >> Edge(label="charged at delivery, once:\nledger + credits + delivered\nin ONE immediate tx", color="purple", penwidth="2.0") >> db
    svc >> Edge(label="stage output becomes\nthe next stage's input", color="brown") >> blob
    api >> Edge(label="GET /files/{id}: blob out (lease),\nresult out (deleted on success)", color="brown", style="dotted") >> blob
    svc >> Edge(label="results", color="red") >> res
    svc >> Edge(label="transitions", color="orange", style="dashed") >> bus
    bus >> Edge(label="SSE: work signal + ready backlog,\nkeepalive", color="orange", style="dashed") >> api

    svc >> Edge(color="grey", style="dotted") >> reg
    reg >> Edge(label="GET /metrics on --metrics-addr 127.0.0.1:9090\n(private listener, no auth)", color="grey", style="dotted") >> Prometheus("Prometheus")

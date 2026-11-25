"""Private, opt-in OTLP pipeline for the authorized recall path."""
import asyncio
import ipaddress
import logging
import os
from contextlib import asynccontextmanager, contextmanager
from urllib.parse import urlsplit



def _provider(endpoint):
    import requests
    from opentelemetry.exporter.otlp.proto.http.trace_exporter import OTLPSpanExporter
    from opentelemetry.sdk.resources import Resource
    from opentelemetry.sdk.trace import TracerProvider
    from opentelemetry.sdk.trace.export import BatchSpanProcessor

    url = urlsplit(endpoint)
    local = url.hostname == "localhost"
    if url.hostname and not local:
        try:
            local = ipaddress.ip_address(url.hostname).is_loopback
        except ValueError:
            pass
    if (not url.hostname or url.username is not None or url.password is not None or url.query or url.fragment
            or not url.path or not (url.scheme == "https" or (local and url.scheme == "http"))):
        raise ValueError("invalid trace endpoint")
    session = requests.Session()
    session.max_redirects = 0
    exporter = OTLPSpanExporter(endpoint=endpoint, session=session, timeout=3)
    provider = TracerProvider(resource=Resource({"service.name": "memosphere"}), shutdown_on_exit=False)
    provider.add_span_processor(BatchSpanProcessor(exporter, max_queue_size=256, max_export_batch_size=32,
                                                  schedule_delay_millis=1000, export_timeout_millis=3000))
    return provider


@asynccontextmanager
async def memory_tracing(database):
    endpoint = os.environ.get("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT")
    provider = None
    if endpoint:
        try:
            provider = _provider(endpoint)
        except (ImportError, ValueError):
            logging.getLogger(__name__).warning("Memory tracing disabled; check optional tracing dependencies and endpoint")
    database.recall_tracer = provider.get_tracer("memosphere.memory") if provider else None
    try:
        yield
    finally:
        database.recall_tracer = None
        if provider:
            await asyncio.to_thread(provider.shutdown)


@contextmanager
def recall_span(database, headers):
    tracer = getattr(database, "recall_tracer", None)
    if tracer is None:
        yield None
        return
    from opentelemetry.context import Context
    from opentelemetry.trace import Status, StatusCode
    from opentelemetry.trace.propagation.tracecontext import TraceContextTextMapPropagator

    # Only correlate traceparent. Never propagate baggage, cookies, or MCP credentials.
    parent = TraceContextTextMapPropagator().extract({"traceparent": headers.get("traceparent", "")}, context=Context())
    with tracer.start_as_current_span("memosphere.recall", context=parent,
                                      record_exception=False, set_status_on_exception=False) as span:
        try:
            yield span
        except Exception:
            span.set_status(Status(StatusCode.ERROR))
            raise


def recall_metrics(span, result):
    if span is None or not span.is_recording():
        return
    rows = result["memories"]
    span.set_attribute("memory.candidate_count", len(rows))
    span.set_attribute("memory.purpose", result["purpose"])
    info = result.get("retrieval", {})
    span.set_attribute("memory.degraded", bool(info.get("degraded")))
    for pos, row in enumerate(rows, 1):
        attrs = {"memory.rank": pos, "memory.revision": row["revision"]}
        ranks = row.get("ranking", {}).get("ranks", {})
        for channel in ("semantic", "bm25", "entity"):
            if channel in ranks:
                attrs["memory.rank." + channel] = ranks[channel]
        span.add_event("memory.candidate", attrs)

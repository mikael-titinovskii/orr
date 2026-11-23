"""Run the NeMo Guardrails check server on loopback.

The nemoguardrails CLI binds 0.0.0.0 and has no host option. This runner sets
the same attributes the CLI sets, then binds 127.0.0.1 so the check endpoint is
reachable only from this machine.
"""

import asyncio
import hashlib
import json
import logging
import os
import sys
import threading
import time

config_path = os.environ["ORR_GUARDRAILS_CONFIG"]

# The installer keeps the weights inside the installation. Deriving HF_HOME
# here, before the imports that read it, makes the sidecar find them even when
# it is started by something that does not pass the variable — an older orr, or
# an operator running this file by hand. Offline mode then guarantees a check
# loads from disk instead of reaching the network without a token.
install_dir = os.path.dirname(os.path.dirname(os.path.abspath(config_path)))
os.environ.setdefault("HF_HOME", os.path.join(install_dir, "hf"))
if os.path.isdir(os.path.join(os.environ["HF_HOME"], "hub")):
    os.environ.setdefault("HF_HUB_OFFLINE", "1")
    os.environ.setdefault("TRANSFORMERS_OFFLINE", "1")

import uvicorn
from starlette.middleware.base import BaseHTTPMiddleware

from nemoguardrails.library.hf_classifier import actions as hf_classifier_actions
from nemoguardrails.library.hf_classifier import backends as hf_classifier_backends
from nemoguardrails.library.hf_classifier.backends import get_backend
from nemoguardrails.rails.llm.config import RailsConfig
from nemoguardrails.server import api

# The library logs checked text at DEBUG and INFO. orr must not persist prompts
# or response bodies, so keep the sidecar at WARNING and above.
logging.getLogger("nemoguardrails").setLevel(logging.WARNING)

api.app.rails_config_path = config_path
api.set_default_config_id(os.environ.get("ORR_GUARDRAILS_CONFIG_ID", "orr"))

# The check response says whether a rail blocked, not how long each rail took.
# orr needs the model's own share to separate Prompt Guard from the rule rails
# and the HTTP round trip, so the classifier calls are timed and the sum rides
# back on the response.
#
# The bucket is keyed by the text being classified, not held in a stack or a
# context variable. NeMo serves rails from a worker whose context was frozen at
# the first request it handled, so a ContextVar set per request reads back as
# that first value forever; and a stack is process-global, so a second check
# arriving while the first is still classifying would take the top slot and be
# credited with time it never spent. The text is the one thing both the request
# and the classifier call agree on, so it is the key.
#
# The check response also says only that a rail blocked, not the number behind
# it, and the number is what separates a false positive from a true one. Each
# classification's own label and score are kept under the same key and ride back
# on the response with the timing.
_active_buckets = {}
_active_scores = {}
_active_lock = threading.Lock()
# Each classifier's blocked labels and threshold, filled in by warm_classifiers
# before the server accepts a check. They let the verdict header name the window
# that decided a block rather than a benign one that merely scored higher.
_classifier_policies = []
_classify_and_check = hf_classifier_actions._classify_and_check


def _text_key(text):
    if not isinstance(text, str):
        return None
    return hashlib.sha256(text.encode("utf-8", "replace")).hexdigest()


def _best_score(results):
    """Return the (label, score) that best explains a classification.

    A text longer than one model window (512 tokens for Prompt Guard) comes back
    as a list of per-window result lists, so the entries are flattened before
    comparing. Without the flatten a long message reported a block with no number
    behind it — exactly the sample an operator cannot narrow without a score.

    When the windows disagree, the number that decided the verdict wins: a
    blocked label at or above its threshold is reported ahead of a benign window
    that merely scored higher, so a block never names the window that passed.
    """
    best = None
    flagged = None
    pending = [results]
    while pending:
        current = pending.pop()
        if isinstance(current, (list, tuple)):
            pending.extend(current)
            continue
        if not isinstance(current, dict):
            continue
        try:
            score = float(current.get("score"))
        except (TypeError, ValueError):
            continue
        label = str(current.get("label") or "")
        entry = (label, score)
        if best is None or score > best[1]:
            best = entry
        if _is_blocked_score(label, score) and (flagged is None or score > flagged[1]):
            flagged = entry
    return flagged or best


def _is_blocked_score(label, score):
    """Report whether a classifier's own policy blocks this label and score."""
    return any(label in labels and score >= threshold for labels, threshold in _classifier_policies)


def _remember_score(text, results):
    """Keep the label and score behind one classification, for the verdict.

    The rail reports blocked or allowed and drops the number, which is the one
    thing that tells a false positive from a true one. The threshold compares
    the highest score, so that is the one kept.
    """
    key = _text_key(text)
    if key is None:
        return
    best = _best_score(results)
    if best is None:
        return
    with _active_lock:
        samples = _active_scores.get(key)
        if samples is not None:
            samples.append(best)


def _check_text_keys(body):
    """Return the keys of every string a check may classify.

    The request carries the messages as JSON strings, so decoding them yields the
    same text the rail receives. A body that will not decode simply reports no
    timing, which is the same fallback as a classifier that was never called.
    """
    try:
        payload = json.loads(body or b"{}")
    except ValueError:
        return []
    keys = set()

    def register(text):
        if isinstance(text, str):
            key = _text_key(text)
            if key:
                keys.add(key)

    for message in payload.get("messages") or []:
        if not isinstance(message, dict):
            continue
        content = message.get("content")
        # orr marshals the text as a JSON string, so the content a check carries
        # is both the encoded value and the text the rail will classify. Register
        # each form; a body of either shape then matches its own call.
        register(content)
        if isinstance(content, str):
            try:
                register(json.loads(content))
            except ValueError:
                pass
    return list(keys)


async def timed_classify_and_check(*args, **kwargs):
    text = kwargs.get("text")
    if text is None and len(args) > 1:
        text = args[1]
    key = _text_key(text)
    started = time.perf_counter()
    try:
        return await _classify_and_check(*args, **kwargs)
    finally:
        if key is not None:
            with _active_lock:
                samples = _active_buckets.get(key)
            if samples is not None:
                samples.append((time.perf_counter() - started) * 1000)


hf_classifier_actions._classify_and_check = timed_classify_and_check


class ClassifierTimingMiddleware(BaseHTTPMiddleware):
    """Report the classifier's share and verdict as X-Orr-Guard-* headers."""

    async def dispatch(self, request, call_next):
        if request.url.path != "/v1/checks":
            return await call_next(request)
        body = await request.body()
        registered = []
        with _active_lock:
            for key in _check_text_keys(body):
                samples = _active_buckets.setdefault(key, [])
                scores = _active_scores.setdefault(key, [])
                registered.append((key, samples, scores))
        try:
            response = await call_next(request)
        finally:
            with _active_lock:
                for key, samples, scores in registered:
                    if _active_buckets.get(key) is samples:
                        _active_buckets.pop(key, None)
                    if _active_scores.get(key) is scores:
                        _active_scores.pop(key, None)
        measured = [samples for _, samples, _ in registered if samples]
        if measured:
            response.headers["X-Orr-Guard-Model-Ms"] = f"{sum(sum(s) for s in measured):.1f}"
        scored = [score for _, _, scores in registered for score in scores]
        if scored:
            label, score = max(scored, key=lambda item: item[1])
            response.headers["X-Orr-Guard-Label"] = f"{label} {score:.3f}"
        return response


api.app.add_middleware(ClassifierTimingMiddleware)

# The classifier is small and runs on the CPU. Dynamically quantizing its
# Linear layers cuts a window's inference by about a third, and it is the one
# optimization that measured a win here: batching NeMo's windows gave nothing at
# 512 tokens, where compute rather than call overhead dominates.
#
# NeMo's chunking windows a text of any length without truncation, which makes a
# long message cost unbounded CPU: 512-token windows at ~75 ms each on the
# machines measured here, so a 120 KB paste took 6.6 s. The sidecar runs one
# rails worker, so that check holds every check behind it past the proxy's 2 s
# budget. The model therefore reads a bounded text of 8 KiB — the first and last
# 4 KiB, because whoever writes the message controls the padding and an
# instruction is appended as often as prefixed. The deterministic rails are
# separate flows and still read the whole text; a cut is logged so that the part
# the model never saw is visible rather than implied.
CLASSIFIER_TEXT_LIMIT = 8 << 10
CLASSIFIER_EDGE_LIMIT = CLASSIFIER_TEXT_LIMIT // 2


def _bounded_classifier_text(text):
    """Return what the classifier reads of *text*, and whether anything was cut."""
    if not isinstance(text, str):
        return text, False
    encoded = text.encode("utf-8", "replace")
    if len(encoded) <= CLASSIFIER_TEXT_LIMIT:
        return text, False
    head = encoded[:CLASSIFIER_EDGE_LIMIT].decode("utf-8", "ignore")
    tail = encoded[-CLASSIFIER_EDGE_LIMIT:].decode("utf-8", "ignore")
    return head + "\n…\n" + tail, True


_pipeline_lock = threading.Lock()
_pipeline_cache = {}
_original_pipeline = hf_classifier_backends._get_or_create_pipeline


class SerializedPipeline:
    """NeMo's pipeline behind a lock; quantized inference is not documented as thread-safe."""

    def __init__(self, pipe):
        self.pipe = pipe
        self.lock = threading.Lock()

    def __call__(self, text):
        bounded, cut = _bounded_classifier_text(text)
        if cut:
            print(
                f"classifier: judged {CLASSIFIER_TEXT_LIMIT} of "
                f"{len(text.encode('utf-8', 'replace'))} bytes (head and tail)",
                file=sys.stderr,
                flush=True,
            )
        with self.lock:
            results = self.pipe(bounded)
        _remember_score(text, results)
        return results


def _quantized_pipeline(model_name, task, parameters):
    if task != "text-classification":
        return _original_pipeline(model_name, task, parameters)
    key = (model_name, task)
    with _pipeline_lock:
        cached = _pipeline_cache.get(key)
    if cached is not None:
        return cached
    pipe = _original_pipeline(model_name, task, parameters)
    # torch is installed only with the classifier, so it is imported where the
    # quantization needs it: a deterministic install runs this runner without it.
    try:
        import torch
        from torch.ao.quantization import quantize_dynamic

        pipe.model = quantize_dynamic(pipe.model.eval(), {torch.nn.Linear}, dtype=torch.qint8)
        print("classifier: int8 pipeline", file=sys.stderr, flush=True)
    except Exception as exc:  # noqa: BLE001 - an unquantized pipeline still works
        print(f"classifier: int8 unavailable ({exc}); using fp32", file=sys.stderr, flush=True)
    wrapped = SerializedPipeline(pipe)
    with _pipeline_lock:
        _pipeline_cache[key] = wrapped
    return wrapped


hf_classifier_backends._get_or_create_pipeline = _quantized_pipeline


def warm_classifiers():
    """Load every declared classifier before the health check can pass.

    A model loaded lazily is reported healthy and then fails the first request
    that matters. Failing here instead exits non-zero, and orr refuses to start
    with the sidecar's own error rather than passing the failure to a client.
    """
    config = RailsConfig.from_path(config_path)
    classifiers = getattr(config.rails.config, "hf_classifier", None) or {}
    for name, classifier in classifiers.items():
        asyncio.run(get_backend(classifier, name=name).classify("warm up"))
        _classifier_policies.append((
            set(getattr(classifier, "blocked_labels", None) or []),
            float(getattr(classifier, "threshold", 0.0) or 0.0),
        ))


warm_classifiers()

uvicorn.run(
    api.app,
    host="127.0.0.1",
    port=int(os.environ.get("ORR_GUARDRAILS_PORT", "8765")),
    log_level=os.environ.get("ORR_GUARDRAILS_LOG_LEVEL", "warning"),
)

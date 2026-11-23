"""HTTP server for the KTMS MT contract:

GET  /v1/models     -> {"data": [{"id", "capabilities": ["translate"], "languages": {"source", "target"}}]}
POST /v1/translate  -> {"model", "translations": [{"text", "detected_source_language"}],
                        "usage": {"input_tokens", "output_tokens", "characters"}}
GET  /health        -> {"status": "ok"}
Errors: {"error": {"code", "message"}} with 400 bad_request | unsupported_language_pair,
401, 404 model_not_found, 500 engine_error.
"""

import hmac
import json
import logging
import os
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

from . import __version__
from .catalog import engines_of
from .config import bundle, engine_recipe
from .engine import BadRequest, Engine, Translator, UnsupportedPair

log = logging.getLogger("ktms-mt")
MAX_BODY = 4 << 20


def build(cfg, detect):
    models, engines = {}, {}
    cores = os.cpu_count() or 4
    threads = cfg.threads or cores
    inter = max(1, cfg.max_concurrency)
    for mid in cfg.served:
        b = bundle(cfg, mid)
        models[mid] = b
        for name in engines_of(b):
            if name in engines:
                continue
            r = engine_recipe(name)
            path = os.path.join(cfg.models_dir, name)
            if not os.path.isdir(path):
                raise FileNotFoundError(f"{path}: run `ktms-mt fetch {mid}`")
            engines[name] = Engine(name, path, r.kind, r.source_prefix, cfg.device, cfg.compute_type,
                                   inter_threads=inter, intra_threads=max(1, threads // inter),
                                   beam_size=cfg.beam_size, max_batch_tokens=cfg.max_batch_tokens)
    return Translator(models, engines, detect, cfg.max_segment_tokens)


def languages(b):
    if b.any_engine:
        return {"source": ["*"], "target": ["*"]}
    src = sorted(b.pairs)
    if b.fallback:
        src = ["*"]
    return {"source": src, "target": [b.target]}


class Handler(BaseHTTPRequestHandler):
    server_version = "ktms-mt/" + __version__
    translator: Translator = None
    cfg = None

    def log_message(self, fmt, *args):
        log.debug("%s " + fmt, self.address_string(), *args)

    def _send(self, status, doc):
        body = json.dumps(doc, ensure_ascii=False).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def _error(self, status, code, message):
        self._send(status, {"error": {"code": code, "message": message}})

    def _authorized(self):
        key = self.cfg.api_key
        if not key:
            return True
        got = self.headers.get("Authorization", "")
        return got.startswith("Bearer ") and hmac.compare_digest(got[7:], key)

    def do_GET(self):
        if self.path == "/health":
            return self._send(200, {"status": "ok", "version": __version__})
        if not self._authorized():
            return self._error(401, "unauthorized", "invalid bearer token")
        if self.path == "/v1/models":
            data = [{"id": mid, "object": "model", "owned_by": "ktms-mt", "capabilities": ["translate"],
                     "languages": languages(b)} for mid, b in self.translator.models.items()]
            return self._send(200, {"object": "list", "data": data})
        self._error(404, "not_found", "no such route")

    def do_POST(self):
        if self.path != "/v1/translate":
            return self._error(404, "not_found", "no such route")
        if not self._authorized():
            return self._error(401, "unauthorized", "invalid bearer token")
        try:
            n = int(self.headers.get("Content-Length", "0"))
            if n > MAX_BODY:
                return self._error(400, "bad_request", "body too large")
            req = json.loads(self.rfile.read(n) or b"{}")
            texts = req.get("texts")
            if not isinstance(texts, list) or not all(isinstance(t, str) for t in texts):
                raise BadRequest("texts must be a list of strings")
            if not texts or len(texts) > self.cfg.max_texts:
                raise BadRequest(f"between 1 and {self.cfg.max_texts} texts per call")
            fmt = req.get("format") or "text"
            if fmt not in ("text", "html"):
                raise BadRequest("format is text or html")
            mid = req.get("model") or next(iter(self.translator.models))
            start = time.monotonic()
            out, usage = self.translator.translate(mid, texts, req.get("source_language", ""), req.get("target_language", ""), fmt)
            log.info("translated %d texts, %d chars with %s in %.0f ms", len(texts), usage["characters"], mid,
                     (time.monotonic() - start) * 1000)
            self._send(200, {"model": mid, "translations": out, "usage": usage})
        except KeyError as e:
            self._error(404, "model_not_found", f"model {e} is not served here")
        except (BadRequest, json.JSONDecodeError, ValueError) as e:
            self._error(400, "bad_request", str(e))
        except UnsupportedPair as e:
            self._error(400, "unsupported_language_pair", str(e))
        except Exception as e:  # an engine failure must not kill the worker thread
            log.exception("translation failed")
            self._error(500, "engine_error", str(e)[:300])


def serve(cfg, translator, warm=True):
    if warm:
        for e in translator.engines.values():
            e.load()
            log.info("loaded %s", e.name)
    handler = type("BoundHandler", (Handler,), {"translator": translator, "cfg": cfg})
    httpd = ThreadingHTTPServer((cfg.host, cfg.port), handler)
    httpd.daemon_threads = True
    log.info("ktms-mt %s serving %s on http://%s:%d", __version__, ", ".join(translator.models), cfg.host, cfg.port)
    return httpd


def run(cfg, translator):
    httpd = serve(cfg, translator)
    stop = threading.Event()
    try:
        httpd.serve_forever()
    except KeyboardInterrupt:
        stop.set()
    finally:
        httpd.server_close()

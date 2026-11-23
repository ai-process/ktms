"""Server configuration: a TOML file written by `ktms-mt fetch`."""

import json
import os
import tomllib
from dataclasses import dataclass, field

from .catalog import BUNDLES, ENGINES, Bundle

DEFAULT_PORT = 11436


@dataclass
class Config:
    host: str = "127.0.0.1"
    port: int = DEFAULT_PORT
    models_dir: str = ""
    api_key: str = ""
    device: str = "cpu"
    compute_type: str = "int8"
    threads: int = 0  # 0 = every core
    max_concurrency: int = 2  # batches CTranslate2 runs at once
    max_batch_tokens: int = 4096
    max_segment_tokens: int = 200
    beam_size: int = 1  # greedy: twice the throughput of 2 for a small loss on reviews
    max_texts: int = 128
    served: list = field(default_factory=list)  # model ids, from BUNDLES or [models.<id>]
    custom: dict = field(default_factory=dict)  # id -> Bundle


def default_models_dir():
    return os.environ.get("KTMS_MT_HOME", os.path.expanduser("~/.ktms-mt/models"))


def load(path):
    cfg = Config(models_dir=default_models_dir())
    if path and os.path.exists(path):
        with open(path, "rb") as f:
            doc = tomllib.load(f)
        server = doc.get("server", {})
        for k, v in server.items():
            if not hasattr(cfg, k):
                raise ValueError(f"{path}: unknown setting server.{k}")
            setattr(cfg, k, v)
        cfg.served = list(doc.get("serve", []))
        for mid, spec in doc.get("models", {}).items():
            cfg.custom[mid] = Bundle(target=spec.get("target", "*"), pairs=dict(spec.get("pairs", {})),
                                     fallback=spec.get("fallback", ""), any_engine=spec.get("any", ""))
            if mid not in cfg.served:
                cfg.served.append(mid)
    cfg.api_key = os.environ.get("KTMS_MT_API_KEY", cfg.api_key)
    return cfg


def bundle(cfg, mid):
    if mid in cfg.custom:
        return cfg.custom[mid]
    if mid in BUNDLES:
        return BUNDLES[mid]
    raise ValueError(f"unknown model {mid}: not in the catalog nor in [models.{mid}]")


SERVER_FIELDS = ["host", "port", "models_dir", "api_key", "device", "compute_type", "threads", "max_concurrency",
                 "max_batch_tokens", "max_segment_tokens", "beam_size", "max_texts"]


def _toml(v):
    return json.dumps(v) if isinstance(v, str) else str(v).lower() if isinstance(v, bool) else str(v)


def write(path, cfg):
    """Rewrites the file from cfg: every server setting, the served list and custom models."""
    # Top-level keys come before the first table, or TOML files them under it.
    lines = ["# ktms-mt configuration; `ktms-mt fetch MODEL` adds to `serve`.",
             "serve = [" + ", ".join(f'"{m}"' for m in cfg.served if m not in cfg.custom) + "]", "", "[server]"]
    for k in SERVER_FIELDS:
        v = getattr(cfg, k)
        # An empty api_key stays out: KTMS_MT_API_KEY may set it instead.
        if k == "api_key" and (not v or os.environ.get("KTMS_MT_API_KEY")):
            continue
        lines.append(f"{k} = {_toml(v)}")
    lines.append("")
    for mid, b in cfg.custom.items():
        lines.append(f"[models.{mid}]")
        lines.append(f'target = "{b.target}"')
        if b.pairs:
            lines.append("pairs = { " + ", ".join(f'{k} = "{v}"' for k, v in b.pairs.items()) + " }")
        if b.fallback:
            lines.append(f'fallback = "{b.fallback}"')
        if b.any_engine:
            lines.append(f'any = "{b.any_engine}"')
        lines.append("")
    os.makedirs(os.path.dirname(os.path.abspath(path)), exist_ok=True)
    tmp = path + ".tmp"
    with open(tmp, "w") as f:
        f.write("\n".join(lines))
    os.replace(tmp, path)


def engine_recipe(name):
    if name not in ENGINES:
        raise ValueError(f"unknown engine {name}")
    return ENGINES[name]

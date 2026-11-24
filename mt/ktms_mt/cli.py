"""ktms-mt serve | fetch | bench | catalog"""

import argparse
import logging
import os
import shutil
import ssl
import sys
import time
import urllib.request

from . import __version__
from .catalog import BUNDLES, ENGINES, NON_COMMERCIAL, engines_of
from .config import DEFAULT_PORT, bundle, engine_recipe, load, write
from .langid import LID_FILE, LID_URL, Detector


def default_config():
    return os.environ.get("KTMS_MT_CONFIG", os.path.expanduser("~/.ktms-mt/config.toml"))


def cmd_serve(args):
    from .server import build, run
    cfg = load(args.config)
    if args.port:
        cfg.port = args.port
    if args.host:
        cfg.host = args.host
    if not cfg.served:
        sys.exit("nothing to serve: run `ktms-mt fetch opus-mt-de` first")
    detect = Detector(os.path.join(cfg.models_dir, LID_FILE))
    run(cfg, build(cfg, detect))


def convert(name, dest, quantization):
    r = engine_recipe(name)
    try:
        from ctranslate2.converters import TransformersConverter
    except ImportError:
        sys.exit("conversion needs the convert extra: pip install 'ktms-mt[convert]'")
    tmp = dest + ".tmp"
    shutil.rmtree(tmp, ignore_errors=True)
    print(f"Converting {r.hf} ({r.license}) to CTranslate2 {quantization} ...", flush=True)
    TransformersConverter(r.hf, copy_files=list(r.copy_files)).convert(tmp, quantization=quantization, force=True)
    shutil.rmtree(dest, ignore_errors=True)
    os.replace(tmp, dest)


def download(url, dest):
    # python.org builds ship no CA store; certifi comes with the convert extra.
    ctx = None
    try:
        import certifi
        ctx = ssl.create_default_context(cafile=certifi.where())
    except ImportError:
        pass
    with urllib.request.urlopen(url, context=ctx) as r, open(dest + ".tmp", "wb") as f:
        shutil.copyfileobj(r, f)
    os.replace(dest + ".tmp", dest)


def cmd_fetch(args):
    cfg = load(args.config)
    if args.models_dir:
        cfg.models_dir = args.models_dir
    os.makedirs(cfg.models_dir, exist_ok=True)
    lid = os.path.join(cfg.models_dir, LID_FILE)
    if not os.path.exists(lid):
        print(f"Downloading the language identifier {LID_URL} ...", flush=True)
        download(LID_URL, lid)
    for mid in args.models:
        if mid in NON_COMMERCIAL:
            sys.exit(f"{mid} is licensed {NON_COMMERCIAL[mid]} (non-commercial); ktms-mt does not install it")
        b = bundle(cfg, mid)
        for name in engines_of(b):
            dest = os.path.join(cfg.models_dir, name)
            if os.path.isdir(dest) and not args.force:
                print(f"{name} is already converted.")
                continue
            convert(name, dest, args.quantization)
        if mid not in cfg.served:
            cfg.served.append(mid)
    write(args.config, cfg)
    print(f"Serving {', '.join(cfg.served)}; configuration in {args.config}.")


def cmd_bench(args):
    from .server import build
    cfg = load(args.config)
    detect = Detector(os.path.join(cfg.models_dir, LID_FILE))
    tr = build(cfg, detect)
    with open(args.file, encoding="utf-8") as f:
        texts = [line.rstrip("\n") for line in f if line.strip()][: args.limit]
    for e in tr.engines.values():
        e.load()
    tr.translate(args.model, texts[: args.batch], args.source, args.target)  # warm-up
    start = time.monotonic()
    chars = 0
    for i in range(0, len(texts), args.batch):
        _, usage = tr.translate(args.model, texts[i:i + args.batch], args.source, args.target)
        chars += usage["characters"]
    took = time.monotonic() - start
    print(f"{len(texts)} texts, {chars} chars in {took:.1f} s: {len(texts) / took * 60:.0f} texts/min, "
          f"{chars / took:.0f} chars/s (batch {args.batch}, beam {cfg.beam_size}, {cfg.compute_type})")


def cmd_catalog(_):
    print("Models (install-node.sh --model, ktms-mt fetch):")
    for mid, b in BUNDLES.items():
        print(f"  {mid:20} -> {b.target:3} via {', '.join(engines_of(b))}")
    print("Engines:")
    for name, r in ENGINES.items():
        print(f"  {name:20} {r.hf} [{r.license}]")
    print("Not installed (non-commercial): " + ", ".join(f"{k} ({v})" for k, v in NON_COMMERCIAL.items()))


def main(argv=None):
    p = argparse.ArgumentParser(prog="ktms-mt", description="Machine-translation engine for KTMS nodes")
    p.add_argument("--version", action="version", version=__version__)
    p.add_argument("--config", default=default_config(), help="TOML configuration (KTMS_MT_CONFIG)")
    p.add_argument("-v", "--verbose", action="store_true")
    sub = p.add_subparsers(dest="cmd", required=True)
    s = sub.add_parser("serve", help="serve /v1/models and /v1/translate")
    s.add_argument("--host")
    s.add_argument("--port", type=int, help=f"default {DEFAULT_PORT}")
    s.set_defaults(fn=cmd_serve)
    f = sub.add_parser("fetch", help="download and convert models, and serve them")
    f.add_argument("models", nargs="+")
    f.add_argument("--models-dir")
    f.add_argument("--quantization", default="int8")
    f.add_argument("--force", action="store_true")
    f.set_defaults(fn=cmd_fetch)
    b = sub.add_parser("bench", help="translate a file, one text per line, and report throughput")
    b.add_argument("file")
    b.add_argument("--model", default="opus-mt-de")
    b.add_argument("--source", default="")
    b.add_argument("--target", default="de")
    b.add_argument("--batch", type=int, default=64)
    b.add_argument("--limit", type=int, default=2000)
    b.set_defaults(fn=cmd_bench)
    c = sub.add_parser("catalog", help="list known models")
    c.set_defaults(fn=cmd_catalog)
    args = p.parse_args(argv)
    logging.basicConfig(level=logging.DEBUG if args.verbose else logging.INFO,
                        format="%(asctime)s %(levelname)s %(name)s: %(message)s")
    args.fn(args)

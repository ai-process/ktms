# ktms-mt

The reference machine-translation engine for Keystone (KTMS) nodes: Opus-MT (Marian), M2M100 and
MADLAD-400 on [CTranslate2](https://github.com/OpenNMT/CTranslate2), served over the small HTTP
contract KTMS calls on a node started with `--engine mt`. Any server that implements the same
contract can stand in for it.

## Contract

`GET /v1/models`

```json
{"object": "list", "data": [{"id": "opus-mt-de", "object": "model", "capabilities": ["translate"],
  "languages": {"source": ["*"], "target": ["de"]}}]}
```

`POST /v1/translate`

```json
{"model": "opus-mt-de", "texts": ["Great location."], "source_language": "", "target_language": "de", "format": "text"}
```

```json
{"model": "opus-mt-de",
 "translations": [{"text": "Tolle Lage.", "detected_source_language": "en"}],
 "usage": {"input_tokens": 5, "output_tokens": 6, "characters": 15}}
```

One translation per text, in order. An empty `source_language` means detect each text. Errors are
`{"error": {"code", "message"}}`: 400 `bad_request` or `unsupported_language_pair` (KTMS moves on
to the next model of the chain), 401, 404 `model_not_found` (also moves on), 500 `engine_error`.
A batch is translated whole or not at all: one text in a language the bundle has no model for
fails the call, and KTMS sends the batch to the next model. Bundles with a `fallback`, such as
`opus-mt-de`, never hit this on detected languages. `GET /health` needs no key. With `KTMS_MT_API_KEY` (or `api_key` in the config) set, the other
routes need `Authorization: Bearer <key>`; give the node daemon the same key in `NODE_AUTH_TOKEN`.

## Install

On a node, `install-node.sh --engine mt --model opus-mt-de` does all of this: a venv, the models,
a LaunchAgent or systemd user service on `127.0.0.1:11436`, and the node daemon. By hand:

```bash
python3.12 -m venv ~/.ktms-mt/venv
~/.ktms-mt/venv/bin/pip install 'ktms-mt[convert] @ git+https://github.com/ai-process/ktms#subdirectory=mt'
~/.ktms-mt/venv/bin/ktms-mt fetch opus-mt-de      # downloads, converts to int8, writes ~/.ktms-mt/config.toml
~/.ktms-mt/venv/bin/ktms-mt serve                 # http://127.0.0.1:11436
```

Python 3.11–3.13. `fetch` needs the `convert` extra (Transformers and PyTorch) only to convert
checkpoints; `serve` needs CTranslate2, SentencePiece and fastText. It also downloads fastText's
`lid.176.ftz` (CC-BY-SA 3.0) for language detection.

## Models

`ktms-mt catalog` lists them. A served model routes each text by its language:

| Model | Target | Engines |
| --- | --- | --- |
| `opus-mt-de` | de | `opus-mt-{en,fr,es,it}-de`, every other language through `opus-mt-mul-de` |
| `opus-mt-mul-de` | de | the multilingual model alone |
| `opus-mt-<xx>-de` | de | one direct pair (en, fr, es, it, pl, da, cs, fi, hu, uk, ja, zh, ko, ar, bg, ca, et, lt, no, vi) |
| `m2m100-418m` | any | M2M100 418M, any of its 100 languages to any |
| `madlad400-3b` | any | MADLAD-400 3B, 400+ languages |

`opus-mt-mul-de` is `Helsinki-NLP/opus-mt-tc-bible-big-mul-deu_eng_nld` steered by `>>deu<<`:
there is no `opus-mt-mul-de` or `opus-mt-nl-de` upstream, so Dutch and every language without a
direct pair go through it. A text already in the target language comes back unchanged, and blank
texts are never sent to a model.

Licenses: Opus-MT CC-BY 4.0, M2M100 MIT, MADLAD-400 Apache 2.0 — all usable commercially. NLLB-200
and SeamlessM4T are CC-BY-NC (non-commercial): `ktms-mt fetch` refuses them.

Your own combination goes in the config, no code:

```toml
serve = ["opus-mt-de"]

[server]
port = 11436
device = "cpu"          # or "mps": CTranslate2's experimental Apple GPU backend (float16)
compute_type = "int8"
threads = 0             # 0 = every core
max_concurrency = 2     # batches translated at once
max_batch_tokens = 4096
max_segment_tokens = 200
beam_size = 1           # 2 is slightly better and half as fast

[models.reviews-de]
target = "de"
pairs = { en = "opus-mt-en-de", nl = "opus-mt-mul-de" }
fallback = "m2m100-418m"
```

Long texts are split by line and sentence into segments of up to `max_segment_tokens` and put
back together; `format: html` translates text nodes and keeps tags, `script` and `style` as they are.

## Throughput

`ktms-mt bench FILE --batch 64` translates a file, one text per line. Measured numbers for a Mac
are in the KTMS README, section Translation.

## Tests

```bash
cd mt && python3 -m unittest discover -s tests -t .
```

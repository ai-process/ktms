"""CTranslate2 engines and the translator that routes texts between them."""

import html
import os
import re
import threading
import unicodedata
from dataclasses import dataclass

from .catalog import Bundle

# A sentence ends after its closing quotes and brackets. Japanese and Chinese
# put no space after 。！？, so the separator there may be empty.
CLOSERS = "\"'”’)\\]」』）〉》】〕"
SENTENCE_END = re.compile(
    rf"[.!?…]+[{CLOSERS}]*(\s+)|(?:[。！？｡]|．(?![0-9０-９])|……)+[{CLOSERS}]*(\s*)")
CLAUSE_MARKS = "、，；：,;:"
# Targets written without spaces between words keep synthetic cuts glued.
UNSPACED = {"ja", "zh", "yue", "wuu", "th", "lo", "km", "my", "bo", "dz"}
WORDS = re.compile(r"(\s+)")
TAG = re.compile(r"(<[^>]*>)")
RAW_TEXT_TAGS = re.compile(r"<(script|style)\b", re.I)


class UnsupportedPair(Exception):
    pass


class BadRequest(Exception):
    pass


class ModelNotFound(Exception):
    pass


@dataclass
class Segment:
    """One piece of one text sent to an engine."""
    text_index: int
    text: str
    pieces: list
    out: str = ""


class Engine:
    """One converted model directory, loaded lazily and kept."""

    def __init__(self, name, path, kind, source_prefix="", device="cpu", compute_type="int8",
                 inter_threads=1, intra_threads=0, beam_size=2, max_batch_tokens=4096):
        self.name, self.path, self.kind, self.source_prefix = name, path, kind, source_prefix
        self.device, self.compute_type = device, compute_type
        self.inter_threads, self.intra_threads = inter_threads, intra_threads
        self.beam_size, self.max_batch_tokens = beam_size, max_batch_tokens
        self._lock = threading.Lock()
        self._translator = None

    def load(self):
        with self._lock:
            if self._translator is not None:
                return
            import ctranslate2
            import sentencepiece as spm
            self._translator = ctranslate2.Translator(
                self.path, device=self.device, compute_type=self.compute_type,
                inter_threads=self.inter_threads, intra_threads=self.intra_threads)
            if self.kind == "marian":
                self._src = spm.SentencePieceProcessor(model_file=os.path.join(self.path, "source.spm"))
                self._tgt = spm.SentencePieceProcessor(model_file=os.path.join(self.path, "target.spm"))
            elif self.kind == "m2m100":
                self._src = self._tgt = spm.SentencePieceProcessor(model_file=os.path.join(self.path, "sentencepiece.bpe.model"))
            elif self.kind == "madlad":
                self._src = self._tgt = spm.SentencePieceProcessor(model_file=os.path.join(self.path, "spiece.model"))
            else:
                raise ValueError(f"engine {self.name}: unknown kind {self.kind}")

    @property
    def loaded(self):
        return self._translator is not None

    def tokenize(self, text):
        self.load()
        return self._src.encode(text, out_type=str)

    def translate(self, segments, source, target):
        """Fills seg.out; returns (input tokens, output tokens)."""
        self.load()
        batch, prefix = [], None
        for seg in segments:
            toks = list(seg.pieces)
            if self.kind == "marian":
                toks = ([self.source_prefix] if self.source_prefix else []) + toks + ["</s>"]
            elif self.kind == "m2m100":
                toks = [f"__{source}__"] + toks + ["</s>"]
                prefix = [f"__{target}__"]
            elif self.kind == "madlad":
                toks = [f"<2{target}>"] + toks + ["</s>"]
            batch.append(toks)
        results = self._translator.translate_batch(
            batch, target_prefix=[prefix] * len(batch) if prefix else None,
            max_batch_size=self.max_batch_tokens, batch_type="tokens",
            beam_size=self.beam_size, max_decoding_length=512)
        n_in = sum(len(b) for b in batch)
        n_out = 0
        for seg, res in zip(segments, results):
            out = res.hypotheses[0]
            if prefix and out[:1] == prefix:
                out = out[1:]
            n_out += len(out)
            seg.out = self._tgt.decode(out).strip()
        return n_in, n_out


def split_text(text, tokenize, max_tokens=200):
    """Lines, then sentences (then words, for a run-on sentence), grouped up to
    max_tokens. Returns (pieces, joiners): the text is "".join(p + j) of the
    translated pieces and these joiners, so whitespace between pieces survives."""
    pieces, joiners = [], []

    def gap(s):
        if pieces:
            joiners[-1] += s
        else:
            pieces.append("")
            joiners.append(s)

    lines = text.split("\n")
    for li, line in enumerate(lines):
        end = "\n" if li < len(lines) - 1 else ""
        lead, body, trail = edge_space(line)
        if not body:
            gap(line + end)
            continue
        if lead:
            gap(lead)
        units = []  # (text, whitespace after it, tokens)
        for sent, sep in sentences(body):
            units.extend(fit(sent, sep, tokenize, max_tokens))
        group, n_group = [], 0
        for u in units:
            if group and n_group + u[2] > max_tokens:
                pieces.append("".join(t + s for t, s, _ in group[:-1]) + group[-1][0])
                joiners.append(group[-1][1])
                group, n_group = [], 0
            group.append(u)
            n_group += u[2]
        pieces.append("".join(t + s for t, s, _ in group[:-1]) + group[-1][0])
        joiners.append(group[-1][1] + trail + end)
    return pieces, joiners


def sentences(body):
    """(sentence, whitespace after it) pairs covering body."""
    out, start = [], 0
    for m in SENTENCE_END.finditer(body):
        g = 1 if m.group(1) is not None else 2
        if m.end() == len(body) and not m.group(g):
            break
        out.append((body[start:m.start(g)], m.group(g)))
        start = m.end()
    if start < len(body):
        out.append((body[start:], ""))
    return out


def fit(sent, sep, tokenize, max_tokens):
    """A sentence longer than max_tokens split at word boundaries: the engine
    truncates past its input limit, which would drop text silently."""
    n = len(tokenize(sent))
    if n <= max_tokens:
        return [(sent, sep, n)]
    out, cur, n_cur = [], [], 0
    words = WORDS.split(sent)
    for k in range(0, len(words), 2):
        w, space = words[k], words[k + 1] if k + 1 < len(words) else ""
        nw = len(tokenize(w)) if w else 0
        if cur and n_cur + nw > max_tokens:
            out.append(("".join(cur[:-1]), cur[-1], n_cur))
            cur, n_cur = [], 0
        if nw > max_tokens:
            *heads, (w, nw) = chop(w, tokenize, max_tokens)
            out.extend((c, "", m) for c, m in heads)
        cur += [w, space]
        n_cur += nw
    if cur:
        out.append(("".join(cur[:-1]), sep, n_cur))
    return out


def chop(word, tokenize, max_tokens):
    """A word longer than max_tokens (a run without spaces: Japanese, Chinese,
    a URL) cut by characters; past ~512 tokens the model fails the whole call.
    Only a bounded window is tokenized per cut, so a huge run stays linear."""
    out, cap = [], max_tokens * 8
    while word:
        window = word[:cap]
        n = len(tokenize(window))
        if n <= max_tokens:
            if len(word) <= cap:
                out.append((word, n))
                return out
            size = cap
        else:
            size = max(1, len(window) * max_tokens // n)
            while size > 1 and len(tokenize(word[:size])) > max_tokens:
                size = size * 3 // 4
        size = cut_point(word, size)
        out.append((word[:size], len(tokenize(word[:size]))))
        word = word[size:]
    return out


def cut_point(word, size):
    """Moves a cut back to just after a clause mark in the window's second half,
    else off a grapheme cluster (combining marks, ZWJ, skin tones, flags)."""
    for j in range(size - 1, size // 2 - 1, -1):
        if word[j] in CLAUSE_MARKS:
            return j + 1
    start = size
    while size > 1 and (extends(word[size]) or word[size - 1] == "\u200d"):
        size -= 1
    if is_ri(word[size]):
        k = size
        while k > 0 and is_ri(word[k - 1]):
            k -= 1
        if (size - k) % 2:
            size -= 1
    return size if size > 0 else start


def extends(ch):
    return (unicodedata.category(ch) in ("Mn", "Mc", "Me") or ch == "\u200d"
            or 0x1F3FB <= ord(ch) <= 0x1F3FF or 0xE0020 <= ord(ch) <= 0xE007F)


def is_ri(ch):
    return 0x1F1E6 <= ord(ch) <= 0x1F1FF


class Translator:
    """Routes each text of a request to an engine of a served model."""

    def __init__(self, models, engines, detect, max_segment_tokens=200):
        self.models = models  # id -> Bundle
        self.engines = engines  # name -> Engine
        self.detect = detect  # text -> language code
        self.max_segment_tokens = max_segment_tokens

    def pick(self, bundle: Bundle, source, target):
        if bundle.target not in ("*", target):
            raise UnsupportedPair(f"this model translates into {bundle.target}, not {target}")
        if bundle.any_engine and source in ("", "und"):
            # Many-to-many models need the source tag; there is no fallback to send it to.
            raise UnsupportedPair("could not detect the source language; pass source_language")
        name = bundle.any_engine or bundle.pairs.get(source) or bundle.fallback
        if not name:
            raise UnsupportedPair(f"no model for {source} to {target}")
        return self.engines[name]

    def translate(self, model_id, texts, source, target, fmt="text"):
        bundle = self.models.get(model_id)
        if bundle is None:
            raise ModelNotFound(model_id)
        if not target:
            raise BadRequest("target_language is required")
        target = primary(target)
        source = primary(source) if source else ""
        out = [{"text": t, "detected_source_language": source} for t in texts]
        jobs = {}  # (engine, src) -> [(text index, part index)]
        parts = []  # per text: list of [piece, joiner, translate?]
        for i, text in enumerate(texts):
            plain = strip_tags(text) if fmt == "html" else text
            lang = source
            if not lang and plain.strip():
                lang = self.detect(plain)
                out[i]["detected_source_language"] = lang
            pieces = html_parts(text) if fmt == "html" else [[text, "", True]]
            parts.append(pieces)
            if not plain.strip() or lang == target:
                for p in pieces:
                    p[2] = False
                continue
            engine = self.pick(bundle, lang, target)
            for pi, p in enumerate(pieces):
                if p[2] and p[0].strip():
                    jobs.setdefault((engine.name, lang), []).append((i, pi))
                else:
                    p[2] = False
        n_in = n_out = 0
        for (name, lang), refs in jobs.items():
            engine = self.engines[name]
            segs, layout = [], []
            for i, pi in refs:
                lead, body, trail = edge_space(parts[i][pi][0])
                chunks, joiners = split_text(html.unescape(body) if fmt == "html" else body,
                                             engine.tokenize, self.max_segment_tokens)
                first = len(segs)
                for c in chunks:
                    segs.append(Segment(i, c, engine.tokenize(c) if c.strip() else []))
                layout.append((i, pi, lead, trail, first, len(chunks), joiners))
            todo = [s for s in segs if s.pieces]
            a, b = engine.translate(todo, lang, target) if todo else (0, 0)
            n_in, n_out = n_in + a, n_out + b
            for i, pi, lead, trail, first, n, joiners in layout:
                body = "".join(
                    (segs[first + k].out if segs[first + k].pieces else segs[first + k].text)
                    + self.joiner(joiners[k], segs, first + k, first + n, target)
                    for k in range(n))
                if fmt == "html":
                    body = html.escape(body, quote=False)
                parts[i][pi][0] = lead + body + trail
        for i, pieces in enumerate(parts):
            if any(p[2] for p in pieces):
                out[i]["text"] = "".join(p[0] for p in pieces)
        usage = {"input_tokens": n_in, "output_tokens": n_out, "characters": sum(len(t) for t in texts)}
        return out, usage

    @staticmethod
    def joiner(j, segs, k, end, target):
        """A cut without whitespace between two translations: each output is
        stripped, so a spaced target needs a space there."""
        if j or k + 1 >= end or target in UNSPACED:
            return j
        if segs[k].pieces and segs[k + 1].pieces:
            return " "
        return j


def primary(tag):
    return tag.split("-")[0].split("_")[0].lower()


def edge_space(s):
    body = s.strip()
    if not body:
        return s, "", ""
    start = s.index(body)
    return s[:start], body, s[start + len(body):]


def strip_tags(text):
    return html.unescape(TAG.sub(" ", text))


def html_parts(text):
    """[piece, "", translate?] for tags and text nodes; script and style kept."""
    out, raw = [], False
    for piece in TAG.split(text):
        if not piece:
            continue
        if TAG.fullmatch(piece):
            out.append([piece, "", False])
            if RAW_TEXT_TAGS.match(piece):
                raw = True
            elif piece.lower().startswith(("</script", "</style")):
                raw = False
        else:
            out.append([piece, "", not raw])
    return out

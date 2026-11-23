"""Contract tests with a fake engine: no models, no CTranslate2 needed."""

import json
import threading
import unittest
import urllib.error
import urllib.request

from ktms_mt.catalog import Bundle
from ktms_mt.config import Config
from ktms_mt.engine import Translator, split_text
from ktms_mt.server import serve


class FakeEngine:
    def __init__(self, name):
        self.name = name
        self.calls = []

    def tokenize(self, text):
        return text.split()

    def translate(self, segments, source, target):
        self.calls.append((source, [s.text for s in segments]))
        for s in segments:
            s.out = f"[{self.name}] {s.text.upper()}"
        n = sum(len(s.pieces) for s in segments)
        return n, n


def detect(text):
    return "fr" if "bonjour" in text.lower() else "nl" if "hallo" in text.lower() else "de" if "guten" in text.lower() else "en"


def make():
    engines = {n: FakeEngine(n) for n in ["en-de", "fr-de", "mul-de"]}
    models = {"opus-mt-de": Bundle(target="de", pairs={"en": "en-de", "fr": "fr-de"}, fallback="mul-de"),
              "opus-mt-en-de": Bundle(target="de", pairs={"en": "en-de"})}
    return Translator(models, engines, detect, max_segment_tokens=4), engines


class TranslatorTest(unittest.TestCase):
    def test_routes_by_detected_language(self):
        tr, eng = make()
        out, usage = tr.translate("opus-mt-de", ["good room", "bonjour", "hallo daar", "guten Tag", "", "  "], "", "de")
        self.assertEqual([o["text"] for o in out],
                         ["[en-de] GOOD ROOM", "[fr-de] BONJOUR", "[mul-de] HALLO DAAR", "guten Tag", "", "  "])
        self.assertEqual([o["detected_source_language"] for o in out], ["en", "fr", "nl", "de", "", ""])
        self.assertEqual(usage["characters"], len("good room") + 7 + 10 + 9 + 2)

    def test_explicit_source_and_unsupported_pair(self):
        tr, _ = make()
        out, _ = tr.translate("opus-mt-en-de", ["hallo"], "en-GB", "de-DE")
        self.assertEqual(out[0]["text"], "[en-de] HALLO")
        from ktms_mt.engine import UnsupportedPair
        with self.assertRaises(UnsupportedPair):
            tr.translate("opus-mt-en-de", ["bonjour"], "", "de")
        with self.assertRaises(UnsupportedPair):
            tr.translate("opus-mt-de", ["x"], "en", "fr")

    def test_long_text_split_and_rejoined(self):
        tr, eng = make()
        text = "One two three. Four five six.\n\nSeven eight."
        out, _ = tr.translate("opus-mt-de", [text], "en", "de")
        self.assertEqual(out[0]["text"], "[en-de] ONE TWO THREE. [en-de] FOUR FIVE SIX.\n\n[en-de] SEVEN EIGHT.")
        self.assertEqual(len(eng["en-de"].calls[0][1]), 3)

    def test_html_keeps_tags(self):
        tr, _ = make()
        out, _ = tr.translate("opus-mt-de", ['<p class="x">nice <b>room</b></p><script>var a</script>'], "en", "de", "html")
        self.assertEqual(out[0]["text"], '<p class="x">[en-de] NICE <b>[en-de] ROOM</b></p><script>var a</script>')

    def test_split_text_keeps_layout(self):
        pieces, joiners = split_text("a b. c d.\n\ne", str.split, max_tokens=2)
        self.assertEqual("".join(p + j for p, j in zip(pieces, joiners)), "a b. c d.\n\ne")


class ServerTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        tr, _ = make()
        cls.httpd = serve(Config(port=0, api_key="secret"), tr, warm=False)
        cls.base = f"http://127.0.0.1:{cls.httpd.server_address[1]}"
        threading.Thread(target=cls.httpd.serve_forever, daemon=True).start()

    @classmethod
    def tearDownClass(cls):
        cls.httpd.shutdown()

    def call(self, path, body=None, key="secret"):
        req = urllib.request.Request(self.base + path, data=json.dumps(body).encode() if body is not None else None)
        if key:
            req.add_header("Authorization", "Bearer " + key)
        try:
            with urllib.request.urlopen(req) as r:
                return r.status, json.load(r)
        except urllib.error.HTTPError as e:
            return e.code, json.load(e)

    def test_models(self):
        status, doc = self.call("/v1/models")
        self.assertEqual(status, 200)
        by_id = {m["id"]: m for m in doc["data"]}
        self.assertEqual(by_id["opus-mt-de"]["languages"], {"source": ["*"], "target": ["de"]})
        self.assertEqual(by_id["opus-mt-en-de"]["languages"], {"source": ["en"], "target": ["de"]})
        self.assertEqual(by_id["opus-mt-de"]["capabilities"], ["translate"])

    def test_translate_and_errors(self):
        status, doc = self.call("/v1/translate", {"model": "opus-mt-de", "texts": ["good"], "target_language": "de"})
        self.assertEqual((status, doc["translations"][0]["text"]), (200, "[en-de] GOOD"))
        self.assertEqual(doc["usage"]["characters"], 4)
        status, doc = self.call("/v1/translate", {"model": "opus-mt-en-de", "texts": ["bonjour"], "target_language": "de"})
        self.assertEqual((status, doc["error"]["code"]), (400, "unsupported_language_pair"))
        status, doc = self.call("/v1/translate", {"model": "nope", "texts": ["x"], "target_language": "de"})
        self.assertEqual(status, 404)
        status, doc = self.call("/v1/translate", {"texts": [], "target_language": "de"})
        self.assertEqual((status, doc["error"]["code"]), (400, "bad_request"))
        status, _ = self.call("/v1/translate", {"texts": ["x"], "target_language": "de"}, key="wrong")
        self.assertEqual(status, 401)


class ConfigTest(unittest.TestCase):
    def test_write_then_load(self):
        import os
        import tempfile
        from ktms_mt.config import load, write
        with tempfile.TemporaryDirectory() as d:
            path = os.path.join(d, "config.toml")
            cfg = Config(models_dir=d, port=12000, served=["opus-mt-de"])
            cfg.custom["mine"] = Bundle(target="de", pairs={"en": "opus-mt-en-de"})
            write(path, cfg)
            got = load(path)
            self.assertEqual((got.port, got.served, got.custom["mine"].pairs), (12000, ["opus-mt-de", "mine"], {"en": "opus-mt-en-de"}))


if __name__ == "__main__":
    unittest.main()

"""Language identification with fastText lid.176 (CC-BY-SA 3.0)."""

import os
import threading

LID_URL = "https://dl.fbaipublicfiles.com/fasttext/supervised-models/lid.176.ftz"
LID_FILE = "lid.176.ftz"


class Detector:
    def __init__(self, path):
        if not os.path.exists(path):
            raise FileNotFoundError(f"{path}: run `ktms-mt fetch` to download the language identifier")
        import fasttext
        fasttext.FastText.eprint = lambda *a, **k: None  # silence its deprecation banner
        self._model = fasttext.load_model(path)
        self._lock = threading.Lock()

    def __call__(self, text):
        line = " ".join(text.split())[:2000]
        # The binding's own predict(); the Python wrapper breaks on NumPy 2.
        with self._lock:
            preds = self._model.f.predict(line + "\n", 1, 0.0, "strict")
        return preds[0][1].replace("__label__", "") if preds else "und"

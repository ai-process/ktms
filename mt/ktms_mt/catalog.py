"""Known models and the bundles install-node.sh offers by name.

An engine is one CTranslate2 model directory. A bundle serves one target
language through several engines, picked by the source language, with a
multilingual engine for every other source.
"""

from dataclasses import dataclass, field


@dataclass(frozen=True)
class Recipe:
    hf: str  # Hugging Face checkpoint to convert
    kind: str  # marian | m2m100 | madlad
    source_prefix: str = ""  # a token the model expects first, e.g. >>deu<<
    license: str = ""
    copy_files: tuple = ()


def _opus(src: str, tgt: str) -> Recipe:
    return Recipe(hf=f"Helsinki-NLP/opus-mt-{src}-{tgt}", kind="marian", license="CC-BY-4.0",
                  copy_files=("source.spm", "target.spm"))


# Direct Opus-MT pairs into German that exist upstream. Dutch has none: it goes
# through the multilingual model.
_DE_SOURCES = ["en", "fr", "es", "it", "pl", "da", "cs", "fi", "hu", "uk", "ja", "zh", "ko", "ar", "bg", "ca", "et", "lt", "no", "vi"]

ENGINES: dict[str, Recipe] = {f"opus-mt-{s}-de": _opus(s, "de") for s in _DE_SOURCES}
# There is no opus-mt-mul-de; the OPUS-MT "tc-bible" mul->deu+eng+nld model is
# the multilingual one, steered to German by its target token.
ENGINES["opus-mt-mul-de"] = Recipe(
    hf="Helsinki-NLP/opus-mt-tc-bible-big-mul-deu_eng_nld", kind="marian", source_prefix=">>deu<<",
    license="CC-BY-4.0", copy_files=("source.spm", "target.spm"))
ENGINES["m2m100-418m"] = Recipe(hf="facebook/m2m100_418M", kind="m2m100", license="MIT",
                                copy_files=("sentencepiece.bpe.model",))
ENGINES["madlad400-3b"] = Recipe(hf="google/madlad400-3b-mt", kind="madlad", license="Apache-2.0",
                                 copy_files=("spiece.model",))
# Non-commercial licenses: listed so nobody installs them by accident.
NON_COMMERCIAL = {"nllb-200": "CC-BY-NC-4.0", "seamless-m4t": "CC-BY-NC-4.0"}


@dataclass
class Bundle:
    target: str
    pairs: dict = field(default_factory=dict)  # source language -> engine
    fallback: str = ""  # engine for any other source
    any_engine: str = ""  # a many-to-many engine (m2m100, madlad): every pair


BUNDLES: dict[str, Bundle] = {
    "opus-mt-de": Bundle(target="de", pairs={s: f"opus-mt-{s}-de" for s in ["en", "fr", "es", "it"]},
                         fallback="opus-mt-mul-de"),
    "opus-mt-mul-de": Bundle(target="de", fallback="opus-mt-mul-de"),
    "m2m100-418m": Bundle(target="*", any_engine="m2m100-418m"),
    "madlad400-3b": Bundle(target="*", any_engine="madlad400-3b"),
}
for _s in _DE_SOURCES:
    BUNDLES.setdefault(f"opus-mt-{_s}-de", Bundle(target="de", pairs={_s: f"opus-mt-{_s}-de"}))


def engines_of(bundle: Bundle) -> list[str]:
    names = list(bundle.pairs.values())
    for extra in (bundle.fallback, bundle.any_engine):
        if extra and extra not in names:
            names.append(extra)
    return names

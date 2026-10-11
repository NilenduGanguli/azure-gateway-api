#!/usr/bin/env python3
"""Drive the gateway with the real Azure SDKs.

This is the proof that matters. Every other test asserts what the gateway *should* emit; this one
points unmodified, officially published client libraries at it and checks they work — including
the poller internals that no amount of reading the spec can fully predict.

Usage:
    GATEWAY_URL=http://localhost:8080 ./sdk_test.py
    ./sdk_test.py write-pdf PAGES PATH     # write the multi-page test PDF and exit

The gateway requires no credential, so the key below is a placeholder the SDKs insist on having.

SDK_STRICT=1 turns a skipped check into a failure. scripts/e2e-local.sh sets it: there a missing
SDK means a broken virtualenv, and a run that skipped everything must not read as a pass.
"""

import os
import sys
import time
import uuid

GATEWAY = os.environ.get("GATEWAY_URL", "http://localhost:8080").rstrip("/")
PLACEHOLDER_KEY = "not-required-by-this-gateway"
STRICT = os.environ.get("SDK_STRICT", "") not in ("", "0")

# 23 pages is several parts at any small SPLIT_PAGES (5 cuts it into 5,5,5,4,4) and, being prime,
# never a whole number of equal parts, so a merge that drops, repeats or misorders a part, or
# mishandles a shorter one, changes the page count or the numbering.
LARGE_PAGES = 23

# Long enough for a real container to analyse a 23-page document; the fakes answer in seconds.
LARGE_TIMEOUT_S = 600

# A small valid PNG that clears Azure's 50x50 minimum: a white 200x120 rectangle.
def sample_png() -> bytes:
    try:
        from PIL import Image
        import io
        buf = io.BytesIO()
        Image.new("RGB", (200, 120), "white").save(buf, format="PNG")
        return buf.getvalue()
    except ImportError:
        # Fall back to a fixed 100x100 white PNG so Pillow stays optional.
        import base64
        return base64.b64decode(
            "iVBORw0KGgoAAAANSUhEUgAAAGQAAABkCAIAAAD/gAIDAAAAWklEQVR4nO3BAQ0AAADCoPdPbQ8H"
            "FAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
            "AAAAAAAAAAAAvBsxAAABraQmUAAAAABJRU5ErkJggg=="
        )


def sample_pdf(pages: int):
    """A PDF of `pages` pages built with Pillow, or None when Pillow is missing.

    Page i is (600+i) x 800 points with "PAGE i" drawn on it. The sizes follow internal/pdftest's
    convention on purpose: cmd/fakeazure's paged fake recovers which original pages a split part
    holds from its MediaBox widths, so against the fakes every part is answered for exactly the
    pages it carries and a misplaced part shows up as wrong content, not just a wrong count. A real
    container ignores the sizes and reads the text.
    """
    try:
        import io
        from PIL import Image, ImageDraw, ImageFont
    except ImportError:
        return None
    try:
        # A scalable default font needs Pillow 10.1; the bitmap fallback is tiny but harmless.
        font = ImageFont.load_default(size=48)
    except TypeError:
        font = ImageFont.load_default()
    images = []
    for i in range(1, pages + 1):
        image = Image.new("RGB", (600 + i, 800), "white")
        ImageDraw.Draw(image).text((72, 72), f"PAGE {i}", fill="black", font=font)
        images.append(image)
    buf = io.BytesIO()
    # resolution=72 makes one pixel one point, so the MediaBox is exactly the image size.
    images[0].save(buf, format="PDF", save_all=True, append_images=images[1:], resolution=72.0)
    return buf.getvalue()


class Result:
    def __init__(self):
        self.passed = []
        self.failed = []

    def ok(self, name, detail=""):
        self.passed.append(name)
        print(f"  PASS  {name}" + (f"  ({detail})" if detail else ""))

    def bad(self, name, detail):
        self.failed.append((name, detail))
        print(f"  FAIL  {name}\n        {detail}")

    def skip(self, name, why):
        if STRICT:
            self.bad(name, f"skipped under SDK_STRICT: {why}")
            return
        print(f"  SKIP  {name}  ({why})")


R = Result()


def test_document_intelligence():
    """azure-ai-documentintelligence: the strictest poller of the four SDK families."""
    print("\nazure-ai-documentintelligence (Python)")
    try:
        from azure.ai.documentintelligence import DocumentIntelligenceClient
        from azure.ai.documentintelligence.models import AnalyzeDocumentRequest  # noqa: F401
        from azure.core.credentials import AzureKeyCredential
    except ImportError as e:
        R.skip("document intelligence", f"pip install azure-ai-documentintelligence ({e})")
        return

    client = DocumentIntelligenceClient(
        endpoint=GATEWAY, credential=AzureKeyCredential(PLACEHOLDER_KEY)
    )

    started = time.time()
    try:
        poller = client.begin_analyze_document(
            "prebuilt-layout",
            body=sample_png(),
            content_type="image/png",
        )
    except Exception as e:  # noqa: BLE001
        R.bad("begin_analyze_document", f"{type(e).__name__}: {e}")
        return
    R.ok("begin_analyze_document accepted the 202")

    # .details reads the operation id straight out of Operation-Location with the hard-coded
    # regex [^:]+://[^/]+/documentintelligence/.+/([^?/]+). If the gateway's URL shape is wrong
    # this raises AttributeError rather than returning anything.
    try:
        details = poller.details
        op_id = details.get("operation_id")
        if not op_id:
            R.bad("poller.details operation id", f"no operation_id in {details}")
        else:
            uuid.UUID(op_id)
            R.ok("poller.details parsed the operation id", op_id)
    except Exception as e:  # noqa: BLE001
        R.bad(
            "poller.details operation id",
            f"{type(e).__name__}: {e} — Operation-Location is missing the literal "
            f"/documentintelligence/ segment or has the wrong shape",
        )

    try:
        result = poller.result()
    except Exception as e:  # noqa: BLE001
        R.bad("poller.result()", f"{type(e).__name__}: {e}")
        return
    R.ok("poller.result() completed", f"{time.time() - started:.1f}s")

    if getattr(result, "model_id", None):
        R.ok("analyzeResult deserialised", f"modelId={result.model_id}")
    else:
        R.bad("analyzeResult deserialised", f"result has no model_id: {result}")

    # A fast gateway that omits Retry-After makes azure-core fall back to a 30s polling interval.
    if time.time() - started > 25:
        R.bad(
            "polling interval",
            "the operation took over 25s, which is the signature of a missing or unparseable "
            "Retry-After making azure-core use its 30s default",
        )
    else:
        R.ok("polling interval honoured Retry-After")


def poll_read(client, operation_id, timeout_s):
    """Poll a Read operation the way Microsoft's sample does, until it leaves notStarted/running.

    Returns (read_result, None), or (None, why) when a call raised or the deadline passed.
    """
    deadline = time.time() + timeout_s
    while time.time() < deadline:
        try:
            read_result = client.get_read_result(operation_id)
        except Exception as e:  # noqa: BLE001
            return None, f"{type(e).__name__}: {e}"
        status = str(read_result.status).lower()
        if "notstarted" in status or "running" in status:
            time.sleep(0.5)
            continue
        return read_result, None
    return None, f"operation never reached a terminal status within {timeout_s}s"


def test_computer_vision_read():
    """azure-cognitiveservices-vision-computervision: no poller; the caller splits the URL."""
    print("\nazure-cognitiveservices-vision-computervision (Python)")
    try:
        from azure.cognitiveservices.vision.computervision import ComputerVisionClient
        from msrest.authentication import CognitiveServicesCredentials
    except ImportError as e:
        R.skip("computer vision read", f"pip install azure-cognitiveservices-vision-computervision ({e})")
        return

    import io

    client = ComputerVisionClient(GATEWAY, CognitiveServicesCredentials(PLACEHOLDER_KEY))

    try:
        response = client.read_in_stream(io.BytesIO(sample_png()), raw=True)
    except Exception as e:  # noqa: BLE001
        R.bad("read_in_stream", f"{type(e).__name__}: {e}")
        return
    R.ok("read_in_stream accepted the 202")

    location = response.headers.get("Operation-Location", "")
    if not location:
        R.bad("Operation-Location", "header absent")
        return

    # This is exactly what Microsoft's canonical sample does. A query string or trailing slash
    # makes operation_id junk, which the SDK then percent-encodes into the path and 404s on.
    operation_id = location.split("/")[-1]
    try:
        uuid.UUID(operation_id)
        R.ok("Operation-Location ends in a bare GUID", operation_id)
    except ValueError:
        R.bad(
            "Operation-Location ends in a bare GUID",
            f"split('/')[-1] gave {operation_id!r}; the header must have no query string, "
            f"no trailing slash and no fragment",
        )
        return

    read_result, err = poll_read(client, operation_id, 60)
    if err:
        R.bad("get_read_result", err)
        return

    if "succeeded" in str(read_result.status).lower():
        R.ok("get_read_result reached succeeded")
    else:
        R.bad("get_read_result", f"terminal status was {read_result.status}")

    # The enum is modelAsString:false, so an unrecognised value degrades to a raw string and the
    # canonical sample's while loop spins forever.
    if isinstance(read_result.status, str) and read_result.status not in (
        "notStarted", "running", "failed", "succeeded",
    ):
        R.bad(
            "status enum",
            f"{read_result.status!r} is outside the closed OperationStatus enum; the canonical "
            f"sample loop would never terminate",
        )
    else:
        R.ok("status is inside the closed OperationStatus enum")


def test_unknown_operation_is_a_clean_404():
    """An expired or unknown id must 404 with a parseable body, exactly as Azure does."""
    print("\nunknown operation handling")
    import json
    import urllib.error
    import urllib.request

    cases = [
        (
            "document intelligence",
            f"{GATEWAY}/documentintelligence/documentModels/prebuilt-layout/analyzeResults/"
            f"00000000-0000-4000-8000-000000000000?api-version=2024-11-30",
            # Shapes below are what the containers were *observed* to return, which is the
            # inverse of what the published references show for each service. The gateway
            # matches the containers, so this test does too.
            lambda b: "error" not in b and b.get("code") == "NotFound" and "message" in b,
            'flat {"code":"NotFound","message":...} with no envelope',
        ),
        (
            "computer vision read",
            f"{GATEWAY}/vision/v3.2/read/analyzeResults/00000000-0000-4000-8000-000000000000",
            lambda b: isinstance(b.get("error"), dict) and "code" in b["error"],
            'wrapped {"error":{"code":...,"message":...}}',
        ),
    ]
    for name, url, predicate, expected in cases:
        try:
            urllib.request.urlopen(url, timeout=10)
            R.bad(f"{name} unknown id", "returned success, expected 404")
        except urllib.error.HTTPError as e:
            if e.code != 404:
                R.bad(f"{name} unknown id", f"status {e.code}, want 404")
                continue
            if e.headers.get("Retry-After"):
                R.bad(
                    f"{name} unknown id",
                    "404 carries Retry-After; azure-core retries any >=400 with that header "
                    "up to ten times",
                )
                continue
            try:
                body = json.loads(e.read())
            except Exception:  # noqa: BLE001
                R.bad(f"{name} unknown id", "404 body is not JSON")
                continue
            if predicate(body):
                R.ok(f"{name} unknown id returns {expected}")
            else:
                R.bad(f"{name} unknown id", f"body {body} is not {expected}")
        except Exception as e:  # noqa: BLE001
            R.bad(f"{name} unknown id", f"{type(e).__name__}: {e}")


def expect_pages(name, numbers, detail=""):
    """Pass when the result's page numbers are exactly 1..LARGE_PAGES, in order."""
    want = list(range(1, LARGE_PAGES + 1))
    if numbers == want:
        R.ok(f"{name}: {LARGE_PAGES} pages numbered 1..{LARGE_PAGES}", detail)
    else:
        R.bad(name, f"page numbers {numbers}, want 1..{LARGE_PAGES}")


def spans_out_of_order(spans, content_len):
    """Why the pages' spans are not ascending, disjoint and inside content, or None if they are.

    spans holds one list of (offset, length) per page. Azure lays content out page by page, so a
    whole-document result always passes; a merge that forgot to shift a later part's offsets
    restarts them at 0 and fails. Offsets may count text elements rather than code points, which
    only makes the content bound looser, never wrong.
    """
    end = 0
    for page, page_spans in enumerate(spans, start=1):
        for offset, length in page_spans:
            if offset < end:
                return f"page {page} span at {offset} starts before the previous span's end {end}"
            if offset + length > content_len:
                return f"page {page} span {offset}+{length} runs past the content ({content_len})"
            end = offset + length
    return None


def test_document_intelligence_large_pdf():
    """A multi-page PDF through begin_analyze_document: split, merged, and accepted by the SDK."""
    print(f"\nazure-ai-documentintelligence, {LARGE_PAGES}-page PDF")
    try:
        from azure.ai.documentintelligence import DocumentIntelligenceClient
        from azure.core.credentials import AzureKeyCredential
    except ImportError as e:
        R.skip("document intelligence large PDF", f"pip install azure-ai-documentintelligence ({e})")
        return
    pdf = sample_pdf(LARGE_PAGES)
    if pdf is None:
        R.skip("document intelligence large PDF", "pip install pillow to build the PDF")
        return

    client = DocumentIntelligenceClient(
        endpoint=GATEWAY, credential=AzureKeyCredential(PLACEHOLDER_KEY)
    )
    started = time.time()
    try:
        poller = client.begin_analyze_document(
            "prebuilt-layout", body=pdf, content_type="application/pdf"
        )
        result = poller.result(timeout=LARGE_TIMEOUT_S)
    except Exception as e:  # noqa: BLE001
        R.bad("large PDF begin_analyze_document", f"{type(e).__name__}: {e}")
        return
    if result is None:
        R.bad("large PDF begin_analyze_document", f"no result within {LARGE_TIMEOUT_S}s")
        return

    pages = result.pages or []
    expect_pages(
        "large PDF via begin_analyze_document",
        [p.page_number for p in pages],
        f"{time.time() - started:.1f}s",
    )
    why = spans_out_of_order(
        [[(s.offset, s.length) for s in (p.spans or [])] for p in pages], len(result.content or "")
    )
    if why:
        R.bad("large PDF page spans", why)
    else:
        R.ok("large PDF page spans are ascending and inside the merged content")


def test_read_large_pdf():
    """The same PDF through the Read client: read_in_stream, then the sample's polling loop."""
    print(f"\nazure-cognitiveservices-vision-computervision, {LARGE_PAGES}-page PDF")
    try:
        from azure.cognitiveservices.vision.computervision import ComputerVisionClient
        from msrest.authentication import CognitiveServicesCredentials
    except ImportError as e:
        R.skip("computer vision read large PDF", f"pip install azure-cognitiveservices-vision-computervision ({e})")
        return
    pdf = sample_pdf(LARGE_PAGES)
    if pdf is None:
        R.skip("computer vision read large PDF", "pip install pillow to build the PDF")
        return

    import io

    client = ComputerVisionClient(GATEWAY, CognitiveServicesCredentials(PLACEHOLDER_KEY))
    started = time.time()
    try:
        response = client.read_in_stream(io.BytesIO(pdf), raw=True)
    except Exception as e:  # noqa: BLE001
        R.bad("large PDF read_in_stream", f"{type(e).__name__}: {e}")
        return
    operation_id = response.headers.get("Operation-Location", "").split("/")[-1]
    read_result, err = poll_read(client, operation_id, LARGE_TIMEOUT_S)
    if err:
        R.bad("large PDF get_read_result", err)
        return
    if "succeeded" not in str(read_result.status).lower():
        R.bad("large PDF get_read_result", f"terminal status was {read_result.status}")
        return
    analyzed = read_result.analyze_result
    expect_pages(
        "large PDF via read_in_stream",
        [r.page for r in (analyzed.read_results if analyzed else None) or []],
        f"{time.time() - started:.1f}s",
    )


def test_sync_analyze_large_pdf():
    """The synchronous routes with a raw urllib POST: no SDK calls them, but clients do."""
    print(f"\nsynchronous routes, {LARGE_PAGES}-page PDF (urllib)")
    import json
    import urllib.error
    import urllib.request

    pdf = sample_pdf(LARGE_PAGES)
    if pdf is None:
        R.skip("synchronous large PDF", "pip install pillow to build the PDF")
        return

    cases = [
        (
            "document intelligence :syncAnalyze",
            f"{GATEWAY}/documentintelligence/documentModels/prebuilt-layout:syncAnalyze"
            f"?api-version=2024-11-30",
            lambda r: [p.get("pageNumber") for p in r.get("pages") or []],
            True,
        ),
        (
            "computer vision read syncAnalyze",
            f"{GATEWAY}/vision/v3.2/read/syncAnalyze",
            lambda r: [p.get("page") for p in r.get("readResults") or []],
            False,
        ),
    ]
    for name, url, numbers_of, has_spans in cases:
        req = urllib.request.Request(
            url, data=pdf, method="POST", headers={"Content-Type": "application/pdf"}
        )
        started = time.time()
        try:
            with urllib.request.urlopen(req, timeout=LARGE_TIMEOUT_S) as resp:
                status, raw = resp.status, resp.read()
        except urllib.error.HTTPError as e:
            R.bad(name, f"status {e.code}: {e.read()[:300]!r}")
            continue
        except Exception as e:  # noqa: BLE001
            R.bad(name, f"{type(e).__name__}: {e}")
            continue
        if status != 200:
            R.bad(name, f"status {status}, want 200")
            continue
        try:
            body = json.loads(raw)
        except ValueError:
            R.bad(name, "the 200 body is not JSON")
            continue
        # The containers answer the synchronous route with the operation envelope, so the result
        # sits under analyzeResult, exactly where a poll would have put it.
        result = body.get("analyzeResult")
        if body.get("status") != "succeeded" or not isinstance(result, dict):
            R.bad(name, f"want a succeeded envelope with analyzeResult, got keys {list(body)}")
            continue
        expect_pages(name, numbers_of(result), f"{time.time() - started:.1f}s")
        if has_spans:
            why = spans_out_of_order(
                [
                    [(s.get("offset", 0), s.get("length", 0)) for s in p.get("spans") or []]
                    for p in result.get("pages") or []
                ],
                len(result.get("content") or ""),
            )
            if why:
                R.bad(f"{name} page spans", why)
            else:
                R.ok(f"{name} page spans are ascending and inside the merged content")


# ------------------------------------------------------------------ strict SDK model round trip
#
# The checks above prove the SDKs accept the gateway's answers. These prove the answers carry
# nothing the SDK models do not know and lack nothing they require: every member of a poll body is
# walked against the SDK's own model, a member it has no attribute for, a missing required member,
# a JSON type the model would not produce (an integer written as a float, a string for a number),
# an enum value outside the model's, or an explicit null is a failure, and the model must give the
# body back unchanged — as_dict() for Document Intelligence, a serialize round trip for Read. A body
# that passes is one a generated model reads without parking anything in additional_properties.

import datetime as _dt
import enum
import json
import re
import typing

_RFC3339 = re.compile(r"^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})$")


def _json_type(v):
    if v is None:
        return "null"
    if isinstance(v, bool):
        return "bool"
    return {int: "int", float: "float", str: "str", list: "array", dict: "object"}.get(type(v), type(v).__name__)


def _diff(a, b, path, out):
    """Differences between the input a and its round trip b, as (path, detail)."""
    if isinstance(a, dict) and isinstance(b, dict):
        for k in a:
            if k not in b:
                out.append((f"{path}.{k}", "lost in the round trip"))
            else:
                _diff(a[k], b[k], f"{path}.{k}", out)
        for k in b:
            if k not in a:
                out.append((f"{path}.{k}", "added by the round trip"))
    elif isinstance(a, list) and isinstance(b, list):
        if len(a) != len(b):
            out.append((path, f"{len(a)} items became {len(b)}"))
        for i, (x, y) in enumerate(zip(a, b)):
            _diff(x, y, f"{path}[{i}]", out)
    elif a != b or _json_type(a) != _json_type(b) and not (
            {_json_type(a), _json_type(b)} <= {"int", "float"} and a == b):
        out.append((path, f"{a!r:.60} became {b!r:.60}"))


def _di_models():
    """azure-ai-documentintelligence's model base and models, wherever this version keeps them."""
    from azure.ai.documentintelligence import models
    try:
        from azure.ai.documentintelligence import _model_base as base
    except ImportError:
        from azure.ai.documentintelligence._utils import model_base as base
    return base, models


def _di_strict(base, models, cls, data, path, bad, _hints={}):
    """Walk data against the typespec model cls, recording every departure in bad."""
    if cls not in _hints:
        cls.__new__(cls)  # computes _attr_to_rest_field
        hints = typing.get_type_hints(cls, globalns=vars(__import__(cls.__module__, fromlist=["_"])),
                                      localns={"_models": models})
        _hints[cls] = {rf._rest_name: hints.get(attr, typing.Any)
                       for attr, rf in cls._attr_to_rest_field.items()}
    fields = _hints[cls]
    for k in data:
        if k not in fields:
            bad.append((f"{path}.{k}", f"{cls.__name__} has no member {k!r}"))
    for name, hint in fields.items():
        if name not in data:
            if not (typing.get_origin(hint) is typing.Union and type(None) in typing.get_args(hint)):
                bad.append((f"{path}.{name}", f"{cls.__name__}.{name} is required"))
            continue
        _di_value(base, models, data[name], hint, f"{path}.{name}", bad)


def _di_value(base, models, value, hint, path, bad):
    origin, args = typing.get_origin(hint), typing.get_args(hint)
    if origin is typing.Union:
        if value is None:
            bad.append((path, "an explicit null, where the service leaves the member out"))
            return
        rest = [a for a in args if a is not type(None)]
        enums = [a for a in rest if isinstance(a, type) and issubclass(a, enum.Enum)]
        if enums and str in rest:
            if not isinstance(value, str) or value not in [e.value for e in enums[0]]:
                bad.append((path, f"{value!r} is not a {enums[0].__name__}"))
            return
        if len(rest) == 1:
            _di_value(base, models, value, rest[0], path, bad)
        return
    if origin is typing.Literal:
        if value not in args:
            bad.append((path, f"{value!r} is not one of {args}"))
    elif origin in (list, typing.List):
        if not isinstance(value, list):
            bad.append((path, f"an array is expected, not {_json_type(value)}"))
            return
        for i, v in enumerate(value):
            _di_value(base, models, v, args[0], f"{path}[{i}]", bad)
    elif origin in (dict, typing.Dict):
        if isinstance(value, dict):
            for k, v in value.items():
                _di_value(base, models, v, args[1], f"{path}.{k}", bad)
    elif isinstance(hint, type) and issubclass(hint, base.Model):
        if not isinstance(value, dict):
            bad.append((path, f"{hint.__name__} expects an object, not {_json_type(value)}"))
            return
        _di_strict(base, models, hint, value, path, bad)
    elif hint is int:
        if isinstance(value, bool) or not isinstance(value, int):
            bad.append((path, f"an integer is expected, not {_json_type(value)} {value!r:.40}"))
    elif hint is float:
        if isinstance(value, bool) or not isinstance(value, (int, float)):
            bad.append((path, f"a number is expected, not {_json_type(value)}"))
    elif hint is str or hint is bool:
        if not isinstance(value, hint):
            bad.append((path, f"{hint.__name__} is expected, not {_json_type(value)}"))
    elif hint is _dt.datetime:
        if not isinstance(value, str) or not _RFC3339.match(value):
            bad.append((path, f"{value!r} is not an RFC 3339 date-time"))
    elif isinstance(hint, type) and issubclass(hint, enum.Enum):
        if value not in [e.value for e in hint]:
            bad.append((path, f"{value!r} is not a {hint.__name__}"))


def di_round_trip(envelope):
    """Departures of a DI poll body from azure-ai-documentintelligence's models, or [] for none."""
    base, models = _di_models()
    bad = []
    known = {"status", "createdDateTime", "lastUpdatedDateTime", "error", "analyzeResult"}
    for k in envelope:
        if k not in known:
            bad.append((k, f"AnalyzeOperation has no member {k!r}"))
    for k in ("createdDateTime", "lastUpdatedDateTime"):
        _di_value(base, models, envelope.get(k), _dt.datetime, k, bad)
    _di_value(base, models, envelope.get("status"),
              typing.Union[str, models.DocumentIntelligenceOperationStatus], "status", bad)
    result = envelope.get("analyzeResult")
    if result is not None:
        _di_strict(base, models, models.AnalyzeResult, result, "analyzeResult", bad)
        # The poller does exactly this with the body's analyzeResult.
        model = base._deserialize(models.AnalyzeResult, result)
        back = json.loads(json.dumps(model.as_dict(), cls=base.SdkJSONEncoder))
        _diff(result, back, "analyzeResult", bad)
    return bad


def read_round_trip(envelope):
    """Departures of a Read poll body from the Computer Vision SDK's msrest models, or []."""
    from azure.cognitiveservices.vision.computervision import models
    from msrest import Deserializer, Serializer
    from msrest.serialization import Model

    classes = {k: v for k, v in vars(models).items() if isinstance(v, type)}
    bad = []

    def walk(cls, data, path):
        keys = {v["key"]: (attr, v["type"]) for attr, v in cls._attribute_map.items()}
        rules = getattr(cls, "_validation", {}) or {}
        for k in data:
            if k not in keys:
                bad.append((f"{path}.{k}", f"{cls.__name__} has no member {k!r}"))
        for key, (attr, typ) in keys.items():
            if key not in data:
                if rules.get(attr, {}).get("required"):
                    bad.append((f"{path}.{key}", f"{cls.__name__}.{attr} is required"))
                continue
            value(data[key], typ, f"{path}.{key}")

    def value(v, typ, path):
        if v is None:
            bad.append((path, "an explicit null"))
        elif typ.startswith("["):
            if not isinstance(v, list):
                bad.append((path, f"an array is expected, not {_json_type(v)}"))
                return
            for i, item in enumerate(v):
                value(item, typ[1:-1], f"{path}[{i}]")
        elif typ in ("int", "long"):
            if isinstance(v, bool) or not isinstance(v, int):
                bad.append((path, f"an integer is expected, not {_json_type(v)}"))
        elif typ == "float":
            if isinstance(v, bool) or not isinstance(v, (int, float)):
                bad.append((path, f"a number is expected, not {_json_type(v)}"))
        elif typ == "str":
            if not isinstance(v, str):
                bad.append((path, f"a string is expected, not {_json_type(v)}"))
        elif typ == "iso-8601":
            if not isinstance(v, str) or not _RFC3339.match(v):
                bad.append((path, f"{v!r} is not an RFC 3339 date-time"))
        elif typ in classes:
            cls = classes[typ]
            if issubclass(cls, enum.Enum):
                if v not in [e.value for e in cls]:
                    bad.append((path, f"{v!r} is not a {typ}"))
            elif issubclass(cls, Model):
                if isinstance(v, dict):
                    walk(cls, v, path)
                else:
                    bad.append((path, f"{typ} expects an object"))

    walk(models.ReadOperationResult, envelope, "")
    obj = Deserializer(classes)("ReadOperationResult", envelope)
    back = json.loads(json.dumps(Serializer(classes).body(obj, "ReadOperationResult", keep_readonly=True)))
    diffs = []
    _diff(envelope, back, "", diffs)
    # msrest respells a date-time it round-trips (…:16Z becomes …:16.000Z); the value is the same.
    bad += [d for d in diffs if not re.search(r"DateTime$", d[0])]
    return bad


def _poll_raw(url, timeout_s):
    """Poll an operation by urllib until it is terminal; return its JSON body."""
    import urllib.request
    deadline = time.time() + timeout_s
    while time.time() < deadline:
        with urllib.request.urlopen(url, timeout=30) as resp:
            body = json.loads(resp.read())
        if body.get("status") in ("succeeded", "failed"):
            return body
        time.sleep(0.3)
    raise TimeoutError(f"{url} never finished")


def test_strict_sdk_round_trip():
    """The large PDF, split and merged, read strictly through each SDK's own models."""
    print(f"\nstrict SDK model round trip, {LARGE_PAGES}-page PDF")
    import urllib.request

    pdf = sample_pdf(LARGE_PAGES)
    if pdf is None:
        R.skip("strict SDK round trip", "pip install pillow to build the PDF")
        return
    for name, submit, check in (
        ("azure-ai-documentintelligence AnalyzeResult",
         f"{GATEWAY}/documentintelligence/documentModels/prebuilt-layout:analyze?api-version=2024-11-30",
         di_round_trip),
        ("computer vision ReadOperationResult", f"{GATEWAY}/vision/v3.2/read/analyze", read_round_trip),
    ):
        try:
            req = urllib.request.Request(submit, data=pdf, method="POST",
                                         headers={"Content-Type": "application/pdf"})
            with urllib.request.urlopen(req, timeout=60) as resp:
                location = resp.headers["Operation-Location"]
            body = _poll_raw(location, LARGE_TIMEOUT_S)
        except Exception as e:  # noqa: BLE001
            R.bad(name, f"{type(e).__name__}: {e}")
            continue
        if body.get("status") != "succeeded":
            R.bad(name, f"the operation ended {body.get('status')}")
            continue
        try:
            bad = check(body)
        except ImportError as e:
            R.skip(name, f"the SDK is not installed ({e})")
            continue
        except Exception as e:  # noqa: BLE001
            R.bad(name, f"the SDK could not read the body: {type(e).__name__}: {e}")
            continue
        if bad:
            shown = "; ".join(f"{p}: {d}" for p, d in bad[:5])
            R.bad(name, f"{len(bad)} departures from the SDK model, e.g. {shown}")
        else:
            R.ok(f"{name}: every member known, typed and given back unchanged")


def write_pdf(argv):
    """write-pdf PAGES PATH: write sample_pdf(PAGES) to PATH, for scripts/e2e-local.sh."""
    if len(argv) != 2 or not argv[0].isdigit() or int(argv[0]) < 1:
        print("usage: sdk_test.py write-pdf PAGES PATH", file=sys.stderr)
        return 2
    pdf = sample_pdf(int(argv[0]))
    if pdf is None:
        print("write-pdf needs Pillow: pip install pillow", file=sys.stderr)
        return 2
    with open(argv[1], "wb") as f:
        f.write(pdf)
    return 0


def main():
    print(f"Driving {GATEWAY} with the official Azure SDKs\n" + "=" * 60)
    test_document_intelligence()
    test_computer_vision_read()
    test_unknown_operation_is_a_clean_404()
    test_document_intelligence_large_pdf()
    test_read_large_pdf()
    test_sync_analyze_large_pdf()
    test_strict_sdk_round_trip()

    print("\n" + "=" * 60)
    print(f"{len(R.passed)} passed, {len(R.failed)} failed")
    if R.failed:
        print("\nFailures:")
        for name, detail in R.failed:
            print(f"  - {name}: {detail}")
        return 1
    return 0


if __name__ == "__main__":
    if sys.argv[1:2] == ["write-pdf"]:
        sys.exit(write_pdf(sys.argv[2:]))
    sys.exit(main())

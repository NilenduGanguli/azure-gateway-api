#!/usr/bin/env python3
"""Drive the gateway with the real Azure SDKs.

This is the proof that matters. Every other test asserts what the gateway *should* emit; this one
points unmodified, officially published client libraries at it and checks they work — including
the poller internals that no amount of reading the spec can fully predict.

Usage:
    GATEWAY_URL=http://localhost:8080 ./sdk_test.py

The gateway requires no credential, so the key below is a placeholder the SDKs insist on having.
"""

import os
import sys
import time
import uuid

GATEWAY = os.environ.get("GATEWAY_URL", "http://localhost:8080").rstrip("/")
PLACEHOLDER_KEY = "not-required-by-this-gateway"

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

    deadline = time.time() + 60
    while time.time() < deadline:
        try:
            read_result = client.get_read_result(operation_id)
        except Exception as e:  # noqa: BLE001
            R.bad("get_read_result", f"{type(e).__name__}: {e}")
            return
        status = str(read_result.status).lower()
        if "notstarted" in status or "running" in status:
            time.sleep(0.5)
            continue
        break
    else:
        R.bad("get_read_result", "operation never reached a terminal status within 60s")
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


def main():
    print(f"Driving {GATEWAY} with the official Azure SDKs\n" + "=" * 60)
    test_document_intelligence()
    test_computer_vision_read()
    test_unknown_operation_is_a_clean_404()

    print("\n" + "=" * 60)
    print(f"{len(R.passed)} passed, {len(R.failed)} failed")
    if R.failed:
        print("\nFailures:")
        for name, detail in R.failed:
            print(f"  - {name}: {detail}")
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())

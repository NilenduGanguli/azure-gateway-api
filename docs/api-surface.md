# API-Surface Reference: Byte-Compatible Gateway for Azure AI On-Prem Containers

**Surface A** — Document Intelligence `prebuilt-layout`, `api-version=2024-11-30`
image `mcr.microsoft.com/azure-cognitive-services/form-recognizer/layout-4.0:2024-11-30`, listens `:5000`

**Surface B** — Computer Vision Read v3.2, `model-version=2022-04-30`
image `mcr.microsoft.com/azure-cognitive-services/vision/read:3.2-model-2022-04-30`, listens `:5000`

### Evidence grades used throughout

| Tag | Meaning |
|---|---|
| `[SPEC]` | Azure/azure-rest-api-specs stable swagger (`DocumentIntelligence.json` 2024-11-30; ComputerVision `stable/v3.2/Ocr.json`) |
| `[IMAGE]` | Extracted from the container image itself (`/app/appsettings.api.json`, compiled route literals in `Microsoft.CloudAI.Containers.VDI.Endpoint.Analyze.dll`, shipped XML docs) — the strongest grade here |
| `[DOC]` | learn.microsoft.com / MicrosoftDocs source |
| `[SDK]` | Read from official SDK source (azure-core, azure-ai-documentintelligence, azure-cognitiveservices-vision-computervision, Azure.AI.FormRecognizer, Azure.AI.DocumentIntelligence, azure-sdk-for-java, @azure/core-lro) |
| `[FIELD]` | Reproducible third-party / MS Q&A observation against a real container |
| `[UNVERIFIED]` | Nobody has pinned it; probe listed in §11 |

### Five corrections that override the naive reading of the docs

1. **Both containers have a native synchronous analyze endpoint.** DI's (`:syncAnalyze`) is undocumented and absent from the public swagger, but present in the `layout-4.0:2024-11-30` image's own routing table and compiled routes. See §7.
2. **Multi-replica async is a *configuration* failure, not an architectural one** — and the exact HTTP status/body of a cross-pod poll is **not documented anywhere**. See §8.
3. **SDKs do NOT uniformly poll `Operation-Location` verbatim.** Three of the four families do; two named clients rebuild the URL from their own configured endpoint. See §4.
4. **The two surfaces' error bodies differ in *shape*, not per status code.** Each swagger declares exactly one `default` error response covering every non-2xx. See §6.
5. **The dominant failure mode on both surfaces is HTTP 200**, with `"status":"failed"` in the body. A gateway that switches on HTTP status alone will mis-map every analysis failure.

---

## 1. Endpoint inventory

### 1A. Document Intelligence layout-4.0

Host template `{endpoint}/documentintelligence` (`x-ms-parameterized-host`, `useSchemePrefix: false`) `[SPEC]`. The image *also* registers the legacy `/formrecognizer` family with a different accepted api-version set `[IMAGE]`.

| Method | Exact path template | Purpose | Sync/async | Gateway |
|---|---|---|---|---|
| POST | `/documentintelligence/documentModels/{modelId}:analyze` | Submit analysis (LRO) | **async** → 202 | **YES** |
| GET | `/documentintelligence/documentModels/{modelId}/analyzeResults/{resultId}` | Poll LRO | sync GET, 200 | **YES** |
| DELETE | `/documentintelligence/documentModels/{modelId}/analyzeResults/{resultId}` | Purge result → 204 | sync | **YES** (cheap) |
| POST | `/documentintelligence/documentModels/{modelId}:syncAnalyze` | **Container-only synchronous analyze → 200 + full `analyzeResult`** | **sync** | **YES — this is the gateway's upstream call** `[IMAGE]` |
| POST | `/documentintelligence/documentModels/{modelId}:syncAnalyze/upstreamResult` | Internal container-to-container ("should only be called from another prebuilt FormRecognizer on premises container") | sync | **NO — never expose** `[IMAGE]` |
| GET | `/documentintelligence/documentModels/{modelId}/analyzeResults/{resultId}/pdf` | Searchable PDF (requires `output=pdf`) | sync | passthrough `[UNVERIFIED on layout-4.0]` |
| GET | `/documentintelligence/documentModels/{modelId}/analyzeResults/{resultId}/figures/{figureId}` | Cropped figure PNG (requires `output=figures`) | sync | passthrough `[UNVERIFIED on layout-4.0]` |
| POST | `/documentintelligence/documentModels/{modelId}:analyzeBatch` | Batch over Azure Blob | async | **NO** — Azure-Blob-bound, unusable air-gapped |
| GET/GET/DELETE | `/documentintelligence/documentModels/{modelId}/analyzeBatchResults[/{resultId}]` | Batch results | — | **NO** |
| GET | `/documentintelligence/documentModels` | List models → `PagedDocumentModelDetails` | sync | passthrough |
| GET | `/documentintelligence/documentModels/{modelId}` | `DocumentModelDetails` | sync | passthrough |
| GET | `/documentintelligence/info` | `{"customDocumentModels":{"count":int32,"limit":int32}}` | sync | passthrough |
| GET | `/documentintelligence/operations` · `/operations/{operationId}` | Model-lifecycle LROs only (empty in a layout container) | sync | passthrough |
| POST/DELETE | `/documentintelligence/documentModels:build`, `:compose`, `:authorizeCopy`, `/{id}:copyTo`, `DELETE /{id}` | Custom-model lifecycle | — | **NO** (n/a to layout) |
| * | `/documentintelligence/documentClassifiers/**` | Classifiers | — | **NO** |
| * | `/formrecognizer/documentModels/**` (same shapes) | Legacy v3.x family; accepts only `2022-03-31-preview … 2023-07-31` `[IMAGE]` | mirrors above | **alias only if legacy `azure-ai-formrecognizer` clients matter** (see §10 item 22) |
| GET | `/` | Container home page (HTML) | sync | passthrough |
| GET | `/ready` | Readiness probe; body contains `"ready":"ready"` | sync | **implement** (own health) |
| GET | `/status` | Validates the startup `ApiKey` against billing without consuming a query; body has `apiStatus` / `apiStatusMessage` | sync | **implement** (monitor upstream) |
| GET | `/swagger` | Swagger UI — ground truth for the exact build tag | sync | passthrough (dev only) |
| GET | `/records/usage-logs/` · `/records/usage-logs/{MONTH}/{YEAR}` | Disconnected-mode usage records | sync | passthrough |
| — | `/ContainerLiveness`, `/ContainerStatus`, `/metrics` | **Do not exist on this image.** Zero hits across MS-owned repos and docs. | — | **NO** |

**Accepted `api-version` values per family** `[IMAGE, /app/appsettings.api.json]`:
- `documentintelligence`: `2023-10-31-preview`, `2024-02-29-preview`, `2024-07-31-preview`, `2024-11-30`, `2025-03-31`, `2025-06-08`, `internal`
- `formrecognizer`: `2022-03-31-preview` … `2023-07-31`

Calling `/formrecognizer/...?api-version=2024-11-30` yields an *"api-version is invalid"* error, **not** a 404 `[FIELD]`.

`modelId` for this container: **`prebuilt-layout`**. Path constraints `maxLength: 64`, `pattern: ^[a-zA-Z0-9][a-zA-Z0-9._~-]{1,63}$` `[SPEC]`.

### 1B. Computer Vision Read v3.2

Base path `/vision/v3.2`. The image serves **Read only** — nothing from `ComputerVision.json`.

| Method | Exact path template | Purpose | Sync/async | Gateway |
|---|---|---|---|---|
| POST | `/vision/v3.2/read/analyze` | Submit read (LRO) | **async** → 202 | **YES** |
| GET | `/vision/v3.2/read/analyzeResults/{operationId}` | Poll LRO | sync GET, 200 | **YES** |
| POST | `/vision/v3.2/read/syncAnalyze` | **Container-only synchronous read → 200 + full result graph** | **sync** | **YES — the gateway's upstream call** `[DOC]` |
| GET | `/vision/v3.2/read/operations/{operationId}` | Poll — appears only in the container install doc, which is verbatim stale text copied from the v2.0 article (dated *Fri, 13 Sep 2019*). Migration guide, swagger, and a Dec-2025 field observation of this exact image all say `analyzeResults`. | sync | **Do NOT rely on it. Probe (§11 Q-B1).** If present, alias it. |
| GET | `/` · `/ready` · `/status` · `/swagger` · `/swagger/vision-v3.2-read/swagger.json` | Container control | sync | as above |
| GET | `/records/usage-logs/` · `/records/usage-logs/{MONTH}/{YEAR}` | Disconnected usage | sync | passthrough |
| — | `/computervision/imageanalysis:analyze` (Image Analysis 4.0) | **Not served.** Container logs `No candidates found for the request path '/computervision/imageanalysis:analyze'` → bodyless 404 `[FIELD]` | — | **NO** |
| — | `/vision/v3.2/analyze`, `/describe`, `/detect`, `/tag`, `/ocr`, `/models`, `/generateThumbnail`, `/areaOfInterest` | Not served by the Read image | — | **NO** |
| — | `/vision/v3.1/*`, `/vision/v3.0/*`, `/vision/v2.0/*` aliases | Each version ships as its own image with its own swagger doc name. Not served here. `[INFER, strongly supported]` | — | **NO** |
| — | `/ContainerLiveness`, `/ContainerStatus`, `/metrics` | Do not exist | — | **NO** |

Legacy contrast (do **not** implement — for recognizing misdirected traffic only): the `read:2.0-preview` image used `POST /vision/v2.0/read/core/asyncBatchAnalyze`, `GET /vision/v2.0/read/operations/{id}`, `POST /vision/v2.0/read/core/Analyze`, and emitted status `"Succeeded"` with a capital S.

---

## 2. Request contract

### 2A-1. `POST /documentintelligence/documentModels/{modelId}:analyze`

**Query parameters** `[SPEC]`

| Name | Type | Req | Default | Allowed / constraint |
|---|---|---|---|---|
| `api-version` | string | **yes** | — | `minLength: 1`; container accepts the list in §1A; for this surface `2024-11-30` |
| `pages` | string | no | all pages | `^(\d+(-\d+)?)(,\s*(\d+(-\d+)?))*$`, 1-based, e.g. `1-3,5,7-9` |
| `locale` | string | no | auto-detect | language code (`en`, `fr`) or BCP-47 (`en-US`) |
| `stringIndexType` | enum | no | **`textElements`** | `textElements` \| `unicodeCodePoint` \| `utf16CodeUnit` |
| `features` | array<enum>, `collectionFormat: csv` | no | none | `ocrHighResolution`, `languages`, `barcodes`, `formulas`, `keyValuePairs`, `styleFont`, `queryFields` |
| `queryFields` | array<string>, csv | no | none | requires `features=queryFields` |
| `outputContentFormat` | enum | no | **`text`** | `text` \| `markdown` |
| `output` | array<enum>, csv | no | none | `pdf` \| `figures` |
| `_overload` | string | no | — | **Docs-generation artifact.** Printed by learn (`?_overload=analyzeDocument`), absent from the swagger, never sent by any SDK. The service disambiguates by `Content-Type`. **The gateway must accept and ignore it.** |

`split` is **NOT** a parameter of `documentModels:analyze` — it exists only on `documentClassifiers/{classifierId}:analyze` and as a `DocumentModelDetails` property `[SPEC]`.

**Accepted `Content-Type` values** (swagger `consumes`, exact strings) `[SPEC]`:

```
application/octet-stream
application/pdf
image/jpeg
image/png
image/tiff
image/bmp
image/heif
text/html
application/vnd.openxmlformats-officedocument.wordprocessingml.document
application/vnd.openxmlformats-officedocument.spreadsheetml.sheet
application/vnd.openxmlformats-officedocument.presentationml.presentation
application/json
```

**Body — binary overload:** raw bytes (`{type: string, format: binary}`).

**Body — JSON overload (`Content-Type: application/json`), `AnalyzeDocumentRequest`:**

```jsonc
{
  "urlSource":    "string (format: uri)",  // container fetches the URL itself — unusable air-gapped
  "base64Source": "string (format: byte)"  // base64 of the document bytes
}
```
No `required` list is declared, but the service requires **exactly one** of the two. A blocked fetch surfaces as `InvalidRequest` / inner `ContentSourceNotAccessible` or `OutboundAccessForbidden`.

**Headers**

| Header | Req | Notes |
|---|---|---|
| `Content-Type` | **yes** | one of the list above |
| `Content-Length` | no | **omitted by SDKs for file-like / generator bodies** — the gateway must accept `Transfer-Encoding: chunked` request bodies |
| `Accept` | no | SDKs send `application/json` on the POST; **absent on the poll GET** |
| `Ocp-Apim-Subscription-Key` | no | **The container does not validate it.** *"By default there is no security on the Foundry Tools container API."* Sent by every SDK; accepted and ignored. The gateway is the auth boundary. |
| `x-ms-client-request-id` | no | Declared in the swagger only on the model-management/misc GETs, **not** on `:analyze`. Python/.NET/Java/JS all send it anyway and Python re-injects it verbatim on every poll. Tolerate + echo. |
| `User-Agent`, `traceparent`, `tracestate` | no | forward |
| `Expect: 100-continue` | — | never sent by any of the four SDKs |
| `Accept-Encoding` | no | Python `gzip, deflate`; JS `gzip,deflate`; **.NET sends none** |

### 2A-2. `POST /documentintelligence/documentModels/{modelId}:syncAnalyze` `[IMAGE]`

Route declared once at family level in `/app/appsettings.api.json` as
`"SyncAnalyzePath": "{baseApiEndpoint}/documentModels/{modelId}:syncAnalyze"` — no per-model and no per-api-version gate, so it applies to `2024-11-30`. Compiled literal present in `Microsoft.CloudAI.Containers.VDI.Endpoint.Analyze.dll`; MVC action `AnalyzeController.SynchronousAnalyze`; Microsoft's own batch worker calls it via `VisionBatchConfig.SyncAnalyzeUrlTemplate` = `https://svc--endpoint-analyze.vdi:80/documentintelligence/documentModels/{0}:syncAnalyze`.

**Query parameters, headers and body: assumed identical to `:analyze`** (same controller, same `{baseApiEndpoint}`) — **`[UNVERIFIED]`**, probe Q-A2. Send `api-version=2024-11-30` and the same `Content-Type`/body forms.

### 2A-3. `GET /documentintelligence/documentModels/{modelId}/analyzeResults/{resultId}`

| Param | Type | Req | Notes |
|---|---|---|---|
| `resultId` (path) | string, `format: uuid` | yes | Treat the value as opaque on the wire; **the gateway must mint a canonical 36-char lowercase GUID** (see §10 item 12) |
| `api-version` (query) | string | required in spec | **The gateway must accept it present, absent, or set to any value.** Python does not re-append it; .NET and Java **replace** it with the client's version; JS **appends** it if missing `[SDK]` |

No required headers. **No `Accept` header is sent by azure-core's poller** — do not require one.

`DELETE` on the same path: same params, `→ 204 No Content`.

### 2A-4. Result-file GETs

```
GET .../analyzeResults/{resultId}/pdf?api-version=2024-11-30
    Accept: application/pdf     (SDK default)   produces: application/pdf, application/json
GET .../analyzeResults/{resultId}/figures/{figureId}?api-version=2024-11-30
    Accept: image/png           (SDK default)   produces: image/png, application/json
    figureId: string, no pattern; convention "{pageNumber}.{figureIndex}", figureIndex resets per page
```
Both require the corresponding `output=pdf` / `output=figures` on the original submit; otherwise `NotFound`.

### 2B-1. `POST /vision/v3.2/read/analyze` and `POST /vision/v3.2/read/syncAnalyze`

**Query parameters** `[SPEC for analyze; FIELD-confirmed identical set on syncAnalyze]`

| Name | Type | Req | Default | Allowed |
|---|---|---|---|---|
| `language` | string BCP-47 | no | auto-detect | Spec enum has 73 values but is `modelAsString: true` (**open set**); the 2022-04-30 model supports 164 languages, so the enum is stale. Omit unless forcing. |
| `pages` | array<string>, `collectionFormat: csv` | no | all pages | per-item `^(\d+|\d+-\d+|\d+-|-\d+)$` — open-ended ranges legal: `?pages=1,3-5,8-` |
| `readingOrder` | string | no | **`basic`** | `basic` \| `natural`. No enum in spec, only a default; invalid → `InvalidReadingOrder`. Latin scripts only. |
| `model-version` | string | no | **`latest`** | `^(latest|\d{4}-\d{2}-\d{2})(-preview)?$`. In-container only the baked-in `2022-04-30` exists; behavior for other dates is `[UNVERIFIED]` (probe Q-B4). |
| `overload` | — | — | — | `x-ms-paths` key `/read/analyze?overload=stream` is an **AutoRest disambiguator, not a real parameter**. Do not send; tolerate and ignore. |

**Accepted `Content-Type` values — exactly two are declared** `[SPEC]`:

```
application/json          body: {"url": "<reachable URL>"}      // "url" required
application/octet-stream  body: raw bytes
```

`image/jpeg`, `image/png`, `image/bmp`, `image/tiff`, `application/pdf` are **not declared anywhere in the spec**. Format is sniffed from content, not the header. Explicit image MIME types most likely yield `415` / `UnsupportedMediaType` — `[UNVERIFIED]`, probe Q-B2. **Gateway rule: accept the full DI content-type list on the CV surface too and normalize to `application/octet-stream` upstream**; that is strictly more permissive than any SDK will exercise and costs nothing.

**Headers:** `Ocp-Apim-Subscription-Key` (accepted, unvalidated by the container), `User-Agent` (msrest appends `azure-cognitiveservices-vision-computervision/0.9.0`), `Content-Type` (`application/json; charset=utf-8` for the url form). `x-ms-client-request-id` is **not** sent by the CV Python SDK.

### 2B-2. `GET /vision/v3.2/read/analyzeResults/{operationId}`

Path param `operationId`: `string`, `format: uuid`. **The .NET client's parameter is a `System.Guid`, and the canonical .NET sample does `Guid.Parse(operationLocation.Substring(len - 36))`** — so the id must be a real, canonically-formatted 36-character GUID `[SDK]`. No query params. No required headers.

---

## 3. Response contract

### 3A-1. DI `POST :analyze` → **`202 Accepted`, no body**

| Header (exact spelling) | Format | Contractual? |
|---|---|---|
| `Operation-Location` | absolute URI — see §4 | **YES** `[SPEC]` |
| `Retry-After` | **int32, seconds** — e.g. `1`. **Never an HTTP-date** (§6 note) | **YES** `[SPEC]` |
| `Content-Length` | `0` | Kestrel |
| `Date` | RFC 1123 | Kestrel |
| `Server` | `Kestrel` | Kestrel |
| `apim-request-id` | GUID | **Cloud front-door only. Not in the contract, no evidence the container emits it.** No SDK reads or branches on it; .NET only adds it to `Diagnostics.LoggedHeaderNames`. Emitting it is cosmetic-but-harmless realism. |
| `x-envoy-upstream-service-time` | int ms | Cloud Envoy sidecar only. Zero references in any SDK tree. Do not emit. |
| `Location` | — | **MUST NOT BE EMITTED** — Python and JS will issue an extra final GET to it (§10 item 8) |

Any non-202 → `DocumentIntelligenceErrorResponse` (§6).

### 3A-2. DI `POST :syncAnalyze` → **`200 OK`**, `Content-Type: application/json`

Body: the **full `AnalyzeResult`** returned directly. MS on the sibling 3.1 image: *"The syncAnalyze endpoint is designed to return the result directly in the same call. So normally you should receive a 200 OK with the full response."* `[FIELD]`

⚠️ **Not contractually guaranteed to be synchronous.** Under memory pressure / large documents / high concurrency the layout container has been observed returning **`202 Accepted` + `Operation-Location`** from `:syncAnalyze` instead — MS called 202 *"not standard or documented behavior"* for this route; root cause was container memory, fixed with +8 GB `[FIELD, layout-3.1]`. **The gateway's upstream client must handle both response shapes on this route** (§7).

Exact envelope of the 200 body (whether it is the bare `AnalyzeResult` or the `AnalyzeOperation` wrapper) is **`[UNVERIFIED]` for layout-4.0** — probe Q-A2.

### 3A-3. DI `GET .../analyzeResults/{resultId}` → **`200 OK`**, `application/json`

`AnalyzeOperation` — required: `status`, `createdDateTime`, `lastUpdatedDateTime` `[SPEC]`:

```jsonc
{
  "status": "notStarted|running|failed|succeeded|canceled|skipped",
  "createdDateTime":     "2024-11-30T12:00:00Z",   // date-time, REQUIRED
  "lastUpdatedDateTime": "2024-11-30T12:00:04Z",   // date-time, REQUIRED
  "error":         { /* DocumentIntelligenceError — present when status=failed */ },
  "analyzeResult": { /* AnalyzeResult — present when status=succeeded */ }
}
```

**Response headers on the poll:** `Content-Type: application/json`, `Retry-After: <int seconds>` while non-terminal (see §10 item 6), `Date`, `Server`. **No `Operation-Location`** (or, if emitted, byte-identical to the original — .NET and JS re-read it every poll and switch URLs; Python and Java latch the first value) `[SDK]`.

`AnalyzeResult` — JSON property order as emitted (swagger declaration order):

```jsonc
{
  "apiVersion":      "2024-11-30",                 // REQUIRED
  "modelId":         "prebuilt-layout",            // REQUIRED
  "stringIndexType": "textElements|unicodeCodePoint|utf16CodeUnit",  // REQUIRED
  "contentFormat":   "text|markdown",              // optional
  "content":         "string",                     // REQUIRED — full content in reading order
  "pages":           [DocumentPage],               // REQUIRED
  "paragraphs":      [DocumentParagraph],          // optional
  "tables":          [DocumentTable],              // optional
  "figures":         [DocumentFigure],             // optional
  "sections":        [DocumentSection],            // optional
  "keyValuePairs":   [DocumentKeyValuePair],       // optional (features=keyValuePairs)
  "styles":          [DocumentStyle],              // optional
  "languages":       [DocumentLanguage],           // optional (features=languages)
  "documents":       [AnalyzedDocument],           // optional — absent/empty for prebuilt-layout
  "warnings":        [{code*, message*, target?}]  // optional
}
```

Sub-schemas (`?` = optional) `[SPEC]`:

```
DocumentPage      { pageNumber:int32(min 1); angle?:float(|x|<=180); width?:float(min 0);
                    height?:float(min 0); unit?:"pixel"|"inch"; spans:[DocumentSpan];
                    words?:[DocumentWord]; selectionMarks?:[DocumentSelectionMark];
                    lines?:[DocumentLine]; barcodes?:[DocumentBarcode]; formulas?:[DocumentFormula] }
DocumentSpan      { offset:int32(min 0); length:int32(min 0) }
BoundingRegion    { pageNumber:int32(min 1); polygon:[number] }   // flat x,y,... 8 numbers per quad, clockwise from top-left
DocumentWord      { content:string; polygon?:[number]; span:DocumentSpan; confidence:float 0..1 }
DocumentLine      { content:string; polygon?:[number]; spans:[DocumentSpan] }
DocumentSelectionMark { state:"selected"|"unselected"; polygon?; span; confidence:0..1 }
DocumentBarcode   { kind:QRCode|PDF417|UPCA|UPCE|Code39|Code128|EAN8|EAN13|DataBar|Code93|Codabar|
                          DataBarExpanded|ITF|MicroQRCode|Aztec|DataMatrix|MaxiCode;
                    value:string; polygon?; span; confidence:0..1 }
DocumentFormula   { kind:"inline"|"display"; value:string(LaTeX); polygon?; span; confidence:0..1 }
DocumentParagraph { role?:pageHeader|pageFooter|pageNumber|title|sectionHeading|footnote|formulaBlock;
                    content:string; boundingRegions?:[BoundingRegion]; spans:[DocumentSpan] }
DocumentTable     { rowCount:int32(min 1); columnCount:int32(min 1); cells:[DocumentTableCell];
                    boundingRegions?; spans; caption?:DocumentCaption; footnotes?:[DocumentFootnote] }
DocumentTableCell { kind?:"content"|"rowHeader"|"columnHeader"|"stubHead"|"description" (default "content");
                    rowIndex:int32; columnIndex:int32; rowSpan?:int32=1; columnSpan?:int32=1;
                    content:string; boundingRegions?; spans; elements?:[string] }
DocumentCaption / DocumentFootnote { content:string; boundingRegions?; spans; elements?:[string] }
DocumentFigure    { boundingRegions?; spans; elements?:[string]; caption?; footnotes?; id?:string }
DocumentSection   { spans:[DocumentSpan]; elements?:[string] }
DocumentKeyValuePair    { key:DocumentKeyValueElement; value?:DocumentKeyValueElement; confidence:0..1 }
DocumentKeyValueElement { content:string; boundingRegions?; spans }
DocumentLanguage  { locale:string(BCP-47); spans; confidence:0..1 }
DocumentStyle     { isHandwritten?:bool; similarFontFamily?:string; fontStyle?:"normal"|"italic";
                    fontWeight?:"normal"|"bold"; color?:string; backgroundColor?:string;
                    spans; confidence:0..1 }
AnalyzedDocument  { docType:string; boundingRegions?; spans; fields?:{<name>:DocumentField}; confidence:0..1 }
```

`elements[]` entries are JSON-pointer-ish strings into the same result: `"/paragraphs/15"`, `"/tables/0"`, `"/sections/2"`.

Layout behavioural notes `[DOC]` the gateway must not "helpfully" normalize away:
- `outputContentFormat=markdown` renders **tables as HTML tables** inside `content`, and selection marks as **☒ / ☐** — while the selection-mark element's own `content` still says `:selected:` / `:unselected:`.
- Bounding regions for figures and tables cover **core content only**, excluding caption and footnotes.
- Table analysis is **not supported for XLSX**.
- For DOCX/XLSX/PPTX/HTML, `angle`, `width`/`height`, `unit`, polygons/bounding regions, and the entire `lines` object are **not returned**.
- `figures[].id` convention is `{pageNumber}.{figureIndex}`, `figureIndex` resetting to 1 per page.

### 3A-4. DI result-file GETs

`200`, `Content-Type: application/pdf` (binary, `schema: {type:"file"}`) / `Content-Type: image/png`. On error, `application/json` `DocumentIntelligenceErrorResponse`.

### 3B-1. CV `POST /vision/v3.2/read/analyze` → **`202 Accepted`, empty body**

Exact header dump from the container doc (HTTP/1.1 casing normalized; the doc prints them lowercased HTTP/2-style):

| Header | Value format |
|---|---|
| `Operation-Location` | absolute URI ending in a bare 36-char GUID — see §4 |
| `Content-Length` | `0` |
| `Date` | RFC 1123 |
| `Server` | `Kestrel` |

**No `Retry-After` is documented on the CV 202.** Emit one anyway (integer seconds) — harmless, and Python/JS callers that poll manually benefit. **No `Content-Type`** (empty body). **No `apim-request-id`, no `x-envoy-upstream-service-time`.**

### 3B-2. CV `POST /vision/v3.2/read/syncAnalyze` → **`200 OK`** (implied — the success status is *never stated in any MS doc*; `[UNVERIFIED]`, probe Q-B5)

Body: *"The JSON response object has the same object graph as the asynchronous version."* i.e. the full `ReadOperationResult` including the top-level `status` — not a bare `analyzeResult`. No `Operation-Location`. Production container clients read `resp.analyzeResult.readResults[0]` straight off the 200 without consulting `status`, and branch on **401 and 402** (402 = billing/quota) `[FIELD]`.

### 3B-3. CV `GET /vision/v3.2/read/analyzeResults/{operationId}` → **`200 OK`** for *every* state, terminal or not

```jsonc
{
  "status": "notStarted|running|failed|succeeded",
  "createdDateTime":     "2021-02-04T06:32:08.2752706+00:00",
  "lastUpdatedDateTime": "2021-02-04T06:32:08.7706172+00:00",
  "analyzeResult": {                       // present ONLY on succeeded
    "version":      "3.2.0",
    "modelVersion": "2022-04-30",
    "readResults": [{
      "page": 1, "language": "en", "angle": 2.1243,
      "width": 502, "height": 252, "unit": "pixel",
      "lines": [{
        "boundingBox": [8 numbers], "language": "en", "text": "Tabs vs",
        "appearance": { "style": { "name": "handwriting", "confidence": 0.96 } },
        "words": [{ "boundingBox": [8 numbers], "text": "Tabs", "confidence": 0.933 }]
      }]
    }]
  }
}
```

Field notes the gateway must reproduce faithfully:
- `createdDateTime` / `lastUpdatedDateTime` are **`type: string` with NO `format: date-time`** in the spec. Observed shapes differ materially: `2021-02-04T06:32:08.2752706+00:00` (7-digit fraction, numeric offset) from the container doc vs `2019-10-03T14:32:04.236Z` from the spec example. **Pass the upstream string through verbatim; do not normalize.**
- `analyzeResult.version` is unstable across MS's own docs (`"3.2.0"`, `"3.2"`, `"v3.2"`). **Never branch on it; echo whatever upstream emitted.**
- `modelVersion` is `required` in v3.2's `analyzeResults` but absent from v3.0's required list and from the container doc's own sample. Treat as optional-on-read; emit it.
- `readResults[].language` (page-level BCP-47) and `lines[].language` (present **only** when the line differs from the page) are real optional fields — how multi-language documents are expressed.
- `unit`: closed enum `pixel` | `inch`. **PDF bounding boxes are in inches.**
- `angle`: `[-180, 180)`, clockwise.
- `appearance.style.name`: `other` | `handwriting`, `modelAsString: true` (**open set**). Latin-only; `appearance` is absent otherwise.
- `Line.words` is **required**; `Line.appearance` is not.
- **`ReadOperationResult` has NO `error` property and `analyzeResult` has NO `errors[]` array — in v3.0, v3.1 and v3.2.** A failed read is a 200 with `"status":"failed"` and **zero diagnostics**. There is nothing to map; the gateway must synthesize its own diagnostics from its own upstream logs, and must not invent an `error` field the SDK models will drop anyway.

**Response headers on the poll:** `Content-Type: application/json`, `Date`, `Server: Kestrel`. **The CV SDK requires exactly `200`** — a `202` on the poll is a hard error.

---

## 4. `Operation-Location` semantics

### 4.1 What each surface emits

**Surface A (DI):**

```
Operation-Location: {scheme}://{authority}/documentintelligence/documentModels/{modelId}/analyzeResults/{resultId}?api-version=2024-11-30
```
Spec example verbatim:
```
https://myendpoint.cognitiveservices.azure.com/documentintelligence/documentModels/prebuilt-layout/analyzeResults/3b31320d-8bab-4f88-b19c-2322a7f11034?api-version=2024-11-30
```
Container form `[FIELD, layout-4.0]`:
```
http://localhost:5000/documentintelligence/documentModels/prebuilt-layout/analyzeResults/<guid>?api-version=2024-11-30
```
- The `:analyze` colon-action becomes a plain `/analyzeResults/` path segment.
- **`?api-version=` IS carried.** Confirmed at the config level: `ApiSetting.ResponseApiVersion` — *"Is Operation-Location response api-version. Default is true, but legacy APIs should override to false"* — is never overridden for any 2024-11-30 entry `[IMAGE]`.
- All other query params (`pages`, `features`, `_overload`, `output`, …) are **dropped**.
- The `{authority}` is **whatever the container saw on the inbound request** (Host / Referer). MS's own nginx sample for this container sets `proxy_set_header Host $host:$server_port;` and `proxy_set_header Referer $scheme://$host:$server_port;` and contains **no** `proxy_redirect` — the header rewrite exists precisely because the container mints the URL from those inbound headers `[DOC]`.
- A common real-world proxy break is **scheme only** (TLS terminated at the proxy → container emits `http://`). The published field fix is an nginx `map $upstream_http_operation_location ... "~^http://(.*)$" "https://$1"` plus `add_header 'Operation-Location' $operation_location always;`.

**Surface B (CV):**

```
Operation-Location: {scheme}://{host}:{port}/vision/v3.2/read/analyzeResults/{36-char GUID}
```
**Bare GUID. No query string. No trailing slash. No fragment.**

Three formats circulate; only this one is real:

| Source | Emitted value | Verdict |
|---|---|---|
| Container install doc | `http://localhost:5000/vision/v3.2/read/operations/{guid}` | **Stale v2.0 copy-paste — wrong** |
| Spec example files | `https://{domain}/vision/v3.2/read/{guid}` (sibling file even names the header lowercase `location`) | **Unmaintained — wrong** |
| Field observation on `3.2-model-2022-04-30` + `call-read-api` | `{scheme}://{host}:{port}/vision/v3.2/read/analyzeResults/{guid}` | **Correct** |

⚠️ **CONTAINER BUG — `Operation-Location` is corrupted when the request authority contains the substring `vision`.** Confirmed by an MS moderator against this exact image, Dec 2025. The container rebuilds the callback URL from the inbound Host and performs a naive string operation on `"vision"`, **stripping both the port and the `/vision` path segment**:

```
POST http://azure-vision:5000/vision/v3.2/read/analyze
  -> Operation-Location: http://azure-vision/v3.2/read/analyzeResults/...        (port + /vision gone)
POST http://read-svc.azure-vision.svc.cluster.local:5000/vision/v3.2/read/analyze
  -> Operation-Location: http://read-svc.azure-vision/v3.2/read/analyzeResults/...
POST http://visionazure:5000/vision/v3.2/read/analyze
  -> Operation-Location: http://vision/v3.2/read/analyzeResults/...
```
MS's only remedy is renaming the host. **Hard deployment constraint: no `vision` substring anywhere in the Kubernetes Service name, docker-compose service name, or DNS label of the upstream.** The gateway must never follow this header verbatim regardless.

### 4.2 What each SDK actually does with it

| Client | Reads the header? | Polls the header URL verbatim (host included)? | Re-appends `api-version`? | Terminal-status matching |
|---|---|---|---|---|
| **azure-ai-documentintelligence (Python)** | yes, case-insensitively | **YES.** `OperationResourcePolling._set_async_url_if_present` → `self._async_url = response.headers["operation-location"]`; `get_polling_url()` returns it; `PipelineClientBase.format_url` passes an absolute URL through untouched | **NO** — the gateway must put it in the header itself | `str(status).lower() in {"succeeded","canceled","failed"}` |
| **azure-ai-formrecognizer (Python)** | yes | **YES** — `AnalyzePolling`/`CopyPolling` subclass `OperationResourcePolling` and override only `get_status` | no | as above; also raises early on `failed` by reading `body["analyzeResult"]["errors"]` |
| **Azure.AI.DocumentIntelligence (.NET, 1.x)** | yes | **YES** — Azure.Core `NextLinkOperationImplementation`: absolute non-`file` URI used verbatim; relative resolved against the start URI. **Re-reads the header on every poll and switches URLs.** | **YES — `AppendOrReplaceApiVersion` rewrites/appends** using the api-version extracted from the initial request URI. Naive `IndexOf("api-version")` substring search — never put the literal text `api-version` in a path segment. | `ToLowerInvariant()` vs `{"failed","canceled"}` / `{"succeeded"}` |
| **Azure.AI.FormRecognizer (.NET, 4.x)** | yes, then **discards the URL** | **NO — rebuilds from its own configured endpoint.** `operationLocation.Split('/','?')` → `_resultId = substrs[len-2]`, `_modelId = substrs[len-4]`; poll request starts `uri.Reset(_endpoint); uri.AppendRaw("/formrecognizer", false); ...` | n/a | as above |
| **Azure.AI.FormRecognizer (.NET, 3.1.x)** | yes, then discards | **NO** — `Id = operationLocation.Split('/').Last(); ... GetAnalyzeLayoutResult(new Guid(Id))` | n/a | — |
| **azure-ai-documentintelligence (Java)** | yes | **YES**, but rewrites the query | **YES — `urlBuilder.setQueryParameter("api-version", serviceVersion)` replaces yours on every poll** | `equalsIgnoreCase` vs `NotStarted, InProgress, Running, Failed, Succeeded, Canceled` |
| **@azure/ai-document-intelligence-rest (JS)** | yes (lowercase key) | **YES.** Re-reads `operation-location` on every poll and switches URLs. Relative URLs get joined to the client endpoint. | **YES — `apiVersionPolicy` appends it if absent** on every request incl. the poll | `.includes("succeeded")` / `.includes("fail")` / `["canceled","cancelled"]`, else **running** |
| **azure-cognitiveservices-vision-computervision (Python)** | **NO — no poller exists.** `read()` surfaces the header only via `raw=True` | **NO.** The caller does `headers["Operation-Location"].split("/")[-1]`; `get_read_result(operation_id)` builds `/read/analyzeResults/{operationId}` against `{Endpoint}/vision/v3.2` | n/a | msrest `deserialize_enum` matches case-insensitively; unknown value → raw string → sample's `while` loop spins forever |
| **Microsoft.Azure.CognitiveServices.Vision.ComputerVision (.NET, retired)** | **NO** | **NO** | n/a | `GetReadResultWithHttpMessagesAsync(System.Guid operationId, ...)` — the canonical sample does `operationLocation.Substring(len - 36)` then `Guid.Parse` |

### 4.3 What the gateway MUST rewrite

**Surface A — rewrite the full header, preserving path shape exactly:**

```
Operation-Location: {gateway-public-scheme}://{gateway-public-authority}/documentintelligence/documentModels/{modelId}/analyzeResults/{resultId}?api-version=2024-11-30
```

Non-negotiable constraints, each traceable to a specific client:
1. **Absolute URL.** A relative value makes Python `_urljoin(base, url)` produce `…/documentintelligence/documentintelligence/…`.
2. **The literal path segment `/documentintelligence/` must be present.** Python `_patch.py`, .NET `OperationWithId.cs`, Java `PollingUtils.java`, and JS `pollingHelper.ts` all hard-code `[^:]+://[^/]+/documentintelligence/.+/([^?/]+)` to derive the operation id. Rewriting to `/v1/di/...` yields `AttributeError` (Python), `null` id (.NET/Java), or a thrown `Error` (JS) — *even when polling itself works*.
3. **Keep `{modelId}` 4 segments from the end and `{resultId}` 2 segments from the end, with the `?api-version=…` query present.** Azure.AI.FormRecognizer 4.x counts backwards after `Split('/','?')`; strip the query and `_resultId` becomes the literal string `analyzeResults`.
4. **Emit `?api-version=2024-11-30`.** Python never re-appends it; .NET and Java replace it; JS appends. Emitting it satisfies all four.
5. **Percent-encode everything.** A literal `{` or `}` raises in Python's `str.format()` (`_format_url_section`); an unencoded `{`, space, `|`, or `^` makes Java's `new URI(path)` throw, which silently demotes `canPoll` to `false` and falls through to `LocationPolling`/`StatusCheckPolling`.
6. **Never a redirect.** .NET builds its handler with `AllowAutoRedirect = false` and has no redirect policy — any 3xx is a terminal failure on both POST and poll. Python/msrest refuse 301/302 for non-GET/HEAD. Serve the URL you advertised, at the host you advertised.

**Surface B — rewrite the full header to a bare-GUID URL:**

```
Operation-Location: {gateway-public-scheme}://{gateway-public-authority}/vision/v3.2/read/analyzeResults/{36-char lowercase GUID}
```
No query string, no trailing slash, no fragment, and the id **must** `Guid.Parse`. Both canonical samples (`split("/")[-1]` in Python, `Substring(len-36)` + `Guid.Parse` in .NET) break otherwise: the Python one percent-encodes the junk into the path (`?`→`%3F`, `=`→`%3D`) producing a 404, the .NET one throws.

**Also:** never propagate the upstream container's own `Operation-Location` — it may name an unroutable internal host, or be corrupted by the `vision`-substring bug. Extract the id, discard the rest, mint your own.

---

## 5. Status enum values — exact strings and casing

### Surface A — `DocumentIntelligenceOperationStatus` (6 values, `AnalyzeOperation.status`) `[SPEC]`

```
notStarted   running   failed   succeeded   canceled   skipped
```

- `canceled` — **one L** (US spelling). `skipped` and `canceled` are used by batch/classifier flows.
- **The gateway must emit only:** `notStarted`, `running`, `succeeded`, `failed`.
  - `"cancelled"` (two Ls) is terminal in **JS only** — Python, .NET and Java poll forever.
  - `"skipped"` is terminal in **none** of them.
  - `"Complete"`, `"Done"`, `"finished"` are terminal in none.
- Casing is safe everywhere (Python `.lower()`, .NET `ToLowerInvariant()`, Java `equalsIgnoreCase`, JS `toLocaleLowerCase()`), but there is no reason to deviate: emit the camelCase strings exactly.

### Surface B — `OperationStatus` (4 values, `modelAsString: false` — a genuinely closed set) `[SPEC]`

```
notStarted   running   failed   succeeded
```

Identical in v3.0, v3.1, v3.2. **Two casing traps:**
- The **v2.0** container emitted `"Succeeded"` with a capital S. Any code that ever spoke to a 2.x container has a wrong comparison for 3.2. Do not emit the capitalized form.
- The documented **`syncAnalyze` error body uses `"Failed"` with a capital F** — so capital-`Failed` and lowercase-`failed` can both legitimately appear from the same container on different routes. Compare case-insensitively when consuming; emit the exact documented casing per route when producing.

### Cross-surface gateway vocabulary

| Internal job state | Surface A emits | Surface B emits |
|---|---|---|
| accepted, not started | `notStarted` | `notStarted` |
| in flight | `running` | `running` |
| done | `succeeded` | `succeeded` |
| terminal error | `failed` | `failed` |

Never surface `canceled` / `skipped` to a client, even if you use them internally.

---

## 6. Error contract

**Structural fact that overrides the "per status code" framing:** each swagger declares exactly **one** `default` error response covering every non-2xx status, on every operation. There is no 400-shape vs 404-shape vs 429-shape. The mapping the gateway needs is **per surface**, plus a code→status table for choosing the right `code` string.

### 6A. Surface A — Document Intelligence: WRAPPED

`DocumentIntelligenceErrorResponse`, `required: [error]`:

```jsonc
{
  "error": {                            // REQUIRED
    "code":    "string",                // REQUIRED
    "message": "string",                // REQUIRED
    "target":  "string",                // optional
    "details": [ /* recursive DocumentIntelligenceError[] */ ],   // optional
    "innererror": {                     // optional — DocumentIntelligenceInnerError
      "code":    "string",              // optional
      "message": "string",              // optional  (note: DI's innererror HAS message, unlike stock Azure.Core)
      "innererror": { /* recursive, unbounded nesting */ }
    }
  }
}
```
Only `code` and `message` are required. Real 4xx bodies routinely carry just `{"error":{"code":...,"message":...,"innererror":{...}}}` with no `target` and no `details`. Defensive parsers should cap `innererror` recursion at ~3 levels.

**The identical `error` object is ALSO returned at HTTP 200 inside `AnalyzeOperation`** when `status = "failed"`:

```jsonc
{ "status": "failed",
  "createdDateTime": "2021-07-14T10:17:51Z",
  "lastUpdatedDateTime": "2021-07-14T10:17:51Z",
  "error": {
    "code": "InternalServerError",
    "message": "An unexpected error occurred.",
    "details": [
      { "code": "InternalServerError", "message": "An unexpected error occurred." },
      { "code": "InvalidContentDimensions",
        "message": "The input image dimensions are out of range. Refer to documentation for supported image dimensions.",
        "target": "2" }
    ] } }
```
Rule: top-level `error.code` = **most severe** error; per-page/per-item errors go in `error.details[]` with `target` = the page number **as a string**.

**Top-level `code` → HTTP status** `[DOC — resolve-errors]`

| `code` | Canonical `message` | HTTP |
|---|---|---|
| `InvalidRequest` | `Invalid request.` | **400** |
| `InvalidArgument` | `Invalid argument.` | **400** |
| `Forbidden` | `Access forbidden due to policy or other configuration.` | **403** |
| `NotFound` | `Resource not found.` | **404** |
| `MethodNotAllowed` | `The requested HTTP method isn't allowed.` | **405** |
| `Conflict` | `The request couldn't be completed due to a conflict.` | **409** |
| `UnsupportedMediaType` | `Request content type isn't supported.` | **415** |
| `InternalServerError` | `An unexpected error occurred.` | **500** |
| `ServiceUnavailable` | `A transient error occurred. Try again.` | **503** |

No row exists for **401** or **429** — the docs table has neither.

**Inner `code` map** (all of these are `innererror.code`, never top-level):

| Top-level | `innererror.code` | Message |
|---|---|---|
| `Conflict` | `ModelExists` | A model with the provided name already exists. |
| `Forbidden` | `AuthorizationFailed` | Authorization failed: {details} |
| `Forbidden` | `InvalidDataProtectionKey` | Data protection key is invalid: {details} |
| `Forbidden` | `OutboundAccessForbidden` | The request contains a disallowed domain name or violates the current access control policy. |
| `InternalServerError` | `Unknown` | Unknown error. |
| `InvalidArgument` | `InvalidContentSourceFormat` | Invalid content source: {details} |
| `InvalidArgument` | `InvalidParameter` | The parameter {parameterName} is invalid: {details} |
| `InvalidArgument` | `InvalidParameterLength` | Parameter {parameterName} length must not exceed {maxChars} characters. |
| `InvalidArgument` | `InvalidSasToken` | The shared access signature (SAS) is invalid: {details} |
| `InvalidArgument` | `ParameterMissing` | The parameter {parameterName} is required. |
| `InvalidRequest` | `ContentSourceNotAccessible` | Content isn't accessible: {details} |
| `InvalidRequest` | `ContentSourceTimeout` | Timeout while receiving the file from client. |
| `InvalidRequest` | **`InvalidContent`** | The file is corrupted or format is unsupported. Refer to documentation for the list of supported formats. |
| `InvalidRequest` | **`InvalidContentDimensions`** | The input image dimensions are out of range. Refer to documentation for supported image dimensions. |
| `InvalidRequest` | **`InvalidContentLength`** | The input image is too large. Refer to documentation for the maximum file size. |
| `InvalidRequest` | `UnsupportedContent` | Content isn't supported: {details} |
| `InvalidRequest` | **`NotSupportedApiVersion`** | The requested operation requires {minimumApiVersion} or later. |
| `InvalidRequest` | `ModelAnalyzeError` | Couldn't analyze using a custom model: {details} |
| `InvalidRequest` | `ModelNotReady` / `ModelReadOnly` / `ModelBuildError` / `ModelComposeError` | model lifecycle |
| `InvalidRequest` | `DocumentModelLimit` / `DocumentModelLimitNeural` / `DocumentModelLimitComposed` | model-count limits |
| `InvalidRequest` | `OperationNotCancellable` | The operation can no longer be canceled. |
| `NotFound` | `ModelNotFound` | The requested model wasn't found. It was deleted or still building. |
| `NotFound` | **`OperationNotFound`** | The requested operation wasn't found. The identifier is invalid or the operation is expired. |

Note the codes commonly mistaken for top-level: `InvalidContent`, `InvalidContentDimensions`, `InvalidContentLength` all sit **under `InvalidRequest` (400)**.

⚠️ **Field divergence:** the cloud DI service has been observed returning the **flat, unwrapped** `{"code":"NotFound","message":"Analyze result does not exist."}` for an expired/unknown resultId on both `2023-07-31` and `2024-11-30` — an MS moderator confirmed it as *"a valid difference between documentation and Service side."* **The gateway should emit the documented wrapped shape** (that is what SDK models parse), but must **tolerate both** when reading upstream.

### 6B. Surface B — Computer Vision Read v3.2: FLAT (no envelope)

`ComputerVisionOcrError`, `required: [code, message]` — defined in `Ocr.json`, which owns both Read routes `[SPEC]`:

```jsonc
{
  "code":      "InvalidImageUrl",   // REQUIRED, ComputerVisionOcrErrorCodes, modelAsString: true (OPEN set)
  "message":   "string",            // REQUIRED
  "requestId": "string"             // optional
}
```

**No `error` wrapper. No `target`. No `details`. No `innererror`.** This is the *opposite* of the rest of Computer Vision: the other v3.2 operations (`/analyze`, `/describe`, `/detect`, `/tag`, `/ocr`, `/models`, `/generateThumbnail`, `/areaOfInterest`, defined in `ComputerVision.json`) use the wrapped `ComputerVisionErrorResponse` = `{"error":{"code","message","innererror":{"code","message"}}}`. Read is the flat one. Writing the gateway from the wrong file inverts the shape.

**Complete `code` enum — 17 values, `modelAsString: true` so unlisted values are possible:**

```
InvalidImageFormat     UnsupportedMediaType    InvalidImageUrl        InternalServerError
InvalidImageSize       BadArgument             NotSupportedLanguage   FailedToProcess
Unspecified            StorageException        InvalidPageRange       FailedToDownloadImage
InvalidImage           UnsupportedImageFormat  InvalidImageDimension  InvalidReadingOrder
InvalidRequest
```

**Not in this enum** (they belong to other CV files — do not emit them on the Read surface): `Timeout`, `ServiceUnavailable`, `NotSupportedFeature`, `NotSupportedImage`, `DetectFaceError`, `InvalidThumbnailSize`, `InvalidDetails`, `InvalidModel`, `CancelledRequest`, `NotSupportedVisualFeature`, `NotFound`, `ResourceNotFound`. **There is no not-found code in the Read enum at all.**

### 6C. Combined status → body table

| HTTP | Surface A body | Surface B (async routes) body |
|---|---|---|
| **200** with `"status":"failed"` | `{"status":"failed","createdDateTime":…,"lastUpdatedDateTime":…,"error":{"code":…,"message":…,"details":[…]}}` | `{"status":"failed","createdDateTime":…,"lastUpdatedDateTime":…}` — **no error detail exists in the schema** |
| **400** | `{"error":{"code":"InvalidRequest"\|"InvalidArgument","message":…,"innererror":{"code":"InvalidContent"\|"InvalidContentLength"\|"InvalidContentDimensions"\|"InvalidParameter"\|"ParameterMissing",…}}}` | `{"code":"BadArgument"\|"InvalidRequest"\|"InvalidImageUrl"\|"InvalidPageRange"\|"InvalidReadingOrder"\|"NotSupportedLanguage","message":…,"requestId":…}` |
| **401** | *No documented mapping.* Gateway-defined. Recommend `{"error":{"code":"Unauthorized","message":"Access denied due to invalid subscription key."}}` | Gateway-defined; CV container clients are known to branch on 401. Recommend flat `{"code":"Unauthorized","message":…}` |
| **402** | n/a | Observed in production container clients (billing/quota). Flat shape. |
| **403** | `{"error":{"code":"Forbidden","message":"Access forbidden due to policy or other configuration.","innererror":{"code":"AuthorizationFailed"\|"OutboundAccessForbidden",…}}}` | Not in the enum; use `{"code":"InvalidRequest",…}` or gateway-defined |
| **404** | `{"error":{"code":"NotFound","message":"Resource not found.","innererror":{"code":"OperationNotFound","message":"The requested operation wasn't found. The identifier is invalid or the operation is expired."}}}` — *and tolerate the flat variant upstream* | **No not-found code exists.** Recommend flat `{"code":"InvalidRequest","message":"Operation not found."}` — but see §10 item 15: **prefer not to 404 a live id at all** |
| **405** | `{"error":{"code":"MethodNotAllowed","message":"The requested HTTP method isn't allowed."}}` | Kestrel-level; typically bodyless |
| **413** | Not modeled. If you front with nginx, `client_max_body_size` returns **HTML**, not JSON. Emit `{"error":{"code":"InvalidRequest","message":"…","innererror":{"code":"InvalidContentLength",…}}}` with 400 instead where possible. | Emit `{"code":"InvalidImageSize","message":…}` |
| **415** | `{"error":{"code":"UnsupportedMediaType","message":"Request content type isn't supported."}}` | `{"code":"UnsupportedMediaType"\|"UnsupportedImageFormat","message":…}` |
| **429** | **Not modeled; the container never emits it** (containers don't cap TPS). If the *gateway* throttles: `{"error":{"code":"ServiceUnavailable"\|"TooManyRequests",…}}` **+ `Retry-After` (integer seconds)** | Not modeled. Flat `{"code":"InvalidRequest",…}` + `Retry-After` |
| **500** | `{"error":{"code":"InternalServerError","message":"An unexpected error occurred.","innererror":{"code":"Unknown","message":"Unknown error."}}}` | `{"code":"InternalServerError"\|"FailedToProcess"\|"StorageException","message":…}` |
| **503** | `{"error":{"code":"ServiceUnavailable","message":"A transient error occurred. Try again."}}` — also the container's state when the billing heartbeat has failed 10 consecutive times: *"Container isn't in a valid state. Subscription validation failed with status 'OutOfQuota' API key is out of quota."* | Not in the Read enum; use `{"code":"Unspecified",…}` or gateway-defined |

**Fourth, unschematized error class — the gateway must never let these reach a client:**

| Producer | Shape |
|---|---|
| CV `syncAnalyze` failure | **`{"status": "Failed"}`** — capital F, no `code`, no `message`. The only error form MS documents for this route. Unactionable. |
| Kestrel unmatched route | bodyless 404 (log line: `No candidates found for the request path '…'` / `Request did not match any endpoints`) |
| nginx in front of the DI container | HTML for 413 / 502 / 504 |
| Container startup / billing failure | free-text `apiStatus` / `apiStatusMessage` on `/status`; values `Valid \| Invalid \| Mismatch \| CouldNotConnect \| OutOfQuota \| BillingEndpointBusy \| ContainerUseUnauthorized \| Unknown`. `CouldNotConnect` and `BillingEndpointBusy` carry a `Retry-After`. |
| CV Read container log noise | `QueuingOperationException` every few seconds — MS: *"caused by some internal bugs. Customers can ignore these errors for now."* **Benign; do not alert, do not surface.** |

**Practical parsing rule for the gateway's upstream client:** switch on body probe, not HTTP status.
- top-level `"error"` object → DI shape
- top-level `"code"` + `"message"` (+ optional `"requestId"`) → CV Read shape
- HTTP 200 with `"status"` → operation envelope (DI has `.error`; CV Read has nothing)
- `{"status":"Failed"}`, empty body, or HTML → the unschematized class; log it and synthesize a well-formed error for the client

---

## 7. Sync vs async reality — definitive

### 7.1 The finding

**Both containers expose a native synchronous analyze endpoint. Neither cloud service does.**

| | Surface A (DI layout-4.0) | Surface B (CV Read 3.2) |
|---|---|---|
| Native sync route | **`POST /documentintelligence/documentModels/{modelId}:syncAnalyze`** | **`POST /vision/v3.2/read/syncAnalyze`** |
| Documented on learn.microsoft.com? | **NO** — completely undocumented | **YES** — install doc §"Synchronous read" + migration guide |
| In the public REST spec? | **NO** — `syncAnalyze` occurs 0 times in `azure-rest-api-specs` for DI | **NO** — `syncAnalyze` occurs 0 times in every ComputerVision spec file (v1.0…v3.2) |
| SDK method? | **NONE** — must be hand-rolled raw HTTP | **NONE** — must be hand-rolled raw HTTP |
| Evidence | `[IMAGE]` `/app/appsettings.api.json` `"SyncAnalyzePath": "{baseApiEndpoint}/documentModels/{modelId}:syncAnalyze"` under the `documentintelligence` family (api-versions incl. `2024-11-30`); compiled route literal `/documentintelligence/documentModels/{modelId}:syncAnalyze` in `Microsoft.CloudAI.Containers.VDI.Endpoint.Analyze.dll`; MVC action `AnalyzeController.SynchronousAnalyze`; MS's own batch worker calls it (`VisionBatchConfig.SyncAnalyzeUrlTemplate`). `[FIELD, 3.1]` MS staff: *"designed to return the result directly in the same call… you should receive a 200 OK with the full response."* | `[DOC]` *"When the image is read in its entirety, then and only then does the API return a JSON response."* + *"Synchronous operations are only supported in containers."* `[FIELD]` production clients read `resp.analyzeResult.readResults[0]` straight off the POST |
| Reliability | **Not guaranteed sync.** Observed intermittently returning `202` + `Operation-Location` under memory pressure / large docs / high concurrency; MS called 202 *"not standard or documented behavior"* for this route; fixed with +8 GB RAM `[FIELD, layout-3.1]` | No 202-fallback report exists for CV Read specifically, but the same engine and storage path apply |
| Storage bypass? | **No.** Same controller family; the analyze pipeline still writes through the shared/output store. | **No.** A user on `3.2-model-2022-04-30` got a `StorageException` **returned from `syncAnalyze`** instead of results — a broken/full `/share` mount breaks sync too. |
| Sibling internal route | `:syncAnalyze/upstreamResult` — *"should only be called from another prebuilt FormRecognizer on premises container"*. **Never expose.** | none |

**Availability caveat, stated plainly:** `:syncAnalyze` is present in the `layout-4.0:2024-11-30` image's routing table and compiled routes. It is **absent or broken on 3.0-era builds** — `POST /formrecognizer/documentModels/prebuilt-layout:syncAnalyze?api-version=2023-07-31` returned `500 UnhandledEndpointException` on a disconnected layout build (`1.2.1187.0-20241022.6`) and on `prebuilt-invoice` / `document-3.0` / `layout-3.0`. No public first-hand report exists of a successful end-to-end `prebuilt-layout:syncAnalyze?api-version=2024-11-30` round trip on `layout-4.0`. Treat the route as **present-per-image-forensics, runtime-unverified on 4.0** (probe Q-A1).

### 7.2 Documented limits on the sync routes

**There are none.** Neither Microsoft doc, spec, nor container page states a page cap, size cap, format restriction, or query-parameter set for either sync route. The frequently quoted numbers (2,000 pages / 2 on free tier; 500 MB / 4 MB; 50×50 to 10,000×10,000 px) come from the **cloud service** "Input requirements" sections, are scoped to the Read/Analyze API generally, are gated on the billing resource's pricing tier, and are **never restated for the container or for the sync route.** Quoting them as "the documented syncAnalyze limits" is inference, not documentation.

The only real bounds on a sync call:

| Bound | Value | Source |
|---|---|---|
| `Task:MaxRunningTimeSpanInMinutes` | **60 minutes** default — *"Maximum running time for a single request"*, after which the request is treated as timed out | `[DOC]`, both containers, v3.x+ |
| Every intermediate proxy / LB / ingress idle timeout | typically **60 s** — severs the held connection long before the 60-minute ceiling | operational |
| Container RAM | the observed cause of DI's 202 fallback | `[FIELD]` |
| nginx `client_max_body_size` | **90 MB** in MS's own published DI reverse-proxy sample — caps uploads regardless of the service's 500 MB figure | `[DOC]` |

**Practical rule:** keep sync calls to small single images and short page ranges; use page-range fan-out for documents (§9.4).

### 7.3 Therefore: what "call the container synchronously" MUST mean in the gateway

**Surface B (CV Read) — a native synchronous call.**
```
POST {upstream}/vision/v3.2/read/syncAnalyze?language=…&readingOrder=…
Content-Type: application/octet-stream   (or application/json + {"url":…})
-> 200, full ReadOperationResult
```
Stateless with respect to the *poll*: no operation id is ever handed back, so it load-balances correctly across replicas with plain round-robin. This is the workaround the field actually adopted for the multi-replica problem. It still touches the result store (StorageException caveat), so `/share` must be healthy on every replica.

**Surface A (DI layout) — a native synchronous call, with a mandatory async fallback path.**
```
POST {upstream}/documentintelligence/documentModels/prebuilt-layout:syncAnalyze?api-version=2024-11-30
Content-Type: application/pdf | application/octet-stream | application/json
-> 200, full analyzeResult                      // the happy path
-> 202 + Operation-Location                      // the degraded path — MUST be handled
```
The gateway's upstream adapter is a **dual-mode call**:

```
result = POST :syncAnalyze
if status == 200:
    parse and store the result; job -> succeeded
elif status == 202:
    # container degraded to async; the operation lives on THAT replica only
    extract resultId from Operation-Location
    PIN this job to the replica that answered (persist upstream_host + upstream_op_url)
    poll GET {that replica}/documentintelligence/documentModels/{modelId}/analyzeResults/{resultId}
         with instance affinity, honouring Retry-After, until terminal
elif status == 404 / 500 UnhandledEndpointException:
    # this image build does not serve :syncAnalyze at all
    fall back permanently to submit-and-poll-with-affinity via :analyze
```

**Instance affinity is not optional on the fallback path.** The moment a 202 comes back, the operation id is meaningful only to the replica that minted it (unless you configured a shared Azure Blob + Azure Queue backend — §8). The gateway must therefore address a **specific upstream pod** (headless Service + per-pod DNS, e.g. `up-2.up-hl.ns.svc.cluster.local`, chosen by rendezvous hash on the job id), persist `upstream_host` alongside `upstream_op_url`, and re-drive the job — with the same idempotency key — if that pod disappears. `sessionAffinity: ClientIP` on the Kubernetes Service is **useless here**: every request from one gateway pod shares one source IP, so all traffic pins to one backend.

**Summary statement for the engineer:**
- *Surface B*: "call the container synchronously" = **a real synchronous HTTP call**. No polling, no affinity, no shared state required.
- *Surface A*: "call the container synchronously" = **a real synchronous HTTP call on the undocumented `:syncAnalyze` route, with a mandatory submit-and-poll-internally-with-instance-affinity fallback** for the 202-degradation and missing-route cases.
- *Both*: the gateway's **client-facing** surface remains 202 + `Operation-Location` + poll, because that is what every SDK is generated against. The gateway owns the LRO fiction; the container call underneath is synchronous whenever it can be.

---

## 8. Multi-replica failure mode

### 8.1 What is actually true

The containers do **not** store results "per-instance and not shared" as an architectural property. Both families support a pluggable shared backing store, and Microsoft documents multi-replica behind a load balancer as a supported topology. The DI FAQ states it flatly:

> *"For asynchronous calls, you can run multiple containers with shared storage. The container that's processing the POST analyze call stores the output in the storage. Then, any other container can fetch the results from the storage and serve the GET calls. **The request ID isn't tied to a container.**"*

MS's own (now-retired) Kubernetes article ships a Read Deployment with `replicas: 3` behind a `type: LoadBalancer` Service, configured with `Storage__ObjectStore__AzureBlob__ConnectionString` and `Queue__Azure__ConnectionString` pointing at one storage account, and explains the design:

> *"By design, each v3 container has a dispatcher and a recognition worker. The dispatcher is responsible for splitting a multi-page task into multiple single page sub-tasks… To achieve page level parallelism, deploy multiple v3 containers behind a load balancer and let the containers share a universal storage and queue."* — with the hard constraint *"Currently only Azure Storage and Azure Queue are supported."*

### 8.2 What breaks, and exactly why

**The default configuration is unshared.** That is the failure.

| Concern | Read v3.x default | Read v3.x external option | DI Layout default | DI Layout external option |
|---|---|---|---|---|
| Result store | **File level — the container-local `/share` directory** | `Storage:ObjectStore:AzureBlob:ConnectionString` | ephemeral writable layer / `SharedRootFolder`+`Mounts:Shared` if set | `Storage:ObjectStore:AzureBlob:ConnectionString` |
| Queue | **In Memory** — MS: *"Development and testing"* | `Queue:Azure:ConnectionString` — MS: *"Production"* | **In memory** (docs instruct deleting the vars for local runs) | `Queue:Azure:ConnectionString` |
| Redis / RabbitMQ / MongoDB | **Removed in v3.x.** *"`Cache:Redis:Configuration` is no longer supported. The Cache isn't used in the v3.x containers."* RabbitMQ "unavailable". MongoDB removed. | — | n/a | — |

So, with defaults:

1. **Cross-pod poll fails.** *"Because the processing container and the `GET` request container might not be the same, an external cache stores the results and shares them across containers."* A poll routed to a replica that did not run the job finds no operation record. Symptom: **intermittent** poll failures whose rate scales with replica count — the classic misdiagnosed-as-flaky bug.
2. **No cross-replica work sharing.** With the in-memory queue, each replica's dispatcher enqueues its single-page sub-tasks into its *own* process queue. Scaling to N replicas gives you N isolated islands, not N workers. The doc's promised "throughput improved up to n times" is conditional on the shared Azure Queue.
3. **Restart destroys everything.** `/share` is container-local and the queue is in-process. In-flight jobs vanish with **no failure signal**, and every previously issued operation id becomes unresolvable. `HealthCheck:MemoryUpperboundInMB` (default = recommended memory) makes the container **self-report unhealthy under memory pressure** → kubelet restarts the pod mid-analysis; the cgroup OOM killer does the same.
4. **The install doc's own remedy is stale.** It says *"you must have an external cache"* and links to cache settings — but `Cache:Redis` is **v2.0-only**. **Configuring Redis on a 3.2 container silently does nothing.**
5. **Double-processing / double-billing** even when correctly configured: `Queue:Azure:QueueVisibilityTimeoutInMilliseconds` defaults to **30000**; MS recommends **120000** *"to avoid pages from being redundantly processed."* At the default, any page taking >30 s becomes visible again and is processed twice.
6. **The K8s trap MS never documents:** `Mounts:Shared` guidance is written for single-host Docker Compose, where `./share` is a host bind mount. On a multi-node cluster a hostPath is **not** shared across nodes. You need a genuine RWX volume (Azure Files / NFS / CephFS) or the Azure Blob object store.
7. **Air-gapped customers have no supported shared backend at all.** "Only Azure Storage and Azure Queue are supported" — no S3, no MinIO, no Redis, no RabbitMQ in v3.x. This is precisely where the gateway must own the result store itself.

### 8.3 The observed HTTP status and body — **UNVERIFIED, flagged**

**No primary source states the status code or body for a cross-pod poll.** Neither swagger declares a 404 for the poll GET (each has only `200` + `default`), and no reproduction, captured response, or bug report exists in the public record. Anyone asserting a specific body is extrapolating.

Two plausible modes, both of which the gateway must be able to survive:

| Mode | Trigger | Observed shape |
|---|---|---|
| **(a) Hard not-found** — the most likely | Fully unshared config; replica B has no record of the id. Corroborated indirectly: the CV container 404s any unrouted path, and the DI cloud service 404s a stale/expired resultId. | **DI:** `404` with `{"error":{"code":"NotFound","message":"Resource not found.","innererror":{"code":"OperationNotFound","message":"The requested operation wasn't found. The identifier is invalid or the operation is expired."}}}` — *or* the flat field variant `{"code":"NotFound","message":"Analyze result does not exist."}`. **CV Read:** the enum has **no not-found code**; expect either a flat `ComputerVisionOcrError` or a bodyless Kestrel 404. |
| **(b) Silent stall** — at least as likely, and worse | Partially shared config (shared queue, unshared object store), or the operation record exists but the result does not. | `200` with `{"status":"running"}` or `{"status":"notStarted"}` that **never advances**. Nearest field corroboration: a DI Layout container on Cloud Run (autoscaling, no shared backend) — *"It seems as though the async endpoint stalls endlessly. I've been using the synchronous endpoint instead."* MS never resolved the thread. That is the exact signature of poll-hits-wrong-instance behind a round-robin LB. |

**Minor precision:** a default Kubernetes ClusterIP Service is **not** round-robin — kube-proxy in iptables mode selects a backend at *random* with equal probability; round-robin is the IPVS default. Same outcome, different mechanism.

### 8.4 What each SDK does when the gateway 404s an in-flight poll

This is why §10 item 15 exists.

| Client | Behavior on a 404 poll |
|---|---|
| Python DI | `_raise_if_bad_http_status_and_method` accepts only `{200,201,202,204}` → `BadStatus` → **`azure.core.exceptions.HttpResponseError`**, immediate, no retry. Message text becomes `(NotFound) Resource not found.` if the body parses as OData, else `Operation returned an invalid status 'Not Found'`. |
| .NET (Azure.AI.DocumentIntelligence) | Poll status outside `200..204` → `failureState = OperationState.Failure(response); return true` → **operation terminally failed**, indistinguishable from a genuine analysis failure. |
| JS | No `status` in the body → `toOperationStatus(statusCode)`: 404 → **failed**, thrown as an `Error` (with a warning if the body lacks `error.code`/`error.message`). |
| **Java** | **Never checks the poll HTTP status code at all.** Deserializes the body, gets `status == null`, then `new PollResponse<>(null, …)` hits `Objects.requireNonNull(status, …)` → **`NullPointerException`**. An opaque crash with no useful error. |
| Python CV (msrest) | `get_read_result` requires exactly 200 → **`ComputerVisionOcrErrorException`**. urllib3 does **not** retry 404. |

Corollary: **any error the gateway must return on a poll should still carry a JSON body containing a top-level `status`** — ideally `{"status":"failed","createdDateTime":…,"lastUpdatedDateTime":…,"error":{…}}` at HTTP 200 — rather than a bare 404. That is the only shape all five clients handle gracefully.

---

## 9. Limits table

### 9.1 Input limits (both surfaces; these are **cloud service** figures, gated on the billing resource's pricing tier — no container-specific overrides are documented)

| Limit | Free F0 | Standard S0/S1 | Applies to |
|---|---|---|---|
| Max document / image file size | **4 MB** | **500 MB** | both |
| PDF file size | — | *no size limit documented* | CV Read |
| Max pages per analysis (PDF/TIFF) | **2** (only the first two pages) | **2,000** | both |
| Min image dimensions | 50 × 50 px | 50 × 50 px | both |
| Max image dimensions | 10,000 × 10,000 px | 10,000 × 10,000 px | both |
| Min text height | 12 px on a 1024×768 image (≈8 pt at 150 DPI) | same | both |
| Max OCR JSON response | 500 MB | 500 MB | DI |
| Office (DOCX/XLSX/PPTX) characters | — | **8 million** | DI only |
| Supported formats | DI Layout: **PDF; JPEG/JPG, PNG, BMP, TIFF, HEIF; DOCX, XLSX, PPTX, HTML** · CV Read: **JPEG, PNG, BMP, PDF, TIFF** | same | — |

⚠️ **A container billed against an F0 resource silently truncates every PDF to 2 pages.** Classic, very quiet dev/prod discrepancy. Password-locked PDFs must be unlocked first. Out-of-range dimensions → DI `InvalidRequest`/`InvalidContentDimensions`, CV `InvalidImageDimension`.

Page counting for billing: 1 PDF page / 1 TIFF frame / 1 XLSX worksheet / 1 PPTX slide = 1 page; Word & HTML in **3,000-character blocks**. Billing is **per page, not per call** — a 100-page PDF = 100 transactions.

### 9.2 Sync vs async limits

| | Surface A async `:analyze` | Surface A sync `:syncAnalyze` | Surface B async `/read/analyze` | Surface B sync `/read/syncAnalyze` |
|---|---|---|---|---|
| Documented size/page limits | the §9.1 table (cloud-scoped) | **NONE documented** | the §9.1 table | **NONE documented** |
| Per-request time ceiling | `Task:MaxRunningTimeSpanInMinutes` = **60 min** | same 60 min | same 60 min | same 60 min |
| Practical ceiling | none beyond the above | **proxy/LB idle timeout, typically 60 s** | none | **proxy/LB idle timeout, typically 60 s** |
| Result retention | see §9.3 | writes through the result store (StorageException observed) | see §9.3 | writes through the result store |
| nginx front-end cap (MS's own sample) | `client_max_body_size 90M;` | same | — | — |

### 9.3 Result retention

| | Surface A (DI Layout) | Surface B (CV Read 3.2) |
|---|---|---|
| Container knob | **`StorageTimeToLiveInMinutes`** — *"TTL duration to remove all intermediate and final files. **Default: Two days.** TTL can be set between five minutes and seven days."* | **`StorageTimeToLiveInMinutes`** (`3.2-model-2021-09-30-preview` and newer → **this is the one this image uses**), default **2,880 min**, range **60 – 10,080 min**. Legacy v3.x knob `Storage:TimeToLiveInDays`, default 2 days, range 1–7. |
| Documented cron | — | *"The Read OCR container has a built Cron job that removes the results and metadata associated with a request after 48 hours."* |
| Guarantee wording | — | *"any result that's live for longer than that period isn't guaranteed to be successfully retrieved"* — a floor, not a promise |
| Cloud contrast | inputs + results deleted **within 24 hours**; batch operation status also 24 h | `Operation-Location` description: *"The operation ID will expire in 48 hours."* |
| Early purge | `DELETE .../analyzeResults/{resultId}` → 204 | none |
| Survives restart? | **No** in the default config (ephemeral layer / container-local `/share`) | **No** in the default config (container-local `/share`) |

**Gateway policy:** treat the container's retention as a lower bound you never rely on. Persist results out of the container **immediately** on completion, and serve every client GET from your own store. Advertise a **24-hour** client-facing TTL (matching the cloud contract the SDKs' users expect) with a `410 Gone` / `NotFound`-`OperationNotFound` transition afterwards.

### 9.4 Concurrency, throughput, resources

**The containers do not rate-limit.** MS: *"**Containers don't cap transactions per second (TPS)** and can be made to scale both up and out to handle demand if you provide the necessary hardware resources."*

| Property | Value |
|---|---|
| 429 from the container | **Never emitted.** The 15 analyze-TPS / 50 get-TPS / 5 model-mgmt-TPS / 10 list-TPS numbers are **cloud resource quotas enforced by APIM**, absent from the container. Do not build retry logic around 429 upstream. |
| Max-concurrency knob | **None exists.** No `MaxConcurrent*`, no worker count, no thread-pool setting in either container's config table. Bounded only by `--cpus` / `--memory` and the internal queue. |
| Overload behavior | **Degradation, not rejection.** Requests are accepted, queued internally, latency climbs unboundedly, RSS climbs, then either `HealthCheck:MemoryUpperboundInMB` trips (container reports **unhealthy to liveness** → pod restart → **every in-flight and stored-but-unretrieved result on that pod is destroyed**) or the cgroup OOM killer fires. |
| Backstop | `Task:MaxRunningTimeSpanInMinutes` = 60 min |
| DI Layout resources | min **8 cores / 16 GB**; recommended **8 cores / 24 GB**; each core ≥ 2.6 GHz; x64 Linux only |
| CV Read 3.2 (2022-04-30) resources | min **4 cores / 8 GB**; recommended **8 cores / 16 GB**; ≥ 2.6 GHz; **host MUST support AVX2** — *"the container will not function correctly without AVX2 support"* (rules out most ARM hosts and some VM-masked CPUs) |
| CV benchmark basis | *"a single request per second, using a 523-KB image of a scanned business letter that contains 29 lines and a total of 803 characters."* Recommended config ≈ **2× faster** than minimum. Anything resembling a 50-page PDF is far outside this envelope. |
| Real-world sizing (MS support) | *"There is no official Microsoft sizing guide"*; starting point for 50 concurrent users × 10–15 dense pages across Layout+Read+Custom on one machine: **8–12 CPU cores, 32–48 GB RAM, SSD with 20–30 GB free**, plus *"Load testing with real documents is essential."* |
| Latency guidance | No SLA. MS's operational threshold: *"If you observe sustained periods (exceeding one hour) where latency per page consistently surpasses 15 seconds, consider addressing the issue."* |
| Field latency (Layout, 8 CPU / 32 GB) | **>100-page PDF: 5–10 minutes** as one call. Same work cut into **~3-page calls and parallelised: ~20+ seconds** wall-clock. **Cold start ≈ 15 s/pod.** |
| Disk | `/share` holds intermediate page images for up to `StorageTimeToLiveInMinutes`. Size an `emptyDir`/PVC at tens of GB, not hundreds of MB, or it fills and fails quietly. |
| Client poll interval | MS cloud guidance: **≥ 1 s**, and *"not more than once every 2 seconds"* per outstanding POST, honouring `Retry-After`; a 2-5-13-34 backoff pattern is suggested. |

**Design consequences:** these are 8-core / 16–24 GB pods, so a 16-core / 64 GB node fits 2–3 Layout replicas — horizontal scale is expensive and coarse. **HPA on CPU is near-useless** (one multi-page doc pins all 8 cores regardless of queue depth) — scale on *the gateway's* queue depth. Scale-to-zero is not viable given ~15 s cold start. And **page-range fan-out inside the gateway is worth ~15–30×** on large documents: chunk into ~3-page `pages=` calls and parallelise, which mirrors what the CV Read dispatcher does internally and what the DI Layout container will **not** do for you.

---

## 10. Gateway fidelity checklist

Ordered. Each item is testable. "SDK" means an unmodified official client must be unable to tell the difference.

### Submit path

1. **`POST /documentintelligence/documentModels/{modelId}:analyze?api-version=2024-11-30` returns exactly `202`.** Not 200, not 201.
   *Test:* Python `_analyze_document_initial` asserts `status_code in [202]`; .NET uses `new StatusCodeClassifier(stackalloc ushort[]{202})`; Java `@ExpectedResponses({202})`; JS `isUnexpected`. Any other code raises.
2. **`POST /vision/v3.2/read/analyze` returns exactly `202`; `GET /vision/v3.2/read/analyzeResults/{id}` returns exactly `200`.** A `202` on the CV poll is a hard SDK error.
3. **Accept both request encodings on both surfaces**, plus every `Content-Type` in §2. Accept `Transfer-Encoding: chunked` request bodies with **no `Content-Length`** — Python file-like bodies, .NET non-seekable `RequestContent`, Node streams, and Java `Flux<ByteBuffer>` all chunk.
   *Test:* `curl -H 'Transfer-Encoding: chunked' -T file.pdf …` must not 411.
4. **Tolerate and ignore the doc-artifact query params** `_overload=analyzeDocument` / `_overload=analyzeDocumentFromStream` (Surface A) and `overload=stream` (Surface B). Never require them.
5. **Tolerate and forward** `Ocp-Apim-Subscription-Key`, `x-ms-client-request-id`, `User-Agent`, `Accept`, `Content-Type`, `traceparent`, `tracestate`. Echo `x-ms-client-request-id` back. **Never require** an `Accept` header — azure-core's poll GET sends none.

### 202 response

6. **Emit `Retry-After` as an integer number of seconds** (`1` or `2`) on the 202 **and on every non-terminal 200 poll**.
   - Omitting it makes the Python DI client sleep **30 s** per poll (`polling_interval` default) — a fast gateway looks 30× slower.
   - .NET takes `Max(RetryAfter, backoff)` — it can only lengthen, never shorten.
   *Test:* a job that finishes in 1 s must be observed as finishing in ~1–2 s by `poller.result()`, not ~30 s.
7. **NEVER emit an HTTP-date `Retry-After`.** .NET silently ignores it (`int.TryParse` fails). **Python DI hard-fails**: `self._deserialize("int", response.headers.get("Retry-After"))` → `eval("int")("Wed, 21 Oct …")` → `DeserializationError`, on both the initial 202 and the terminal poll.
8. **NEVER emit a `Location` header on the 202.** Python (`get_final_get_url` → `self._request.method == "POST" and self._location_url`) and JS (`findResourceLocation` defaulting to the `Location` header for a POST) will issue an **extra final GET to it** and then parse *that* response as the analyze result.
9. **NEVER emit `resourceLocation` in the poll body.** Triggers an extra final GET in Python, Java and JS.
10. **Emit `Operation-Location` with exact canonical casing** and nothing else non-contractual. `apim-request-id` is optional cosmetic realism; `x-envoy-upstream-service-time` should be omitted.

### `Operation-Location` shape

11. **Surface A:** `{public-scheme}://{public-authority}/documentintelligence/documentModels/{modelId}/analyzeResults/{resultId}?api-version=2024-11-30`
    - absolute (never relative)
    - contains the literal segment `/documentintelligence/` (four SDKs regex on it)
    - `{modelId}` exactly 4 segments from the end and `{resultId}` exactly 2 from the end **after splitting on `/` and `?`**, with the `?api-version=` query present (Azure.AI.FormRecognizer 4.x indexes backwards)
    - fully percent-encoded; **no literal `{`, `}`, space, `|`, `^`** (Python `str.format()` raises; Java `new URI()` throws and silently demotes the polling strategy)
    *Test:* `re.match(r"[^:]+://[^/]+/documentintelligence/.+/([^?/]+)", header).group(1)` must equal your result id; and `header.split('/','?')[-2] == resultId`, `[-4] == modelId`.
12. **Surface B:** `{public-scheme}://{public-authority}/vision/v3.2/read/analyzeResults/{guid}` — **bare, canonically-formatted, 36-character lowercase GUID; no query string, no trailing slash, no fragment.**
    *Test:* `Guid.Parse(header.Substring(header.Length - 36))` must succeed, and `header.split("/")[-1]` must be a usable `operationId`.
13. **Never propagate the upstream container's `Operation-Location`.** Extract the id, discard the rest. This immunizes you against unroutable internal hosts and against the CV `vision`-substring corruption bug.
14. **On poll responses, either omit `Operation-Location` entirely or echo it byte-identically.** .NET and JS re-read it on every poll and switch polling URLs.

### Poll path

15. **Never `404` a live result id.** Result ids must be routable to whichever gateway replica receives the poll (shared store or consistent-hash routing). A 404 on an in-flight poll is: `HttpResponseError` (Python), terminal operation failure (.NET, JS), and an opaque **`NullPointerException`** (Java).
    *Test:* `kubectl scale --replicas=3` the gateway, submit 100 jobs, poll all of them; zero non-200 responses.
16. **A GET on a genuinely unknown / expired result id returns:**
    - **Surface A:** `404` with `{"error":{"code":"NotFound","message":"Resource not found.","innererror":{"code":"OperationNotFound","message":"The requested operation wasn't found. The identifier is invalid or the operation is expired."}}}`
    - **Surface B:** `404` with flat `{"code":"InvalidRequest","message":"Operation not found."}` (the Read enum has no not-found code) — or bodyless, matching Kestrel. Pick one and document it.
    - **In both cases, prefer a `200` with `{"status":"failed", …, "error":{…}}` for an id you know *existed* and lost** — that is the only shape all five clients handle gracefully.
17. **The poll body must always be JSON with a top-level `status`, even on non-200 responses.** Python raises `BadResponse("The response from long running operation does not contain a body.")` on an empty body; .NET treats a zero-length `ContentStream` as failure; Java NPEs.
18. **Accept `api-version` on the poll present, absent, or set to any value.** Python omits it; .NET and Java replace it; JS appends it.
19. **The terminal Surface-A body must be the full envelope** `{"status":"succeeded","createdDateTime":…,"lastUpdatedDateTime":…,"analyzeResult":{…}}` — **not** a bare `analyzeResult`. Python `.get("analyzeResult")` yields an empty result; .NET `GetProperty("analyzeResult")` **throws**; Java raises `AzureException("Cannot get final result")`.
20. **Status vocabulary is exactly `notStarted | running | succeeded | failed` on both surfaces.** Never `cancelled` (two Ls — terminal in JS only), never `skipped` (terminal in none), never `Complete`/`Done`/`finished`, never capital-S `Succeeded`.
21. **`createdDateTime` and `lastUpdatedDateTime` are always present** on every poll response, in both surfaces, in every state. On Surface B pass the upstream string through **verbatim** (no `format: date-time` in the spec; both 7-digit-fraction+offset and 3-digit-Z forms are legitimate).

### Path families and prefixes

22. **If legacy `azure-ai-formrecognizer` clients matter, alias `/formrecognizer/documentModels/*` → the same handlers**, and **emit an `Operation-Location` matching the family the caller arrived on** (`/formrecognizer/…` for FR clients, `/documentintelligence/…` for DI clients). Add `/formrecognizer/v2.1/*` only if v2.1 clients matter. For admin LROs the FR poller requires `Operation-Location` to contain `/operations/` **and** to spell the query exactly `?api-version` (i.e. api-version must be the *first* query parameter) — `prefix.split("?api-version")[0].split("/operations/")[1]` `IndexError`s otherwise.
23. **Never serve `/computervision/imageanalysis:analyze`.** It is a 404 on the real container; matching that is fidelity.

### Transport

24. **Never redirect. Any status.** .NET builds its transport with `AllowAutoRedirect = false` and has no redirect policy — a 302 is terminal on both POST and poll. Python/msrest refuse 301/302 for non-GET/HEAD (a 302 on `POST :analyze` surfaces as `status_code not in [202]` → `HttpResponseError`). JS follows GET/HEAD only.
25. **Never attach `Retry-After` to a non-retriable error.** Python's `is_retry` returns true for *any* status ≥ 400 carrying `Retry-After`, **bypassing the method allowlist**, up to `retry_total = 10`. A 404 or 400 with `Retry-After` gets hammered 10 times.
26. **If you throttle, always attach `Retry-After`** (integer seconds). JS retries 429/503 **only** when `retry-after-ms` / `x-ms-retry-after-ms` / `Retry-After` is present and parseable; without it, a 429 is not retried at all. Python CV/msrest **never** retries 429 (it is in `safe_codes`).
27. **Compress only what the request's `Accept-Encoding` permits. `gzip`/`deflate` only — never `br`.** .NET sends **no `Accept-Encoding`** and cannot decompress; Node accepts `gzip,deflate` only.
28. **If you rewrite any response body, recompute or drop `Content-Length`** and let the framework chunk. A stale length after body rewriting causes transport-level truncation or a hang before the SDK ever sees the payload.
29. **`POST` may be replayed by the SDKs.** msrest's `method_whitelist` includes POST for 5xx; .NET's retry policy has no method allowlist. Request content must be re-readable on your side, and your submit path must be idempotent under a client-side replay (key on `x-ms-client-request-id` or a content fingerprint).

### Semantics the gateway owns

30. **Preserve `AnalyzeResult` property order and optionality exactly** as in §3A-3 — including omitting `angle`/`width`/`height`/`unit`/polygons/`lines` for DOCX/XLSX/PPTX/HTML inputs, and the `☒`/`☐` vs `:selected:` markdown asymmetry.
31. **Preserve Surface-B `analyzeResult.version` and `modelVersion` verbatim**; never normalize and never branch on them.
32. **Advertise a 24-hour client-facing result TTL**, transitioning to §10-16's not-found body afterwards, independent of the container's `StorageTimeToLiveInMinutes`.
33. **Support `DELETE .../analyzeResults/{resultId}` → `204 No Content`** (early purge) on Surface A.
34. **Health:** liveness on your own process; **readiness gated on the upstream `/status`**, not `/ready` alone — the billing-heartbeat failure mode leaves the container *running and passing liveness* while refusing to serve queries (10 retries at 10–15 min intervals ≈ 100–150 min of grace, then a hard stop). Treat `apiStatus` values `Valid` and `nonmetered`/`non-metered` as healthy.
35. **Ignore `QueuingOperationException` in the CV container logs.** MS: *"caused by some internal bugs. Customers can ignore these errors for now."* Do not alert; do not surface.
36. **Enforce a deployment constraint: no `vision` substring in the upstream container's DNS authority** (K8s Service name, compose service name, DNS label). Assert it at startup and fail loudly.

---

## 11. Open questions / unverified

Ordered by how much they change the design. Every probe is a curl against a running container.

### Surface A — Document Intelligence layout-4.0

| # | Question | Status | Probe that settles it |
|---|---|---|---|
| **Q-A1** | Does `layout-4.0:2024-11-30` actually **serve** `:syncAnalyze` at runtime and return `200` with the result? Route is present in the image's config and compiled route literals `[IMAGE]`; MS staff confirm 200-OK design `[FIELD]` — but every first-hand success report is on **3.1**, and 3.0-era builds returned `500 UnhandledEndpointException`. | **UNVERIFIED at runtime on 4.0** — highest-priority open item; §7's whole design rests on it | `curl -s http://localhost:5000/swagger/v1/swagger.json \| jq '.paths\|keys[]\|select(test("[Ss]ync"))'` then `curl -si -X POST "http://localhost:5000/documentintelligence/documentModels/prebuilt-layout:syncAnalyze?api-version=2024-11-30" -H "Content-Type: application/pdf" --data-binary @1page.pdf` |
| **Q-A2** | Does `:syncAnalyze` accept the same query params (`pages`, `features`, `outputContentFormat`, `output`, `stringIndexType`, `locale`) and the same `Content-Type` set as `:analyze`? Is the 200 body the bare `AnalyzeResult` or the `AnalyzeOperation` wrapper? | **UNVERIFIED** | Same POST as Q-A1 with each param appended; diff the 200 body against the corresponding `:analyze` terminal poll body |
| **Q-A3** | Under what load/memory conditions does `:syncAnalyze` degrade to `202 + Operation-Location` on the 4.0 image? What is the safe document size / page count? | **UNVERIFIED** (observed on 3.1; root cause was RAM) | Load test at the container's memory ceiling with escalating page counts; record the first 202 |
| **Q-A4** | Does `layout-4.0` serve `/analyzeResults/{resultId}/pdf` and `/analyzeResults/{resultId}/figures/{figureId}`? Contract says yes; no container-side confirmation exists publicly. | **UNVERIFIED** | `curl -s "http://localhost:5000/documentintelligence/documentModels/prebuilt-layout/analyzeResults/<id>/pdf?api-version=2024-11-30"` after a submit with `&output=pdf` |
| **Q-A5** | Are all add-on `features` (`ocrHighResolution`, `barcodes`, `formulas`, `keyValuePairs`, `styleFont`, `queryFields`) actually enabled inside `layout-4.0`? The `keyValuePairs` add-on's container availability is unstated. | **UNVERIFIED** | Submit with each feature individually; check for a `warnings[]` entry or a 400 |
| **Q-A6** | Exact raw-swagger JSON path under `/swagger` (`/swagger/v1/swagger.json` vs `/swagger/v1.0/swagger.json` vs an api-docs path). Field reports mention both `/swagger` and `/api-docs`. | **UNVERIFIED** | Load `http://localhost:5000/swagger` in a browser and read the JSON link off the Swagger UI page |
| **Q-A7** | Does the container emit `apim-request-id` or `x-envoy-upstream-service-time`? (Both are cloud front-door headers; not in the contract.) | **UNVERIFIED — assume no** | `curl -si -X POST …:analyze… \| head -20` |
| **Q-A8** | Does the container ever return HTTP `429`, and at what queue depth? Docs say containers don't cap TPS. | **UNVERIFIED — assume never** | Saturate with N× concurrent submits; watch for any 429 |
| **Q-A9** | Exact status + body for a GET on an unknown/expired `resultId` **on the container** (cloud returns 404, sometimes wrapped, sometimes flat). | **UNVERIFIED** | `curl -si "http://localhost:5000/documentintelligence/documentModels/prebuilt-layout/analyzeResults/00000000-0000-0000-0000-000000000000?api-version=2024-11-30"` |
| **Q-A10** | Does the container derive `Operation-Location`'s authority from `Host`, from `Referer`, or from both (and in what precedence)? MS's nginx sample sets both, which implies it matters. | **PARTIALLY VERIFIED** (Host reflection observed; precedence unknown) | POST with `Host:` and `Referer:` set to *different* values; read the emitted header |

### Surface B — CV Read 3.2

| # | Question | Status | Probe |
|---|---|---|---|
| **Q-B1** | Is `GET /vision/v3.2/read/operations/{id}` **also** served as a live alias? The install doc says yes; the migration guide, the swagger, and a Dec-2025 field observation of this exact image all say `analyzeResults`. | **UNVERIFIED — assume only `analyzeResults`** | `curl -si "http://localhost:5000/vision/v3.2/read/operations/<guid>"` vs `.../analyzeResults/<guid>` after a real submit |
| **Q-B2** | Behavior of `image/jpeg`, `image/png`, `application/pdf` etc. as an explicit request `Content-Type` (spec declares only `application/json` + `application/octet-stream`). Does it 415 or sniff? | **UNVERIFIED** | `curl -si -X POST …/read/analyze -H "Content-Type: image/png" --data-binary @x.png` |
| **Q-B3** | Exact HTTP status and body for an expired/unknown `operationId` on the container. The Read error enum has **no** not-found code. | **UNVERIFIED** | `curl -si "http://localhost:5000/vision/v3.2/read/analyzeResults/00000000-0000-0000-0000-000000000000"` |
| **Q-B4** | Does in-container `model-version` accept anything other than the baked-in `2022-04-30`? Silently ignored, or an error? | **UNVERIFIED** | `?model-version=2021-04-12` and `?model-version=latest`; diff `analyzeResult.modelVersion` |
| **Q-B5** | The **success HTTP status code for `syncAnalyze`** is never stated in any MS doc. 200 is implied, not documented. | **UNVERIFIED** | `curl -si -X POST http://localhost:5000/vision/v3.2/read/syncAnalyze -H "Content-Type: application/octet-stream" --data-binary @x.png \| head -1` |
| **Q-B6** | Real error-body shape at the container for each 4xx (spec models only a single `default`; Kestrel-level rejections almost certainly carry no `ComputerVisionOcrError` at all). | **UNVERIFIED** | Malformed JSON body → status+body; bad `readingOrder` → status+body; bad `pages` → status+body; wrong verb → status+body |
| **Q-B7** | Does the container ever emit `429`? | **UNVERIFIED — assume never** | saturate; watch |

### Cross-cutting

| # | Question | Status | Probe |
|---|---|---|---|
| **Q-X1** | **The exact HTTP status and body of a cross-pod poll in the default unshared configuration** (§8.3). No primary source documents it; two plausible modes (hard 404 vs stalled 200-`running`). This is the demo that justifies the gateway, and it is worth generating your own citation for. | **UNVERIFIED — the single most important unknown in this document** | `kubectl scale deploy/<container> --replicas=2` with no shared storage/queue → POST to pod A's IP directly → `curl -si` the same result id against pod A **and** pod B → record both status lines and bodies. Repeat with `Storage:ObjectStore:AzureBlob:ConnectionString` + `Queue:Azure:ConnectionString` set, and again with a shared RWX volume at `/share`, to confirm all three configurations. |
| **Q-X2** | Does a shared **RWX volume at `/share`** (Azure Files / NFS) alone fix cross-pod polling for either container, or is `Queue:Azure:ConnectionString` also mandatory? MS documents Azure Blob + Azure Queue as the supported answer and says *"Currently only Azure Storage and Azure Queue are supported"* — but `Mounts:Shared` is documented as a first-class result-store setting. **This determines whether an air-gapped customer has any supported multi-replica option at all.** | **UNVERIFIED** | The Q-X1 harness with a shared RWX PVC mounted at `/share` on both pods and no Azure connection strings |
| **Q-X3** | Does either container emit a `Retry-After` on the 202? (Declared in the DI spec; absent from the CV doc's header dump.) | **PARTIALLY VERIFIED** | `curl -si` the submit on each surface |
| **Q-X4** | Actual per-replica concurrency ceiling before latency degrades or `HealthCheck:MemoryUpperboundInMB` trips. No knob exists; must be determined empirically per document profile. | **UNVERIFIED by design** | Ramped concurrency load test with representative documents; watch p99 latency, RSS, and pod restarts |

**The single fastest move for Surface A and Surface B alike:** start the exact image tag you will deploy and pull its own swagger —
`GET http://localhost:5000/swagger` (DI, then follow the UI's JSON link) and
`GET http://localhost:5000/swagger/vision-v3.2-read/swagger.json` (CV).
That page is ground truth for the exact build, and it supersedes every learn.microsoft.com article cited here wherever they disagree. It closes Q-A1, Q-A2, Q-A4, Q-A5, Q-A6, Q-B1, Q-B2 and Q-B4 in about ten minutes.
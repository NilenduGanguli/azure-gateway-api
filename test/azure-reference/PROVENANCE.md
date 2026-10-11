# Azure reference data: provenance and licences

This directory holds the official Microsoft REST specifications and a small set of genuine service
outputs. The gateway's tests validate its responses against them. Every file was downloaded from a
public GitHub raw URL pinned to a commit SHA, on 2026-10-11, and is vendored unchanged except
where a row says *extracted*. No credentials were used, and none are in these files: the SDK
recordings carry sanitized hosts and empty keys.

What uses what:

| Consumer | Files |
|---|---|
| `internal/azschema` (Swagger 2.0 validator, tests only) | `specs/*/*.json` |
| `test/conformance/azureshape_test.go` | the specs, to validate the gateway's poll and sync bodies (whole and split, both surfaces); the real outputs, to calibrate the validator |
| `internal/merge/realdata_test.go` | `real/**/*.analyzeResult.json` and the Read spec example, cut into parts by `internal/realcut` and merged back |
| `internal/docgen` tests | the real outputs' member order and presence, which the fakes' bodies follow |

## Specifications: Azure/azure-rest-api-specs (MIT)

Commit `65eb9311d304dcf9874d05fdec39ea65baae8db8`.
Base URL: `https://raw.githubusercontent.com/Azure/azure-rest-api-specs/65eb9311d304dcf9874d05fdec39ea65baae8db8/`

| File | Source path (append to the base URL) | sha256 |
|---|---|---|
| `specs/di-2024-11-30/DocumentIntelligence.json` | `specification/ai/data-plane/DocumentIntelligence/stable/2024-11-30/DocumentIntelligence.json` | `3f750834da9e3acc162b781711bf0eda02ab2bd8a30c2b4c6a62c6910e7f1bd0` |
| `specs/di-2024-11-30/example.GetAnalyzeDocumentResult.envelope.json` | *extracted*: `responses["200"].body` of `.../stable/2024-11-30/examples/GetAnalyzeDocumentResult.json` (prebuilt-invoice, 1 page) | `caf0d20fd79f33e875eab682a762f3ac4c43cba7c2853501e4e385cccb46bc58` |
| `specs/cv-v3.2/Ocr.json` | `specification/cognitiveservices/data-plane/ComputerVision/stable/v3.2/Ocr.json` | `b7d166be1731052ee68d2168d40666eb69ff485fb97aa07e2047ccf2acdd802b` |
| `specs/cv-v3.2/example.SuccessfulGetReadResult.envelope.json` | *extracted*: `responses["200"].body` of `.../stable/v3.2/examples/SuccessfulGetReadResult.json`. It is synthetic: `version` is `"v3.2"`, where the service says `"3.2.0"` | `01accfe0c59d741160158f92eff30c4f831a0130c831cab7664fae3f298ae0f0` |
| `specs/fr-2023-07-31/FormRecognizer.json` | `specification/cognitiveservices/data-plane/FormRecognizer/stable/2023-07-31/FormRecognizer.json` (the `/formrecognizer` family) | `9665bb58e33af22e55bae91d639f3ab4fa997f82fa91dbd10c244bc378e33415` |

Neither public spec describes the containers' `:syncAnalyze` routes. The gateway answers them with
the operation envelope, as the containers were observed to (docs/CONTAINER-BEHAVIOUR.md), so the
tests validate a sync body against the same definitions as a poll body.

## Real Document Intelligence 2024-11-30 prebuilt-layout output

These come from the Azure SDK live-test recordings in **Azure/azure-sdk-assets (MIT)**, which
record calls to the cloud service at `api-version=2024-11-30`. Each file is the `analyzeResult` of
the final `succeeded` poll body, *extracted* from the recording and pretty-printed with 2-space
indentation. Key order and scalar spelling are the recording's.

| File | Raw source URL | sha256 |
|---|---|---|
| `py_layout_multipage_table_span_pdf` | https://raw.githubusercontent.com/Azure/azure-sdk-assets/5f676bd1311ffb01abf7bdca7eb65ced13217089/python/sdk/documentintelligence/azure-ai-documentintelligence/tests/recordings/test_dac_analyze_layout.pyTestDACAnalyzeLayouttest_layout_multipage_table_span_pdf.json | `7f5111d4bf6ba7653637769faf47583ea628daf0e743b11fa73ab444841b078e` |
| `py_layout_multipage_transform` | …/5f676bd1311ffb01abf7bdca7eb65ced13217089/…/test_dac_analyze_layout.pyTestDACAnalyzeLayouttest_layout_multipage_transform.json | `3bab28efd2c64d2889b60adecb0ff78d34ec6cd2b60d34dfab9f395d9075d5b6` |
| `py_layout_figures` | …/5f676bd1311ffb01abf7bdca7eb65ced13217089/…/test_dac_analyze_layout.pyTestDACAnalyzeLayouttest_get_analyze_result_figures.json | `f54d807b5ed0a0cc7322872051525749845294630d47c8f092a9d3dc62da6a63` |
| `py_layout_barcodes` | …/5f676bd1311ffb01abf7bdca7eb65ced13217089/…/test_dac_analyze_layout.pyTestDACAnalyzeLayouttest_layout_url_barcode.json | `775f00574b9ea05e217344f7565d1d623219e66925a3cfa6593e78d0de00241b` |
| `py_layout_stylefont` | …/5f676bd1311ffb01abf7bdca7eb65ced13217089/…/test_dac_analyze_layout.pyTestDACAnalyzeLayouttest_layout_stream_transform_pdf.json | `50e38cfc488ca279804fdfca4c4c2acdb96b4655a7dc5551f6db88a51b9e3b30` |
| `java_layout_with_pages` | https://raw.githubusercontent.com/Azure/azure-sdk-assets/b4bb23cfcfb2a346fe3d205a1fe6d6dba3faacfe/java/sdk/documentintelligence/azure-ai-documentintelligence/src/test/resources/session-records/DocumentIntelligenceClientTest.analyzeLayoutWithPages%5B1%5D.json | `2093f413756e7ffeb72281841fccd403f2cae6e38f1b382b91c288874aa10f43` |
| `java_layout` | …/b4bb23cfcfb2a346fe3d205a1fe6d6dba3faacfe/…/session-records/DocumentIntelligenceClientTest.analyzeLayout%5B1%5D.json | `b562653173c25e0e19e9e24088bf086aa05bca6984e093be2a3b1dfd16d56ff3` |
| `js_layout_selection_marks` | https://raw.githubusercontent.com/Azure/azure-sdk-assets/557bf0ed19e4c3fabbbc9cb469a7599459e99a14/js/sdk/documentintelligence/ai-document-intelligence-rest/recordings/node/documentintelligenceclient_content_analysis/recording_with_selection_marks.json | `ea8266065dfdac9fe523c47a84421cdd70f79cba236427caf09a0bc9f0aead65` |
| `py_layout_multipage_table_span_pdf.exchange.json` | the same Python recording: every HTTP exchange, with its response headers (the 202, five running polls with `Retry-After: 1`, the final poll) | `2cc705e08d1b6c3a6a507ccebfa27a42e9b364729c4ca72b379147a5274868f6` |

`…` stands for the full prefix shown in the row above it with the same commit. The assets tags were
read from `assets.json` in Azure/azure-sdk-for-python@afec8fa7e25687daf70802f427628ddb3e75cba2,
Azure/azure-sdk-for-java@41e887c0e710e1300fb14be6957eaf6a3cadd483 and
Azure/azure-sdk-for-js@2fb339538c9e5011bd3d01fd358c20191f133c35.

`py_layout_figures` and `py_layout_barcodes` carry the figure id `"Sanitized"`, a test-proxy
artefact. The service writes `"<page>.<n>"`, as `js_layout_selection_marks` shows (`"1.1"`,
`"1.2"`), and the tests restore that id before using them.

One third-party capture is included because it is the only multi-page real output with a figure,
and the only markdown one:

| File | Raw source URL | Licence | sha256 |
|---|---|---|---|
| `thirdparty_docler_markdown_2page` | https://raw.githubusercontent.com/phil65/docler/71eed1e9658c4028600e728ae59ea6245b7fc70a/tests/responses/azure-response.json (vendored as fetched; it is already a bare analyzeResult, saved through an SDK, with no `styles` member) | MIT (phil65/docler) | `7cdd545e63b8374266130b3c1fe9e46622d88c5f9d90cb8a49211659be3c4adf` |

## Real Read v3.2 output

From the track-1 Computer Vision 7.0.1 session records in **Azure/azure-sdk-for-net (MIT)**, tag
`Microsoft.Azure.CognitiveServices.Vision.ComputerVision_7.0.1`, commit
`bcb4cf046f8ecacf0b258f66699a38d5686a1b13`. The raw URL prefix is
`https://raw.githubusercontent.com/Azure/azure-sdk-for-net/bcb4cf046f8ecacf0b258f66699a38d5686a1b13/sdk/cognitiveservices/Vision.ComputerVision/tests/SessionRecords/VisionReadTests/`.
Each analyzeResult is *extracted* from the final `succeeded` poll.

| File | Raw file | Notes | sha256 |
|---|---|---|---|
| `net_ReadPdfFileInStreamTest.analyzeResult.json` | `ReadPdfFileInStreamTest.json` | PDF, unit `inch`, version `3.2.0`, modelVersion `2021-04-12` | `0ef6f4fb0d66543ca7a55bec08c689483a6b13ee76faaa8bde8a9d3f34bb3746` |
| `net_ReadPdfFileInStreamTest.exchange.json` | the same | the 202 and the polls with their headers (no `Retry-After` on running polls) | `ed38d4a2fcf05b341991ac055bf33b4f8779da31f18894df16d1322a4868a4c0` |
| `net_ReadFileInStreamTest.analyzeResult.json` | `ReadFileInStreamTest.json` | image, unit `pixel` | `cf5ad18609bd3fc5f49a84b32fe173e18da7984a85b497124a06dd615e5107c4` |
| `net_ReadLatestNaturalTest.analyzeResult.json` | `ReadLatestNaturalTest.json` | image, `readingOrder=natural` | `15dc505593179b6a2c01ff098e9c4e094021f20ddae4c37eac04fbfb801e5ec2` |

## Facts the tests rely on

Taken from the files above:

- **DI analyzeResult member order:** `apiVersion, modelId, stringIndexType, content, pages, tables, paragraphs, styles, contentFormat, sections, figures`. `styles` is always present, `[]` included. `figures`, and a page's `selectionMarks` and `barcodes`, are left out when there are none.
- **DI items:**
  - Page: `pageNumber, angle, width, height, unit, words, [selectionMarks], lines, spans`.
  - Paragraph: `spans, boundingRegions, [role], content`.
  - Table: `rowCount, columnCount, cells, boundingRegions, spans, [caption]`.
  - Figure: `id, boundingRegions, spans, elements, [caption]`. A caption is `content, boundingRegions, spans, elements`, and the figure's own `elements` leave the caption out.
- **DI pages:** pages are joined by a single `\n`. A blank page has `words: []`, `lines: []` and one zero-length span. The furthest end of the pages' spans is the content's length in the unit, in every file.
- **DI sections:** a root's spans cover only what sections hold. Page headers, footers and page numbers are left out, and so is a blank page, so a root can stop short of the content's end or come in several spans (`py_layout_multipage_transform`). Where only a page separator lies between two runs, the span runs on (`py_layout_multipage_table_span_pdf`).
- **Read v3.2:** `version` is `"3.2.0"`. A page is `page, angle, width, height, unit, lines`. A line is `boundingBox, text, appearance{style{name, confidence}}, words`. PDFs are in `inch` with 4-decimal coordinates; images are in `pixel` with integers. The failed-operation envelope has no `error` member: `ReadOperationResult` declares none.

## Licences

- Azure/azure-rest-api-specs, Azure/azure-sdk-assets, Azure/azure-sdk-for-net: MIT License, Copyright (c) Microsoft Corporation.
- phil65/docler: MIT License, Copyright (c) phil65.

The MIT License: Permission is hereby granted, free of charge, to any person obtaining a copy of this
software and associated documentation files (the "Software"), to deal in the Software without
restriction, including without limitation the rights to use, copy, modify, merge, publish,
distribute, sublicense, and/or sell copies of the Software, and to permit persons to whom the
Software is furnished to do so, subject to the following conditions: The above copyright notice
and this permission notice shall be included in all copies or substantial portions of the Software.
THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR IMPLIED, INCLUDING BUT
NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY, FITNESS FOR A PARTICULAR PURPOSE AND
NONINFRINGEMENT. IN NO EVENT SHALL THE AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES
OR OTHER LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM, OUT OF OR IN
CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE SOFTWARE.

Files left out on purpose: Read 3.2 captures with modelVersion `2022-04-30` exist only in
repositories whose licence is GPL-2.0 or undeclared, and the bcgov markdown capture is Apache-2.0.
Their facts (the `appearance` member, inch units for PDFs) also hold in the MIT files above.

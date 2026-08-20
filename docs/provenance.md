# Project provenance and licensing

## Relationship to the original Tracetest

This project is a new-generation fork and reimplementation of the original
[Tracetest](https://github.com/kubeshop/tracetest), led by one of its original
maintainers. It continues the same core mission: using observability data to
verify behavior that a trigger result alone cannot expose.

The implementation is being rebuilt around a smaller declarative architecture
rather than preserving the original server and cloud architecture. “Fork” here
describes project lineage, maintainership, mission, and product direction; this
repository is not intended to provide source-level or API compatibility with
every previous Tracetest component.

The implementation intentionally differs in scope:

- one CLI process rather than a persistent control plane;
- a Lamplight-specific DSL, using HCL syntax, as the source of truth;
- an extensible trigger model, currently implemented by `http_request`;
- tracing backends adapted from the original supported set, using direct
  provider queries or an embedded OTLP/HTTP receiver;
- local artifacts and stdout rather than a database or dashboard;
- no copied compatibility promise with the original project's DSL, API, UI, or
  deployment model.

Users should rely only on this repository's
[DSL and CLI reference](reference.md), not on documentation for the
original project.

## Implementation origin

The current code was implemented for this repository around a new data model,
DSL contract, execution engine, Tempo adapter, result schema, and tests. Its
lineage with the original project does not imply source-code or API
compatibility.

The backend inventory and externally observable request/response behavior were
cross-checked against `kubeshop/tracetest` commit
`64eb49ff2037e0ba5d16237278d984758e7c7309`. The agent implementation at that
revision is covered by the Tracetest Community License, so Lamplight does not
copy those source files. The adapters in `internal/datasource` are original,
smaller implementations against the providers' wire formats and Lamplight's
`model.DataStore` contract.

When contributing code influenced by another implementation, identify the
source and license in the pull request before copying or adapting it. Do not
paste code with uncertain provenance.

## Direct Go dependencies

The module's direct third-party dependencies are:

| Dependency | Purpose | Upstream license |
| --- | --- | --- |
| `github.com/hashicorp/hcl/v2` | HCL parsing, syntax diagnostics, and expression evaluation | Mozilla Public License 2.0 |
| `github.com/modelcontextprotocol/go-sdk` | MCP server transport and protocol types | MIT License |
| `github.com/zclconf/go-cty` | Typed values and standard HCL-compatible functions | MIT License |
| `go.opentelemetry.io/proto/otlp` | Standard OTLP trace request types | Apache License 2.0 |
| `google.golang.org/protobuf` | OTLP protobuf and proto-JSON encoding | BSD 3-Clause License |

Transitive dependencies and exact versions are recorded in `go.mod` and
`go.sum`. Release automation should generate a complete dependency and license
inventory from the locked module graph rather than treating this table as a
substitute for a software bill of materials.

Direct-query providers are accessed over their HTTP APIs. Collector-backed
providers send standard OTLP/HTTP protobuf or JSON to Lamplight's embedded
receiver. This repository does not embed provider source code.

## Project license

This repository is distributed under the [MIT License](../LICENSE). The license
permits use, copying, modification, merging, publication, distribution,
sublicensing, and sale, subject to preservation of the copyright and license
notice. It includes the standard warranty and liability disclaimer.

Contributions intentionally submitted for inclusion are expected to use the
same license unless explicitly agreed otherwise. Contributors retain copyright
in their contributions.

The original Tracetest repository contains code under MIT and the Tracetest
Community License, selected by file or containing directory. This repository's
MIT license does not relicense upstream material. Before copying or adapting
upstream code, contributors must inspect that material's applicable license and
preserve every required notice. The current implementation was developed
independently and does not include known TCL-licensed source.

Release maintainers should continue to verify dependency licenses, generate a
software bill of materials, provide a private vulnerability-reporting route,
and review naming and trademarks. This document records technical provenance
and is not legal advice.

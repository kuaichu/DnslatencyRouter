# NextTrace for preferred-IP route tracing

This directory holds runtime files for **unmodified NextTrace v1.7.3** from
https://github.com/nxtrace/NTrace-core/releases/tag/v1.7.3 .
`manifest.json` pins upstream asset URLs and SHA-256 digests. The binary files
are local deployment artifacts, ignored by Git; restore them with:

```text
python scripts/fetch-nexttrace.py
```

Available: Linux amd64/arm64, macOS arm64, Windows amd64. Deploy the appropriate
`nexttrace_<os>_<arch>` file under `tools/nexttrace/` next to the controller or
Agent working directory. The Agent installer fetches its matching file from
the controller and validates the pinned digest. Linux/macOS require raw-socket
privileges (the existing Agent services run as root). Windows TCP MTR also
requires the official WinDivert runtime; run `nexttrace_windows_amd64.exe --init`
as administrator if you choose to enable it. Windows drivers are not installed
automatically by this project.

NextTrace is licensed under GPL-3.0; see `LICENSE`. A matching upstream source
archive can be downloaded and restored as `source-v1.7.3.tar.gz` with
`python scripts/fetch-nexttrace.py`; it is intentionally ignored by Git. When
present, the archive is available from the controller at
`/api/agent/nexttrace/source`, with `/api/agent/nexttrace/LICENSE` providing the
license. Its canonical source is
https://github.com/nxtrace/NTrace-core/tree/v1.7.3 .

The application invokes NextTrace as a separate process, never imports its Go
packages, and does not modify its executable or remove its notices.

Command used: regular TCP traceroute to the configured business port, 10
measurements per hop, 30 hops, 1500 ms per-probe timeout, a 90-second process
limit, `--map --no-color --data-provider NextTrace-API`. Here `--map` is
NextTrace's flag to **disable** map output. NextTrace-API supplies hop ASN,
operator and location; reverse DNS supplies PTR names. Missing GeoIP metadata
does not establish a routing failure: use the stop reason and endpoint replies
to interpret the trace. Results are diagnostic snapshots, not inputs to DNS
selection; a middle hop may rate-limit ICMP while forwarding traffic normally.

# Third-party notices

hy2tun itself is licensed under the MIT License (see [`LICENSE`](LICENSE)).
hy2tun uses third-party Go modules in its source tree and release binaries.
This file lists the modules declared in `go.mod` and the licenses that apply to
them. The full license texts are in [`LICENSES/`](LICENSES/).

Versions below match the `go.mod` used for this release line. Third-party code
is used as published; hy2tun does not vendor or modify these modules.

## Mozilla Public License 2.0 component

### `github.com/fatedier/yamux` v0.3.0

- License: Mozilla Public License 2.0 (MPL-2.0).
- Copyright: Copyright IBM Corp. 2014, 2025.
- Modification status: unmodified. hy2tun uses the published module as-is; no
  files in this module were changed.
- Source: <https://github.com/fatedier/yamux/tree/v0.3.0> (tag `v0.3.0`).
- Source offer: the complete corresponding source of the MPL-covered module is
  the upstream `v0.3.0` tag linked above. The same source archive is available
  from the Go module proxy at
  <https://proxy.golang.org/github.com/fatedier/yamux/@v/v0.3.0.zip>.
- License text: [`LICENSES/MPL-2.0.txt`](LICENSES/MPL-2.0.txt); a durable copy
  is also published at <https://www.mozilla.org/MPL/2.0/>.

## Direct dependencies

| Module | Version | License | Copyright / notice | Source |
| --- | --- | --- | --- | --- |
| `github.com/apernet/hysteria/core/v2` | v2.13.0 | MIT | Copyright 2023 Toby | <https://github.com/apernet/hysteria> |
| `github.com/apernet/hysteria/extras/v2` | v2.13.0 | MIT | Copyright 2023 Toby | <https://github.com/apernet/hysteria> |
| `github.com/apernet/quic-go` | v0.63.1-0.20261004180939-a10df75c260c | MIT | Copyright (c) 2016 the quic-go authors & Google, Inc. | <https://github.com/apernet/quic-go> |
| `github.com/fatedier/yamux` | v0.3.0 | MPL-2.0 | Copyright IBM Corp. 2014, 2025 | <https://github.com/fatedier/yamux> |
| `github.com/stretchr/testify` | v1.12.1 | MIT | Copyright (c) 2012-2020 Mat Ryer, Tyler Bunnell and contributors. Test-only. | <https://github.com/stretchr/testify> |
| `golang.org/x/crypto` | v0.58.0 | BSD-3-Clause | Copyright 2009 The Go Authors. | <https://go.googlesource.com/crypto> |
| `golang.org/x/net` | v0.62.0 | BSD-3-Clause | Copyright 2009 The Go Authors. | <https://go.googlesource.com/net> |
| `golang.org/x/time` | v0.16.0 | BSD-3-Clause | Copyright 2009 The Go Authors. | <https://go.googlesource.com/time> |
| `gopkg.in/yaml.v3` | v3.0.1 | MIT and Apache-2.0 (dual) | MIT portions: Copyright (c) 2006-2010 Kirill Simonov; Copyright (c) 2006-2011 Kirill Simonov. Apache-2.0 portions: Copyright (c) 2011-2019 Canonical Ltd. See the module `LICENSE` and `NOTICE`. | <https://github.com/go-yaml/yaml> |

## Indirect build dependencies

These modules are selected by `go.mod` for packages imported by hy2tun or its
tests. They are listed here with the same attribution requirements.

| Module | Version | License | Copyright / notice | Source |
| --- | --- | --- | --- | --- |
| `github.com/andybalholm/brotli` | v1.1.0 | MIT | Copyright (c) 2009, 2010, 2013-2016 by the Brotli Authors. | <https://github.com/andybalholm/brotli> |
| `github.com/klauspost/compress` | v1.17.9 | BSD-3-Clause (main); Apache-2.0 (`gzhttp/*`); MIT (`s2/cmd/internal/readahead/*`, `s2/cmd/internal/filepathx/*`); BSD-3-Clause (`snappy/*`, `internal/snapref/*`) | Copyright (c) 2012 The Go Authors; Copyright (c) 2019 Klaus Post; Copyright (c) 2015 Klaus Post; Copyright (c) 2011 The Snappy-Go Authors; Copyright 2016 The filepathx Authors. See [`LICENSES/klauspost-compress.txt`](LICENSES/klauspost-compress.txt). | <https://github.com/klauspost/compress> |
| `github.com/quic-go/qpack` | v0.6.0 | MIT | Copyright 2019 Marten Seemann | <https://github.com/quic-go/qpack> |
| `github.com/refraction-networking/utls` | v1.8.2 | BSD-3-Clause | Copyright (c) 2009 The Go Authors. All rights reserved. | <https://github.com/refraction-networking/utls> |
| `github.com/stretchr/objx` | v0.5.3 | MIT | Copyright (c) 2014 Stretchr, Inc.; Copyright (c) 2017-2018 objx contributors. Test-only. | <https://github.com/stretchr/objx> |
| `go.yaml.in/yaml/v3` | v3.0.5 | MIT and Apache-2.0 (dual) | MIT portions: Copyright (c) 2006-2010 Kirill Simonov; Copyright (c) 2006-2011 Kirill Simonov. Apache-2.0 portions: Copyright (c) 2011-2019 Canonical Ltd; NOTICE: Copyright 2011-2016 Canonical Ltd. | <https://pkg.go.dev/go.yaml.in/yaml/v3> |
| `golang.org/x/exp` | v0.0.0-20240506185415-9bf2ced13842 | BSD-3-Clause | Copyright (c) 2009 The Go Authors. All rights reserved. | <https://go.googlesource.com/exp> |
| `golang.org/x/sys` | v0.49.0 | BSD-3-Clause | Copyright 2009 The Go Authors. | <https://go.googlesource.com/sys> |
| `golang.org/x/text` | v0.43.0 | BSD-3-Clause | Copyright 2009 The Go Authors. | <https://go.googlesource.com/text> |

## License texts

- MIT: [`LICENSES/MIT.txt`](LICENSES/MIT.txt). The copyright notice for each
  MIT component is listed in the tables above.
- BSD-3-Clause (Go Authors text): [`LICENSES/BSD-3-Clause.txt`](LICENSES/BSD-3-Clause.txt).
  Applies to `golang.org/x/crypto`, `golang.org/x/net`, `golang.org/x/time`,
  `golang.org/x/sys`, and `golang.org/x/text`.
- BSD-3-Clause (Google Inc. text): [`LICENSES/BSD-3-Clause-Google-Inc.txt`](LICENSES/BSD-3-Clause-Google-Inc.txt).
  Applies to `golang.org/x/exp` and `github.com/refraction-networking/utls`.
- BSD-3-Clause, Apache-2.0, and MIT sections: [`LICENSES/klauspost-compress.txt`](LICENSES/klauspost-compress.txt)
  for `github.com/klauspost/compress`.
- Apache-2.0: [`LICENSES/Apache-2.0.txt`](LICENSES/Apache-2.0.txt). Applies to
  the Apache-2.0 portions of `gopkg.in/yaml.v3` and `go.yaml.in/yaml/v3`.
- MPL-2.0: [`LICENSES/MPL-2.0.txt`](LICENSES/MPL-2.0.txt). Applies to
  `github.com/fatedier/yamux`; see the source offer above.

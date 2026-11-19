# Third-party notices

This repository includes and depends on the following third-party components.

## Bundled JavaScript

### Chart.js

- File: `admin/web/chart.min.js`
- Version: 4.4.4 (with `@kurkle/color` 0.3.2)
- License: MIT
- Copyright (c) 2024 Chart.js Contributors; (c) 2023 Jukka Kurkela
- Source: <https://github.com/chartjs/Chart.js>

Chart.js is distributed under the MIT license:

> Permission is hereby granted, free of charge, to any person obtaining a copy
> of this software and associated documentation files (the "Software"), to deal
> in the Software without restriction, including without limitation the rights
> to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
> copies of the Software, and to permit persons to whom the Software is
> furnished to do so, subject to the following conditions:
>
> The above copyright notice and this permission notice shall be included in all
> copies or substantial portions of the Software.
>
> THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
> IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
> FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
> AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
> LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
> OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
> SOFTWARE.

The full license text is also embedded in the header of `admin/web/chart.min.js`.

## Container images

| Image                       | Usage                        | License / source                                            |
| --------------------------- | ---------------------------- | ----------------------------------------------------------- |
| `dperson/openvpn-client`    | OpenVPN client container     | <https://github.com/dperson/openvpn-client> (no longer actively updated) |
| `serjs/go-socks5-proxy`     | SOCKS5 proxy container       | <https://github.com/serjs/go-socks5-proxy>                  |
| `golang` (build stage)      | Builds the admin binary      | <https://go.dev/dl/> (BSD-style license)                    |
| `docker:29-cli` (base image)| Runtime for the admin panel (docker CLI + compose plugin) | <https://docs.docker.com/> (Apache-2.0)                     |

## Go dependencies

Go module dependencies of the admin panel (`admin/go.mod`) are declared in
that file; their licenses are those of the respective upstream projects
(`modernc.org/sqlite`, `golang.org/x/net` and their indirect dependencies).

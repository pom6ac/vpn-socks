# Using the proxy

[Back to README](../README.md)

Any client that supports SOCKS5 can use `127.0.0.1:1080`. Let the proxy
resolve host names (SOCKS5 with remote DNS, `socks5h://`), otherwise name
lookups happen on the host outside the VPN.

```sh
# curl
curl --socks5-hostname 127.0.0.1:1080 https://example.com

# Tools that honour ALL_PROXY (socks5h = resolve names through the proxy)
ALL_PROXY=socks5h://127.0.0.1:1080 some-tool

# With SOCKS_REQUIRE_AUTH=true
curl --socks5-hostname 127.0.0.1:1080 --proxy-user "$PROXY_USER:$PROXY_PASSWORD" https://example.com
```

- **Firefox**: Settings → Network Settings → Manual proxy configuration →
  SOCKS Host `127.0.0.1`, Port `1080`, SOCKS v5, and enable "Proxy DNS when
  using SOCKS v5".
- **Chromium-based browsers**: start with
  `--proxy-server="socks5://127.0.0.1:1080"` (Chromium then resolves names
  through the proxy), or use a proxy-switching extension.
- **Only some sites through the VPN**: point the browser at a PAC file of your
  own (none is shipped), for example:

  ```javascript
  function FindProxyForURL(url, host) {
    if (dnsDomainIs(host, ".example.com")) return "SOCKS5 127.0.0.1:1080";
    return "DIRECT";
  }
  ```

- **System-wide SOCKS proxy (macOS)**:
  `networksetup -setsocksfirewallproxy "Wi-Fi" 127.0.0.1 1080`
  (turn off with `networksetup -setsocksfirewallproxystate "Wi-Fi" off`).
  Only applications that honour the system proxy use it.

Browsers generally do not support SOCKS5 username/password; with
`SOCKS_REQUIRE_AUTH=true` use clients such as `curl`.

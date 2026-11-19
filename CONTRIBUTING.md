# Contributing

Thanks for your interest in contributing!

## Development

Go code lives in `admin/`. Common commands from the repository root:

```sh
make test    # go test -race ./...
make lint    # golangci-lint run (config: admin/.golangci.yml)
make shell   # kill switch script tests (+ shellcheck when installed)
make cover   # coverage, must stay >= 85%
make build   # go build with -trimpath
```

Docker changes can be validated with:

```sh
docker compose --env-file .env.example config
```

## Commit style

Use short, imperative commit messages, optionally prefixed with the area:

```
admin: fix false DEGRADED in tunnel_ping probe
docker-compose: pin images by digest
docs: add SECURITY policy
```

## Pull requests

- Keep PRs focused; one topic per PR.
- Run `make test` and `make lint` before pushing (`make shell` when you
  change `ovpn/killswitch-entrypoint.sh`).
- Do not include any real VPN configs (`*.ovpn`), credentials, or private
  hostnames/IPs in commits, issues or logs.

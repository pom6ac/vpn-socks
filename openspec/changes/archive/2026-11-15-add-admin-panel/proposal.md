> **Архивное изменение (историческая запись).** Описывает первоначальный
> дизайн. В текущей публичной версии: htmx не используется (UI — `app.js` +
> `fetch`/SSE), эндпоинт `/api/rebuild` и кнопка Full Rebuild удалены
> (пересборка — `make rebuild` на хосте), `docker-compose.yml` не монтируется
> в `/compose`: проект монтируется read-only по тому же пути
> (`HOST_PROJECT_DIR`), recreate выполняет
> `docker compose --project-directory $HOST_PROJECT_DIR -p $COMPOSE_PROJECT_NAME -f $COMPOSE_FILE up -d --force-recreate ovpn socks5`,
> вывод compose пишется только в лог admin. Актуальное поведение — в `README.md`.

## Why

VPN-шлюз периодически перестаёт работать по множеству причин (упавший контейнер, зависший туннель, недоступность 2-го хопа, DNS-резолв через туннель, упавший SOCKS5-прокси). Сейчас диагностика — ручной `docker logs` в терминале. Нужен локальный веб-интерфейс на localhost, показывающий состояние стека в реальном времени и позволяющий перезапускать компоненты без захода в CLI.

## What Changes

- Новый контейнер `admin` в `docker-compose.yml` — Go-бинарий с embedded web UI (htmx + SSE), слушает на `127.0.0.1:8080`.
- **Read-only мониторинг (итерация 1):**
  - Статус контейнеров `ovpn` и `socks5` (up/down/restarting, uptime, счётчик рестартов).
  - Live-tail логов обоих контейнеров через SSE с подсветкой ошибок (регекс по типичным OpenVPN/SOCKS5 маркерам).
  - Health-checks туннеля: ping 2-го хопа через SOCKS5, DNS-резолв через туннель, HTTP-доступность эталонных сайтов через прокси.
  - Простой график трафика (байт/сек через интерфейс `tun`).
- **Рычаги управления (итерация 2):**
  - Restart отдельного контейнера (Docker API).
  - Reconnect OpenVPN (SIGUSR1 в контейнер `ovpn`).
  - Пересоздать стек (`docker compose up -d --force-recreate ovpn socks5`).
  - Полная пересборка (`docker compose down && docker compose build --no-cache && docker compose up -d`) — отдельная кнопка.
- **Метрики (итерация 3):**
  - Хранение истории метрик в SQLite (embedded).
  - Графики за час/день.
  - Браузерные уведомления при падении туннеля.
- `admin` живёт в bridge-сети, независимой от `ovpn` — рестарт ovpn не гасит панель. Альтернатива `network_mode: service:ovpn` отклонена: при рестарте ovpn netns пересоздаётся и admin теряет сеть (подробно в design.md D1). Health-checks идут через `host.docker.internal:1080` (published SOCKS5 порт).
- Docker socket монтируется в `admin` (rw) для Docker API и events-стрима.
- Трафик `tun` читается через `docker exec` в ovpn (admin в bridge-сети не видит `/proc/net/dev` ovpn напрямую).
- Полная пересборка запускается в новой сессии (`Setsid`), переживая смерть admin.

## Capabilities

### New Capabilities
- `container-monitoring`: статус контейнеров (up/down/restarting, uptime, рестарт-счётчик), live-tail логов через SSE с подсветкой ошибок.
- `tunnel-health`: health-checks туннеля — ping 2-го хопа, DNS-резолв, HTTP-доступность сайтов через SOCKS5-прокси.
- `traffic-metrics`: сбор и отображение трафика через интерфейс `tun`, хранение в SQLite, графики за час/день.
- `stack-control`: рычаги управления — restart контейнера, reconnect OpenVPN, пересоздать стек, полная пересборка.

### Modified Capabilities
<!-- Нет существующих specs — все_capabilities новые -->

## Impact

- **docker-compose.yml**: добавляется сервис `admin` (сборка из `./admin/Dockerfile`, монтирование docker socket, `extra_hosts: host.docker.internal:host-gateway`).
- **Новая директория `admin/`**: Go-код, Dockerfile, embedded статика (HTML/CSS/JS), `go.mod`.
- **Зависимости Go**: `docker/docker/client` (Docker API), `chi` или `net/http` (HTTP routing), `mattn/go-sqlite3` или `modernc.org/sqlite` (SQLite), htmx (CDN или vendored).
- **Без изменений** в существующих сервисах `ovpn` и `socks5` — их конфигурация не трогается.
- Безопасность: панель только на `127.0.0.1:8080`, без аутентификации (локальный инструмент).

> **Архивное изменение (историческая запись).** Описывает первоначальный
> дизайн. В текущей публичной версии: htmx не используется (UI — `app.js` +
> `fetch`/SSE), эндпоинт `/api/rebuild` и кнопка Full Rebuild удалены
> (пересборка — `make rebuild` на хосте), `docker-compose.yml` не монтируется
> в `/compose`: проект монтируется read-only по тому же пути
> (`HOST_PROJECT_DIR`), recreate выполняет
> `docker compose --project-directory $HOST_PROJECT_DIR -p $COMPOSE_PROJECT_NAME -f $COMPOSE_FILE up -d --force-recreate ovpn socks5`,
> вывод compose пишется только в лог admin. Актуальное поведение — в `README.md`.

## Context

Локальный VPN-шлюз из двух контейнеров (`ovpn` + `socks5`) в Docker Compose на macOS. Прокси на `127.0.0.1:1080`, трафик заворачивается через PAC-скрипт. При сбое диагностика — ручной `docker logs`. Нужна локальная веб-панель на localhost для мониторинга и управления.

Ключевое ограничение: на macOS Docker Desktop нет host-networking. Контейнер `admin` не может просто обратиться к `127.0.0.1:1080` хоста. Кроме того, `admin` должен выживать при рестарте `ovpn` — иначе панель гаснет именно когда нужна.

## Goals / Non-Goals

**Goals:**
- Live-мониторинг контейнеров и туннеля в браузере на `localhost:8080`.
- Health-checks, репрезентативные для реального пути трафика (через SOCKS5).
- Рычаги управления: restart контейнера, reconnect OpenVPN, пересоздание стека.
- Независимость `admin` от рестартов `ovpn`/`socks5`.
- Один бинарь Go с embedded статикой — минимальный образ, ноль внешних рантаймов.
- Хранение метрик в SQLite для графиков за час/день.

**Non-Goals:**
- Аутентификация (локальный инструмент на localhost).
- Удалённый доступ к панели (только `127.0.0.1`).
- Мониторинг нескольких VPN-стеков.
- Web-based редактор `.ovpn` конфигов.
- Метрики производительности (CPU/RAM) — только здоровье туннеля и трафик.

## Decisions

### D1: admin в bridge-сети, health-checks через host.docker.internal:1080

```
┌─────────────────────────────────────────────────────────────┐
│  Docker Desktop (macOS)                                     │
│                                                             │
│  ┌── default bridge net ──────────┐                         │
│  │  admin (Go)                    │                         │
│  │  127.0.0.1:8080:8080           │                         │
│  │  health ──▶ host.docker.       │                         │
│  │             internal:1080      │                         │
│  └────────────────┬───────────────┘                         │
│                   │                                         │
│  ═════════════════╪══════════════ published port boundary    │
│                   │                                         │
│  ┌── ovpn netns ──┴──────────────┐                          │
│  │  ovpn    1080 ◀────────────── │  ◀── host publishes 1080  │
│  │  socks5  (service:ovpn)       │                           │
│  └───────────────────────────────┘                           │
└─────────────────────────────────────────────────────────────┘
```

**Почему не `network_mode: service:ovpn`**: при рестарте ovpn netns пересоздаётся, admin теряет сеть и гаснет. Панель недоступна именно в момент восстановления — контрпродуктивно.

**Почему не сайдкар-зонд**: over-engineering. Один extra-контейнер + протокол между зондом и admin. Крюк через host-published порт для health-probe (раз в 1-5 сек) не критичен.

**Альтернатива рассмотрена**: хостовый Go-бинарь (не в Docker). Плюсы: прямой доступ к `127.0.0.1:1080`, `docker compose` нативно. Отклонено: пользователь хочет всё через `docker compose up`.

### D2: Go + htmx + SSE, static embed

```
┌─────────────────────────────────────────────────────────┐
│  Go binary (single container)                           │
│                                                         │
│  net/http (or chi)                                      │
│  ├── /                   → embed.FS → index.html        │
│  ├── /static/*           → embed.FS → htmx.min.js, css  │
│  ├── /api/status         → JSON: containers[]           │
│  ├── /api/logs/{c}       → SSE stream (docker logs -f)  │
│  ├── /api/health         → JSON: tunnel probes          │
│  ├── /api/metrics/traffic→ JSON: bps (tun interface)    │
│  ├── /api/metrics/history→ JSON: SQLite time-series     │
│  ├── /api/restart/{c}    → POST → Docker API restart    │
│  ├── /api/reconnect      → POST → SIGUSR1 to ovpn       │
│  ├── /api/recreate       → POST → compose up --force    │
│  └── /api/rebuild        → POST → compose build --no-cache│
│                                                         │
│  embed.FS: index.html, app.js, style.css, htmx.min.js   │
└─────────────────────────────────────────────────────────┘
```

> **Примечание (публичная версия):** эндпоинт `/api/rebuild` удалён из
> админки. Полная пересборка выполняется на хосте командой `make rebuild`
> (`docker compose build admin && docker compose up -d admin`).

**Почему htmx, не SPA**: один бинарь, ноль node-тулчейна. htmx + SSE покрывает live-tail и partial updates. SPA (Svelte/Vue) — оверкилл для 4-5 виджетов.

**Графики — Chart.js (vendored)**: htmx не умеет canvas-графики. Самописный canvas-график — трудоёмко и выглядит грубо. Chart.js (~60KB min) vendored в `admin/web/chart.min.js` через `embed.FS` — ноль CDN-зависимости, ноль node-билда. Используется для real-time traffic chart и исторических графиков.

**Почему SSE, не WebSocket**: однонаправленный стрим логов/метрик. SSE проще, работает через `EventSource` в браузере, не нужна библиотека. Для рычагов — обычные POST.

### D3: Docker API + os/exec для рычагов

```
Действие              Метод                           Что использует
─────────────────    ────────────────────────────   ──────────────────────
restart контейнера    Docker API ContainerRestart    docker/client
reconnect OpenVPN     Docker API ContainerKill       docker/client
                      (signal=SIGUSR1)
пересоздать стек      os/exec: docker compose         docker CLI в образе
                      up -d --force-recreate          stdout/stderr → ответ
                      ovpn socks5
полная пересборка     os/exec (Setsid):               docker CLI в образе
                      docker compose down &&          ВЫПОЛНЯЕТСЯ В НОВОЙ
                      build --no-cache && up -d       СЕССИИ — переживает
                                                      смерть admin
```

**Почему Docker API для restart/reconnect, но os/exec для compose**: Docker API даёт типобезопасность и events-стрим. `compose up --force-recreate` — это compose-level операция, Docker API не знает про compose project. Проще вызвать CLI.

**rebuild через `syscall.SysProcAttr{Setsid: true}`**: `docker compose down` убьёт admin (SIGTERM). Если запускать compose-команду обычным `os/exec`, она умрёт вместе с admin, и `build --no-cache && up -d` не выполнятся. Решение — запустить команду в новой сессии (в публичной версии этот механизм заменён на `make rebuild` на хосте):

```go
cmd := exec.Command("sh", "-c", "docker compose down && docker compose build --no-cache && docker compose up -d")
cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
cmd.Start()  // не Wait — admin ответит 202 и умрёт
```

Дочерний процесс в новой сессии переживает SIGTERM admin. Браузер теряет SSE, переподключается после подъёма admin.

**SIGUSR1 не требует SYS_PTRACE**: Docker API `ContainerKill(signal=SIGUSR1)` отправляет сигнал PID 1 в контейнере через Docker daemon — не через ptrace. Дополнительные capabilities не нужны.

**Захват stdout/stderr compose-команд**: для `recreate` (admin живёт) — вывод захватывается и возвращается в HTTP-ответе. Для `rebuild` (admin умрёт) — вывод пишется в лог admin (stdout) до смерти; после подъёма пользователь видит результат через `docker logs vpn-admin` (в публичной версии rebuild выполняется через `make rebuild` на хосте).

**Образ admin**: multi-stage: build в `golang:alpine`, runtime в `docker:cli` + `docker compose plugin`. Проверить, что `docker:cli` включает compose v2 plugin (на некоторых версиях нужно ставить отдельно).

### D4: SQLite (modernc.org/sqlite) для метрик

```
Schema (минимум):

  metrics (
    id        INTEGER PRIMARY KEY,
    ts        DATETIME DEFAULT CURRENT_TIMESTAMP,
    kind      TEXT,        -- 'rx_bytes', 'tx_bytes', 'tunnel_ping_ms',
                           -- 'dns_resolve_ms', 'http_check_ms'
    value     REAL
  )

  container_events (
    id        INTEGER PRIMARY KEY,
    ts        DATETIME DEFAULT CURRENT_TIMESTAMP,
    container TEXT,        -- 'ovpn', 'socks5'
    event     TEXT         -- 'start', 'die', 'restart', 'health'
  )
```

**Почему modernc.org/sqlite, не mattn/go-sqlite3**: pure Go, без CGO. Проще кросс-компиляция (macOS arm64 → linux amd64 в Docker). Чуть медленнее, но для локальной панели с записью раз в секунду — неактуально.

**Retention**: храним 7 дней, старшее — `DELETE WHERE ts < datetime('now', '-7 days')` раз в час (не при каждой записи — лишний I/O).

### D5: Health-check матрица

```
Probe              Что делает                                     Интервал
─────────────     ────────────────────────────────────────────   ─────────
tunnel_ping        TCP connect через SOCKS5 до VPN server IP       5 sec
                   (env TUNNEL_TARGET, по умолч. 203.0.113.10)
dns_resolve        HTTP GET через SOCKS5 до домена                 10 sec
                   (env DNS_CHECK_URL, по умолч. http://example.com)
                   — если резолв домена через прокси проходит,
                     DNS через туннель жив
http_check         HTTP GET через SOCKS5 до эталонных URL          30 sec
                   (env HTTP_CHECK_URLS, по умолч.
                   https://ifconfig.me,https://example.com)
                   — проверка status 200
container_status   Docker API inspect (ovpn, socks5)               2 sec
                   (state, started_at, restart_count)
```

Probe URLs настраиваются через env-переменные с разумными умолчаниями — не захардкожены.

**Агрегация health-статуса** (зафиксировано):

```
Все probe успешны        → "healthy"  (green)
1-2 probe провалены      → "degraded" (yellow)
Все probe провалены      → "down"     (red)
```

Подсветка ошибок в логах — регекс по маркерам:

```
OpenVPN errors:  "TLS Error", "AUTH_FAILED", "Connection reset",
                 "Inactivity timeout", "SIGTERM", "Initialization
                 Sequence Completed" (info, не error)
SOCKS5 errors:   "connection refused", "timeout", "EOF"
```

### D5b: Чтение трафика через docker exec

Admin в bridge-сети не видит `/proc/net/dev` контейнера ovpn, а `docker stats` даёт агрегированный трафик по всем интерфейсам (включая bridge). Для точного трафика через `tun`:

```
admin ──▶ Docker API ContainerExec ──▶ ovpn: cat /proc/net/dev
                                              │
                                              ▼
  интерфейс: tun0
  столбцы:   rx_bytes, tx_bytes
  парсим строку, считаем delta/sec
```

Выполняется раз в секунду. Admin имеет docker socket, `docker exec` не требует специальных capabilities. Даёт точный RX/TX через VPN-туннель, а не агрегат.

### D6: Compose-скелет

```yaml
services:
  ovpn:
    # ... без изменений ...

  socks5:
    # ... без изменений ...

  admin:
    build: ./admin
    container_name: vpn-admin
    ports:
      - "127.0.0.1:8080:8080"
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
      - ./docker-compose.yml:/compose/docker-compose.yml:ro
      - admin-data:/data
    extra_hosts:
      - "host.docker.internal:host-gateway"
    environment:
      - SOCKS5_PROXY=host.docker.internal:1080
      - COMPOSE_FILE=/compose/docker-compose.yml
      - DB_PATH=/data/admin.db
      - TUNNEL_TARGET=203.0.113.10
      - DNS_CHECK_URL=http://example.com
      - HTTP_CHECK_URLS=https://ifconfig.me,https://example.com
    healthcheck:
      test: ["CMD", "wget", "--spider", "-q", "http://localhost:8080/healthz"]
      interval: 30s
      timeout: 5s
      retries: 3
    restart: unless-stopped

volumes:
  admin-data:
```

**`admin-data` volume**: SQLite persistent, переживает пересоздание контейнера.
**docker socket rw**: нужен для restart/reconnect/recreate (Docker API write-операции). Локальный инструмент — принимаем риск.
**compose.yml ro**: для `os/exec` compose-команд (монтируем проектный compose-файл, только чтение).
**healthcheck**: `/healthz` эндпоинт в admin возвращает 200 если HTTP-сервер жив. Docker healthcheck через `wget --spider`.
**probe URLs через env**: настраиваемые, с умолчаниями.

## Risks / Trade-offs

- **[host.docker.internal нестабилен на Linux native]** → `extra_hosts: host.docker.internal:host-gateway` решает на Linux. На macOS/Windows Docker Desktop работает из коробки.
- **[Крюк через host-published порт добавляет latency к health-probe]** → Приемлемо: probe раз в 5 сек, лишние ~1-2 мс не влияют на диагностику.
- **[admin не видит реальный netns ovpn]** → Health-check через published порт функционально эквивалентен реальному пути трафика (тот же SOCKS5, тот же туннель). Разница только в netns path, не в логике.
- **[docker compose CLI в образе — увеличенный размер]** → `docker:cli` + compose plugin ~ 150MB. Альтернатива — Go-библиотека для compose (незрелая). Принимаем размер ради надёжности.
- **[Полная пересборка убивает admin]** → Команда запускается в новой сессии (`syscall.SysProcAttr{Setsid: true}`), переживает SIGTERM admin. Браузер теряет SSE, переподключается после подъёма. Пользователь предупреждён диалогом.
- **[modernc.org/sqlite медленнее CGO-версии]** → Для 1 запись/сек и чтения графика — не измеримо.
- **[Docker socket rw — security surface]** → Локальный инструмент на localhost без аутентификации. Принимаем риск.
- **[docker.sock временно недоступен]** → Admin не крашится: Docker API вызовы оборачиваются в retry с backoff, UI показывает "Docker API unavailable" вместо крашлупа.

## Observability

- **`/healthz` эндпоинт**: возвращает `200 OK` если HTTP-сервер жив. Используется Docker healthcheck в compose.
- **Admin-логи**: пишутся в stdout контейнера (`log.Printf`). Видны через `docker logs vpn-admin`. Включают: startup, Docker API ошибки, compose-команды (stdout/stderr), probe transitions.
- **Graceful degradation**: при недоступности docker.sock admin продолжает работать (HTTP-сервер жив), UI показывает статус "Docker API unavailable" для контейнеров/логов, health-probes продолжают работать (не зависят от docker.sock).

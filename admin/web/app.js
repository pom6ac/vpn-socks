(function () {
    "use strict";

    // --- State ---
    // Container names come from /api/config; these are the defaults.
    var containers = { ovpn: "vpn-ovpn", socks: "vpn-socks" };
    var activeLogContainer = containers.ovpn;
    var logSources = {};
    var logCursor = {};

    // --- Helpers ---
    function $(sel) { return document.querySelector(sel); }
    function $$(sel) { return document.querySelectorAll(sel); }

    function showToast(msg, type) {
        var t = $("#toast");
        t.textContent = msg;
        t.className = "toast " + (type || "success");
        setTimeout(function () { t.classList.add("hidden"); }, 5000);
    }

    function esc(s) {
        return String(s).replace(/[&<>"']/g, function (c) {
            return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c];
        });
    }

    // Statuses used as CSS classes are restricted to known values.
    var STATUS_CLASSES = { up: 1, degraded: 1, down: 1, unknown: 1, healthy: 1 };
    function statusClass(s) { return STATUS_CLASSES[s] ? s : "unknown"; }

    // api wraps fetch: it marks requests for the server-side CSRF check and,
    // when the server requires a token, asks for it once and retries.
    var loginPending = null;
    function api(url, opts) {
        opts = opts || {};
        opts.headers = Object.assign({ "X-Requested-With": "vpn-admin" }, opts.headers || {});
        opts.credentials = "same-origin";
        return fetch(url, opts).then(function (r) {
            if (r.status !== 401 || url === "/api/login") return r;
            return login().then(function () { return fetch(url, opts); });
        });
    }

    var loginCancelled = false;
    function login() {
        if (loginPending) return loginPending;
        if (loginCancelled) return Promise.reject(new Error("login cancelled"));
        var token = window.prompt("Admin token");
        if (token === null) {
            // Do not nag on every poll; reload the page to try again.
            loginCancelled = true;
            return Promise.reject(new Error("login cancelled"));
        }
        loginPending = fetch("/api/login", {
            method: "POST",
            credentials: "same-origin",
            headers: { "Content-Type": "application/json", "X-Requested-With": "vpn-admin" },
            body: JSON.stringify({ token: token || "" })
        }).then(function (r) {
            loginPending = null;
            if (!r.ok) throw new Error("login failed");
        }, function (e) { loginPending = null; throw e; });
        return loginPending;
    }

    function getJSON(url) {
        return api(url).then(function (r) { return r.json(); });
    }

    function fmtBytes(bps) {
        if (bps < 0 || bps === undefined) return "—";
        if (bps < 1024) return bps.toFixed(0);
        if (bps < 1048576) return (bps / 1024).toFixed(1) + "K";
        return (bps / 1048576).toFixed(2) + "M";
    }

    // --- Status polling ---
    function pollStatus() {
        getJSON("/api/status").then(function (data) {
            var banner = $("#docker-banner");
            if (data.docker_api_unavailable) {
                banner.classList.remove("hidden");
                $("#container-cards").innerHTML = "";
                return;
            }
            banner.classList.add("hidden");
            var html = "";
            (data.containers || []).forEach(function (c) {
                var cls = c.state === "running" ? "green" : "red";
                html += '<div class="card"><span class="indicator ' + cls + '"></span>' +
                    '<span class="name">' + esc(c.name) + '</span>' +
                    '<div class="detail">State: ' + esc(c.state) + '</div>' +
                    '<div class="detail">Restarts: ' + esc(Number(c.restart_count) || 0) + '</div>' +
                    '<div class="detail">Started: ' + esc(c.started_at || "—") + '</div></div>';
            });
            $("#container-cards").innerHTML = html;
        }).catch(function (e) { console.error("status poll error", e); });
    }

    // --- Health polling ---
    function pollHealth() {
        getJSON("/api/health").then(function (data) {
            var badge = $("#overall-health");
            badge.className = "health-badge " + statusClass(data.overall);
            badge.textContent = String(data.overall || "unknown").toUpperCase();

            var html = "";
            (data.probes || []).forEach(function (p) {
                var cls = p.ok ? "green" : "red";
                // detail (egress_ip: the observed exit IP) and error are server
                // data: always escaped.
                var detail = p.detail ? esc(p.detail) + ", " : "";
                html += '<div class="card"><span class="indicator ' + cls + '"></span>' +
                    '<span class="name">' + esc(p.name) + '</span>' +
                    '<div class="detail">' + (p.ok ? detail + esc(Number(p.latency_ms).toFixed(0)) + "ms" : detail + esc(p.error || "failed")) + '</div></div>';
            });
            $("#health-probes").innerHTML = html;
        }).catch(function (e) { console.error("health poll error", e); });
    }

    // --- Topology diagram ---
    // /api/topology returns paths; each path is rendered as one row of nodes
    // joined by status-coloured links (links[i] sits between nodes[i] and nodes[i+1]).
    function pollTopology() {
        getJSON("/api/topology").then(function (data) {
            var html = "";
            (data.paths || []).forEach(function (p, pi) {
                html += '<div class="topo-path' + (pi > 0 ? " topo-aux" : "") + '">';
                p.nodes.forEach(function (n, i) {
                    if (i > 0) {
                        var l = p.links[i - 1];
                        var ls = statusClass(l.status);
                        html += '<div class="topo-link ' + ls + '" title="' + ls + '">' +
                            '<span>' + esc(l.label) + '</span></div>';
                    }
                    var ns = statusClass(n.status);
                    html += '<div class="topo-node ' + ns + '" title="' + ns + '">' +
                        '<div class="topo-title"><span class="dot"></span>' + esc(n.title) + '</div>';
                    (n.lines || []).forEach(function (line) {
                        html += '<div class="topo-line">' + esc(line) + '</div>';
                    });
                    html += '</div>';
                });
                html += '</div>';
            });
            $("#topology").innerHTML = html;
        }).catch(function (e) { console.error("topology poll error", e); });
    }

    // --- Traffic chart ---
    var trafficChart;
    var trafficData = { rx: [], tx: [], labels: [] };
    var maxPoints = 60;

    function initChart() {
        var ctx = $("#traffic-chart").getContext("2d");
        trafficChart = new Chart(ctx, {
            type: "line",
            data: {
                labels: trafficData.labels,
                datasets: [
                    { label: "RX", data: trafficData.rx, borderColor: "#4caf50", fill: false, tension: 0.1 },
                    { label: "TX", data: trafficData.tx, borderColor: "#2196f3", fill: false, tension: 0.1 }
                ]
            },
            options: { animation: false, scales: { x: { display: false } } }
        });
    }

    function pollTraffic() {
        getJSON("/api/metrics/traffic").then(function (data) {
            var unavail = $("#traffic-unavailable");
            if (!data.ok) {
                if (unavail) unavail.classList.remove("hidden");
                return;
            }
            if (unavail) unavail.classList.add("hidden");
            var now = new Date().toLocaleTimeString();
            trafficData.labels.push(now);
            trafficData.rx.push(data.rx_bps);
            trafficData.tx.push(data.tx_bps);
            if (trafficData.labels.length > maxPoints) {
                trafficData.labels.shift();
                trafficData.rx.shift();
                trafficData.tx.shift();
            }
            if (trafficChart) trafficChart.update("none");
            $("#rx-current").textContent = fmtBytes(data.rx_bps);
            $("#tx-current").textContent = fmtBytes(data.tx_bps);
        }).catch(function (e) { console.error("traffic poll error", e); });
    }

    function loadHistory() {
        var range = $("#traffic-range").value;
        getJSON("/api/metrics/history?kind=rx_bytes&range=" + encodeURIComponent(range)).then(function (rxPoints) {
            return getJSON("/api/metrics/history?kind=tx_bytes&range=" + encodeURIComponent(range)).then(function (txPoints) {
                var labels = (rxPoints || []).map(function (p) { return p.ts; });
                var rx = (rxPoints || []).map(function (p) { return p.value; });
                var tx = (txPoints || []).map(function (p) { return p.value; });
                if (trafficChart) {
                    trafficChart.data.labels = labels;
                    trafficChart.data.datasets[0].data = rx;
                    trafficChart.data.datasets[1].data = tx;
                    trafficData.labels = labels;
                    trafficData.rx = rx;
                    trafficData.tx = tx;
                    trafficChart.update();
                }
            });
        }).catch(function (e) { console.error("history load error", e); });
    }

    // --- SSE log streaming ---
    // Docker logs with timestamps=true prefix each line with an RFC3339
    // timestamp, e.g. "2024-08-12T10:00:05.123456789Z message". extractTimestamp
    // pulls that prefix so the SSE client can resume from the last-seen line
    // via the ?since= query param on reconnect.
    function extractTimestamp(line) {
        var idx = line.indexOf(" ");
        if (idx <= 0) return null;
        var ts = line.substring(0, idx);
        if (!/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}/.test(ts)) return null;
        return ts;
    }

    function connectLogs(container) {
        if (logSources[container]) return;
        var url = "/api/logs/" + encodeURIComponent(container);
        if (logCursor[container]) {
            url += "?since=" + encodeURIComponent(logCursor[container]);
        }
        var es = new EventSource(url);
        logSources[container] = es;

        es.addEventListener("connected", function () {
            console.log("SSE connected:", container);
        });

        es.onmessage = function (event) {
            var line, severity;
            try {
                var payload = JSON.parse(event.data);
                line = payload.line;
                severity = payload.severity;
            } catch (e) {
                line = event.data;
                severity = "info";
            }
            var ts = extractTimestamp(line);
            if (ts) {
                logCursor[container] = ts;
            }
            appendLog(container, line, severity);
        };

        es.onerror = function () {
            console.log("SSE error, reconnecting:", container);
            es.close();
            delete logSources[container];
            setTimeout(function () { connectLogs(container); }, 2000);
        };
    }

    function appendLog(container, line, severity) {
        var panel = $("#log-lines");
        if (container !== activeLogContainer) return;
        var div = document.createElement("div");
        div.className = "log-line " + (severity || "info");
        div.textContent = line;
        panel.appendChild(div);
        while (panel.children.length > 500) panel.removeChild(panel.firstChild);
        panel.scrollTop = panel.scrollHeight;
    }

    function switchLogTab(container) {
        activeLogContainer = container;
        $$(".tab").forEach(function (t) { t.classList.toggle("active", t.dataset.container === container); });
        $("#log-lines").innerHTML = "";
        for (var key in logSources) {
            if (logSources[key] && key !== container) {
                logSources[key].close();
                delete logSources[key];
            }
        }
        connectLogs(container);
    }

    // --- Control actions ---
    function performAction(btn) {
        var action = btn.dataset.action;
        var container = btn.dataset.container;
        var url;
        switch (action) {
            case "restart": url = "/api/restart/" + container; break;
            case "reconnect": url = "/api/reconnect"; break;
            case "recreate": url = "/api/recreate"; break;
        }
        if (!url) return;
        btn.disabled = true;
        btn.classList.add("loading");
        var output = $("#action-output");
        output.textContent = "Running " + action + "...";

        api(url, { method: "POST" }).then(function (r) { return r.json(); }).then(function (data) {
            btn.disabled = false;
            if (data.error) {
                showToast(action + " failed: " + data.error, "error");
                output.textContent = data.error;
            } else {
                showToast(action + " succeeded", "success");
                output.textContent = "OK";
            }
        }).catch(function (e) {
            btn.disabled = false;
            showToast(action + " failed: " + e.message, "error");
            output.textContent = e.message;
        });
    }

    // --- Notifications ---
    var prevOverall = null;
    function checkNotification() {
        getJSON("/api/health").then(function (data) {
            if (prevOverall && prevOverall !== "down" && data.overall === "down") {
                sendNotification("VPN Tunnel Down", "Tunnel health is down!");
            }
            prevOverall = data.overall;
        }).catch(function () {});
    }

    function sendNotification(title, body) {
        if (!("Notification" in window)) return;
        if (Notification.permission === "granted") {
            new Notification(title, { body: body });
        } else if (Notification.permission !== "denied") {
            Notification.requestPermission().then(function (perm) {
                if (perm === "granted") {
                    hideNotifBanner();
                    new Notification(title, { body: body });
                }
            });
        }
    }

    function showNotifBannerIfNeeded() {
        var banner = $("#notif-banner");
        if (!("Notification" in window)) {
            banner.classList.add("hidden");
            return;
        }
        if (Notification.permission === "default") {
            banner.classList.remove("hidden");
        } else {
            banner.classList.add("hidden");
        }
    }

    function hideNotifBanner() {
        $("#notif-banner").classList.add("hidden");
    }

    // --- Init ---
    // applyConfig binds buttons and log tabs (data-role) to container names.
    function applyConfig(cfg) {
        if (cfg && cfg.ovpn) containers.ovpn = cfg.ovpn;
        if (cfg && cfg.socks) containers.socks = cfg.socks;
        $$("[data-role]").forEach(function (el) { el.dataset.container = containers[el.dataset.role]; });
        activeLogContainer = containers.ovpn;
    }

    function init() {
        getJSON("/api/config").then(applyConfig, function () { applyConfig(null); }).then(start);
    }

    function start() {
        initChart();

        setInterval(pollStatus, 2000); pollStatus();
        setInterval(pollHealth, 5000); pollHealth();
        setInterval(pollTopology, 5000); pollTopology();
        setInterval(pollTraffic, 1000); pollTraffic();
        setInterval(checkNotification, 10000);
        setInterval(loadHistory, 60000); loadHistory();

        $("#traffic-range").addEventListener("change", loadHistory);

        $$(".tab").forEach(function (t) {
            t.addEventListener("click", function () { switchLogTab(t.dataset.container); });
        });
        connectLogs(containers.ovpn);

        $$(".btn[data-action]").forEach(function (btn) {
            btn.addEventListener("click", function () { performAction(btn); });
        });

        document.body.addEventListener("click", function () {
            if ("Notification" in window && Notification.permission === "default") {
                Notification.requestPermission().then(function () { showNotifBannerIfNeeded(); });
            }
        }, { once: true });

        var notifEnable = $("#notif-enable");
        if (notifEnable) {
            notifEnable.addEventListener("click", function (e) {
                e.preventDefault();
                if ("Notification" in window && Notification.permission === "default") {
                    Notification.requestPermission().then(function () { showNotifBannerIfNeeded(); });
                }
            });
        }
        showNotifBannerIfNeeded();
    }

    document.addEventListener("DOMContentLoaded", init);
})();

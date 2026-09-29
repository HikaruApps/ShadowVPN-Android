(() => {
  "use strict";

  const $ = id => document.getElementById(id);
  const hasNativeBridge = !!window.ShadowVpnAndroid;
  const native = window.ShadowVpnAndroid || {
    ready() {}, toggleVpn() {}, selectServer() {}, refreshSubscriptions() {}, pingServers() {},
    saveSubscriptions() {}, saveSettings() {}, getLogs() { return ""; }, copyText() {}, openVpnSettings() {},
    getInstalledApps() { return '{"apps":[]}'; }, openBatterySettings() {}
  };

  const app = $("app");
  const serverSheet = $("serverSheet");
  const serverScrim = $("serverScrim");
  const serverList = $("serverList");
  const panelOverlay = $("panelOverlay");
  const panel = $("panel");
  const panelBody = $("panelBody");
  let snapshot = { servers: [], settings: {}, subscriptions: {wifi:"",lte:""}, traffic: {} };
  let category = "all";
  let sortMode = "original";
  let serversSignature = "";
  let toastTimer = 0;
  let lastMessage = "";
  let sheetOpen = false;
  let panelBackAction = null;
  let panelCloseTimer = 0;

  function svgFlag(server, large = false) {
    const slot = document.createElement("span");
    slot.className = `server-flag${large ? " large" : ""}`;
    if (server?.auto) {
      slot.classList.add("auto");
    } else if (server?.countryCode) {
      const image = document.createElement("img");
      image.src = `flags/${server.countryCode}.svg`;
      image.alt = "";
      image.onerror = () => { image.remove(); slot.classList.add("fallback"); };
      slot.append(image);
    } else {
      slot.classList.add("fallback");
    }
    return slot;
  }

  function replaceFlag(target, server, large = false) {
    const next = svgFlag(server || {}, large);
    next.id = target.id;
    target.replaceWith(next);
    return next;
  }

  function meta(server) {
    if (!server) return "Потяните вверх, чтобы открыть список";
    if (server.auto) return "АВТОВЫБОР · ВСЕ СЕРВЕРЫ";
    return `${server.protocol} · ${server.transport}`;
  }

  function latencyText(server) {
    if (snapshot.pingRunning && server.latency == null) return { text: "•••", cls: "pending" };
    if (server.latency == null) return { text: "", cls: "" };
    if (!server.available) return { text: "н/д", cls: "" };
    return { text: `${server.latency} мс`, cls: server.latency < 180 ? "good" : server.latency < 450 ? "medium" : "" };
  }

  function quotaBytes(value) {
    const bytes = Math.max(0, Number(value) || 0);
    const units = ["Б", "КБ", "МБ", "ГБ", "ТБ"];
    let size = bytes, unit = 0;
    while (size >= 1024 && unit < units.length - 1) { size /= 1024; unit++; }
    return `${size >= 100 || unit === 0 ? size.toFixed(0) : size.toFixed(1).replace(".", ",")} ${units[unit]}`;
  }

  function matches(server) {
    if (category === "all") return true;
    if (server.auto) return false;
    const name = server.name.toLowerCase();
    if (category === "fast") return name.includes("hysteria") || name.includes(" ws");
    if (category === "p2p") return name.includes("torrent") || name.includes("p2p");
    if (category === "gemini") return name.includes("gemini");
    if (category === "warp") return name.includes("warp");
    return name.includes("no tls") || name.includes("no-tls") || name.includes("no_tls");
  }

  function visibleServers() {
    const list = snapshot.servers.filter(matches).slice();
    if (sortMode === "name") list.sort((a, b) => a.auto ? -1 : b.auto ? 1 : a.name.localeCompare(b.name, "ru"));
    if (sortMode === "latency") list.sort((a, b) => {
      if (a.auto !== b.auto) return a.auto ? -1 : 1;
      const av = a.available ? a.latency : Number.MAX_SAFE_INTEGER;
      const bv = b.available ? b.latency : Number.MAX_SAFE_INTEGER;
      return av - bv;
    });
    return list;
  }

  function renderServers(force = false) {
    const visible = visibleServers();
    const signature = JSON.stringify([category, sortMode, snapshot.pingRunning,
      visible.map(item => [item.id, item.selected, item.latency, item.available])]);
    if (!force && signature === serversSignature) return;
    serversSignature = signature;
    const scroll = serverList.scrollTop;
    serverList.replaceChildren();
    if (!visible.length) {
      const empty = document.createElement("div");
      empty.className = "empty-list";
      empty.textContent = snapshot.importRunning ? "Загружаем серверы…" : "В этой категории пока нет серверов";
      serverList.append(empty);
    }
    visible.forEach(server => {
      const card = document.createElement("button");
      card.className = `server-card${server.selected ? " selected" : ""}`;
      card.append(svgFlag(server));
      const copy = document.createElement("span");
      copy.className = "server-copy";
      const name = document.createElement("b");
      name.textContent = server.name;
      const details = document.createElement("small");
      details.textContent = meta(server);
      copy.append(name, details);
      const latency = document.createElement("span");
      const info = latencyText(server);
      latency.className = `latency ${info.cls}`;
      latency.textContent = info.text;
      card.append(copy, latency);
      card.addEventListener("click", () => {
        native.selectServer(server.id);
        window.setTimeout(closeServers, 110);
      });
      serverList.append(card);
    });
    $("serverCount").textContent = `${visible.length} из ${snapshot.servers.length} серверов`;
    serverList.scrollTop = Math.min(scroll, serverList.scrollHeight);
  }

  function render(next) {
    snapshot = {
      ...snapshot,
      ...(next || {}),
      traffic: { ...(snapshot.traffic || {}), ...((next && next.traffic) || {}) },
      settings: (next && next.settings) || snapshot.settings || {},
      servers: (next && next.servers) || snapshot.servers || [],
      subscriptions: (next && next.subscriptions) || snapshot.subscriptions || {wifi:"",lte:""}
    };
    app.dataset.state = snapshot.state || "disconnected";
    const connected = snapshot.state === "connected";
    const connecting = snapshot.state === "connecting";
    $("powerButton").disabled = connecting || !snapshot.selected;
    $("powerButton").setAttribute("aria-label", connected ? "Отключить VPN" : "Подключить VPN");
    $("statusText").textContent = connected ? "Подключено"
      : connecting ? "Подключение…"
      : snapshot.state === "error" ? snapshot.serviceStatus
      : "Нажмите, чтобы подключиться";

    const location = $("connectedLocation");
    location.replaceChildren();
    if (connected && snapshot.selected) {
      location.append(svgFlag(snapshot.selected));
      location.firstChild.classList.remove("server-flag");
      location.firstChild.classList.add("connected-mini-flag");
      const label = document.createElement("span");
      label.textContent = snapshot.selected.name;
      location.append(label);
      location.hidden = false;
    } else location.hidden = true;

    $("ipLabel").textContent = connected ? "Защищённый IP" : connecting ? "Шифруем IP" : "Ваш IP";
    $("ipValue").textContent = snapshot.publicIp || (connecting ? "•••.•••.•••.•••" : "Определяем…");
    $("sessionTime").textContent = snapshot.session || "00:00:00";
    $("connectionDuration").hidden = !connected;
    $("trafficStats").hidden = !connected;
    $("downloadSpeed").textContent = snapshot.traffic?.downloadSpeed || "0 Б/с";
    $("downloadTotal").textContent = snapshot.traffic?.downloadTotal || "0 Б";
    $("uploadSpeed").textContent = snapshot.traffic?.uploadSpeed || "0 Б/с";
    $("uploadTotal").textContent = snapshot.traffic?.uploadTotal || "0 Б";

    const selected = snapshot.selected;
    const subscription = selected?.subscription;
    const quota = $("subscriptionQuota");
    const showQuota = connected && !!subscription;
    quota.hidden = !showQuota;
    if (showQuota) {
      const used = Math.max(0, Number(subscription.used) || 0);
      const total = Math.max(0, Number(subscription.total) || 0);
      const unlimited = total === 0;
      const percent = unlimited ? 100 : Math.min(100, used / total * 100);
      $("quotaTitle").textContent = subscription.title ? `Лимит · ${subscription.title}` : "Лимит подписки";
      $("quotaValue").textContent = unlimited ? (used > 0 ? `${quotaBytes(used)} · ∞` : "∞") : `${quotaBytes(used)} из ${quotaBytes(total)}`;
      $("quotaProgress").style.width = `${percent}%`;
      $("quotaProgress").classList.toggle("unlimited", unlimited);
      $("quotaProgress").classList.toggle("warning", !unlimited && percent >= 85);
      const expires = Number(subscription.expire) || 0;
      $("quotaExpire").textContent = expires > 0
        ? `Действует до ${new Date(expires * 1000).toLocaleDateString("ru-RU", {day:"numeric",month:"short",year:"numeric"})}`
        : "Обновляется вместе с подпиской";
    }
    replaceFlag($("dockFlag"), selected || {}, true);
    $("dockName").textContent = selected?.name || "Выберите сервер";
    $("dockMeta").textContent = meta(selected);
    const ping = selected ? latencyText(selected) : { text: "" };
    $("dockPing").textContent = ping.text;
    $("pingButton").classList.toggle("running", !!snapshot.pingRunning);
    $("pingButton").disabled = connected || snapshot.pingRunning;
    $("refreshButton").disabled = connected || snapshot.importRunning;
    renderServers();

    $("welcome").hidden = !snapshot.needsSubscription;
    if (snapshot.message && snapshot.message !== lastMessage) showToast(snapshot.message);
    lastMessage = snapshot.message || "";
  }

  function showToast(message) {
    const toast = $("toast");
    toast.textContent = message;
    toast.hidden = false;
    clearTimeout(toastTimer);
    toastTimer = setTimeout(() => { toast.hidden = true; }, 2600);
  }

  function openServers() {
    if (sheetOpen) return;
    sheetOpen = true;
    serverScrim.hidden = false;
    serverSheet.setAttribute("aria-hidden", "false");
    requestAnimationFrame(() => serverSheet.classList.add("open"));
  }

  function closeServers() {
    if (!sheetOpen) return false;
    sheetOpen = false;
    serverSheet.classList.remove("dragging", "open");
    serverSheet.style.removeProperty("--drag-y");
    serverScrim.style.opacity = "";
    serverSheet.setAttribute("aria-hidden", "true");
    setTimeout(() => { if (!sheetOpen) serverScrim.hidden = true; }, 520);
    return true;
  }

  function bindDockGesture() {
    const dock = $("serverDock");
    let startY = 0;
    let openedByDrag = false;
    dock.addEventListener("pointerdown", event => { startY = event.clientY; openedByDrag = false; dock.setPointerCapture(event.pointerId); });
    dock.addEventListener("pointermove", event => {
      if (!openedByDrag && startY - event.clientY > 18) { openedByDrag = true; openServers(); }
    });
    dock.addEventListener("pointerup", event => {
      if (!openedByDrag && Math.abs(startY - event.clientY) < 12 && event.target.closest(".selected-server,.grabber")) openServers();
    });
  }

  function bindSheetDrag() {
    let startY = 0;
    let startAt = 0;
    let dragging = false;
    const begin = event => {
      startY = event.clientY;
      startAt = performance.now();
      dragging = true;
      serverSheet.classList.add("dragging");
      event.currentTarget.setPointerCapture?.(event.pointerId);
    };
    const move = event => {
      if (!dragging) return;
      const distance = Math.max(0, event.clientY - startY);
      serverSheet.style.setProperty("--drag-y", `${distance}px`);
      serverScrim.style.opacity = String(Math.max(0, 1 - distance / (innerHeight * .55)));
    };
    const end = event => {
      if (!dragging) return;
      dragging = false;
      const distance = Math.max(0, event.clientY - startY);
      const velocity = distance / Math.max(1, performance.now() - startAt);
      serverSheet.classList.remove("dragging");
      if (distance > 100 || velocity > .65) closeServers();
      else {
        serverSheet.style.removeProperty("--drag-y");
        serverScrim.style.opacity = "";
      }
    };
    [$("sheetHeader"), serverSheet.querySelector(".sheet-grabber")].forEach(element => {
      element.addEventListener("pointerdown", begin);
      element.addEventListener("pointermove", move);
      element.addEventListener("pointerup", end);
      element.addEventListener("pointercancel", end);
    });

    let listStart = 0;
    let pulling = false;
    serverList.addEventListener("touchstart", event => {
      listStart = event.touches[0].clientY;
      pulling = serverList.scrollTop <= 0;
      startAt = performance.now();
    }, { passive: true });
    serverList.addEventListener("touchmove", event => {
      if (!pulling) return;
      const distance = event.touches[0].clientY - listStart;
      if (distance <= 4) return;
      event.preventDefault();
      serverSheet.classList.add("dragging");
      serverSheet.style.setProperty("--drag-y", `${distance}px`);
      serverScrim.style.opacity = String(Math.max(0, 1 - distance / (innerHeight * .55)));
    }, { passive: false });
    serverList.addEventListener("touchend", event => {
      if (!pulling) return;
      const distance = (event.changedTouches[0]?.clientY || listStart) - listStart;
      pulling = false;
      serverSheet.classList.remove("dragging");
      if (distance > 100) closeServers();
      else { serverSheet.style.removeProperty("--drag-y"); serverScrim.style.opacity = ""; }
    });
  }

  function closeMenu() { $("appMenu").hidden = true; }

  function openPanel(name) {
    closeMenu();
    closeServers();
    clearTimeout(panelCloseTimer);
    panel.classList.remove("dragging", "closing");
    panel.style.removeProperty("--panel-drag-y");
    panelOverlay.style.opacity = "";
    panelOverlay.hidden = false;
    if (name === "settings") buildSettings();
    if (name === "subscriptions") buildSubscriptions();
    if (name === "logs") buildLogs();
  }

  function closePanel() {
    if (panelOverlay.hidden) return false;
    panelBackAction = null;
    panel.classList.remove("dragging");
    panel.classList.add("closing");
    panel.style.removeProperty("--panel-drag-y");
    panelOverlay.style.opacity = "0";
    clearTimeout(panelCloseTimer);
    panelCloseTimer = setTimeout(() => {
      panelOverlay.hidden = true;
      panel.classList.remove("closing");
      panelOverlay.style.opacity = "";
    }, 420);
    return true;
  }

  function bindPanelDrag() {
    let startY = 0;
    let startAt = 0;
    let dragging = false;

    const setDistance = distance => {
      panel.style.setProperty("--panel-drag-y", `${distance}px`);
      panelOverlay.style.opacity = String(Math.max(0, 1 - distance / (innerHeight * .55)));
    };
    const settle = (distance, elapsed) => {
      dragging = false;
      const velocity = distance / Math.max(1, elapsed);
      panel.classList.remove("dragging");
      if (distance > 100 || velocity > .65) closePanel();
      else {
        panel.style.removeProperty("--panel-drag-y");
        panelOverlay.style.opacity = "";
      }
    };
    const begin = event => {
      if (panelOverlay.hidden || panel.classList.contains("closing")) return;
      startY = event.clientY;
      startAt = performance.now();
      dragging = true;
      panel.classList.add("dragging");
      event.currentTarget.setPointerCapture?.(event.pointerId);
    };
    const move = event => {
      if (!dragging) return;
      setDistance(Math.max(0, event.clientY - startY));
    };
    const end = event => {
      if (!dragging) return;
      settle(Math.max(0, event.clientY - startY), performance.now() - startAt);
    };
    [panel.querySelector(".sheet-grabber"), panel.querySelector(".panel-header")].forEach(element => {
      element.addEventListener("pointerdown", begin);
      element.addEventListener("pointermove", move);
      element.addEventListener("pointerup", end);
      element.addEventListener("pointercancel", end);
    });

    let bodyStartY = 0;
    let pulling = false;
    panelBody.addEventListener("touchstart", event => {
      bodyStartY = event.touches[0].clientY;
      pulling = panelBody.scrollTop <= 0 && !panel.classList.contains("closing");
      startAt = performance.now();
    }, { passive: true });
    panelBody.addEventListener("touchmove", event => {
      if (!pulling) return;
      const distance = event.touches[0].clientY - bodyStartY;
      if (distance <= 4) return;
      event.preventDefault();
      panel.classList.add("dragging");
      setDistance(distance);
    }, { passive: false });
    panelBody.addEventListener("touchend", event => {
      if (!pulling) return;
      pulling = false;
      const distance = Math.max(0, (event.changedTouches[0]?.clientY || bodyStartY) - bodyStartY);
      settle(distance, performance.now() - startAt);
    });
    panelBody.addEventListener("touchcancel", () => {
      if (!pulling) return;
      pulling = false;
      settle(0, performance.now() - startAt);
    });
  }

  function title(name, subtitle, backAction = null, settingsIndex = false) {
    $("panelTitle").textContent = name;
    $("panelSubtitle").textContent = subtitle;
    $("panel").classList.toggle("settings-index", settingsIndex);
    const back = $("panelBack");
    back.hidden = !backAction;
    back.onclick = backAction;
    panelBackAction = backAction;
  }
  function section(text) { const node = document.createElement("div"); node.className = "section-label"; node.textContent = text; return node; }
  function settingRow(name, detail, control) {
    const row = document.createElement("div"); row.className = "setting-row";
    const copy = document.createElement("div"); copy.className = "setting-copy";
    const label = document.createElement("b"); label.textContent = name;
    const hint = document.createElement("small"); hint.textContent = detail;
    copy.append(label, hint); row.append(copy, control); return row;
  }
  function selectControl(values, current) {
    const select = document.createElement("select");
    values.forEach(([value, label]) => { const option = new Option(label, value); option.selected = value === current; select.add(option); });
    return select;
  }
  function switchControl(on) {
    const button = document.createElement("button"); button.className = `switch${on ? " on" : ""}`;
    button.addEventListener("click", () => button.classList.toggle("on")); return button;
  }

  function buildSettings() {
    title("Настройки", "ShadowVPN для Android", null, true);
    const body = $("panelBody"); body.replaceChildren(); body.scrollTop = 0;
    const home = document.createElement("div"); home.className = "settings-home";
    home.append(
      settingsCategory("servers", "Серверы", "Подписки, Auto и проверка при запуске", buildServerSettings),
      settingsCategory("connection", "Соединение", "DNS, маршрутизация и параметры TLS", buildConnectionSettings),
      settingsCategory("split", "Раздельное туннелирование", "Выбор приложений для VPN", buildSplitTunneling),
      settingsCategory("application", "Приложение", "Устройство и обновление подписки", buildApplicationSettings),
      settingsCategory("logs", "Диагностика", "Логи Xray и Android TUN", () => buildLogs(true)),
      settingsCategory("about", "О ShadowVPN", "Версия 0.15.2 · patched uTLS", buildAbout)
    );
    body.append(home);
  }

  function settingsCategory(iconName, name, detail, action) {
    const icons = {
      servers:'<path d="M5 4h14v6H5zM5 14h14v6H5zM8 7h.01M8 17h.01"/>',
      connection:'<path d="M5 16a8 8 0 0 1 14 0M8 14a5 5 0 0 1 8 0M10.5 12a2 2 0 0 1 3 0M12 18v2"/>',
      split:'<path d="M6 4v5a3 3 0 0 0 3 3h6a3 3 0 0 1 3 3v5M18 4v5a3 3 0 0 1-3 3H9a3 3 0 0 0-3 3v5M4 6l2-2 2 2M16 18l2 2 2-2"/>',
      application:'<path d="M5 5h5v5H5zM14 5h5v5h-5zM5 14h5v5H5zM14 14h5v5h-5z"/>',
      logs:'<path d="M6 4h12v16H6zM9 8h6M9 12h6M9 16h4"/>',
      about:'<path d="M12 21a9 9 0 1 0 0-18 9 9 0 0 0 0 18ZM12 11v6M12 7h.01"/>'
    };
    const row = document.createElement("button"); row.className = "settings-category";
    const icon = document.createElement("span"); icon.className = "category-icon";
    icon.innerHTML = `<svg viewBox="0 0 24 24">${icons[iconName]}</svg>`;
    const copy = document.createElement("span"); copy.className = "category-copy";
    const label = document.createElement("b"); label.textContent = name;
    const hint = document.createElement("small"); hint.textContent = detail;
    copy.append(label, hint);
    const arrow = document.createElement("span");
    arrow.innerHTML = '<svg class="category-chevron" viewBox="0 0 24 24"><path d="m9 6 6 6-6 6"/></svg>';
    row.append(icon, copy, arrow); row.addEventListener("click", action); return row;
  }

  function saveButton(body, collect) {
    const save = document.createElement("button"); save.className = "primary"; save.textContent = "Сохранить";
    save.addEventListener("click", () => {
      native.saveSettings(JSON.stringify({ ...(snapshot.settings || {}), ...collect() }));
      buildSettings();
    });
    body.append(save);
  }

  function buildServerSettings() {
    title("Серверы", "Список хостов и автоматический выбор", buildSettings);
    const body = $("panelBody"); body.replaceChildren(); body.scrollTop = 0;
    const s = snapshot.settings || {};
    body.append(section("СПИСОК СЕРВЕРОВ"));
    const subscriptions = document.createElement("button"); subscriptions.className = "secondary"; subscriptions.textContent = "Открыть";
    subscriptions.addEventListener("click", () => buildSubscriptions(true));
    const sourceCount = [snapshot.subscriptions?.wifi, snapshot.subscriptions?.lte].filter(Boolean).length;
    body.append(settingRow("Управление подписками", `${sourceCount} из 2 источников`, subscriptions));
    const pingOnOpen = switchControl(!!s.pingOnOpen);
    body.append(settingRow("Пинговать при открытии", "Проверять серверы после загрузки", pingOnOpen));
    body.append(section("СЕРВЕРЫ ДЛЯ AUTO"));
    const selectedIds = new Set(s.autoProfiles || []);
    const autoList = document.createElement("div"); autoList.className = "auto-list";
    snapshot.servers.filter(server => !server.auto).forEach(server => {
      const label = document.createElement("label"); label.className = "auto-item";
      const check = document.createElement("input"); check.type = "checkbox"; check.value = server.id; check.checked = !selectedIds.size || selectedIds.has(server.id);
      const name = document.createElement("span"); name.textContent = server.name; label.append(check, name); autoList.append(label);
    });
    body.append(autoList);
    saveButton(body, () => ({ pingOnOpen:pingOnOpen.classList.contains("on"), autoProfiles:[...autoList.querySelectorAll("input:checked")].map(input => input.value) }));
  }

  function buildConnectionSettings() {
    title("Соединение", "Сеть и параметры туннеля", buildSettings);
    const body = $("panelBody"); body.replaceChildren(); body.scrollTop = 0;
    const s = snapshot.settings || {};
    body.append(section("СЕТЬ"));
    const dns = selectControl([["subscription-doh","DoH · ShadowVPN"],["cloudflare","Cloudflare"],["google","Google"],["quad9","Quad9"]], s.dnsProvider);
    body.append(settingRow("DNS внутри VPN", s.doh || "Системный DNS", dns));
    const fragmentation = switchControl(!!s.fragmentation);
    body.append(settingRow("Фрагментация TLS", "Делит ClientHello для обхода DPI", fragmentation));
    const mtu = selectControl([["1280","1280 · максимум совместимости"],["1360","1360"],["1400","1400 · рекомендуется"],["1500","1500 · локальные сети"]], s.tunMtu || "1400");
    body.append(settingRow("MTU туннеля", "1400 уменьшает фрагментацию в мобильных сетях", mtu));
    const ipv6 = switchControl(!!s.ipv6Enabled);
    body.append(settingRow("IPv6 внутри VPN", "Включайте только при стабильной поддержке сервером", ipv6));
    const vpnSettings = document.createElement("button"); vpnSettings.className = "secondary"; vpnSettings.textContent = "Открыть";
    vpnSettings.addEventListener("click", () => native.openVpnSettings());
    body.append(settingRow("Kill Switch", "Системная блокировка без VPN", vpnSettings));
    body.append(section("МАРШРУТИЗАЦИЯ"));
    const routing = selectControl([["full","Весь трафик"],["bypass","Домены напрямую"],["proxy_only","Только выбранные"]], s.routingMode);
    body.append(settingRow("Режим маршрутизации", "Применится при следующем подключении", routing));
    const rules = document.createElement("textarea"); rules.className = "field multiline"; rules.placeholder = "example.com\ngeosite:youtube\ngeoip:ru"; rules.value = s.routingRules || ""; body.append(rules);
    body.append(section("ПРОВЕРКА ЗАДЕРЖКИ"));
    const pingMethod = selectControl([["tcp","TCP"],["head","HTTP HEAD"],["get","HTTP GET"]], s.pingMethod);
    body.append(settingRow("Метод проверки", "Как измерять задержку сервера", pingMethod));
    saveButton(body, () => ({ dnsProvider:dns.value,fragmentation:fragmentation.classList.contains("on"),tunMtu:mtu.value,ipv6Enabled:ipv6.classList.contains("on"),routingMode:routing.value,routingRules:rules.value,pingMethod:pingMethod.value }));
  }

  function appIcon(app) {
    const icon = document.createElement("span"); icon.className = "installed-app-icon";
    if (app.icon) { const image = document.createElement("img"); image.src = app.icon; image.alt = ""; icon.append(image); }
    else icon.textContent = (app.name || "?").slice(0, 1).toUpperCase();
    return icon;
  }

  function buildSplitTunneling() {
    title("Раздельное туннелирование", "Маршрутизация трафика приложений", buildSettings);
    const body = $("panelBody"); body.replaceChildren(); body.scrollTop = 0;
    const s = snapshot.settings || {};
    let data = { apps: [] };
    try { data = JSON.parse(native.getInstalledApps() || '{"apps":[]}'); } catch (_) {}
    const selected = new Set(s.appRoutingPackages || []);
    body.append(section("РЕЖИМ"));
    const modes = document.createElement("div"); modes.className = "routing-choices";
    [["all","Все приложения","Весь трафик устройства проходит через VPN"],
     ["exclude","Исключить выбранные","Отмеченные приложения работают напрямую"],
     ["include","Только выбранные","Только отмеченные приложения используют VPN"]].forEach(([value,name,detail]) => {
      const label = document.createElement("label"); label.className = "routing-choice";
      const radio = document.createElement("input"); radio.type = "radio"; radio.name = "app-routing"; radio.value = value; radio.checked = (s.appRoutingMode || "all") === value;
      const copy = document.createElement("span");
      const heading = document.createElement("b"); heading.textContent = name;
      const hint = document.createElement("small"); hint.textContent = detail;
      copy.append(heading, hint); label.append(radio, copy); modes.append(label);
    });
    body.append(modes, section("ПРИЛОЖЕНИЯ"));
    const search = document.createElement("input"); search.className = "field app-search"; search.placeholder = "Поиск приложения"; body.append(search);
    const list = document.createElement("div"); list.className = "installed-app-list"; body.append(list);
    const draw = () => {
      const query = search.value.trim().toLowerCase(); list.replaceChildren();
      data.apps.filter(app => !query || app.name.toLowerCase().includes(query) || app.packageName.toLowerCase().includes(query)).forEach(app => {
        const label = document.createElement("label"); label.className = "installed-app";
        const check = document.createElement("input"); check.type = "checkbox"; check.checked = selected.has(app.packageName);
        check.addEventListener("change", () => check.checked ? selected.add(app.packageName) : selected.delete(app.packageName));
        const copy = document.createElement("span"); copy.className = "installed-app-copy";
        const name = document.createElement("b"); name.textContent = app.name;
        const pkg = document.createElement("small"); pkg.textContent = app.packageName; copy.append(name, pkg);
        label.append(appIcon(app), copy, check); list.append(label);
      });
      if (!list.childElementCount) { const empty = document.createElement("div"); empty.className = "empty-list"; empty.textContent = data.error || "Приложения не найдены"; list.append(empty); }
    };
    search.addEventListener("input", draw); draw();
    saveButton(body, () => ({ appRoutingMode:modes.querySelector("input:checked")?.value || "all", appRoutingPackages:[...selected] }));
  }

  function buildApplicationSettings() {
    title("Приложение", "Устройство и фоновые обновления", buildSettings);
    const body = $("panelBody"); body.replaceChildren(); body.scrollTop = 0;
    const s = snapshot.settings || {};
    body.append(section("ФОНОВАЯ РАБОТА"));
    const battery = document.createElement("button"); battery.className = "secondary";
    battery.textContent = s.batteryUnrestricted ? "Без ограничений" : "Настроить";
    battery.addEventListener("click", () => native.openBatterySettings());
    body.append(settingRow("Использование батареи", s.batteryUnrestricted ? "Android не ограничивает VPN в фоне" : "Разрешите работу без ограничений для стабильного VPN", battery));
    body.append(section("УСТРОЙСТВО"));
    const copyHwid = document.createElement("button"); copyHwid.className = "secondary"; copyHwid.textContent = "Копировать";
    copyHwid.addEventListener("click", () => native.copyText(s.hwid || ""));
    body.append(settingRow("HWID устройства", s.hwid || "Недоступно", copyHwid));
    body.append(section("ПОДПИСКА"));
    const autoUpdate = selectControl([["15","15 минут"],["60","1 час"],["360","6 часов"],["1440","Раз в сутки"],["off","Выключено"]], s.autoUpdate);
    body.append(settingRow("Обновление подписки", "Не прерывает активный VPN", autoUpdate));
    const fingerprint = document.createElement("span"); fingerprint.className = "setting-value"; fingerprint.textContent = "Chrome 152";
    body.append(settingRow("TLS fingerprint", "Актуальный patched uTLS", fingerprint));
    saveButton(body, () => ({ autoUpdate:autoUpdate.value }));
  }

  function buildAbout() {
    title("О ShadowVPN", "Клиент для Android", buildSettings);
    const body = $("panelBody"); body.replaceChildren(); body.scrollTop = 0;
    const mark = document.createElement("div"); mark.className = "about-mark";
    mark.innerHTML = '<svg viewBox="0 0 24 24"><path d="M12 3v8M6.5 6.5a7 7 0 1 0 11 0"/></svg>';
    const copy = document.createElement("div"); copy.className = "about-copy";
    copy.innerHTML = '<h2>SHADOWVPN</h2><p>Версия 0.15.2<br>Android VPN · Xray · patched uTLS</p>';
    body.append(mark, copy);
  }

  function buildSubscriptions(fromSettings = false) {
    title("Управление подписками", "Отдельные источники для Wi-Fi и LTE", fromSettings ? buildServerSettings : null);
    const body = $("panelBody"); body.replaceChildren(); body.scrollTop = 0;
    const slots = snapshot.subscriptions || {wifi:"",lte:""};
    body.append(section("ИСТОЧНИКИ"));
    const makeSlot = (kind, name, detail, iconPath) => {
      const card = document.createElement("label"); card.className = "subscription-slot";
      const heading = document.createElement("span"); heading.className = "subscription-slot-heading";
      const icon = document.createElement("span"); icon.innerHTML = `<svg viewBox="0 0 24 24">${iconPath}</svg>`;
      const copy = document.createElement("span"); const label = document.createElement("b"); label.textContent = name;
      const hint = document.createElement("small"); hint.textContent = detail; copy.append(label, hint); heading.append(icon, copy);
      const input = document.createElement("input"); input.className = "field"; input.type = "url"; input.inputMode = "url"; input.placeholder = "https://…"; input.value = slots[kind] || "";
      card.append(heading, input); body.append(card); return input;
    };
    const wifi = makeSlot("wifi", "Wi-Fi", "Подписка для домашней и публичной Wi-Fi сети", '<path d="M4 9a12 12 0 0 1 16 0M7 12a8 8 0 0 1 10 0M10 15a4 4 0 0 1 4 0M12 19h.01"/>');
    const lte = makeSlot("lte", "LTE", "Подписка для мобильной сети", '<path d="M5 19V15M9 19V12M13 19V9M17 19V6M21 19V3"/>');
    const save = document.createElement("button"); save.className = "primary"; save.textContent = "Сохранить и синхронизировать";
    save.addEventListener("click", () => {
      const working = {wifi:wifi.value.trim(),lte:lte.value.trim()};
      if ([working.wifi,working.lte].some(value => value && !value.startsWith("https://"))) { showToast("Нужны корректные HTTPS-ссылки"); return; }
      if (working.wifi && working.wifi === working.lte) { showToast("Для Wi-Fi и LTE нужны разные ссылки"); return; }
      snapshot.subscriptions = working; native.saveSubscriptions(JSON.stringify(working));
      if (fromSettings) buildServerSettings(); else closePanel();
    }); body.append(save);
    const clear = document.createElement("button"); clear.className = "danger"; clear.textContent = "Очистить оба источника";
    clear.addEventListener("click", () => { snapshot.subscriptions={wifi:"",lte:""};native.saveSubscriptions('{"wifi":"","lte":""}');closePanel(); }); body.append(clear);
  }

  function buildLogs(fromSettings = false) {
    title("Логи", "Диагностика Xray и Android TUN", fromSettings ? buildSettings : null);
    const body = $("panelBody"); body.replaceChildren();
    const log = document.createElement("pre"); log.className = "log-box"; log.textContent = native.getLogs() || "Событий пока нет"; body.append(log);
    const copy = document.createElement("button"); copy.className = "secondary"; copy.textContent = "Копировать логи"; copy.addEventListener("click", () => native.copyText(log.textContent)); body.append(copy);
  }

  function closeTopLayer() {
    if (!panelOverlay.hidden && panelBackAction) { panelBackAction(); return true; }
    if (closePanel()) return true;
    if (sheetOpen) return closeServers();
    if (!$("appMenu").hidden) { closeMenu(); return true; }
    return false;
  }

  $("powerButton").addEventListener("click", () => native.toggleVpn());
  $("menuButton").addEventListener("click", event => { event.stopPropagation(); $("appMenu").hidden = !$("appMenu").hidden; });
  document.addEventListener("click", event => { if (!event.target.closest(".app-menu,#menuButton")) closeMenu(); });
  document.querySelectorAll("[data-panel]").forEach(button => button.addEventListener("click", () => openPanel(button.dataset.panel)));
  $("refreshButton").addEventListener("click", () => { closeMenu(); native.refreshSubscriptions(); });
  $("closePanel").addEventListener("click", closePanel); panelOverlay.addEventListener("click", event => { if (event.target === panelOverlay) closePanel(); });
  $("closeServers").addEventListener("click", closeServers); serverScrim.addEventListener("click", closeServers);
  $("selectedServerButton").addEventListener("click", openServers);
  $("categories").addEventListener("click", event => { const button=event.target.closest("button[data-category]");if(!button)return;category=button.dataset.category;document.querySelectorAll(".categories button").forEach(item=>item.classList.toggle("active",item===button));renderServers(true); });
  $("sortButton").addEventListener("click", () => { sortMode=sortMode==="original"?"latency":sortMode==="latency"?"name":"original";showToast(sortMode==="latency"?"Сначала быстрые":sortMode==="name"?"По алфавиту":"Исходный порядок");renderServers(true); });
  $("pingButton").addEventListener("click", () => native.pingServers());
  $("welcomeSubmit").addEventListener("click", () => { const value=$("welcomeUrl").value.trim();if(!value.startsWith("https://")){$("welcomeError").textContent="Введите корректную HTTPS-ссылку";return;}$("welcomeError").textContent="";native.saveSubscriptions(JSON.stringify({wifi:value,lte:""})); });
  bindDockGesture(); bindSheetDrag(); bindPanelDrag();

  window.ShadowVPN = { render, closeTopLayer };
  if (hasNativeBridge) native.ready();
  else { render({
    state:"connected",serviceStatus:"Подключено",needsSubscription:false,importRunning:false,pingRunning:false,message:"",
    publicIp:"132.243.***",session:"00:02:18",traffic:{downloadSpeed:"25,7 КБ/с",downloadTotal:"6,1 МБ",uploadSpeed:"58,8 КБ/с",uploadTotal:"135 КБ"},
    selected:{id:"preview",name:"Финляндия | Hysteria",countryCode:"fi",protocol:"VLESS",transport:"TCP",auto:false,selected:true,latency:43,available:true,subscription:{title:"ShadowVPN",used:12884901888,total:107374182400,expire:1893456000}},
    servers:[
      {id:"auto",name:"Авто",countryCode:"",protocol:"AUTO",transport:"ВСЕ СЕРВЕРЫ",auto:true,selected:false,latency:43,available:true},
      {id:"preview",name:"Финляндия | Hysteria",countryCode:"fi",protocol:"VLESS",transport:"TCP",auto:false,selected:true,latency:43,available:true},
      {id:"nl",name:"Нидерланды | Torrent",countryCode:"nl",protocol:"VLESS",transport:"TCP",auto:false,selected:false,latency:76,available:true},
      {id:"pl",name:"Польша | Gemini | gRPC",countryCode:"pl",protocol:"VLESS",transport:"GRPC",auto:false,selected:false,available:false}
    ],subscriptions:{wifi:"https://example.com/wifi",lte:"https://example.com/lte"},settings:{autoUpdate:"1440",pingOnOpen:true,dnsProvider:"subscription-doh",routingMode:"full",routingRules:"",pingMethod:"tcp",fragmentation:true,tunMtu:"1400",ipv6Enabled:false,appRoutingMode:"all",appRoutingPackages:[],doh:"dns.shadowvpn.io",autoProfiles:[],hwid:"preview-device",batteryUnrestricted:false}
  }); }
})();

(() => {
  "use strict";

  const $ = id => document.getElementById(id);
  const hasNativeBridge = !!window.ShadowVpnAndroid;
  const native = window.ShadowVpnAndroid || {
    ready() {}, toggleVpn() {}, selectServer() {}, refreshSubscriptions() {}, pingServers() {},
    saveSubscriptions() {}, saveSettings() {}, getLogs() { return ""; }, copyText() {}, openVpnSettings() {},
    getInstalledApps() { return '{"apps":[]}'; }, requestInstalledApps() {}, requestAppIcon() {}, openBatterySettings() {}, resetProxyCredentials() {}
  };

  // Weak devices get a static UI: no shadows and no decorative animation.
  const params = new URLSearchParams(location.search);
  const lowEnd = params.has("lite")
    || (navigator.deviceMemory > 0 && navigator.deviceMemory <= 2)
    || (navigator.hardwareConcurrency > 0 && navigator.hardwareConcurrency <= 4);
  document.documentElement.classList.toggle("lite", lowEnd);

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
  // Set when something in the cards really changed. The status timer pushes a
  // snapshot twice a second; it must not touch the cards every time.
  let cardsDirty = true;
  let categoriesSignature = "";
  let serverPager = null;
  let dockFlagKey = null;
  let statusKey = "";
  let toastTimer = 0;
  let toastHideTimer = 0;
  let lastMessage = "";
  let sheetOpen = false;
  let panelBackAction = null;
  let panelCloseTimer = 0;
  // The page the panel shows right now: its wrapper is animated out when
  // another page opens.
  let currentPage = null;
  // Direction of the page movement: 1 is forward, -1 is back.
  let pageDirection = 1;
  // The second half of a page change: the new page arrives after the old
  // one has left.
  let pageSwapTimer = 0;
  // Cancels a deferred panel build when the user navigated away meanwhile.
  let panelBuildToken = 0;
  let panelPager = null;
  let splitAppsReceiver = null;
  let appIconObserver = null;
  let refreshBattery = null;
  let refreshProxy = null;
  // Collected settings changes; flushed when the user leaves the page or
  // switches the settings tab.
  let pendingSettings = [];
  // Server models and their cards, kept between renders: a new latency only
  // changes a few nodes, so the list is never rebuilt from scratch.
  const serverModels = new Map();
  const serverCards = new Map();
  const cardParts = new WeakMap();
  // Visual order of the cards. CSS "order" moves them without rebuilding
  // the list, so sorting by latency stays cheap while a ping runs.
  const serverOrder = new Map();
  // A drag that ends over a button must not also press it. Only the click
  // that follows the drag is suppressed, not every click for a while.
  let suppressNextClick = false;
  document.addEventListener("pointerdown", () => { suppressNextClick = false; }, true);
  document.addEventListener("click", event => {
    if (!suppressNextClick) return;
    suppressNextClick = false;
    event.preventDefault(); event.stopPropagation();
  }, true);

  const ICONS = {
    auto: '<path d="M13 3 5 13.5h6L10 21l8-10.5h-6z"/>',
    ping: '<path d="M3 12h4l3-7 4 14 3-7h4"/>',
    globe: '<circle cx="12" cy="12" r="9"/><path d="M3 12h18M12 3c2.5 2.6 3.7 5.6 3.7 9s-1.2 6.4-3.7 9c-2.5-2.6-3.7-5.6-3.7-9S9.5 5.6 12 3Z"/>',
    servers: '<rect x="4" y="4" width="16" height="6" rx="1.5"/><rect x="4" y="14" width="16" height="6" rx="1.5"/><path d="M8 7h.01M8 17h.01"/>',
    connection: '<path d="M5 12.5a10 10 0 0 1 14 0M8 15.5a5.5 5.5 0 0 1 8 0M12 19h.01"/>',
    proxy: '<rect x="3" y="4" width="18" height="12" rx="2"/><path d="M8 20h8M12 16v4M7 10h.01M11 10h6"/>',
    tunnel: '<path d="M3 19v-8a9 9 0 0 1 18 0v8M2 19h20M8 19v-7a4 4 0 0 1 8 0v7"/>',
    routing: '<circle cx="6" cy="18" r="2.5"/><circle cx="18" cy="6" r="2.5"/><path d="M8.5 18H14a4 4 0 0 0 4-4V8.5M15.5 6H10a4 4 0 0 0-4 4v5.5"/>',
    split: '<path d="M6 3v6a3 3 0 0 0 3 3h6a3 3 0 0 1 3 3v6M18 3v6a3 3 0 0 1-3 3"/><path d="M9 12a3 3 0 0 0-3 3v6"/>',
    application: '<rect x="4" y="4" width="6.5" height="6.5" rx="1.5"/><rect x="13.5" y="4" width="6.5" height="6.5" rx="1.5"/><rect x="4" y="13.5" width="6.5" height="6.5" rx="1.5"/><rect x="13.5" y="13.5" width="6.5" height="6.5" rx="1.5"/>',
    logs: '<path d="M4 5h16v14H4z"/><path d="m8 10 2 2-2 2M13 14h3"/>',
    about: '<circle cx="12" cy="12" r="9"/><path d="M12 11v5M12 8h.01"/>',
    wifi: '<path d="M2.5 9a14 14 0 0 1 19 0M5.5 12.5a9.5 9.5 0 0 1 13 0M8.8 15.8a4.8 4.8 0 0 1 6.4 0M12 19h.01"/>',
    lte: '<path d="M5 19v-3M9.5 19v-6M14 19v-9M18.5 19V6"/>',
    chevron: '<path d="m9 6 6 6-6 6"/>'
  };

  function svg(name, cls = "") {
    const holder = document.createElement("span");
    holder.innerHTML = `<svg${cls ? ` class="${cls}"` : ""} viewBox="0 0 24 24">${ICONS[name]}</svg>`;
    return holder.firstChild;
  }

  function el(tag, cls, text) {
    const node = document.createElement(tag);
    if (cls) node.className = cls;
    if (text != null) node.textContent = text;
    return node;
  }

  function setText(id, value) {
    const node = $(id);
    if (node.textContent !== value) node.textContent = value;
  }

  // Runs the callback after the given number of frames. Two frames are enough
  // for the browser to paint the current state, so an animation that starts
  // afterwards really animates instead of jumping to its end state.
  function afterFrames(count, callback) {
    const step = () => {
      count -= 1;
      if (count <= 0) { callback(); return; }
      requestAnimationFrame(step);
    };
    requestAnimationFrame(step);
  }

  // Waits until the browser has drawn the frame that was just started, so an
  // animation is already on screen before the heavy work begins.
  function afterFirstPaint(callback) {
    afterFrames(2, callback);
  }

  // Runs the callback when the browser has nothing better to do.
  function scheduleIdle(callback) {
    if (typeof requestIdleCallback === "function") requestIdleCallback(callback, { timeout: 300 });
    else setTimeout(callback, 60);
  }

  function plural(count, forms) {
    const n = Math.abs(count) % 100, d = n % 10;
    if (n > 10 && n < 20) return forms[2];
    if (d === 1) return forms[0];
    if (d > 1 && d < 5) return forms[1];
    return forms[2];
  }

  function flag(server) {
    const slot = el("span", "server-flag");
    if (server?.auto || server?.members > 0) {
      slot.append(svg("auto"));
    } else if (server?.countryCode) {
      const image = el("img");
      image.src = `flags/${server.countryCode}.svg`;
      image.alt = "";
      image.width = 36; image.height = 36;
      image.loading = "lazy";
      image.decoding = "async";
      image.onerror = () => image.replaceWith(svg("globe"));
      slot.append(image);
    } else {
      slot.append(svg("globe"));
    }
    return slot;
  }

  function meta(server) {
    if (!server) return "Список серверов";
    if (server.auto) return "Автовыбор лучшего сервера";
    if (server.members > 0) return `Балансировщик · ${server.members} ${plural(server.members, ["сервер", "сервера", "серверов"])}`;
    // Hysteria reports the same name as protocol and transport.
    const transport = server.transport && server.transport !== server.protocol ? server.transport : "";
    return [server.protocol, transport].filter(Boolean).join(" · ");
  }

  // "JSON" badge left of protocol · transport for servers imported from Xray JSON.
  function fillMeta(node, server) {
    const key = `${server?.format || ""}|${meta(server)}`;
    if (node.dataset.key === key) return;
    node.dataset.key = key;
    node.replaceChildren();
    if (server?.format === "json") node.append(el("span", "badge", "JSON"));
    node.append(meta(server));
  }

  function latencyText(server) {
    if (snapshot.pingRunning && server.latency == null) return { text: "•••", cls: "pending" };
    if (server.latency == null) return { text: "", cls: "" };
    if (!server.available) return { text: "—", cls: "bad" };
    return { text: `${server.latency} мс`, cls: server.latency < 180 ? "good" : server.latency < 450 ? "medium" : "bad" };
  }

  function quotaBytes(value) {
    const bytes = Math.max(0, Number(value) || 0);
    const units = ["Б", "КБ", "МБ", "ГБ", "ТБ"];
    let size = bytes, unit = 0;
    while (size >= 1024 && unit < units.length - 1) { size /= 1024; unit++; }
    return `${size >= 100 || unit === 0 ? size.toFixed(0) : size.toFixed(1).replace(".", ",")} ${units[unit]}`;
  }

  function matchesCategory(server, name) {
    if (name === "all") return true;
    if (name.startsWith("source:")) return !server.auto && server.source === name.slice(7);
    if (server.auto) return false;
    const title = server.name.toLowerCase();
    if (name === "fast") return title.includes("hysteria") || title.includes(" ws");
    if (name === "p2p") return title.includes("torrent") || title.includes("p2p");
    if (name === "gemini") return title.includes("gemini");
    if (name === "warp") return title.includes("warp");
    return title.includes("no tls") || title.includes("no-tls") || title.includes("no_tls");
  }

  function visibleServers() {
    const list = snapshot.servers.filter(server => matchesCategory(server, category));
    if (sortMode === "name") list.sort((a, b) => a.auto ? -1 : b.auto ? 1 : a.name.localeCompare(b.name, "ru"));
    if (sortMode === "latency") list.sort((a, b) => {
      if (a.auto !== b.auto) return a.auto ? -1 : 1;
      const av = a.available ? a.latency : Number.MAX_SAFE_INTEGER;
      const bv = b.available ? b.latency : Number.MAX_SAFE_INTEGER;
      return av - bv;
    });
    return list;
  }

  // Appends items page by page as a sentinel scrolls into view, so long lists
  // never build thousands of nodes up front.
  function lazyList(container, items, renderItem, root, pageSize = 30) {
    let index = 0;
    let observer = null;
    const sentinel = el("div", "list-sentinel");
    const appendPage = () => {
      const fragment = document.createDocumentFragment();
      const end = Math.min(items.length, index + pageSize);
      for (; index < end; index++) fragment.append(renderItem(items[index], index));
      container.insertBefore(fragment, sentinel);
      if (index >= items.length) { observer?.disconnect(); sentinel.remove(); }
    };
    container.append(sentinel);
    appendPage();
    if (index < items.length) {
      if ("IntersectionObserver" in window) {
        observer = new IntersectionObserver(entries => {
          if (!entries.some(entry => entry.isIntersecting)) return;
          appendPage();
          // Re-observe so a sentinel that is still visible fires again.
          if (index < items.length) { observer.unobserve(sentinel); observer.observe(sentinel); }
        }, { root, rootMargin: "600px 0px" });
        observer.observe(sentinel);
      } else {
        while (index < items.length) appendPage();
      }
    }
    return { disconnect() { observer?.disconnect(); } };
  }

  function updateCategories() {
    const signature = snapshot.servers.map(server => server.id + server.name).join("|");
    if (signature === categoriesSignature) return;
    categoriesSignature = signature;
    let reset = false;
    $("categories").querySelectorAll("button[data-source]").forEach(button => button.remove());
    const sources = [...new Set(snapshot.servers.filter(server => !server.auto && server.source).map(server => server.source))];
    if (sources.length > 1) {
      const labels = { manual: "Свои" };
      const anchor = $("categories").querySelector('button[data-category="all"]');
      sources.reverse().forEach(source => {
        const chip = el("button", null, labels[source] || source);
        chip.dataset.category = `source:${source}`;
        chip.dataset.source = "1";
        anchor.after(chip);
      });
    }
    $("categories").querySelectorAll("button[data-category]").forEach(button => {
      const name = button.dataset.category;
      const empty = name !== "all" && !snapshot.servers.some(server => matchesCategory(server, name));
      button.hidden = empty;
      if (empty && name === category) reset = true;
      button.classList.toggle("active", name === category);
    });
    if (reset) setCategory("all");
    $("categories").hidden = $("categories").querySelectorAll("button:not([hidden])").length < 2;
    moveChipIndicator();
  }

  function setCategory(name) {
    category = name;
    $("categories").querySelectorAll("button[data-category]").forEach(item =>
      item.classList.toggle("active", item.dataset.category === name));
    moveChipIndicator();
  }

  // «Таблетка» под активной категорией переезжает на неё.
  function moveChipIndicator(strip = $("categories")) {
    const indicator = strip.querySelector(".chip-indicator");
    if (!indicator) return;
    const active = strip.querySelector("button.active");
    if (!active || active.hidden || strip.hidden) { indicator.style.opacity = "0"; return; }
    indicator.style.opacity = "1";
    indicator.style.width = `${active.offsetWidth}px`;
    indicator.style.transform = `translateX(${active.offsetLeft}px)`;
  }

  // Cards are reused between renders. A new latency or a new selection only
  // touches a few nodes, so the list survives a ping without being rebuilt.
  function flagKey(server) {
    return `${server.auto ? 1 : 0}|${server.members || 0}|${server.countryCode || ""}`;
  }

  function serverCard(server) {
    const cached = serverCards.get(server.id);
    if (cached) { updateServerCard(cached, server); return cached; }
    const card = el("button", "server-card");
    card.type = "button";
    card.dataset.id = server.id;
    card.style.order = String(serverOrder.get(server.id) ?? 0);
    const copy = el("span", "server-copy");
    const name = el("b", null, server.name);
    const details = el("small");
    const flagSlot = flag(server);
    const latency = el("span", "latency");
    fillMeta(details, server);
    copy.append(name, details);
    card.append(flagSlot, copy, latency);
    cardParts.set(card, { name, details, flag: flagSlot, latency, flagKey: flagKey(server) });
    serverCards.set(server.id, card);
    updateServerCard(card, server);
    return card;
  }

  function updateServerCard(card, server) {
    if (!server) return;
    const parts = cardParts.get(card);
    if (!parts) return;
    card.classList.toggle("selected", !!server.selected);
    if (parts.name.textContent !== server.name) parts.name.textContent = server.name;
    fillMeta(parts.details, server);
    const info = latencyText(server);
    if (parts.latency.textContent !== info.text) parts.latency.textContent = info.text;
    parts.latency.className = `latency ${info.cls}`;
    const key = flagKey(server);
    if (parts.flagKey !== key) {
      parts.flagKey = key;
      const nextFlag = flag(server);
      parts.flag.replaceWith(nextFlag);
      parts.flag = nextFlag;
    }
  }

  /** Recomputes the visual order of the cards from the current sort. */
  function updateServerOrder() {
    serverOrder.clear();
    visibleServers().forEach((server, index) => serverOrder.set(server.id, index));
  }

  /** Keeps the cards in sync with the models without touching the list order. */
  function updateMountedCards() {
    updateServerOrder();
    serverCards.forEach((card, id) => {
      const server = serverModels.get(id);
      if (!server) return;
      updateServerCard(card, server);
      const rank = serverOrder.get(id);
      if (rank != null) card.style.order = String(rank);
    });
  }

  /** Takes the server list from Android and keeps the known latencies. */
  function ingestServers(items) {
    const next = new Map();
    for (const item of items || []) next.set(item.id, item);
    for (const [id, old] of serverModels) {
      const fresh = next.get(id);
      if (!fresh) {
        serverModels.delete(id);
        serverCards.delete(id);
        continue;
      }
      // The list carries no latency any more: it arrives separately, as soon
      // as each host answers. Keep the last known value until it does.
      if (fresh.latency == null && old.latency != null) {
        fresh.latency = old.latency;
        fresh.available = old.available;
      }
      next.set(id, fresh);
    }
    serverModels.clear();
    for (const [id, item] of next) serverModels.set(id, item);
    snapshot.servers = [...serverModels.values()];
    cardsDirty = true;
  }

  /** Marks the chosen server; the selection arrives as a plain ID. */
  function applySelectedServer(id) {
    const chosen = id || "";
    for (const [serverId, server] of serverModels) server.selected = serverId === chosen;
    snapshot.selectedId = chosen;
    snapshot.selected = serverModels.get(chosen) || null;
    cardsDirty = true;
    updateMountedCards();
  }

  /** A host answered: show its latency right away. */
  function applyPingResult(item) {
    if (!item || !item.id) return;
    const server = serverModels.get(item.id);
    if (!server) return;
    server.latency = item.latency;
    server.available = item.available;
    cardsDirty = true;
    const card = serverCards.get(item.id);
    if (card) {
      updateServerCard(card, server);
      popLatency(cardParts.get(card)?.latency);
    }
    if (snapshot.selectedId === item.id) {
      const ping = latencyText(server);
      setText("dockPing", ping.text);
      $("dockPing").className = `latency ${ping.cls}`;
    }
    // Sorting by latency reorders the list; "order" moves the cards without
    // rebuilding them.
    if (sortMode === "latency") updateMountedCards();
  }

  // A fresh latency pops in (as in Hiddify): scale from half size with a
  // small overshoot. Web Animations run on the compositor and need no
  // reflow, so dozens of answers at once stay cheap.
  function popLatency(node) {
    if (!node || noMotion() || typeof node.animate !== "function") return;
    node.animate(
      [{ transform: "scale(.5)", opacity: 0 }, { transform: "scale(1)", opacity: 1 }],
      { duration: 450, easing: "cubic-bezier(.34,1.56,.64,1)" });
  }

  /** A new check starts: every host waits for its own answer again. */
  function clearPingResults() {
    for (const server of serverModels.values()) {
      delete server.latency;
      delete server.available;
    }
    cardsDirty = true;
    updateMountedCards();
    const selected = snapshot.selected;
    if (selected) {
      const ping = latencyText(selected);
      setText("dockPing", ping.text);
      $("dockPing").className = `latency ${ping.cls}`;
    }
  }

  function renderServers(force = false) {
    // The list is built only while the sheet is visible.
    if (!sheetOpen) { serversSignature = ""; return; }
    updateCategories();
    const visible = visibleServers();
    // Only the structure is part of the signature. A new latency or a new
    // selection updates the cards in place instead of rebuilding the list.
    const structure = visible.map(item => item.id).join("|");
    const signature = `${category}|${sortMode}|${snapshot.pingRunning}|${snapshot.importRunning}|${structure}`;
    if (!force && signature === serversSignature) {
      // Nothing about the list changed: the cards do not have to be touched.
      if (!cardsDirty) return;
      updateMountedCards();
      cardsDirty = false;
      return;
    }
    serversSignature = signature;
    cardsDirty = false;
    const scroll = serverList.scrollTop;
    serverPager?.disconnect();
    serverList.replaceChildren();
    const total = snapshot.servers.length;
    setText("serverCount", category === "all"
      ? `${total} ${plural(total, ["сервер", "сервера", "серверов"])}`
      : `${visible.length} из ${total}`);
    if (!visible.length) {
      serverList.append(el("div", "empty-list", snapshot.importRunning ? "Загружаем серверы…" : "Нет серверов"));
      serverPager = null;
      return;
    }
    // Cards that left the list are dropped, the rest are reused as they are.
    const visibleIds = new Set(visible.map(server => server.id));
    for (const id of [...serverCards.keys()]) if (!visibleIds.has(id)) serverCards.delete(id);
    updateServerOrder();
    serverPager = lazyList(serverList, visible, serverCard, serverList, 40);
    updateMountedCards();
    serverList.scrollTop = scroll;
  }

  function renderStatus(connected, connecting) {
    const selected = snapshot.selected;
    const error = snapshot.state === "error";
    const blocked = snapshot.state === "blocked";
    const target = switchTarget();
    const key = [snapshot.state, selected?.id, selected?.name, target?.id, error ? snapshot.serviceStatus : ""].join("\u0000");
    if (key === statusKey) return;
    statusKey = key;
    if (connected && target) {
      // The tunnel stays up on the current server until the new one answers.
      setText("statusText", "Переключение…");
      const detail = $("statusDetail");
      detail.replaceChildren(flag(target), el("span", null, target.name));
      return;
    }
    setText("statusText", connected ? "Подключено"
      : blocked ? "Интернет заблокирован"
      : connecting ? "Подключение…"
      : error ? "Ошибка подключения"
      : "Не подключено");
    const detail = $("statusDetail");
    detail.replaceChildren();
    if (blocked) {
      detail.textContent = "Kill Switch: нет связи с сервером, подключаемся снова";
    } else if (error) {
      const text = String(snapshot.serviceStatus || "").replace(/^Ошибка:\s*/, "") || "Не удалось подключиться";
      detail.textContent = text.charAt(0).toUpperCase() + text.slice(1);
    } else if ((connected || connecting) && selected) {
      detail.append(flag(selected), el("span", null, selected.name));
    } else {
      detail.textContent = selected ? "Нажмите, чтобы подключиться" : "Выберите сервер ниже";
    }
  }

  function switchTarget() {
    const id = snapshot.switchingTo;
    return id ? snapshot.servers.find(server => server.id === id) || { id, name: "другой сервер" } : null;
  }

  function renderQuota(connected) {
    const subscription = snapshot.selected?.subscription;
    const quota = $("subscriptionQuota");
    const show = connected && !!subscription;
    quota.hidden = !show;
    if (!show) return;
    const used = Math.max(0, Number(subscription.used) || 0);
    const total = Math.max(0, Number(subscription.total) || 0);
    const unlimited = total === 0;
    const percent = unlimited ? 100 : Math.min(100, used / total * 100);
    setText("quotaTitle", subscription.title || "Трафик подписки");
    setText("quotaValue", unlimited ? `${quotaBytes(used)} · без лимита` : `${quotaBytes(used)} из ${quotaBytes(total)}`);
    const bar = $("quotaProgress");
    // The bar grows with a transform: animating the width would relayout the
    // page on every frame.
    const scale = `scaleX(${percent / 100})`;
    if (bar.style.transform !== scale) bar.style.transform = scale;
    bar.classList.toggle("unlimited", unlimited);
    bar.classList.toggle("warning", !unlimited && percent >= 85);
    const expires = Number(subscription.expire) || 0;
    setText("quotaExpire", expires > 0
      ? `Действует до ${new Date(expires * 1000).toLocaleDateString("ru-RU", {day:"numeric",month:"long",year:"numeric"})}`
      : "Без срока действия");
  }

  function render(next) {
    snapshot = {
      ...snapshot,
      ...(next || {}),
      traffic: { ...(snapshot.traffic || {}), ...((next && next.traffic) || {}) },
      settings: (next && next.settings) || snapshot.settings || {},
      subscriptions: (next && next.subscriptions) || snapshot.subscriptions || {wifi:"",lte:""}
    };
    // The server list arrives without latencies and without the selection:
    // both come separately, so a new latency never rebuilds the list.
    if (next && Array.isArray(next.servers)) ingestServers(next.servers);
    if (next && Object.prototype.hasOwnProperty.call(next, "selectedId")) {
      applySelectedServer(next.selectedId);
    } else if (next && next.selected) {
      applySelectedServer(next.selected.id);
    }
    const state = snapshot.state || "disconnected";
    if (app.dataset.state !== state) app.dataset.state = state;
    const connected = state === "connected";
    const connecting = state === "connecting";
    const power = $("powerButton");
    power.disabled = connecting || !snapshot.selected || !!snapshot.switchingTo;
    power.setAttribute("aria-label", connected || state === "blocked" ? "Отключить VPN" : "Подключить VPN");
    renderStatus(connected, connecting);

    setText("ipLabel", connected ? "Защищённый IP" : "IP");
    setText("ipValue", snapshot.publicIp || (connecting || state === "blocked" ? "—" : "Определяем…"));
    setText("sessionTime", snapshot.session || "00:00:00");
    $("sessionTime").hidden = !connected;
    $("detailsDivider").hidden = !connected;
    $("trafficStats").hidden = !connected;
    if (connected) {
      setText("downloadSpeed", snapshot.traffic?.downloadSpeed || "0 Б/с");
      setText("downloadTotal", snapshot.traffic?.downloadTotal || "0 Б");
      setText("uploadSpeed", snapshot.traffic?.uploadSpeed || "0 Б/с");
      setText("uploadTotal", snapshot.traffic?.uploadTotal || "0 Б");
    }
    renderQuota(connected);

    const selected = snapshot.selected;
    const flagKey = selected ? `${selected.id}|${selected.auto}|${selected.countryCode}` : "";
    if (flagKey !== dockFlagKey) {
      dockFlagKey = flagKey;
      const nextFlag = flag(selected || {});
      nextFlag.id = "dockFlag";
      $("dockFlag").replaceWith(nextFlag);
    }
    setText("dockName", selected?.name || "Выберите сервер");
    fillMeta($("dockMeta"), selected);
    const ping = selected ? latencyText(selected) : { text: "", cls: "" };
    setText("dockPing", ping.text);
    $("dockPing").className = `latency ${ping.cls}`;
    $("pingButton").classList.toggle("running", !!snapshot.pingRunning);
    $("pingButton").disabled = !!snapshot.pingRunning;
    $("refreshButton").disabled = !!snapshot.importRunning;
    renderServers();
    refreshBattery?.(!!snapshot.settings?.batteryUnrestricted);
    refreshProxy?.(snapshot.settings || {});

    $("welcome").hidden = !snapshot.needsSubscription;
    if (snapshot.message && snapshot.message !== lastMessage) showToast(snapshot.message);
    lastMessage = snapshot.message || "";
  }

  function showToast(message) {
    const toast = $("toast");
    toast.textContent = message;
    toast.hidden = false;
    clearTimeout(toastTimer);
    clearTimeout(toastHideTimer);
    // The toast slides in and out; it is only hidden after the fade-out, so
    // the dismissal is visible instead of an instant jump.
    toast.classList.remove("visible");
    requestAnimationFrame(() => toast.classList.add("visible"));
    toastTimer = setTimeout(() => {
      toast.classList.remove("visible");
      toastHideTimer = setTimeout(() => {
        if (!toast.classList.contains("visible")) toast.hidden = true;
      }, 180);
    }, 2600);
  }

  /* ---------- Menus ---------- */

  // Popovers fade in and out. The element stays in the layout until the
  // fade-out is over, so closing it is seen instead of an instant jump.
  const popoverTimers = new WeakMap();

  function setPopoverVisible(menu, open) {
    const timer = popoverTimers.get(menu);
    if (timer) { clearTimeout(timer); popoverTimers.delete(menu); }
    const wasOpen = menu.classList.contains("open");
    if (open) {
      menu.hidden = false;
      menu.classList.remove("closing");
      // The class comes one frame later: otherwise the browser would only
      // ever see the final state and would not animate at all.
      afterFirstPaint(() => menu.classList.add("open"));
      return;
    }
    if (!wasOpen && menu.hidden) { menu.classList.remove("closing"); return; }
    menu.classList.remove("open");
    menu.classList.add("closing");
    popoverTimers.set(menu, setTimeout(() => {
      popoverTimers.delete(menu);
      if (menu.classList.contains("open")) return;
      menu.hidden = true;
      menu.classList.remove("closing");
    }, 150));
  }

  function setMenu(open) {
    setPopoverVisible($("appMenu"), open);
    $("menuButton").setAttribute("aria-expanded", String(open));
  }
  function closeMenu() { setMenu(false); }

  function toggleSortMenu(force) {
    const menu = $("sortMenu");
    const open = typeof force === "boolean" ? force : !menu.classList.contains("open");
    if (open) menu.querySelectorAll("button[data-sort]").forEach(button =>
      button.setAttribute("aria-checked", String(button.dataset.sort === sortMode)));
    setPopoverVisible(menu, open);
    $("sortButton").setAttribute("aria-expanded", String(open));
  }

  /* ---------- Server sheet ---------- */

  function openServers() {
    if (sheetOpen) return;
    sheetOpen = true;
    closeMenu();
    serverScrim.hidden = false;
    serverScrim.classList.remove("closing");
    serverSheet.setAttribute("aria-hidden", "false");
    // The sheet starts moving first; the list is built a frame later, so a
    // long list never delays the opening animation.
    afterFirstPaint(() => {
      serverScrim.classList.add("open");
      serverSheet.classList.add("open");
    });
    afterFrames(3, () => { renderServers(); moveChipIndicator(); });
  }

  function closeServers() {
    if (!sheetOpen) return false;
    toggleSortMenu(false);
    sheetOpen = false;
    serverSheet.classList.remove("dragging", "open");
    serverSheet.style.removeProperty("--drag-y");
    serverScrim.classList.remove("open");
    serverScrim.style.opacity = "";
    serverSheet.setAttribute("aria-hidden", "true");
    setTimeout(() => {
      if (sheetOpen) return;
      serverScrim.hidden = true;
      serverScrim.classList.remove("closing");
      // Free the list while it is not visible.
      serverPager?.disconnect();
      serverPager = null;
      serverList.replaceChildren();
      serversSignature = "";
    }, 240);
    return true;
  }

  function bindDockGesture() {
    const dock = $("serverDock");
    let startY = 0;
    let openedByDrag = false;
    dock.addEventListener("pointerdown", event => { startY = event.clientY; openedByDrag = false; });
    dock.addEventListener("pointermove", event => {
      if (event.buttons && !openedByDrag && startY - event.clientY > 18) { openedByDrag = true; openServers(); }
    });
  }

  function bindDrag(target, handles, scroller, onMove, onEnd, canStart = () => true, canPull = () => true) {
    let startY = 0;
    let startAt = 0;
    let dragging = false;
    const begin = event => {
      if (!canStart(event)) return;
      startY = event.clientY;
      startAt = performance.now();
      dragging = true;
      target.classList.add("dragging");
      event.currentTarget.setPointerCapture?.(event.pointerId);
    };
    const move = event => { if (dragging) onMove(Math.max(0, event.clientY - startY)); };
    const end = event => {
      if (!dragging) return;
      dragging = false;
      target.classList.remove("dragging");
      const distance = Math.max(0, event.clientY - startY);
      onEnd(distance, distance / Math.max(1, performance.now() - startAt));
    };
    handles.forEach(element => {
      element.addEventListener("pointerdown", begin);
      element.addEventListener("pointermove", move);
      element.addEventListener("pointerup", end);
      element.addEventListener("pointercancel", end);
    });

    // Pulling a list down from its top edge drags the whole sheet, wherever
    // the finger lands (cards and rows are buttons too). The gesture only
    // becomes a drag once it is clearly downward, so taps stay taps.
    const SLOP = 8;
    let startX = 0;
    let listStart = 0;
    let armed = false;
    let pulling = false;
    scroller.addEventListener("touchstart", event => {
      const touch = event.touches[0];
      startX = touch.clientX;
      listStart = touch.clientY;
      pulling = false;
      armed = event.touches.length === 1 && scroller.scrollTop <= 0
        && canPull() && !event.target.closest?.("input,textarea,select");
    }, { passive: true });
    scroller.addEventListener("touchmove", event => {
      if (!armed && !pulling) return;
      const touch = event.touches[0];
      const dy = touch.clientY - listStart;
      if (!pulling) {
        const dx = touch.clientX - startX;
        if (Math.abs(dy) < SLOP && Math.abs(dx) < SLOP) return;
        armed = false;
        // Upward or sideways: let the list scroll normally.
        if (dy <= 0 || Math.abs(dx) > Math.abs(dy) || scroller.scrollTop > 0) return;
        pulling = true;
        listStart = touch.clientY;
        startAt = performance.now();
        target.classList.add("dragging");
      }
      event.preventDefault();
      onMove(Math.max(0, touch.clientY - listStart));
    }, { passive: false });
    const finish = event => {
      armed = false;
      if (!pulling) return;
      pulling = false;
      // The click that follows a drag must not press the card under the
      // finger. Only that one click is blocked.
      suppressNextClick = true;
      target.classList.remove("dragging");
      const y = (event.changedTouches && event.changedTouches[0]?.clientY) || listStart;
      const distance = Math.max(0, y - listStart);
      onEnd(distance, distance / Math.max(1, performance.now() - startAt));
    };
    scroller.addEventListener("touchend", finish);
    scroller.addEventListener("touchcancel", event => {
      if (pulling) { pulling = false; target.classList.remove("dragging"); onEnd(0, 0); }
      armed = false;
    });
  }

  function bindSheetDrag() {
    bindDrag(serverSheet, [$("sheetHeader"), serverSheet.querySelector(".grabber")], serverList,
      distance => {
        serverSheet.style.setProperty("--drag-y", `${distance}px`);
        serverScrim.style.opacity = String(Math.max(0, 1 - distance / (innerHeight * .55)));
      },
      (distance, velocity) => {
        if (distance > 100 || velocity > .65) closeServers();
        else { serverSheet.style.removeProperty("--drag-y"); serverScrim.style.opacity = ""; }
      },
      event => !event.target.closest?.("button"),
      () => sheetOpen);
  }

  /* ---------- Panels ---------- */

  function openPanel(name) {
    closeMenu();
    closeServers();
    clearTimeout(panelCloseTimer);
    panel.classList.remove("dragging", "closing");
    panel.style.removeProperty("--panel-drag-y");
    panelOverlay.classList.remove("closing");
    panelOverlay.hidden = false;
    panelOverlay.style.removeProperty("--scrim-opacity");
    // The panel starts moving first; its content is built a frame later, so
    // the opening animation is not blocked by the DOM work.
    afterFirstPaint(() => {
      panelOverlay.classList.add("open");
      panel.classList.add("open");
    });
    const token = ++panelBuildToken;
    afterFrames(3, () => {
      if (token !== panelBuildToken) return;
      if (name === "settings") buildSettings();
      if (name === "subscriptions") buildSubscriptions();
      if (name === "logs") buildLogs();
    });
  }

  function resetPanelState() {
    flushSettings();
    panelBackAction = null;
    splitAppsReceiver = null;
    clearTimeout(pageSwapTimer);
    panelPager?.disconnect();
    panelPager = null;
    refreshBattery = null;
    refreshProxy = null;
    if (appIconObserver) { appIconObserver.disconnect(); appIconObserver = null; }
  }

  function closePanel() {
    if (panelOverlay.hidden) return false;
    resetPanelState();
    panelBuildToken++;
    panel.classList.remove("dragging", "open");
    panel.classList.add("closing");
    panel.style.removeProperty("--panel-drag-y");
    panelOverlay.classList.remove("open");
    panelOverlay.style.removeProperty("--scrim-opacity");
    clearTimeout(panelCloseTimer);
    panelCloseTimer = setTimeout(() => {
      panelOverlay.hidden = true;
      panel.classList.remove("closing");
      panelBody.replaceChildren();
      currentPage = null;
    }, 260);
    return true;
  }

  function bindPanelDrag() {
    bindDrag(panel, [panel.querySelector(".grabber"), panel.querySelector(".panel-header")], panelBody,
      distance => {
        panel.style.setProperty("--panel-drag-y", `${distance}px`);
        // Only the backdrop fades; the panel itself must not become
        // transparent while it is being dragged.
        panelOverlay.style.setProperty("--scrim-opacity", String(Math.max(0, 1 - distance / (innerHeight * .55))));
      },
      (distance, velocity) => {
        if (distance > 100 || velocity > .65) closePanel();
        else { panel.style.removeProperty("--panel-drag-y"); panelOverlay.style.removeProperty("--scrim-opacity"); }
      },
      event => !panelOverlay.hidden && !panel.classList.contains("closing")
        && !event.target.closest?.("button,input,select,textarea,label"),
      () => !panelOverlay.hidden && !panel.classList.contains("closing"));
  }

  // Resets the panel for a new page and returns its emptied body.
  // The change goes in two steps: first the old page leaves together with
  // the title (ten pixels aside and a fade), then the new one arrives. The
  // pages never sit on top of each other: the new page waits hidden while
  // the old one is still on the screen.
  function page(name, backAction = null) {
    // A pending deferred build is obsolete now.
    panelBuildToken++;
    resetPanelState();
    // A transition that never finished should not leave pages behind.
    panelBody.querySelectorAll(".page-out").forEach(node => node.remove());
    const goingBack = pageDirection < 0;
    const back = $("panelBack");
    back.hidden = !backAction;
    back.onclick = event => {
      event.preventDefault();
      event.stopPropagation();
      if (!backAction) return;
      // A step back moves the pages the other way.
      pageDirection = -1;
      backAction();
      pageDirection = 1;
    };
    panelBackAction = backAction;
    const outgoing = currentPage;
    const wrapper = el("div", "page");
    currentPage = wrapper;
    if (goingBack) wrapper.classList.add("back");
    const settle = () => {
      $("panelTitle").textContent = name;
      animateTitle(goingBack);
      wrapper.classList.add("page-in");
      panelBody.scrollTop = 0;
    };
    panelBody.append(wrapper);
    if (outgoing && !noMotion() && outgoing.style.visibility !== "hidden") {
      wrapper.style.visibility = "hidden";
      outgoing.classList.remove("page-in");
      outgoing.classList.add("page-out");
      if (goingBack) outgoing.classList.add("back");
      titleOut(goingBack);
      pageSwapTimer = setTimeout(() => {
        outgoing.remove();
        wrapper.style.visibility = "";
        settle();
      }, 140);
    } else {
      outgoing?.remove();
      settle();
    }
    pageDirection = 1;
    return wrapper;
  }

  // The title of the panel follows its page: the old one leaves with the
  // old page, the new one arrives with the new page.
  function titleOut(goingBack) {
    const title = $("panelTitle");
    title.classList.remove("title-in");
    if (goingBack) title.classList.add("back");
    title.classList.add("title-out");
  }

  function animateTitle(goingBack = false) {
    const title = $("panelTitle");
    title.classList.remove("title-in", "title-out", "back");
    void title.offsetWidth;
    if (goingBack) title.classList.add("back");
    title.classList.add("title-in");
  }

  // Low-end devices and the "reduce motion" setting get no animation at all.
  function noMotion() {
    return document.documentElement.classList.contains("lite")
      || !!(window.matchMedia && matchMedia("(prefers-reduced-motion: reduce)").matches);
  }

  function group(label, ...rows) {
    const fragment = document.createDocumentFragment();
    if (label) fragment.append(el("div", "group-label", label));
    const list = el("div", "list");
    list.append(...rows);
    fragment.append(list);
    return fragment;
  }

  function row(name, hint, control, tag = "div") {
    const node = el(tag, "row");
    const copy = el("span", "row-copy");
    copy.append(el("b", null, name));
    if (hint) copy.append(el("small", null, hint));
    node.append(copy);
    if (control) node.append(control);
    return node;
  }

  function navRow(iconName, name, hint, action) {
    const node = row(name, hint, svg("chevron", "chevron"), "button");
    node.prepend(svg(iconName, "row-icon"));
    node.addEventListener("click", action);
    return node;
  }

  function valueRow(name, value) {
    return row(name, null, el("span", "row-value", value));
  }

  function selectControl(values, current) {
    const select = el("select", "control");
    values.forEach(([value, label]) => {
      const option = new Option(label, value);
      option.selected = value === current;
      select.add(option);
    });
    return select;
  }

  function switchControl(on) {
    const control = el("button", "switch");
    control.setAttribute("role", "switch");
    control.setAttribute("aria-checked", String(!!on));
    control.addEventListener("click", () =>
      control.setAttribute("aria-checked", String(control.getAttribute("aria-checked") !== "true")));
    return control;
  }
  const isOn = control => control.getAttribute("aria-checked") === "true";

  function button(text, kind, action, small = false) {
    const node = el("button", `button ${kind}${small ? " small" : ""}`, text);
    node.addEventListener("click", action);
    return node;
  }

  function actions(...buttons) {
    const node = el("div", "actions");
    node.append(...buttons);
    return node;
  }

  // Settings save themselves when the user leaves the page; no explicit
  // "Save" button is needed. Every section adds its own collector, so a tab
  // can hold several of them at once.
  function saveActions(collect) {
    pendingSettings.push(collect);
    return el("div");
  }

  function flushSettings(stayOnPage = false) {
    const collectors = pendingSettings;
    if (!stayOnPage) pendingSettings = [];
    if (!collectors.length) return;
    const current = snapshot.settings || {};
    const changes = {};
    collectors.forEach(collect => Object.assign(changes, collect()));
    // Lists such as Auto servers may come back in another order.
    const same = (a, b) => Array.isArray(a) && Array.isArray(b)
      ? JSON.stringify([...a].sort()) === JSON.stringify([...b].sort())
      : JSON.stringify(a) === JSON.stringify(b);
    const changed = Object.keys(changes).some(key => !same(changes[key], current[key]));
    if (!changed) return;
    snapshot.settings = { ...current, ...changes };
    native.saveSettings(JSON.stringify(snapshot.settings));
  }

  // The settings are a list of categories; every row opens its own page.
  function buildSettings() {
    const body = page("Настройки");
    body.append(
      group("Серверы",
        navRow("servers", "Подписки", "Ссылки, обновление и HWID", buildServerSettings),
        navRow("ping", "Проверка серверов", "Как и когда проверять серверы", buildPingSettings),
        navRow("auto", "Автовыбор", "Из каких серверов выбирает Auto", buildAutoSettings)),
      group("Подключение",
        navRow("connection", "Подключение и защита", "Автоподключение и Kill Switch", buildConnectionSettings)),
      group("Обход блокировок",
        navRow("tunnel", "Фрагментация и мультиплексирование", "Помогает против DPI", buildTunnelSettings)),
      group("Сеть",
        navRow("globe", "DNS и сеть", "DNS, IPv6, MTU и локальная сеть", buildNetworkSettings),
        navRow("proxy", "Локальный прокси", "SOCKS5 и HTTP, доступ по хотспоту", buildProxySettings)),
      group("Маршрутизация",
        navRow("routing", "Правила маршрутизации", "Что идёт через VPN, а что напрямую", buildRoutingSettings),
        navRow("split", "Раздельное туннелирование", "Какие приложения идут через VPN", buildSplitTunneling)),
      group("Приложение",
        navRow("application", "Работа приложения", "Фон и память", buildApplicationSettings),
        navRow("logs", "Логи", "События приложения и ядра", () => buildLogs(true)),
        navRow("about", "О приложении", `Версия ${snapshot.appVersion || "—"}`, buildAbout)));
  }

  function buildServerSettings() {
    const body = page("Подписки", () => buildSettings());
    body.append(serverSettingsContent());
  }

  function serverSettingsContent() {
    const fragment = document.createDocumentFragment();
    const s = snapshot.settings || {};
    const sourceCount = [snapshot.subscriptions?.wifi, snapshot.subscriptions?.lte].filter(Boolean).length;
    const autoUpdate = selectControl([["15","Каждые 15 минут"],["60","Каждый час"],["360","Каждые 6 часов"],["1440","Раз в сутки"],["off","Выключено"]], s.autoUpdate);
    const updateOnOpen = switchControl(!!s.updateOnOpen);
    const notifyUpdates = switchControl(!!s.notifyUpdates);
    const sendHwid = switchControl(s.sendHwid !== false);
    fragment.append(
      group("Подписки",
        row("Ссылки на подписку", `Настроено ${sourceCount} из 2`,
          button("Изменить", "secondary", () => buildSubscriptions(true), true)),
        row("Отправлять HWID", "Без него панель может не выдать подписку", sendHwid)),
      group("Обновление",
        row("Автообновление", "Не прерывает активное подключение", autoUpdate),
        row("Обновлять при запуске", "Загружать подписку при открытии приложения", updateOnOpen),
        row("Уведомлять об обновлениях", "Сколько серверов добавилось и пропало", notifyUpdates)),
      saveActions(() => ({
        autoUpdate: autoUpdate.value, updateOnOpen: isOn(updateOnOpen),
        notifyUpdates: isOn(notifyUpdates), sendHwid: isOn(sendHwid)
      })));
    return fragment;
  }

  function buildPingSettings() {
    const body = page("Проверка серверов", () => buildSettings());
    body.append(pingSettingsContent());
  }

  function pingSettingsContent() {
    const fragment = document.createDocumentFragment();
    const s = snapshot.settings || {};
    const method = selectControl([["real","Полная"],["tcp","Быстрая"]], s.pingMethod === "tcp" ? "tcp" : "real");
    const methodHint = {
      real: "Запрос идёт через сам сервер: рабочим считается только тот, через который открываются сайты",
      tcp: "Проверяет лишь, что порт открыт. Быстро, но сломанный сервер может выглядеть рабочим"
    };
    const methodRow = row("Способ", null, method);
    const methodDetail = el("small", null, methodHint[method.value]);
    methodRow.querySelector(".row-copy").append(methodDetail);
    method.addEventListener("change", () => { methodDetail.textContent = methodHint[method.value]; });
    const pingOnOpen = switchControl(!!s.pingOnOpen);
    const pingAfterUpdate = switchControl(!!s.pingAfterUpdate);
    fragment.append(
      group("Проверка серверов", methodRow),
      group("Когда проверять",
        row("При запуске", "Для сохранённого списка серверов", pingOnOpen),
        row("После обновления", "Когда подписка загружена заново", pingAfterUpdate)),
      el("p", "group-note", "Auto и балансировщики всегда выбирают среди серверов, прошедших полную проверку."),
      saveActions(() => ({
        pingMethod: method.value, pingOnOpen: isOn(pingOnOpen), pingAfterUpdate: isOn(pingAfterUpdate)
      })));
    return fragment;
  }

  function buildAutoSettings() {
    const body = page("Автовыбор", () => buildSettings());
    body.append(autoSettingsContent());
  }

  function autoSettingsContent() {
    const fragment = document.createDocumentFragment();
    const s = snapshot.settings || {};
    const servers = snapshot.servers.filter(server => !server.auto);
    const saved = new Set(s.autoProfiles || []);
    const chosen = new Set(servers.filter(server => !saved.size || saved.has(server.id)).map(server => server.id));
    fragment.append(el("div", "group-label", "Автовыбор"));
    fragment.append(el("p", "group-note", "Auto подключается к самому быстрому из отмеченных серверов, который прошёл проверку."));
    const list = el("div", "list");
    fragment.append(list);
    if (!servers.length) list.append(row("Список серверов пуст", "Добавьте или обновите подписку"));
    else panelPager = lazyList(list, servers, server => {
      const label = row(server.name, null, null, "label");
      label.classList.add("lazy-row");
      const check = el("input");
      check.type = "checkbox";
      check.checked = chosen.has(server.id);
      check.addEventListener("change", () => check.checked ? chosen.add(server.id) : chosen.delete(server.id));
      label.append(check);
      return label;
    }, panelBody, 40);
    fragment.append(saveActions(() => ({
      autoProfiles: chosen.size === servers.length ? [] : servers.filter(server => chosen.has(server.id)).map(server => server.id)
    })));
    return fragment;
  }

  function buildConnectionSettings() {
    const body = page("Подключение и защита", () => buildSettings());
    body.append(connectionSettingsContent());
  }

  function connectionSettingsContent() {
    const fragment = document.createDocumentFragment();
    const s = snapshot.settings || {};
    const killSwitch = switchControl(!!s.killSwitch);
    const autoConnectOpen = switchControl(!!s.autoConnectOpen);
    const autoConnectBoot = switchControl(!!s.autoConnectBoot);
    fragment.append(
      group("Автоподключение",
        row("При запуске приложения", "К выбранному серверу", autoConnectOpen),
        row("При загрузке устройства", "Если разрешение на VPN уже выдано", autoConnectBoot)),
      group("Защита",
        row("Kill Switch", "Без интернета, пока VPN не подключится или если сервер пропал", killSwitch),
        row("Системная блокировка", "Если Android остановит приложение: «Постоянная VPN» и «Блокировать без VPN»",
          button("Открыть", "secondary", () => native.openVpnSettings(), true))),
      saveActions(() => ({
        autoConnectOpen: isOn(autoConnectOpen), autoConnectBoot: isOn(autoConnectBoot), killSwitch: isOn(killSwitch)
      })));
    return fragment;
  }

  function buildNetworkSettings() {
    const body = page("DNS и сеть", () => buildSettings());
    body.append(networkSettingsContent());
  }

  function networkSettingsContent() {
    const fragment = document.createDocumentFragment();
    const s = snapshot.settings || {};
    const dns = selectControl([["subscription-doh","Из подписки"],["cloudflare","Cloudflare"],["google","Google"],["quad9","Quad9"],["custom","Свой"]], s.dnsProvider);
    const customDns = el("textarea", "field");
    customDns.placeholder = "1.1.1.1\n8.8.8.8";
    customDns.value = s.customDns || "";
    customDns.spellcheck = false;
    customDns.rows = 2;
    const customRow = row("Свои DNS-серверы", "До четырёх IPv4 или IPv6, по одному на строку", customDns);
    customRow.classList.add("stack");
    const syncDns = () => { customRow.hidden = dns.value !== "custom"; };
    dns.addEventListener("change", syncDns);
    syncDns();
    const dnsHint = { "subscription-doh": s.doh || "DoH из подписки", cloudflare: "1.1.1.1", google: "8.8.8.8", quad9: "9.9.9.9", custom: "Свои серверы" };
    const dnsRow = row("DNS", null, dns);
    const dnsDetail = el("small", null, dnsHint[dns.value] || "");
    dnsRow.querySelector(".row-copy").append(dnsDetail);
    dns.addEventListener("change", () => { dnsDetail.textContent = dnsHint[dns.value] || ""; });

    const ipv6 = switchControl(!!s.ipv6Enabled);
    const ipType = selectControl([["auto","Авто"],["ipv4","IPv4"],["ipv6","IPv6"]], s.ipv6Enabled ? (s.ipType || "auto") : (s.ipType === "ipv6" ? "auto" : s.ipType || "auto"));
    const ipTypeRow = row("Тип IP", null, ipType);
    const ipTypeDetail = el("small");
    ipTypeRow.querySelector(".row-copy").append(ipTypeDetail);
    // IPv6-only answers make no sense without an IPv6 tunnel.
    const syncIpType = () => {
      const on = isOn(ipv6);
      ipType.querySelector('option[value="ipv6"]').disabled = !on;
      if (!on && ipType.value === "ipv6") ipType.value = "auto";
      ipTypeDetail.textContent = on ? "Какие адреса получают приложения" : "Без IPv6 приложения получают только IPv4";
    };
    ipv6.addEventListener("click", syncIpType);
    syncIpType();
    const lan = switchControl(s.lanDirect !== false);
    const mtu = selectControl([["1280","1280"],["1360","1360"],["1400","1400"],["1500","1500"]], s.tunMtu || "1400");
    fragment.append(
      group("DNS", dnsRow, customRow),
      el("p", "group-note", "Все DNS-запросы идут через VPN, провайдер их не видит."),
      group("Туннель",
        row("IPv6", "Включайте, если сервер поддерживает IPv6", ipv6),
        ipTypeRow,
        row("Локальная сеть напрямую", "Роутер, принтеры и трансляция на ТВ без VPN", lan),
        row("MTU туннеля", "1400 подходит для большинства сетей", mtu)),
      saveActions(() => ({
        dnsProvider: dns.value, customDns: customDns.value, tunMtu: mtu.value,
        ipv6Enabled: isOn(ipv6), ipType: ipType.value, lanDirect: isOn(lan)
      })));
    return fragment;
  }

  function buildRoutingSettings() {
    const body = page("Маршрутизация", () => buildSettings());
    body.append(routingSettingsContent());
  }

  function routingSettingsContent() {
    const fragment = document.createDocumentFragment();
    const s = snapshot.settings || {};
    const mode = selectControl([["full","Весь трафик"],["bypass","Кроме правил"],["proxy_only","Только правила"]], s.routingMode);
    const modeHint = {
      full: "Правила ниже не применяются",
      bypass: "Совпавшие с правилами сайты открываются напрямую",
      proxy_only: "Через VPN идут только совпавшие с правилами сайты"
    };
    const modeRow = row("Режим", null, mode);
    const modeDetail = el("small", null, modeHint[mode.value] || "");
    modeRow.querySelector(".row-copy").append(modeDetail);
    mode.addEventListener("change", () => { modeDetail.textContent = modeHint[mode.value] || ""; });

    const rules = el("textarea", "field");
    rules.placeholder = "example.com\ngeosite:youtube\ngeoip:ru";
    rules.value = s.routingRules || "";
    rules.spellcheck = false;
    const rulesRow = row("Правила", "По одному на строку: домен, full:, keyword:, regexp:, geosite: или geoip:", rules);
    rulesRow.classList.add("stack");
    const preset = button("Российские сайты напрямую", "secondary", () => {
      const lines = rules.value.split(/\n/).map(line => line.trim()).filter(Boolean);
      ["geosite:category-ru", "geoip:ru"].forEach(item => { if (!lines.includes(item)) lines.push(item); });
      rules.value = lines.join("\n");
      if (mode.value === "full") { mode.value = "bypass"; mode.dispatchEvent(new Event("change")); }
    });
    rulesRow.append(preset);

    const subscriptionRules = switchControl(s.subscriptionRules !== false);
    const routeOnly = switchControl(!!s.routeOnly);
    const blockUdp = switchControl(!!s.blockUdp);
    fragment.append(
      group("Свои правила", modeRow, rulesRow),
      group("Подписка",
        row("Правила из подписки", "Маршруты, которые задал провайдер в Xray JSON", subscriptionRules)),
      group("Трафик",
        row("Маршрутизация по домену", "Домен только для правил, подключение к исходному IP", routeOnly),
        row("Блокировать UDP", "Кроме DNS. Игры и звонки могут не работать", blockUdp)));

    const geoIP = el("input", "field");
    geoIP.type = "url"; geoIP.spellcheck = false; geoIP.placeholder = "runetfreedom · geoip.dat";
    geoIP.value = s.geoipUrl || "";
    const geoSite = el("input", "field");
    geoSite.type = "url"; geoSite.spellcheck = false; geoSite.placeholder = "v2fly · dlc.dat";
    geoSite.value = s.geositeUrl || "";
    const geoIPRow = row("GeoIP", "Списки IP для geoip:", geoIP);
    const geoSiteRow = row("GeoSite", "Списки доменов для geosite:", geoSite);
    geoIPRow.classList.add("stack");
    geoSiteRow.classList.add("stack");
    fragment.append(
      group("Источники списков", geoIPRow, geoSiteRow),
      el("p", "group-note", "Списки скачиваются при первом подключении с такими правилами и обновляются раз в неделю. Оставьте поля пустыми, чтобы использовать источники по умолчанию."),
      saveActions(() => ({
        routingMode: mode.value, routingRules: rules.value, subscriptionRules: isOn(subscriptionRules),
        routeOnly: isOn(routeOnly), blockUdp: isOn(blockUdp),
        geoipUrl: geoIP.value.trim(), geositeUrl: geoSite.value.trim()
      })));
    return fragment;
  }

  function appIcon(item) {
    const icon = el("span", "app-icon", (item.name || "?").slice(0, 1).toUpperCase());
    icon.dataset.package = item.packageName;
    if (item.icon) setAppIcon(icon, item.icon);
    return icon;
  }

  function setAppIcon(slot, source) {
    if (!slot || !source || slot.querySelector("img")) return;
    const image = el("img");
    image.src = source;
    image.alt = "";
    image.decoding = "async";
    slot.textContent = "";
    slot.append(image);
  }

  function buildSplitTunneling() {
    const body = page("Раздельное туннелирование", () => buildSettings());
    body.append(splitTunnelingContent());
  }

  function splitTunnelingContent() {
    const fragment = document.createDocumentFragment();
    const s = snapshot.settings || {};
    const selected = new Set(s.appRoutingPackages || []);
    let data = { apps: [] };

    const modes = el("div", "list");
    [["all", "Все приложения", "Весь трафик идёт через VPN"],
     ["exclude", "Кроме выбранных", "Отмеченные приложения работают напрямую"],
     ["include", "Только выбранные", "Через VPN идут только отмеченные"]].forEach(([value, name, hint]) => {
      const label = row(name, hint, null, "label");
      const radio = el("input");
      radio.type = "radio"; radio.name = "app-routing"; radio.value = value;
      radio.checked = (s.appRoutingMode || "all") === value;
      label.append(radio);
      modes.append(label);
    });
    fragment.append(el("div", "group-label", "Режим"), modes, el("div", "group-label", "Приложения"));

    const searchWrap = el("div", "search");
    const search = el("input", "field");
    search.type = "search";
    search.placeholder = "Поиск";
    searchWrap.append(search);
    const list = el("div", "list");
    list.append(el("div", "loading"));
    list.firstChild.append(el("i"), el("span", null, "Загружаем приложения…"));
    fragment.append(searchWrap, list);

    // Icons are requested from Android only when their row is near the viewport.
    appIconObserver = "IntersectionObserver" in window ? new IntersectionObserver(entries => {
      entries.forEach(entry => {
        if (!entry.isIntersecting) return;
        const slot = entry.target;
        appIconObserver.unobserve(slot);
        if (!slot.dataset.loading && !slot.querySelector("img")) {
          slot.dataset.loading = "1";
          native.requestAppIcon?.(slot.dataset.package || "");
        }
      });
    }, { root: panelBody, rootMargin: "200px 0px" }) : null;

    const renderApp = item => {
      const label = row(item.name, item.packageName, null, "label");
      label.classList.add("lazy-row");
      const icon = appIcon(item);
      label.prepend(icon);
      const check = el("input");
      check.type = "checkbox";
      check.checked = selected.has(item.packageName);
      check.addEventListener("change", () => check.checked ? selected.add(item.packageName) : selected.delete(item.packageName));
      label.append(check);
      if (appIconObserver) appIconObserver.observe(icon);
      else native.requestAppIcon?.(item.packageName);
      return label;
    };

    const draw = () => {
      if (data.loading) return;
      const query = search.value.trim().toLocaleLowerCase("ru");
      const filtered = data.apps.filter(item => !query
        || item.name.toLocaleLowerCase("ru").includes(query)
        || item.packageName.toLowerCase().includes(query));
      panelPager?.disconnect();
      appIconObserver?.disconnect();
      list.replaceChildren();
      if (!filtered.length) { list.append(el("div", "empty-list", data.error || "Ничего не найдено")); return; }
      panelPager = lazyList(list, filtered, renderApp, panelBody, 30);
    };
    let searchTimer = 0;
    search.addEventListener("input", () => { clearTimeout(searchTimer); searchTimer = setTimeout(draw, 120); });
    splitAppsReceiver = payload => {
      try { data = typeof payload === "string" ? JSON.parse(payload) : payload; }
      catch (_) { data = { apps: [], error: "Не удалось прочитать список приложений" }; }
      data.apps = data.apps || [];
      draw();
    };
    if (hasNativeBridge && typeof native.requestInstalledApps === "function") native.requestInstalledApps();
    else {
      try { splitAppsReceiver(native.getInstalledApps() || '{"apps":[]}'); }
      catch (_) { splitAppsReceiver({ apps: [] }); }
    }
    fragment.append(saveActions(() => ({
      appRoutingMode: modes.querySelector("input:checked")?.value || "all",
      appRoutingPackages: [...selected]
    })));
    return fragment;
  }

  function buildTunnelSettings() {
    const body = page("Обход блокировок", () => buildSettings());
    body.append(tunnelSettingsContent());
  }

  function tunnelSettingsContent() {
    const fragment = document.createDocumentFragment();
    const s = snapshot.settings || {};
    const fragmentation = switchControl(!!s.fragmentation);
    const mux = switchControl(!!s.mux);
    fragment.append(group("Протокол",
      row("Фрагментация TLS", "Делит ClientHello, помогает против DPI", fragmentation),
      row("Мультиплексирование", "Mux: несколько соединений в одном. Не для Vision и XHTTP", mux)));
    fragment.append(saveActions(() => ({ fragmentation: isOn(fragmentation), mux: isOn(mux) })));
    return fragment;
  }

  function buildProxySettings() {
    const body = page("Локальный прокси", () => buildSettings());
    body.append(proxySettingsContent());
  }

  function proxySettingsContent() {
    const fragment = document.createDocumentFragment();
    const s = snapshot.settings || {};
    const enabled = switchControl(!!s.proxyEnabled);
    const http = switchControl(!!s.proxyHttp);
    const lan = switchControl(!!s.proxyLan);
    const only = switchControl(!!s.proxyOnly);
    const portField = (value, fallback) => {
      const input = el("input", "field port-field");
      input.type = "number";
      input.inputMode = "numeric";
      input.min = "1024";
      input.max = "65535";
      input.value = value || fallback;
      return input;
    };
    const socksPort = portField(s.proxySocksPort, "10808");
    const httpPort = portField(s.proxyHttpPort, "10809");
    fragment.append(
      el("p", "group-note", "Другие приложения на телефоне видят открытые порты, поэтому прокси работает только с логином и паролем."),
      group("Прокси",
        row("SOCKS5-прокси", "Через то же подключение, что и VPN", enabled),
        row("Порт SOCKS5", null, socksPort),
        row("HTTP-прокси", "Для приложений без поддержки SOCKS5", http),
        row("Порт HTTP", null, httpPort)),
      group("Доступ",
        row("Доступ через хотспот", "Для устройств в вашей Wi-Fi-сети или точке доступа. UDP только на самом телефоне", lan)));

    const address = el("small", "mono");
    const user = el("small", "mono");
    const pass = el("small", "mono");
    const credentialRow = (name, value, field) => {
      const node = row(name, null, button("Копировать", "secondary", () => native.copyText(field() || ""), true));
      node.querySelector(".row-copy").append(value);
      return node;
    };
    const addressRow = row("Адрес", null);
    addressRow.querySelector(".row-copy").append(address);
    fragment.append(group("Подключение",
      addressRow,
      credentialRow("Логин", user, () => snapshot.settings?.proxyUser),
      credentialRow("Пароль", pass, () => snapshot.settings?.proxyPass)));
    fragment.append(actions(button("Сбросить логин и пароль", "secondary", () => native.resetProxyCredentials())));
    fragment.append(group("Режим",
      row("Только прокси, без VPN", "Без туннеля и значка VPN: прокси нужно указать в приложениях вручную", only)));

    refreshProxy = settings => {
      const hosts = isOn(lan) ? (settings.proxyAddresses || []) : [];
      const host = hosts[0] || (isOn(lan) ? "IP телефона в сети" : "127.0.0.1");
      const ports = [`SOCKS5 ${socksPort.value}`].concat(isOn(http) ? [`HTTP ${httpPort.value}`] : []);
      address.textContent = `${host} · ${ports.join(" · ")}`;
      user.textContent = settings.proxyUser || "Появится после сохранения";
      pass.textContent = settings.proxyPass || "Появится после сохранения";
    };
    const sync = () => refreshProxy(snapshot.settings || {});
    [lan, http].forEach(control => control.addEventListener("click", sync));
    [socksPort, httpPort].forEach(field => field.addEventListener("input", sync));
    sync();

    fragment.append(saveActions(() => ({
      proxyEnabled: isOn(enabled), proxyHttp: isOn(http), proxyLan: isOn(lan), proxyOnly: isOn(only),
      proxySocksPort: socksPort.value, proxyHttpPort: httpPort.value
    })));
    return fragment;
  }

  function buildApplicationSettings() {
    const body = page("Работа приложения", () => buildSettings());
    body.append(applicationSettingsContent());
  }

  function applicationSettingsContent() {
    const fragment = document.createDocumentFragment();
    const s = snapshot.settings || {};
    const battery = row("Работа в фоне", null,
      button("", "secondary", () => native.openBatterySettings(), true));
    const hint = el("small");
    battery.querySelector(".row-copy").append(hint);
    // Android reports the new state when the user comes back from settings.
    refreshBattery = unrestricted => {
      hint.textContent = unrestricted
        ? "Ограничений нет, VPN не отключится в фоне"
        : "Откроются настройки приложения: Батарея → Без ограничений";
      battery.querySelector("button").textContent = unrestricted ? "Открыть" : "Настроить";
    };
    refreshBattery(!!s.batteryUnrestricted);
    const memory = selectControl([["0","Без ограничения"],["40","40 МБ"],["60","60 МБ"],["80","80 МБ"],["100","100 МБ"],["150","150 МБ"]], s.memoryLimit || "0");
    fragment.append(
      group("Фоновая работа", battery),
      group("Память", row("Память ядра", "Лимит помогает на слабых устройствах", memory)),
      saveActions(() => ({ memoryLimit: memory.value })));
    return fragment;
  }

  function buildAbout() {
    const body = page("О приложении", () => buildSettings());
    body.append(aboutContent());
  }

  function aboutContent() {
    const fragment = document.createDocumentFragment();
    const head = el("div", "about-head");
    const logo = el("img");
    logo.src = "logo.png"; logo.alt = ""; logo.width = 56; logo.height = 56;
    head.append(logo, el("h3", null, "ShadowVPN"), el("p", null, `Версия ${snapshot.appVersion || "—"}`));
    fragment.append(head);

    const hwid = snapshot.settings?.hwid || "Недоступно";
    const hwidRow = row("HWID", hwid, button("Копировать", "secondary", () => native.copyText(snapshot.settings?.hwid || ""), true));
    hwidRow.querySelector("small").classList.add("mono");
    fragment.append(group("Устройство", hwidRow));
    fragment.append(group("Компоненты",
      valueRow("Ядро", "Xray Core"),
      valueRow("QUIC и Naive", "Cronet"),
      valueRow("TLS-отпечаток", "uTLS · Chrome 152"),
      valueRow("Протоколы", "VLESS, VMess, Trojan, Shadowsocks, Hysteria2, Naive"),
      valueRow("Платформа", "Android 10+ · arm64, x86_64")));
    return fragment;
  }

  function buildSubscriptions(fromSettings = false) {
    const body = page("Подписки", fromSettings ? () => buildServerSettings() : null);
    const slots = snapshot.subscriptions || {wifi:"",lte:""};
    const slot = (kind, iconName, name, hint) => {
      const node = el("div", "row stack");
      const head = row(name, hint);
      head.className = "slot-head";
      head.prepend(svg(iconName, "row-icon"));
      const input = el("input", "field");
      input.type = "url"; input.inputMode = "url"; input.spellcheck = false; input.autocomplete = "off";
      input.placeholder = "https://";
      input.value = slots[kind] || "";
      node.append(head, input);
      return { node, input };
    };
    const wifi = slot("wifi", "wifi", "Wi-Fi", "Используется в сетях Wi-Fi");
    const lte = slot("lte", "lte", "Мобильная сеть", "Используется в LTE и 5G");
    body.append(group("Подписки", wifi.node, lte.node));
    const manual = el("textarea", "field");
    manual.spellcheck = false;
    manual.placeholder = "vless://…\nvmess://…\nss://…\n[{ \"remarks\": …, \"outbounds\": [ … ] }]";
    manual.value = slots.manual || "";
    manual.classList.add("mono");
    const manualRow = row("Свои конфигурации", "Xray JSON или ссылки vless, vmess, trojan, ss, hy2, naive", manual);
    manualRow.classList.add("stack");
    body.append(group("Вручную", manualRow));
    body.append(actions(
      button("Сохранить и обновить", "primary", () => {
        const working = { wifi: wifi.input.value.trim(), lte: lte.input.value.trim(), manual: manual.value.trim() };
        if ([working.wifi, working.lte].some(value => value && !value.startsWith("https://"))) { showToast("Нужна ссылка, начинающаяся с https://"); return; }
        if (working.wifi && working.wifi === working.lte) { showToast("Для Wi-Fi и мобильной сети нужны разные ссылки"); return; }
        snapshot.subscriptions = working;
        native.saveSubscriptions(JSON.stringify(working));
        if (fromSettings) buildServerSettings(); else closePanel();
      }),
      button("Удалить все источники", "danger", () => {
        snapshot.subscriptions = {wifi:"",lte:"",manual:""};
        native.saveSubscriptions('{"wifi":"","lte":"","manual":""}');
        closePanel();
      })));
  }

  function buildLogs(fromSettings = false) {
    const body = page("Логи", fromSettings ? () => buildSettings() : null);
    const log = el("pre", "log-box", native.getLogs() || "Событий пока нет");
    body.append(log, actions(button("Копировать", "secondary", () => native.copyText(log.textContent))));
  }

  function closeTopLayer() {
    if ($("appMenu").classList.contains("open")) { closeMenu(); return true; }
    if (!panelOverlay.hidden && panelBackAction) { panelBackAction(); return true; }
    if (closePanel()) return true;
    if ($("sortMenu").classList.contains("open")) { toggleSortMenu(false); return true; }
    if (sheetOpen) return closeServers();
    return false;
  }

  /* ---------- Wiring ---------- */

  document.addEventListener("visibilitychange", () => { if (document.hidden) flushSettings(true); });

  $("powerButton").addEventListener("click", () => native.toggleVpn());
  $("menuButton").addEventListener("click", event => { event.stopPropagation(); setMenu(!$("appMenu").classList.contains("open")); });
  document.addEventListener("click", event => {
    if (!event.target.closest(".app-menu,#menuButton")) closeMenu();
    if (!event.target.closest("#sortMenu,#sortButton")) toggleSortMenu(false);
  });
  document.querySelectorAll("[data-panel]").forEach(item => item.addEventListener("click", () => openPanel(item.dataset.panel)));
  $("refreshButton").addEventListener("click", () => { closeMenu(); native.refreshSubscriptions(); });
  $("closePanel").addEventListener("click", closePanel);
  panelOverlay.addEventListener("click", event => { if (event.target === panelOverlay) closePanel(); });
  $("closeServers").addEventListener("click", closeServers);
  serverScrim.addEventListener("click", closeServers);
  $("selectedServerButton").addEventListener("click", openServers);
  // One handler for the whole list: the cards are reused between renders, so
  // they carry no listeners of their own.
  serverList.addEventListener("click", event => {
    const card = event.target.closest(".server-card");
    if (!card || !serverList.contains(card)) return;
    native.selectServer(card.dataset.id);
    window.setTimeout(closeServers, 110);
  });
  $("categories").addEventListener("click", event => {
    const item = event.target.closest("button[data-category]");
    if (!item) return;
    setCategory(item.dataset.category);
    serverList.scrollTop = 0;
    renderServers(true);
  });
  $("sortButton").addEventListener("click", event => { event.stopPropagation(); toggleSortMenu(); });
  $("sortMenu").addEventListener("click", event => {
    const item = event.target.closest("button[data-sort]");
    if (!item) return;
    sortMode = item.dataset.sort;
    toggleSortMenu(false);
    serverList.scrollTop = 0;
    renderServers(true);
  });
  $("pingButton").addEventListener("click", () => native.pingServers());
  const submitWelcome = () => {
    const value = $("welcomeUrl").value.trim();
    if (!value) { $("welcomeError").textContent = "Вставьте ссылку на подписку или конфигурацию"; return; }
    $("welcomeError").textContent = "";
    // A single https:// link is a subscription; anything else is a config.
    const subscription = /^https:\/\/\S+$/.test(value);
    native.saveSubscriptions(JSON.stringify(subscription ? {wifi:value,lte:"",manual:""} : {wifi:"",lte:"",manual:value}));
  };
  $("welcomeSubmit").addEventListener("click", submitWelcome);
  $("welcomeUrl").addEventListener("keydown", event => { if (event.key === "Enter" && !event.shiftKey && !$("welcomeUrl").value.includes("\n")) { event.preventDefault(); submitWelcome(); } });
  bindDockGesture(); bindSheetDrag(); bindPanelDrag();

  window.ShadowVPN = {
    render,
    closeTopLayer,
    // A check starts: every host waits for its own answer again.
    pingStarted() { clearPingResults(); },
    // One host answered: show its latency right away.
    pingResult(item) { applyPingResult(item); },
    // The selection changed: mark the card without rebuilding the list.
    selectedServer(id) { applySelectedServer(id); },
    installedApps(payload) { if (splitAppsReceiver) splitAppsReceiver(payload); },
    appIcon(packageName, source) {
      panelBody.querySelectorAll(".app-icon").forEach(slot => {
        if (slot.dataset.package === packageName) setAppIcon(slot, source);
      });
    }
  };

  if (hasNativeBridge) { native.ready(); return; }

  // Browser-only preview with mock data; the Android bridge never executes this.
  const countries = ["fi","nl","pl","de","se","ee","lv","tr","kz","us","gb","fr","jp","sg","ch","at","cz","ro","rs","am"];
  const mockServers = [{id:"auto",name:"Авто",countryCode:"",protocol:"AUTO",transport:"",auto:true,selected:false,latency:43,available:true}];
  for (let i = 0; i < 120; i++) {
    const code = countries[i % countries.length];
    mockServers.push({id:`s${i}`,source:i % 5 === 0 ? "LTE" : "Wi-Fi",name:`${code.toUpperCase()} ${i + 1} | ${["Hysteria","Torrent","Gemini | gRPC","WARP"][i % 4]}`,countryCode:code,protocol:"VLESS",transport:["TCP","GRPC","WS"][i % 3],format:i % 3 === 0 ? "json" : "",auto:false,selected:i === 0,latency:i % 7 === 6 ? 0 : 40 + (i * 37) % 500,available:i % 7 !== 6});
  }
  mockServers.splice(1, 0, {id:"group",name:"Авто-выбор 🚀",countryCode:"",protocol:"BALANCER",transport:"",members:8,format:"json",source:"Wi-Fi",auto:false,selected:false,latency:52,available:true});
  mockServers[1].subscription = {title:"ShadowVPN",used:12884901888,total:107374182400,expire:1893456000};
  const mockApps = Array.from({length: 180}, (_, i) => ({name:`Приложение ${i + 1}`,packageName:`com.example.app${i + 1}`}));
  native.getInstalledApps = () => JSON.stringify({apps: mockApps});
  render({
    state:"connected",serviceStatus:"Подключено",needsSubscription:false,importRunning:false,pingRunning:false,message:"",appVersion:"0.16.0",
    publicIp:"132.243.***",session:"00:02:18",traffic:{downloadSpeed:"25,7 КБ/с",downloadTotal:"6,1 МБ",uploadSpeed:"58,8 КБ/с",uploadTotal:"135 КБ"},
    selectedId:mockServers[1].id,
    servers:mockServers,
    subscriptions:{wifi:"https://example.com/wifi",lte:"https://example.com/lte",manual:""},
    settings:{autoUpdate:"1440",updateOnOpen:false,notifyUpdates:false,sendHwid:true,pingOnOpen:true,pingAfterUpdate:true,autoConnectOpen:false,autoConnectBoot:false,killSwitch:false,mux:false,ipType:"auto",routeOnly:false,blockUdp:false,memoryLimit:"0",proxyEnabled:true,proxyHttp:true,proxySocksPort:"10808",proxyHttpPort:"10809",proxyLan:true,proxyOnly:false,proxyUser:"svpnA7kq",proxyPass:"Xm4tQ9rWb2nLp8Vz",proxyAddresses:["192.168.43.1"],dnsProvider:"subscription-doh",routingMode:"full",routingRules:"",pingMethod:"real",fragmentation:true,tunMtu:"1400",ipv6Enabled:false,appRoutingMode:"all",appRoutingPackages:[],doh:"dns.shadowvpn.io",lanDirect:true,customDns:"",subscriptionRules:true,geoipUrl:"",geositeUrl:"",autoProfiles:[],hwid:"5191f00c7aa812b926a9f785944ff53c18c7e846969125ba66dced29a123ff53",batteryUnrestricted:false}
  });
  if (params.has("disconnected")) render({ state:"disconnected", serviceStatus:"", publicIp:"85.95.178.108", session:"" });
  if (params.has("blocked")) render({ state:"blocked", serviceStatus:"Ошибка: сервер не отвечает", publicIp:"" });
  if (params.has("error")) render({ state:"error", serviceStatus:"Ошибка: не удалось подключиться к серверу", publicIp:"85.95.178.108" });
  const preview = params.get("preview");
  if (preview === "servers") { openServers(); renderServers(true); }
  if (preview === "menu") setMenu(true);
  if (preview === "welcome") render({ needsSubscription:true });
  if (["settings", "subscriptions", "logs"].includes(preview)) {
    openPanel(preview);
    ({ settings:buildSettings, subscriptions:buildSubscriptions, logs:buildLogs })[preview]();
  }
  if (["providers", "ping", "network", "connection", "tunnel", "proxy", "routing", "split", "application", "about", "auto"].includes(preview)) {
    openPanel("settings");
    ({ providers:buildServerSettings, ping:buildPingSettings, network:buildNetworkSettings, connection:buildConnectionSettings, tunnel:buildTunnelSettings, proxy:buildProxySettings, routing:buildRoutingSettings, split:buildSplitTunneling, application:buildApplicationSettings, about:buildAbout, auto:buildAutoSettings })[preview]();
  }
  if (preview === "tab") {
    openPanel("settings");
    buildSettings();
  }
  // The preview is a still picture: it has no time to wait for the opening
  // animation, so the layers are put into their final state right away.
  document.documentElement.classList.add("no-anim");
  serverScrim.classList.add("open");
  serverSheet.classList.add("open");
  panelOverlay.classList.add("open");
  panel.classList.add("open");
})();

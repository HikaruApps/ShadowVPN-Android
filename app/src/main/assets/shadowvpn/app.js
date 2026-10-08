(() => {
  "use strict";

  const $ = id => document.getElementById(id);
  const hasNativeBridge = !!window.ShadowVpnAndroid;
  const native = window.ShadowVpnAndroid || {
    ready() {}, toggleVpn() {}, selectServer() {}, refreshSubscriptions() {}, pingServers() {},
    saveSubscriptions() {}, saveSettings() {}, getLogs() { return ""; }, copyText() {}, openVpnSettings() {},
    getInstalledApps() { return '{"apps":[]}'; }, requestInstalledApps() {}, requestAppIcon() {}, openBatterySettings() {}
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
  let categoriesSignature = "";
  let serverPager = null;
  let dockFlagKey = null;
  let statusKey = "";
  let toastTimer = 0;
  let lastMessage = "";
  let sheetOpen = false;
  let panelBackAction = null;
  let panelCloseTimer = 0;
  let panelPager = null;
  let splitAppsReceiver = null;
  let appIconObserver = null;
  let refreshBattery = null;

  const ICONS = {
    auto: '<path d="M13 3 5 13.5h6L10 21l8-10.5h-6z"/>',
    globe: '<circle cx="12" cy="12" r="9"/><path d="M3 12h18M12 3c2.5 2.6 3.7 5.6 3.7 9s-1.2 6.4-3.7 9c-2.5-2.6-3.7-5.6-3.7-9S9.5 5.6 12 3Z"/>',
    servers: '<rect x="4" y="4" width="16" height="6" rx="1.5"/><rect x="4" y="14" width="16" height="6" rx="1.5"/><path d="M8 7h.01M8 17h.01"/>',
    connection: '<path d="M5 12.5a10 10 0 0 1 14 0M8 15.5a5.5 5.5 0 0 1 8 0M12 19h.01"/>',
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

  function plural(count, forms) {
    const n = Math.abs(count) % 100, d = n % 10;
    if (n > 10 && n < 20) return forms[2];
    if (d === 1) return forms[0];
    if (d > 1 && d < 5) return forms[1];
    return forms[2];
  }

  function flag(server) {
    const slot = el("span", "server-flag");
    if (server?.auto) {
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
    return [server.protocol, server.transport].filter(Boolean).join(" · ");
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
  }

  function setCategory(name) {
    category = name;
    $("categories").querySelectorAll("button[data-category]").forEach(item =>
      item.classList.toggle("active", item.dataset.category === name));
  }

  function serverCard(server) {
    const card = el("button", `server-card${server.selected ? " selected" : ""}`);
    const copy = el("span", "server-copy");
    const details = el("small");
    fillMeta(details, server);
    copy.append(el("b", null, server.name), details);
    const info = latencyText(server);
    card.append(flag(server), copy, el("span", `latency ${info.cls}`, info.text));
    card.addEventListener("click", () => {
      native.selectServer(server.id);
      window.setTimeout(closeServers, 110);
    });
    return card;
  }

  function renderServers(force = false) {
    // The list is built only while the sheet is visible.
    if (!sheetOpen) { serversSignature = ""; return; }
    updateCategories();
    const visible = visibleServers();
    const signature = JSON.stringify([category, sortMode, snapshot.pingRunning, snapshot.importRunning,
      visible.map(item => [item.id, item.selected, item.latency, item.available])]);
    if (!force && signature === serversSignature) return;
    serversSignature = signature;
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
    serverPager = lazyList(serverList, visible, serverCard, serverList, 40);
    serverList.scrollTop = scroll;
  }

  function renderStatus(connected, connecting) {
    const selected = snapshot.selected;
    const error = snapshot.state === "error";
    const key = [snapshot.state, selected?.id, selected?.name, error ? snapshot.serviceStatus : ""].join("\u0000");
    if (key === statusKey) return;
    statusKey = key;
    setText("statusText", connected ? "Подключено"
      : connecting ? "Подключение…"
      : error ? "Ошибка подключения"
      : "Не подключено");
    const detail = $("statusDetail");
    detail.replaceChildren();
    if (error) {
      const text = String(snapshot.serviceStatus || "").replace(/^Ошибка:\s*/, "") || "Не удалось подключиться";
      detail.textContent = text.charAt(0).toUpperCase() + text.slice(1);
    } else if ((connected || connecting) && selected) {
      detail.append(flag(selected), el("span", null, selected.name));
    } else {
      detail.textContent = selected ? "Нажмите, чтобы подключиться" : "Выберите сервер ниже";
    }
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
    const width = `${percent}%`;
    if (bar.style.width !== width) bar.style.width = width;
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
      servers: (next && next.servers) || snapshot.servers || [],
      subscriptions: (next && next.subscriptions) || snapshot.subscriptions || {wifi:"",lte:""}
    };
    const state = snapshot.state || "disconnected";
    if (app.dataset.state !== state) app.dataset.state = state;
    const connected = state === "connected";
    const connecting = state === "connecting";
    const power = $("powerButton");
    power.disabled = connecting || !snapshot.selected;
    power.setAttribute("aria-label", connected ? "Отключить VPN" : "Подключить VPN");
    renderStatus(connected, connecting);

    setText("ipLabel", connected ? "Защищённый IP" : "IP");
    setText("ipValue", snapshot.publicIp || (connecting ? "—" : "Определяем…"));
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

  /* ---------- Menus ---------- */

  function setMenu(open) {
    $("appMenu").hidden = !open;
    $("menuButton").setAttribute("aria-expanded", String(open));
  }
  function closeMenu() { setMenu(false); }

  function toggleSortMenu(force) {
    const menu = $("sortMenu");
    const open = typeof force === "boolean" ? force : menu.hidden;
    if (open) menu.querySelectorAll("button[data-sort]").forEach(button =>
      button.setAttribute("aria-checked", String(button.dataset.sort === sortMode)));
    menu.hidden = !open;
    $("sortButton").setAttribute("aria-expanded", String(open));
  }

  /* ---------- Server sheet ---------- */

  function openServers() {
    if (sheetOpen) return;
    sheetOpen = true;
    closeMenu();
    serverScrim.hidden = false;
    serverSheet.setAttribute("aria-hidden", "false");
    renderServers(true);
    requestAnimationFrame(() => serverSheet.classList.add("open"));
  }

  function closeServers() {
    if (!sheetOpen) return false;
    toggleSortMenu(false);
    sheetOpen = false;
    serverSheet.classList.remove("dragging", "open");
    serverSheet.style.removeProperty("--drag-y");
    serverScrim.style.opacity = "0";
    serverSheet.setAttribute("aria-hidden", "true");
    setTimeout(() => {
      if (sheetOpen) return;
      serverScrim.hidden = true;
      serverScrim.style.opacity = "";
      // Free the list while it is not visible.
      serverPager?.disconnect();
      serverPager = null;
      serverList.replaceChildren();
      serversSignature = "";
    }, 360);
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

  function bindDrag(target, handles, scroller, onMove, onEnd, canStart = () => true) {
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

    // Pulling a list down from its top edge drags the whole sheet.
    let listStart = 0;
    let pulling = false;
    scroller.addEventListener("touchstart", event => {
      listStart = event.touches[0].clientY;
      pulling = scroller.scrollTop <= 0 && canStart(event);
      startAt = performance.now();
    }, { passive: true });
    scroller.addEventListener("touchmove", event => {
      if (!pulling) return;
      const distance = event.touches[0].clientY - listStart;
      if (distance <= 4) return;
      event.preventDefault();
      target.classList.add("dragging");
      onMove(distance);
    }, { passive: false });
    const finish = event => {
      if (!pulling) return;
      pulling = false;
      target.classList.remove("dragging");
      const distance = Math.max(0, ((event.changedTouches && event.changedTouches[0]?.clientY) || listStart) - listStart);
      onEnd(distance, distance / Math.max(1, performance.now() - startAt));
    };
    scroller.addEventListener("touchend", finish);
    scroller.addEventListener("touchcancel", () => { if (pulling) { pulling = false; target.classList.remove("dragging"); onEnd(0, 0); } });
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
      event => !event.target.closest?.("button"));
  }

  /* ---------- Panels ---------- */

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

  function resetPanelState() {
    panelBackAction = null;
    splitAppsReceiver = null;
    panelPager?.disconnect();
    panelPager = null;
    refreshBattery = null;
    if (appIconObserver) { appIconObserver.disconnect(); appIconObserver = null; }
  }

  function closePanel() {
    if (panelOverlay.hidden) return false;
    resetPanelState();
    panel.classList.remove("dragging");
    panel.classList.add("closing");
    panel.style.removeProperty("--panel-drag-y");
    panelOverlay.style.opacity = "0";
    clearTimeout(panelCloseTimer);
    panelCloseTimer = setTimeout(() => {
      panelOverlay.hidden = true;
      panel.classList.remove("closing");
      panelOverlay.style.opacity = "";
      panelBody.replaceChildren();
    }, 360);
    return true;
  }

  function bindPanelDrag() {
    bindDrag(panel, [panel.querySelector(".grabber"), panel.querySelector(".panel-header")], panelBody,
      distance => {
        panel.style.setProperty("--panel-drag-y", `${distance}px`);
        panelOverlay.style.opacity = String(Math.max(0, 1 - distance / (innerHeight * .55)));
      },
      (distance, velocity) => {
        if (distance > 100 || velocity > .65) closePanel();
        else { panel.style.removeProperty("--panel-drag-y"); panelOverlay.style.opacity = ""; }
      },
      event => !panelOverlay.hidden && !panel.classList.contains("closing")
        && !event.target.closest?.("button,input,select,textarea,label"));
  }

  // Resets the panel for a new page and returns its emptied body.
  function page(name, backAction = null) {
    resetPanelState();
    $("panelTitle").textContent = name;
    const back = $("panelBack");
    back.hidden = !backAction;
    back.onclick = event => {
      event.preventDefault();
      event.stopPropagation();
      if (backAction) backAction();
    };
    panelBackAction = backAction;
    panelBody.replaceChildren();
    panelBody.scrollTop = 0;
    return panelBody;
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

  function saveActions(collect) {
    return actions(button("Сохранить", "primary", () => {
      // Keep the local copy current so a second page saved before Android
      // answers does not send stale values back.
      snapshot.settings = { ...(snapshot.settings || {}), ...collect() };
      native.saveSettings(JSON.stringify(snapshot.settings));
      buildSettings();
    }));
  }

  function buildSettings() {
    const body = page("Настройки");
    body.append(
      group(null,
        navRow("servers", "Серверы", "Подписки и автовыбор", buildServerSettings),
        navRow("connection", "Соединение", "DNS, MTU, IPv6 и локальная сеть", buildConnectionSettings),
        navRow("routing", "Маршрутизация", "Что идёт через VPN, а что напрямую", buildRoutingSettings),
        navRow("split", "Раздельное туннелирование", "Какие приложения идут через VPN", buildSplitTunneling),
        navRow("application", "Приложение", "Фоновая работа и обновления", buildApplicationSettings)),
      group(null,
        navRow("logs", "Логи", null, () => buildLogs(true)),
        navRow("about", "О приложении", `Версия ${snapshot.appVersion || "—"}`, buildAbout)));
  }

  function buildServerSettings() {
    const body = page("Серверы", buildSettings);
    const s = snapshot.settings || {};
    const sourceCount = [snapshot.subscriptions?.wifi, snapshot.subscriptions?.lte].filter(Boolean).length;
    const pingOnOpen = switchControl(!!s.pingOnOpen);
    body.append(group("Подписки",
      row("Источники", `Настроено ${sourceCount} из 2`,
        button("Изменить", "secondary", () => buildSubscriptions(true), true)),
      row("Проверять задержку", "После загрузки списка серверов", pingOnOpen)));

    const servers = snapshot.servers.filter(server => !server.auto);
    const saved = new Set(s.autoProfiles || []);
    const chosen = new Set(servers.filter(server => !saved.size || saved.has(server.id)).map(server => server.id));
    body.append(el("div", "group-label", "Серверы для автовыбора"));
    const list = el("div", "list");
    body.append(list);
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
    body.append(saveActions(() => ({
      pingOnOpen: isOn(pingOnOpen),
      autoProfiles: chosen.size === servers.length ? [] : servers.filter(server => chosen.has(server.id)).map(server => server.id)
    })));
  }

  function buildConnectionSettings() {
    const body = page("Соединение", buildSettings);
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
    const lan = switchControl(s.lanDirect !== false);
    const mtu = selectControl([["1280","1280"],["1360","1360"],["1400","1400"],["1500","1500"]], s.tunMtu || "1400");
    const fragmentation = switchControl(!!s.fragmentation);
    body.append(
      group("DNS", dnsRow, customRow),
      el("p", "group-note", "Все DNS-запросы идут через VPN, провайдер их не видит."),
      group("Сеть",
        row("IPv6", "Включайте, если сервер поддерживает IPv6", ipv6),
        row("Локальная сеть напрямую", "Роутер, принтеры и трансляция на ТВ без VPN", lan),
        row("MTU туннеля", "1400 подходит для большинства сетей", mtu)),
      group("Обход блокировок",
        row("Фрагментация TLS", "Делит ClientHello, помогает против DPI", fragmentation)),
      group("Защита",
        row("Kill Switch", "Включите «Постоянная VPN» и «Блокировать без VPN»",
          button("Открыть", "secondary", () => native.openVpnSettings(), true))));
    const pingMethod = selectControl([["tcp","TCP"],["head","HTTP HEAD"],["get","HTTP GET"]], s.pingMethod);
    body.append(group("Проверка задержки", row("Метод", "HTTP точнее, TCP быстрее", pingMethod)));
    body.append(saveActions(() => ({
      dnsProvider: dns.value, customDns: customDns.value, fragmentation: isOn(fragmentation), tunMtu: mtu.value,
      ipv6Enabled: isOn(ipv6), lanDirect: isOn(lan), pingMethod: pingMethod.value
    })));
  }

  function buildRoutingSettings() {
    const body = page("Маршрутизация", buildSettings);
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
    body.append(
      group("Свои правила", modeRow, rulesRow),
      group("Подписка",
        row("Правила из подписки", "Маршруты, которые задал провайдер в Xray JSON", subscriptionRules)));

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
    body.append(
      group("Источники списков", geoIPRow, geoSiteRow),
      el("p", "group-note", "Списки скачиваются при первом подключении с такими правилами и обновляются раз в неделю. Оставьте поля пустыми, чтобы использовать источники по умолчанию."));
    body.append(saveActions(() => ({
      routingMode: mode.value, routingRules: rules.value, subscriptionRules: isOn(subscriptionRules),
      geoipUrl: geoIP.value.trim(), geositeUrl: geoSite.value.trim()
    })));
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
    const body = page("Раздельное туннелирование", buildSettings);
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
    body.append(el("div", "group-label", "Режим"), modes, el("div", "group-label", "Приложения"));

    const searchWrap = el("div", "search");
    const search = el("input", "field");
    search.type = "search";
    search.placeholder = "Поиск";
    searchWrap.append(search);
    const list = el("div", "list");
    list.append(el("div", "loading"));
    list.firstChild.append(el("i"), el("span", null, "Загружаем приложения…"));
    body.append(searchWrap, list);

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
    body.append(saveActions(() => ({
      appRoutingMode: modes.querySelector("input:checked")?.value || "all",
      appRoutingPackages: [...selected]
    })));
  }

  function buildApplicationSettings() {
    const body = page("Приложение", buildSettings);
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
    body.append(group("Фоновая работа", battery));
    const autoUpdate = selectControl([["15","Каждые 15 минут"],["60","Каждый час"],["360","Каждые 6 часов"],["1440","Раз в сутки"],["off","Выключено"]], s.autoUpdate);
    body.append(group("Подписка", row("Автообновление", "Не прерывает активное подключение", autoUpdate)));
    body.append(saveActions(() => ({ autoUpdate: autoUpdate.value })));
  }

  function buildAbout() {
    const body = page("О приложении", buildSettings);
    const head = el("div", "about-head");
    const logo = el("img");
    logo.src = "logo.png"; logo.alt = ""; logo.width = 56; logo.height = 56;
    head.append(logo, el("h3", null, "ShadowVPN"), el("p", null, `Версия ${snapshot.appVersion || "—"}`));
    body.append(head);

    const hwid = snapshot.settings?.hwid || "Недоступно";
    const hwidRow = row("HWID", hwid, button("Копировать", "secondary", () => native.copyText(snapshot.settings?.hwid || ""), true));
    hwidRow.querySelector("small").classList.add("mono");
    body.append(group("Устройство", hwidRow));
    body.append(group("Компоненты",
      valueRow("Ядро", "Xray Core"),
      valueRow("QUIC и Naive", "Cronet"),
      valueRow("TLS-отпечаток", "uTLS · Chrome 152"),
      valueRow("Протоколы", "VLESS, VMess, Trojan, Shadowsocks, Hysteria2, Naive"),
      valueRow("Платформа", "Android 10+ · arm64, x86_64")));
  }

  function buildSubscriptions(fromSettings = false) {
    const body = page("Подписки", fromSettings ? buildServerSettings : null);
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
    const body = page("Логи", fromSettings ? buildSettings : null);
    const log = el("pre", "log-box", native.getLogs() || "Событий пока нет");
    body.append(log, actions(button("Копировать", "secondary", () => native.copyText(log.textContent))));
  }

  function closeTopLayer() {
    if (!$("appMenu").hidden) { closeMenu(); return true; }
    if (!panelOverlay.hidden && panelBackAction) { panelBackAction(); return true; }
    if (closePanel()) return true;
    if (!$("sortMenu").hidden) { toggleSortMenu(false); return true; }
    if (sheetOpen) return closeServers();
    return false;
  }

  /* ---------- Wiring ---------- */

  $("powerButton").addEventListener("click", () => native.toggleVpn());
  $("menuButton").addEventListener("click", event => { event.stopPropagation(); setMenu($("appMenu").hidden); });
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
  const mockApps = Array.from({length: 180}, (_, i) => ({name:`Приложение ${i + 1}`,packageName:`com.example.app${i + 1}`}));
  native.getInstalledApps = () => JSON.stringify({apps: mockApps});
  render({
    state:"connected",serviceStatus:"Подключено",needsSubscription:false,importRunning:false,pingRunning:false,message:"",appVersion:"0.16.0",
    publicIp:"132.243.***",session:"00:02:18",traffic:{downloadSpeed:"25,7 КБ/с",downloadTotal:"6,1 МБ",uploadSpeed:"58,8 КБ/с",uploadTotal:"135 КБ"},
    selected:{...mockServers[1],subscription:{title:"ShadowVPN",used:12884901888,total:107374182400,expire:1893456000}},
    servers:mockServers,
    subscriptions:{wifi:"https://example.com/wifi",lte:"https://example.com/lte",manual:""},
    settings:{autoUpdate:"1440",pingOnOpen:true,dnsProvider:"subscription-doh",routingMode:"full",routingRules:"",pingMethod:"tcp",fragmentation:true,tunMtu:"1400",ipv6Enabled:false,appRoutingMode:"all",appRoutingPackages:[],doh:"dns.shadowvpn.io",lanDirect:true,customDns:"",subscriptionRules:true,geoipUrl:"",geositeUrl:"",autoProfiles:[],hwid:"5191f00c7aa812b926a9f785944ff53c18c7e846969125ba66dced29a123ff53",batteryUnrestricted:false}
  });
  if (params.has("disconnected")) render({ state:"disconnected", serviceStatus:"", publicIp:"85.95.178.108", session:"" });
  if (params.has("error")) render({ state:"error", serviceStatus:"Ошибка: не удалось подключиться к серверу", publicIp:"85.95.178.108" });
  const preview = params.get("preview");
  if (preview === "servers") openServers();
  if (preview === "menu") setMenu(true);
  if (preview === "welcome") render({ needsSubscription:true });
  if (["settings", "subscriptions", "logs"].includes(preview)) openPanel(preview);
  if (["connection", "routing", "split", "application", "about", "auto"].includes(preview)) {
    openPanel("settings");
    ({ connection:buildConnectionSettings, routing:buildRoutingSettings, split:buildSplitTunneling, application:buildApplicationSettings, about:buildAbout, auto:buildServerSettings })[preview]();
  }
})();

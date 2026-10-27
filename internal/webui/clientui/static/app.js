(() => {
  "use strict";

  const $ = (selector, root = document) => root.querySelector(selector);
  const $$ = (selector, root = document) => Array.from(root.querySelectorAll(selector));

  function fmtBytes(value) {
    let bytes = Number(value);
    if (!Number.isFinite(bytes) || bytes < 0) {
      bytes = 0;
    }
    if (bytes < 1024) {
      return `${Math.round(bytes)} B`;
    }
    const units = ["KiB", "MiB", "GiB", "TiB", "PiB"];
    let index = -1;
    while (bytes >= 1024 && index < units.length - 1) {
      bytes /= 1024;
      index += 1;
    }
    const digits = bytes >= 100 ? 0 : bytes >= 10 ? 1 : 2;
    return `${bytes.toFixed(digits)} ${units[index]}`;
  }

  function fmtDuration(value) {
    const total = Math.max(0, Math.floor(Number(value) || 0));
    const days = Math.floor(total / 86400);
    const hours = Math.floor((total % 86400) / 3600);
    const minutes = Math.floor((total % 3600) / 60);
    const seconds = total % 60;
    if (days > 0) {
      return `${days}天 ${hours}小时`;
    }
    if (hours > 0) {
      return `${hours}小时 ${minutes}分`;
    }
    if (minutes > 0) {
      return `${minutes}分 ${seconds}秒`;
    }
    return `${seconds}秒`;
  }

  function fmtTime(value) {
    if (!value) {
      return "—";
    }
    const date = new Date(value);
    if (Number.isNaN(date.getTime())) {
      return String(value);
    }
    try {
      return date.toLocaleString("zh-CN", { hour12: false });
    } catch (error) {
      return date.toISOString();
    }
  }

  function setTextAll(selector, value) {
    if (value === undefined || value === null) {
      return;
    }
    $$(selector).forEach((element) => {
      element.textContent = String(value);
    });
  }

  function showToast(message, kind = "ok") {
    const stack = document.getElementById("toast-stack");
    if (!stack) {
      return;
    }
    const toast = document.createElement("div");
    toast.className = `toast toast-${kind}`;
    toast.textContent = message;
    stack.appendChild(toast);
    window.setTimeout(() => toast.classList.add("is-out"), 3200);
    window.setTimeout(() => toast.remove(), 3800);
  }

  function setLive(state) {
    const indicator = $("[data-live]");
    if (!indicator) {
      return;
    }
    indicator.dataset.live = state;
    const text = $(".live-text", indicator);
    if (text) {
      text.textContent = state === "live" ? "实时" : state === "retry" ? "重连中" : "连接中";
    }
  }

  function statusLabel(status) {
    switch (status) {
      case "running":
        return "运行中";
      case "pending":
        return "待启动";
      case "failed":
        return "失败";
      case "disabled":
        return "已禁用";
      case "offline":
        return "离线";
      default:
        return "未知";
    }
  }

  function statusClass(status) {
    switch (status) {
      case "running":
        return "ok";
      case "pending":
        return "warn";
      case "failed":
        return "bad";
      default:
        return "muted";
    }
  }

  function element(tag, className, text) {
    const node = document.createElement(tag);
    if (className) {
      node.className = className;
    }
    if (text !== undefined && text !== null) {
      node.textContent = String(text);
    }
    return node;
  }

  let currentServer = "";

  function mappingRow(mapping) {
    const row = document.createElement("tr");
    row.dataset.mappingRow = "";
    row.dataset.mappingId = mapping.id || "";
    row.dataset.mappingType = mapping.type || "tcp";
    row.dataset.mappingPort = String(mapping.remote_port || 0);
    row.dataset.mappingLocal = mapping.local_addr || "";
    row.dataset.mappingNote = mapping.note || "";
    row.dataset.mappingEnabled = String(Boolean(mapping.enabled));
    row.dataset.mappingAssigned = String(mapping.assigned_port || 0);
    row.dataset.mappingStatus = mapping.status || "";

    const typeCell = document.createElement("td");
    typeCell.appendChild(element("span", `type-tag type-${mapping.type}`, String(mapping.type || "").toUpperCase()));
    row.appendChild(typeCell);

    const serverCell = element("td", "mono");
    serverCell.appendChild(element("span", "server-endpoint", currentServer || "—"));
    serverCell.appendChild(document.createElement("br"));
    const port = mapping.assigned_port || mapping.remote_port;
    serverCell.appendChild(element("span", "mapping-port", port ? `:${port}` : ":自动"));
    row.appendChild(serverCell);

    row.appendChild(element("td", "mono", mapping.local_addr || ""));

    const noteCell = element("td", "note");
    if (mapping.note) {
      noteCell.textContent = mapping.note;
    } else {
      noteCell.appendChild(element("span", "muted", "—"));
    }
    row.appendChild(noteCell);

    const stateCell = document.createElement("td");
    const state = element("span", `status status-${statusClass(mapping.status)}`);
    state.appendChild(element("span", "status-dot"));
    state.appendChild(document.createTextNode(statusLabel(mapping.status)));
    stateCell.appendChild(state);
    if (mapping.error) {
      stateCell.appendChild(element("span", "error-code mono", mapping.error));
    }
    row.appendChild(stateCell);

    const trafficCell = element("td", "mono nowrap");
    trafficCell.appendChild(document.createTextNode(`${fmtBytes(mapping.tx_bytes)} ↑`));
    trafficCell.appendChild(document.createElement("br"));
    trafficCell.appendChild(element("span", "muted", `${fmtBytes(mapping.rx_bytes)} ↓`));
    row.appendChild(trafficCell);

    const actionCell = element("td", "col-actions");
    const edit = element("button", "btn btn-quiet btn-xs", "编辑");
    edit.type = "button";
    edit.dataset.action = "edit-mapping";
    edit.dataset.mappingId = mapping.id || "";
    actionCell.appendChild(edit);
    row.appendChild(actionCell);

    return row;
  }

  function emptyMappingRow() {
    const row = document.createElement("tr");
    row.dataset.emptyRow = "";
    const cell = document.createElement("td");
    cell.colSpan = 7;
    cell.appendChild(element("div", "empty-inline", "还没有任何映射。点击“新增映射”创建第一条。"));
    row.appendChild(cell);
    return row;
  }

  function renderMappings(mappings) {
    const tbody = document.getElementById("mapping-rows");
    if (!tbody) {
      return;
    }
    const list = Array.isArray(mappings) ? mappings : [];
    const fragment = document.createDocumentFragment();
    if (list.length === 0) {
      fragment.appendChild(emptyMappingRow());
    } else {
      for (const mapping of list) {
        fragment.appendChild(mappingRow(mapping));
      }
    }
    tbody.replaceChildren(fragment);
    updateMappingStats(list);
  }

  function updateMappingStats(mappings) {
    const list = Array.isArray(mappings) ? mappings : [];
    const active = list.filter((mapping) => mapping.status === "running").length;
    setTextAll("[data-stat='active']", active);
    setTextAll("[data-stat='active-pill']", active);
    setTextAll("[data-stat='total']", list.length);
    const count = $("[data-mapping-count]");
    if (count) {
      count.textContent = `${list.length} 条映射`;
    }
  }

  function applyStatus(status) {
    if (!status) {
      return;
    }
    currentServer = status.server || currentServer;
    setTextAll("[data-stat='uptime']", fmtDuration(status.uptime_seconds));
    setTextAll("[data-stat='tx']", fmtBytes(status.tx_bytes));
    setTextAll("[data-stat='rx']", fmtBytes(status.rx_bytes));
    setTextAll("[data-stat='active']", status.active_mappings);
    setTextAll("[data-stat='active-pill']", status.active_mappings);
    setTextAll("[data-stat='total']", status.mappings);

    const activePill = $("[data-stat='active-pill']");
    if (activePill) {
      const pill = activePill.closest(".pill");
      const active = Number(status.active_mappings) || 0;
      if (pill) {
        pill.classList.toggle("pill-ok", active > 0);
        pill.classList.toggle("pill-muted", active === 0);
        const dot = $(".dot", pill);
        if (dot) {
          dot.classList.toggle("dot-ok", active > 0);
        }
      }
    }

    const pill = $("[data-connection]");
    if (pill) {
      pill.dataset.connected = String(Boolean(status.connected));
      pill.classList.toggle("is-online", Boolean(status.connected));
      pill.classList.toggle("is-offline", !status.connected);
      const dot = $(".status-dot", pill);
      if (dot) {
        dot.classList.toggle("is-online", Boolean(status.connected));
        dot.classList.toggle("is-offline", !status.connected);
      }
      const text = $("[data-connection-text]", pill);
      if (text) {
        text.textContent = status.connected ? "已连接" : "未连接";
      }
    }
  }

  function applySnapshot(data) {
    if (!data) {
      return;
    }
    applyStatus(data.status);
    renderMappings(data.mappings);
  }

  function initSSE() {
    const indicator = $("[data-live]");
    if (!window.EventSource) {
      if (indicator) {
        indicator.hidden = true;
      }
      return;
    }
    const source = new EventSource("/api/events");
    source.addEventListener("open", () => setLive("live"));
    source.addEventListener("error", () => {
      setLive("retry");
      if (source.readyState === EventSource.CLOSED) {
        window.location.href = "/login";
      }
    });
    source.addEventListener("snapshot", (event) => {
      setLive("live");
      try {
        applySnapshot(JSON.parse(event.data));
      } catch (error) {
        // Ignore a malformed frame; the next tick refreshes the view.
      }
    });
  }

  async function fetchMappings() {
    try {
      const response = await fetch("/api/mappings", {
        headers: { Accept: "application/json" },
        credentials: "same-origin",
      });
      if (response.status === 401) {
        window.location.href = "/login";
        return;
      }
      if (!response.ok) {
        throw new Error(`HTTP ${response.status}`);
      }
      renderMappings(await response.json());
    } catch (error) {
      showToast(`刷新映射失败：${error.message}`, "bad");
    }
  }

  function initTabs() {
    const buttons = $$("[data-tab]");
    const panels = $$("[data-panel]");
    if (buttons.length === 0) {
      return;
    }
    const valid = new Set(buttons.map((button) => button.dataset.tab));

    const activate = (name) => {
      const target = valid.has(name) ? name : "mappings";
      for (const button of buttons) {
        const active = button.dataset.tab === target;
        button.classList.toggle("is-active", active);
        button.setAttribute("aria-selected", active ? "true" : "false");
      }
      for (const panel of panels) {
        panel.hidden = panel.dataset.panel !== target;
      }
      if (target === "logs") {
        refreshLogs();
        startLogPolling();
      } else {
        stopLogPolling();
      }
      if (window.history && window.history.replaceState) {
        window.history.replaceState(null, "", `#${target}`);
      }
    };

    for (const button of buttons) {
      button.addEventListener("click", () => activate(button.dataset.tab));
    }

    const initial = window.location.hash ? window.location.hash.slice(1) : "mappings";
    activate(initial);
  }

  let logTimer = null;
  let logLoading = false;

  function logRow(entry) {
    const level = entry.Level || entry.level || "INFO";
    const message = entry.Message || entry.message || "";
    const time = entry.Time || entry.time;
    const row = element("div", "log-row");
    row.dataset.level = String(level).toUpperCase();
    row.appendChild(element("span", "log-time", fmtTime(time)));
    row.appendChild(element("span", "log-level", String(level).toUpperCase()));
    row.appendChild(element("span", "log-msg", message));
    return row;
  }

  function renderLogs(entries) {
    const list = document.getElementById("log-list");
    if (!list) {
      return;
    }
    const items = Array.isArray(entries) ? entries : [];
    const fragment = document.createDocumentFragment();
    for (const entry of items) {
      fragment.appendChild(logRow(entry));
    }
    list.replaceChildren(fragment);
    const empty = document.getElementById("log-empty");
    if (empty) {
      empty.hidden = items.length > 0;
    }
  }

  async function refreshLogs() {
    if (logLoading) {
      return;
    }
    const select = document.getElementById("log-level");
    if (!select) {
      return;
    }
    logLoading = true;
    try {
      const level = select.value;
      const response = await fetch(`/api/logs?level=${encodeURIComponent(level)}&limit=200`, {
        headers: { Accept: "application/json" },
        credentials: "same-origin",
      });
      if (response.status === 401) {
        window.location.href = "/login";
        return;
      }
      if (!response.ok) {
        throw new Error(`HTTP ${response.status}`);
      }
      renderLogs(await response.json());
    } catch (error) {
      showToast(`加载日志失败：${error.message}`, "bad");
    } finally {
      logLoading = false;
    }
  }

  function startLogPolling() {
    stopLogPolling();
    logTimer = window.setInterval(refreshLogs, 3000);
  }

  function stopLogPolling() {
    if (logTimer !== null) {
      window.clearInterval(logTimer);
      logTimer = null;
    }
  }

  function initLogs() {
    const refresh = document.getElementById("log-refresh");
    if (refresh) {
      refresh.addEventListener("click", refreshLogs);
    }
    const select = document.getElementById("log-level");
    if (select) {
      select.addEventListener("change", refreshLogs);
    }
  }

  function drawerError(message) {
    const box = document.getElementById("drawer-error");
    if (!box) {
      return;
    }
    box.textContent = message;
    box.hidden = false;
    box.scrollIntoView({ block: "nearest" });
  }

  function openDrawer(options) {
    const drawer = document.getElementById("mapping-drawer");
    const form = document.getElementById("mapping-form");
    if (!drawer || !form) {
      return;
    }
    form.reset();
    const errorBox = document.getElementById("drawer-error");
    if (errorBox) {
      errorBox.hidden = true;
      errorBox.textContent = "";
    }
    const editing = Boolean(options.mappingId);
    document.getElementById("mapping-id").value = options.mappingId || "";
    document.getElementById("mapping-type").value = options.type || "tcp";
    document.getElementById("mapping-port").value = String(options.port || 0);
    document.getElementById("mapping-local").value = options.local || "";
    document.getElementById("mapping-note").value = options.note || "";
    document.getElementById("mapping-enabled").checked = options.enabled !== false;
    document.getElementById("mapping-server").value = currentServer || document.getElementById("mapping-server").value;
    document.getElementById("drawer-title").textContent = editing ? "编辑映射" : "新增映射";
    document.getElementById("drawer-sub").textContent = editing
      ? options.mappingId
      : "配置公网端口到本机内网目标的转发";
    document.getElementById("mapping-delete").hidden = !editing;
    drawer.showModal();
    window.setTimeout(() => document.getElementById("mapping-local").focus(), 0);
  }

  function closeDrawer() {
    const drawer = document.getElementById("mapping-drawer");
    if (drawer && drawer.open) {
      drawer.close();
    }
  }

  function initDrawer() {
    const drawer = document.getElementById("mapping-drawer");
    const form = document.getElementById("mapping-form");
    if (!drawer || !form) {
      return;
    }

    document.addEventListener("click", (event) => {
      const addButton = event.target.closest('[data-action="add-mapping"]');
      if (addButton) {
        event.preventDefault();
        openDrawer({ mode: "add", type: "tcp", port: 0, local: "", note: "", enabled: true });
        return;
      }
      const editButton = event.target.closest('[data-action="edit-mapping"]');
      if (editButton) {
        event.preventDefault();
        const row = editButton.closest("tr[data-mapping-row]");
        if (!row) {
          return;
        }
        openDrawer({
          mode: "edit",
          mappingId: row.dataset.mappingId || "",
          type: row.dataset.mappingType || "tcp",
          port: Number(row.dataset.mappingPort || 0),
          local: row.dataset.mappingLocal || "",
          note: row.dataset.mappingNote || "",
          enabled: row.dataset.mappingEnabled === "true",
        });
        return;
      }
      if (event.target.closest("[data-close-drawer]")) {
        closeDrawer();
      }
    });

    drawer.addEventListener("click", (event) => {
      if (event.target === drawer) {
        closeDrawer();
      }
    });

    form.addEventListener("submit", async (event) => {
      event.preventDefault();
      const id = document.getElementById("mapping-id").value.trim();
      const mapping = {
        type: document.getElementById("mapping-type").value,
        remote_port: Number(document.getElementById("mapping-port").value || 0),
        local_addr: document.getElementById("mapping-local").value.trim(),
        note: document.getElementById("mapping-note").value.trim(),
        enabled: document.getElementById("mapping-enabled").checked,
      };
      if (id) {
        mapping.id = id;
      }
      if (!mapping.local_addr) {
        drawerError("请填写内网目标。");
        document.getElementById("mapping-local").focus();
        return;
      }
      if (!Number.isInteger(mapping.remote_port) || mapping.remote_port < 0 || mapping.remote_port > 65535) {
        drawerError("服务端端口必须在 0 到 65535 之间。");
        document.getElementById("mapping-port").focus();
        return;
      }
      if (Array.from(mapping.note).length > 128) {
        drawerError("备注不能超过 128 个字符。");
        document.getElementById("mapping-note").focus();
        return;
      }

      const save = document.getElementById("mapping-save");
      save.disabled = true;
      save.textContent = "保存中…";
      try {
        const response = await fetch(id ? `/api/mappings/${encodeURIComponent(id)}` : "/api/mappings", {
          method: id ? "PUT" : "POST",
          headers: {
            "Content-Type": "application/json",
            "X-Requested-With": "hy2tun",
          },
          credentials: "same-origin",
          body: JSON.stringify(mapping),
        });
        let payload = null;
        try {
          payload = await response.json();
        } catch (error) {
          payload = null;
        }
        if (response.status === 401) {
          window.location.href = "/login";
          return;
        }
        if (!response.ok) {
          drawerError((payload && payload.error) || `保存失败（HTTP ${response.status}）`);
          return;
        }
        closeDrawer();
        showToast(id ? "映射已更新" : "映射已创建");
        await fetchMappings();
      } catch (error) {
        drawerError(`请求失败：${error.message}`);
      } finally {
        save.disabled = false;
        save.textContent = "保存";
      }
    });

    document.getElementById("mapping-delete").addEventListener("click", async () => {
      const id = document.getElementById("mapping-id").value.trim();
      if (!id) {
        return;
      }
      if (!window.confirm("确定删除这条映射？已建立的公网连接会被关闭。")) {
        return;
      }
      const button = document.getElementById("mapping-delete");
      button.disabled = true;
      try {
        const response = await fetch(`/api/mappings/${encodeURIComponent(id)}`, {
          method: "DELETE",
          headers: { "X-Requested-With": "hy2tun" },
          credentials: "same-origin",
        });
        if (response.status === 401) {
          window.location.href = "/login";
          return;
        }
        if (!response.ok) {
          let payload = null;
          try {
            payload = await response.json();
          } catch (error) {
            payload = null;
          }
          drawerError((payload && payload.error) || `删除失败（HTTP ${response.status}）`);
          return;
        }
        closeDrawer();
        showToast("映射已删除");
        await fetchMappings();
      } catch (error) {
        drawerError(`请求失败：${error.message}`);
      } finally {
        button.disabled = false;
      }
    });
  }

  function initLogout() {
    const button = $("[data-logout]");
    if (!button) {
      return;
    }
    button.addEventListener("click", async () => {
      button.disabled = true;
      try {
        const response = await fetch("/api/logout", {
          method: "POST",
          headers: { "X-Requested-With": "hy2tun" },
          credentials: "same-origin",
        });
        if (!response.ok) {
          showToast("退出失败，请重试", "bad");
          button.disabled = false;
          return;
        }
      } catch (error) {
        showToast("退出失败，请检查网络", "bad");
        button.disabled = false;
        return;
      }
      window.location.href = "/login";
    });
  }

  function init() {
    initSSE();
    initTabs();
    initLogs();
    initDrawer();
    initLogout();
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", init, { once: true });
  } else {
    init();
  }
})();

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
    applyCredentialStatus(status);
  }

  function setSyncBadge(synced) {
    const badge = $("[data-sync-badge]");
    if (!badge) {
      return;
    }
    badge.classList.toggle("badge-ok", synced);
    badge.classList.toggle("badge-muted", !synced);
    badge.textContent = synced ? "已同步" : "待同步";
  }

  function applyCredentialStatus(status) {
    if (!status) {
      return;
    }
    const generation = Number(status.credential_generation) || 0;
    if (generation > 0) {
      setTextAll("[data-settings-generation]", generation);
    }
    const serverGeneration = Number(status.server_credential_generation) || 0;
    setTextAll("[data-server-generation]", serverGeneration > 0 ? String(serverGeneration) : "未知");
    setSyncBadge(Boolean(status.credential_synced));
  }

  function updateSecretMasks(credentials) {
    const masks = {
      "current-auth": credentials.current_auth_masked,
      "current-token": credentials.current_control_token_masked,
    };
    for (const [key, value] of Object.entries(masks)) {
      const element = document.querySelector(`[data-secret="${key}"]`);
      if (!element) {
        continue;
      }
      element.dataset.mask = value || "未设置";
      if (element.dataset.revealed !== "true") {
        element.textContent = element.dataset.mask;
      }
    }
  }

  function applySettings(settings) {
    if (!settings) {
      return;
    }
    const credentials = settings.credentials || {};
    const generation = Number(credentials.generation) || 0;
    setTextAll("[data-settings-generation]", generation > 0 ? String(generation) : "—");
    const serverGeneration = Number(credentials.server_generation) || 0;
    setTextAll("[data-server-generation]", serverGeneration > 0 ? String(serverGeneration) : "未知");
    setSyncBadge(Boolean(credentials.synced));
    updateSecretMasks(credentials);

    if (settings.name) {
      const heading = $("[data-client-name]");
      if (heading) {
        heading.textContent = settings.name;
      }
      const input = document.getElementById("client-name");
      if (input && document.activeElement !== input) {
        input.value = settings.name;
      }
    }
    if (settings.web_username) {
      const input = document.getElementById("web-username");
      if (input && document.activeElement !== input) {
        input.value = settings.web_username;
      }
    }
  }

  async function settingsRequest(path, method, body) {
    const response = await fetch(path, {
      method,
      headers: {
        "Content-Type": "application/json",
        "X-Requested-With": "hy2tun",
      },
      credentials: "same-origin",
      body: JSON.stringify(body || {}),
    });
    if (response.status === 401) {
      window.location.href = "/login";
      throw new Error("会话已失效，请重新登录");
    }
    let payload = null;
    try {
      payload = await response.json();
    } catch (error) {
      payload = null;
    }
    if (!response.ok) {
      const message = (payload && payload.error) || `请求失败（HTTP ${response.status}）`;
      throw new Error(message);
    }
    return payload;
  }

  async function refreshSettings() {
    try {
      const response = await fetch("/api/settings", {
        headers: { Accept: "application/json" },
        credentials: "same-origin",
      });
      if (response.status === 401) {
        window.location.href = "/login";
        return null;
      }
      if (!response.ok) {
        return null;
      }
      const settings = await response.json();
      applySettings(settings);
      return settings;
    } catch (error) {
      return null;
    }
  }

  let credentialPollTimer = null;

  function stopCredentialPolling() {
    if (credentialPollTimer !== null) {
      window.clearTimeout(credentialPollTimer);
      credentialPollTimer = null;
    }
  }

  function pollCredentialStatus(expected) {
    stopCredentialPolling();
    let attempts = 0;
    const tick = async () => {
      attempts += 1;
      try {
        const response = await fetch("/api/status", {
          headers: { Accept: "application/json" },
          credentials: "same-origin",
        });
        if (response.status === 401) {
          window.location.href = "/login";
          return;
        }
        if (response.ok) {
          const status = await response.json();
          applyCredentialStatus(status);
          if (Number(status.credential_generation) === expected && status.credential_synced) {
            showToast(`凭据已同步到代际 ${expected}`);
            return;
          }
        }
      } catch (error) {
        // Keep polling; the reconnect can briefly interrupt requests.
      }
      if (attempts >= 20) {
        showToast("凭据已应用，等待服务端确认同步状态");
        return;
      }
      credentialPollTimer = window.setTimeout(tick, 1000);
    };
    tick();
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
      if (target === "settings") {
        refreshSettings();
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

  function initSettings() {
    if (!$("#settings-web-form")) {
      return;
    }

    const secretFields = $$("[data-secret]");
    secretFields.forEach((element) => {
      element.dataset.mask = element.textContent;
    });
    let revealTimer = 0;
    const hideButton = $("[data-hide-secrets]");

    const maskSecrets = () => {
      secretFields.forEach((element) => {
        element.textContent = element.dataset.mask || "未设置";
        delete element.dataset.revealed;
      });
      if (hideButton) {
        hideButton.hidden = true;
      }
      if (revealTimer) {
        window.clearTimeout(revealTimer);
        revealTimer = 0;
      }
    };

    const showSecrets = (data) => {
      const current = (data && data.current) || {};
      const values = {
        "current-auth": current.auth,
        "current-token": current.control_token,
      };
      for (const [key, value] of Object.entries(values)) {
        const element = document.querySelector(`[data-secret="${key}"]`);
        if (!element) {
          continue;
        }
        if (value) {
          element.dataset.revealed = "true";
          element.textContent = value;
        } else {
          delete element.dataset.revealed;
          element.textContent = "未设置";
        }
      }
      if (hideButton) {
        hideButton.hidden = false;
      }
      if (revealTimer) {
        window.clearTimeout(revealTimer);
      }
      revealTimer = window.setTimeout(maskSecrets, 30000);
    };

    document.addEventListener("click", async (event) => {
      const revealButton = event.target.closest("[data-reveal-credentials]");
      if (revealButton) {
        const passwordInput = $("#credentials-reveal-password");
        const password = passwordInput ? passwordInput.value : "";
        if (!password) {
          showToast("请输入当前 Web 密码", "bad");
          if (passwordInput) {
            passwordInput.focus();
          }
          return;
        }
        revealButton.disabled = true;
        try {
          const data = await settingsRequest("/api/settings/credentials/reveal", "POST", { password });
          showSecrets(data);
          showToast("已显示凭据，30 秒后自动隐藏");
        } catch (error) {
          showToast(error.message, "bad");
        } finally {
          revealButton.disabled = false;
          if (passwordInput) {
            passwordInput.value = "";
          }
        }
        return;
      }

      if (event.target.closest("[data-hide-secrets]")) {
        maskSecrets();
      }
    });

    const webForm = $("#settings-web-form");
    webForm.addEventListener("submit", async (event) => {
      event.preventDefault();
      const username = $("#web-username").value.trim();
      const currentPassword = $("#web-current-password").value;
      const newPassword = $("#web-new-password").value;
      const confirmPassword = $("#web-confirm-password").value;
      if (!username) {
        showToast("用户名不能为空", "bad");
        return;
      }
      if (Array.from(newPassword).length < 8) {
        showToast("新密码至少需要 8 个字符", "bad");
        return;
      }
      if (newPassword !== confirmPassword) {
        showToast("两次输入的新密码不一致", "bad");
        return;
      }
      if (!window.confirm("保存后会退出所有 Web 会话，需要使用新用户名和密码重新登录。继续？")) {
        return;
      }
      const button = $("button[type='submit']", webForm);
      if (button) {
        button.disabled = true;
      }
      try {
        await settingsRequest("/api/settings/web", "PUT", {
          username,
          current_password: currentPassword,
          new_password: newPassword,
        });
        showToast("Web 管理员已更新，请重新登录");
        window.location.href = "/login";
      } catch (error) {
        showToast(error.message, "bad");
        if (button) {
          button.disabled = false;
        }
      }
    });

    const nameForm = $("#settings-name-form");
    nameForm.addEventListener("submit", async (event) => {
      event.preventDefault();
      const name = $("#client-name").value.trim();
      const passwordInput = $("#name-current-password");
      const currentPassword = passwordInput.value;
      if (!name) {
        showToast("客户端名称不能为空", "bad");
        return;
      }
      const button = $("button[type='submit']", nameForm);
      if (button) {
        button.disabled = true;
      }
      try {
        const settings = await settingsRequest("/api/settings/name", "PUT", {
          name,
          current_password: currentPassword,
        });
        applySettings(settings);
        passwordInput.value = "";
        showToast("客户端名称已更新");
      } catch (error) {
        showToast(error.message, "bad");
      } finally {
        if (button) {
          button.disabled = false;
        }
      }
    });

    const credentialsForm = $("#settings-credentials-form");
    credentialsForm.addEventListener("submit", async (event) => {
      event.preventDefault();
      const generation = Number($("#credentials-generation").value);
      const auth = $("#credentials-auth").value;
      const token = $("#credentials-token").value;
      const passwordInput = $("#credentials-current-password");
      const currentPassword = passwordInput.value;
      const localGeneration = Number(($("[data-settings-generation]") || {}).textContent || 0);
      const serverText = (($("[data-server-generation]") || {}).textContent || "").trim();
      const serverGeneration = Number(serverText) || 0;

      if (!Number.isInteger(generation) || generation < 1) {
        showToast("新代际必须是正整数", "bad");
        return;
      }
      if (generation <= localGeneration) {
        showToast(`新代际必须大于本地代际 ${localGeneration}`, "bad");
        return;
      }
      if (!auth) {
        showToast("请填写新的 HY2 密码", "bad");
        return;
      }
      if (!token) {
        showToast("请填写新的 control token", "bad");
        return;
      }
      let warning = "保存后客户端会立即写入配置并使用新凭据重连，当前连接会短暂中断。继续？";
      if (serverGeneration > 0 && generation !== serverGeneration) {
        warning = `新代际 ${generation} 与服务端代际 ${serverGeneration} 不一致。仍会保存，但可能显示待同步。继续？`;
      }
      if (!window.confirm(warning)) {
        return;
      }

      const button = $("button[type='submit']", credentialsForm);
      if (button) {
        button.disabled = true;
        button.textContent = "保存中…";
      }
      try {
        await settingsRequest("/api/settings/credentials", "PUT", {
          generation,
          auth,
          control_token: token,
          current_password: currentPassword,
        });
        $("#credentials-auth").value = "";
        $("#credentials-token").value = "";
        passwordInput.value = "";
        maskSecrets();
        showToast(`凭据已保存到代际 ${generation}，客户端正在重连`);
        await refreshSettings();
        pollCredentialStatus(generation);
      } catch (error) {
        showToast(error.message, "bad");
      } finally {
        if (button) {
          button.disabled = false;
          button.textContent = "保存并重连";
        }
      }
    });
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
    initSettings();
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

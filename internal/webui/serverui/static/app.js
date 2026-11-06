(() => {
  "use strict";

  const $ = (selector, root = document) => root.querySelector(selector);
  const $$ = (selector, root = document) => Array.from(root.querySelectorAll(selector));

  function escapeSelector(value) {
    const text = String(value);
    if (window.CSS && typeof window.CSS.escape === "function") {
      return window.CSS.escape(text);
    }
    return text.replace(/["\\]/g, "\\$&");
  }

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

  function setTextAll(selector, value) {
    if (value === undefined || value === null) {
      return;
    }
    $$(selector).forEach((element) => {
      element.textContent = String(value);
    });
  }

  function setTextIn(root, selector, value) {
    const element = $(selector, root);
    if (element && value !== undefined && value !== null) {
      element.textContent = String(value);
    }
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

  function exposureValue(value) {
    return value === "public" || value === "local" ? value : "";
  }

  function exposureLabel(value) {
    switch (exposureValue(value)) {
      case "public":
        return "公网";
      case "local":
        return "仅本机";
      default:
        return "—";
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

  function familyLabel(family) {
    const key = String(family || "").trim().toLowerCase();
    if (key === "ipv4") {
      return "IPv4";
    }
    if (key === "ipv6") {
      return "IPv6";
    }
    return String(family || "").trim();
  }

  function unavailableFamiliesLabel(families) {
    if (!Array.isArray(families)) {
      return "";
    }
    let hasIPv4 = false;
    let hasIPv6 = false;
    const others = [];
    for (const family of families) {
      const label = familyLabel(family);
      if (label === "IPv4") {
        hasIPv4 = true;
      } else if (label === "IPv6") {
        hasIPv6 = true;
      } else if (label && !others.includes(label)) {
        others.push(label);
      }
    }
    const labels = [];
    if (hasIPv4) {
      labels.push("IPv4");
    }
    if (hasIPv6) {
      labels.push("IPv6");
    }
    labels.push(...others);
    return labels.length === 0 ? "" : `${labels.join("/")} 不可用`;
  }

  function mappingValue(mapping, pascal, snake) {
    if (mapping && mapping[pascal] !== undefined) {
      return mapping[pascal];
    }
    if (mapping && mapping[snake] !== undefined) {
      return mapping[snake];
    }
    return undefined;
  }

  function applyExposureToRow(row, exposure, families) {
    if (!row) {
      return;
    }
    const value = exposureValue(exposure);
    const label = exposureLabel(value);
    const button = $("[data-action='toggle-exposure']", row);
    if (button) {
      button.dataset.exposure = value;
      button.textContent = label;
      button.setAttribute("aria-label", `切换映射 ${button.dataset.mappingId || ""} 的暴露范围，当前${label}`);
      button.title = `点击切换为${value === "local" ? "公网" : "仅本机"}`;
    }
    row.dataset.exposure = value;
    const warning = $("[data-exposure-warning]", row);
    if (warning) {
      const text = unavailableFamiliesLabel(families);
      warning.textContent = text;
      warning.hidden = !text;
    }
  }

  // applyMappingToRow refreshes every visible part of a row from the raw
  // view.MappingView returned by the server (PascalCase runtime fields plus
  // the embedded snake_case mapping fields).
  function applyMappingToRow(row, mapping) {
    if (!row || !mapping) {
      return;
    }
    applyExposureToRow(
      row,
      mappingValue(mapping, "Exposure", "exposure"),
      mappingValue(mapping, "UnavailableFamilies", "unavailable_families")
    );

    const status = String(mappingValue(mapping, "Status", "status") || "");
    if (status) {
      row.dataset.status = status;
      row.dataset.mappingStatus = status;
      const statusElement = $("[data-mapping-status-label]", row);
      if (statusElement) {
        statusElement.className = `status status-${statusClass(status)}`;
        statusElement.replaceChildren();
        const dot = document.createElement("span");
        dot.className = "status-dot";
        dot.setAttribute("aria-hidden", "true");
        statusElement.appendChild(dot);
        statusElement.appendChild(document.createTextNode(statusLabel(status)));
      }
    }

    const error = String(mappingValue(mapping, "Error", "error") || "");
    row.dataset.mappingError = error;
    const errorElement = $("[data-mapping-error]", row);
    if (errorElement) {
      errorElement.textContent = error;
      errorElement.hidden = !error;
    }

    const assigned = Number(mappingValue(mapping, "AssignedPort", "assigned_port") || 0);
    const remote = Number(mappingValue(mapping, "RemotePort", "remote_port") || 0);
    const port = assigned || remote;
    row.dataset.mappingAssigned = String(assigned);
    row.dataset.port = String(port);
    const portElement = $("[data-mapping-port-cell]", row);
    if (portElement) {
      if (port) {
        portElement.textContent = String(port);
      } else {
        portElement.replaceChildren();
        const automatic = document.createElement("span");
        automatic.className = "muted";
        automatic.textContent = "自动";
        portElement.appendChild(automatic);
      }
    }

    const tx = mappingValue(mapping, "TxBytes", "tx_bytes");
    const rx = mappingValue(mapping, "RxBytes", "rx_bytes");
    const txElement = $("[data-mapping-tx]", row);
    if (txElement && tx !== undefined) {
      txElement.textContent = fmtBytes(tx);
    }
    const rxElement = $("[data-mapping-rx]", row);
    if (rxElement && rx !== undefined) {
      rxElement.textContent = fmtBytes(rx);
    }
  }

  function findClientMapping(clients, clientID, mappingID) {
    if (!Array.isArray(clients)) {
      return null;
    }
    for (const client of clients) {
      const id = client.ID !== undefined ? client.ID : client.id;
      if (id !== clientID) {
        continue;
      }
      const mappings = client.Mappings !== undefined ? client.Mappings : client.mappings;
      if (!Array.isArray(mappings)) {
        return null;
      }
      for (const mapping of mappings) {
        const idValue = mapping.ID !== undefined ? mapping.ID : mapping.id;
        if (idValue === mappingID) {
          return mapping;
        }
      }
      return null;
    }
    return null;
  }

  async function refreshMappingRow(clientID, mappingID, row) {
    try {
      const response = await fetch("/api/clients", {
        headers: { Accept: "application/json" },
        credentials: "same-origin",
      });
      if (response.status === 401) {
        window.location.href = "/login";
        return true;
      }
      if (!response.ok) {
        return false;
      }
      const mapping = findClientMapping(await response.json(), clientID, mappingID);
      if (!mapping) {
        return false;
      }
      applyMappingToRow(row, mapping);
      return true;
    } catch (error) {
      return false;
    }
  }

  async function refreshAfterExposureFailure(clientID, mappingID, row) {
    const detail = currentDetail();
    if (detail && detail.dataset.clientId === clientID) {
      const refreshed = await refreshDetail(clientID, true);
      if (!refreshed) {
        window.location.reload();
      }
      return;
    }
    const refreshed = await refreshMappingRow(clientID, mappingID, row);
    if (!refreshed) {
      window.location.reload();
    }
  }

  async function toggleExposure(button) {
    const row = button.closest("tr");
    if (!row) {
      return;
    }
    const clientID = button.dataset.clientId || row.dataset.client || "";
    const mappingID = button.dataset.mappingId || row.dataset.mappingId || "";
    if (!clientID || !mappingID) {
      showToast("缺少客户端或映射标识，请刷新后重试", "bad");
      return;
    }
    const current = exposureValue(button.dataset.exposure);
    // Unknown legacy/corrupt state switches to the fail-closed local mode.
    const next = current === "local" ? "public" : "local";
    const clientName = row.dataset.clientName || clientID;
    const confirmText = button.dataset.confirm || "切换会立即关闭现有连接，确定继续？";
    if (!window.confirm(`将映射 ${clientName} 的暴露范围从 ${exposureLabel(current)} 切换为 ${exposureLabel(next)}。${confirmText}`)) {
      return;
    }

    button.disabled = true;
    try {
      const response = await fetch(
        `/api/clients/${encodeURIComponent(clientID)}/mappings/${encodeURIComponent(mappingID)}/exposure`,
        {
          method: "PUT",
          headers: {
            "Content-Type": "application/json",
            "X-Requested-With": "hy2tun",
          },
          credentials: "same-origin",
          body: JSON.stringify({ exposure: next }),
        }
      );
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
      if (response.ok) {
        // The success body is the raw view.MappingView, so status, error,
        // assigned port, exposure, and family warning are all refreshed from
        // the actual server state.
        applyMappingToRow(row, payload);
        const exposure = mappingValue(payload, "Exposure", "exposure");
        const families = mappingValue(payload, "UnavailableFamilies", "unavailable_families");
        const warning = unavailableFamiliesLabel(families);
        showToast(warning
          ? `暴露范围已切换为${exposureLabel(exposure)}；${warning}`
          : `暴露范围已切换为${exposureLabel(exposure)}`);
        return;
      }
      if (response.status === 409 && payload && payload.code === "exposure_conflict") {
        // Conflict responses are snake_case and describe the state that
        // remains active after the failed switch. The row status/error is
        // re-read because a closed rollback can change it.
        applyExposureToRow(row, payload.active_exposure, payload.unavailable_families);
        let message = payload.error || (payload.closed ? "暴露范围切换失败，映射已关闭" : "暴露范围切换失败，已保留当前状态");
        if (payload.closed && !message.includes("关闭")) {
          message += "；映射已关闭";
        }
        const warning = unavailableFamiliesLabel(payload.unavailable_families);
        if (warning) {
          message += `；${warning}`;
        }
        showToast(message, "bad");
        await refreshAfterExposureFailure(clientID, mappingID, row);
        return;
      }
      showToast((payload && payload.error) || `切换暴露范围失败（HTTP ${response.status}）`, "bad");
      if (response.status === 404 || response.status === 409 || response.status >= 500) {
        // The response is not a structured conflict, so the actual row state
        // is uncertain (for example a 500 after a failed rollback) or the
        // mapping may no longer exist.
        await refreshAfterExposureFailure(clientID, mappingID, row);
      }
    } catch (error) {
      showToast(`切换暴露范围失败：${error.message}`, "bad");
      await refreshAfterExposureFailure(clientID, mappingID, row);
    } finally {
      button.disabled = false;
    }
  }

  function initExposure() {
    document.addEventListener("click", (event) => {
      const button = event.target.closest('[data-action="toggle-exposure"]');
      if (!button) {
        return;
      }
      event.preventDefault();
      toggleExposure(button);
    });
  }

  function currentDetail() {
    return $(".client-detail");
  }

  function isDrawerOpen() {
    const drawer = document.getElementById("mapping-drawer");
    return Boolean(drawer && drawer.open);
  }

  let refreshing = false;
  async function refreshDetail(clientId, silent) {
    if (!clientId || refreshing) {
      return false;
    }
    const target = document.getElementById("client-detail");
    if (!target) {
      return false;
    }
    refreshing = true;
    try {
      const response = await fetch(`/clients/${encodeURIComponent(clientId)}/detail`, {
        headers: { Accept: "text/html", "HX-Request": "true" },
        credentials: "same-origin",
      });
      if (response.status === 401) {
        window.location.href = "/login";
        return true;
      }
      if (!response.ok) {
        throw new Error(`HTTP ${response.status}`);
      }
      target.innerHTML = await response.text();
      if (!silent) {
        showToast("已刷新");
      }
      return true;
    } catch (error) {
      if (!silent) {
        showToast(`刷新失败：${error.message}`, "bad");
      }
      return false;
    } finally {
      refreshing = false;
    }
  }

  function updateClientRow(client) {
    const row = document.querySelector(`[data-client-row="${escapeSelector(client.id)}"]`);
    if (!row) {
      return;
    }
    row.dataset.online = String(Boolean(client.online));
    const dot = $(".status-dot", row);
    if (dot) {
      dot.classList.toggle("is-online", Boolean(client.online));
      dot.classList.toggle("is-offline", !client.online);
    }
    const badge = $(".badge", row);
    if (badge) {
      badge.textContent = client.online ? "在线" : "离线";
      badge.classList.toggle("badge-ok", Boolean(client.online));
      badge.classList.toggle("badge-muted", !client.online);
      badge.classList.remove("badge-bad");
    }
    const count = $("[data-mapping-count]", row);
    if (count) {
      count.textContent = `${Number(client.mappings) || 0} 映射`;
    }
  }

  function updateDetailStats(client) {
    const detail = currentDetail();
    if (!detail || detail.dataset.clientId !== client.id) {
      return;
    }
    const wasOnline = detail.dataset.online === "true";
    const revision = Number(detail.dataset.revision || 0);
    const needsRefresh =
      Number(client.revision) !== revision || Boolean(client.online) !== wasOnline;
    if (needsRefresh) {
      if (!isDrawerOpen()) {
        refreshDetail(client.id, true);
        return;
      }
      // Keep data-online/data-revision stale while the drawer is open so the
      // next tick refreshes the fragment once editing finishes.
    } else {
      detail.dataset.online = String(Boolean(client.online));
    }
    setTextIn(detail, '[data-client-stat="active"]', client.active_mappings);
    setTextIn(detail, '[data-client-stat="tx"]', fmtBytes(client.tx_bytes));
    setTextIn(detail, '[data-client-stat="rx"]', fmtBytes(client.rx_bytes));
  }

  function applySnapshot(data) {
    const overview = data.overview || {};
    setTextAll('[data-stat="online-clients"]', overview.online_clients);
    setTextAll('[data-stat="active-mappings"]', overview.active_mappings);
    setTextAll('[data-stat="tx-bytes"]', fmtBytes(overview.tx_bytes));
    setTextAll('[data-stat="rx-bytes"]', fmtBytes(overview.rx_bytes));
    setTextAll('[data-stat="uptime"]', fmtDuration(overview.uptime_seconds));

    const clients = Array.isArray(data.clients) ? data.clients : [];
    setTextAll('[data-stat="total-clients"]', clients.length);
    for (const client of clients) {
      updateClientRow(client);
      updateDetailStats(client);
    }
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
        // Ignore a malformed frame; the next tick will refresh the view.
      }
    });
  }

  function initFilters() {
    const table = document.getElementById("mappings-table");
    if (!table) {
      return;
    }
    const rows = $$("[data-mapping-row]", table);
    const count = document.getElementById("mapping-result-count");
    const empty = document.getElementById("mapping-filter-empty");
    const filters = { search: "", type: "", status: "", port: "" };

    const apply = () => {
      let shown = 0;
      for (const row of rows) {
        const haystack = (row.dataset.search || "").toLowerCase();
        const matches =
          (!filters.search || haystack.includes(filters.search)) &&
          (!filters.type || row.dataset.type === filters.type) &&
          (!filters.status || row.dataset.status === filters.status) &&
          (!filters.port || (row.dataset.port || "").includes(filters.port));
        row.hidden = !matches;
        if (matches) {
          shown += 1;
        }
      }
      if (count) {
        count.textContent = `${shown} 条映射`;
      }
      if (empty) {
        empty.hidden = shown !== 0 || rows.length === 0;
      }
    };

    $$("[data-filter]").forEach((input) => {
      const key = input.dataset.filter;
      if (!(key in filters)) {
        return;
      }
      const handler = () => {
        filters[key] = input.value.trim().toLowerCase();
        apply();
      };
      input.addEventListener("input", handler);
      input.addEventListener("change", handler);
    });
    apply();
  }

  function initCopy() {
    document.addEventListener("click", async (event) => {
      const button = event.target.closest("[data-copy]");
      if (!button) {
        return;
      }
      const container = button.closest(".copyable");
      const textElement = container && container.querySelector("[data-copy-text]");
      const text = textElement ? textElement.textContent.trim() : "";
      if (!text || text === "—") {
        return;
      }
      try {
        if (navigator.clipboard && window.isSecureContext) {
          await navigator.clipboard.writeText(text);
        } else {
          const area = document.createElement("textarea");
          area.value = text;
          area.className = "copy-source";
          area.setAttribute("readonly", "");
          document.body.appendChild(area);
          area.select();
          const copied = document.execCommand("copy");
          area.remove();
          if (!copied) {
            throw new Error("copy command was rejected");
          }
        }
        showToast("已复制");
      } catch (error) {
        showToast("复制失败", "bad");
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

  function newMappingId() {
    const bytes = new Uint8Array(6);
    if (window.crypto && typeof window.crypto.getRandomValues === "function") {
      window.crypto.getRandomValues(bytes);
    } else {
      for (let index = 0; index < bytes.length; index += 1) {
        bytes[index] = Math.floor(Math.random() * 256);
      }
    }
    return "m-" + Array.from(bytes, (value) => value.toString(16).padStart(2, "0")).join("");
  }

  function pushErrorMessage(status, payload) {
    if (payload && Array.isArray(payload.results)) {
      const details = payload.results
        .filter((result) => result && result.error)
        .map((result) => `${result.id || "映射"}: ${result.error}`)
        .join("；");
      if (details) {
        return `${payload.error || "保存失败"}：${details}`;
      }
    }
    if (payload && payload.code) {
      switch (payload.code) {
        case "conflict":
          return "客户端配置已变化，请刷新后重试。";
        case "client_offline":
          return "客户端离线，无法修改配置。";
        case "client_not_ready":
          return "客户端正在连接，请稍后重试。";
        case "push_timeout":
          return "客户端未在超时时间内确认，请稍后重试。";
        default:
          break;
      }
    }
    if (payload && payload.error) {
      return payload.error;
    }
    return `保存失败（HTTP ${status}）`;
  }

  function initDrawer() {
    const drawer = document.getElementById("mapping-drawer");
    const form = document.getElementById("mapping-form");
    if (!drawer || !form) {
      return;
    }

    const title = document.getElementById("drawer-title");
    const subtitle = document.getElementById("drawer-sub");
    const errorBox = document.getElementById("drawer-error");
    const idInput = document.getElementById("mapping-id");
    const typeInput = document.getElementById("mapping-type");
    const portInput = document.getElementById("mapping-port");
    const localInput = document.getElementById("mapping-local");
    const noteInput = document.getElementById("mapping-note");
    const enabledInput = document.getElementById("mapping-enabled");
    const assigned = document.getElementById("mapping-assigned");
    const deleteButton = document.getElementById("mapping-delete");
    const saveButton = document.getElementById("mapping-save");

    const closeDrawer = () => {
      if (drawer.open) {
        drawer.close();
      }
    };

    const showDrawerError = (message) => {
      if (!errorBox) {
        return;
      }
      errorBox.textContent = message;
      errorBox.hidden = false;
      errorBox.scrollIntoView({ block: "nearest" });
    };

    const openDrawer = (options) => {
      form.reset();
      if (errorBox) {
        errorBox.hidden = true;
        errorBox.textContent = "";
      }
      drawer.dataset.clientId = options.clientId;
      drawer.dataset.mode = options.mode;
      drawer.dataset.mappingId = options.mappingId || "";
      idInput.value = options.mappingId || "";
      title.textContent = options.mode === "edit" ? "编辑映射" : "新增映射";
      subtitle.textContent = options.clientId;
      typeInput.value = options.type || "tcp";
      portInput.value = String(options.port || 0);
      localInput.value = options.local || "";
      noteInput.value = options.note || "";
      enabledInput.checked = options.enabled !== false;
      assigned.textContent = options.assigned ? String(options.assigned) : "—";
      deleteButton.hidden = options.mode !== "edit";
      drawer.showModal();
      window.setTimeout(() => localInput.focus(), 0);
    };

    const collectMappings = () => {
      const detail = currentDetail();
      if (!detail) {
        return [];
      }
      return $$("tr[data-mapping-id]", detail).map((row) => ({
        id: row.dataset.mappingId,
        type: row.dataset.mappingType,
        remote_port: Number(row.dataset.mappingPort || 0),
        local_addr: row.dataset.mappingLocal || "",
        note: row.dataset.mappingNote || "",
        enabled: row.dataset.mappingEnabled === "true",
      }));
    };

    let saving = false;
    const pushMappings = async (clientId, baseRevision, mappings) => {
      if (saving) {
        return false;
      }
      saving = true;
      if (saveButton) {
        saveButton.disabled = true;
        saveButton.textContent = "保存中…";
      }
      try {
        const response = await fetch(`/api/clients/${encodeURIComponent(clientId)}/mappings`, {
          method: "PUT",
          headers: {
            "Content-Type": "application/json",
            "X-Requested-With": "hy2tun",
          },
          credentials: "same-origin",
          body: JSON.stringify({ base_revision: baseRevision, mappings }),
        });
        let payload = null;
        try {
          payload = await response.json();
        } catch (error) {
          payload = null;
        }
        if (response.status === 401) {
          window.location.href = "/login";
          return false;
        }
        if (!response.ok) {
          showDrawerError(pushErrorMessage(response.status, payload));
          return false;
        }
        closeDrawer();
        showToast("映射已更新");
        await refreshDetail(clientId, true);
        return true;
      } catch (error) {
        showDrawerError(`请求失败：${error.message}`);
        return false;
      } finally {
        saving = false;
        if (saveButton) {
          saveButton.disabled = false;
          saveButton.textContent = "保存";
        }
      }
    };

    document.addEventListener("click", async (event) => {
      const deleteClientButton = event.target.closest('[data-action="delete-client"]');
      if (deleteClientButton) {
        event.preventDefault();
        const clientId = deleteClientButton.dataset.clientId || "";
        const clientName = deleteClientButton.dataset.clientName || clientId;
        if (!clientId || !window.confirm(`删除客户端 ${clientName}？它的映射和端口保留会一并删除，客户端重新连接前不会恢复。`)) {
          return;
        }
        deleteClientButton.disabled = true;
        try {
          const response = await fetch(`/api/clients/${encodeURIComponent(clientId)}`, {
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
            showToast((payload && payload.error) || `删除失败（HTTP ${response.status}）`, "bad");
            deleteClientButton.disabled = false;
            return;
          }
          window.location.href = "/clients";
        } catch (error) {
          showToast("删除失败，请检查网络", "bad");
          deleteClientButton.disabled = false;
        }
        return;
      }

      const addButton = event.target.closest('[data-action="add-mapping"]');
      if (addButton) {
        event.preventDefault();
        const detail = currentDetail();
        if (!detail) {
          return;
        }
        openDrawer({
          mode: "add",
          clientId: detail.dataset.clientId,
          type: "tcp",
          port: 0,
          local: "",
          note: "",
          enabled: true,
          assigned: 0,
        });
        return;
      }

      const editButton = event.target.closest('[data-action="edit-mapping"]');
      if (editButton) {
        event.preventDefault();
        const detail = currentDetail();
        const row = editButton.closest("tr[data-mapping-id]");
        if (!detail || !row) {
          return;
        }
        openDrawer({
          mode: "edit",
          clientId: editButton.dataset.clientId || detail.dataset.clientId,
          mappingId: row.dataset.mappingId,
          type: row.dataset.mappingType || "tcp",
          port: Number(row.dataset.mappingPort || 0),
          local: row.dataset.mappingLocal || "",
          note: row.dataset.mappingNote || "",
          enabled: row.dataset.mappingEnabled === "true",
          assigned: Number(row.dataset.mappingAssigned || 0),
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
      const detail = currentDetail();
      if (!detail) {
        return;
      }
      const mappingId = idInput.value.trim();
      const mapping = {
        id: mappingId || newMappingId(),
        type: typeInput.value,
        remote_port: Number(portInput.value || 0),
        local_addr: localInput.value.trim(),
        note: noteInput.value.trim(),
        enabled: enabledInput.checked,
      };
      if (!mapping.local_addr) {
        showDrawerError("请填写内网目标。");
        localInput.focus();
        return;
      }
      if (!Number.isInteger(mapping.remote_port) || mapping.remote_port < 0 || mapping.remote_port > 65535) {
        showDrawerError("服务端端口必须在 0 到 65535 之间。");
        portInput.focus();
        return;
      }
      if (Array.from(mapping.note).length > 128) {
        showDrawerError("备注不能超过 128 个字符。");
        noteInput.focus();
        return;
      }

      const mappings = collectMappings();
      if (mappingId) {
        const index = mappings.findIndex((item) => item.id === mappingId);
        if (index >= 0) {
          mappings[index] = mapping;
        } else {
          mappings.push(mapping);
        }
      } else {
        mappings.push(mapping);
      }
      await pushMappings(detail.dataset.clientId, Number(detail.dataset.revision || 0), mappings);
    });

    deleteButton.addEventListener("click", async () => {
      const detail = currentDetail();
      const mappingId = idInput.value.trim();
      if (!detail || !mappingId) {
        return;
      }
      if (!window.confirm("确定删除这条映射？已建立的公网连接会被关闭。")) {
        return;
      }
      const mappings = collectMappings().filter((item) => item.id !== mappingId);
      await pushMappings(detail.dataset.clientId, Number(detail.dataset.revision || 0), mappings);
    });
  }

  function initHtmxFallback() {
    document.addEventListener("click", (event) => {
      const button = event.target.closest("[data-refresh-detail]");
      if (!button || window.htmx) {
        return;
      }
      event.preventDefault();
      refreshDetail(button.dataset.refreshDetail || "", false);
    });
  }

  function parsePortList(value) {
    return String(value || "")
      .split(/[\s,]+/)
      .map((item) => item.trim())
      .filter(Boolean);
  }

  function fmtDateTime(value) {
    if (!value) {
      return "—";
    }
    const date = new Date(value);
    if (Number.isNaN(date.getTime()) || date.getFullYear() <= 1) {
      return "—";
    }
    return date.toLocaleString("zh-CN", { hour12: false });
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

  function credentialBadge(status) {
    switch (status) {
      case "synced":
        return ["badge-ok", "已同步"];
      case "pending":
        return ["badge-muted", "待同步"];
      case "unsupported":
        return ["badge-bad", "不支持推送"];
      case "timeout":
        return ["badge-bad", "超时"];
      default:
        return ["badge-muted", "未知"];
    }
  }

  function renderCredentialClients(clients) {
    const body = $("[data-credential-clients]");
    if (!body) {
      return;
    }
    body.textContent = "";
    if (!Array.isArray(clients) || clients.length === 0) {
      const row = document.createElement("tr");
      const cell = document.createElement("td");
      cell.colSpan = 5;
      cell.className = "empty-inline";
      cell.textContent = "暂无客户端";
      row.appendChild(cell);
      body.appendChild(row);
      return;
    }
    for (const client of clients) {
      const row = document.createElement("tr");
      row.dataset.credentialClient = client.client_id || "";

      const nameCell = document.createElement("td");
      const name = document.createElement("div");
      name.textContent = client.name || client.client_id || "—";
      const id = document.createElement("div");
      id.className = "note mono";
      id.textContent = client.client_id || "";
      nameCell.appendChild(name);
      nameCell.appendChild(id);
      row.appendChild(nameCell);

      const onlineCell = document.createElement("td");
      const onlineBadge = document.createElement("span");
      onlineBadge.className = `badge ${client.online ? "badge-ok" : "badge-muted"}`;
      onlineBadge.textContent = client.online ? "在线" : "离线";
      onlineCell.appendChild(onlineBadge);
      row.appendChild(onlineCell);

      const generationCell = document.createElement("td");
      generationCell.className = "mono";
      generationCell.dataset.credentialGeneration = client.client_id || "";
      generationCell.textContent = client.generation ? String(client.generation) : "—";
      row.appendChild(generationCell);

      const statusCell = document.createElement("td");
      const [badgeClass, badgeText] = credentialBadge(client.status);
      const statusBadge = document.createElement("span");
      statusBadge.className = `badge ${badgeClass}`;
      statusBadge.textContent = badgeText;
      statusCell.appendChild(statusBadge);
      if (client.error) {
        const error = document.createElement("span");
        error.className = "note mono";
        error.textContent = client.error;
        statusCell.appendChild(error);
      }
      row.appendChild(statusCell);

      const lastSeenCell = document.createElement("td");
      lastSeenCell.className = "mono";
      lastSeenCell.textContent = fmtDateTime(client.last_seen);
      row.appendChild(lastSeenCell);

      body.appendChild(row);
    }
  }

  function updateSecretMasks(credentials) {
    const masks = {
      "current-auth": credentials.current_auth_masked,
      "current-token": credentials.current_control_token_masked,
      "previous-auth": credentials.previous_auth_masked,
      "previous-token": credentials.previous_control_token_masked,
    };
    for (const [key, value] of Object.entries(masks)) {
      const element = document.querySelector(`[data-secret="${key}"]`);
      if (!element) {
        continue;
      }
      element.dataset.mask = value || "—";
      if (element.dataset.revealed !== "true") {
        element.textContent = element.dataset.mask;
      }
    }
  }

  function applyTransportMode(settings) {
    if (!settings) {
      return;
    }
    const mode = settings.mode || "hy2";
    setTextAll("[data-transport-mode]", mode);
    const select = document.getElementById("transport-mode");
    if (select && document.activeElement !== select) {
      select.value = mode;
    }
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
      const credentials = settings.credentials || {};
      setTextAll("[data-settings-generation]", credentials.generation);
      setTextAll(
        "[data-settings-previous-generation]",
        credentials.previous_generation ? String(credentials.previous_generation) : "—"
      );
      const secondRotationWarning = $("[data-second-rotation-warning]");
      if (secondRotationWarning) {
        secondRotationWarning.hidden = !(Number(credentials.previous_generation) > 0);
      }
      const secondRotationGeneration = $("[data-second-rotation-generation]");
      if (secondRotationGeneration) {
        secondRotationGeneration.textContent = credentials.previous_generation
          ? String(credentials.previous_generation)
          : "";
      }
      updateSecretMasks(credentials);
      renderCredentialClients(settings.clients);
      const count = $("[data-credential-client-count]");
      if (count) {
        count.textContent = `${Array.isArray(settings.clients) ? settings.clients.length : 0} 个客户端`;
      }
      const revokeButton = $("[data-revoke-credentials]");
      if (revokeButton) {
        revokeButton.disabled = !(Number(credentials.previous_generation) > 0);
      }
      applyTransportMode(settings);
      return settings;
    } catch (error) {
      return null;
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
        element.textContent = element.dataset.mask || "********";
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
      const previous = (data && data.previous) || {};
      const values = {
        "current-auth": current.auth,
        "current-token": current.control_token,
        "previous-auth": previous.auth,
        "previous-token": previous.control_token,
      };
      for (const [key, value] of Object.entries(values)) {
        const element = document.querySelector(`[data-secret="${key}"]`);
        if (element) {
          if (value) {
            element.dataset.revealed = "true";
            element.textContent = value;
          } else {
            delete element.dataset.revealed;
            element.textContent = "—";
          }
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
        return;
      }

      const revokeButton = event.target.closest("[data-revoke-credentials]");
      if (revokeButton) {
        const passwordInput = $("#credentials-revoke-password");
        const password = passwordInput ? passwordInput.value : "";
        if (!password) {
          showToast("请输入当前 Web 密码", "bad");
          if (passwordInput) {
            passwordInput.focus();
          }
          return;
        }
        if (!window.confirm("撤销后上一代密码和 control token 立即失效，仍在使用旧代际的客户端会失去连接。确定撤销？")) {
          return;
        }
        revokeButton.disabled = true;
        try {
          await settingsRequest("/api/settings/credentials/revoke", "POST", { current_password: password });
          showToast("旧凭据已撤销");
          if (passwordInput) {
            passwordInput.value = "";
          }
          maskSecrets();
          await refreshSettings();
        } catch (error) {
          showToast(error.message, "bad");
        } finally {
          revokeButton.disabled = false;
        }
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
      if (newPassword.length < 8) {
        showToast("新密码至少需要 8 个字符", "bad");
        return;
      }
      if (newPassword !== confirmPassword) {
        showToast("两次输入的新密码不一致", "bad");
        return;
      }
      if (!window.confirm("保存后会退出所有 Web 会话，需要使用新凭据重新登录。继续？")) {
        return;
      }
      try {
        await settingsRequest("/api/settings/web", "PUT", {
          username,
          current_password: currentPassword,
          new_password: newPassword,
        });
        window.location.href = "/login";
      } catch (error) {
        showToast(error.message, "bad");
      }
    });

    const modeForm = $("#settings-mode-form");
    if (modeForm) {
      modeForm.addEventListener("submit", async (event) => {
        event.preventDefault();
        const select = $("#transport-mode");
        const passwordInput = $("#mode-current-password");
        const mode = select ? select.value : "";
        if (!mode) {
          showToast("请选择传输模式", "bad");
          return;
        }
        if (!passwordInput || !passwordInput.value) {
          showToast("请输入当前 Web 密码", "bad");
          if (passwordInput) {
            passwordInput.focus();
          }
          return;
        }
        const button = $("button[type='submit']", modeForm);
        if (button) {
          button.disabled = true;
        }
        try {
          const settings = await settingsRequest("/api/settings/mode", "PUT", {
            mode,
            current_password: passwordInput.value,
          });
          applyTransportMode(settings);
          passwordInput.value = "";
          showToast("传输模式已更新");
          await refreshSettings();
        } catch (error) {
          showToast(error.message, "bad");
        } finally {
          if (button) {
            button.disabled = false;
          }
        }
      });
    }

    const proxyForm = $("#settings-proxy-form");
    proxyForm.addEventListener("submit", async (event) => {
      event.preventDefault();
      const passwordInput = $("#proxy-current-password");
      try {
        await settingsRequest("/api/settings/proxy", "PUT", {
          current_password: passwordInput.value,
          trust_proxy: $("#trust-proxy").checked,
        });
        passwordInput.value = "";
        showToast("代理信任设置已更新");
      } catch (error) {
        showToast(error.message, "bad");
      }
    });

    const credentialsForm = $("#settings-credentials-form");
    credentialsForm.addEventListener("submit", async (event) => {
      event.preventDefault();
      const auth = $("#credentials-auth").value;
      const token = $("#credentials-token").value;
      const currentPassword = $("#credentials-current-password").value;
      const syncMode = $("#credentials-sync").value;
      if (!auth && !token) {
        showToast("请填写新的 HY2 密码或 control token", "bad");
        return;
      }
      if (auth && auth.length < 8) {
        showToast("HY2 密码至少需要 8 个字符", "bad");
        return;
      }
      if (token && token.length < 16) {
        showToast("control token 至少需要 16 个字符", "bad");
        return;
      }
      const previous = $("[data-settings-previous-generation]");
      const hasPrevious = previous && previous.textContent.trim() !== "" && previous.textContent.trim() !== "—";
      const warning = hasPrevious
        ? "当前存在上一代凭据。继续轮换会替换上一代：只有紧邻的上一代保持有效，仍在使用更早代际的客户端会立即断开。自动推送会等待在线客户端确认，未确认的客户端保持待同步。继续轮换？"
        : "确认轮换 HY2 凭据？旧凭据会保留为上一代，直到显式撤销或被下一次轮换替换。";
      if (!window.confirm(warning)) {
        return;
      }
      try {
        const result = await settingsRequest("/api/settings/credentials", "PUT", {
          current_password: currentPassword,
          auth,
          control_token: token,
          sync_mode: syncMode,
        });
        $("#credentials-auth").value = "";
        $("#credentials-token").value = "";
        $("#credentials-current-password").value = "";
        maskSecrets();
        showToast(`凭据已轮换到代际 ${result.generation}`);
        await refreshSettings();
      } catch (error) {
        showToast(error.message, "bad");
      }
    });

    const portsForm = $("#settings-ports-form");
    portsForm.addEventListener("submit", async (event) => {
      event.preventDefault();
      const reserve = Number($("#ports-offline-reserve").value || 0);
      if (!Number.isFinite(reserve) || reserve < 0) {
        showToast("离线预留时间必须是非负数", "bad");
        return;
      }
      if (!window.confirm("保存端口策略？已运行的映射不会被关闭，但新注册和更新会立即使用新策略。")) {
        return;
      }
      try {
        await settingsRequest("/api/settings/ports", "PUT", {
          allow: parsePortList($("#ports-allow").value),
          deny: parsePortList($("#ports-deny").value),
          auto_range: $("#ports-auto-range").value.trim(),
          offline_reserve_seconds: Math.floor(reserve),
        });
        showToast("端口策略已更新");
        await refreshSettings();
      } catch (error) {
        showToast(error.message, "bad");
      }
    });

    refreshSettings();
    window.setInterval(refreshSettings, 5000);
  }

  function init() {
    initSSE();
    initFilters();
    initCopy();
    initLogout();
    initDrawer();
    initExposure();
    initHtmxFallback();
    initSettings();
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", init, { once: true });
  } else {
    init();
  }
})();

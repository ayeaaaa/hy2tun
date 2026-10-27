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
      return;
    }
    const target = document.getElementById("client-detail");
    if (!target) {
      return;
    }
    refreshing = true;
    try {
      const response = await fetch(`/clients/${encodeURIComponent(clientId)}/detail`, {
        headers: { Accept: "text/html", "HX-Request": "true" },
        credentials: "same-origin",
      });
      if (response.status === 401) {
        window.location.href = "/login";
        return;
      }
      if (!response.ok) {
        throw new Error(`HTTP ${response.status}`);
      }
      target.innerHTML = await response.text();
      if (!silent) {
        showToast("已刷新");
      }
    } catch (error) {
      if (!silent) {
        showToast(`刷新失败：${error.message}`, "bad");
      }
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

    document.addEventListener("click", (event) => {
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

  function init() {
    initSSE();
    initFilters();
    initCopy();
    initLogout();
    initDrawer();
    initHtmxFallback();
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", init, { once: true });
  } else {
    init();
  }
})();

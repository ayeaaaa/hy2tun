(() => {
  "use strict";

  const body = document.body;
  const basePath = body.dataset.basePath || "/";

  const meta = (name) => {
    const element = document.querySelector(`meta[name="${name}"]`);
    return element ? element.content : "";
  };

  // The manager's own anti-CSRF header comes from the rendered page. It is a
  // static header value, not a per-session secret token.
  const csrfHeaders = () => {
    const header = meta("csrf-header") || "X-Requested-With";
    const value = meta("csrf-value") || "hy2tun";
    return { [header]: value };
  };

  const showNotice = (message, kind) => {
    const notice = document.getElementById("notice");
    if (!notice) {
      return;
    }
    notice.textContent = message;
    notice.dataset.kind = kind || "info";
    notice.hidden = false;
    window.clearTimeout(showNotice.timer);
    showNotice.timer = window.setTimeout(() => {
      notice.hidden = true;
    }, 6000);
  };

  const responseError = async (response) => {
    try {
      const data = await response.json();
      if (data && typeof data.error === "string" && data.error) {
        return data.error;
      }
    } catch (_error) {
      // Fall through to the generic message.
    }
    return "请求失败，请稍后再试。";
  };

  const handleUnauthorized = (response) => {
    if (response.status === 401) {
      window.location.assign(`${basePath}login`);
      return true;
    }
    return false;
  };

  const formPayload = (form) => {
    const payload = {};
    new FormData(form).forEach((value, key) => {
      payload[key] = String(value);
    });
    form.querySelectorAll('input[type="checkbox"][name]').forEach((input) => {
      payload[input.name] = input.checked;
    });
    return payload;
  };

  const sendJSON = (url, method, payload) =>
    fetch(url, {
      method,
      credentials: "same-origin",
      headers: {
        ...csrfHeaders(),
        Accept: "application/json",
        "Content-Type": "application/json",
      },
      body: JSON.stringify(payload),
    });

  const loginForm = document.querySelector("[data-login-form]");
  if (loginForm) {
    loginForm.addEventListener("submit", async (event) => {
      event.preventDefault();
      const errorBox = document.getElementById("login-error");
      const button = loginForm.querySelector("button[type='submit']");
      if (errorBox) {
        errorBox.hidden = true;
      }
      if (button) {
        button.disabled = true;
      }
      try {
        const response = await fetch(loginForm.action, {
          method: "POST",
          credentials: "same-origin",
          headers: {
            ...csrfHeaders(),
            Accept: "application/json",
          },
          body: new URLSearchParams(new FormData(loginForm)),
        });
        if (response.status === 204) {
          window.location.assign(`${basePath}clients`);
          return;
        }
        if (errorBox) {
          errorBox.textContent = await responseError(response);
          errorBox.hidden = false;
        }
      } catch (_error) {
        if (errorBox) {
          errorBox.textContent = "无法连接到管理器，请检查网络。";
          errorBox.hidden = false;
        }
      } finally {
        if (button) {
          button.disabled = false;
        }
      }
    });
  }

  const logout = document.querySelector("[data-logout]");
  if (logout) {
    logout.addEventListener("click", async () => {
      logout.disabled = true;
      try {
        const response = await fetch(`${basePath}api/logout`, {
          method: "POST",
          credentials: "same-origin",
          headers: {
            ...csrfHeaders(),
            Accept: "application/json",
          },
        });
        if (!response.ok) {
          showNotice(await responseError(response), "error");
          logout.disabled = false;
          return;
        }
        window.location.assign(`${basePath}login`);
      } catch (_error) {
        showNotice("无法连接到管理器，请检查网络。", "error");
        logout.disabled = false;
      }
    });
  }

  // Server add/edit forms are progressive enhancement: without JavaScript the
  // form does not submit a mutation (mutations need the CSRF header anyway).
  document.querySelectorAll("[data-json-form]").forEach((form) => {
    form.addEventListener("submit", async (event) => {
      event.preventDefault();
      if (!form.reportValidity()) {
        return;
      }
      const button = form.querySelector("button[type='submit']");
      if (button) {
        button.disabled = true;
      }
      try {
        const response = await sendJSON(
          form.action,
          form.dataset.method || "POST",
          formPayload(form),
        );
        if (handleUnauthorized(response)) {
          return;
        }
        if (!response.ok) {
          showNotice(await responseError(response), "error");
          return;
        }
        window.location.assign(form.dataset.success || `${basePath}servers`);
      } catch (_error) {
        showNotice("无法连接到管理器，请检查网络。", "error");
      } finally {
        if (button) {
          button.disabled = false;
        }
      }
    });
  });

  document.querySelectorAll("[data-test-connection]").forEach((button) => {
    button.addEventListener("click", async () => {
      const form = button.closest("form");
      if (!form || !form.reportValidity()) {
        return;
      }
      button.disabled = true;
      try {
        const response = await sendJSON(
          form.dataset.testUrl,
          "POST",
          formPayload(form),
        );
        if (handleUnauthorized(response)) {
          return;
        }
        if (!response.ok) {
          showNotice(await responseError(response), "error");
          return;
        }
        const data = await response.json();
        const version = data.version ? `版本 ${data.version}` : "已登录";
        const clients =
          typeof data.client_count === "number"
            ? `，${data.client_count} 个在线客户端`
            : "";
        showNotice(`连接成功：${version}${clients}。`, "info");
      } catch (_error) {
        showNotice("无法连接到管理器，请检查网络。", "error");
      } finally {
        button.disabled = false;
      }
    });
  });

  document.querySelectorAll("[data-action]").forEach((button) => {
    button.addEventListener("click", async () => {
      const confirmation = button.dataset.confirm;
      if (confirmation && !window.confirm(confirmation)) {
        return;
      }
      button.disabled = true;
      try {
        const response = await fetch(button.dataset.action, {
          method: button.dataset.method || "POST",
          credentials: "same-origin",
          headers: {
            ...csrfHeaders(),
            Accept: "application/json",
          },
        });
        if (handleUnauthorized(response)) {
          return;
        }
        if (!response.ok) {
          showNotice(await responseError(response), "error");
          return;
        }
        if (button.dataset.notice) {
          showNotice(button.dataset.notice, "info");
        }
        if (button.dataset.success) {
          window.location.assign(button.dataset.success);
          return;
        }
        window.location.reload();
      } catch (_error) {
        showNotice("无法连接到管理器，请检查网络。", "error");
      } finally {
        button.disabled = false;
      }
    });
  });

  // Mapping editor. Every mutation sends the complete list plus the revision
  // rendered with the current page; a conflict is reported and the operator
  // reloads, never auto-retried against a newer revision.
  const mappingTable = document.querySelector("[data-mappings-table]");
  const mutationError = document.getElementById("client-mutation-error");
  const mutationErrorText = mutationError
    ? mutationError.querySelector("[data-mutation-error-text]")
    : null;
  const mutationDetails = mutationError
    ? mutationError.querySelector("[data-mutation-details]")
    : null;

  const responseData = async (response) => {
    try {
      return await response.json();
    } catch (_error) {
      return null;
    }
  };

  const setDetailLine = (selector, text) => {
    const element = mutationError ? mutationError.querySelector(selector) : null;
    if (!element) {
      return;
    }
    element.textContent = text || "";
    element.hidden = !text;
  };

  const renderMutationDetails = (data) => {
    if (!mutationDetails || !data) {
      return;
    }
    let hasDetails = false;
    if (typeof data.revision === "number" && data.revision > 0) {
      setDetailLine("[data-mutation-revision]", `服务端最新配置版本：r${data.revision}`);
      hasDetails = true;
    } else {
      setDetailLine("[data-mutation-revision]", "");
    }
    if (data.partial) {
      setDetailLine("[data-mutation-partial]", "服务端只应用了部分映射，请按逐映射结果处理后重新加载。");
      hasDetails = true;
    } else {
      setDetailLine("[data-mutation-partial]", "");
    }

    const resultsSection = mutationError.querySelector("[data-mutation-results]");
    const resultsBody = mutationError.querySelector("[data-mutation-results-body]");
    if (resultsSection && resultsBody) {
      resultsBody.replaceChildren();
      if (Array.isArray(data.results) && data.results.length > 0) {
        data.results.forEach((result) => {
          const row = document.createElement("tr");
          const cells = [
            result.id || "—",
            result.ok ? "成功" : "失败",
            result.assigned_port > 0 ? String(result.assigned_port) : "—",
            result.error || "—",
          ];
          cells.forEach((value, index) => {
            const cell = document.createElement("td");
            cell.textContent = value;
            if (index === 0 || index === 3) {
              cell.className = "mono wrap";
            }
            if (index === 3 && result.error) {
              cell.className = "error-text mono wrap";
            }
            row.appendChild(cell);
          });
          resultsBody.appendChild(row);
        });
        resultsSection.hidden = false;
        hasDetails = true;
      } else {
        resultsSection.hidden = true;
      }
    }

    const exposure = data.active_exposure === "local" ? "仅本机" : data.active_exposure === "public" ? "公网" : "";
    setDetailLine("[data-mutation-exposure]", exposure ? `当前生效暴露范围：${exposure}` : "");
    if (exposure) {
      hasDetails = true;
    }
    const families = Array.isArray(data.unavailable_families) ? data.unavailable_families.filter(Boolean) : [];
    setDetailLine(
      "[data-mutation-unavailable]",
      families.length > 0 ? `不可用协议族：${families.join("、")}` : "",
    );
    if (families.length > 0) {
      hasDetails = true;
    }
    setDetailLine(
      "[data-mutation-closed]",
      data.closed ? "映射当前已关闭，没有可用的监听端口。" : "",
    );
    if (data.closed) {
      hasDetails = true;
    }
    mutationDetails.hidden = !hasDetails;
  };

  const mappingShownError = (message, data) => {
    showNotice(message, "error");
    if (!mutationError || !mutationErrorText) {
      return;
    }
    renderMutationDetails(data);
    const hasDetails = Boolean(data) && (
      data.reload ||
      data.partial ||
      data.closed ||
      (Array.isArray(data.results) && data.results.length > 0) ||
      typeof data.revision === "number" ||
      Boolean(data.active_exposure) ||
      (Array.isArray(data.unavailable_families) && data.unavailable_families.length > 0)
    );
    if (hasDetails) {
      mutationErrorText.textContent = message;
      mutationError.hidden = false;
      mutationError.scrollIntoView({ block: "nearest" });
    }
  };

  const freezeMappingEditor = () => {
    if (!mappingTable) {
      return;
    }
    mappingTable.setAttribute("data-frozen", "true");
    mappingTable.querySelectorAll("[data-mapping-field]").forEach((input) => {
      input.disabled = true;
    });
    document.querySelectorAll("[data-mapping-save-list], [data-mapping-delete], [data-mapping-toggle]").forEach((button) => {
      button.disabled = true;
    });
    document.querySelectorAll("[data-mapping-add] [data-mapping-add-field], [data-mapping-add] button").forEach((control) => {
      control.disabled = true;
    });
  };

  const freezeExposureControl = (row, button) => {
    if (!row) {
      return;
    }
    row.setAttribute("data-exposure-frozen", "true");
    const select = row.querySelector("[data-exposure-select]");
    if (select) {
      select.disabled = true;
    }
    button.disabled = true;
  };

  const mappingEndpoint = mappingTable ? mappingTable.dataset.endpoint : "";
  const mappingBaseRevision = mappingTable
    ? Number(mappingTable.dataset.baseRevision || 0)
    : 0;

  const readMappings = () => {
    if (!mappingTable) {
      return [];
    }
    return Array.from(mappingTable.querySelectorAll("tbody tr[data-mapping-id]")).map((row) => {
      const field = (name) => row.querySelector(`[data-mapping-field="${name}"]`);
      const type = field("type");
      const port = field("remote_port");
      const local = field("local_addr");
      const note = field("note");
      const enabled = field("enabled");
      return {
        id: row.dataset.mappingId || "",
        type: type ? type.value : "tcp",
        remote_port: Number(port && port.value ? port.value : 0),
        local_addr: local ? local.value.trim() : "",
        note: note ? note.value.trim() : "",
        enabled: Boolean(enabled && enabled.checked),
      };
    });
  };

  const validMappings = (mappings) => {
    for (const mapping of mappings) {
      if (!mapping.id) {
        showNotice("映射缺少 ID，请重新加载页面。", "error");
        return false;
      }
      if (mapping.type !== "tcp" && mapping.type !== "udp") {
        showNotice(`映射 ${mapping.id} 的类型无效。`, "error");
        return false;
      }
      if (!Number.isInteger(mapping.remote_port) || mapping.remote_port < 0 || mapping.remote_port > 65535) {
        showNotice(`映射 ${mapping.id} 的远程端口必须在 0–65535 之间。`, "error");
        return false;
      }
      if (!mapping.local_addr) {
        showNotice(`映射 ${mapping.id} 需要填写本地地址。`, "error");
        return false;
      }
    }
    return true;
  };

  const sendMappings = async (mappings, exposures, successMessage) => {
    if (!mappingEndpoint) {
      return false;
    }
    const payload = {
      base_revision: mappingBaseRevision,
      mappings,
      exposures: exposures || null,
    };
    const response = await sendJSON(mappingEndpoint, "PUT", payload);
    if (handleUnauthorized(response)) {
      return false;
    }
    const data = await responseData(response);
    if (!response.ok) {
      const message = data && data.error ? data.error : "保存映射失败，请稍后再试。";
      mappingShownError(message, data);
      if (data && data.reload) {
        freezeMappingEditor();
      }
      return false;
    }
    showNotice(successMessage, "info");
    window.location.reload();
    return true;
  };

  const saveMappingsButton = document.querySelector("[data-mapping-save-list]");
  if (saveMappingsButton && mappingTable) {
    saveMappingsButton.addEventListener("click", async () => {
      const mappings = readMappings();
      if (!validMappings(mappings)) {
        return;
      }
      saveMappingsButton.disabled = true;
      try {
        await sendMappings(mappings, null, "映射列表已保存。");
      } catch (_error) {
        showNotice("无法连接到管理器，请检查网络。", "error");
      } finally {
        if (!mappingTable.getAttribute("data-frozen")) {
          saveMappingsButton.disabled = false;
        }
      }
    });
  }

  document.querySelectorAll("[data-mapping-delete]").forEach((button) => {
    button.addEventListener("click", async () => {
      const row = button.closest("tr[data-mapping-id]");
      if (!row) {
        return;
      }
      const mappingID = row.dataset.mappingId || "";
      if (button.dataset.confirm && !window.confirm(button.dataset.confirm)) {
        return;
      }
      const mappings = readMappings().filter((mapping) => mapping.id !== mappingID);
      button.disabled = true;
      try {
        await sendMappings(mappings, null, `映射 ${mappingID} 已删除。`);
      } catch (_error) {
        showNotice("无法连接到管理器，请检查网络。", "error");
      } finally {
        if (!mappingTable.hasAttribute("data-frozen")) {
          button.disabled = false;
        }
      }
    });
  });

  document.querySelectorAll("[data-mapping-toggle]").forEach((button) => {
    button.addEventListener("click", async () => {
      const row = button.closest("tr[data-mapping-id]");
      if (!row) {
        return;
      }
      const mappingID = row.dataset.mappingId || "";
      const mappings = readMappings().map((mapping) =>
        mapping.id === mappingID ? { ...mapping, enabled: !mapping.enabled } : mapping,
      );
      button.disabled = true;
      try {
        await sendMappings(mappings, null, "映射启用状态已更新。");
      } catch (_error) {
        showNotice("无法连接到管理器，请检查网络。", "error");
      } finally {
        if (!mappingTable.hasAttribute("data-frozen")) {
          button.disabled = false;
        }
      }
    });
  });

  const addMappingForm = document.querySelector("[data-mapping-add]");
  if (addMappingForm && mappingTable) {
    addMappingForm.addEventListener("submit", async (event) => {
      event.preventDefault();
      if (!addMappingForm.reportValidity()) {
        return;
      }
      const field = (name) => addMappingForm.querySelector(`[data-mapping-add-field="${name}"]`);
      const type = field("type");
      const port = field("remote_port");
      const local = field("local_addr");
      const note = field("note");
      const exposure = field("exposure");
      const enabled = field("enabled");
      const mapping = {
        id: newMappingID(),
        type: type ? type.value : "tcp",
        remote_port: Number(port && port.value ? port.value : 0),
        local_addr: local ? local.value.trim() : "",
        note: note ? note.value.trim() : "",
        enabled: Boolean(enabled && enabled.checked),
      };
      if (!validMappings([mapping])) {
        return;
      }
      const mappings = readMappings();
      mappings.push(mapping);
      let exposures = null;
      if (exposure && exposure.value === "local") {
        exposures = { [mapping.id]: "local" };
      }
      const submit = addMappingForm.querySelector("button[type='submit']");
      if (submit) {
        submit.disabled = true;
      }
      try {
        await sendMappings(mappings, exposures, "映射已新增。");
      } catch (_error) {
        showNotice("无法连接到管理器，请检查网络。", "error");
      } finally {
        if (submit && !(mappingTable && mappingTable.hasAttribute("data-frozen"))) {
          submit.disabled = false;
        }
      }
    });
  }

  document.querySelectorAll("[data-mapping-exposure]").forEach((button) => {
    button.addEventListener("click", async () => {
      const row = button.closest("tr[data-mapping-id]");
      const select = row ? row.querySelector("[data-exposure-select]") : null;
      if (!select) {
        return;
      }
      const exposure = select.value === "local" ? "local" : "public";
      if (!window.confirm("切换暴露范围会立即关闭该映射的现有连接，确定继续？")) {
        return;
      }
      button.disabled = true;
      try {
        const response = await sendJSON(button.dataset.endpoint, "PUT", { exposure });
        if (handleUnauthorized(response)) {
          return;
        }
        const data = await responseData(response);
        if (!response.ok) {
          const message = data && data.error ? data.error : "调整暴露范围失败，请稍后再试。";
          mappingShownError(message, data);
          if (data && data.reload) {
            freezeExposureControl(row, button);
          }
          return;
        }
        showNotice("暴露范围已更新。", "info");
        window.location.reload();
      } catch (_error) {
        showNotice("无法连接到管理器，请检查网络。", "error");
      } finally {
        if (!row.hasAttribute("data-exposure-frozen")) {
          button.disabled = false;
        }
      }
    });
  });

  // Server settings forms. Every mutation is explicit; failures leave the
  // operator's non-secret input untouched. Passwords and tokens are cleared
  // only after the manager confirms the upstream change.
  const credentialStatusLabel = (status) => {
    switch (String(status || "").toLowerCase()) {
      case "synced":
        return "已同步";
      case "pending":
        return "待同步";
      case "unsupported":
        return "不支持推送";
      case "timeout":
        return "超时";
      case "unknown":
        return "未知";
      default:
        return status || "未知";
    }
  };

  const credentialStatusClass = (status) => {
    switch (String(status || "").toLowerCase()) {
      case "synced":
        return "ok";
      case "pending":
        return "warn";
      case "unsupported":
      case "timeout":
        return "bad";
      default:
        return "muted";
    }
  };

  const splitPortList = (value) => String(value || "")
    .split(/[\s,]+/)
    .map((part) => part.trim())
    .filter(Boolean);

  const settingsFormField = (form, name) => form.elements.namedItem(name);

  // The upstream HY2 credential fields are validated in bytes by the server
  // API. UTF-8 encoding keeps the browser check aligned for non-ASCII input.
  const utf8ByteLength = (value) => {
    try {
      return new TextEncoder().encode(String(value || "")).length;
    } catch (_error) {
      return String(value || "").length;
    }
  };

  const settingsPayload = (form, kind) => {
    switch (kind) {
      case "mode": {
        const select = settingsFormField(form, "mode");
        if (!select || !select.value) {
          showNotice("请选择传输模式。", "error");
          return null;
        }
        return { mode: select.value };
      }
      case "ports": {
        const allow = settingsFormField(form, "allow");
        const deny = settingsFormField(form, "deny");
        const autoRange = settingsFormField(form, "auto_range");
        const reserve = settingsFormField(form, "offline_reserve_seconds");
        const seconds = Number(reserve ? reserve.value : 0);
        if (!Number.isInteger(seconds) || seconds < 1) {
          showNotice("离线预留时间至少需要 1 秒。", "error");
          return null;
        }
        return {
          allow: splitPortList(allow ? allow.value : ""),
          deny: splitPortList(deny ? deny.value : ""),
          auto_range: autoRange ? autoRange.value.trim() : "",
          offline_reserve_seconds: seconds,
        };
      }
      case "web": {
        const username = settingsFormField(form, "username");
        const newPassword = settingsFormField(form, "new_password");
        const confirmPassword = settingsFormField(form, "confirm_password");
        const next = newPassword ? newPassword.value : "";
        const confirm = confirmPassword ? confirmPassword.value : "";
        if (!username || !username.value.trim()) {
          showNotice("Web 管理员用户名不能为空。", "error");
          return null;
        }
        if (Array.from(next).length < 8) {
          showNotice("新密码至少需要 8 个字符。", "error");
          return null;
        }
        if (next !== confirm) {
          showNotice("两次输入的新密码不一致。", "error");
          return null;
        }
        return {
          username: username.value.trim(),
          new_password: next,
          confirm_password: confirm,
        };
      }
      case "proxy": {
        const trust = settingsFormField(form, "trust_proxy");
        return { trust_proxy: Boolean(trust && trust.checked) };
      }
      case "rotate": {
        const auth = settingsFormField(form, "auth");
        const token = settingsFormField(form, "control_token");
        const syncMode = settingsFormField(form, "sync_mode");
        const authValue = auth ? auth.value : "";
        const tokenValue = token ? token.value : "";
        if (utf8ByteLength(authValue) < 8) {
          showNotice("新的 HY2 密码至少需要 8 个字节。", "error");
          return null;
        }
        if (utf8ByteLength(tokenValue) < 16) {
          showNotice("新的 control token 至少需要 16 个字节。", "error");
          return null;
        }
        const sync = syncMode ? syncMode.value : "auto";
        if (sync !== "auto" && sync !== "manual") {
          showNotice("同步方式必须是 auto 或 manual。", "error");
          return null;
        }
        return { auth: authValue, control_token: tokenValue, sync_mode: sync };
      }
      default:
        return null;
    }
  };

  const setDisplayText = (selector, value) => {
    document.querySelectorAll(selector).forEach((element) => {
      element.textContent = value;
    });
  };

  const generationText = (value) => {
    const generation = Number(value);
    return Number.isInteger(generation) && generation > 0 ? String(generation) : "—";
  };

  const applySettingsSnapshot = (settings) => {
    if (!settings || typeof settings !== "object") {
      return;
    }
    setDisplayText("[data-settings-web-username]", settings.web_username || "—");
    if (settings.credentials) {
      setDisplayText("[data-settings-generation]", generationText(settings.credentials.generation));
      setDisplayText(
        "[data-settings-previous-generation]",
        settings.credentials.previous_generation > 0
          ? generationText(settings.credentials.previous_generation)
          : "—",
      );
      setDisplayText(
        "[data-settings-current-auth]",
        settings.credentials.current_auth_masked ? "********" : "—",
      );
      setDisplayText(
        "[data-settings-current-token]",
        settings.credentials.current_control_token_masked ? "********" : "—",
      );
    }
  };

  const clearSettingsSecrets = (form, kind) => {
    if (kind !== "web" && kind !== "rotate") {
      return;
    }
    form.querySelectorAll('input[type="password"]').forEach((input) => {
      input.value = "";
    });
  };

  const renderRotationResults = (data) => {
    const container = document.querySelector("[data-rotation-results]");
    const tbody = document.querySelector("[data-rotation-results-body]");
    if (!container || !tbody || !Array.isArray(data.results) || data.results.length === 0) {
      return;
    }
    tbody.replaceChildren();
    data.results.forEach((result) => {
      const row = document.createElement("tr");

      const clientCell = document.createElement("td");
      const name = document.createElement("div");
      name.textContent = result.Name || result.ClientID || "";
      const id = document.createElement("div");
      id.className = "muted mono wrap";
      id.textContent = result.ClientID || "";
      clientCell.append(name, id);
      row.appendChild(clientCell);

      const generationCell = document.createElement("td");
      generationCell.className = "mono";
      generationCell.textContent = generationText(result.Generation);
      row.appendChild(generationCell);

      const statusCell = document.createElement("td");
      const badge = document.createElement("span");
      badge.className = `state-badge ${credentialStatusClass(result.Status)}`;
      badge.textContent = credentialStatusLabel(result.Status);
      statusCell.appendChild(badge);
      row.appendChild(statusCell);

      const errorCell = document.createElement("td");
      if (result.Error) {
        const error = document.createElement("span");
        error.className = "error-text mono wrap";
        error.textContent = result.Error;
        errorCell.appendChild(error);
      } else {
        errorCell.textContent = "—";
      }
      row.appendChild(errorCell);

      tbody.appendChild(row);
    });
    container.hidden = false;
  };

  document.querySelectorAll("[data-settings-form]").forEach((form) => {
    form.addEventListener("submit", async (event) => {
      event.preventDefault();
      if (!form.reportValidity()) {
        return;
      }
      const kind = form.dataset.settingsKind || "";
      const payload = settingsPayload(form, kind);
      if (!payload) {
        return;
      }
      if (form.dataset.confirm && !window.confirm(form.dataset.confirm)) {
        return;
      }
      const button = form.querySelector("button[type='submit']");
      if (button) {
        button.disabled = true;
      }
      try {
        const response = await sendJSON(form.action, form.dataset.method || "POST", payload);
        if (handleUnauthorized(response)) {
          return;
        }
        const data = await responseData(response);
        if (!response.ok) {
          const message = data && data.error ? data.error : "保存设置失败，请稍后再试。";
          showNotice(message, "error");
          return;
        }
        clearSettingsSecrets(form, kind);
        if (data && data.settings_available !== false) {
          applySettingsSnapshot(data);
        }
        renderRotationResults(data || {});
        if (data && data.warning) {
          showNotice(data.warning, "warn");
          return;
        }
        if (kind === "mode" || kind === "ports" || kind === "proxy" || kind === "revoke") {
          window.location.reload();
          return;
        }
        showNotice("设置已保存。", "info");
      } catch (_error) {
        showNotice("无法连接到管理器，请检查网络。", "error");
      } finally {
        if (button) {
          button.disabled = false;
        }
      }
    });
  });

  function newMappingID() {
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
})();
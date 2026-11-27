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
          window.location.assign(basePath);
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

  const responseData = async (response) => {
    try {
      return await response.json();
    } catch (_error) {
      return null;
    }
  };

  const mappingShownError = (message, data) => {
    showNotice(message, "error");
    if (data && data.reload && mutationError && mutationErrorText) {
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
        button.disabled = false;
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
        button.disabled = false;
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
          return;
        }
        showNotice("暴露范围已更新。", "info");
        window.location.reload();
      } catch (_error) {
        showNotice("无法连接到管理器，请检查网络。", "error");
      } finally {
        button.disabled = false;
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
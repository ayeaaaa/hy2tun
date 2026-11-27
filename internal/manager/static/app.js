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
        button.disabled = false;
      }
    });
  });
})();
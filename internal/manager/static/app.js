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
})();
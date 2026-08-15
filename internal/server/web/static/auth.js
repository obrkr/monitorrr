"use strict";

// Setup and sign-in share this: two small forms, the same failure handling, and
// neither page loads the main application script.

function showError(message) {
  const el = document.getElementById("error");
  el.textContent = message;
  el.hidden = false;
}

async function submit(path, body) {
  const res = await fetch(path, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
  if (!res.ok) {
    let message = res.statusText;
    try {
      const parsed = await res.json();
      if (parsed.error) message = parsed.error;
    } catch (_) {
      /* keep the status text */
    }
    throw new Error(message);
  }
  return res.json();
}

const setupForm = document.getElementById("setup-form");
if (setupForm) {
  setupForm.addEventListener("submit", async (e) => {
    e.preventDefault();
    const password = document.getElementById("password").value;
    if (password !== document.getElementById("confirm").value) {
      showError("The two passwords do not match.");
      return;
    }
    try {
      await submit("/setup", {
        username: document.getElementById("username").value,
        password,
      });
      // The response set a session cookie, so this lands signed in.
      window.location.href = "/";
    } catch (err) {
      showError(err.message);
    }
  });
}

const loginForm = document.getElementById("login-form");
if (loginForm) {
  loginForm.addEventListener("submit", async (e) => {
    e.preventDefault();
    try {
      await submit("/login", {
        username: document.getElementById("username").value,
        password: document.getElementById("password").value,
      });
      window.location.href = "/";
    } catch (err) {
      showError(err.message);
    }
  });
}

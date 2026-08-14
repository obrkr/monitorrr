"use strict";

// One script serves both pages; each block no-ops when its elements are absent.

const $ = (sel) => document.querySelector(sel);

function toast(msg, isError) {
  const el = document.createElement("div");
  el.className = "toast" + (isError ? " err" : "");
  el.textContent = msg;
  document.body.appendChild(el);
  setTimeout(() => el.remove(), 2600);
}

async function api(path, options) {
  const res = await fetch(path, {
    headers: { "Content-Type": "application/json" },
    ...options,
  });
  if (!res.ok) {
    let msg = res.statusText;
    try {
      const body = await res.json();
      if (body.error) msg = body.error;
    } catch (_) {
      /* non-JSON error body; the status text will do */
    }
    throw new Error(msg);
  }
  return res.status === 204 ? null : res.json();
}

function relTime(iso) {
  const secs = Math.max(0, (Date.now() - new Date(iso).getTime()) / 1000);
  if (secs < 10) return "just now";
  if (secs < 90) return `${Math.round(secs)}s ago`;
  if (secs < 5400) return `${Math.round(secs / 60)}m ago`;
  if (secs < 172800) return `${Math.round(secs / 3600)}h ago`;
  return `${Math.round(secs / 86400)}d ago`;
}

const OS_LABEL = { darwin: "macOS", windows: "Windows", linux: "Linux" };

// ---- dashboard ----

const devicesBody = $("#devices-body");

if (devicesBody) {
  // The user may be mid-edit in the interval box; don't clobber it on refresh.
  let intervalFocused = false;
  const intervalInput = $("#interval-input");
  intervalInput.addEventListener("focus", () => (intervalFocused = true));
  intervalInput.addEventListener("blur", () => (intervalFocused = false));

  async function refresh() {
    try {
      const [d, e] = await Promise.all([api("/api/devices"), api("/api/events?limit=25")]);
      renderStats(d);
      renderDevices(d.devices, d.default_interval);
      renderEvents(e.events);
      $("#refresh-note").textContent = "refreshing every 5s";
    } catch (err) {
      $("#refresh-note").textContent = `refresh failed: ${err.message}`;
    }
  }

  function renderStats(d) {
    $("#stat-total").textContent = d.total;
    $("#stat-online").textContent = d.online;
    $("#stat-offline").textContent = d.total - d.online;
    if (!intervalFocused) $("#interval-input").value = d.default_interval;
  }

  function renderDevices(devices, defaultInterval) {
    if (!devices.length) {
      devicesBody.innerHTML =
        '<tr class="empty"><td colspan="7">No devices enrolled yet — see <a href="/deployment">Deployment</a> to install an agent.</td></tr>';
      return;
    }

    devicesBody.innerHTML = devices
      .map((dev) => {
        const online = dev.status === "online";
        const interval = dev.interval_seconds || defaultInterval;
        // Flag a device that is late but not yet past the offline grace period.
        const age = (Date.now() - new Date(dev.last_seen).getTime()) / 1000;
        const late = online && age > interval * 1.5;
        const ips = (dev.local_ips || []).join(", ") || "—";

        // Retirement has two visible stages: requested (waiting for the agent
        // to check in) and confirmed (the agent reported it uninstalled).
        const retired = dev.status === "retired";
        const retiring = !retired && !!dev.retired_at;
        // An agent that keeps checking in without acting on the instruction is
        // stuck, not slow. Distinguish "too old to understand it" from "should
        // understand it but hasn't", because the fixes differ.
        const canRetire = (dev.features || []).includes("retire");
        const ignored = retiring && dev.retire_checkins > 0 && !canRetire;
        const stalled = retiring && dev.retire_checkins >= 3 && canRetire;

        let statusCell;
        if (retired) {
          statusCell = '<span class="status retired"><span class="dot"></span>Retired</span>';
        } else if (ignored) {
          statusCell = `<span class="status stuck" title="This agent predates the retire feature, so it ignores the instruction. Upgrade the agent on this machine and it will retire on its next check-in. (${dev.retire_checkins} check-ins since the request)"><span class="dot"></span>Retire ignored — agent too old</span>`;
        } else if (stalled) {
          statusCell = `<span class="status stuck" title="The agent supports retirement but has not acknowledged after ${dev.retire_checkins} check-ins. Check its logs — the uninstall may be failing, e.g. insufficient privileges."><span class="dot"></span>Retire not acknowledged</span>`;
        } else if (retiring) {
          statusCell = '<span class="status retiring" title="waiting for the agent to check in and uninstall"><span class="dot"></span>Retiring…</span>';
        } else {
          statusCell = `<span class="status ${online ? "online" : "offline"}"><span class="dot"></span>${online ? "Online" : "Offline"}</span>`;
        }

        return `<tr>
          <td>${statusCell}</td>
          <td>
            <div class="hostname">${esc(dev.hostname)}</div>
            <div class="device-id">${esc(dev.id.slice(0, 12))}</div>
          </td>
          <td><span class="os-badge os-${esc(dev.os)}">${esc(OS_LABEL[dev.os] || dev.os)}</span> <span class="muted">${esc(dev.arch)}</span></td>
          <td class="muted">${esc(dev.agent_version || "—")}<br><span class="device-id">${interval}s</span></td>
          <td>
            <div>${esc(dev.remote_ip || "—")}</div>
            <div class="device-id">${esc(ips)}</div>
          </td>
          <td class="${late ? "stale" : ""}">${relTime(dev.last_seen)}</td>
          <td class="row-actions">
            ${retired || retiring ? "" : `<button class="link" data-retire="${esc(dev.id)}" title="Tell the agent to uninstall itself from this machine">Retire</button>`}
            <button class="link danger-link" data-delete="${esc(dev.id)}" title="Remove this record. A running agent will re-enroll.">Delete</button>
          </td>
        </tr>`;
      })
      .join("");
  }

  function renderEvents(events) {
    const list = $("#events");
    if (!events.length) {
      list.innerHTML = '<li class="muted">Nothing yet.</li>';
      return;
    }
    list.innerHTML = events
      .map(
        (ev) => `<li>
          <span class="ev-time">${new Date(ev.ts).toLocaleString()}</span>
          <span class="ev-host">${esc(ev.hostname)}</span>
          <span class="ev-kind ev-${esc(ev.kind)}">${esc(ev.kind)}</span>
          <span class="ev-detail">${esc(ev.detail)}</span>
        </li>`
      )
      .join("");
  }

  devicesBody.addEventListener("click", async (e) => {
    const retireID = e.target.dataset?.retire;
    if (retireID) {
      if (!confirm(
        "Retire this device?\n\n" +
        "On its next check-in the agent will uninstall itself — removing its " +
        "service, identity file, and binary — then stop.\n\n" +
        "The record is kept so you can see it was decommissioned. Any queued " +
        "jobs are cancelled."
      )) return;
      try {
        await api(`/api/devices/${retireID}/retire`, { method: "POST" });
        toast("Retiring — waiting for the agent to check in");
        refresh();
      } catch (err) {
        toast(err.message, true);
      }
      return;
    }

    const id = e.target.dataset?.delete;
    if (!id) return;
    if (!confirm(
      "Delete this device record and its history?\n\n" +
      "This does NOT uninstall the agent. A still-running agent will re-enroll " +
      "on its next check-in as a new device.\n\n" +
      "To remove the agent from the machine, use Retire instead."
    )) return;
    try {
      await api(`/api/devices/${id}`, { method: "DELETE" });
      toast("Device record deleted");
      refresh();
    } catch (err) {
      toast(err.message, true);
    }
  });

  $("#interval-form").addEventListener("submit", async (e) => {
    e.preventDefault();
    const seconds = parseInt($("#interval-input").value, 10);
    try {
      await api("/api/settings/interval", {
        method: "POST",
        body: JSON.stringify({ seconds }),
      });
      $("#interval-hint").textContent = `Set to ${seconds}s — agents adopt it on their next check-in.`;
      toast("Interval updated");
    } catch (err) {
      toast(err.message, true);
    }
  });

  refresh();
  setInterval(refresh, 5000);
}

// ---- deployment ----

document.querySelectorAll("[data-copy]").forEach((btn) => {
  btn.addEventListener("click", async () => {
    const text = $(btn.dataset.copy).textContent.trim();
    try {
      await navigator.clipboard.writeText(text);
      toast("Copied");
    } catch (_) {
      // Clipboard API needs a secure context; plain http:// lab hosts fall here.
      toast("Copy failed — select the text manually", true);
    }
  });
});

const revealBtn = $("#reveal-token");
if (revealBtn) {
  revealBtn.addEventListener("click", () => {
    const el = $("#enroll-token");
    const hidden = el.classList.toggle("masked");
    revealBtn.textContent = hidden ? "Reveal" : "Hide";
  });
}

const rotateBtn = $("#rotate-token");
if (rotateBtn) {
  rotateBtn.addEventListener("click", async () => {
    if (!confirm("Rotate the enrollment token?\n\nAlready-enrolled devices keep working. Install commands on this page will need re-copying.")) return;
    try {
      const res = await api("/api/settings/rotate-token", { method: "POST" });
      $("#enroll-token").textContent = res.enroll_token;
      toast("Token rotated — reload to refresh install commands");
    } catch (err) {
      toast(err.message, true);
    }
  });
}

document.querySelectorAll(".tab").forEach((tab) => {
  tab.addEventListener("click", () => {
    document.querySelectorAll(".tab").forEach((t) => t.classList.remove("active"));
    document.querySelectorAll(".tab-panel").forEach((p) => p.classList.remove("active"));
    tab.classList.add("active");
    document.querySelector(`[data-panel="${tab.dataset.tab}"]`).classList.add("active");
  });
});

// ---- scripts ----

const scriptList = $("#script-list");

if (scriptList) {
  let scripts = [];
  let devices = [];
  let current = null; // the script loaded in the editor, null when new

  const form = $("#script-form");
  const picker = $("#device-picker");

  const compatible = (interpreter, os) =>
    interpreter === "sh" ? os === "linux" || os === "darwin" : os === "windows";

  async function load() {
    try {
      const [s, d] = await Promise.all([api("/api/scripts"), api("/api/devices")]);
      scripts = s.scripts;
      devices = d.devices;
      renderList();
      renderPicker();
    } catch (err) {
      toast(err.message, true);
    }
  }

  function renderList() {
    if (!scripts.length) {
      scriptList.innerHTML = '<li class="muted pad">No scripts yet.</li>';
      return;
    }
    scriptList.innerHTML = scripts
      .map(
        (s) => `<li class="script-item ${current && current.id === s.id ? "selected" : ""}" data-id="${esc(s.id)}">
          <div>
            <div class="script-name">${esc(s.name)}</div>
            <div class="muted">${esc(s.description || "no description")}</div>
          </div>
          <span class="os-badge">${esc(s.interpreter)}</span>
        </li>`
      )
      .join("");
  }

  function loadScript(script) {
    current = script;
    $("#script-id").value = script ? script.id : "";
    $("#script-name").value = script ? script.name : "";
    $("#script-desc").value = script ? script.description : "";
    $("#script-interpreter").value = script ? script.interpreter : "sh";
    $("#script-timeout").value = script ? script.timeout_seconds : 300;
    $("#script-content").value = script ? script.content : "";
    $("#editor-title").textContent = script ? script.name : "New script";
    $("#script-hash").textContent = script ? `sha256 ${script.sha256.slice(0, 16)}…` : "";
    $("#delete-script").hidden = !script;
    $("#dispatch-note").textContent = script
      ? `${script.name} → ${script.interpreter}`
      : "save or select a script first";
    renderList();
    renderPicker();
    updateRunButton();
  }

  // The picker greys out devices whose OS cannot run the selected interpreter,
  // rather than hiding them — knowing a machine was skipped matters.
  function renderPicker() {
    if (!devices.length) {
      picker.innerHTML = '<p class="muted pad">No devices enrolled yet.</p>';
      return;
    }
    const interpreter = $("#script-interpreter").value;
    picker.innerHTML = devices
      .map((d) => {
        const ok = compatible(interpreter, d.os);
        const offline = d.status !== "online";
        return `<label class="device-option ${ok ? "" : "incompatible"}" title="${ok ? "" : `${d.os} cannot run ${interpreter} scripts`}">
          <input type="checkbox" value="${esc(d.id)}" ${ok ? "" : "disabled"}>
          <span class="status ${offline ? "offline" : "online"}"><span class="dot"></span></span>
          <span class="device-option-name">${esc(d.hostname)}</span>
          <span class="os-badge os-${esc(d.os)}">${esc(OS_LABEL[d.os] || d.os)}</span>
          ${offline ? '<span class="muted">offline — will run when it checks back in</span>' : ""}
        </label>`;
      })
      .join("");
  }

  const selectedDevices = () =>
    [...picker.querySelectorAll("input[type=checkbox]:checked")].map((c) => c.value);

  function updateRunButton() {
    const n = selectedDevices().length;
    $("#selection-count").textContent = `${n} selected`;
    $("#run-btn").disabled = !current || n === 0;
  }

  scriptList.addEventListener("click", (e) => {
    const item = e.target.closest("[data-id]");
    if (item) loadScript(scripts.find((s) => s.id === item.dataset.id));
  });

  $("#new-script").addEventListener("click", () => loadScript(null));
  $("#script-interpreter").addEventListener("change", () => {
    renderPicker();
    updateRunButton();
  });
  picker.addEventListener("change", updateRunButton);

  $("#select-all").addEventListener("click", () => {
    picker.querySelectorAll("input[type=checkbox]:not(:disabled)").forEach((c) => (c.checked = true));
    updateRunButton();
  });
  $("#select-none").addEventListener("click", () => {
    picker.querySelectorAll("input[type=checkbox]").forEach((c) => (c.checked = false));
    updateRunButton();
  });

  form.addEventListener("submit", async (e) => {
    e.preventDefault();
    try {
      const saved = await api("/api/scripts", {
        method: "POST",
        body: JSON.stringify({
          id: $("#script-id").value,
          name: $("#script-name").value,
          description: $("#script-desc").value,
          interpreter: $("#script-interpreter").value,
          content: $("#script-content").value,
          timeout_seconds: parseInt($("#script-timeout").value, 10),
        }),
      });
      const s = await api("/api/scripts");
      scripts = s.scripts;
      loadScript(saved);
      toast("Script saved");
    } catch (err) {
      toast(err.message, true);
    }
  });

  $("#delete-script").addEventListener("click", async () => {
    if (!current) return;
    if (!confirm(`Delete "${current.name}"?\n\nPast runs keep their own copy, so run history stays intact.`)) return;
    try {
      await api(`/api/scripts/${current.id}`, { method: "DELETE" });
      const s = await api("/api/scripts");
      scripts = s.scripts;
      loadScript(null);
      toast("Script deleted");
    } catch (err) {
      toast(err.message, true);
    }
  });

  $("#run-btn").addEventListener("click", async () => {
    const ids = selectedDevices();
    if (!current || !ids.length) return;
    if (!confirm(`Run "${current.name}" on ${ids.length} device(s)?\n\nThis executes the script as root/SYSTEM on each one.`)) return;
    try {
      const res = await api(`/api/scripts/${current.id}/dispatch`, {
        method: "POST",
        body: JSON.stringify({ device_ids: ids }),
      });
      toast(`Queued on ${res.queued} device(s) — see Runs`);
      picker.querySelectorAll("input[type=checkbox]").forEach((c) => (c.checked = false));
      updateRunButton();
    } catch (err) {
      toast(err.message, true);
    }
  });

  load();
}

// ---- runs ----

const jobsBody = $("#jobs-body");

if (jobsBody) {
  const stateClass = (job) => {
    if (job.state === "done") return job.error || job.exit_code !== 0 ? "bad" : "ok";
    if (job.state === "lost") return "bad";
    if (job.state === "running") return "warn";
    return "muted";
  };

  const duration = (ms) => (ms >= 1000 ? `${(ms / 1000).toFixed(1)}s` : `${ms}ms`);

  async function refreshJobs() {
    try {
      const d = await api("/api/jobs?limit=100");
      const failed = d.jobs.filter((j) => j.state === "lost" || (j.state === "done" && (j.error || j.exit_code !== 0))).length;
      $("#stat-runs").textContent = d.jobs.length;
      $("#stat-pending").textContent = d.pending;
      $("#stat-failed").textContent = failed;

      if (!d.jobs.length) {
        jobsBody.innerHTML = '<tr class="empty"><td colspan="7">Nothing has been run yet — dispatch a script from <a href="/scripts">Scripts</a>.</td></tr>';
        return;
      }
      jobsBody.innerHTML = d.jobs
        .map(
          (j) => `<tr class="clickable" data-id="${esc(j.id)}">
            <td><span class="${stateClass(j)}">${esc(j.state)}</span></td>
            <td>${esc(j.script_name)} <span class="os-badge">${esc(j.interpreter)}</span></td>
            <td>${esc(j.device_hostname)}</td>
            <td class="${stateClass(j)}">${j.exit_code === undefined || j.exit_code === null ? "—" : j.exit_code}</td>
            <td class="muted">${j.duration_ms ? duration(j.duration_ms) : "—"}</td>
            <td class="muted">${esc(j.created_by)}</td>
            <td class="muted">${relTime(j.created_at)}</td>
          </tr>`
        )
        .join("");
    } catch (err) {
      toast(err.message, true);
    }
  }

  jobsBody.addEventListener("click", async (e) => {
    const row = e.target.closest("[data-id]");
    if (!row) return;
    try {
      const j = await api(`/api/jobs/${row.dataset.id}`);
      $("#detail-title").textContent = `${j.script_name} on ${j.device_hostname}`;
      $("#detail-body").innerHTML = `
        <div class="field-grid">
          <label>State</label><div class="field"><code class="${stateClass(j)}">${esc(j.state)}</code></div>
          <label>Exit code</label><div class="field"><code>${j.exit_code === undefined || j.exit_code === null ? "—" : j.exit_code}</code></div>
          ${j.error ? `<label>Error</label><div class="field"><code class="bad">${esc(j.error)}</code></div>` : ""}
          <label>Duration</label><div class="field"><code>${j.duration_ms ? duration(j.duration_ms) : "—"}</code></div>
          <label>Dispatched by</label><div class="field"><code>${esc(j.created_by)}</code></div>
          <label>Script sha256</label><div class="field"><code>${esc(j.script_sha256)}</code></div>
        </div>
        ${j.truncated ? '<p class="note bad">Output was truncated at 64 KB.</p>' : ""}
        <div class="output-block"><h3>stdout</h3><pre><code>${esc(j.stdout) || '<span class="muted">(empty)</span>'}</code></pre></div>
        <div class="output-block"><h3>stderr</h3><pre><code>${esc(j.stderr) || '<span class="muted">(empty)</span>'}</code></pre></div>
        <div class="output-block"><h3>script as dispatched</h3><pre><code>${esc(j.content)}</code></pre></div>`;
      $("#job-detail").hidden = false;
      $("#job-detail").scrollIntoView({ behavior: "smooth", block: "start" });
    } catch (err) {
      toast(err.message, true);
    }
  });

  $("#close-detail").addEventListener("click", () => ($("#job-detail").hidden = true));

  refreshJobs();
  setInterval(refreshJobs, 5000);
}

function esc(s) {
  return String(s ?? "").replace(/[&<>"']/g, (c) => ({
    "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;",
  })[c]);
}

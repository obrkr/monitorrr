"use strict";

// One script serves every page; each block no-ops when its elements are absent.

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

function esc(s) {
  return String(s ?? "").replace(/[&<>"']/g, (c) => ({
    "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;",
  })[c]);
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

const duration = (ms) => (ms >= 1000 ? `${(ms / 1000).toFixed(1)}s` : `${ms}ms`);

// Retirement has several visible stages, and "stuck" needs distinguishing from
// "not yet" — the fixes differ, so the badge says which.
function deviceStatus(dev) {
  const retired = dev.status === "retired";
  const retiring = !retired && !!dev.retired_at;
  const canRetire = (dev.features || []).includes("retire");

  if (retired) return { cls: "retired", label: "Retired", title: "agent uninstalled itself" };
  if (retiring && dev.retire_checkins > 0 && !canRetire) {
    return {
      cls: "stuck",
      label: "Retire ignored — agent too old",
      title: `This agent predates the retire feature, so it ignores the instruction. Upgrade the agent and it will retire on its next check-in. (${dev.retire_checkins} check-ins since the request)`,
    };
  }
  if (retiring && dev.retire_checkins >= 3) {
    return {
      cls: "stuck",
      label: "Retire not acknowledged",
      title: `No acknowledgement after ${dev.retire_checkins} check-ins. Check the agent's logs — the uninstall may be failing.`,
    };
  }
  if (retiring) {
    return { cls: "retiring", label: "Retiring…", title: "waiting for the agent to check in and uninstall" };
  }
  if (dev.status === "online") return { cls: "online", label: "Online", title: "" };
  return { cls: "offline", label: "Offline", title: "" };
}

function statusBadge(dev) {
  const s = deviceStatus(dev);
  return `<span class="status ${s.cls}"${s.title ? ` title="${esc(s.title)}"` : ""}><span class="dot"></span>${esc(s.label)}</span>`;
}

function jobStateClass(job) {
  if (job.state === "done") return job.error || job.exit_code !== 0 ? "bad" : "ok";
  if (job.state === "lost") return "bad";
  if (job.state === "running") return "warn";
  return "muted";
}

function jobDetailHTML(j) {
  return `
    <div class="field-grid">
      <label>State</label><div class="field"><code class="${jobStateClass(j)}">${esc(j.state)}</code></div>
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
}

// Wires a jobs table so selecting a row opens its detail panel.
function wireJobDetail(tbody) {
  tbody.addEventListener("click", async (e) => {
    const row = e.target.closest("[data-job-id]");
    if (!row) return;
    try {
      const j = await api(`/api/jobs/${row.dataset.jobId}`);
      $("#detail-title").textContent = `${j.script_name} on ${j.device_hostname}`;
      $("#detail-body").innerHTML = jobDetailHTML(j);
      $("#job-detail").hidden = false;
      $("#job-detail").scrollIntoView({ behavior: "smooth", block: "start" });
    } catch (err) {
      toast(err.message, true);
    }
  });
  const close = $("#close-detail");
  if (close) close.addEventListener("click", () => ($("#job-detail").hidden = true));
}

// ---- dashboard ----

const devicesBody = $("#devices-body");

if (devicesBody) {
  let intervalFocused = false;
  let allDevices = [];
  let selected = new Set();
  let activeTag = "";
  let search = "";
  const intervalInput = $("#interval-input");
  intervalInput.addEventListener("focus", () => (intervalFocused = true));
  intervalInput.addEventListener("blur", () => (intervalFocused = false));

  async function refresh() {
    try {
      const [d, e, t, sc] = await Promise.all([
        api("/api/devices"),
        api("/api/events?limit=25"),
        api("/api/tags"),
        api("/api/scripts"),
      ]);
      allDevices = d.devices;
      renderStats(d);
      renderTagFilters(t.tags);
      renderScriptChoices(sc.scripts);
      renderDevices(d.default_interval);
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

  function renderTagFilters(tags) {
    const el = $("#tag-filters");
    if (!tags.length) {
      el.innerHTML = '<span class="muted">no tags yet — select devices below to add some</span>';
      return;
    }
    el.innerHTML =
      `<button type="button" class="tag-chip ${activeTag === "" ? "active" : ""}" data-tag="">All</button>` +
      tags
        .map(
          (t) => `<button type="button" class="tag-chip ${activeTag === t.tag ? "active" : ""}" data-tag="${esc(t.tag)}">${esc(t.tag)} <span class="muted">${t.count}</span></button>`
        )
        .join("");
  }

  function renderScriptChoices(scripts) {
    const el = $("#bulk-script");
    if (document.activeElement === el) return;
    const previous = el.value;
    el.innerHTML =
      '<option value="">Select a script…</option>' +
      scripts
        .map((s) => `<option value="${esc(s.id)}">${esc(s.name)} (${esc(s.interpreter)})</option>`)
        .join("");
    if (previous) el.value = previous;
  }

  // Filtering is client-side: the whole fleet arrives on every poll anyway, and
  // at lab scale a round trip per keystroke would be worse, not better.
  function visibleDevices() {
    return allDevices.filter((d) => {
      if (activeTag && !(d.tags || []).includes(activeTag)) return false;
      if (!search) return true;
      const hay = [d.hostname, d.public_ip, ...(d.local_ips || []), ...(d.tags || [])]
        .join(" ")
        .toLowerCase();
      return hay.includes(search);
    });
  }

  function renderDevices(defaultInterval) {
    const devices = visibleDevices();
    if (!devices.length) {
      devicesBody.innerHTML = allDevices.length
        ? '<tr class="empty"><td colspan="9">No devices match this filter.</td></tr>'
        : '<tr class="empty"><td colspan="9">No devices enrolled yet — see <a href="/deployment">Deployment</a> to install an agent.</td></tr>';
      updateBulkBar();
      return;
    }

    devicesBody.innerHTML = devices
      .map((dev) => {
        const interval = dev.interval_seconds || defaultInterval;
        const age = (Date.now() - new Date(dev.last_seen).getTime()) / 1000;
        const late = dev.status === "online" && !dev.retired_at && age > interval * 1.5;
        const ips = (dev.local_ips || []).join(", ") || "—";
        const tags = (dev.tags || []).length
          ? dev.tags.map((t) => `<span class="tag">${esc(t)}</span>`).join(" ")
          : '<span class="muted">—</span>';

        return `<tr data-device-id="${esc(dev.id)}">
          <td class="tick"><input type="checkbox" data-select="${esc(dev.id)}" ${selected.has(dev.id) ? "checked" : ""}></td>
          <td class="go">${statusBadge(dev)}</td>
          <td class="go">
            <div class="hostname">${esc(dev.hostname)}</div>
            <div class="device-id">${esc(dev.id.slice(0, 12))}</div>
          </td>
          <td class="go">${tags}</td>
          <td class="go"><span class="os-badge os-${esc(dev.os)}">${esc(OS_LABEL[dev.os] || dev.os)}</span> <span class="muted">${esc(dev.arch)}</span></td>
          <td class="go muted">${esc(dev.agent_version || "—")}<br><span class="device-id">${interval}s</span></td>
          <td class="go">${esc(dev.public_ip || "—")}</td>
          <td class="go muted">${esc(ips)}</td>
          <td class="go ${late ? "stale" : ""}">${relTime(dev.last_seen)}</td>
        </tr>`;
      })
      .join("");

    const shown = devices.map((d) => d.id);
    $("#select-all-devices").checked = shown.length > 0 && shown.every((id) => selected.has(id));
    updateBulkBar();
  }

  function renderEvents(events) {
    const list = $("#events");
    if (!events.length) {
      list.innerHTML = '<li class="muted pad">Nothing yet.</li>';
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

  function updateBulkBar() {
    const n = selected.size;
    $("#bulk-bar").hidden = n === 0;
    $("#bulk-count").textContent = `${n} selected`;
    $("#bulk-run").disabled = n === 0 || !$("#bulk-script").value;
  }

  // A row opens the device; only the checkbox cell selects it.
  devicesBody.addEventListener("click", (e) => {
    const box = e.target.closest("input[data-select]");
    if (box) {
      if (box.checked) selected.add(box.dataset.select);
      else selected.delete(box.dataset.select);
      updateBulkBar();
      return;
    }
    if (!e.target.closest("td.go")) return;
    const row = e.target.closest("[data-device-id]");
    if (row) window.location.href = `/devices/${row.dataset.deviceId}`;
  });

  $("#select-all-devices").addEventListener("change", (e) => {
    const shown = visibleDevices().map((d) => d.id);
    if (e.target.checked) shown.forEach((id) => selected.add(id));
    else shown.forEach((id) => selected.delete(id));
    renderDevices(parseInt($("#interval-input").value, 10));
  });

  $("#tag-filters").addEventListener("click", (e) => {
    const chip = e.target.closest("[data-tag]");
    if (!chip) return;
    activeTag = chip.dataset.tag;
    refresh();
  });

  $("#device-search").addEventListener("input", (e) => {
    search = e.target.value.trim().toLowerCase();
    renderDevices(parseInt($("#interval-input").value, 10));
  });

  $("#bulk-script").addEventListener("change", updateBulkBar);
  $("#bulk-clear").addEventListener("click", () => {
    selected.clear();
    renderDevices(parseInt($("#interval-input").value, 10));
  });

  $("#bulk-run").addEventListener("click", async () => {
    const scriptID = $("#bulk-script").value;
    if (!scriptID || !selected.size) return;
    const name = $("#bulk-script").options[$("#bulk-script").selectedIndex].text;
    if (!confirm(`Run "${name}" on ${selected.size} device(s)?\n\nIt executes as root/SYSTEM on each one.`)) return;
    try {
      const res = await api("/api/dispatch", {
        method: "POST",
        body: JSON.stringify({ script_id: scriptID, device_ids: [...selected] }),
      });
      // Skipped devices are reported, never silently dropped: a fleet run that
      // covered less than you asked for is exactly what you need to know.
      if (res.skipped && res.skipped.length) {
        toast(`Queued on ${res.queued}; skipped ${res.skipped.length} — ${res.skipped.map((s) => s.hostname + " (" + s.reason + ")").join("; ")}`, true);
      } else {
        toast(`Queued on ${res.queued} device(s) — see Runs`);
      }
      selected.clear();
      refresh();
    } catch (err) {
      toast(err.message, true);
    }
  });

  $("#bulk-tag").addEventListener("click", async () => {
    const tags = $("#bulk-tags").value.split(",").map((t) => t.trim()).filter(Boolean);
    if (!selected.size) return;
    if (!confirm(`Replace tags on ${selected.size} device(s) with: ${tags.join(", ") || "(none)"}?`)) return;
    try {
      for (const id of selected) {
        await api(`/api/devices/${id}/tags`, {
          method: "POST",
          body: JSON.stringify({ tags }),
        });
      }
      toast("Tags updated");
      $("#bulk-tags").value = "";
      selected.clear();
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

// ---- device page ----

const devicePanel = $("#device-panel");

if (devicePanel) {
  const deviceID = devicePanel.dataset.deviceId;
  let scriptFocused = false;
  let tagsFocused = false;

  async function loadDevice() {
    let d;
    try {
      d = await api(`/api/devices/${deviceID}`);
    } catch (err) {
      $("#device-hostname").textContent = "Device not found";
      $("#device-facts").innerHTML = `<p class="muted pad">${esc(err.message)}</p>`;
      return;
    }

    const dev = d.device;
    // Re-rendering while the operator is typing tags would discard their input.
    if (tagsFocused) return;
    document.title = `monitorrr · ${dev.hostname}`;
    $("#device-hostname").textContent = dev.hostname;
    $("#device-status").innerHTML = statusBadge(dev);

    const retired = dev.status === "retired";
    const retiring = !retired && !!dev.retired_at;

    $("#device-facts").innerHTML = `
      <label>Platform</label><div class="field"><span class="os-badge os-${esc(dev.os)}">${esc(OS_LABEL[dev.os] || dev.os)}</span> <code>${esc(dev.arch)}</code></div>
      <label>Public address</label><div class="field"><code>${esc(dev.public_ip || "not reported")}</code></div>
      <label>Local addresses</label><div class="field"><code>${esc((dev.local_ips || []).join(", ") || "—")}</code></div>
      <label>Seen from</label><div class="field"><code>${esc(dev.remote_ip || "—")}</code> <span class="muted">source address of its connection</span></div>
      <label>Agent</label><div class="field"><code>${esc(dev.agent_version || "—")}</code></div>
      <label>Capabilities</label><div class="field"><code>${esc((dev.features || []).join(", ") || "none advertised")}</code></div>
      <label>Check-in every</label><div class="field"><code>${dev.interval_seconds}s</code></div>
      <label>Last seen</label><div class="field"><code>${relTime(dev.last_seen)}</code> <span class="muted">${new Date(dev.last_seen).toLocaleString()}</span></div>
      <label>Enrolled</label><div class="field"><span class="muted">${new Date(dev.enrolled_at).toLocaleString()}</span></div>
      <label>Device ID</label><div class="field"><code>${esc(dev.id)}</code></div>
      <label>Tags</label>
      <div class="field">
        <input type="text" id="device-tags" value="${esc((dev.tags || []).join(", "))}" placeholder="site-a, laptop">
        <button type="button" id="save-tags">Save</button>
        <span class="muted">comma separated; used to target fleet-wide runs</span>
      </div>`;

    $("#device-actions").innerHTML = `
      ${retired || retiring ? "" : '<button type="button" id="retire-device">Retire</button>'}
      <button type="button" id="delete-device" class="danger">Delete record</button>
      <span class="muted">Retire uninstalls the agent from this machine. Delete only removes the record — a running agent re-enrols.</span>`;

    // Rebuilt on every poll, so the listener is attached to the new element.
    const tagsInput = $("#device-tags");
    tagsInput.addEventListener("focus", () => (tagsFocused = true));
    tagsInput.addEventListener("blur", () => (tagsFocused = false));
    $("#save-tags").addEventListener("click", async () => {
      const tags = tagsInput.value.split(",").map((t) => t.trim()).filter(Boolean);
      try {
        await api(`/api/devices/${dev.id}/tags`, { method: "POST", body: JSON.stringify({ tags }) });
        toast("Tags updated");
        loadDevice();
      } catch (err) {
        toast(err.message, true);
      }
    });

    const retireBtn = $("#retire-device");
    if (retireBtn) retireBtn.addEventListener("click", () => retireDevice(dev));
    $("#delete-device").addEventListener("click", () => deleteDevice(dev));

    renderRunPanel(d, dev, retired || retiring);
    renderJobs(d.jobs);
    renderEvents(d.events);
  }

  function renderRunPanel(d, dev, blocked) {
    const select = $("#run-script");
    const runBtn = $("#run-now");

    if (blocked) {
      select.innerHTML = '<option value="">unavailable</option>';
      select.disabled = true;
      runBtn.disabled = true;
      $("#run-note").textContent = "This device is being retired and no longer accepts jobs.";
      return;
    }
    if (!d.runnable_scripts.length) {
      select.innerHTML = '<option value="">no compatible scripts</option>';
      select.disabled = true;
      runBtn.disabled = true;
      $("#run-note").innerHTML = `No stored script can run on ${esc(OS_LABEL[dev.os] || dev.os)}. Write one under <a href="/scripts">Scripts</a>.`;
      return;
    }

    select.disabled = false;
    if (!scriptFocused) {
      const previous = select.value;
      select.innerHTML = d.runnable_scripts
        .map((s) => `<option value="${esc(s.id)}">${esc(s.name)} (${esc(s.interpreter)}, ${s.timeout_seconds}s)</option>`)
        .join("");
      if (previous) select.value = previous;
    }
    runBtn.disabled = false;
    $("#run-note").textContent = "Runs as root or SYSTEM on this machine.";
  }

  function renderJobs(jobs) {
    const body = $("#device-jobs");
    if (!jobs.length) {
      body.innerHTML = '<tr class="empty"><td colspan="6">Nothing has been run on this device yet.</td></tr>';
      return;
    }
    body.innerHTML = jobs
      .map(
        (j) => `<tr class="clickable" data-job-id="${esc(j.id)}">
          <td><span class="${jobStateClass(j)}">${esc(j.state)}</span></td>
          <td>${esc(j.script_name)} <span class="os-badge">${esc(j.interpreter)}</span></td>
          <td class="${jobStateClass(j)}">${j.exit_code === undefined || j.exit_code === null ? "—" : j.exit_code}</td>
          <td class="muted">${j.duration_ms ? duration(j.duration_ms) : "—"}</td>
          <td class="muted">${esc(j.created_by)}</td>
          <td class="muted">${relTime(j.created_at)}</td>
        </tr>`
      )
      .join("");
  }

  function renderEvents(events) {
    const list = $("#device-events");
    if (!events.length) {
      list.innerHTML = '<li class="muted pad">Nothing yet.</li>';
      return;
    }
    list.innerHTML = events
      .map(
        (ev) => `<li>
          <span class="ev-time">${new Date(ev.ts).toLocaleString()}</span>
          <span class="ev-kind ev-${esc(ev.kind)}">${esc(ev.kind)}</span>
          <span class="ev-detail">${esc(ev.detail)}</span>
        </li>`
      )
      .join("");
  }

  async function retireDevice(dev) {
    if (!confirm(
      `Retire ${dev.hostname}?\n\n` +
      "On its next check-in the agent will uninstall itself — removing its " +
      "service, identity file, and binary — then stop.\n\n" +
      "The record is kept so you can see it was decommissioned. Any queued " +
      "jobs are cancelled."
    )) return;
    try {
      await api(`/api/devices/${dev.id}/retire`, { method: "POST" });
      toast("Retiring — waiting for the agent to check in");
      loadDevice();
    } catch (err) {
      toast(err.message, true);
    }
  }

  async function deleteDevice(dev) {
    if (!confirm(
      `Delete the record for ${dev.hostname}?\n\n` +
      "This does NOT uninstall the agent. A still-running agent will re-enrol " +
      "on its next check-in as a new device.\n\n" +
      "To remove the agent from the machine, use Retire instead."
    )) return;
    try {
      await api(`/api/devices/${dev.id}`, { method: "DELETE" });
      toast("Device record deleted");
      window.location.href = "/";
    } catch (err) {
      toast(err.message, true);
    }
  }

  const select = $("#run-script");
  select.addEventListener("focus", () => (scriptFocused = true));
  select.addEventListener("blur", () => (scriptFocused = false));

  $("#run-now").addEventListener("click", async () => {
    const scriptID = select.value;
    if (!scriptID) return;
    const name = select.options[select.selectedIndex].text;
    if (!confirm(`Run "${name}" on this device?\n\nIt executes as root/SYSTEM.`)) return;
    try {
      await api(`/api/devices/${deviceID}/dispatch`, {
        method: "POST",
        body: JSON.stringify({ script_id: scriptID }),
      });
      toast("Queued — it runs on the next check-in");
      loadDevice();
    } catch (err) {
      toast(err.message, true);
    }
  });

  wireJobDetail($("#device-jobs"));

  loadDevice();
  setInterval(loadDevice, 5000);
}

// ---- scripts ----

const scriptList = $("#script-list");

if (scriptList) {
  let scripts = [];
  let current = null; // the script loaded in the editor, null when new

  const form = $("#script-form");

  async function load() {
    try {
      const s = await api("/api/scripts");
      scripts = s.scripts;
      renderList();
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
    renderList();
  }

  scriptList.addEventListener("click", (e) => {
    const item = e.target.closest("[data-id]");
    if (item) loadScript(scripts.find((s) => s.id === item.dataset.id));
  });

  $("#new-script").addEventListener("click", () => loadScript(null));

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

  load();
}

// ---- runs ----

const jobsBody = $("#jobs-body");

if (jobsBody) {
  async function refreshJobs() {
    try {
      const d = await api("/api/jobs?limit=100");
      const failed = d.jobs.filter((j) => j.state === "lost" || (j.state === "done" && (j.error || j.exit_code !== 0))).length;
      $("#stat-runs").textContent = d.jobs.length;
      $("#stat-pending").textContent = d.pending;
      $("#stat-failed").textContent = failed;

      if (!d.jobs.length) {
        jobsBody.innerHTML = '<tr class="empty"><td colspan="7">Nothing has been run yet — open a device and run a script.</td></tr>';
        return;
      }
      jobsBody.innerHTML = d.jobs
        .map(
          (j) => `<tr class="clickable" data-job-id="${esc(j.id)}">
            <td><span class="${jobStateClass(j)}">${esc(j.state)}</span></td>
            <td>${esc(j.script_name)} <span class="os-badge">${esc(j.interpreter)}</span></td>
            <td><a href="/devices/${esc(j.device_id)}">${esc(j.device_hostname)}</a></td>
            <td class="${jobStateClass(j)}">${j.exit_code === undefined || j.exit_code === null ? "—" : j.exit_code}</td>
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

  wireJobDetail(jobsBody);
  refreshJobs();
  setInterval(refreshJobs, 5000);
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

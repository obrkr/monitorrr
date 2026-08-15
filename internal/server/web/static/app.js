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

// The instance-wide display zone, set in Settings. Falls back to UTC, never to
// the browser's zone: two people reading the same timestamp must see the same
// time, whichever machine they are sitting at.
const DISPLAY_TZ = document.documentElement.dataset.timezone || "UTC";

const dateTimeFormat = new Intl.DateTimeFormat("en-GB", {
  timeZone: DISPLAY_TZ,
  year: "numeric", month: "short", day: "2-digit",
  hour: "2-digit", minute: "2-digit", second: "2-digit",
  hour12: false,
});

const dateFormat = new Intl.DateTimeFormat("en-GB", {
  timeZone: DISPLAY_TZ, year: "numeric", month: "short", day: "2-digit",
});

// fmtTime renders an instant in the configured zone.
function fmtTime(iso) {
  if (!iso) return "—";
  return dateTimeFormat.format(new Date(iso));
}

function fmtDate(iso) {
  if (!iso) return "—";
  return dateFormat.format(new Date(iso));
}

function relTime(iso) {
  const secs = Math.max(0, (Date.now() - new Date(iso).getTime()) / 1000);
  if (secs < 10) return "just now";
  if (secs < 90) return `${Math.round(secs)}s ago`;
  if (secs < 5400) return `${Math.round(secs / 60)}m ago`;
  if (secs < 172800) return `${Math.round(secs / 3600)}h ago`;
  return `${Math.round(secs / 86400)}d ago`;
}

function relFuture(iso) {
  const secs = (new Date(iso).getTime() - Date.now()) / 1000;
  if (secs <= 0) return "due";
  if (secs < 3600) return `in ${Math.round(secs / 60)}m`;
  if (secs < 172800) return `in ${Math.round(secs / 3600)}h`;
  return `in ${Math.round(secs / 86400)}d`;
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
          <span class="ev-time">${fmtTime(ev.ts)}</span>
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
    // Re-rendering while the operator is typing would discard their input.
    if (tagsFocused || document.activeElement === $("#collect-path")) return;
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
      <label>Last seen</label><div class="field"><code>${relTime(dev.last_seen)}</code> <span class="muted">${fmtTime(dev.last_seen)}</span></div>
      <label>Enrolled</label><div class="field"><span class="muted">${fmtTime(dev.enrolled_at)}</span></div>
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
    renderCollections(d.collections || [], d.retention_days || 7);
    renderJobs(d.jobs);
    renderEvents(d.events);
  }

  const humanSize = (n) =>
    n >= 1 << 30 ? `${(n / (1 << 30)).toFixed(1)} GB`
    : n >= 1 << 20 ? `${(n / (1 << 20)).toFixed(1)} MB`
    : n >= 1024 ? `${(n / 1024).toFixed(1)} KB`
    : `${n} B`;

  function collectStateClass(c) {
    if (c.state === "done") return "ok";
    if (c.state === "failed") return "bad";
    if (c.state === "expired") return "muted";
    return "warn";
  }

  function renderCollections(collections, retentionDays) {
    $("#retention-note").textContent = `kept for ${retentionDays} days, then deleted`;
    const body = $("#collections");
    if (!collections.length) {
      body.innerHTML = '<tr class="empty"><td colspan="6">Nothing collected yet.</td></tr>';
      return;
    }

    body.innerHTML = collections
      .map((c) => {
        // Size is known before any bytes move, so it is shown as soon as the
        // agent has looked at the file.
        const size = c.size ? humanSize(c.size) : "—";
        let progress;
        if (c.state === "transferring" && c.size > 0) {
          const pct = Math.min(100, Math.round((c.received / c.size) * 100));
          progress = `<div class="bar"><div class="bar-fill" style="width:${pct}%"></div></div>
                      <span class="device-id">${pct}% · ${humanSize(c.received)}</span>`;
        } else if (c.state === "done") {
          progress = '<span class="ok">complete</span>';
        } else if (c.state === "failed") {
          progress = `<span class="bad" title="${esc(c.error)}">${esc(c.error.slice(0, 60))}</span>`;
        } else if (c.state === "expired") {
          progress = '<span class="muted">deleted after retention</span>';
        } else {
          progress = '<span class="muted">waiting for the agent…</span>';
        }

        const expires = c.expires_at
          ? (c.state === "expired" ? "deleted" : relFuture(c.expires_at))
          : "—";

        const actions =
          c.state === "done"
            ? `<a class="btn" href="/api/collections/${esc(c.id)}/download">Download</a>
               <button class="link danger-link" data-delete-collection="${esc(c.id)}">Delete</button>`
            : `<button class="link danger-link" data-delete-collection="${esc(c.id)}">Remove</button>`;

        return `<tr>
          <td><span class="${collectStateClass(c)}">${esc(c.state)}</span>${c.copied ? ' <span class="os-badge" title="the file was locked, so it was copied aside and read from the copy">copied</span>' : ""}</td>
          <td><code class="device-id">${esc(c.path)}</code></td>
          <td class="muted">${size}</td>
          <td>${progress}</td>
          <td class="muted">${expires}</td>
          <td class="row-actions">${actions}</td>
        </tr>`;
      })
      .join("");
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
          <span class="ev-time">${fmtTime(ev.ts)}</span>
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

  $("#collect-now").addEventListener("click", async () => {
    const path = $("#collect-path").value.trim();
    if (!path) {
      toast("Enter a full file path", true);
      return;
    }
    try {
      await api(`/api/devices/${deviceID}/collect`, {
        method: "POST",
        body: JSON.stringify({ path }),
      });
      $("#collect-path").value = "";
      toast("Requested — the agent will send it on its next check-in");
      loadDevice();
    } catch (err) {
      toast(err.message, true);
    }
  });

  $("#collect-path").addEventListener("keydown", (e) => {
    if (e.key === "Enter") $("#collect-now").click();
  });

  $("#collections").addEventListener("click", async (e) => {
    const id = e.target.dataset?.deleteCollection;
    if (!id) return;
    if (!confirm("Delete this collected file from the server?")) return;
    try {
      await api(`/api/collections/${id}`, { method: "DELETE" });
      toast("Deleted");
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

  let payloads = [];

  async function load() {
    try {
      const [s, p] = await Promise.all([api("/api/scripts"), api("/api/payloads")]);
      scripts = s.scripts;
      payloads = p.payloads;
      renderList();
      renderPayloads();
      renderPayloadPicker();
    } catch (err) {
      toast(err.message, true);
    }
  }

  const humanSize = (n) =>
    n >= 1 << 30 ? `${(n / (1 << 30)).toFixed(1)} GB`
    : n >= 1 << 20 ? `${(n / (1 << 20)).toFixed(1)} MB`
    : n >= 1024 ? `${(n / 1024).toFixed(1)} KB`
    : `${n} B`;

  function renderPayloads() {
    const body = $("#payload-list");
    if (!payloads.length) {
      body.innerHTML = '<tr class="empty"><td colspan="6">No files uploaded yet.</td></tr>';
      return;
    }
    body.innerHTML = payloads
      .map(
        (p) => `<tr>
          <td><code>${esc(p.filename)}</code></td>
          <td class="muted">${humanSize(p.size)}</td>
          <td class="device-id">${esc(p.sha256.slice(0, 16))}…</td>
          <td class="muted">${esc((p.used_by || []).join(", ") || "—")}</td>
          <td class="muted">${relTime(p.created_at)}</td>
          <td><button class="link danger-link" data-delete-payload="${esc(p.id)}">Delete</button></td>
        </tr>`
      )
      .join("");
  }

  function renderPayloadPicker() {
    const sel = $("#script-payload");
    if (document.activeElement === sel) return;
    const chosen = current ? current.payload_id || "" : "";
    sel.innerHTML =
      '<option value="">No file</option>' +
      payloads
        .map((p) => `<option value="${esc(p.id)}" ${p.id === chosen ? "selected" : ""}>${esc(p.filename)} (${humanSize(p.size)})</option>`)
        .join("");
    $("#payload-hint").textContent = chosen
      ? "pushed to the device, path in $MONITORRR_PAYLOAD"
      : "optional — upload files below";
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
    // Attaching only makes sense once a script exists to attach to.
    $("#payload-row").hidden = !script;
    renderList();
    renderPayloadPicker();
  }

  scriptList.addEventListener("click", (e) => {
    const item = e.target.closest("[data-id]");
    if (item) loadScript(scripts.find((s) => s.id === item.dataset.id));
  });

  $("#new-script").addEventListener("click", () => loadScript(null));

  $("#import-starters").addEventListener("click", async () => {
    try {
      const preview = await api("/api/scripts/starter");
      const missing = preview.starters.filter((s) => !s.already_present);
      if (!missing.length) {
        toast("Every starter script is already here");
        return;
      }
      const names = missing.map((s) => `  • ${s.name} — ${s.description}`).join("\n");
      if (!confirm(`Add ${missing.length} starter script(s)?\n\n${names}\n\nAll are read-only diagnostics. Existing scripts with the same name are left untouched.`)) return;

      const res = await api("/api/scripts/starter", { method: "POST" });
      await load();
      toast(`Added ${res.imported.length} script(s)`);
    } catch (err) {
      toast(err.message, true);
    }
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

  $("#script-payload").addEventListener("change", async (e) => {
    if (!current) return;
    try {
      const updated = await api(`/api/scripts/${current.id}/payload`, {
        method: "POST",
        body: JSON.stringify({ payload_id: e.target.value }),
      });
      current = updated;
      await load();
      loadScript(updated);
      toast(e.target.value ? "File attached" : "File detached");
    } catch (err) {
      toast(err.message, true);
    }
  });

  $("#upload-payload").addEventListener("click", async () => {
    const input = $("#payload-file");
    if (!input.files.length) {
      toast("Choose a file first", true);
      return;
    }
    const file = input.files[0];
    const form = new FormData();
    form.append("file", file);

    $("#upload-status").textContent = `Uploading ${file.name} (${humanSize(file.size)})…`;
    try {
      // Not via api(): the body is multipart, so the JSON content type the
      // helper sets would corrupt the boundary.
      const res = await fetch("/api/payloads", { method: "POST", body: form });
      if (!res.ok) {
        const body = await res.json().catch(() => ({}));
        throw new Error(body.error || res.statusText);
      }
      input.value = "";
      $("#upload-status").textContent = "";
      await load();
      toast("File uploaded");
    } catch (err) {
      $("#upload-status").textContent = "";
      toast(err.message, true);
    }
  });

  $("#payload-list").addEventListener("click", async (e) => {
    const id = e.target.dataset?.deletePayload;
    if (!id) return;
    const p = payloads.find((x) => x.id === id);
    const warning = p && p.used_by && p.used_by.length
      ? `\n\nIt is attached to: ${p.used_by.join(", ")}. Those scripts will run without it.`
      : "";
    if (!confirm(`Delete ${p ? p.filename : "this file"}?${warning}`)) return;
    try {
      await api(`/api/payloads/${id}`, { method: "DELETE" });
      await load();
      toast("File deleted");
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

// ---- settings ----

const usersBody = $("#users");

if (usersBody) {
  let you = "";

  async function loadUsers() {
    try {
      const d = await api("/api/users");
      you = d.you;
      usersBody.innerHTML = d.users
        .map(
          (u) => `<tr>
            <td><strong>${esc(u.username)}</strong>${u.id === you ? ' <span class="muted">(you)</span>' : ""}</td>
            <td>
              <select data-role-for="${esc(u.id)}" ${u.id === you ? "disabled" : ""}>
                <option value="admin" ${u.role === "admin" ? "selected" : ""}>admin</option>
                <option value="readonly" ${u.role === "readonly" ? "selected" : ""}>read-only</option>
              </select>
            </td>
            <td class="muted">${fmtDate(u.created_at)}</td>
            <td class="muted">${u.last_login ? relTime(u.last_login) : "never"}</td>
            <td class="row-actions">
              <button class="link" data-password-for="${esc(u.id)}" data-name="${esc(u.username)}">Set password</button>
              ${u.id === you ? "" : `<button class="link danger-link" data-delete-user="${esc(u.id)}" data-name="${esc(u.username)}">Delete</button>`}
            </td>
          </tr>`
        )
        .join("");
    } catch (err) {
      toast(err.message, true);
    }
  }

  usersBody.addEventListener("change", async (e) => {
    const id = e.target.dataset?.roleFor;
    if (!id) return;
    try {
      await api(`/api/users/${id}`, {
        method: "PATCH",
        body: JSON.stringify({ role: e.target.value }),
      });
      toast("Role updated");
      loadUsers();
    } catch (err) {
      toast(err.message, true);
      loadUsers();
    }
  });

  usersBody.addEventListener("click", async (e) => {
    const del = e.target.dataset?.deleteUser;
    if (del) {
      if (!confirm(`Delete the account "${e.target.dataset.name}"?\n\nAny active session it has ends immediately.`)) return;
      try {
        await api(`/api/users/${del}`, { method: "DELETE" });
        toast("Account deleted");
        loadUsers();
      } catch (err) {
        toast(err.message, true);
      }
      return;
    }

    const pw = e.target.dataset?.passwordFor;
    if (pw) {
      const password = prompt(`New password for "${e.target.dataset.name}" (at least 8 characters):`);
      if (!password) return;
      try {
        await api(`/api/users/${pw}`, {
          method: "PATCH",
          body: JSON.stringify({ password }),
        });
        // Changing a password ends that account's other sessions, so say so.
        toast("Password changed — their other sessions were ended");
      } catch (err) {
        toast(err.message, true);
      }
    }
  });

  $("#new-user").addEventListener("submit", async (e) => {
    e.preventDefault();
    try {
      await api("/api/users", {
        method: "POST",
        body: JSON.stringify({
          username: $("#new-username").value,
          password: $("#new-password").value,
          role: $("#new-role").value,
        }),
      });
      $("#new-username").value = "";
      $("#new-password").value = "";
      toast("Account created");
      loadUsers();
    } catch (err) {
      toast(err.message, true);
    }
  });

  // Both of these are irreversible, so they ask for the word to be typed
  // rather than accepting a click.
  async function confirmDestructive(title, detail, path) {
    if (!confirm(`${title}\n\n${detail}`)) return null;
    const typed = prompt(`Type RESET to confirm:`);
    if (typed !== "RESET") {
      if (typed !== null) toast("Not confirmed — nothing was changed");
      return null;
    }
    return api(path, { method: "POST", body: JSON.stringify({ confirm: typed }) });
  }

  $("#retire-all").addEventListener("click", async () => {
    try {
      const res = await confirmDestructive(
        "Retire every agent?",
        "Each machine will uninstall its agent on its next check-in and stop reporting.",
        "/api/admin/retire-all"
      );
      if (res) toast(`${res.retiring} device(s) retiring`);
    } catch (err) {
      toast(err.message, true);
    }
  });

  $("#reset-instance").addEventListener("click", async () => {
    try {
      const res = await confirmDestructive(
        "Reset this instance?",
        "Every device, script, run and stored file is deleted and the enrollment token is rotated. Accounts are kept. Agents are NOT uninstalled.",
        "/api/admin/reset"
      );
      if (res) {
        const r = res.removed;
        toast(`Reset: ${r.devices} devices, ${r.scripts} scripts, ${r.jobs} runs removed`);
      }
    } catch (err) {
      toast(err.message, true);
    }
  });

  const themeSelect = $("#theme");
  if (themeSelect) {
    themeSelect.addEventListener("change", async (e) => {
      const theme = e.target.value;
      try {
        await api("/api/settings/theme", {
          method: "POST",
          body: JSON.stringify({ theme }),
        });
        // Applied immediately rather than on the next page load, so the choice
        // can actually be judged.
        document.documentElement.dataset.theme = theme;
        toast(`Theme set to ${theme}`);
      } catch (err) {
        toast(err.message, true);
      }
    });
  }

  const saveTimezone = $("#save-timezone");
  if (saveTimezone) {
    saveTimezone.addEventListener("click", async () => {
      try {
        const res = await api("/api/settings/timezone", {
          method: "POST",
          body: JSON.stringify({ timezone: $("#timezone").value }),
        });
        toast(`Times now shown in ${res.timezone} — reload to apply`);
      } catch (err) {
        toast(err.message, true);
      }
    });
  }

  loadUsers();
}

// ---- audit ----

const auditRows = $("#audit-rows");

if (auditRows) {
  let oldest = 0;
  let filters = { q: "", action: "", username: "" };

  // Destructive and security-relevant actions are picked out, so a long log
  // still shows the things worth noticing.
  const DANGER = ["admin.reset", "admin.retire-all", "device.retire", "device.delete",
                  "script.run", "account.delete", "account.role", "settings.rotate-token"];

  function actionClass(entry) {
    if (entry.action.endsWith("-failed")) return "audit-action audit-failed";
    if (DANGER.includes(entry.action)) return "audit-action audit-danger";
    return "audit-action";
  }

  function rowsFor(entries) {
    return entries
      .map(
        (e) => `<tr>
          <td class="audit-when">${fmtTime(e.ts)}</td>
          <td>${esc(e.username)}${e.role ? ` <span class="muted">${esc(e.role)}</span>` : ""}</td>
          <td><span class="${actionClass(e)}">${esc(e.action)}</span></td>
          <td>${esc(e.target || "—")}</td>
          <td class="audit-detail">${esc(e.detail || "")}</td>
          <td class="muted">${esc(e.ip)}</td>
        </tr>`
      )
      .join("");
  }

  function query(before) {
    const p = new URLSearchParams();
    if (filters.q) p.set("q", filters.q);
    if (filters.action) p.set("action", filters.action);
    if (filters.username) p.set("username", filters.username);
    if (before) p.set("before", before);
    p.set("limit", "100");
    return "/api/audit?" + p.toString();
  }

  async function loadAudit() {
    try {
      const d = await api(query(0));
      $("#audit-count").textContent = `${d.total} entries recorded`;

      const select = $("#audit-action");
      if (document.activeElement !== select) {
        const current = select.value;
        select.innerHTML =
          '<option value="">All actions</option>' +
          d.actions.map((a) => `<option value="${esc(a)}">${esc(a)}</option>`).join("");
        select.value = current;
      }

      if (!d.entries.length) {
        auditRows.innerHTML = '<tr class="empty"><td colspan="6">Nothing recorded yet.</td></tr>';
        $("#audit-more").hidden = true;
        return;
      }
      auditRows.innerHTML = rowsFor(d.entries);
      oldest = d.entries[d.entries.length - 1].id;
      $("#audit-more").hidden = d.entries.length < 100;
    } catch (err) {
      toast(err.message, true);
    }
  }

  $("#audit-more").addEventListener("click", async () => {
    try {
      const d = await api(query(oldest));
      if (!d.entries.length) {
        $("#audit-more").hidden = true;
        return;
      }
      auditRows.insertAdjacentHTML("beforeend", rowsFor(d.entries));
      oldest = d.entries[d.entries.length - 1].id;
      $("#audit-more").hidden = d.entries.length < 100;
    } catch (err) {
      toast(err.message, true);
    }
  });

  let debounce;
  function onFilterChange() {
    clearTimeout(debounce);
    debounce = setTimeout(() => {
      filters = {
        q: $("#audit-search").value.trim(),
        action: $("#audit-action").value,
        username: $("#audit-user").value.trim(),
      };
      loadAudit();
    }, 200);
  }

  $("#audit-search").addEventListener("input", onFilterChange);
  $("#audit-action").addEventListener("change", onFilterChange);
  $("#audit-user").addEventListener("input", onFilterChange);
  $("#audit-clear").addEventListener("click", () => {
    $("#audit-search").value = "";
    $("#audit-action").value = "";
    $("#audit-user").value = "";
    onFilterChange();
  });

  loadAudit();
  // Slower than the dashboard: an audit log is reviewed, not watched.
  setInterval(loadAudit, 15000);
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

const autoUpdateBox = $("#auto-update");
if (autoUpdateBox) {
  autoUpdateBox.addEventListener("change", async (e) => {
    const enabled = e.target.checked;
    try {
      await api("/api/settings/auto-update", {
        method: "POST",
        body: JSON.stringify({ enabled }),
      });
      $("#auto-update-state").textContent = enabled ? "enabled" : "disabled";
      toast(enabled ? "Auto-update enabled" : "Auto-update disabled");
    } catch (err) {
      e.target.checked = !enabled;
      toast(err.message, true);
    }
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

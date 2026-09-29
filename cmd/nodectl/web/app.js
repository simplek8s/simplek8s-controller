// SimpleK8s setup wizard SPA (vanilla JS, no build step).
"use strict";
const $ = (id) => document.getElementById(id);
const alertBox = $("alert");

function showAlert(msg, kind) {
  alertBox.textContent = msg;
  alertBox.className = "alert alert-" + (kind || "danger");
}
function clearAlert() { alertBox.className = "alert d-none"; }
function hideAll() {
  for (const id of ["view-login", "view-install", "view-cluster", "view-tokens"])
    $(id).classList.add("d-none");
}
async function api(method, path, body) {
  const opts = { method, headers: {} };
  if (body !== undefined) { opts.headers["Content-Type"] = "application/json"; opts.body = JSON.stringify(body); }
  const r = await fetch(path, opts);
  let data = {};
  try { data = await r.json(); } catch (_) { /* non-JSON */ }
  if (r.status === 401 && path !== "/api/login") { await render(false); throw new Error("login required"); }
  if (!r.ok) throw new Error((data && data.error) || ("HTTP " + r.status));
  return data;
}
function copyText(text, btn) {
  const done = () => { const o = btn.textContent; btn.textContent = "✅ Copied!"; setTimeout(() => btn.textContent = o, 1500); };
  if (navigator.clipboard && navigator.clipboard.writeText) {
    navigator.clipboard.writeText(text).then(done, () => fallbackCopy(text, done));
  } else fallbackCopy(text, done);
}
// browserNodeIP returns the node address the browser used, when
// it is an IP literal: the natural advertise-address default
// (editable). DNS names are left for the operator to replace.
function browserNodeIP() {
  let h = window.location.hostname || "";
  if (h.startsWith("[") && h.endsWith("]")) h = h.slice(1, -1);
  if (/^\d+\.\d+\.\d+\.\d+$/.test(h)) return h;
  if (h.includes(":")) return h;
  return "";
}
// prefillInitAddr fills the advertise address with the node's
// primary IP (server-detected, like getty's \4), falling back
// to the browser-visible address. Never overwrites typing.
async function prefillInitAddr() {
  if ($("init-addr").value) return;
  try {
    const n = await api("GET", "/api/network");
    if (n.primaryIP) { $("init-addr").value = n.primaryIP; return; }
  } catch (_) { /* fall back below */ }
  $("init-addr").value = browserNodeIP();
}
function fallbackCopy(text, done) {
  const ta = document.createElement("textarea");
  ta.value = text; document.body.appendChild(ta); ta.select();
  try { document.execCommand("copy"); done(); } catch (_) { showAlert("Copy manually", "warning"); }
  document.body.removeChild(ta);
}

async function render(loggedIn) {
  clearAlert(); hideAll();
  $("logout").classList.toggle("d-none", !loggedIn);
  if (!loggedIn) { $("view-login").classList.remove("d-none"); return; }
  let state = "live";
  try { state = (await api("GET", "/api/state")).state; }
  catch (e) { $("view-login").classList.remove("d-none"); return; }
  if (state === "live") { $("view-install").classList.remove("d-none"); await loadInstaller(); }
  else if (state === "fresh") { $("view-cluster").classList.remove("d-none"); await prefillInitAddr(); }
  else if (state === "cp") { $("view-tokens").classList.remove("d-none"); await listTokens(); }
  else showAlert("Nothing to manage on this node (worker).", "info");
}

async function loadInstaller() {
  $("ins-title").classList.remove("d-none");
  const disks = (await api("GET", "/api/disks")).disks || [];
  const sel = $("ins-disk"); sel.innerHTML = "";
  for (const d of disks) {
    const o = document.createElement("option");
    o.value = d.path;
    o.textContent = d.path + " (" + Math.round(d.sizeBytes / 1073741824) + "GiB" + (d.model ? ", " + d.model : "") + ")" + (d.selectable ? "" : " — " + d.reason);
    o.disabled = !d.selectable;
    sel.appendChild(o);
  }
  updateConfirmLabel();
  await loadVersions();
  clearAlert();
}
function updateConfirmLabel() {
  const dev = $("ins-disk").value || "/dev/vda";
  $("ins-confirm-label").innerHTML = "Type <code>" + dev + "</code> to confirm previous data destruction";
}
async function loadVersions() {
  const ch = $("ins-channel").value.trim() || "stable";
  const v = await api("GET", "/api/versions?channel=" + encodeURIComponent(ch));
  clearAlert();
  const dl = $("ins-ts-list"); dl.innerHTML = "";
  for (const r of (v.releases || [])) {
    const o = document.createElement("option");
    o.value = r.ts;
    dl.appendChild(o);
  }
}
function pollJob(jobId, logEl, barEl, done) {
  const pctEl = barEl ? document.getElementById("ins-pct") : null;
  const iv = setInterval(async () => {
    try {
      const j = await api("GET", "/api/jobs/" + jobId);
      let text = (j.log || []).join("\n") + (j.message ? "\n" + j.message : "") + (j.error ? "\n" + j.error : "");
      if (j.status === "running") text += (text ? "\n" : "") + "...";
      if (logEl) logEl.textContent = text;
      if (logEl) logEl.scrollTop = logEl.scrollHeight;
      if (barEl) {
        let pct = 100;
        if (j.totalMiB > 0) pct = Math.min(100, Math.round((j.downloadedMiB / j.totalMiB) * 100));
        else if (j.status === "running") pct = Math.min(99, (parseInt(barEl.style.width) || 0) + 1);
        barEl.style.width = pct + "%";
        if (pctEl) pctEl.textContent = pct + "%";
      }
      if (j.status !== "running") {
        clearInterval(iv);
        if (barEl) { barEl.style.width = "100%"; if (pctEl) pctEl.textContent = "100%"; }
        done(j);
      }
    } catch (e) { clearInterval(iv); showAlert(String(e)); }
  }, 1500);
}

document.addEventListener("DOMContentLoaded", () => {
  $("theme").onclick = () => {
    const cur = document.documentElement.dataset.theme === "dark" ? "light" : "dark";
    document.documentElement.dataset.theme = cur; localStorage.setItem("theme", cur);
  };
  $("login-form").onsubmit = async (ev) => {
    ev.preventDefault();
    clearAlert();
    $("login-go").disabled = true;
    $("login-go").textContent = "Validating...";
    try {
      await api("POST", "/api/login", { username: $("login-user").value, password: $("login-pass").value });
      $("login-pass").value = "";
      await render(true);
    } catch (e) { showAlert(String(e)); }
    $("login-go").disabled = false;
    $("login-go").textContent = "Log in";
  };
  $("logout").onclick = async () => { try { await api("POST", "/api/logout"); } catch (_) {} await render(false); };
  $("ins-channel").onchange = () => loadVersions().catch((e) => showAlert(String(e)));
  $("ins-disk").onchange = () => updateConfirmLabel();
  for (const id of ["ins-mode-pass", "ins-mode-config"])
    $(id).onchange = () => {
      const adv = $("ins-mode-config").checked;
      $("ins-pass-block").classList.toggle("d-none", adv);
      $("ins-keys-block").classList.toggle("d-none", adv);
      $("ins-config-block").classList.toggle("d-none", !adv);
    };
  $("ins-config-file").onchange = (ev) => {
    const f = ev.target.files[0]; if (!f) return;
    const rd = new FileReader();
    rd.onload = () => { $("ins-config").value = rd.result; };
    rd.readAsText(f);
  };
  $("ins-go").onclick = async () => {
    clearAlert();
    const body = {
      device: $("ins-disk").value, channel: $("ins-channel").value.trim() || "stable",
      ts: $("ins-ts").value.trim(), confirmDevice: $("ins-confirm").value,
    };
    if ($("ins-mode-config").checked) { body.mode = "config"; body.configYaml = $("ins-config").value; }
    else { body.mode = "password"; body.password = $("ins-pass").value; body.passwordConfirm = $("ins-pass2").value; body.sshPublicKeys = $("ins-keys").value; }
    try {
      const r = await api("POST", "/api/install", body);
      $("ins-pass").value = ""; $("ins-pass2").value = "";
      $("ins-intro").classList.add("d-none");
      $("ins-form").classList.add("d-none");
      $("ins-prog").classList.remove("d-none");
      $("ins-result").classList.add("d-none");
      pollJob(r.job, $("ins-log"), $("ins-bar"), (j) => {
        if (j.status === "done") { $("ins-result").classList.remove("d-none"); }
        else { showAlert(j.error || "Failed"); $("ins-intro").classList.remove("d-none"); $("ins-form").classList.remove("d-none"); }
      });
    } catch (e) { showAlert(String(e)); }
  };
  $("ins-reboot").onclick = async () => {
    $("ins-reboot").disabled = true;
    const msg = "Rebooting... the server will restart. Refresh this page to continue with the wizard; the rebooted node uses a new self-signed certificate, so accept the browser warning if shown.";
    // Once the reboot is underway the install view is over: hide its
    // sections, including the "Installer" title.
    const rebooting = () => {
      for (const id of ["ins-prog", "ins-title"]) $(id).classList.add("d-none");
      showAlert(msg, "info");
    };
    try {
      await api("POST", "/api/reboot", { confirm: true });
      rebooting();
    } catch (e) {
      // The node may go down before the 202 flushes: a
      // network-level failure after asking for reboot means
      // it is already on its way down. HTTP errors are real
      // failures instead.
      if (e instanceof TypeError) {
        rebooting();
      } else { showAlert(String(e)); $("ins-reboot").disabled = false; }
    }
  };
  for (const id of ["kube-mode-init", "kube-mode-join"])
    $(id).onchange = () => {
      const join = $("kube-mode-join").checked;
      $("pane-init").classList.toggle("d-none", join);
      $("pane-join").classList.toggle("d-none", !join);
      if (!join && !$("init-addr").value) prefillInitAddr().catch(() => {});
    };
  $("init-go").onclick = async () => {
    clearAlert();
    try {
      const r = await api("POST", "/api/kubeadm/init", { advertiseAddress: $("init-addr").value.trim(), hostname: $("init-host").value.trim() });
      $("kube-form").classList.add("d-none");
      $("kube-prog").classList.remove("d-none");
      $("kube-continue").classList.add("d-none");
      pollJob(r.job, $("kube-log"), null, (j) => {
        if (j.status === "done") { showAlert(j.message || "Done", "success"); $("kube-continue").classList.remove("d-none"); }
        else { showAlert(j.error || "Failed"); $("kube-form").classList.remove("d-none"); }
      });
    } catch (e) { showAlert(String(e)); }
  };
  $("join-go").onclick = async () => {
    clearAlert();
    let cfg;
    try { cfg = JSON.parse($("join-config").value); } catch (_) { showAlert("config is not valid JSON"); return; }
    try {
      const r = await api("POST", "/api/kubeadm/join", { config: cfg });
      $("kube-form").classList.add("d-none");
      $("kube-prog").classList.remove("d-none");
      $("kube-continue").classList.add("d-none");
      pollJob(r.job, $("kube-log"), null, (j) => {
        if (j.status === "done") { showAlert(j.message || "Done", "success"); $("kube-continue").classList.remove("d-none"); }
        else { showAlert(j.error || "Failed"); $("kube-form").classList.remove("d-none"); }
      });
    } catch (e) { showAlert(String(e)); }
  };
  $("kube-continue").onclick = async () => { await render(true); };
  $("tok-go").onclick = async () => {
    clearAlert(); $("tok-out").classList.add("d-none");
    try {
      const r = await api("POST", "/api/kubeadm/tokens", { role: $("tok-role").value, hostname: $("tok-host").value.trim() });
      $("tok-cmd").textContent = r.command;
      $("tok-json").textContent = r.joinJson;
      $("tok-exp").textContent = "Expires: " + (r.expires || "unknown");
      $("tok-out").classList.remove("d-none");
      try { await navigator.clipboard.writeText(r.command); } catch (_) { /* button fallback */ }
      await listTokens();
    } catch (e) { showAlert(String(e)); }
  };
  $("tok-copy").onclick = (ev) => copyText($("tok-cmd").textContent, ev.target);
  $("tok-copy-json").onclick = (ev) => copyText($("tok-json").textContent, ev.target);
  $("tok-list").onclick = () => listTokens().catch((e) => showAlert(String(e)));
  render(false);
});

async function listTokens() {
  const ul = $("tok-items"); ul.innerHTML = "";
  let doc = {};
  try { doc = await api("GET", "/api/kubeadm/tokens"); } catch (e) { showAlert(String(e)); return; }
  clearAlert();
  for (const it of (doc.tokens || [])) {
    const li = document.createElement("li");
    li.className = "list-group-item d-flex justify-content-between align-items-center";
    li.textContent = it.id + " — " + (it.description || "") + " (expires " + (it.expires || "?") + ")";
    const b = document.createElement("button");
    b.className = "btn btn-sm btn-outline-danger"; b.textContent = "Revoke";
    b.onclick = async () => { try { await api("DELETE", "/api/kubeadm/tokens/" + it.id); await listTokens(); } catch (e) { showAlert(String(e)); } };
    li.appendChild(b); ul.appendChild(li);
  }
}

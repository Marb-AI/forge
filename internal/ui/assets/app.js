"use strict";

// Forge browser UI.
//
// Terminals stream over SSE (output) and POST (input, resize) — no WebSocket.
// Mouse reports are just more input bytes, so Claude's clickable options work
// over the same POST path as typing.
//
// Two terminals can be live at once: the workspace's Claude session (the main
// stage, tmux-backed and persistent) and an ssh shell in an overlay panel that
// keeps running while hidden. Around them: a read-only file browser, the
// checkpoint/restart/stop actions, and a wizard that can register a server.

const state = {
  workspaces: [],
  active: null,   // workspace name
  claude: null,   // the Claude terminal session (main stage)
  ssh: null,      // the ssh shell session (overlay panel) — survives hiding
  reconnectOnEnd: false, // after restart/checkpoint the session ends then comes back
  openFiles: [],  // [{path, name}] open in the read-only viewer
  activeFile: null, // path shown in the viewer, or null (terminal visible)
  showHidden: false, // show dotfiles at the tree root
};

// ---- theme ----------------------------------------------------------------
function initTheme() {
  const saved = localStorage.getItem("forge-theme") || "dark";
  document.documentElement.dataset.theme = saved;
  applyHljsTheme();
}
// One way to change the theme, so the tab-bar toggle and the settings panel
// can't disagree about what's selected.
document.getElementById("theme-toggle").addEventListener("click", () => {
  setTheme(document.documentElement.dataset.theme === "dark" ? "light" : "dark");
});
function applyHljsTheme() {
  const dark = document.documentElement.dataset.theme === "dark";
  document.getElementById("hljs-theme").href =
    dark ? "/assets/vendor/hljs-dark.min.css" : "/assets/vendor/hljs-light.min.css";
}

// ---- base64 <-> utf8 bytes -------------------------------------------------
function b64encode(str) {
  const bytes = new TextEncoder().encode(str);
  let bin = "";
  for (let i = 0; i < bytes.length; i++) bin += String.fromCharCode(bytes[i]);
  return btoa(bin);
}
function b64decodeBytes(b64) {
  const bin = atob(b64);
  const bytes = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) bytes[i] = bin.charCodeAt(i);
  return bytes;
}

// ---- workspaces / tabs -----------------------------------------------------
async function loadWorkspaces() {
  try {
    const res = await fetch("/api/workspaces");
    if (!res.ok) throw new Error("HTTP " + res.status);
    state.workspaces = await res.json();
  } catch (e) {
    state.workspaces = [];
  }
  renderTabs();

  // With nothing to show, the terminal would be a black void — offer the one
  // action that makes sense instead.
  const empty = document.getElementById("empty");
  if (!state.workspaces.length) {
    empty.hidden = false;
    teardownTerminal();
    teardownSSH();
    hideSSHPanel();
    resetFiles();
    state.active = null;
    setStatus(null);
    return;
  }
  empty.hidden = true;
  if (!state.active) selectWs(state.workspaces[0].name);
}

function renderTabs() {
  const tabs = document.getElementById("tabs");
  tabs.innerHTML = "";
  for (const ws of state.workspaces) {
    const tab = document.createElement("button");
    tab.className = "tab" + (ws.name === state.active ? " active" : "") +
      (ws.status === "running" ? " running" : "");
    tab.title = ws.host + " · " + ws.status;

    // Built as nodes, like every other list here — no innerHTML, so no hand-rolled
    // escaping to get wrong later.
    const dot = document.createElement("span");
    dot.className = "dot";
    const label = document.createElement("span");
    label.textContent = ws.name;
    tab.append(dot, label);

    tab.addEventListener("click", () => selectWs(ws.name));
    tabs.appendChild(tab);
  }
}

function selectWs(name) {
  if (state.active === name && state.claude) return;
  state.active = name;
  renderTabs();
  resetFiles();
  // The ssh shell belongs to one workspace, so switching tabs drops it rather
  // than silently leaving you in the previous workspace's shell.
  teardownSSH();
  hideSSHPanel();
  openTerminal(name);
  loadTree(name);
}

function hideSSHPanel() {
  document.getElementById("sshpanel").hidden = true;
  document.querySelector('.rail-btn[data-action="ssh"]').classList.remove("active");
}

// ---- terminal --------------------------------------------------------------
function termTheme() {
  const dark = document.documentElement.dataset.theme === "dark";
  return dark
    ? { background: "#0a0a0a", foreground: "#e6e6e6", cursor: "#e6e6e6" }
    : { background: "#ffffff", foreground: "#1a1a1a", cursor: "#1a1a1a" };
}
function applyTermTheme() {
  for (const s of [state.claude, state.ssh]) {
    if (s) s.term.options.theme = termTheme();
  }
}

// A terminal session: one xterm bound to one server-side pty of a given kind
// ("claude" — the persistent tmux session, or "ssh" — a plain login shell).
function makeTerminal(ws, kind, el, onEnd) {
  const term = new Terminal({
    fontFamily: 'ui-monospace, SFMono-Regular, Menlo, Consolas, monospace',
    fontSize: 13,
    cursorBlink: true,
    scrollback: 5000,
    theme: termTheme(),
  });
  const fit = new FitAddon.FitAddon();
  term.loadAddon(fit);
  term.open(el);

  const sess = { ws, kind, el, term, fit, es: null, ro: null, ended: false, disposed: false };

  // Keystrokes AND mouse reports go the same way. When Claude enables mouse
  // tracking, xterm encodes clicks as escape sequences that arrive here just
  // like typing — so clicking Claude's options works over plain POST.
  term.onData((data) => postInput(ws, kind, data));
  term.onResize(({ cols, rows }) => postResize(ws, kind, cols, rows));

  const ro = new ResizeObserver(() => { try { fit.fit(); } catch (e) {} });
  ro.observe(el);
  sess.ro = ro;

  // Fit only after the browser has laid the container out — fitting too early
  // measures a zero/partial box and desyncs xterm from the pty. Once fitted we
  // know our real cols/rows and open the stream sized to match.
  requestAnimationFrame(() => {
    // Switching tabs fast can dispose this session before the frame lands.
    // Connecting anyway would open a stream nobody ever closes — and spawn an
    // orphan ssh + pty on the server for it.
    if (sess.disposed) return;
    try { fit.fit(); } catch (e) {}
    connectStream(sess, onEnd);
    term.focus();
  });
  return sess;
}

function connectStream(sess, onEnd) {
  if (sess.disposed) return;
  const url = `/api/term/${encodeURIComponent(sess.ws)}/${sess.kind}/stream` +
    `?cols=${sess.term.cols}&rows=${sess.term.rows}`;
  const es = new EventSource(url);
  sess.es = es;
  es.onmessage = (ev) => { if (ev.data) sess.term.write(b64decodeBytes(ev.data)); };
  es.addEventListener("end", () => {
    es.close();
    sess.ended = true;
    if (onEnd) onEnd();
  });
  es.onerror = () => { /* browser auto-reconnects; ignore transient errors */ };
}

function disposeTerminal(sess) {
  if (!sess) return;
  // Set first: a deferred connect (the rAF in makeTerminal) checks this and bails
  // out rather than opening a stream for a session that's already gone.
  sess.disposed = true;
  if (sess.es) sess.es.close();
  if (sess.ro) sess.ro.disconnect();
  if (sess.term) sess.term.dispose();
}

function postInput(ws, kind, data) {
  fetch(`/api/term/${encodeURIComponent(ws)}/${kind}/input`, {
    method: "POST",
    headers: { "Content-Type": "text/plain" },
    body: b64encode(data),
  }).catch(() => {});
}

function postResize(ws, kind, cols, rows) {
  fetch(`/api/term/${encodeURIComponent(ws)}/${kind}/resize`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ cols, rows }),
  }).catch(() => {});
}

// ---- the Claude terminal (main stage) --------------------------------------
function openTerminal(ws) {
  teardownTerminal();
  setStatus(null);
  state.claude = makeTerminal(ws, "claude", document.getElementById("terminal"), () => {
    // After a restart/checkpoint the old session is killed on purpose and a
    // fresh one is (being) started — reconnect to attach to it. Otherwise the
    // session genuinely ended (e.g. stop), so leave it and say so.
    if (state.reconnectOnEnd) {
      state.reconnectOnEnd = false;
      setStatus("Reconnecting to the fresh session…");
      setTimeout(() => { if (state.active === ws) openTerminal(ws); }, 1000);
    } else {
      setStatus("Session ended. Reselect the tab to reconnect.");
    }
  });
}

function teardownTerminal() {
  disposeTerminal(state.claude);
  state.claude = null;
}

// ---- the SSH shell (overlay panel) -----------------------------------------
// Hiding the panel does NOT close the shell — the stream stays open, so you keep
// your shell (and its cwd, and any running command). That's the Warp gripe fixed.
function ensureSSH(ws) {
  if (state.ssh && state.ssh.ws === ws && !state.ssh.ended) return;
  disposeTerminal(state.ssh);
  setSSHNote(null);
  state.ssh = makeTerminal(ws, "ssh", document.getElementById("sshterm"), () => {
    setSSHNote("Shell exited. Hide and reopen the panel to start a new one.");
  });
}

function teardownSSH() {
  disposeTerminal(state.ssh);
  state.ssh = null;
  setSSHNote(null);
}

function setSSHNote(msg) {
  const el = document.getElementById("ssh-note");
  if (!msg) { el.hidden = true; el.textContent = ""; return; }
  el.hidden = false;
  el.textContent = msg;
}

function setStatus(msg) {
  const el = document.getElementById("term-status");
  if (!msg) { el.hidden = true; el.textContent = ""; return; }
  el.hidden = false;
  el.textContent = msg;
}

// ---- right rail ------------------------------------------------------------
// The SSH panel is an overlay: toggling it must NOT resize the terminal, so we
// never call fit() here — the Claude terminal keeps its exact size underneath.
function toggleSSH(force) {
  const panel = document.getElementById("sshpanel");
  const open = force !== undefined ? force : panel.hidden;
  if (open && !state.active) return; // nothing to open a shell into

  panel.hidden = !open;
  document.querySelector('.rail-btn[data-action="ssh"]').classList.toggle("active", open);

  if (open) {
    document.getElementById("ssh-ws").textContent = state.active;
    ensureSSH(state.active);
    // The panel was display:none, so xterm couldn't measure itself — refit now
    // that it has a real box.
    requestAnimationFrame(() => {
      if (!state.ssh) return;
      try { state.ssh.fit.fit(); } catch (e) {}
      state.ssh.term.focus();
    });
  } else if (state.claude) {
    state.claude.term.focus(); // hidden, not closed: the shell keeps running
  }
}
document.getElementById("rail").addEventListener("click", (e) => {
  const btn = e.target.closest(".rail-btn");
  if (!btn) return;
  switch (btn.dataset.action) {
    case "ssh": toggleSSH(); break;
    case "settings": openSettings(); break;
    case "checkpoint": doCheckpoint(); break;
    case "restart": doRestart(); break;
    case "stop": doStop(); break;
  }
});

// ---- settings ---------------------------------------------------------------
// The port comes from the address we were loaded on, so this needs no endpoint.
function openSettings() {
  document.getElementById("set-port").textContent = location.host;
  syncThemeButtons();
  document.getElementById("settings").hidden = false;
}
function closeSettings() {
  document.getElementById("settings").hidden = true;
  if (state.claude) state.claude.term.focus();
}
function syncThemeButtons() {
  const dark = document.documentElement.dataset.theme === "dark";
  document.getElementById("set-dark").classList.toggle("active", dark);
  document.getElementById("set-light").classList.toggle("active", !dark);
}
function setTheme(theme) {
  document.documentElement.dataset.theme = theme;
  localStorage.setItem("forge-theme", theme);
  applyTermTheme();
  applyHljsTheme();
  syncThemeButtons();
}
document.getElementById("set-close").addEventListener("click", closeSettings);
document.getElementById("set-done").addEventListener("click", closeSettings);
document.getElementById("set-dark").addEventListener("click", () => setTheme("dark"));
document.getElementById("set-light").addEventListener("click", () => setTheme("light"));

// ---- Claude session actions -----------------------------------------------
// post() throws with the server's own message (the handlers explain themselves:
// "stop: ssh failed", "a checkpoint is already running") rather than a bare code.
async function post(action, ws) {
  const res = await fetch(`/api/ws/${encodeURIComponent(ws)}/${action}`, { method: "POST" });
  const data = await res.json().catch(() => ({}));
  if (!res.ok && res.status !== 202) {
    const err = new Error(data.error || "HTTP " + res.status);
    err.status = res.status;
    throw err;
  }
  return data;
}

async function doStop() {
  if (!state.active) return;
  const ws = state.active;
  if (!confirm(`Stop the Claude session for "${ws}"?\nThe session is killed and its context is lost.`)) return;

  state.reconnectOnEnd = false;
  setStatus("Stopping…");
  // Drop the terminal BEFORE killing the session: otherwise its stream ends
  // mid-request and the "session ended" handler races our own status message.
  teardownTerminal();
  try {
    await post("stop", ws);
    setStatus(`Claude session stopped. Reselect "${ws}" to start it again.`);
  } catch (e) {
    // The stop failed, so the session is still alive — put the terminal back
    // rather than leaving an empty stage over a session that never died.
    setStatus("Stop failed: " + e.message);
    if (state.active === ws) openTerminal(ws);
  }
  loadWorkspaces(); // refresh the tab's status dot either way
}

async function doRestart() {
  if (!state.active) return;
  const ws = state.active;
  if (!confirm(`Restart the Claude session for "${ws}"?\nThe current context is lost and a fresh session starts.`)) return;

  // The restart kills the session; the stream's "end" then reconnects us to the
  // fresh one.
  state.reconnectOnEnd = true;
  setStatus("Restarting…");
  try {
    await post("restart", ws);
    loadWorkspaces();
  } catch (e) {
    state.reconnectOnEnd = false;
    setStatus("Restart failed: " + e.message);
  }
}

async function doCheckpoint() {
  if (!state.active) return;
  const ws = state.active;
  if (!confirm(`Checkpoint "${ws}"?\nClaude writes a handoff to memory, then the session restarts with fresh context. Do this while Claude is idle.`)) return;

  // Checkpoint ends the session (after saving) and starts a fresh one, so the
  // stream's "end" should reconnect us rather than report a dead session.
  state.reconnectOnEnd = true;
  setStatus("Checkpoint starting…");
  try {
    const data = await post("checkpoint", ws); // throws with the server's message
    if (!data.id) throw new Error("server did not start the checkpoint");

    // Follow it: a checkpoint can fail outright (Claude busy) and that verdict
    // has to reach the user — otherwise "running…" hangs there forever.
    await followJob(data.id, (text) => setStatus(lastLine(text) || "Checkpoint running…"));
    setStatus("Handoff saved — restarting from memory…");
  } catch (e) {
    // It failed, so the session was NOT killed: clear the flag, or the next
    // "stop" would see an end event and helpfully restart what you just stopped.
    state.reconnectOnEnd = false;
    setStatus("Checkpoint failed: " + e.message);
  }
}

// followJob streams a long-running server job (prepare, checkpoint) and settles
// only on its verdict. onChunk receives the text accumulated so far.
function followJob(id, onChunk) {
  return new Promise((resolve, reject) => {
    const es = new EventSource(`/api/jobs/${encodeURIComponent(id)}/stream`);
    const dec = new TextDecoder(); // streaming: a rune split across chunks survives
    let text = "";
    let settled = false;

    es.onmessage = (ev) => {
      if (!ev.data) return;
      text += dec.decode(b64decodeBytes(ev.data), { stream: true });
      if (onChunk) onChunk(text);
    };
    es.addEventListener("done", (ev) => {
      es.close();
      if (settled) return;
      settled = true;
      let err = "";
      try { err = (JSON.parse(ev.data) || {}).error || ""; } catch (e) { /* none */ }
      if (err) reject(new Error(err));
      else resolve(text);
    });
    es.onerror = () => {
      // On an HTTP error (unknown/expired job) the browser closes for good rather
      // than retrying. Without this the promise would never settle and the caller
      // would hang on "running…" with its inputs disabled.
      if (es.readyState === EventSource.CLOSED && !settled) {
        settled = true;
        reject(new Error("lost the job stream"));
      }
    };
  });
}

function lastLine(text) {
  const lines = text.split("\n").map((l) => l.trim()).filter(Boolean);
  return lines.length ? lines[lines.length - 1] : "";
}
document.getElementById("ssh-close").addEventListener("click", () => toggleSSH(false));
document.addEventListener("keydown", (e) => {
  if (e.key !== "Escape") return;
  // Topmost layer wins: the modals, then the ssh overlay.
  if (!document.getElementById("wizard").hidden) { closeWizard(); return; }
  if (!document.getElementById("settings").hidden) { closeSettings(); return; }
  if (!document.getElementById("sshpanel").hidden) {
    toggleSSH(false);
    if (state.claude) state.claude.term.focus();
  }
});

document.getElementById("files-refresh").addEventListener("click", () => {
  if (state.active) loadTree(state.active);
});

// Eye: toggle root dotfiles. Purely a CSS class flip, so expanded folders keep
// their state (no reload).
function applyShowHidden() {
  document.getElementById("filetree").classList.toggle("show-hidden", state.showHidden);
  const btn = document.getElementById("files-hidden");
  btn.classList.toggle("active", state.showHidden);
  btn.title = state.showHidden ? "Hide hidden files (root only)" : "Show hidden files (root only)";
}
document.getElementById("files-hidden").addEventListener("click", () => {
  state.showHidden = !state.showHidden;
  localStorage.setItem("forge-show-hidden", state.showHidden ? "1" : "0");
  applyShowHidden();
});
// ---- new-workspace wizard --------------------------------------------------
const wiz = {
  el: () => document.getElementById("wizard"),
  name: () => document.getElementById("wiz-name"),
  host: () => document.getElementById("wiz-host"),
  err: () => document.getElementById("wiz-error"),
  create: () => document.getElementById("wiz-create"),
};

const NAME_RE = /^[A-Za-z0-9_-]{1,32}$/;
// "Register a new server" is an OPTION in the dropdown, not a mode that disables
// it — a greyed-out select with a separate toggle read as broken. Picking a real
// server hides the prepare fields; picking this shows them.
const NEW_HOST = "__new__";

document.getElementById("add-tab").addEventListener("click", openWizard);
document.getElementById("empty-create").addEventListener("click", openWizard);
document.getElementById("wiz-close").addEventListener("click", closeWizard);
document.getElementById("wiz-cancel").addEventListener("click", closeWizard);
document.getElementById("wiz-create").addEventListener("click", submitWizard);
wiz.host().addEventListener("change", syncHostMode);
// The "+" is just a shortcut to that option.
document.getElementById("wiz-addhost").addEventListener("click", () => {
  wiz.host().value = NEW_HOST;
  syncHostMode();
});
wiz.name().addEventListener("keydown", (e) => { if (e.key === "Enter") submitWizard(); });

function isNewHost() { return wiz.host().value === NEW_HOST; }

// Show the prepare fields exactly when the "new server" option is selected.
function syncHostMode() {
  const on = isNewHost();
  document.getElementById("wiz-newhost").hidden = !on;
  document.getElementById("wiz-addhost").classList.toggle("active", on);
  if (on) document.getElementById("wiz-target").focus();
}

async function openWizard() {
  wiz.name().value = "";
  document.getElementById("wiz-target").value = "";
  document.getElementById("wiz-alias").value = "";
  const log = document.getElementById("wiz-log");
  log.hidden = true;
  log.textContent = "";
  showWizError(null);
  setWizBusy(false);

  // With nothing registered, the only way forward is to register a server.
  await refreshHostOptions(null);

  wiz.el().hidden = false;
  wiz.name().focus();
}

// refreshHostOptions reloads the registered servers into the dropdown, always
// with the "register a new one" option last. select names the option to land on;
// null means "first server, or the new-server option if there are none".
async function refreshHostOptions(select) {
  const sel = wiz.host();
  sel.innerHTML = "";

  let hosts = [];
  try {
    const res = await fetch("/api/hosts");
    if (res.ok) hosts = await res.json();
  } catch (e) { /* treated as none */ }

  for (const h of hosts) {
    const opt = document.createElement("option");
    opt.value = h;
    opt.textContent = h;
    sel.appendChild(opt);
  }
  const opt = document.createElement("option");
  opt.value = NEW_HOST;
  opt.textContent = "＋  Register a new server…";
  sel.appendChild(opt);

  if (select && hosts.includes(select)) sel.value = select;
  else sel.value = hosts.length ? hosts[0] : NEW_HOST;
  syncHostMode();
}

function closeWizard() {
  wiz.el().hidden = true;
  if (state.claude) state.claude.term.focus();
}

function showWizError(msg) {
  const el = wiz.err();
  if (!msg) { el.hidden = true; el.textContent = ""; return; }
  el.hidden = false;
  el.textContent = msg;
}

function setWizBusy(busy, label) {
  wiz.create().disabled = busy;
  wiz.create().textContent = busy ? (label || "Working…") : "Create";
  for (const id of ["wiz-name", "wiz-host", "wiz-addhost", "wiz-target", "wiz-alias",
                    "wiz-firewall", "wiz-harden", "wiz-cancel"]) {
    document.getElementById(id).disabled = busy;
  }
}

// Create: in "+" mode this provisions the server first (streamed live), then
// creates the workspace on it — one button, two phases.
async function submitWizard() {
  const name = wiz.name().value.trim();
  if (!NAME_RE.test(name)) {
    showWizError("Workspace name must be 1-32 chars: letters, digits, dash or underscore.");
    return;
  }
  showWizError(null);

  try {
    let host;
    if (isNewHost()) {
      const target = document.getElementById("wiz-target").value.trim();
      const alias = document.getElementById("wiz-alias").value.trim();
      if (!target) throw new Error("SSH target required, e.g. root@1.2.3.4");
      if (!NAME_RE.test(alias)) {
        throw new Error("Alias must be 1-32 chars: letters, digits, dash or underscore.");
      }
      setWizBusy(true, "Preparing server…");
      await prepareHost(target, alias,
        document.getElementById("wiz-firewall").checked,
        document.getElementById("wiz-harden").checked);
      host = alias;
      // The server is registered now. Fold it into the dropdown and select it,
      // so if the workspace step fails, hitting Create again retries just that —
      // rather than re-running a several-minute prepare on a prepared server.
      await refreshHostOptions(alias);
    } else {
      host = wiz.host().value;
      if (!host) throw new Error("Pick a server.");
    }

    setWizBusy(true, "Creating workspace…");
    const res = await fetch("/api/workspaces", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ name, host }),
    });
    const data = await res.json().catch(() => ({}));
    if (!res.ok) throw new Error(data.error || "HTTP " + res.status);

    closeWizard();
    await loadWorkspaces();
    selectWs(name); // jump straight into the new workspace
  } catch (e) {
    showWizError(e.message);
  } finally {
    setWizBusy(false);
  }
}

// prepareHost runs `host prepare` server-side and streams its output into the
// wizard's log, resolving when it finishes. Same run you'd watch in a terminal.
async function prepareHost(target, alias, firewall, harden) {
  const log = document.getElementById("wiz-log");
  log.hidden = false;
  log.textContent = "";

  const res = await fetch("/api/hosts/prepare", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ target, alias, firewall, harden }),
  });
  const data = await res.json().catch(() => ({}));
  if (!res.ok && res.status !== 202) throw new Error(data.error || "HTTP " + res.status);
  if (!data.id) throw new Error("server did not start the prepare");

  await followJob(data.id, (text) => {
    log.textContent = text;
    log.scrollTop = log.scrollHeight;
  });
}

// ---- read-only file browser -----------------------------------------------
// Machinery directories hidden at EVERY level, not just the root — they're never
// what you open the browser to look at. The eye reveals them like any dotfile.
const GLOBAL_HIDDEN = new Set([".git", ".claude"]);

const SVG_ATTRS = 'viewBox="0 0 24 24" width="13" height="13" fill="none" stroke="currentColor" ' +
  'stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"';
const ICON_FOLDER = `<svg ${SVG_ATTRS}><path d="M22 19a2 2 0 0 1-2 2H4a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h5l2 3h9a2 2 0 0 1 2 2z"/></svg>`;
const ICON_FILE = `<svg ${SVG_ATTRS}><path d="M13 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V9z"/><polyline points="13 2 13 9 20 9"/></svg>`;
// A real chevron, rotated on expand — a 9px "▸" just read as a dot.
const ICON_CHEVRON = `<svg viewBox="0 0 24 24" width="11" height="11" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><polyline points="9 18 15 12 9 6"/></svg>`;

function resetFiles() {
  state.openFiles = [];
  state.activeFile = null;
  document.getElementById("filetabs").innerHTML = "";
  hideFileView();
  // Don't leave the pane blank — say why it's empty.
  const tree = document.getElementById("filetree");
  tree.innerHTML = '<div class="muted">Select a workspace.</div>';
}

async function fsList(ws, path) {
  try {
    const res = await fetch(`/api/fs/${encodeURIComponent(ws)}/list?path=${encodeURIComponent(path)}`);
    if (!res.ok) throw new Error("HTTP " + res.status);
    return (await res.json()).entries || [];
  } catch (e) { return null; }
}

async function loadTree(ws) {
  const root = document.getElementById("filetree");
  root.classList.remove("muted");
  root.innerHTML = '<div class="muted">Loading…</div>';
  const entries = await fsList(ws, "");
  if (entries === null) { root.innerHTML = '<div class="muted">Couldn\'t list files. Try refresh.</div>'; return; }
  root.innerHTML = "";
  root.appendChild(renderLevel(ws, "", entries));
}

function renderLevel(ws, base, entries) {
  const wrap = document.createElement("div");
  for (const e of entries) {
    const full = base ? base + "/" + e.name : e.name;
    const node = document.createElement("div");
    node.className = "tnode";

    // Root-level dotfiles are hideable (home is full of .cache/.ssh noise).
    // Deeper ones (.env, .github…) are real project files and always show —
    // except GLOBAL_HIDDEN, which is pure machinery at any depth.
    if (GLOBAL_HIDDEN.has(e.name) || (base === "" && e.name.startsWith("."))) {
      node.classList.add("dotfile");
    }

    const row = document.createElement("div");
    row.className = "trow " + (e.dir ? "dir" : "file");
    const tw = document.createElement("span");
    tw.className = "tw";
    tw.innerHTML = e.dir ? ICON_CHEVRON : "";
    const ti = document.createElement("span");
    ti.className = "ti";
    ti.innerHTML = e.dir ? ICON_FOLDER : ICON_FILE;
    const tn = document.createElement("span");
    tn.className = "tn";
    tn.textContent = e.name;
    row.append(tw, ti, tn);
    node.appendChild(row);

    if (e.dir) {
      const children = document.createElement("div");
      children.className = "tchildren";
      children.hidden = true;
      node.appendChild(children);
      row.addEventListener("click", async () => {
        if (!children.dataset.loaded) {
          const sub = await fsList(ws, full);
          if (sub) { children.appendChild(renderLevel(ws, full, sub)); children.dataset.loaded = "1"; }
        }
        const open = children.hidden;
        children.hidden = !open;
        row.classList.toggle("open", open); // rotates the chevron
      });
    } else {
      row.addEventListener("click", () => openFile(ws, full, e.name));
    }
    wrap.appendChild(node);
  }
  return wrap;
}

function openFile(ws, path, name) {
  if (!state.openFiles.some((f) => f.path === path)) state.openFiles.push({ path, name });
  state.activeFile = path;
  renderFileTabs(ws);
  showFile(ws, path);
}

function renderFileTabs(ws) {
  const bar = document.getElementById("filetabs");
  bar.innerHTML = "";
  for (const f of state.openFiles) {
    const tab = document.createElement("div");
    tab.className = "ftab" + (f.path === state.activeFile ? " active" : "");
    tab.title = f.path;
    const label = document.createElement("span");
    label.textContent = f.name;
    const x = document.createElement("span");
    x.className = "x";
    x.textContent = "✕";
    x.addEventListener("click", (ev) => { ev.stopPropagation(); closeFile(ws, f.path); });
    tab.append(label, x);
    tab.addEventListener("click", () => activateFile(ws, f.path));
    bar.appendChild(tab);
  }
}

function activateFile(ws, path) {
  if (state.activeFile === path) {
    // clicking the active tab flips back to the terminal
    state.activeFile = null;
    renderFileTabs(ws);
    hideFileView();
    if (state.claude) state.claude.term.focus();
    return;
  }
  state.activeFile = path;
  renderFileTabs(ws);
  showFile(ws, path);
}

function closeFile(ws, path) {
  const idx = state.openFiles.findIndex((f) => f.path === path);
  if (idx < 0) return;
  state.openFiles.splice(idx, 1);
  if (state.activeFile === path) {
    const next = state.openFiles[idx] || state.openFiles[idx - 1];
    state.activeFile = next ? next.path : null;
  }
  renderFileTabs(ws);
  if (state.activeFile) {
    showFile(ws, state.activeFile);
  } else {
    hideFileView();
    if (state.claude) state.claude.term.focus();
  }
}

async function showFile(ws, path) {
  const view = document.getElementById("fileview");
  const code = document.getElementById("fileview-code");
  const head = document.getElementById("fileview-head");
  view.hidden = false;
  head.textContent = path + "  ·  read-only";
  code.className = "hljs";
  code.textContent = "Loading…";
  try {
    const res = await fetch(`/api/fs/${encodeURIComponent(ws)}/read?path=${encodeURIComponent(path)}`);
    const data = await res.json().catch(() => ({}));
    // The server explains itself (deleted, binary, not a file) — show that
    // rather than a bare status code. A stale tree entry is a normal thing.
    if (!res.ok) throw new Error(data.error || "HTTP " + res.status);
    // Guard against a slower request for a since-switched file.
    if (state.activeFile !== path) return;
    code.textContent = data.content + (data.truncated ? "\n\n… (truncated)" : "");
    code.removeAttribute("data-highlighted");
    code.className = "";
    try { hljs.highlightElement(code); } catch (e) {}
  } catch (e) {
    code.textContent = "Failed to open: " + e.message;
  }
}

function hideFileView() {
  document.getElementById("fileview").hidden = true;
}


// ---- boot ------------------------------------------------------------------
initTheme();
state.showHidden = localStorage.getItem("forge-show-hidden") === "1";
applyShowHidden();
loadWorkspaces();

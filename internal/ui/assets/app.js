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
  hosts: [],       // registered servers, cached so Settings paints instantly
  active: null,   // workspace name
  claude: null,   // the Claude terminal session (main stage)
  ssh: null,      // the ssh shell session (overlay panel) — survives hiding
  reconnectOnEnd: false, // after restart/checkpoint the session ends then comes back
  openFiles: [],  // [{path, name}] open in the read-only viewer
  activeFile: null, // path shown in the viewer, or null (terminal visible)
  showHidden: false, // show dotfiles at the tree root
  stopped: false, // the active workspace has no Claude session running
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
// The single way the theme changes: the toggle, and the saved value at boot.
function setTheme(theme) {
  document.documentElement.dataset.theme = theme;
  localStorage.setItem("forge-theme", theme);
  applyTermTheme();
  applyHljsTheme();
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
  // Both, in parallel. /api/hosts is a local file and answers in about 5ms;
  // /api/workspaces goes over SSH to ask which Claude sessions are up and takes
  // half a second. Fetching the cheap one here keeps state.hosts warm, so Settings
  // can paint the truth immediately instead of painting "No servers registered."
  // and correcting itself half a second later — which is exactly the few-pixel
  // reflow this used to cause.
  const [ws, hosts] = await Promise.all([
    fetch("/api/workspaces").then((r) => (r.ok ? r.json() : [])).catch(() => []),
    fetch("/api/hosts").then((r) => (r.ok ? r.json() : [])).catch(() => []),
  ]);
  state.workspaces = ws;
  state.hosts = hosts;

  renderTabs();

  // With nothing to show, the terminal would be a black void — offer the one
  // action that makes sense instead.
  if (!state.workspaces.length) {
    teardownTerminal();
    teardownSSH();
    hideSSHPanel();
    resetFiles();
    state.active = null;
    state.stopped = false;
    setStatus(null);
    renderStage();
    return;
  }
  if (!state.active) selectWs(state.workspaces[0].name);
  else renderStage();
}


// The stage shows exactly one of: nothing to do, a stopped session, or the
// terminal. Keeping that in one place is what stops the three from fighting.
function renderStage() {
  const none = !state.workspaces.length;
  document.getElementById("empty").hidden = !none;

  const card = document.getElementById("stopped");
  card.hidden = none || !state.stopped;
  if (!none && state.stopped) paintStoppedCard();

  renderPowerButton();

  // Nothing to act on without a workspace the host has actually confirmed —
  // checkpoint/restart/ssh against a missing or unreachable one can only fail.
  const ws = state.workspaces.find((w) => w.name === state.active);
  const usable = !!ws && isUsable(ws.status);
  for (const b of document.querySelectorAll('.rail-btn[data-action]')) {
    if (b.dataset.action === "settings") continue; // settings always works
    b.disabled = !usable;
  }
}

// The card that stands in for the terminal. It has to say which of three different
// things is actually true — offering "Start Claude" for a workspace the host no
// longer has would only ssh into a user that doesn't exist.
function paintStoppedCard() {
  const ws = state.workspaces.find((w) => w.name === state.active);
  const status = ws ? ws.status : "stopped";
  const title = document.getElementById("stopped-title");
  const text = document.getElementById("stopped-text");
  const start = document.getElementById("stopped-start");

  if (status === "missing") {
    title.textContent = "Not on the server";
    text.textContent = `"${state.active}" is in your local config, but "${ws.host}" doesn't have it — ` +
      `it was most likely deleted from another machine. Remove it in Settings.`;
    start.hidden = true;
    return;
  }
  if (!isUsable(status)) {
    // Name the server, not just the workspace: knowing it's unreachable is no use
    // if you have to go and look up which machine to check.
    const host = ws ? ws.host : "it";
    title.textContent = "Server unreachable";
    text.textContent = `Can't reach "${host}", the server "${state.active}" lives on, ` +
      `so there is no telling whether Claude is running. Nothing has been changed.`;
    start.hidden = true;
    return;
  }
  title.textContent = "Session stopped";
  text.textContent = `Claude isn't running in "${state.active}". Its files are untouched — ` +
    `starting it again gives you a fresh session.`;
  start.hidden = false;
}

// The one rail button is stop or start, depending on what the session is doing —
// a "stop" you can press on a dead session is just a lie.
function renderPowerButton() {
  const b = document.getElementById("rail-power");
  const stopped = state.stopped;
  b.dataset.action = stopped ? "start" : "stop";
  b.querySelector(".ico").textContent = stopped ? "▶" : "■";
  b.querySelector(".cap").textContent = stopped ? "start" : "stop";
  b.title = stopped ? "Start the Claude session" : "Stop the Claude session";
}

// The status the agent reports is `tmux has-session -t claude` — it is the state
// of the CLAUDE SESSION, not of the workspace. A workspace can't be "stopped":
// it's a Linux user and a home directory, and it exists until you delete it.
// Saying "stopped" next to its name reads as though the whole thing is down.
function sessionLabel(status) {
  switch (status) {
    case "running": return "Claude running";
    case "stopped": return "Claude stopped";
    // Ours, per the config — but the host doesn't have it. Deleted from another
    // machine, most likely. Calling that "stopped" would be a lie you could act on.
    case "missing": return "not on the server";
    default: return "server unreachable";
  }
}

// Only a workspace the host confirmed can be started, attached to or browsed.
function isUsable(status) { return status === "running" || status === "stopped"; }

function renderTabs() {
  const tabs = document.getElementById("tabs");
  tabs.innerHTML = "";
  for (const ws of state.workspaces) {
    const active = ws.name === state.active;
    const tab = document.createElement("button");
    tab.className = "tab" + (active ? " active" : "") +
      (ws.status === "running" ? " running" : "");
    tab.title = `${ws.host} · ${sessionLabel(ws.status)}`;

    // Real tab semantics, since we claim role="tablist": screen readers get told
    // which one is selected, and a roving tabindex keeps Tab from walking through
    // every workspace — the arrow keys move between them instead.
    tab.setAttribute("role", "tab");
    tab.setAttribute("aria-selected", active ? "true" : "false");
    tab.tabIndex = active ? 0 : -1;

    // Built as nodes, like every other list here — no innerHTML, so no hand-rolled
    // escaping to get wrong later.
    const dot = document.createElement("span");
    dot.className = "dot";
    const label = document.createElement("span");
    label.textContent = ws.name;
    tab.append(dot, label);

    // Clicking a tab hands focus to the tab button, and a click on the tab you are
    // already on returns early from selectWs — so nothing takes the focus back and
    // your next keystrokes go to a <button> instead of Claude. Give the terminal
    // back its focus: clicking a workspace means "I want to be typing in it".
    tab.addEventListener("click", () => {
      selectWs(ws.name);
      state.claude?.term.focus();
    });
    tabs.appendChild(tab);
  }
}

// Arrow keys move between workspaces, Home/End jump to the ends — the keyboard
// contract a tablist promises.
document.getElementById("tabs").addEventListener("keydown", (e) => {
  const names = state.workspaces.map((w) => w.name);
  if (names.length < 2) return;

  const i = names.indexOf(state.active);
  let next = null;
  switch (e.key) {
    case "ArrowRight": next = names[(i + 1) % names.length]; break;
    case "ArrowLeft": next = names[(i - 1 + names.length) % names.length]; break;
    case "Home": next = names[0]; break;
    case "End": next = names[names.length - 1]; break;
    default: return;
  }
  e.preventDefault();
  selectWs(next);
  document.querySelector("#tabs .tab.active")?.focus();
});

function selectWs(name) {
  if (state.active === name && state.claude) return;
  state.active = name;
  renderTabs();
  resetFiles();
  // The ssh shell belongs to one workspace, so switching tabs drops it rather
  // than silently leaving you in the previous workspace's shell.
  teardownSSH();
  hideSSHPanel();

  // The terminal stream attaches-or-creates (like `forge workspace <name> claude`),
  // so opening it on a stopped workspace would quietly resurrect the session you
  // just stopped. Show the Start card instead and let the choice be yours.
  const ws = state.workspaces.find((w) => w.name === name);
  state.stopped = !ws || ws.status !== "running";
  teardownTerminal();
  if (!state.stopped) openTerminal(name);
  renderStage();

  // No point walking a tree on a host we can't reach, or in a home that is gone.
  if (ws && isUsable(ws.status)) loadTree(name);
  else document.getElementById("filetree").innerHTML =
    '<div class="muted">No files to show.</div>';
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
  for (const sess of [state.claude, state.ssh]) {
    if (sess) sess.term.options.theme = termTheme();
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

  // OSC 52 is the ONLY way text gets out of a session and into your clipboard.
  // The workspace is a headless Linux box: no X, no Wayland, no xclip — nothing
  // there has a clipboard to copy into. So everything that copies (Claude's
  // "press c" on the login URL, a tmux copy-mode yank, Claude writing a snippet)
  // hands the text to the *terminal* as an OSC 52 escape and trusts it to reach
  // you. xterm.js does not implement OSC 52; unhandled, it is dropped on the
  // floor. That is why "copied" appeared and nothing was ever copied — the
  // message was Claude reporting it had sent the escape, not the clipboard
  // confirming it arrived.
  term.parser.registerOscHandler(52, (payload) => {
    copyFromSession(payload);
    return true; // handled: never let it fall through and print as garbage
  });

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
      // The session is gone (stopped here, or killed from elsewhere). Say so with
      // the Start card rather than a status line nobody reads.
      teardownTerminal();
      state.stopped = true;
      setStatus(null);
      renderStage();
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

// flashStatus says something and then gets out of the way. Only for things the
// terminal already did — never for state you'd want to come back and read.
let statusTimer = null;
function flashStatus(msg, ms = 2000) {
  setStatus(msg);
  clearTimeout(statusTimer);
  statusTimer = setTimeout(() => setStatus(null), ms);
}

// copyFromSession handles an OSC 52 payload: "<selection>;<base64>", e.g. "c;aGk=".
// A payload of "?" is the terminal being *asked* for the clipboard's contents;
// we ignore it rather than answer, because a session that can read your clipboard
// on demand can read whatever you last copied — a password, a token — and Forge
// runs Claude in these sessions with permission prompts turned off.
//
// Anything a session writes is untrusted — Claude runs there unattended, and a
// runaway command's output is terminal output like any other. So the payload is
// capped before it is decoded: a copy is a URL, a snippet, a stack trace. A
// megabyte of base64 is not a copy, and decoding it to find that out is exactly
// the work we do not want to be tricked into.
const maxClipboardBytes = 1 << 20; // 1 MiB of base64, before decoding

function copyFromSession(payload) {
  const semi = payload.indexOf(";");
  if (semi < 0) return;
  const data = payload.slice(semi + 1);
  if (data === "?" || data === "") return;
  if (data.length > maxClipboardBytes) {
    flashStatus("Refused a clipboard payload over 1 MB");
    return;
  }
  let text;
  try {
    text = new TextDecoder().decode(b64decodeBytes(data));
  } catch (e) {
    return; // not base64: nothing we can put anywhere
  }
  writeClipboard(text);
}

// writeClipboard puts text on the *browser's* clipboard. The async Clipboard API
// is the real path (the UI is served from 127.0.0.1, which counts as a secure
// context, so it is available), but it can still be refused — Safari wants a
// recent user gesture, and an OSC 52 arriving from the server is not one. The
// old execCommand path is the fallback, and if even that is refused we say so:
// a copy that silently does nothing is the bug we are here to fix, and telling
// you "copied" when nothing was copied would just be the same lie in a new place.
function writeClipboard(text) {
  const fallback = () => {
    // The copy has to steal the focus — execCommand("copy") copies the selection,
    // so the text must really be selected in a really-rendered element. Give the
    // focus back afterwards: the copy was triggered from inside a session you are
    // typing in, and the keystroke after it belongs to Claude, not to the page.
    const focused = document.activeElement;
    const ta = document.createElement("textarea");
    ta.value = text;
    ta.style.position = "fixed";
    ta.style.opacity = "0";
    document.body.appendChild(ta);
    ta.select();
    let ok = false;
    try { ok = document.execCommand("copy"); } catch (e) { ok = false; }
    ta.remove();
    if (focused && focused.focus) focused.focus();
    else state.claude?.term.focus();
    flashStatus(ok ? "Copied to clipboard" : "Could not reach the clipboard — select the text and copy it yourself");
  };
  if (!navigator.clipboard || !navigator.clipboard.writeText) return fallback();
  navigator.clipboard.writeText(text).then(
    () => flashStatus("Copied to clipboard"),
    () => fallback(),
  );
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
    case "start": doStart(); break;
  }
});

// ---- settings: the administrative, mostly-irreversible stuff ----------------
// Theme lives in the tab bar; this panel is for the things you'd otherwise have
// to drop to the CLI for — and the things you should have to think about first.
async function openSettings() {
  setSettingsMsg(null);
  setSettingsError(null);
  document.getElementById("set-port").value = location.port || "";
  document.getElementById("settings").hidden = false;
  await renderAdminLists();
}

function closeSettings() {
  document.getElementById("settings").hidden = true;
  if (state.claude) state.claude.term.focus();
}

function setSettingsMsg(msg) {
  const el = document.getElementById("set-msg");
  if (!msg) { el.hidden = true; el.textContent = ""; return; }
  el.hidden = false;
  el.textContent = msg;
}
function setSettingsError(msg) {
  const el = document.getElementById("set-error");
  if (!msg) { el.hidden = true; el.textContent = ""; return; }
  el.hidden = false;
  el.textContent = msg;
}

// Paint from what the app already knows, then refresh in the background.
//
// /api/workspaces goes over SSH to ask the server which Claude sessions are up —
// half a second, sometimes more — while /api/hosts is a local file and answers in
// four milliseconds. Awaiting both meant the panel opened with the servers listed
// and the workspaces section conspicuously blank, filling in later. We already
// have the workspaces in hand, so there is no reason to make anyone watch that.
async function renderAdminLists() {
  paintAdminLists(state.workspaces, state.hosts);

  let workspaces = state.workspaces;
  let hosts = state.hosts;
  try {
    const [a, b] = await Promise.all([fetch("/api/workspaces"), fetch("/api/hosts")]);
    if (a.ok) workspaces = await a.json();
    if (b.ok) hosts = await b.json();
  } catch (e) { return; } // keep what we painted; it is the last thing we knew

  state.workspaces = workspaces;
  state.hosts = hosts;
  // Only repaint if the panel is still open — the fetch may outlive it.
  if (!document.getElementById("settings").hidden) paintAdminLists(workspaces, hosts);
}

function paintAdminLists(workspaces, hosts) {
  const wsBox = document.getElementById("set-workspaces");
  const hostBox = document.getElementById("set-hosts");
  wsBox.textContent = "";
  hostBox.textContent = "";

  if (!workspaces.length) wsBox.appendChild(mutedRow("No workspaces."));
  for (const w of workspaces) {
    wsBox.appendChild(adminRow(w.name, `${w.host} · ${sessionLabel(w.status)}`, "Delete", true,
      () => confirmDeleteWorkspace(w.name)));
  }
  if (!hosts.length) hostBox.appendChild(mutedRow("No servers registered."));
  for (const h of hosts) {
    hostBox.appendChild(adminRow(h, "", "Remove", false, () => confirmRemoveHost(h)));
  }
}

function mutedRow(text) {
  const d = document.createElement("div");
  d.className = "muted";
  d.textContent = text;
  return d;
}

function adminRow(name, meta, action, destructive, onClick) {
  const row = document.createElement("div");
  row.className = "adminrow";

  const left = document.createElement("div");
  const title = document.createElement("div");
  title.textContent = name;
  left.appendChild(title);
  if (meta) {
    const m = document.createElement("div");
    m.className = "meta";
    m.textContent = meta;
    left.appendChild(m);
  }

  const btn = document.createElement("button");
  btn.textContent = action;
  if (destructive) btn.classList.add("destructive");
  btn.addEventListener("click", onClick);

  row.append(left, btn);
  return row;
}

// Deleting a workspace runs `userdel -r` on the server: the user and its whole
// home go with it. A yes/no dialog is far too easy to click through for that, so
// you type the name.
async function confirmDeleteWorkspace(name) {
  const ok = await confirmAction({
    title: `Delete the workspace "${name}"?`,
    body: [
      { text: "This runs userdel -r on the server." },
      { text: "The workspace user and its ENTIRE HOME — every file, every repo, every uncommitted change in it — are permanently destroyed. Nothing undoes this.", warn: true },
    ],
    confirmLabel: "Delete forever",
    destructive: true,
    requireWord: name,
  });
  if (!ok) return;

  setSettingsError(null);
  setSettingsMsg(`Deleting "${name}"…`);
  try {
    const res = await fetch(`/api/workspaces/${encodeURIComponent(name)}`, { method: "DELETE" });
    const data = await res.json().catch(() => ({}));
    if (!res.ok) throw new Error(data.error || "HTTP " + res.status);

    setSettingsMsg(`Deleted "${name}".`);
    if (state.active === name) {
      teardownTerminal();
      teardownSSH();
      hideSSHPanel();
      resetFiles();
      state.active = null;
    }
    await loadWorkspaces();
    await renderAdminLists();
  } catch (e) {
    setSettingsMsg(null);
    setSettingsError("Delete failed: " + e.message);
  }
}

// Removing a server only makes Forge forget it — the machine and its workspaces
// are untouched — so a plain confirm is proportionate here.
async function confirmRemoveHost(alias) {
  const ok = await confirmAction({
    title: `Remove the server "${alias}"?`,
    body: [
      { text: "Forge forgets this server, and the workspaces it knows about there disappear from the UI." },
      { text: "The machine is NOT touched — those workspaces keep running on it, and `forge host add` brings it all back.", warn: false },
    ],
    confirmLabel: "Remove server",
    destructive: true,
  });
  if (!ok) return;

  setSettingsError(null);
  setSettingsMsg(`Removing "${alias}"…`);
  try {
    const res = await fetch(`/api/hosts/${encodeURIComponent(alias)}`, { method: "DELETE" });
    const data = await res.json().catch(() => ({}));
    if (!res.ok) throw new Error(data.error || "HTTP " + res.status);

    setSettingsMsg(`Removed "${alias}".`);
    await loadWorkspaces();
    await renderAdminLists();
  } catch (e) {
    setSettingsMsg(null);
    setSettingsError("Remove failed: " + e.message);
  }
}

async function saveUIPort() {
  const port = parseInt(document.getElementById("set-port").value, 10);
  setSettingsError(null);
  setSettingsMsg(null);
  try {
    const res = await fetch("/api/config/ui-port", {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ port }),
    });
    const data = await res.json().catch(() => ({}));
    if (!res.ok) throw new Error(data.error || "HTTP " + res.status);
    // This daemon is holding the old port, so it cannot move while it runs.
    setSettingsMsg(`Port saved as ${data.port}. Restart to apply: forge ui stop && forge ui`);
  } catch (e) {
    setSettingsError("Couldn't save the port: " + e.message);
  }
}

document.getElementById("set-close").addEventListener("click", closeSettings);
document.getElementById("set-done").addEventListener("click", closeSettings);
document.getElementById("set-port-save").addEventListener("click", saveUIPort);


// ---- confirm dialog ---------------------------------------------------------
// Our own, not the browser's: these actions destroy work in progress, and the
// native box can't explain what exactly is about to be lost — nor make you type
// a name when the thing at stake is a whole workspace.
//
// Returns a promise that resolves true only if the user really confirmed.
let cfResolve = null;

function confirmAction({ title, body, confirmLabel = "Confirm", destructive = false, requireWord = null }) {
  const modal = document.getElementById("confirm");
  // Only one dialog at a time. Opening a second over the first would strand the
  // first promise forever — its caller would sit there awaiting an answer that
  // can never arrive.
  if (!modal.hidden) return Promise.resolve(false);

  document.getElementById("cf-title").textContent = title;

  const bodyEl = document.getElementById("cf-body");
  bodyEl.textContent = "";
  for (const part of Array.isArray(body) ? body : [body]) {
    const p = document.createElement("div");
    if (typeof part === "object") {
      p.textContent = part.text;
      if (part.warn) p.className = "warn";
    } else {
      p.textContent = part;
    }
    bodyEl.appendChild(p);
  }

  const ok = document.getElementById("cf-ok");
  ok.textContent = confirmLabel;
  ok.classList.toggle("destructive", destructive);

  // Typing the name is the only guard proportionate to an irreversible delete.
  const typeBox = document.getElementById("cf-type");
  const input = document.getElementById("cf-input");
  typeBox.hidden = !requireWord;
  input.value = "";
  if (requireWord) {
    document.getElementById("cf-word").textContent = requireWord;
    ok.disabled = true;
    input.oninput = () => { ok.disabled = input.value.trim() !== requireWord; };
  } else {
    ok.disabled = false;
    input.oninput = null;
  }

  modal.hidden = false;
  (requireWord ? input : ok).focus();

  return new Promise((resolve) => { cfResolve = resolve; });
}

function closeConfirm(result) {
  document.getElementById("confirm").hidden = true;
  const resolve = cfResolve;
  cfResolve = null;
  if (resolve) resolve(result);
  if (!result && state.claude) state.claude.term.focus();
}

document.getElementById("cf-ok").addEventListener("click", () => closeConfirm(true));
document.getElementById("cf-cancel").addEventListener("click", () => closeConfirm(false));
document.getElementById("cf-x").addEventListener("click", () => closeConfirm(false));
document.getElementById("cf-input").addEventListener("keydown", (e) => {
  if (e.key === "Enter" && !document.getElementById("cf-ok").disabled) closeConfirm(true);
});

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
  const ok = await confirmAction({
    title: "Stop the Claude session?",
    body: [
      { text: `Claude is running in "${ws}". Stopping kills the session.`, warn: false },
      { text: "Whatever it was doing stops, and its context — everything you've said this session — is gone. The files on the server are untouched.", warn: true },
    ],
    confirmLabel: "Stop session",
    destructive: true,
  });
  if (!ok) return;

  state.reconnectOnEnd = false;
  setStatus("Stopping…");
  // Drop the terminal BEFORE killing the session: otherwise its stream ends
  // mid-request and the "session ended" handler races our own status message.
  teardownTerminal();
  try {
    await post("stop", ws);
    state.stopped = true;
    setStatus(null);
  } catch (e) {
    // The stop failed, so the session is still alive — put the terminal back
    // rather than leaving a Start card over a session that never died.
    setStatus("Stop failed: " + e.message);
    state.stopped = false;
    if (state.active === ws) openTerminal(ws);
  }
  renderStage();
  loadWorkspaces(); // refresh the tab's status dot either way
}

// Start is exactly what `forge workspace <name> claude` does: the terminal stream
// attaches or creates. No separate endpoint needed — the session comes up because
// we attach to it.
function doStart() {
  if (!state.active) return;
  const ws = state.active;
  state.stopped = false;
  state.reconnectOnEnd = false;
  renderStage();
  // No "starting…" message: openTerminal clears the status line anyway, and the
  // terminal itself appearing is the feedback.
  openTerminal(ws);
  // Give the session a moment to exist, then refresh the tab's status dot.
  setTimeout(() => { if (state.active === ws) loadWorkspaces(); }, 2000);
}

async function doRestart() {
  if (!state.active) return;
  const ws = state.active;
  const ok = await confirmAction({
    title: "Restart the Claude session?",
    body: [
      { text: `This kills Claude in "${ws}" and starts a brand-new session.`, warn: false },
      { text: "The current context is lost — nothing is saved first. If you want to keep what Claude knows, run Checkpoint instead.", warn: true },
    ],
    confirmLabel: "Restart session",
    destructive: true,
  });
  if (!ok) return;

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
  const ok = await confirmAction({
    title: "Checkpoint this session?",
    body: [
      { text: `Claude writes a handoff to its memory, then the session in "${ws}" restarts and continues from it with a fresh context.`, warn: false },
      { text: "Do this while Claude is idle — if it's mid-task the checkpoint refuses rather than interrupt it.", warn: false },
    ],
    confirmLabel: "Checkpoint",
  });
  if (!ok) return;

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
  // Topmost layer wins: the confirm dialog, then the other modals, then ssh.
  if (!document.getElementById("confirm").hidden) { closeConfirm(false); return; }
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
document.getElementById("stopped-start").addEventListener("click", doStart);
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
  // Reset the safety checkboxes too, not just the text. Otherwise unticking the
  // firewall once quietly leaves it unticked for the next server you register.
  for (const id of ["wiz-firewall", "wiz-harden", "wiz-prune"]) {
    document.getElementById(id).checked = true;
  }
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
  state.hosts = hosts;

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
                    "wiz-firewall", "wiz-harden", "wiz-prune", "wiz-cancel"]) {
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
        document.getElementById("wiz-harden").checked,
        document.getElementById("wiz-prune").checked);
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
async function prepareHost(target, alias, firewall, harden, dockerPrune) {
  const log = document.getElementById("wiz-log");
  log.hidden = false;
  log.textContent = "";

  const res = await fetch("/api/hosts/prepare", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ target, alias, firewall, harden, dockerPrune }),
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
    // A language mark where we have one (fileicons.js), the plain glyph otherwise,
    // so an unknown extension looks like a file rather than like a gap.
    const lang = e.dir ? "" : fileIconSVG(e.name);
    const ti = document.createElement("span");
    ti.className = lang ? "ti lang" : "ti";
    ti.innerHTML = e.dir ? ICON_FOLDER : (lang || ICON_FILE);
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

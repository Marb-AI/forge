"use strict";

// Forge browser UI.
//
// Terminals stream over SSE (output) and POST (input, resize) — no WebSocket.
// Mouse reports are just more input bytes, so Claude's clickable options work
// over the same POST path as typing.
//
// Two terminals can be live at once: the workspace's Claude session (the main
// stage, tmux-backed and persistent) and a shell in an overlay panel that keeps
// running while hidden — a workspace login shell ("ssh"), a shell on the host as
// its own login user ("host"), or a shell on this machine ("local"). Around them:
// a read-only file browser, the checkpoint/restart/stop actions, and a wizard
// that can register a server.

const state = {
  workspaces: [],
  hosts: [],       // registered servers, cached so Settings paints instantly
  active: null,   // workspace name
  claude: null,   // the Claude terminal session (main stage)
  shell: null,    // the shell session currently shown in the overlay panel, or null
  shellByKey: {}, // shellKey() -> its shell session; each survives tab switches
                  // and switching between the shells, so a shell you leave is
                  // exactly where you left it when you return
  panelKindByWs: {}, // ws -> which shell kind ("ssh"|"host"|"local") its panel had
                     // open, if any
  reconnectOnEnd: false, // after restart/checkpoint the session ends then comes back
  openFiles: [],  // [{path, name}] open in the read-only viewer
  activeFile: null, // path shown in the viewer, or null (terminal visible)
  showHidden: false, // show dotfiles at the tree root
  stopped: false, // the active workspace has no Claude session running
  // Why the terminal went away, which decides what the card says. The stream ends
  // the same way whether tmux died or the ssh link dropped, so an end we did not
  // ask for starts as "checking" and only becomes a verdict once we've asked the
  // host: "stopped" (Claude is really gone) or "lost" (Claude is fine, we aren't).
  endCause: "stopped", // "stopped" | "checking" | "lost"
  // The reattach loop that runs while the link is down. `busy` is what keeps a
  // slow attempt from overlapping the next one — see scheduleReconnect.
  reconnect: { timer: null, tries: 0, busy: false, pending: false },
  activity: {},   // ws name -> {state, ts, topic, topic_ts}: what Claude's up to, polled
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
// maxAge (seconds) says a recent answer is good enough. Only the reconnect probe
// passes it: it asks on a loop, and connectivity is a property of the SERVER, so
// the twentieth tab asking "is it back yet?" within the same few seconds should
// reuse the answer rather than buy another SSH handshake. Everything else — page
// load, and every refresh after you stop/start/restart something — leaves it off
// and gets a freshly measured status, because that is what you are about to act on.
async function loadWorkspaces({ maxAge = 0 } = {}) {
  const wsURL = maxAge > 0 ? `/api/workspaces?maxAge=${maxAge}` : "/api/workspaces";
  // Both, in parallel. /api/hosts is a local file and answers in about 5ms;
  // /api/workspaces goes over SSH to ask which Claude sessions are up and takes
  // half a second. Fetching the cheap one here keeps state.hosts warm, so Settings
  // can paint the truth immediately instead of painting "No servers registered."
  // and correcting itself half a second later — which is exactly the few-pixel
  // reflow this used to cause.
  const [ws, hosts] = await Promise.all([
    fetch(wsURL).then((r) => (r.ok ? r.json() : [])).catch(() => []),
    fetch("/api/hosts").then((r) => (r.ok ? r.json() : [])).catch(() => []),
  ]);
  state.workspaces = orderWorkspaces(ws);
  state.hosts = hosts;

  renderTabs();
  // The Claude panel groups the workspaces it knows as tabs, so a usage poll that
  // landed before this list did found nothing to group and left the panel empty.
  // Re-render it here: whichever of the two arrives second completes the picture.
  renderLogins();
  renderIdent();

  // With nothing to show, the terminal would be a black void — offer the one
  // action that makes sense instead.
  if (!state.workspaces.length) {
    teardownTerminal();
    teardownAllShells();
    resetFiles();
    state.active = null;
    state.stopped = false;
    setStatus(null);
    renderStage();
    return;
  }
  // A workspace deleted from another machine takes its (now-dead) shells with it.
  pruneShells();
  // If the tab we were on vanished from the refreshed list (deleted elsewhere,
  // host removed), don't cling to it: nothing would match state.active, the rail
  // would sit disabled, and the restored ssh wouldn't line up. Drop it so the
  // remembered-or-first workspace is selected instead.
  if (state.active && !state.workspaces.some((w) => w.name === state.active)) {
    state.active = null;
  }
  if (!state.active) selectWs(initialWorkspace());
  else renderStage();
}

// Where to land on a fresh page load: the tab you left, if it is still here.
// A refresh should drop you back where you were working, not at whichever
// workspace happens to sort first. If the remembered one is gone — deleted, or
// this is a first visit — fall back to the front of the list.
const ACTIVE_KEY = "forge-active-tab";
function initialWorkspace() {
  const saved = localStorage.getItem(ACTIVE_KEY);
  if (saved && state.workspaces.some((w) => w.name === saved)) return saved;
  return state.workspaces[0].name;
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
    // Settings always works: nothing in it is about the session in front of you.
    if (b.dataset.action === "settings") continue;
    // The local shell runs here, not on the server, so a host that stopped
    // answering is the moment you most want it — not a reason to grey it out. It
    // still opens in the workspace panel, so it needs a tab to open into.
    if (b.dataset.action === "local") { b.disabled = !ws; continue; }
    b.disabled = !usable;
  }

  renderHostShellButton();
}

// The host-shell rail button names the host's actual login user (root, or the
// sudo user the server was prepared as — it differs per host), since a fixed
// "root" would be a lie on a host reached as an ordinary user. Updated as the
// active workspace changes, because a different tab can live on a different host.
function renderHostShellButton() {
  const cap = document.getElementById("host-cap");
  const btn = document.querySelector('.rail-btn[data-action="host"]');
  if (!cap || !btn) return;
  const user = hostUserFor(state.active);
  cap.textContent = user || "host";
  btn.title = user
    ? `Open a shell on the server as ${user} (the host's login user)`
    : "Open a shell on the server as its login user";
}

// The card that stands in for the terminal. It has to say which of several
// different things is actually true — offering "Start Claude" for a workspace the
// host no longer has would only ssh into a user that doesn't exist, and offering
// it for a session that is still running would be flatly wrong.
function paintStoppedCard() {
  const ws = state.workspaces.find((w) => w.name === state.active);
  const status = ws ? ws.status : "stopped";
  const title = document.getElementById("stopped-title");
  const text = document.getElementById("stopped-text");
  const start = document.getElementById("stopped-start");
  start.dataset.action = "start";
  start.textContent = "▶  Start Claude";

  // The stream just dropped and we haven't heard back from the host yet. Claiming
  // "stopped" here is a guess, and the wrong one whenever the link is what broke.
  if (state.endCause === "checking") {
    title.textContent = "Connection lost";
    text.textContent = `The connection to "${state.active}" dropped. ` +
      `Checking whether Claude is still running…`;
    start.hidden = true;
    return;
  }
  // The host says the session is up: it was the link between this browser and the
  // server that went — a network blip, or the laptop sleeping. Nothing was
  // interrupted, so reattach; do NOT offer to "start" what never stopped.
  if (state.endCause === "lost") {
    title.textContent = "Connection lost";
    text.textContent = `Claude is still running in "${state.active}" — it was the connection ` +
      `from this browser that dropped, not the session. Nothing was interrupted; ` +
      `reattaching automatically.`;
    start.dataset.action = "reconnect";
    start.textContent = "⟲  Reconnect now";
    start.hidden = false;
    return;
  }

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
      `so there is no telling whether Claude is running. Nothing has been changed.` +
      // A server that is down comes back, and when it does we reattach on our own.
      // Saying so is the difference between "wait" and "go and fix something".
      (reconnecting() ? ` Retrying until it answers.` : ``);
    // Retrying, so offer to skip the wait — but never a "Start", which would be a
    // claim about a session no one can currently see.
    start.dataset.action = "reconnect";
    start.textContent = "⟲  Retry now";
    start.hidden = !reconnecting();
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

// ---- tab order -------------------------------------------------------------
// The server lists workspaces alphabetically, which is a fine default and a poor
// permanent arrangement: the tabs you keep are the ones you work in, not the ones
// whose names sort first. So the order you drag them into is yours, and it lives
// here in the browser — there is nothing about "the tabs I like left-to-right"
// that belongs on the host, and every machine you open the UI from gets to
// disagree about it.
const ORDER_KEY = "forge-tab-order";

function savedOrder() {
  try {
    const v = JSON.parse(localStorage.getItem(ORDER_KEY) || "[]");
    return Array.isArray(v) ? v.filter((n) => typeof n === "string") : [];
  } catch { return []; }
}

// Sort by the saved order; anything the saved order has never seen — a workspace
// just created, or one created on another machine — keeps its server position at
// the end, so new tabs appear rather than silently landing in the middle.
// Every workspace gets a real number for a rank — a saved one its saved position,
// an unseen one its server position pushed past the end of the saved list. Two
// unknowns then compare by where the server put them, which is the alphabetical
// order we wanted, rather than by whatever a comparator returning Infinity minus
// Infinity happens to mean to the engine.
function orderWorkspaces(list) {
  const order = savedOrder();
  const rank = new Map(order.map((n, i) => [n, i]));
  return list
    .map((ws, i) => ({ ws, r: rank.has(ws.name) ? rank.get(ws.name) : order.length + i }))
    .sort((a, b) => a.r - b.r)
    .map((x) => x.ws);
}

// Written from the tab strip's own DOM after a drag, so what you see is what gets
// stored. Deleted workspaces fall out of the list here rather than accumulating.
function saveOrder() {
  const names = [...document.querySelectorAll("#tabs .tab")].map((t) => t.dataset.name);
  state.workspaces = orderBy(names, state.workspaces);
  localStorage.setItem(ORDER_KEY, JSON.stringify(names));
}

function orderBy(names, list) {
  const byName = new Map(list.map((w) => [w.name, w]));
  return names.map((n) => byName.get(n)).filter(Boolean);
}

// The tab the pointer is currently to the left of: the first one whose horizontal
// midpoint we haven't passed yet. Null means "past them all" — append.
function tabBefore(tabs, x) {
  for (const t of tabs.querySelectorAll(".tab:not(.dragging)")) {
    const r = t.getBoundingClientRect();
    if (x < r.left + r.width / 2) return t;
  }
  return null;
}

// Drag to reorder. The dragged tab moves through the DOM as you pass each
// neighbour's midpoint, so the strip rearranges under the cursor and the drop is
// just where you let go — no drop indicator to interpret, no jump at the end.
function initTabDrag() {
  const tabs = document.getElementById("tabs");

  tabs.addEventListener("dragover", (e) => {
    const dragging = tabs.querySelector(".tab.dragging");
    if (!dragging) return;
    e.preventDefault(); // without this the drop is refused and the drag "snaps back"
    e.dataTransfer.dropEffect = "move";
    const before = tabBefore(tabs, e.clientX);
    if (before === dragging) return;
    if (before) tabs.insertBefore(dragging, before);
    else tabs.appendChild(dragging);
  });

  // Let go anywhere — over the strip, or off it. The tabs are already sitting where
  // the drag left them, so both paths commit the same order; dragend always fires.
  tabs.addEventListener("drop", (e) => e.preventDefault());
}

function renderTabs() {
  const tabs = document.getElementById("tabs");
  tabs.innerHTML = "";
  for (const ws of state.workspaces) {
    const active = ws.name === state.active;
    const tab = document.createElement("button");
    tab.className = "tab" + (active ? " active" : "") +
      (ws.status === "running" ? " running" : "");
    // The topic goes in the tooltip rather than the strip: it is a sentence, and a
    // sentence per tab would make twenty workspaces unreadable at the one moment
    // you have twenty. Hovering is the cheap way to ask "which one was that".
    const topic = topicFor(ws.name);
    tab.title = `${ws.host} · ${sessionLabel(ws.status)}` + (topic ? `\n${topic.text}` : "");

    // Real tab semantics, since we claim role="tablist": screen readers get told
    // which one is selected, and a roving tabindex keeps Tab from walking through
    // every workspace — the arrow keys move between them instead.
    tab.setAttribute("role", "tab");
    tab.setAttribute("aria-selected", active ? "true" : "false");
    tab.tabIndex = active ? 0 : -1;

    // Reordering: the name is what the order is stored by, and dataTransfer must
    // carry something or Firefox won't start the drag at all.
    tab.dataset.name = ws.name;
    tab.draggable = true;
    tab.addEventListener("dragstart", (e) => {
      e.dataTransfer.effectAllowed = "move";
      e.dataTransfer.setData("text/plain", ws.name);
      // Deferred: setting it now would be captured in the drag image, and the tab
      // you're dragging would be the one that looks faded out from under itself.
      requestAnimationFrame(() => tab.classList.add("dragging"));
    });
    tab.addEventListener("dragend", () => {
      tab.classList.remove("dragging");
      saveOrder();
    });

    // Built as nodes, like every other list here — no innerHTML, so no hand-rolled
    // escaping to get wrong later.
    const dot = document.createElement("span");
    dot.className = "dot";
    const label = document.createElement("span");
    label.textContent = ws.name;
    tab.append(dot, label);

    // A workspace where Claude is waiting for you gets a mark you can spot from
    // another tab. It clears the moment you look (see wantsYou / ackActivity).
    if (wantsYou(ws.name)) {
      tab.classList.add("attn");
      const mark = document.createElement("span");
      mark.className = "attn-mark";
      mark.textContent = "✳";
      mark.title = "Claude is waiting for you";
      tab.insertBefore(mark, label);
    }

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
  attnSig = attnSignature();
  paintBrowserTab();
}

// ---- Claude activity: the idle indicator -----------------------------------
// The tabs can tell you Claude is waiting in a workspace you aren't looking at.
// The state comes from Claude Code hooks on the host (Stop -> idle, Notification
// -> waiting, UserPromptSubmit -> busy), recorded per workspace and polled here.
// "Seen" is remembered per workspace by the timestamp of the episode you looked
// at, so a mark clears when you view the tab and lights again on the next Stop —
// exactly like a notification, and surviving a reload.
const ACK_KEY = "forge-activity-ack";
function activityAcks() {
  try {
    const v = JSON.parse(localStorage.getItem(ACK_KEY) || "{}");
    return v && typeof v === "object" ? v : {};
  } catch { return {}; }
}
function ackActivity(ws) {
  const a = state.activity[ws];
  if (!a) return;
  const acks = activityAcks();
  if (acks[ws] === a.ts) return;
  acks[ws] = a.ts;
  localStorage.setItem(ACK_KEY, JSON.stringify(acks));
}
// Claude wants you here: it finished (idle) or needs a decision (waiting), and
// this is a newer moment than the one you last acknowledged. The active tab never
// flags — looking at it IS acknowledging it. A workspace you've never
// acknowledged always flags, so a missing timestamp (ts 0 — the hooks normally
// stamp one, but tolerate its absence) still lights the tab once instead of
// staying dark forever because 0 isn't greater than 0.
function wantsYou(ws) {
  // The active tab doesn't flag — looking at it IS acknowledging it. But once the
  // whole forge tab is hidden you aren't looking at ANY of them, so even the active
  // workspace becomes eligible: that's how "I left forge and my session finished"
  // reaches the browser tab and an OS toast.
  if (ws === state.active && !document.hidden) return false;
  const a = state.activity[ws];
  if (!a || (a.state !== "idle" && a.state !== "waiting")) return false;
  const acked = activityAcks()[ws];
  return acked === undefined || a.ts > acked;
}
function attnSignature() {
  return state.workspaces.filter((w) => wantsYou(w.name)).map((w) => w.name).join("|");
}
function wanters() {
  return state.workspaces.filter((w) => wantsYou(w.name)).map((w) => w.name);
}

// ---- browser-tab attention -------------------------------------------------
// Carry the "Claude wants you" signal all the way out to the browser tab, so you
// can be off on another tab (or another app) and still notice. Three layers:
// the tab title gets a dot, the favicon gets a badge, and — if you've allowed it —
// an OS notification pops when a workspace finishes while you're looking elsewhere.
const faviconLink = document.querySelector('link[rel="icon"]');
const faviconPlainHref = faviconLink ? faviconLink.getAttribute("href") : null;
let faviconBadgedHref = null;

// Build the badged favicon from the real anvil (fetched, so it tracks the actual
// icon instead of a copy): drop an amber dot in the corner, same colour as the ✳.
(async function buildBadgedFavicon() {
  if (!faviconPlainHref) return;
  try {
    const res = await fetch(faviconPlainHref);
    if (!res.ok) return; // a 404/HTML page would make a garbage data-URI icon
    const svg = await res.text();
    // Insert before the closing tag, matched case-insensitively; if there isn't a
    // recognisable </svg>, leave the badge off rather than build a broken icon.
    if (!/<\/svg\s*>/i.test(svg)) return;
    const dot = '<circle cx="24" cy="8" r="7" fill="#f5a623" stroke="#0a0a0a" stroke-width="1.5"/>';
    const badged = svg.replace(/<\/svg\s*>/i, dot + "</svg>");
    faviconBadgedHref = "data:image/svg+xml," + encodeURIComponent(badged);
  } catch {}
})();

// Title + favicon reflect whether ANY workspace wants you. Idempotent and cheap,
// so it's safe to call from every tab repaint.
function paintBrowserTab() {
  const n = wanters().length;
  document.title = n ? (n > 1 ? `● Forge (${n})` : "● Forge") : "Forge";
  if (faviconLink && faviconBadgedHref) {
    faviconLink.setAttribute("href", n ? faviconBadgedHref : faviconPlainHref);
  }
}

// One-time, on the first click (a user gesture — browsers require one): if you
// haven't decided yet, ask whether Forge may show OS notifications. Deny and the
// title/favicon still work; nothing else changes.
document.addEventListener("click", () => {
  if (!("Notification" in window) || Notification.permission !== "default") return;
  // Older implementations return undefined (callback style) or can throw rather
  // than reject, so don't assume a Promise — guard the .catch and the call itself,
  // or a denied/legacy browser would break this click handler.
  try {
    const req = Notification.requestPermission();
    if (req && typeof req.catch === "function") req.catch(() => {});
  } catch {}
}, { once: true });

// Edge-triggered OS toast: notify only for a workspace that has NEWLY started
// wanting you since the last poll, and only while you're looking elsewhere (a
// visible forge tab already shows the mark). Clicking the toast brings you here
// and opens that workspace.
let prevWanters = new Set();
function maybeNotify() {
  const now = wanters();
  if ("Notification" in window && Notification.permission === "granted" && document.hidden) {
    for (const ws of now) {
      if (!prevWanters.has(ws)) {
        const note = new Notification("Claude is waiting for you", { body: ws, tag: "forge-" + ws });
        note.onclick = () => { window.focus(); selectWs(ws); note.close(); };
      }
    }
  }
  prevWanters = new Set(now);
}

let attnSig = "";
async function pollActivity() {
  // Poll even while the tab is hidden — that's exactly when the OS toast earns its
  // keep (you're off on another tab and a workspace finishes). The browser throttles
  // hidden-tab timers to about once a minute on its own, so the SSH churn while away
  // stays small; when visible it runs at the full interval.
  if (!state.workspaces.length) return;
  let act;
  try {
    act = await fetch("/api/activity").then((r) => (r.ok ? r.json() : null));
  } catch { return; }
  if (!act) return;
  state.activity = act;
  renderTopic();
  renderIdent();
  if (state.active && !document.hidden) ackActivity(state.active); // you're looking at it
  // Repaint the tabs only when the flagged set changed, and never mid-drag — a
  // reorder owns the strip. paintBrowserTab rides along inside renderTabs; call it
  // directly too so the title/favicon still update on a hidden tab that isn't
  // otherwise re-rendering.
  if (attnSignature() !== attnSig && !document.querySelector(".tab.dragging")) {
    renderTabs();
  } else {
    paintBrowserTab();
  }
  maybeNotify();
}
setInterval(pollActivity, 4000);
document.addEventListener("visibilitychange", () => {
  if (document.visibilityState === "visible") pollActivity();
});

// ---- session + activity tracking -------------------------------------------
// Two per-workspace clocks in a banner above the file tree.
//
// Session clock: how long the active workspace's Claude session has run. The
// server owns it (tmux session_created, held across a checkpoint, reset on stop/
// restart); we just tick wall-time from the start it reports.
//
// Activity clock: how long YOU have been present at the active workspace this
// session. We accumulate it here — a second per second while the conditions hold —
// and flush the arrears to the server so it survives a reload or a dropped link.
// It only ever runs for the active workspace, and only while its session is up:
// user activity is a subset of the Claude session.
//
// It counts while: the workspace is active AND its session is running AND the forge
// tab is visible AND the window is focused AND you haven't paused. Switching
// workspace hands the clock to the other one; leaving the page (tab/window/app) or
// the pause button stops it. ANY interaction on the page clears a manual pause —
// the button is a one-shot "I'm stepping away", and your next click resumes it.
const TRACK_HIDE_KEY = "forge-hide-tracking";
const track = {
  data: {},                                   // ws -> {session_start, active_seconds} (server)
  pending: {},                                // ws -> locally accrued, not-yet-flushed seconds
  inflight: {},                               // ws -> a flush is mid-POST; guards against double-count
  paused: false,                              // manual pause; any interaction clears it
  hasFocus: typeof document !== "undefined" ? document.hasFocus() : true,
  hidden: localStorage.getItem(TRACK_HIDE_KEY) === "1",
};

// Whether the activity clock is accruing right now, for the active workspace. It
// needs a session the server is actually tracking (track.data) — so a host whose
// agent predates this feature never silently piles up seconds it can't flush.
function trackingLive() {
  return !!state.active && !state.stopped && !!track.data[state.active] &&
    !document.hidden && track.hasFocus && !track.paused;
}

function fmtDur(s) {
  s = Math.max(0, Math.floor(s));
  const h = Math.floor(s / 3600), m = Math.floor((s % 3600) / 60), sec = s % 60;
  const pad = (n) => String(n).padStart(2, "0");
  return h > 0 ? `${h}:${pad(m)}:${pad(sec)}` : `${m}:${pad(sec)}`;
}

// The displayed numbers for a workspace: session = wall-time since start; active =
// the server's count plus whatever we've accrued locally since the last flush.
function trackNumbers(ws) {
  const d = ws ? track.data[ws] : null;
  if (!d || !d.session_start) return null;
  const nowSec = Math.floor(Date.now() / 1000);
  return {
    session: Math.max(0, nowSec - d.session_start),
    active: (d.active_seconds || 0) + (track.pending[ws] || 0),
  };
}

function trackCopyText() {
  const n = trackNumbers(state.active);
  if (!n) return "";
  const now = new Date();
  const pad = (x) => String(x).padStart(2, "0");
  const date = `${now.getFullYear()}-${pad(now.getMonth() + 1)}-${pad(now.getDate())}`;
  // No workspace name: the names are internal pseudonyms and don't travel; the copy
  // is what makes the numbers portable into a task in Linear/Jira/etc.
  return `Session ${fmtDur(n.session)} · Active ${fmtDur(n.active)} · ${date}`;
}

function renderTrackBanner() {
  const banner = document.getElementById("track-banner");
  const n = trackNumbers(state.active);
  const have = !!n;                            // there's a running session to show
  banner.hidden = !have;
  if (!have) return;

  // The eye hides the NUMBERS, and nothing else. Tracking goes on, the buttons
  // stay where they are, and the way back is the same eye you pressed — which is
  // the whole point: it is for not watching the counter, not for turning the
  // feature off.
  banner.classList.toggle("numbers-hidden", track.hidden);
  const hide = document.getElementById("track-hide");
  hide.title = track.hidden
    ? "Show the times — tracking never stopped"
    : "Hide the times — tracking keeps running";
  hide.classList.toggle("active", track.hidden);

  banner.classList.toggle("live", trackingLive());
  banner.classList.toggle("paused", track.paused);
  document.getElementById("track-session").textContent = fmtDur(n.session);
  document.getElementById("track-active").textContent = fmtDur(n.active);
  const pause = document.getElementById("track-pause");
  // innerHTML, not textContent: the button holds an icon, and assigning text to it
  // would replace the SVG with a glyph the first time tracking was paused.
  pause.innerHTML = track.paused ? ICON_PLAY : ICON_PAUSE;
  pause.title = track.paused ? "Resume activity tracking" : "Pause activity tracking";
}

// ---- workspace topic --------------------------------------------------------
// What the workspace is working on, in Claude's own words. Claude writes it with
// `forge-topic`; a UserPromptSubmit hook asks it to whenever the label is missing
// or predates the current session. Nobody types one by hand — which is the only
// way twenty of them stay current.

function topicFor(ws) {
  const a = ws ? state.activity[ws] : null;
  return a && a.topic ? { text: a.topic, ts: a.topic_ts || 0 } : null;
}

// Coarse on purpose: the only question an age answers here is "is this still what's
// going on", and "3d" answers it as well as a timestamp would, in a corner of a
// narrow pane.
function fmtAge(sec) {
  if (sec < 90) return "now";
  if (sec < 3600) return `${Math.round(sec / 60)}m`;
  if (sec < 86400) return `${Math.round(sec / 3600)}h`;
  return `${Math.round(sec / 86400)}d`;
}

function renderTopic() {
  const box = document.getElementById("ws-topic");
  const t = topicFor(state.active);
  box.hidden = !t;
  if (!t) return;
  const age = Math.max(0, Math.floor(Date.now() / 1000) - t.ts);
  // "Stale" means here what it means to the hook that nudges for a new one: the
  // label predates the session it claims to describe. A workspace with no running
  // session is all history, so it counts too — dimmed, not hidden, because what it
  // was last doing is exactly what you came to the tab to remember.
  const start = (track.data[state.active] || {}).session_start || 0;
  box.classList.toggle("stale", !start || (t.ts > 0 && t.ts < start));
  document.getElementById("ws-topic-text").textContent = t.text;
  document.getElementById("ws-topic-age").textContent = t.ts ? fmtAge(age) : "";
  box.title = t.ts ? `${t.text}\nSet ${fmtAge(age)} ago by Claude.` : t.text;
}

// ---- workspace identity (login, server, context) ----------------------------
// The topic says what this workspace is doing. These say where it is doing it —
// whose Claude allowance it spends, whose disk it fills — and how full its context
// window is. All three are properties of THIS workspace, which is why they live up
// here with the topic rather than in the panel below: the context window is per
// session, and a global view of it would be a number about nothing.
//
// The server is known from the workspace list, so it is there immediately. The
// login and the context come from the host, so they appear when the first usage
// poll lands.
function renderIdent() {
  const box = document.getElementById("ws-ident");
  const serverLine = document.getElementById("ws-line-server");
  const loginLine = document.getElementById("ws-line-login");
  const serverEl = document.getElementById("ws-server");
  const loginEl = document.getElementById("ws-login");
  const ctxEl = document.getElementById("ws-ctx");
  const ws = state.workspaces.find((w) => w.name === state.active);
  const u = state.active ? usage.data[state.active] : null;

  // Percentage only, and no bar: this is the one number, and it shares a line
  // with a server name that also has to fit.
  const ctx = contextPercent(u);
  ctxEl.hidden = ctx == null;
  if (ctx != null) ctxEl.textContent = `(${Math.round(ctx)}% context)`;

  const host = ws ? ws.host : "";
  serverLine.hidden = !host;
  if (host) {
    serverEl.textContent = host;
    // Everything the line had to truncate, plus what it never had room for. The
    // context lives here too: the line says the number, the tooltip says of what.
    const lines = [`Runs on ${host}` + (ws.host_user ? ` (as ${ws.host_user})` : "")];
    if (ctx != null) {
      lines.push(`Context window: ${fmtTokens(u.context_used)} of ${fmtTokens(u.context_size)} tokens` +
        (u.model ? ` · ${u.model}` : ""));
      lines.push("Resets when the session is compacted or restarted.");
    }
    serverLine.title = lines.join("\n");
  }

  loginLine.hidden = !u;
  if (u) {
    const account = u.account || {};
    loginEl.textContent = loginLabel(u);
    loginLine.classList.toggle("dim", !account.uuid);
    const lines = [account.uuid ? `Claude login: ${loginLabel(u)}` : `No Claude login on this workspace.`];
    if (account.org) lines.push(account.org);
    if (u.auth) lines.push(`Paying by: ${AUTH_LABELS[u.auth] || u.auth}`);
    if (u.note) lines.push(u.note);
    loginLine.title = lines.join("\n");
  }
  box.hidden = serverLine.hidden && loginLine.hidden;
}

// Token counts are for the tooltip, where "128k of 200k" answers the question the
// percentage can't: how much room is actually left.
function fmtTokens(n) {
  if (!n) return "0";
  if (n < 1000) return String(n);
  return `${Math.round(n / 1000)}k`;
}

// Merge a fresh server poll, keeping the activity count monotonic within a session:
// a poll that raced ahead of an in-flight flush must not make the clock tick
// backwards. A changed session_start (a restart) legitimately resets it.
function mergeTrack(next) {
  const out = next || {};
  for (const ws in out) {
    const prev = track.data[ws];
    if (prev && prev.session_start === out[ws].session_start) {
      out[ws].active_seconds = Math.max(out[ws].active_seconds, prev.active_seconds);
    }
  }
  track.data = out;
}

async function pollTrack() {
  if (!state.workspaces.length) return;
  try {
    const t = await fetch("/api/track").then((r) => (r.ok ? r.json() : null));
    if (t) mergeTrack(t);
  } catch { /* unreachable host: clocks just don't advance their base this round */ }
  renderTrackBanner();
  renderTopic(); // a changed session_start can flip a topic to stale
  renderIdent();
}

// Flush accrued activity for a workspace. Optimistically fold it into the local
// base so the clock doesn't dip between the flush landing and the next poll; on
// failure keep the arrears and let the next flush carry them.
async function flushTrack(ws) {
  const n = ws ? (track.pending[ws] | 0) : 0;
  if (!ws || n <= 0) return;
  // One flush per workspace at a time: the interval, blur, pause and switch can
  // all fire a flush, and two overlapping POSTs would each snapshot n, send it,
  // and both subtract on success — double-counting the seconds and driving
  // pending negative. A later flush carries whatever accrues while this one runs.
  if (track.inflight[ws]) return;
  track.inflight[ws] = true;
  try {
    const res = await fetch(`/api/track/${encodeURIComponent(ws)}/inc`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ seconds: n }),
    });
    if (!res.ok) return;
    track.pending[ws] -= n;
    if (track.data[ws]) track.data[ws].active_seconds += n;
  } catch { /* keep pending, retry next flush */ }
  finally { delete track.inflight[ws]; }
}
function flushActive() { return flushTrack(state.active); }

// On leaving the page, the arrears would be lost to an in-flight fetch the browser
// cancels — sendBeacon is the one thing that survives unload.
function beaconFlush(ws) {
  const n = ws ? (track.pending[ws] | 0) : 0;
  if (!ws || n <= 0 || !navigator.sendBeacon) return;
  try {
    const blob = new Blob([JSON.stringify({ seconds: n })], { type: "application/json" });
    if (navigator.sendBeacon(`/api/track/${encodeURIComponent(ws)}/inc`, blob)) {
      track.pending[ws] -= n;
    }
  } catch {}
}

// One-second tick: bank a second for the active workspace when live, and repaint
// (so the session clock advances even when nothing is accruing).
function trackTick() {
  if (trackingLive()) {
    track.pending[state.active] = (track.pending[state.active] | 0) + 1;
  }
  renderTrackBanner();
}

// Clear a workspace's tracking locally — used when we stop or restart it, so the
// banner doesn't linger on a session that's gone before the next poll catches up.
function clearTrack(ws) {
  delete track.pending[ws];
  delete track.data[ws];
  delete track.inflight[ws];
  renderTrackBanner();
}

function setTrackHidden(v) {
  track.hidden = v;
  localStorage.setItem(TRACK_HIDE_KEY, v ? "1" : "0");
  renderTrackBanner();
}

// Any interaction clears a manual pause — but not one on the pause button itself,
// which is a toggle (mousedown there would otherwise cancel the very pause it sets).
function trackInteraction(e) {
  if (e && e.target && e.target.closest && e.target.closest("#track-pause")) return;
  if (track.paused) { track.paused = false; renderTrackBanner(); }
}
for (const ev of ["mousedown", "keydown", "wheel", "touchstart"]) {
  document.addEventListener(ev, trackInteraction, { passive: true });
}

// Focus/visibility gate the activity clock and are good moments to flush.
window.addEventListener("focus", () => { track.hasFocus = true; renderTrackBanner(); });
window.addEventListener("blur", () => { track.hasFocus = false; flushActive(); renderTrackBanner(); });
document.addEventListener("visibilitychange", () => {
  if (document.hidden) flushActive();
  renderTrackBanner();
});
window.addEventListener("pagehide", () => beaconFlush(state.active));

document.getElementById("track-pause").addEventListener("click", () => {
  track.paused = !track.paused;
  if (track.paused) flushActive(); // bank what you've done before stepping away
  renderTrackBanner();
});
document.getElementById("track-copy").addEventListener("click", (e) =>
  copyToClipboard(trackCopyText(), e.currentTarget));
document.getElementById("track-hide").addEventListener("click", () => setTrackHidden(!track.hidden));

setInterval(trackTick, 1000);
setInterval(flushActive, 15000);
setInterval(pollTrack, 5000);

// ---- Claude panel (one line per login) --------------------------------------
// Twenty workspaces spend three or four Claude accounts between them, and a rate
// limit belongs to the ACCOUNT: three workspaces on one login are all drawing down
// the same five-hour window. So this panel is grouped the way the limits actually
// work — one line per login, carrying that login's 5-hour and weekly percentages.
//
// A line and nothing more. No bars, no row per workspace, no costs: the panel sits
// above the servers panel in a column the file tree also needs, and the one thing
// it exists to answer is which login is about to stop working. Which workspaces
// draw on a login is answered by the chip at the top of the pane; how full a
// context window is belongs there too, being a property of one session. Everything
// else — reset times, member names, the age of the reading — is in the tooltip.
//
// Nothing is summed across a group. The window is one number that every workspace
// on the login reports identically, so the group shows the FRESHEST report of it,
// and dims the line when that report is too old to present as current.
//
// The poll is gated like the servers one, and for the same reason — a round is an
// SSH round trip per host — with one difference: it also runs while we have never
// loaded, so the login chip at the top of the pane has a value even when this panel
// is collapsed. Identity is wanted whether or not you are watching the numbers.
const USAGE_POLL_MS = 10000;
const LOGINS_COLLAPSED_KEY = "forge-logins-collapsed";
const usage = {
  // ws name -> {account, auth, ts, model, context_*, cost_usd, five_hour, seven_day, note}
  // cost_usd arrives and is deliberately not rendered: nothing actionable follows
  // from it, and on a subscription it isn't a bill but what the same usage would
  // have cost on the API.
  data: {},
  at: 0,          // when the last reading landed
  timer: null,
  busy: false,
  loaded: false,
};

function loginsCollapsed() { return localStorage.getItem(LOGINS_COLLAPSED_KEY) === "1"; }

function setLoginsCollapsed(v) {
  localStorage.setItem(LOGINS_COLLAPSED_KEY, v ? "1" : "0");
  applyLoginsCollapsed();
  refreshUsage(); // expanding must not leave a limit from before you left
}

function applyLoginsCollapsed() {
  const collapsed = loginsCollapsed();
  document.getElementById("logins").classList.toggle("collapsed", collapsed);
  document.getElementById("logins-toggle").title = collapsed ? "Expand" : "Collapse";
}

function usageWanted() {
  if (document.hidden) return false;
  return !loginsCollapsed() || !usage.loaded;
}

function refreshUsage({ force = false } = {}) {
  if (!usageWanted() || usage.busy) return scheduleUsagePoll();
  if (!force && usage.at && Date.now() - usage.at < USAGE_POLL_MS) return scheduleUsagePoll();
  pollUsage();
}

function scheduleUsagePoll() {
  clearTimeout(usage.timer);
  usage.timer = null;
  if (!usageWanted()) return;
  usage.timer = setTimeout(pollUsage, USAGE_POLL_MS);
}

async function pollUsage() {
  if (usage.busy) return;
  usage.busy = true;
  try {
    const res = await fetch("/api/usage");
    if (res.ok) {
      usage.data = await res.json();
      usage.at = Date.now();
      usage.loaded = true;
      renderLogins();
      renderIdent();
    }
  } catch {
    // A poll that didn't land leaves the last reading up, stamped with its own age.
  } finally {
    usage.busy = false;
    scheduleUsagePoll();
  }
}

// What a login is called, in the order of what a person recognises. An account
// with no id is not a login at all — it is a workspace paying another way, and the
// group is named after that instead, because "unknown" would be a worse answer
// than "API credits" when the latter is exactly what it is.
const AUTH_LABELS = {
  api: "API credits",
  bedrock: "Bedrock",
  vertex: "Vertex AI",
};

function loginLabel(u) {
  const a = u.account || {};
  if (a.uuid) return a.email || a.name || a.org || "Claude login";
  return AUTH_LABELS[u.auth] || "No login yet";
}

// Group the workspaces we have tabs for by the login they run as, newest sample
// winning the group's windows. Keyed by account uuid — the same person in two
// organisations is two accounts with two allowances — and by auth kind for the
// workspaces that have no login to key on.
function loginGroups() {
  const groups = new Map();
  for (const ws of state.workspaces) {
    const u = usage.data[ws.name];
    if (!u) continue;
    const account = u.account || {};
    const key = account.uuid || `auth:${u.auth || "none"}`;
    let g = groups.get(key);
    if (!g) {
      g = {
        key,
        label: loginLabel(u),
        org: account.uuid ? account.org || "" : "",
        auth: u.auth || "",
        ts: 0,
        five: null,
        seven: null,
        names: [],
      };
      groups.set(key, g);
    }
    // The freshest member speaks for the group's windows: they are one number
    // reported many times, not many numbers to reconcile.
    if ((u.ts || 0) > g.ts) {
      g.ts = u.ts || 0;
      g.five = u.five_hour || null;
      g.seven = u.seven_day || null;
    }
    // Names only — which workspaces draw on this login is a tooltip question, not
    // a row each.
    g.names.push(ws.name);
  }
  for (const g of groups.values()) g.names.sort((a, b) => a.localeCompare(b));
  // By name, and only by name. This used to lead with whichever login was closest
  // to a limit, which sounds right and reads badly: the figures move while you
  // work, so the rows swap under you and every glance costs a re-read of a list
  // you already knew. Urgency is already carried where it does not move anything
  // — the figure itself, and the colour it turns.
  return [...groups.values()].sort((a, b) => a.label.localeCompare(b.label));
}

function renderLogins() {
  const list = document.getElementById("loginlist");
  const count = document.getElementById("logins-count");
  const groups = loginGroups();
  count.textContent = groups.length > 1 ? String(groups.length) : "";
  if (!groups.length) {
    list.className = "muted";
    list.textContent = usage.loaded ? "No Claude usage reported yet." : "Loading…";
    return;
  }
  list.className = "";
  list.replaceChildren(...groups.map(loginGroupRow));
}

// One login, one line: who it is and how much of each window it has spent. No
// bars and no rows underneath — a login's workspaces are named by the chip at the
// top of the pane, and the point of this panel is to be readable at a glance
// without pushing the file tree down. Everything that doesn't fit on the line
// (which workspaces, when each window resets, how old the reading is) is in the
// tooltip.
function loginGroupRow(g) {
  const row = document.createElement("div");
  row.className = "lgn" + (staleSample(g.ts) ? " stale" : "");
  row.title = loginTitle(g);

  const name = document.createElement("span");
  name.className = "lgn-name";
  // textContent, always: every string here came out of a file in a workspace home.
  name.textContent = g.label;
  row.appendChild(name);

  const windows = document.createElement("span");
  windows.className = "lgn-windows";
  if (g.five || g.seven) {
    windows.append(windowSpan("5h", g.five), windowSpan("7d", g.seven));
  } else {
    // Only a Claude.ai subscription HAS these windows. For anything else their
    // absence is the nature of the thing, not a gap in our reading — so say that
    // rather than showing two figures that would imply an untouched allowance.
    const none = document.createElement("span");
    none.className = "lgn-nowin";
    none.textContent = g.ts ? "no limit windows" : "no sample";
    windows.appendChild(none);
  }
  row.appendChild(windows);
  return row;
}

// One window as label + percentage, kept in one element so the pair reads as a
// pair and never wraps apart. The percentage carries the colour — amber at 75,
// red at 90, the same thresholds a disk uses — because that is the whole signal:
// which login is about to stop working.
function windowSpan(label, w) {
  const el = document.createElement("span");
  el.className = "win";
  const tag = document.createElement("i");
  tag.textContent = label;
  const val = document.createElement("b");
  // A window this login has but that wasn't in the last sample reads as 0%, the way
  // Claude's own usage display puts it. It is the friendlier reading of the same
  // situation: nothing spent that we know of. (A login with NO windows is a
  // different thing and never reaches here — see loginGroupRow.)
  const pct = w ? Math.max(0, Math.min(100, w.used_percent)) : 0;
  val.textContent = Math.round(pct) + "%";
  if (pct >= 90) val.className = "crit";
  else if (pct >= 75) val.className = "warn";
  el.append(tag, val);
  return el;
}

function contextPercent(u) {
  if (!u || !u.context_size) return null;
  return Math.max(0, Math.min(100, (u.context_used / u.context_size) * 100));
}

// How old a reading may be before the row stops presenting it as current. These
// figures only move while a workspace's Claude is running, so a group whose
// members are all stopped would otherwise show an hour-old percentage as fact.
// Dimming costs no space, which a visible age would.
const SAMPLE_STALE_S = 600;
function staleSample(ts) { return !ts || nowSeconds() - ts > SAMPLE_STALE_S; }

// The tooltip carries everything the line has no room for: the organisation, the
// workspaces on this login, when each window resets, and how old the reading is.
function loginTitle(g) {
  const lines = [g.label + (g.org ? ` · ${g.org}` : "")];
  if (g.auth) lines.push(`Paying by: ${AUTH_LABELS[g.auth] || g.auth}`);
  if (g.names.length) lines.push(`Workspaces: ${g.names.join(", ")}`);
  for (const [label, w] of [["5-hour", g.five], ["Weekly", g.seven]]) {
    if (!w) {
      // The line reads 0% for this one; the tooltip is where "0% of what we last
      // heard" can be said in full.
      if (g.five || g.seven) lines.push(`${label}: not in the last reading`);
      continue;
    }
    const resets = w.resets_at ? `, resets ${fmtReset(w.resets_at)}` : "";
    lines.push(`${label}: ${Math.round(w.used_percent)}% used${resets}`);
  }
  if (!g.five && !g.seven && g.ts) {
    lines.push("No rate-limit windows — Claude.ai subscriptions only.");
  }
  lines.push(g.ts
    ? `Read ${fmtAge(nowSeconds() - g.ts)} ago. Figures only move while a workspace's Claude is running.`
    : "No workspace on this login has reported yet.");
  return lines.join("\n");
}

// A reset is only ever a few hours or days out, so what you want is "in 2h", not a
// date you have to subtract from now yourself.
function fmtReset(at) {
  const left = at - nowSeconds();
  if (left <= 0) return "now";
  return `in ${fmtAge(left)}`;
}

function nowSeconds() { return Math.floor(Date.now() / 1000); }

document.getElementById("logins-head").addEventListener("click", () =>
  setLoginsCollapsed(!loginsCollapsed()));

// ---- servers panel ---------------------------------------------------------
// Every registered server, under the file tree, with what it is using right now:
// CPU, memory, and the disk the workspaces live on. It is ambient information —
// you glance at it to see which machine has room, or why one feels slow.
//
// Each round costs one SSH round trip PER HOST, so the loop is careful about when
// it runs at all: it is parked while the panel can't be seen (collapsed, or this
// browser tab in the background) and resumes when it can. And it re-arms after
// each poll settles rather than running on a fixed timer — an ssh to a hung host
// blocks for the TCP timeout, which outlasts the interval, so a repeating timer
// would stack connections on a machine that is already in trouble.
const SERVERS_POLL_MS = 10000;
const SERVERS_COLLAPSED_KEY = "forge-servers-collapsed";
const servers = {
  stats: [],      // [{host, addr, reachable, note, cpu_*, mem_*, disk_*, uptime}]
  at: 0,          // when the last reading landed
  timer: null,
  busy: false,    // a poll is in flight; nothing may start a second one
  loaded: false,  // we have had an answer, so "no servers" means it, not "not yet"
};

function serversCollapsed() { return localStorage.getItem(SERVERS_COLLAPSED_KEY) === "1"; }

function setServersCollapsed(v) {
  localStorage.setItem(SERVERS_COLLAPSED_KEY, v ? "1" : "0");
  applyServersCollapsed();
  refreshServers(); // expanding must not leave a reading from before you left
}

function applyServersCollapsed() {
  const collapsed = serversCollapsed();
  document.getElementById("servers").classList.toggle("collapsed", collapsed);
  document.getElementById("servers-toggle").title = collapsed ? "Expand" : "Collapse";
}

// Nothing polls for a panel nobody can see.
function serversWanted() { return !document.hidden && !serversCollapsed(); }

// Poll now if what we have is older than a round; otherwise just make sure the
// loop is armed. force skips the freshness check, for the moments when the list
// itself changed (a server registered) and the answer on screen is about a
// different set of machines.
function refreshServers({ force = false } = {}) {
  if (!serversWanted() || servers.busy) return scheduleServersPoll();
  if (!force && servers.at && Date.now() - servers.at < SERVERS_POLL_MS) return scheduleServersPoll();
  pollServers();
}

function scheduleServersPoll() {
  clearTimeout(servers.timer);
  servers.timer = null;
  if (!serversWanted()) return;
  servers.timer = setTimeout(pollServers, SERVERS_POLL_MS);
}

async function pollServers() {
  if (servers.busy) return;
  servers.busy = true;
  try {
    const res = await fetch("/api/hosts/stats");
    if (res.ok) {
      servers.stats = await res.json();
      servers.at = Date.now();
      servers.loaded = true;
      renderServers();
    }
  } catch (e) {
    // A poll that didn't land leaves the last reading up rather than blanking the
    // panel: the numbers are a few seconds old, which the next round corrects.
  } finally {
    servers.busy = false;
    scheduleServersPoll();
  }
}

// A server just removed in Settings should leave the panel at once, not linger
// until a poll (and the daemon's own freshness window) catch up.
function dropServer(alias) {
  servers.stats = servers.stats.filter((s) => s.host !== alias);
  renderServers();
}

function renderServers() {
  const list = document.getElementById("serverlist");
  const count = document.getElementById("servers-count");
  const stats = servers.stats || [];
  count.textContent = stats.length > 1 ? String(stats.length) : "";
  if (!stats.length) {
    list.className = "muted";
    list.textContent = servers.loaded ? "No servers registered." : "Loading…";
    return;
  }
  list.className = "";
  list.replaceChildren(...stats.map(serverRow));
}

// One line: the name, then CPU as a percentage and memory and disk as figures.
// Three stacked meters used three lines each, which a handful of servers turned
// into the whole pane. Every value here is abbreviated, so every value has its
// own tooltip saying what it is and what is left.
function serverRow(s) {
  const row = document.createElement("div");
  row.className = "srv" + (s.reachable ? "" : " down");

  const name = document.createElement("span");
  name.className = "srv-name";
  name.textContent = s.host;
  name.title = serverTitle(s);
  row.appendChild(name);

  if (!s.reachable) {
    // The row stays — a server that went down is exactly the one you want to see
    // listed — but there are no figures to line up, so the reason takes their place.
    const note = document.createElement("span");
    note.className = "srv-note";
    note.textContent = s.note || "unreachable";
    note.title = s.detail || s.note || "unreachable";
    row.appendChild(note);
    return row;
  }

  const memPct = pctOf(s.mem_used, s.mem_total);
  const diskPct = pctOf(s.disk_used, s.disk_total);
  const metrics = document.createElement("span");
  metrics.className = "srv-metrics";
  metrics.append(
    metric(ICON_CPU, s.cpu_cores ? Math.round(s.cpu_percent) + "%" : "—",
      s.cpu_cores ? s.cpu_percent : null,
      s.cpu_cores
        ? `CPU ${Math.round(s.cpu_percent)}% of ${s.cpu_cores} core${s.cpu_cores === 1 ? "" : "s"}`
        : "CPU not measured"),
    metric(ICON_MEM, figures(s.mem_used, s.mem_total), memPct,
      s.mem_total
        ? `RAM ${fmtBytes(s.mem_used)} of ${fmtBytes(s.mem_total)} · ${Math.round(memPct)}% used · ` +
          `${fmtBytes(s.mem_total - s.mem_used)} free`
        : "RAM not measured"),
    metric(ICON_DISK, figures(s.disk_used, s.disk_total), diskPct,
      s.disk_total
        ? `Disk ${fmtBytes(s.disk_used)} of ${fmtBytes(s.disk_total)} · ${Math.round(diskPct)}% used · ` +
          `${fmtBytes(s.disk_total - s.disk_used)} free` + (s.disk_path ? ` on ${s.disk_path}` : "")
        : "Disk not measured"),
  );
  row.appendChild(metrics);
  return row;
}

// One icon and one value. pct is what decides the colour — the value itself may be
// a pair of figures with no threshold of its own — and null means unmeasured, so a
// dash rather than a confident zero.
function metric(icon, text, pct, title) {
  const el = document.createElement("span");
  el.className = "m";
  el.title = title;
  const box = document.createElement("span");
  box.innerHTML = icon;
  const val = document.createElement("b");
  val.textContent = text;
  if (pct != null && pct >= 90) val.className = "crit";
  else if (pct != null && pct >= 75) val.className = "warn";
  el.append(box.firstChild, val);
  return el;
}

// "41/63" — the unit is the same on both sides of the slash and on every row, so
// printing it six times per panel says nothing the tooltip does not say better.
// Whole gibibytes: a sidebar column is no place for a decimal that moves every
// ten seconds.
function figures(used, total) {
  if (!total) return "—";
  const g = (n) => Math.round(n / (1024 * 1024 * 1024));
  return `${g(used)}/${g(total)}`;
}

function pctOf(used, total) {
  if (!total) return null;
  return (used / total) * 100;
}

// The name's tooltip: what the name itself had to truncate, plus the two facts
// that have nowhere else to go. The figures are not repeated here — each carries
// its own tooltip now, next to the number it explains.
function serverTitle(s) {
  const lines = [s.host + (s.addr ? " · " + s.addr : "")];
  if (!s.reachable) {
    lines.push(s.detail || s.note || "unreachable");
    return lines.join("\n");
  }
  if (s.uptime) lines.push("up " + uptimeLabel(s.uptime).replace(/^up /, ""));
  return lines.join("\n");
}

// Binary units, named as such: these come from the kernel's own KiB counts, and
// calling 1024³ bytes a "GB" is how a 32 GiB machine gets described as 34 GB.
function fmtBytes(n) {
  if (!n) return "0 B";
  const units = ["B", "KiB", "MiB", "GiB", "TiB"];
  let i = 0;
  let v = n;
  while (v >= 1024 && i < units.length - 1) { v /= 1024; i++; }
  return (v >= 100 || i === 0 ? Math.round(v) : v.toFixed(1)) + " " + units[i];
}

function uptimeLabel(secs) {
  if (!secs) return "";
  const d = Math.floor(secs / 86400);
  const h = Math.floor((secs % 86400) / 3600);
  const m = Math.floor((secs % 3600) / 60);
  if (d) return `up ${d}d ${h}h`;
  if (h) return `up ${h}h ${m}m`;
  return `up ${m}m`;
}

document.getElementById("servers-head").addEventListener("click", () =>
  setServersCollapsed(!serversCollapsed()));
// Coming back to the tab is the moment the numbers matter again — and the moment
// they are most out of date.
document.addEventListener("visibilitychange", () => {
  refreshServers();
  refreshUsage();
  refreshPorts();
});

// ---- clipboard -------------------------------------------------------------
function flashCopied(btn) {
  if (!btn) return;
  btn.classList.add("ok");
  setTimeout(() => btn.classList.remove("ok"), 1000);
}
async function copyToClipboard(text, btn) {
  if (!text) return;
  try {
    await navigator.clipboard.writeText(text); // localhost is a secure context
    flashCopied(btn);
    return;
  } catch {}
  try { // fallback for a browser that blocks the async clipboard API
    const ta = document.createElement("textarea");
    ta.value = text;
    ta.style.position = "fixed";
    ta.style.opacity = "0";
    document.body.appendChild(ta);
    ta.select();
    document.execCommand("copy");
    document.body.removeChild(ta);
    flashCopied(btn);
  } catch {}
}

// Arrow keys move between workspaces, Home/End jump to the ends — the keyboard
// contract a tablist promises.
document.getElementById("tabs").addEventListener("keydown", (e) => {
  const names = state.workspaces.map((w) => w.name);
  if (names.length < 2) return;

  const i = names.indexOf(state.active);

  // Alt+Arrow moves the tab instead of moving between tabs — dragging is the
  // obvious way to reorder, but it can't be the only way for anyone who isn't
  // using a mouse. Doesn't wrap: a tab at the end has nowhere further to go, and
  // teleporting it to the other side is never what was meant.
  if (e.altKey && (e.key === "ArrowRight" || e.key === "ArrowLeft")) {
    const j = e.key === "ArrowRight" ? i + 1 : i - 1;
    if (i < 0 || j < 0 || j >= names.length) return;
    e.preventDefault();
    names.splice(j, 0, names.splice(i, 1)[0]);
    state.workspaces = orderBy(names, state.workspaces);
    localStorage.setItem(ORDER_KEY, JSON.stringify(names));
    renderTabs();
    document.querySelector("#tabs .tab.active")?.focus();
    return;
  }

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
  flushTrack(state.active); // bank the outgoing workspace's activity before we switch
  state.active = name;
  // Remember it so a refresh comes back here (see initialWorkspace).
  localStorage.setItem(ACTIVE_KEY, name);
  ackActivity(name); // opening a workspace clears its "waiting for you" mark
  renderTabs();
  resetFiles();
  refreshPorts(); // the ports panel is about the workspace you are looking at
  // The ssh shell used to be dropped on every tab switch, resetting you to a
  // fresh prompt each time you came back. Now each workspace keeps its own shell
  // alive in the background; switching just shows the one that belongs to this
  // tab, exactly as you left it (panel open or not).
  restoreShells(name);

  // The terminal stream attaches-or-creates (like `forge workspace <name> claude`),
  // so opening it on a stopped workspace would quietly resurrect the session you
  // just stopped. Show the Start card instead and let the choice be yours.
  const ws = state.workspaces.find((w) => w.name === name);
  state.stopped = !ws || ws.status !== "running";
  state.endCause = "stopped"; // a fresh tab's card describes the host's status, not a drop
  cancelReconnect(); // the loop belonged to the workspace you just left
  teardownTerminal();
  if (!state.stopped) openTerminal(name);
  renderStage();

  // No point walking a tree on a host we can't reach, or in a home that is gone.
  if (ws && isUsable(ws.status)) loadTree(name);
  else document.getElementById("filetree").innerHTML =
    '<div class="muted">No files to show.</div>';

  renderTopic();       // say what this workspace is about before anything loads
  renderIdent();      // and where it runs
  renderTrackBanner(); // reflect the new workspace's clocks immediately
  pollTrack();         // and fetch its start/active without waiting for the interval
}

function hideSSHPanel() {
  document.getElementById("sshpanel").hidden = true;
  setPanelActive(null);
}

// ---- terminal --------------------------------------------------------------
function termTheme() {
  const dark = document.documentElement.dataset.theme === "dark";
  return dark
    ? { background: "#0a0a0a", foreground: "#e6e6e6", cursor: "#e6e6e6" }
    : { background: "#ffffff", foreground: "#1a1a1a", cursor: "#1a1a1a" };
}
function applyTermTheme() {
  for (const sess of [state.claude, ...Object.values(state.shellByKey)]) {
    if (sess) sess.term.options.theme = termTheme();
  }
}

// A terminal session: one xterm bound to one pty behind the daemon, of a given
// kind ("claude" — the persistent tmux session; "ssh" — a workspace login shell;
// "host" — a shell on the host as its own login user; "local" — a shell on this
// machine, the only kind whose pty is not on the far end of an ssh).
function makeTerminal(ws, kind, el, onEnd) {
  const term = new Terminal({
    fontFamily: 'ui-monospace, SFMono-Regular, Menlo, Consolas, monospace',
    fontSize: 13,
    cursorBlink: true,
    scrollback: 5000,
    theme: termTheme(),
    // When Claude's TUI turns on mouse tracking, a plain drag is forwarded to the
    // session (so clicking Claude's options works) and xterm suppresses its own
    // text selection. To still let you select — including dragging up into the
    // scrollback — xterm honours a "force selection" modifier: Shift off the Mac,
    // but on the Mac only Option, and only when this flag is on (it defaults off,
    // which left Mac users with no way to select at all). So: ⌥-drag to select.
    macOptionClickForcesSelection: true,
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

// Terminal endpoints are per (workspace, kind) — except the local shell, which is
// no workspace's: it runs on this machine, so it has ws-less paths of its own and
// there is exactly one of it.
function termPath(ws, kind, action) {
  return kind === "local"
    ? `/api/term/local/${action}`
    : `/api/term/${encodeURIComponent(ws)}/${kind}/${action}`;
}

function connectStream(sess, onEnd) {
  if (sess.disposed) return;
  const url = termPath(sess.ws, sess.kind, "stream") +
    `?cols=${sess.term.cols}&rows=${sess.term.rows}`;
  const es = new EventSource(url);
  sess.es = es;
  es.onmessage = (ev) => {
    // A byte arriving is the only proof the link actually works — an ssh that
    // connects and is then refused looks like success right up until it isn't.
    // So the backoff resets here, on evidence, rather than when we merely decide
    // to try again; otherwise a reattach that dies on arrival would restart the
    // curve at one second every time and hammer a server that is already
    // struggling.
    if (!sess.gotData) {
      sess.gotData = true;
      if (sess.kind === "claude") cancelReconnect();
    }
    if (ev.data) sess.term.write(b64decodeBytes(ev.data));
  };
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
  fetch(termPath(ws, kind, "input"), {
    method: "POST",
    headers: { "Content-Type": "text/plain" },
    body: b64encode(data),
  }).catch(() => {});
}

function postResize(ws, kind, cols, rows) {
  fetch(termPath(ws, kind, "resize"), {
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
      // The stream ended and we didn't ask for it. That looks identical whether
      // tmux died or the ssh link did, so don't guess: show "Connection lost"
      // while we ask the host which it was.
      teardownTerminal();
      state.stopped = true;
      state.endCause = "checking";
      setStatus(null);
      renderStage();
      diagnoseEnd(ws);
    }
  });
}

// Did the session die, or just our connection to it? Only the host can say, so
// ask it. A workspace still "running" means tmux is alive and it was the link
// that broke — the case that used to be reported as "Session stopped", telling
// you your work was gone when Claude was in fact still working.
//
// Asking is one status call, which the daemon makes once per HOST for all of its
// workspaces — so this costs the same whether you keep one workspace or twenty.
// Reattaching is what costs a fresh ssh handshake, and we only do that once the
// host has said the session is actually up.
async function diagnoseEnd(ws) {
  if (state.reconnect.busy) return; // an attempt is already in flight
  state.reconnect.busy = true;
  try {
    // A probe, not an action: reuse an answer up to the loop's own floor old, so
    // that N tabs watching the same server cost one round trip, not N.
    await loadWorkspaces({ maxAge: RECONNECT_CAP_MIN / 1000 });
  } catch {
    // Can't even reach our own daemon. Nothing to conclude — just try again.
  } finally {
    state.reconnect.busy = false;
  }
  // Slow ssh: you may have switched tabs, or started the session again, while we
  // were asking. Either way this answer is no longer about what's on screen.
  if (state.active !== ws || !state.stopped) return cancelReconnect();

  const w = state.workspaces.find((x) => x.name === ws);
  const status = w ? w.status : null;

  // The session is up: it was the link that broke, so reattach on your behalf —
  // you shouldn't have to click anything because the wifi blinked. It goes
  // through the same backoff as everything else, which is what stops a reattach
  // that fails instantly (sshd refusing under MaxStartups) from becoming a tight
  // loop of handshakes. The first wait is a second, so a blip still heals at once.
  if (status === "running") {
    state.endCause = "lost";
    renderStage();
    scheduleReconnect(ws, () => reattach(ws));
    return;
  }
  // The host answered, and the session is genuinely gone (or the workspace is).
  // Stop: the terminal stream attaches-or-CREATES, so retrying here would quietly
  // resurrect a session you stopped on purpose.
  if (status === "stopped" || status === "missing") {
    cancelReconnect();
    state.endCause = "stopped";
    renderStage();
    return;
  }
  // Unreachable, or we never got an answer: the server is down or the link still
  // is. Keep trying — this is the case that comes back on its own.
  state.endCause = "stopped"; // paints the truthful "Server unreachable" card
  renderStage();
  scheduleReconnect(ws, () => diagnoseEnd(ws));
}

// Put the terminal back. Note we only get here once the host has said the session
// is running — the stream attaches-or-creates, so reattaching to a session that
// really stopped would silently start a new Claude.
function reattach(ws) {
  if (state.active !== ws || !state.stopped) return;
  state.stopped = false;
  state.endCause = "stopped";
  renderStage();
  openTerminal(ws);
}

// How long to wait before the next reattach: 1s, 2s, 4s, 8s, then a random
// 10–30s forever.
//
// Two reasons the tail is random rather than a round number. A fixed interval
// synchronises every tab and every machine you left the UI open on: the server
// comes back and they all knock at the same instant, which is exactly when sshd's
// MaxStartups (10 concurrent unauthenticated connections by default) starts
// refusing them — a self-inflicted stampede at the worst moment. And an outage
// long enough to matter is an outage where a per-tab 10s and a per-tab 30s are
// equally fine, so spreading them costs nothing.
const RECONNECT_CAP_MIN = 10000;
const RECONNECT_CAP_MAX = 30000;
function reconnectDelay(tries) {
  const step = 1000 * Math.pow(2, tries);
  if (step < RECONNECT_CAP_MIN) return step;
  return RECONNECT_CAP_MIN + Math.random() * (RECONNECT_CAP_MAX - RECONNECT_CAP_MIN);
}

// Arm the next attempt. Scheduling happens only AFTER the previous one resolved,
// never on a fixed interval: an ssh connect to a machine that is hung (rather
// than refusing) blocks for the TCP timeout, which is longer than the interval —
// a repeating timer would stack overlapping connections on top of each other and
// still be piling them up when the server finally answers.
function scheduleReconnect(ws, attempt) {
  clearTimeout(state.reconnect.timer);
  state.reconnect.attempt = attempt;
  // A hidden tab is a tab nobody is looking at. Retrying in the background is
  // pure cost — ssh handshakes for a terminal that isn't on screen — so park the
  // loop and pick it up the moment the tab is looked at again. This is what keeps
  // "the UI open in twenty tabs" from meaning twenty reconnect loops: at most the
  // visible one runs.
  if (document.hidden) {
    state.reconnect.pending = true;
    return;
  }
  state.reconnect.pending = false;
  const delay = reconnectDelay(state.reconnect.tries++);
  state.reconnect.timer = setTimeout(attempt, delay);
}

function cancelReconnect() {
  clearTimeout(state.reconnect.timer);
  state.reconnect.timer = null;
  state.reconnect.tries = 0;
  state.reconnect.pending = false;
  state.reconnect.attempt = null;
}

// Is a reattach still coming? The card says so, so that a server that will come
// back on its own doesn't read like something you have to go and fix.
function reconnecting() {
  return !!state.reconnect.timer || state.reconnect.pending || state.reconnect.busy;
}

// Two signals worth more than any timer, because both mean "the thing that was
// broken may have just been fixed": the tab coming back to the foreground, and
// the OS reporting the network is up. Both reset the backoff — the wait so far
// was about a situation that no longer holds.
function retryNow() {
  if (!state.active || !state.stopped || !reconnecting()) return;
  const attempt = state.reconnect.attempt || (() => diagnoseEnd(state.active));
  clearTimeout(state.reconnect.timer);
  state.reconnect.timer = null;
  state.reconnect.tries = 0;
  state.reconnect.pending = false;
  attempt();
}
document.addEventListener("visibilitychange", () => { if (!document.hidden) retryNow(); });
window.addEventListener("online", retryNow);

function teardownTerminal() {
  disposeTerminal(state.claude);
  state.claude = null;
}

// ---- shells (overlay panel) ------------------------------------------------
// The overlay panel hosts one shell at a time, of one of three kinds: a workspace
// login shell ("ssh", as the workspace user), a shell on the host as its own
// login user ("host" — the account `host prepare` connected as, for server-wide
// work like installing a package), or a shell on this machine ("local", in your
// home directory — for the commands that go with the servers but run here).
//
// The first two are the workspace's, so every (workspace, kind) gets its own; the
// local one belongs to no workspace, so there is exactly ONE of it and every tab
// shows that same shell. All of them are kept alive across tab switches AND
// across switching kinds: hiding the panel — or switching tab or kind — does NOT
// close a shell, its stream stays open, so the shell (its cwd, scrollback, any
// running command) is right where you left it. Each renders into its own host
// element inside #sshterm; only the shell currently on screen is shown, the rest
// wait hidden.
const SHELL_KINDS = ["ssh", "host", "local"];
// The kinds that belong to a workspace, and so go away with it. "local" does not:
// it outlives any one workspace, exactly as the machine it runs on does.
const WS_SHELL_KINDS = ["ssh", "host"];
function isLocalKind(kind) { return kind === "local"; }
// One local machine, one local shell: its key carries no workspace, so every tab
// resolves to the same session instead of spawning a shell per tab.
function shellKey(ws, kind) { return isLocalKind(kind) ? "local" : ws + "/" + kind; }

// The host's login user differs per server (root, or whatever sudo user it was
// prepared as), so the host shell is named after the real account rather than
// assuming "root". Falls back to "host" until the workspace list has loaded.
function hostUserFor(ws) {
  const w = state.workspaces.find((x) => x.name === ws);
  return (w && w.host_user) || "";
}
function shellLabel(kind, ws) {
  if (kind === "ssh") return "SSH";
  if (isLocalKind(kind)) return "Local";
  return hostUserFor(ws) || "host";
}

// The panel header says which shell you are in and, just as importantly, where it
// is: the workspace for the two remote kinds, this machine for the local one —
// which must never be labelled with a workspace name, since typing into it does
// nothing to that workspace.
function setPanelHead(kind, ws) {
  document.getElementById("ssh-kind").textContent = shellLabel(kind, ws);
  document.getElementById("ssh-ws").textContent = isLocalKind(kind) ? "this machine" : ws;
}

// setPanelActive lights the rail button whose shell the panel is showing (none
// when the panel is closed), so ssh and host can never both look open at once.
function setPanelActive(kind) {
  // Chat is not a shell, but it is the fourth thing that can be the thing you are
  // looking at, and a rail that lit up for three of them would be lying about the
  // fourth.
  const chatBtn = document.querySelector('.rail-btn[data-action="chat"]');
  if (chatBtn) chatBtn.classList.toggle("active", kind === "chat");
  if (kind !== "chat") document.getElementById("chatpanel").hidden = true;
  for (const k of SHELL_KINDS) {
    const b = document.querySelector(`.rail-btn[data-action="${k}"]`);
    if (b) b.classList.toggle("active", k === kind);
  }
}

function ensureShell(ws, kind) {
  const key = shellKey(ws, kind);
  let sess = state.shellByKey[key];
  if (sess && !sess.ended) return sess;
  if (sess) disposeShell(ws, kind); // the shell exited — replace it with a fresh one

  const host = document.createElement("div");
  host.className = "sshhost";
  host.dataset.key = key;
  document.getElementById("sshterm").appendChild(host);

  // The local shell has no workspace of its own — passing the tab it happened to
  // be opened from would be a lie the moment you switch tabs (and the endpoints
  // it talks to take no workspace anyway).
  sess = makeTerminal(isLocalKind(kind) ? "" : ws, kind, host, () => {
    const s = state.shellByKey[key];
    if (s) s.note = "Shell exited. Hide and reopen the panel to start a new one.";
    if (state.shell === s) setSSHNote(s ? s.note : null);
  });
  sess.host = host;
  sess.note = null;
  state.shellByKey[key] = sess;
  return sess;
}

// Show the active workspace's open shell (if any) and hide every other. Called on
// every tab switch — the shells themselves are never touched, only which one is on
// screen, and which kind (if any) this tab had open.
function restoreShells(ws) {
  for (const s of Object.values(state.shellByKey)) if (s.host) s.host.hidden = true;

  const panel = document.getElementById("sshpanel");
  const kind = state.panelKindByWs[ws];
  const sess = kind ? state.shellByKey[shellKey(ws, kind)] || null : null;
  state.shell = sess;

  // Only reopen for a workspace the host can actually reach. If it went missing/
  // unreachable while you were away, keep the remembered kind but leave the panel
  // closed — so it comes back on its own once the host answers again, and until
  // then you can't type into a shell the rest of the UI has disabled. The local
  // shell is exempt: it doesn't run on the host, so the host's state says nothing
  // about whether it works.
  const wsObj = state.workspaces.find((w) => w.name === ws);
  const usable = isLocalKind(kind) || (!!wsObj && isUsable(wsObj.status));
  if (sess && usable) {
    sess.host.hidden = false;
    panel.hidden = false;
    setPanelActive(kind);
    setPanelHead(kind, ws);
    setSSHNote(sess.note);
    // It was display:none until now, so xterm couldn't measure itself — refit
    // once it has a real box, and only if we're still on this workspace.
    requestAnimationFrame(() => {
      if (state.shell !== sess || sess.disposed) return;
      try { sess.fit.fit(); } catch (e) {}
      sess.term.focus();
    });
  } else {
    panel.hidden = true;
    setPanelActive(null);
    setSSHNote(null);
  }
}

function disposeShell(ws, kind) {
  const key = shellKey(ws, kind);
  const sess = state.shellByKey[key];
  if (!sess) return;
  disposeTerminal(sess);
  if (sess.host) sess.host.remove();
  delete state.shellByKey[key];
  if (state.shell === sess) state.shell = null;
}

// Drop every shell belonging to a workspace — used when its user is gone
// (deleted) or the workspace vanished from the list. Only the workspace's own
// kinds: the local shell is nobody's, and deleting a workspace is no reason to
// kill the shell you were running commands in on your own machine.
function disposeWsShells(ws) {
  for (const kind of WS_SHELL_KINDS) disposeShell(ws, kind);
  delete state.panelKindByWs[ws];
}

function teardownAllShells() {
  for (const sess of Object.values(state.shellByKey)) disposeShell(sess.ws, sess.kind);
  state.shell = null;
  state.panelKindByWs = {};
  hideSSHPanel();
  setSSHNote(null);
}

// Drop shells whose workspace no longer exists (deleted from another machine).
// The local shell has no workspace to lose, so it is never pruned.
function pruneShells() {
  for (const sess of Object.values(state.shellByKey)) {
    if (isLocalKind(sess.kind)) continue;
    if (!state.workspaces.some((w) => w.name === sess.ws)) disposeShell(sess.ws, sess.kind);
  }
  for (const ws of Object.keys(state.panelKindByWs)) {
    if (!state.workspaces.some((w) => w.name === ws)) delete state.panelKindByWs[ws];
  }
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
// The shell panel is an overlay: toggling it must NOT resize the terminal, so we
// never call fit() on the Claude terminal here — it keeps its exact size beneath.

// openShell shows the given kind of shell for the active workspace, creating it on
// first use. With the panel already showing the other kind, this just swaps which
// shell the one panel displays (both keep running underneath).
function openShell(kind) {
  if (!state.active) return; // nothing to open a shell into
  const ws = state.active;

  document.getElementById("sshpanel").hidden = false;
  state.panelKindByWs[ws] = kind;
  setPanelActive(kind);

  const sess = ensureShell(ws, kind);
  state.shell = sess;
  // Show this shell, hide every other (the other kinds, or another tab's leftovers).
  for (const s of Object.values(state.shellByKey)) if (s.host) s.host.hidden = s !== sess;
  setPanelHead(kind, ws);
  setSSHNote(sess.note);
  // The panel was display:none, so xterm couldn't measure itself — refit now that
  // it has a real box.
  requestAnimationFrame(() => {
    if (state.shell !== sess || sess.disposed) return;
    try { sess.fit.fit(); } catch (e) {}
    sess.term.focus();
  });
}

// closePanel hides the overlay without closing any shell: the shells keep running,
// we just forget this tab had a panel open and hand focus back to Claude.
function closePanel() {
  const ws = state.active;
  document.getElementById("sshpanel").hidden = true;
  setPanelActive(null);
  if (ws) delete state.panelKindByWs[ws];
  state.shell = null;
  if (state.claude) state.claude.term.focus();
}

// toggleShell is what the rail buttons call: clicking the kind already on screen
// closes the panel; clicking the other kind (or with the panel closed) shows it.
function toggleShell(kind) {
  if (!state.active) return;
  if (state.panelKindByWs[state.active] === kind) closePanel();
  else openShell(kind);
}
document.getElementById("rail").addEventListener("click", (e) => {
  const btn = e.target.closest(".rail-btn");
  if (!btn) return;
  switch (btn.dataset.action) {
    case "chat": toggleChat(); break;
    case "ssh": toggleShell("ssh"); break;
    case "host": toggleShell("host"); break;
    case "local": toggleShell("local"); break;
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
    // Its shells are gone with the user — drop them whether or not it's our tab.
    disposeWsShells(name);
    if (state.active === name) {
      teardownTerminal();
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
    dropServer(alias);
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

function confirmAction({ title, body, confirmLabel = "Confirm", destructive = false, requireWord = null, copyText = "" }) {
  const modal = document.getElementById("confirm");
  // Only one dialog at a time. Opening a second over the first would strand the
  // first promise forever — its caller would sit there awaiting an answer that
  // can never arrive.
  if (!modal.hidden) return Promise.resolve(false);

  document.getElementById("cf-title").textContent = title;

  // A copy button by the confirm action, so you can grab the session's final times
  // and paste them into a task before you close the session for good.
  const copyBtn = document.getElementById("cf-copy");
  copyBtn.hidden = !copyText;
  copyBtn.onclick = copyText ? () => copyToClipboard(copyText, copyBtn) : null;

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
  // Poll once if the clocks haven't landed yet, so the dialog's Copy button
  // isn't hidden on a fresh load where /api/track hasn't returned.
  if (!trackNumbers(ws)) await pollTrack();
  const ok = await confirmAction({
    title: "Stop the Claude session?",
    body: [
      { text: `Claude is running in "${ws}". Stopping kills the session.`, warn: false },
      { text: "Whatever it was doing stops, and its context — everything you've said this session — is gone. The files on the server are untouched.", warn: true },
    ],
    confirmLabel: "Stop session",
    destructive: true,
    copyText: trackCopyText(),
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
    state.endCause = "stopped"; // we killed it — no need to go and ask why it ended
    cancelReconnect(); // and definitely no reattaching: it would start a new one
    clearTrack(ws); // the session's clocks end with it (the server cleared the file)
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
  state.endCause = "stopped";
  state.reconnectOnEnd = false;
  cancelReconnect(); // you asked directly; the loop's opinion is now irrelevant
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
  if (!trackNumbers(ws)) await pollTrack();
  const ok = await confirmAction({
    title: "Restart the Claude session?",
    body: [
      { text: `This kills Claude in "${ws}" and starts a brand-new session.`, warn: false },
      { text: "The current context is lost — nothing is saved first. If you want to keep what Claude knows, run Checkpoint instead.", warn: true },
    ],
    confirmLabel: "Restart session",
    destructive: true,
    copyText: trackCopyText(),
  });
  if (!ok) return;

  // The restart kills the session; the stream's "end" then reconnects us to the
  // fresh one.
  state.reconnectOnEnd = true;
  setStatus("Restarting…");
  try {
    await post("restart", ws);
    clearTrack(ws); // a hard restart is a new session — its clocks start over
    loadWorkspaces();
  } catch (e) {
    state.reconnectOnEnd = false;
    setStatus("Restart failed: " + e.message);
  }
}

async function doCheckpoint() {
  if (!state.active) return;
  const ws = state.active;
  if (!trackNumbers(ws)) await pollTrack();
  const ok = await confirmAction({
    title: "Checkpoint this session?",
    body: [
      { text: `Claude writes a handoff to its memory, then the session in "${ws}" restarts and continues from it with a fresh context.`, warn: false },
      { text: "Do this while Claude is idle — if it's mid-task the checkpoint refuses rather than interrupt it.", warn: false },
    ],
    confirmLabel: "Checkpoint",
    copyText: trackCopyText(),
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
document.getElementById("ssh-close").addEventListener("click", () => closePanel());
document.addEventListener("keydown", (e) => {
  if (e.key !== "Escape") return;
  // Topmost layer wins: the confirm dialog, then the modals, then ssh.
  if (!document.getElementById("confirm").hidden) { closeConfirm(false); return; }
  if (!document.getElementById("wizard").hidden) { closeWizard(); return; }
  if (!document.getElementById("settings").hidden) { closeSettings(); return; }
  if (!document.getElementById("sshpanel").hidden) {
    closePanel();
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
// One button, two meanings: it starts a session that is really stopped, and
// reattaches to one that only lost its connection. Same code path either way —
// the stream attaches-or-creates — but the label has to match what's true, or it
// reads as "you have to start Claude again" over a session that never stopped.
document.getElementById("stopped-start").addEventListener("click", (e) => {
  if (e.currentTarget.dataset.action === "reconnect") retryNow();
  else doStart();
});
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

// The image and named-volume sweeps are tiers of the nightly clean-up, so neither
// can be on without it: untick and disable them whenever the clean-up is off. The
// server rejects the combo too — this just keeps the contradiction unbuildable in
// the UI.
function syncPruneTiers() {
  const prune = document.getElementById("wiz-prune");
  for (const id of ["wiz-prune-images", "wiz-prune-volumes"]) {
    const tier = document.getElementById(id);
    tier.disabled = !prune.checked;
    if (!prune.checked) tier.checked = false;
  }
}
document.getElementById("wiz-prune").addEventListener("change", syncPruneTiers);

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
  // Opt-in, so it resets to off — the aggressive image sweep is never a default.
  document.getElementById("wiz-prune-images").checked = false;
  document.getElementById("wiz-prune-volumes").checked = false;
  syncPruneTiers();
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
                    "wiz-jump", "wiz-firewall", "wiz-harden", "wiz-prune", "wiz-prune-images",
                    "wiz-prune-volumes", "wiz-cancel"]) {
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
        document.getElementById("wiz-jump").value.trim(),
        document.getElementById("wiz-firewall").checked,
        document.getElementById("wiz-harden").checked,
        document.getElementById("wiz-prune").checked,
        document.getElementById("wiz-prune-images").checked,
        document.getElementById("wiz-prune-volumes").checked);
      host = alias;
      refreshServers({ force: true }); // a machine we've never measured just joined
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
// jump is the route to a server this machine cannot reach directly — ssh's -J,
// empty for the usual case. It is sent as typed; the core reads it.
async function prepareHost(target, alias, jump, firewall, harden, dockerPrune, pruneImages, pruneVolumes) {
  const log = document.getElementById("wiz-log");
  log.hidden = false;
  log.textContent = "";

  const res = await fetch("/api/hosts/prepare", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ target, alias, jump, firewall, harden, dockerPrune, pruneImages, pruneVolumes }),
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
// The server metrics' icons. Small, because each sits in an 11px column and
// stands in for a word ("CPU", "RAM") rather than illustrating one.
const ICON_CPU = `<svg viewBox="0 0 24 24" width="11" height="11" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><rect x="8" y="8" width="8" height="8" rx="1"/><rect x="4.5" y="4.5" width="15" height="15" rx="2"/><path d="M9 2v2.5M15 2v2.5M9 19.5V22M15 19.5V22M2 9h2.5M2 15h2.5M19.5 9H22M19.5 15H22"/></svg>`;
const ICON_MEM = `<svg viewBox="0 0 24 24" width="11" height="11" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><rect x="2.5" y="7" width="19" height="10" rx="1.5"/><path d="M6.5 17v2.5M12 17v2.5M17.5 17v2.5M7 11v2M12 11v2M17 11v2"/></svg>`;
const ICON_DISK = `<svg viewBox="0 0 24 24" width="11" height="11" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><ellipse cx="12" cy="6.5" rx="8" ry="3"/><path d="M4 6.5v11c0 1.7 3.6 3 8 3s8-1.3 8-3v-11"/><path d="M4 12c0 1.7 3.6 3 8 3s8-1.3 8-3"/></svg>`;
// The tracking banner's pause/resume pair, swapped by renderTrackBanner.
const ICON_PAUSE = `<svg viewBox="0 0 24 24" width="18" height="18" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><rect x="6.5" y="4.5" width="4" height="15" rx="1"/><rect x="13.5" y="4.5" width="4" height="15" rx="1"/></svg>`;
const ICON_PLAY = `<svg viewBox="0 0 24 24" width="18" height="18" fill="currentColor" aria-hidden="true"><polygon points="7,4.5 19.5,12 7,19.5"/></svg>`;
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
initTabDrag();
initChat();
state.showHidden = localStorage.getItem("forge-show-hidden") === "1";
applyShowHidden();
applyServersCollapsed();
applyLoginsCollapsed();
renderServers();
renderLogins();
refreshServers();
refreshUsage();
loadWorkspaces().then(pollActivity);

// ── Ports ────────────────────────────────────────────────────────────────────
// What this workspace publishes, and a way in. Same polling discipline as the
// servers panel: parked while the panel can't be seen, re-armed after each poll
// settles rather than on a fixed timer, and a poll that doesn't land leaves the
// last answer up instead of blanking the panel.
const PORTS_POLL_MS = 6000;
const PORTS_COLLAPSED_KEY = "forge-ports-collapsed";
const ports = {
  ws: null,       // the workspace the rows describe
  info: null,     // {block, rows:[…]}
  at: 0,
  timer: null,
  busy: false,
  loaded: false,
};

// Container ports that are famously not HTTP. The heuristic runs on the TARGET
// port — the one inside the container — because the host port comes from the
// workspace's block and says nothing about what is behind it: Postgres published
// at 16003 is still Postgres. Getting it wrong costs a copy instead of a link,
// which is why a short list beats probing.
const NON_HTTP_PORTS = new Set([
  5432, 3306, 1433, 27017, 6379, 11211, 5672, 9092, 2181, 25, 587, 22,
]);

function portsCollapsed() { return localStorage.getItem(PORTS_COLLAPSED_KEY) === "1"; }

function setPortsCollapsed(v) {
  localStorage.setItem(PORTS_COLLAPSED_KEY, v ? "1" : "0");
  applyPortsCollapsed();
  refreshPorts({ force: true });
}

function applyPortsCollapsed() {
  const collapsed = portsCollapsed();
  document.getElementById("ports").classList.toggle("collapsed", collapsed);
  document.getElementById("ports-toggle").title = collapsed ? "Expand" : "Collapse";
}

function portsWanted() {
  return !document.hidden && !portsCollapsed() && !!state.active;
}

function refreshPorts({ force = false } = {}) {
  // Switching workspace makes what's on screen about a different machine, so it
  // is refetched rather than aged out.
  if (ports.ws !== state.active) {
    ports.ws = state.active;
    ports.info = null;
    ports.at = 0;
    ports.loaded = false;
    renderPorts();
    force = true;
  }
  if (!portsWanted() || ports.busy) return schedulePortsPoll();
  if (!force && ports.at && Date.now() - ports.at < PORTS_POLL_MS) return schedulePortsPoll();
  pollPorts();
}

function schedulePortsPoll() {
  clearTimeout(ports.timer);
  ports.timer = null;
  if (!portsWanted()) return;
  ports.timer = setTimeout(pollPorts, PORTS_POLL_MS);
}

async function pollPorts() {
  if (ports.busy || !state.active) return;
  const ws = state.active;
  ports.busy = true;
  try {
    const res = await fetch(`/api/ports/${encodeURIComponent(ws)}`);
    if (res.ok) {
      const info = await res.json();
      // The answer is about the workspace that was active when it was asked for;
      // if that changed while it was in flight, it is about the wrong one.
      if (ws !== state.active) return;
      ports.info = info;
      ports.at = Date.now();
      ports.loaded = true;
      renderPorts();
    }
  } catch (e) {
    // Left as-is: a few seconds stale beats an empty panel that looks like a
    // workspace with nothing running.
  } finally {
    ports.busy = false;
    schedulePortsPoll();
  }
}

function renderPorts() {
  const list = document.getElementById("portlist");
  const block = document.getElementById("ports-block");
  const info = ports.info;
  block.textContent = info && info.block ? info.block : "";
  const rows = (info && info.rows) || [];
  if (!rows.length) {
    list.className = "muted";
    // A note means the daemon knows why the panel is empty — a server it could
    // not reach — and saying so beats sitting on "Loading…" for a machine that is
    // never going to answer.
    if (!state.active) list.textContent = "Select a workspace.";
    else if (info && info.note) list.textContent = info.note;
    else if (!ports.loaded) list.textContent = "Loading…";
    else if (info && info.block) list.textContent = `Nothing published yet (${info.block}).`;
    else list.textContent = "Nothing published yet.";
    return;
  }
  list.className = "";
  list.replaceChildren(...rows.map(portRow));
}

// A row's state, in one word — and the same word the dot's colour means.
//
// Every tunnel state the daemon can send is answered here by name. "retrying" is
// the only one left to the default, because it is the only one whose meaning is
// "wait a moment"; the others each need saying, and an auth failure quietly
// rendered as "connecting" would be a spinner for something that will never
// connect.
function portState(p) {
  if (!p.in_block) return "untunnelled";
  if (!p.running) return "stopped";
  switch (p.tunnel) {
    case "up": return "ok";
    case "blocked": return "blocked";
    case "error": return "error";
    case "none": return "notunnel";
    default: return "connecting";
  }
}

function portRow(p) {
  const st = portState(p);
  const row = document.createElement("div");
  row.className = "port" + (st === "ok" ? "" : " down");
  row.title = portTitle(p, st);

  // The dot answers one question — is the container running — and the tunnel's
  // state is carried by whether the row offers a link at all.
  const dot = document.createElement("span");
  dot.className = "port-dot " + (p.running ? "running" : "stopped");

  const label = document.createElement("span");
  label.className = "port-label";
  label.appendChild(portTarget(p, st));

  row.append(dot, label);
  // Only a container can be started and stopped. A plain process — a dev server
  // someone ran in a shell — has no command Forge could bring it back with, so
  // it gets no button rather than one that fails.
  if (p.kind === "container") row.appendChild(portButton(p));
  return row;
}

// The clickable part. A link only when it would actually work: the tunnel has to
// be up, and the thing behind it has to plausibly speak HTTP. Everything else is
// a button that copies `127.0.0.1:<port>`, because the port is what you'd paste
// into curl or a redirect URI anyway.
function portTarget(p, st) {
  const text = `${p.name}:${p.port}`;
  if (st === "ok" && !NON_HTTP_PORTS.has(p.target)) {
    const a = document.createElement("a");
    a.href = `http://127.0.0.1:${p.port}`;
    a.target = "_blank";
    a.rel = "noopener";
    a.textContent = text;
    return a;
  }
  const b = document.createElement("button");
  b.className = "plain";
  b.textContent = text;
  b.addEventListener("click", () => writeClipboard(`127.0.0.1:${p.port}`));
  return b;
}

function portButton(p) {
  const b = document.createElement("button");
  const stop = p.running;
  b.className = "port-act " + (stop ? "stop" : "start");
  b.title = stop ? `Stop ${p.name}` : `Start ${p.name}`;
  b.innerHTML = stop
    ? '<svg viewBox="0 0 24 24" width="12" height="12" fill="currentColor" aria-hidden="true">' +
      '<rect x="6" y="6" width="12" height="12" rx="1"></rect></svg>'
    : '<svg viewBox="0 0 24 24" width="12" height="12" fill="currentColor" aria-hidden="true">' +
      '<polygon points="7,5 19,12 7,19"></polygon></svg>';
  b.addEventListener("click", async () => {
    b.disabled = true;
    try {
      const res = await fetch(`/api/ports/${encodeURIComponent(ports.ws)}/container`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ service: p.name, action: stop ? "stop" : "start" }),
      });
      if (!res.ok) {
        const body = await res.json().catch(() => ({}));
        flashStatus(body.error || `Could not ${stop ? "stop" : "start"} ${p.name}`, 4000);
      }
    } catch (e) {
      flashStatus(`Could not ${stop ? "stop" : "start"} ${p.name}`, 4000);
    } finally {
      b.disabled = false;
      // Docker has moved on; the panel should say so now rather than in six
      // seconds, while you are still looking at the button you pressed.
      refreshPorts({ force: true });
    }
  });
  return b;
}

// The tooltip carries what one narrow row cannot: what is behind the port, and
// why it isn't reachable when it isn't.
function portTitle(p, st) {
  const lines = [`${p.name} — host port ${p.port}` + (p.target ? ` → ${p.target} in the container` : "")];
  switch (st) {
    case "ok":
      lines.push(`Reachable at 127.0.0.1:${p.port}`);
      break;
    case "stopped":
      lines.push("Container is stopped — its port stays reserved");
      break;
    case "blocked":
      lines.push(p.tunnel_detail || "Something on this machine is holding the port");
      break;
    case "error":
      lines.push(p.tunnel_detail || "The tunnel failed and will not retry");
      break;
    case "notunnel":
      lines.push("No tunnel for this port — is `forge spawn` running?");
      break;
    case "untunnelled":
      lines.push("Published outside this workspace's port block, so Forge does not tunnel it");
      break;
    default:
      lines.push(p.tunnel_detail || "Tunnel is connecting");
  }
  return lines.join("\n");
}

document.getElementById("ports-head").addEventListener("click", () =>
  setPortsCollapsed(!portsCollapsed()));

applyPortsCollapsed();
renderPorts();
refreshPorts();

// ---- chat -----------------------------------------------------------------
//
// The other way into the same Claude: a prompt goes to the workspace's own
// session and stream-json comes back. It exists because a phone cannot usefully
// render a TUI — the keyboard eats the screen and every redraw crosses the
// network — so this is the face of a session that survives being small.
//
// Everything below reads the stream and nothing else knows how: the host writes
// it to a file, the core pipes it, the server frames it one event per line, and
// this is the one place that has an opinion about what the objects mean. That is
// deliberate — the format is versioned by somebody else, and one place to be
// wrong about it is better than four.
const chat = {
  // ws -> {log: [rendered nodes are in the DOM; this is what to repaint], turn,
  //        es, live: the element text is streaming into}
  byWs: {},
};

function chatFor(ws) {
  if (!chat.byWs[ws]) chat.byWs[ws] = { turn: null, es: null, live: null, sent: false };
  return chat.byWs[ws];
}

// toggleChat is what the rail button calls. Chat and the terminal are two faces
// of one session, so this swaps which is showing rather than stacking anything.
function toggleChat() {
  if (!state.active) return;
  const panel = document.getElementById("chatpanel");
  const open = panel.hidden;
  panel.hidden = !open;
  setPanelActive(open ? "chat" : null);
  if (open) {
    document.getElementById("chatinput").focus();
  } else if (state.claude) {
    state.claude.term.focus();
  }
}

// chatAppend adds a node and keeps the view at the bottom, but only if it was
// already there: a reader scrolled up to something Claude said four tool calls
// ago is reading it, and yanking them back down is the one thing a log must not
// do.
function chatAppend(node) {
  const log = document.getElementById("chatlog");
  const atBottom = log.scrollHeight - log.scrollTop - log.clientHeight < 40;
  log.appendChild(node);
  if (atBottom) log.scrollTop = log.scrollHeight;
}

function chatNode(cls, text) {
  const d = document.createElement("div");
  d.className = cls;
  if (text !== undefined) d.textContent = text;
  return d;
}

// chatSend posts the prompt and opens the stream for the turn it starts.
async function chatSend(text) {
  const ws = state.active;
  if (!ws || !text.trim()) return;
  const c = chatFor(ws);
  if (c.es) return; // a turn is already running; the composer is disabled anyway

  const hint = document.getElementById("chat-empty");
  if (hint) hint.hidden = true;
  chatAppend(chatNode("chat-msg you", text));
  chatSetBusy(true);

  let turn;
  try {
    const res = await fetch(`/api/chat/${encodeURIComponent(ws)}/send`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ prompt: text }),
    });
    const body = await res.json().catch(() => ({}));
    if (!res.ok) throw new Error(body.error || `send failed (${res.status})`);
    turn = body.turn;
  } catch (e) {
    chatAppend(chatNode("chat-note bad", String(e.message || e)));
    chatSetBusy(false);
    return;
  }
  c.turn = turn;
  chatOpenStream(ws, turn);
}

// chatOpenStream follows a turn. The offset is not tracked here on purpose:
// every event carries the byte after it as its SSE id, and EventSource sends the
// last id it saw back as Last-Event-ID on its own reconnect — so a laptop that
// slept, or a phone that spent the turn in a tunnel, resumes at the byte it
// stopped on with nothing here to remember it.
function chatOpenStream(ws, turn) {
  const c = chatFor(ws);
  const es = new EventSource(`/api/chat/${encodeURIComponent(ws)}/${encodeURIComponent(turn)}/stream`);
  c.es = es;

  es.onmessage = (ev) => {
    let msg;
    try { msg = JSON.parse(ev.data); } catch { return; }
    if (ws === state.active) chatRender(c, msg);
  };
  es.addEventListener("done", () => chatEndStream(ws));
  es.addEventListener("error", (ev) => {
    let why = "the turn stopped being readable";
    try { why = JSON.parse(ev.data) || why; } catch { /* keep the default */ }
    chatAppend(chatNode("chat-note bad", why));
    chatEndStream(ws);
  });
  // onerror is the transport's, not the turn's: the browser reconnects by itself
  // and carries the offset with it, so there is nothing to do and nothing to say.
  es.onerror = () => {};
}

function chatEndStream(ws) {
  const c = chatFor(ws);
  if (c.es) c.es.close();
  c.es = null;
  c.turn = null;
  if (c.live) { c.live.classList.remove("live"); c.live = null; }
  if (ws === state.active) chatSetBusy(false);
}

function chatSetBusy(busy) {
  document.getElementById("chatsend").disabled = busy;
  document.getElementById("chatinput").disabled = busy;
  if (!busy) document.getElementById("chatinput").focus();
}

// chatRender turns one stream-json object into what it means on screen.
//
// Every branch is independent and every read is defensive, for the same reason
// the workspace's status-line script is: this runs against a format Claude Code
// versions on its own schedule, and a field that is renamed or missing must cost
// the one thing that came from it — never the conversation.
function chatRender(c, msg) {
  const type = msg && msg.type;

  // A partial message: the text as it is being written. Kept in one node that is
  // replaced when the finished message arrives, so nothing is shown twice.
  if (type === "stream_event") {
    const delta = (msg.event && msg.event.delta) || {};
    const text = typeof delta.text === "string" ? delta.text : "";
    if (!text) return;
    if (!c.live) {
      c.live = chatNode("chat-msg claude live", "");
      chatAppend(c.live);
    }
    c.live.textContent += text;
    const log = document.getElementById("chatlog");
    if (log.scrollHeight - log.scrollTop - log.clientHeight < 80) log.scrollTop = log.scrollHeight;
    return;
  }

  if (type === "assistant") {
    // The finished message replaces whatever streamed: the deltas were a preview
    // of exactly this, and appending both would say everything twice.
    if (c.live) { c.live.remove(); c.live = null; }
    for (const block of chatBlocks(msg)) {
      if (block.type === "text" && block.text) {
        chatAppend(chatNode("chat-msg claude", block.text));
      } else if (block.type === "tool_use") {
        chatAppend(chatToolNode(block));
      }
    }
    return;
  }

  if (type === "result") {
    const cost = typeof msg.total_cost_usd === "number" ? msg.total_cost_usd : null;
    const bits = [];
    if (msg.is_error) bits.push("ended with an error");
    if (cost !== null) bits.push(`$${cost.toFixed(4)}`);
    if (typeof msg.num_turns === "number") bits.push(`${msg.num_turns} turns`);
    if (bits.length) chatAppend(chatNode("chat-note", bits.join(" · ")));
  }
}

// chatBlocks digs the content out of an assistant message without assuming much
// about the wrapper: it has been message.content and content over the life of
// this format, and either is one array of blocks.
function chatBlocks(msg) {
  const content = (msg.message && msg.message.content) || msg.content;
  return Array.isArray(content) ? content : [];
}

// chatToolNode is one line for one thing Claude did. What it did to, not how:
// the argument that names a file or a command is the whole of what a reader
// wants at a glance, and the rest is in the terminal for anyone who needs it.
function chatToolNode(block) {
  const d = chatNode("chat-tool");
  const name = chatNode("name", String(block.name || "tool"));
  d.appendChild(name);
  const arg = chatToolArg(block.input);
  if (arg) d.appendChild(chatNode("arg", arg));
  return d;
}

function chatToolArg(input) {
  if (!input || typeof input !== "object") return "";
  for (const key of ["file_path", "path", "command", "pattern", "url", "prompt"]) {
    const v = input[key];
    if (typeof v === "string" && v) return v.length > 120 ? v.slice(0, 119) + "…" : v;
  }
  return "";
}

function initChat() {
  const form = document.getElementById("chatform");
  const input = document.getElementById("chatinput");
  if (!form || !input) return;

  form.addEventListener("submit", (e) => {
    e.preventDefault();
    const text = input.value;
    input.value = "";
    input.style.height = "auto";
    chatSend(text);
  });

  // Enter sends and Shift+Enter breaks the line, which is what every chat does
  // and therefore what fingers expect. The composer grows with the text instead
  // of scrolling inside four fixed rows, up to the height the CSS allows.
  input.addEventListener("keydown", (e) => {
    if (e.key === "Enter" && !e.shiftKey && !e.isComposing) {
      e.preventDefault();
      form.requestSubmit();
    }
  });
  input.addEventListener("input", () => {
    input.style.height = "auto";
    input.style.height = input.scrollHeight + "px";
  });
}

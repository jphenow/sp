package hub

// pageHTML is the whole dashboard: one file, no build step, no CDN.
//
// It is deliberately plain. The page is loaded on a phone, often on a bad
// connection, to do one of four things — wake a sprite, start Claude, hold it
// awake, hand it to Remote Control. Anything that makes those four slower or
// less obvious is not worth having.
const pageHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1, viewport-fit=cover">
<title>sp hub</title>
<style>
  :root {
    --bg: #0f1115; --card: #171a21; --line: #262b36; --text: #e6e9ef;
    --dim: #8b93a7; --accent: #6ea8fe; --ok: #4ec9a5; --warn: #e2b341; --off: #5a6273;
  }
  @media (prefers-color-scheme: light) {
    :root {
      --bg: #f5f6f8; --card: #ffffff; --line: #e2e5ea; --text: #1a1d23;
      --dim: #68707f; --accent: #2563eb; --ok: #0f8a63; --warn: #a66b00; --off: #9aa2b1;
    }
  }
  * { box-sizing: border-box; -webkit-tap-highlight-color: transparent; }
  body {
    margin: 0 auto; max-width: 1500px; padding: 0 16px 48px;
    background: var(--bg); color: var(--text);
    font: 15px/1.45 -apple-system, BlinkMacSystemFont, "Segoe UI", system-ui, sans-serif;
  }
  /* One column on a phone, because that is the screen the ladder buttons were
     sized for. On a desktop the same cards tile instead: a fleet this size is
     mostly idle sprites, and reading them as a single stack means scrolling
     past dozens of cards to reach the few that are running. */
  main { display: grid; gap: 10px; grid-template-columns: 1fr; align-items: start; }
  @media (min-width: 760px) {
    main { grid-template-columns: repeat(auto-fill, minmax(330px, 1fr)); }
  }
  header {
    position: sticky; top: 0; z-index: 2; background: var(--bg);
    padding: 16px 0 10px; border-bottom: 1px solid var(--line);
    display: flex; align-items: baseline; gap: 10px;
  }
  h1 { font-size: 17px; margin: 0; font-weight: 650; letter-spacing: -0.01em; }
  .meta { color: var(--dim); font-size: 12px; margin-left: auto; }
  .card {
    background: var(--card); border: 1px solid var(--line); border-radius: 12px;
    padding: 12px 14px;
  }
  .card.watched { border-color: var(--accent); }
  .row { display: flex; align-items: center; gap: 8px; }
  .name {
    font-weight: 600; font-size: 14px; word-break: break-all;
    font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
  }
  .pill {
    font-size: 11px; padding: 2px 8px; border-radius: 999px; white-space: nowrap;
    border: 1px solid currentColor; margin-left: auto;
  }
  .pill.running { color: var(--ok); }
  .pill.warm { color: var(--warn); }
  .pill.cold { color: var(--off); }
  .sub { color: var(--dim); font-size: 12px; margin-top: 4px; }
  .acts { display: flex; flex-wrap: wrap; gap: 6px; margin-top: 10px; }
  button {
    font: inherit; font-size: 13px; padding: 8px 12px; min-height: 38px;
    border-radius: 8px; border: 1px solid var(--line); background: transparent;
    color: var(--text); cursor: pointer;
  }
  button.primary { border-color: var(--accent); color: var(--accent); }
  button.on { border-color: var(--ok); color: var(--ok); }
  button:disabled { opacity: 0.45; cursor: default; }
  .sessions { margin-top: 8px; border-top: 1px solid var(--line); padding-top: 8px; }
  .session {
    display: flex; gap: 8px; align-items: flex-start; padding: 6px 0;
    border-bottom: 1px solid var(--line);
  }
  .session:last-child { border-bottom: 0; }
  .session .text { flex: 1; min-width: 0; }
  .session .sum { font-size: 12px; overflow-wrap: anywhere; }
  .session .when { color: var(--dim); font-size: 11px; }
  .session button { padding: 5px 10px; min-height: 0; font-size: 12px; }
  .status { font-size: 12px; margin-top: 8px; min-height: 16px; }
  .status.err { color: var(--warn); }
  .empty { color: var(--dim); padding: 32px 0; text-align: center; }
  details > summary { cursor: pointer; color: var(--dim); font-size: 12px; margin-top: 8px; }
</style>
</head>
<body>
<header>
  <h1>sp hub</h1>
  <span class="meta" id="meta">loading…</span>
</header>
<main id="list"><div class="empty">Loading sprites…</div></main>

<script>
const $ = (s, el) => (el || document).querySelector(s);
let vaultOn = false;

// ago renders a timestamp as a compact relative age.
function ago(iso) {
  const secs = Math.max(0, (Date.now() - new Date(iso).getTime()) / 1000);
  if (secs < 60) return Math.round(secs) + "s ago";
  if (secs < 3600) return Math.round(secs / 60) + "m ago";
  if (secs < 86400) return Math.round(secs / 3600) + "h ago";
  return Math.round(secs / 86400) + "d ago";
}

// rungOf names the highest ladder step a card reached. It mirrors the server's
// logic so the page can label a card without another round trip.
function rungOf(card) {
  if (card.status !== "running") return "asleep";
  const r = card.report;
  if (!r) return "awake";
  if (r.remote_ok) return "remote control";
  if (r.claude_ok) return "claude";
  if (r.tmux_ok) return "tmux";
  return "awake";
}

// act posts a ladder action and polls the resulting job to completion.
async function act(btn, sprite, action, extra) {
  const card = btn.closest(".card");
  const status = $(".status", card);
  const buttons = [...card.querySelectorAll("button")];
  buttons.forEach(b => b.disabled = true);
  status.className = "status";
  status.textContent = action + "…";

  try {
    const res = await fetch("/api/action", {
      method: "POST",
      headers: {"Content-Type": "application/json"},
      body: JSON.stringify(Object.assign({sprite, action}, extra || {})),
    });
    const body = await res.json();
    if (!res.ok) throw new Error(body.error || res.statusText);
    await poll(body.job, status);
  } catch (err) {
    status.className = "status err";
    status.textContent = String(err.message || err);
  } finally {
    buttons.forEach(b => b.disabled = false);
    refresh();
  }
}

// poll watches a background job until it finishes. Waking a cold sprite takes
// tens of seconds, so this is a slow poll rather than a held connection.
async function poll(id, status) {
  for (let i = 0; i < 120; i++) {
    await new Promise(r => setTimeout(r, 1500));
    const res = await fetch("/api/job?id=" + encodeURIComponent(id));
    if (!res.ok) continue;
    const job = await res.json();
    if (job.state === "done") { status.textContent = "done"; return; }
    if (job.state === "failed") throw new Error(job.error || "failed");
    status.textContent = job.key.split(":")[1] + "… " +
      Math.round((Date.now() - new Date(job.started).getTime()) / 1000) + "s";
  }
  throw new Error("timed out waiting for the job");
}

// sessionsHTML renders the resume list for a card.
function sessionsHTML(card) {
  const sessions = (card.report && card.report.sessions) || [];
  if (!sessions.length) return "";
  const rows = sessions.map(s =>
    '<div class="session"><div class="text">' +
      '<div class="sum">' + esc(s.summary || "(no opening message)") + "</div>" +
      '<div class="when">' + esc(s.dir || "") + " · " + ago(s.modified) + "</div>" +
    "</div>" +
    '<button data-act="claude" data-session="' + esc(s.id) + '" data-dir="' + esc(s.dir || "") + '">resume</button>' +
    "</div>"
  ).join("");
  return '<details class="sessions"><summary>' + sessions.length + " resumable session" +
    (sessions.length === 1 ? "" : "s") + "</summary>" + rows + "</details>";
}

// esc escapes a value for HTML interpolation.
function esc(s) {
  return String(s).replace(/[&<>"']/g, c =>
    ({"&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;"}[c]));
}

// cardHTML renders one sprite.
function cardHTML(card) {
  const rung = rungOf(card);
  const rep = card.report;
  const dir = (rep && rep.claude_dir) || "";

  const bits = [];
  if (rep) bits.push("reported " + ago(rep.reported_at));
  else if (card.status === "running") bits.push("no agent report");
  if (dir) bits.push(esc(dir));

  const acts = [];
  if (card.status !== "running") {
    acts.push('<button class="primary" data-act="wake">wake</button>');
  } else {
    acts.push('<button class="primary" data-act="claude" data-dir="' + esc(dir) + '">' +
      (rep && rep.claude_ok ? "new claude" : "start claude") + "</button>");
    acts.push('<button data-act="remote" data-dir="' + esc(dir) + '">remote control</button>');
  }
  // Offered on a cold sprite too: the action wakes before it rejoins, and a
  // sprite that has paused is exactly when this is needed — T3 drops it from
  // the phone and nothing on the phone can call it back.
  const t3On = rep && rep.t3_ok;
  acts.push('<button class="' + (t3On ? "on" : "primary") + '" data-act="t3">' +
    (t3On ? "t3 joined" : "join t3") + "</button>");
  const held = rep && rep.hold_ok;
  acts.push('<button class="' + (held ? "on" : "") + '" data-act="hold" data-on="' +
    (held ? "false" : "true") + '">' + (held ? "holding" : "hold awake") + "</button>");
  if (vaultOn) acts.push('<button data-act="credentials">fix auth</button>');
  acts.push('<button class="' + (card.watched ? "on" : "") + '" data-act="watch" data-on="' +
    (card.watched ? "false" : "true") + '">' + (card.watched ? "watched" : "watch") + "</button>");

  return '<div class="card' + (card.watched ? " watched" : "") + '" data-sprite="' + esc(card.name) + '">' +
    '<div class="row"><span class="name">' + esc(card.name) + "</span>" +
    '<span class="pill ' + esc(card.status) + '">' + esc(rung) + "</span></div>" +
    (bits.length ? '<div class="sub">' + bits.join(" · ") + "</div>" : "") +
    '<div class="acts">' + acts.join("") + "</div>" +
    '<div class="status"></div>' +
    sessionsHTML(card) +
    "</div>";
}

// refresh reloads the card list.
async function refresh() {
  try {
    const res = await fetch("/api/state");
    const body = await res.json();
    if (!res.ok) throw new Error(body.error || res.statusText);
    vaultOn = !!body.vault;
    const cards = body.cards || [];
    const awake = cards.filter(c => c.status === "running").length;
    $("#meta").textContent = awake + " awake · " + cards.length + " total";
    $("#list").innerHTML = cards.length
      ? cards.map(cardHTML).join("")
      : '<div class="empty">No sprites in this org.</div>';
  } catch (err) {
    $("#meta").textContent = "error: " + (err.message || err);
  }
}

// One delegated listener survives every re-render, so buttons keep working
// after a refresh replaces the DOM.
document.addEventListener("click", e => {
  const btn = e.target.closest("button[data-act]");
  if (!btn) return;
  const sprite = btn.closest(".card").dataset.sprite;
  act(btn, sprite, btn.dataset.act, {
    session_id: btn.dataset.session || "",
    dir: btn.dataset.dir || "",
    on: btn.dataset.on === "true",
  });
});

refresh();
setInterval(() => { if (!document.hidden) refresh(); }, 20000);
</script>
</body>
</html>
`

"use strict";
/* Fire Air Hockey client — pure vanilla JS, no dependencies.
 *
 * Server speaks canonical table units (x: 0..100, y: 0..200, seat 0 defends
 * y=200). This client always shows YOU at the bottom: seat 1 mirrors y both
 * when rendering and (implicitly) when aiming, because pointer positions are
 * read in screen space, which is already the player's own view.
 */
const $ = (id) => document.getElementById(id);
const clamp01 = (v) => Math.max(0, Math.min(1, v));

const store = {
  nick: localStorage.getItem("fah_nick") || "",
  seat: -2,          // -2 lobby, -1 spectator, 0/1 player
  room: null,        // last {t:"room"} payload
  snap: null,        // last {t:"snap"} payload
  snapAt: 0,
  prevScores: [0, 0],
  lastCount: -1,
  rematchPending: false,
  lastRoomId: null,
};

let ws = null;
let retryTimer = null;

/* ---------- tiny sound kit (WebAudio, created on first gesture) ---------- */
let AC = null;
function audio() {
  if (!AC) { try { AC = new (window.AudioContext || window.webkitAudioContext)(); } catch (e) { /* no audio */ } }
  if (AC && AC.state === "suspended") { AC.resume().catch(() => {}); }
  return AC;
}
function beep(freq, dur, delay = 0, type = "sine", vol = 0.12) {
  const ac = audio();
  if (!ac) return;
  try {
    const t = ac.currentTime + delay;
    const o = ac.createOscillator(), g = ac.createGain();
    o.type = type; o.frequency.value = freq;
    g.gain.setValueAtTime(vol, t);
    g.gain.exponentialRampToValueAtTime(0.001, t + dur);
    o.connect(g); g.connect(ac.destination);
    o.start(t); o.stop(t + dur + 0.02);
  } catch (e) { /* ignore */ }
}
const sfx = {
  goalMine() { beep(523, 0.12); beep(784, 0.18, 0.1); },
  goalFoe() { beep(392, 0.12); beep(262, 0.2, 0.1); },
  tick() { beep(660, 0.07, 0, "square", 0.05); },
  win() { beep(523, 0.12); beep(659, 0.12, 0.12); beep(784, 0.25, 0.24); },
  click() { beep(440, 0.05, 0, "square", 0.04); },
};
window.addEventListener("pointerdown", () => audio(), { once: true });

/* ---------- connection ---------- */
function wsURL() {
  return (location.protocol === "https:" ? "wss://" : "ws://") + location.host + "/ws";
}
function setConn(on) {
  const el = $("conn");
  el.textContent = on ? "● live" : "● offline";
  el.className = "conn " + (on ? "on" : "off");
  refreshLobbyButtons();
}
function send(obj) {
  if (ws && ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify(obj));
}
function connect() {
  if (ws && (ws.readyState === WebSocket.OPEN || ws.readyState === WebSocket.CONNECTING)) return;
  setConn(false);
  ws = new WebSocket(wsURL());
  ws.onopen = () => {
    setConn(true);
    if (retryTimer) { clearTimeout(retryTimer); retryTimer = null; }
    if (store.nick) {
      const hello = { t: "hello", name: store.nick };
      const pending = store.lastRoomId || pendingRoomFromURL();
      if (pending) { hello.room = pending; store.joinPending = true; }
      send(hello);
    }
  };
  ws.onmessage = (ev) => route(JSON.parse(ev.data));
  ws.onclose = () => {
    setConn(false);
    if (!retryTimer) retryTimer = setTimeout(() => { retryTimer = null; connect(); }, 1500);
  };
  ws.onerror = () => { try { ws.close(); } catch (e) {} };
}
function pendingRoomFromURL() {
  const m = location.pathname.match(/^\/r\/([A-Za-z0-9]+)/);
  if (m) return m[1].toUpperCase();
  const q = new URLSearchParams(location.search).get("room");
  return q ? q.toUpperCase() : null;
}

/* ---------- routing ---------- */
function route(msg) {
  switch (msg.t) {
    case "welcome": onWelcome(msg); break;
    case "lobby": onLobby(msg); break;
    case "room": onRoom(msg); break;
    case "snap": onSnap(msg); break;
    case "error": onError(msg); break;
  }
}
function onWelcome(msg) {
  $("who").textContent = msg.name;
  // hello with a room triggers onRoom shortly; otherwise lobby arrives.
}
function onLobby(msg) {
  renderLobby(msg.rooms || []);
  if (!store.room) show("view-lobby");
}
function onRoom(msg) {
  const first = !store.room;
  store.room = msg;
  store.seat = msg.you;
  store.creating = false;
  store.joinPending = false;
  refreshLobbyButtons();
  if (msg.phase === "wait" || msg.phase === "count" || msg.phase === "play") store.rematchPending = false;
  store.lastRoomId = msg.id;
  history.replaceState({}, "", "/r/" + msg.id);
  show("view-game");
  updateRoomUI();
  if (first) {
    store.snap = null;
    store.prevScores = scoresOf(msg);
    // Park the mallet at home so the first input isn't a jump.
    sendPaddle(0.5, 0.85, true);
  }
}
function onSnap(msg) {
  const first = !store.snap;
  store.snap = msg;
  store.snapAt = performance.now();
  if (!first) {
    for (let s = 0; s < 2; s++) {
      if (msg.sc[s] > store.prevScores[s]) onGoal(s);
    }
  }
  store.prevScores = [msg.sc[0], msg.sc[1]];
  updateLiveUI(msg);
  if (!store.room) { store.room = { phase: msg.ph, id: store.lastRoomId }; show("view-game"); updateRoomUI(); }
}
function onGoal(scorer) {
  const mine = scorer === store.seat;
  flash(mine ? "GOAL!" : "CONCEDED", !mine);
  if (mine) sfx.goalMine(); else sfx.goalFoe();
}
function onError(msg) {
  toast(msg.msg || "Something went wrong");
  store.creating = false;
  refreshLobbyButtons();
  if (store.joinPending) {
    // Our join failed (e.g. stale link): don't linger on a dead table.
    store.joinPending = false;
    backToLobby();
  } else if (!store.room && store.nick) show("view-lobby");
}

/* ---------- screens ---------- */
function show(id) {
  for (const v of ["view-nick", "view-lobby", "view-game"]) $(v).hidden = v !== id;
}
function toast(text) {
  const el = $("toast");
  el.textContent = text;
  el.hidden = false;
  clearTimeout(toast._t);
  toast._t = setTimeout(() => { el.hidden = true; }, 2600);
}

/* ---------- lobby ---------- */
function renderLobby(rooms) {
  $("room-count").textContent = rooms.length ? `(${rooms.length})` : "";
  const box = $("rooms");
  box.innerHTML = "";
  if (!rooms.length) {
    box.innerHTML = '<p class="muted">No public tables right now — create one above.</p>';
    return;
  }
  for (const r of rooms) {
    const row = document.createElement("div");
    row.className = "room-row";
    const live = !!r.playing;
    const info = document.createElement("div");
    info.className = "info";
    const title = document.createElement("div");
    title.className = "title";
    title.textContent = r.title || "Untitled table";
    const sub = document.createElement("div");
    sub.className = "sub";
    sub.textContent = `${r.host || "—"} · ${r.players}/2 players`;
    info.append(title, sub);
    const badge = document.createElement("span");
    badge.className = "badge " + (live ? "live" : "open");
    badge.textContent = live ? "LIVE" : (r.players >= 2 ? "FULL" : "OPEN");
    const link = document.createElement("button");
    link.className = "btn small";
    link.textContent = "Copy link";
    link.onclick = () => copyLink(r.id);
    const join = document.createElement("button");
    join.className = "btn small primary";
    join.textContent = "Join";
    join.onclick = () => { sfx.click(); send({ t: "join", room: r.id }); };
    row.append(info, badge, link, join);
    box.append(row);
  }
}
function inviteURL(id) { return location.origin + "/r/" + id; }
function copyLink(id) {
  const url = inviteURL(id);
  const done = () => toast("Invite link copied");
  if (navigator.clipboard && navigator.clipboard.writeText) {
    navigator.clipboard.writeText(url).then(done, () => fallbackCopy(url, done));
  } else fallbackCopy(url, done);
}
function fallbackCopy(text, done) {
  const ta = document.createElement("textarea");
  ta.value = text;
  document.body.appendChild(ta);
  ta.select();
  try { document.execCommand("copy"); done(); } catch (e) { toast(text); }
  document.body.removeChild(ta);
}

/* ---------- room / game UI ---------- */
function playerName(i) {
  const p = store.room && store.room.players && store.room.players[i];
  return p ? p.name : null;
}
function scoresOf(roomMsg) {
  const s = [0, 0];
  for (let i = 0; i < 2; i++) {
    const p = roomMsg.players && roomMsg.players[i];
    if (p) s[i] = p.score || 0;
  }
  return s;
}
// Seat shown at the TOP of my screen (the opponent side), or 1 for spectator view.
function topSeat() { return store.seat === 1 ? 0 : 1; }
function bottomSeat() { return store.seat === 1 ? 1 : 0; }

function scoreCard(el, seatIdx, tag) {
  const name = playerName(seatIdx);
  const sc = store.snap ? store.snap.sc[seatIdx] : scoresOf(store.room)[seatIdx];
  el.innerHTML = "";
  const nm = document.createElement("span");
  nm.className = "nm";
  nm.textContent = name || (seatIdx === store.seat ? "…" : "Waiting…");
  const score = document.createElement("span");
  score.className = "sc";
  score.textContent = sc;
  el.append(nm);
  if (tag) { const t = document.createElement("span"); t.className = "you-tag"; t.textContent = tag; el.append(t); }
  el.append(score);
  el.classList.toggle("me", seatIdx === store.seat);
  el.classList.toggle("foe", seatIdx !== store.seat);
}

function updateRoomUI() {
  const r = store.room;
  if (!r) return;
  const iAmSeated = store.seat === 0 || store.seat === 1;
  const other = iAmSeated ? r.players && r.players[1 - store.seat] : null;

  // Waiting overlay: invite link, who's here, CPU fallback.
  $("invite-link").value = inviteURL(r.id);
  const wp = $("wait-players");
  wp.innerHTML = "";
  for (let i = 0; i < 2; i++) {
    const p = r.players && r.players[i];
    const d = document.createElement("div");
    d.className = "p";
    d.textContent = p
      ? p.name + (i === store.seat ? " (you)" : "") + (p.bot ? "  ·  CPU" : "")
      : "Waiting for player…";
    wp.append(d);
  }
  $("btn-addcpu").style.display = (r.phase === "wait" && iAmSeated && !other) ? "" : "none";

  // Control bar under the table: exactly one copy button, one leave button.
  $("table-code").textContent = "Table " + r.id;
  $("game-status").textContent =
    r.phase === "wait" ? "Share the link so someone joins you"
    : r.phase === "over" ? ""
    : "First to 7 wins";
  // The waiting overlay already has a copy button; don't show two at once.
  $("btn-copylink").style.display = r.phase === "wait" ? "none" : "";

  // Game-over overlay.
  const over = r.phase === "over";
  $("ov-over").hidden = !over;
  if (over) {
    const w = r.winner;
    const s = scoresOf(r);
    const left = !!r.reason;
    if (store.seat < 0) {
      $("over-title").textContent = w >= 0 ? `${playerName(w) || "Someone"} wins` : "Game over";
    } else if (left) {
      $("over-title").textContent = w === store.seat ? "Opponent left" : `${playerName(w) || "Opponent"} wins`;
    } else {
      $("over-title").textContent = w === store.seat ? "You win!" : `${playerName(w) || "Opponent"} wins`;
    }
    let sub = `${s[0]} – ${s[1]}`;
    if (left) sub += w === store.seat ? " · you take the win" : " · opponent left";
    if (!other && iAmSeated) sub += " · share the link for a new challenger";
    $("over-sub").textContent = sub;
    // Rematch only makes sense when an opponent is still here.
    const canRematch = iAmSeated && !!other;
    $("btn-rematch").style.display = canRematch ? "" : "none";
    $("btn-rematch").disabled = !canRematch;
    $("btn-rematch").textContent = store.rematchPending ? "Waiting…" : "Rematch";
    if (w === store.seat && !updateRoomUI._winned) sfx.win();
    updateRoomUI._winned = true;
  } else updateRoomUI._winned = false;
  $("ov-wait").hidden = r.phase !== "wait";
  scoreCard($("score-top"), topSeat(), topSeat() === store.seat ? "YOU" : "");
  scoreCard($("score-bottom"), bottomSeat(),
    bottomSeat() === store.seat ? "YOU" : (store.seat === -1 ? "SPEC" : ""));
}

// Per-snapshot (30 Hz) lightweight updates: scores, countdown, overlays.
function updateLiveUI(msg) {
  if (store.room) {
    store.room.phase = msg.ph;
    if (store.room.players) {
      for (let i = 0; i < 2; i++) if (store.room.players[i]) store.room.players[i].score = msg.sc[i];
    }
  }
  scoreCard($("score-top"), topSeat(), topSeat() === store.seat ? "YOU" : "");
  scoreCard($("score-bottom"), bottomSeat(),
    bottomSeat() === store.seat ? "YOU" : (store.seat === -1 ? "SPEC" : ""));
  const counting = msg.ph === "count";
  $("ov-count").hidden = !counting;
  if (counting) {
    const n = Math.max(1, Math.ceil(msg.count));
    $("count-num").textContent = n;
    if (n !== store.lastCount) { store.lastCount = n; sfx.tick(); }
  } else store.lastCount = -1;
  if (msg.ph === "wait") { $("ov-wait").hidden = false; $("ov-over").hidden = true; }
  else if (msg.ph === "over") {
    if (store.room) { store.room.winner = msg.winner; }
    updateRoomUI();
  } else {
    $("ov-over").hidden = true;
    if (msg.ph === "play" && store.room) updateRoomUITimersOnly();
  }
}
// During play we only need the countdown overlay hidden; avoid full rebuilds.
function updateRoomUITimersOnly() { $("ov-count").hidden = true; }

let flashTimer = null;
function flash(text, bad) {
  const ov = $("ov-flash"), t = $("flash-text");
  t.textContent = text;
  t.className = "flash-text" + (bad ? " bad" : "");
  ov.hidden = false;
  clearTimeout(flashTimer);
  flashTimer = setTimeout(() => { ov.hidden = true; }, 1100);
}

/* ---------- canvas renderer ---------- */
const cv = $("table");
const ctx = cv.getContext("2d");
const LW = 500, LH = 1000, S = 5, RAIL = 20; // logical px; S maps 100x200 units
let trail = [];

function fitCanvas() {
  const dpr = Math.min(2, window.devicePixelRatio || 1);
  cv.width = LW * dpr;
  cv.height = LH * dpr;
  ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
}
window.addEventListener("resize", fitCanvas);
fitCanvas();

// Screen mapping for the current orientation.
function X(cx) { return RAIL + (cx / 100) * (LW - 2 * RAIL); }
function Y(cy) {
  const v = RAIL + (cy / 200) * (LH - 2 * RAIL);
  return store.seat === 1 ? LH - v : v;
}
function Y01(cy) { return Y(cy) / LH; }

const COL = { you: "#ff5a1f", foe: "#46d9ff", line: "#2b6b5e", dim: "#8fa9a2" };

function drawTable() {
  ctx.clearRect(0, 0, LW, LH);
  ctx.fillStyle = "#0b1512";
  ctx.fillRect(0, 0, LW, LH);

  // Rails with goal mouths top/bottom.
  const mouth = (15 / 100) * (LW - 2 * RAIL); // GoalHalf*2 in px
  const cx0 = LW / 2 - mouth, cx1 = LW / 2 + mouth;
  ctx.strokeStyle = "#3a3129";
  ctx.lineWidth = RAIL;
  ctx.lineCap = "butt";
  const yT = RAIL / 2, yB = LH - RAIL / 2;
  // top rail segments
  ctx.beginPath(); ctx.moveTo(0, yT); ctx.lineTo(cx0, yT); ctx.stroke();
  ctx.beginPath(); ctx.moveTo(cx1, yT); ctx.lineTo(LW, yT); ctx.stroke();
  // bottom rail segments
  ctx.beginPath(); ctx.moveTo(0, yB); ctx.lineTo(cx0, yB); ctx.stroke();
  ctx.beginPath(); ctx.moveTo(cx1, yB); ctx.lineTo(LW, yB); ctx.stroke();
  // side rails
  ctx.beginPath(); ctx.moveTo(RAIL / 2, 0); ctx.lineTo(RAIL / 2, LH); ctx.stroke();
  ctx.beginPath(); ctx.moveTo(LW - RAIL / 2, 0); ctx.lineTo(LW - RAIL / 2, LH); ctx.stroke();
  // glowing goal mouths
  const gyT = Y(0), gyB = Y(200);
  ctx.strokeStyle = COL.foe; ctx.lineWidth = 5;
  ctx.beginPath(); ctx.moveTo(cx0, gyT); ctx.lineTo(cx1, gyT); ctx.stroke();
  ctx.strokeStyle = COL.you;
  ctx.beginPath(); ctx.moveTo(cx0, gyB); ctx.lineTo(cx1, gyB); ctx.stroke();

  // Markings (orientation-independent: center is center).
  ctx.strokeStyle = COL.line; ctx.lineWidth = 4;
  ctx.beginPath(); ctx.moveTo(RAIL, LH / 2); ctx.lineTo(LW - RAIL, LH / 2); ctx.stroke();
  ctx.strokeStyle = COL.you;
  ctx.beginPath(); ctx.arc(LW / 2, LH / 2, 62, 0, Math.PI * 2); ctx.stroke();
  ctx.fillStyle = COL.you;
  ctx.beginPath(); ctx.arc(LW / 2, LH / 2, 6, 0, Math.PI * 2); ctx.fill();
  // creases
  ctx.strokeStyle = COL.line;
  ctx.beginPath(); ctx.arc(LW / 2, gyT, 58, 0.15 * Math.PI, 0.85 * Math.PI); ctx.stroke();
  ctx.beginPath(); ctx.arc(LW / 2, gyB, 58, 1.15 * Math.PI, 1.85 * Math.PI); ctx.stroke();
  // faceoff dots
  ctx.fillStyle = COL.dim;
  for (const [fx, fy] of [[30, 55], [70, 55], [30, 145], [70, 145]]) {
    ctx.beginPath(); ctx.arc(X(fx), Y(fy), 5, 0, Math.PI * 2); ctx.fill();
  }
  // name labels
  ctx.font = "700 17px sans-serif";
  ctx.textAlign = "center";
  ctx.fillStyle = "#7d8b87";
  const top = playerName(topSeat()), bot = playerName(bottomSeat());
  ctx.fillText(top || "Waiting for opponent…", LW / 2, Y(18) + 6);
  ctx.fillText(bottomSeat() === store.seat ? `YOU · ${bot || "…"}` : (bot || "…"), LW / 2, Y(182) - 10);
}

function drawSprite(sx, sy, rad, color, dome) {
  const k = 0.94 + 0.12 * (sy / LH); // subtle perspective: nearer = bigger
  const r = rad * k;
  ctx.fillStyle = "rgba(0,0,0,0.45)";
  ctx.beginPath(); ctx.ellipse(sx + 4, sy + 7, r, r * 0.9, 0, 0, Math.PI * 2); ctx.fill();
  ctx.beginPath(); ctx.arc(sx, sy, r, 0, Math.PI * 2);
  ctx.fillStyle = "#16130f"; ctx.fill();
  ctx.lineWidth = 5; ctx.strokeStyle = color; ctx.stroke();
  if (dome) {
    ctx.beginPath(); ctx.arc(sx, sy, r * 0.55, 0, Math.PI * 2);
    ctx.fillStyle = color; ctx.fill();
    ctx.beginPath(); ctx.arc(sx - r * 0.15, sy - r * 0.18, r * 0.16, 0, Math.PI * 2);
    ctx.fillStyle = "rgba(255,255,255,0.85)"; ctx.fill();
  } else {
    ctx.save();
    ctx.shadowColor = color; ctx.shadowBlur = 18;
    ctx.beginPath(); ctx.arc(sx, sy, r * 0.62, 0, Math.PI * 2);
    ctx.fillStyle = color; ctx.fill();
    ctx.restore();
  }
}

function render(now) {
  requestAnimationFrame(render);
  if ($("view-game").hidden) return;
  drawTable();
  const s = store.snap;
  if (!s) return;
  // Puck with velocity interpolation between 30 Hz snapshots.
  const dt = Math.min(0.15, (now - store.snapAt) / 1000);
  const px = s.puck[0] + s.puck[2] * dt;
  const py = s.puck[1] + s.puck[3] * dt;
  const sx = X(px), sy = Y(py);
  trail.push([sx, sy]);
  if (trail.length > 14) trail.shift();
  for (let i = 0; i < trail.length; i++) {
    const a = (i / trail.length) * 0.35;
    ctx.fillStyle = `rgba(255,122,60,${a.toFixed(3)})`;
    ctx.beginPath(); ctx.arc(trail[i][0], trail[i][1], 4 + (i / trail.length) * 9, 0, Math.PI * 2); ctx.fill();
  }
  drawSprite(sx, sy, 3.0 * S, "#ff7a3c", false);
  // Paddles.
  for (let i = 0; i < 2; i++) {
    const p = s.pads[i];
    if (!p) continue;
    const color = i === store.seat ? COL.you : COL.foe;
    drawSprite(X(p[0]), Y(p[1]), 5.5 * S, color, true);
    if (i === store.seat) {
      ctx.strokeStyle = "rgba(255,255,255,0.5)";
      ctx.lineWidth = 2;
      ctx.beginPath(); ctx.arc(X(p[0]), Y(p[1]), 5.5 * S * 1.25, 0, Math.PI * 2); ctx.stroke();
    }
  }
}
requestAnimationFrame(render);

/* ---------- input: pointer position is already in own-view space ---------- */
let lastSent = 0, lastXY = null;
function pointerToOwnView(ev) {
  const rect = cv.getBoundingClientRect();
  const fx = clamp01((ev.clientX - rect.left) / rect.width);
  const fy = clamp01((ev.clientY - rect.top) / rect.height);
  // Stay on your own half (own-view y in [0.5, 1]).
  return [fx, Math.max(0.5, fy)];
}
function sendPaddle(fx, fy, force) {
  const now = performance.now();
  if (!force && now - lastSent < 15) return;
  if (!force && lastXY && Math.abs(lastXY[0] - fx) < 0.002 && Math.abs(lastXY[1] - fy) < 0.002) return;
  lastSent = now; lastXY = [fx, fy];
  send({ t: "paddle", x: +fx.toFixed(4), y: +fy.toFixed(4) });
}
let dragging = false;
cv.addEventListener("pointerdown", (ev) => {
  if (store.seat !== 0 && store.seat !== 1) return;
  dragging = true;
  try { cv.setPointerCapture(ev.pointerId); } catch (e) {}
  const [fx, fy] = pointerToOwnView(ev);
  sendPaddle(fx, fy, true);
});
cv.addEventListener("pointermove", (ev) => {
  if (!dragging || (store.seat !== 0 && store.seat !== 1)) return;
  const [fx, fy] = pointerToOwnView(ev);
  sendPaddle(fx, fy, false);
});
for (const ev of ["pointerup", "pointercancel", "pointerleave"]) {
  cv.addEventListener(ev, () => { dragging = false; });
}

/* ---------- buttons ---------- */
$("btn-enter").onclick = () => {
  const v = $("nick").value.trim();
  if (!v) { toast("Pick a nickname first"); return; }
  store.nick = cleanClientName(v);
  localStorage.setItem("fah_nick", store.nick);
  $("who").textContent = store.nick;
  sfx.click();
  connect();
  if (ws && ws.readyState === WebSocket.OPEN) {
    const hello = { t: "hello", name: store.nick };
    const p = pendingRoomFromURL();
    if (p) { hello.room = p; store.joinPending = true; }
    send(hello);
  }
  if (pendingRoomFromURL()) { store.joinPending = true; show("view-game"); } else show("view-lobby");
};
$("nick").addEventListener("keydown", (e) => { if (e.key === "Enter") $("btn-enter").click(); });

function cleanClientName(s) { return s.replace(/\s+/g, " ").trim().slice(0, 16); }

// One in-flight create at a time: guards against double-click ghost tables.
// Never fail silently: an offline click explains itself instead of looking dead.
function needConn() {
  if (connected()) return true;
  toast("Still connecting to the server — try again in a moment");
  return false;
}
function createGuarded(payload) {
  if (store.creating) return;
  if (!needConn()) return;
  store.creating = true;
  refreshLobbyButtons();
  sfx.click();
  send(payload);
  setTimeout(() => { store.creating = false; refreshLobbyButtons(); }, 5000);
}
$("btn-public").onclick = () => createGuarded({ t: "create", title: $("room-title").value.trim(), public: true });
$("btn-private").onclick = () => createGuarded({ t: "create", title: $("room-title").value.trim(), public: false });
$("btn-cpu").onclick = () => createGuarded({ t: "create", title: "Practice", public: false, cpu: true });
$("btn-join").onclick = () => {
  if (!needConn()) return;
  const code = extractCode($("join-code").value);
  if (!code) { toast("Paste an invite link or table code"); return; }
  store.joinPending = true;
  sfx.click(); send({ t: "join", room: code });
};
function connected() { return !!(ws && ws.readyState === WebSocket.OPEN); }
function refreshLobbyButtons() {
  const off = !connected() || store.creating;
  for (const id of ["btn-public", "btn-private", "btn-cpu", "btn-join"]) $(id).disabled = off;
  $("lobby-offline").hidden = connected();
}
$("join-code").addEventListener("keydown", (e) => { if (e.key === "Enter") $("btn-join").click(); });
function extractCode(s) {
  s = (s || "").trim();
  if (!s) return null;
  const m = s.match(/\/r\/([A-Za-z0-9]+)/) || s.match(/[?&]room=([A-Za-z0-9]+)/);
  if (m) return m[1].toUpperCase();
  const raw = s.replace(/[^A-Za-z0-9]/g, "").toUpperCase();
  return raw || null;
}

$("btn-copy").onclick = () => { if (store.room) copyLink(store.room.id); };
$("btn-copylink").onclick = () => { if (store.room) copyLink(store.room.id); };
$("btn-addcpu").onclick = () => { if (!needConn()) return; sfx.click(); send({ t: "addcpu" }); };
function requestRematch() {
  if (!needConn()) return;
  sfx.click();
  store.rematchPending = true;
  send({ t: "rematch" });
  updateRoomUI();
}
$("btn-rematch").onclick = requestRematch;
function backToLobby() {
  sfx.click();
  send({ t: "leave" });
  store.room = null; store.snap = null; store.seat = -2; store.lastRoomId = null;
  trail = [];
  history.replaceState({}, "", "/");
  show("view-lobby");
}
$("btn-lobby").onclick = backToLobby;
$("btn-leave").onclick = backToLobby;

/* ---------- boot ---------- */
(function boot() {
  const pending = pendingRoomFromURL();
  if (store.nick) {
    $("who").textContent = store.nick;
    store.joinPending = !!pending;
    connect();
    refreshLobbyButtons();
    show(pending ? "view-game" : "view-lobby");
  } else {
    show("view-nick");
    $("nick").value = "";
    if (pending) {
      $("join-note").hidden = false;
      $("join-note").textContent = `You've been invited to table ${pending} — enter a nickname to join.`;
    }
    setTimeout(() => $("nick").focus(), 50);
  }
})();

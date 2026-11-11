/* WatchFilms client. No framework, no build step. */
"use strict";
const $ = (id) => document.getElementById(id);
const video = $("video");
const SEEK_STEP = 10;

const S = {
  ws: null, you: null, host: false, media: null, state: null, peers: [],
  offset: 0, bestRtt: Infinity,       // serverTime ≈ Date.now() + offset
  name: localStorage.getItem("wf_name") || "",
  joined: false, reportedBuf: false, bufSince: 0,
  hardSeekAt: 0, autoplayBlocked: false, reconnectDelay: 500,
  p2pMode: false, p2pPc: null, p2pControl: null, p2pMedia: null,
};

/* ---------- helpers ---------- */
const serverNow = () => Date.now() + S.offset;
function fmt(t) {
  if (!isFinite(t) || t < 0) t = 0;
  t = Math.floor(t);
  const h = Math.floor(t / 3600), m = Math.floor((t % 3600) / 60), s = t % 60;
  const ss = String(s).padStart(2, "0");
  return h ? `${h}:${String(m).padStart(2, "0")}:${ss}` : `${m}:${ss}`;
}
function fmtSize(b) {
  const u = ["Б", "КБ", "МБ", "ГБ", "ТБ"]; let i = 0;
  while (b >= 1024 && i < u.length - 1) { b /= 1024; i++; }
  return `${b.toFixed(i > 1 ? 1 : 0)} ${u[i]}`;
}
function colorFor(id) {
  let h = 0; for (const c of String(id)) h = (h * 31 + c.charCodeAt(0)) % 360;
  return `hsl(${h} 75% 68%)`;
}
function el(tag, cls, text) {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (text != null) e.textContent = text;
  return e;
}
function expectedPos(st = S.state) {
  if (!st) return 0;
  if (st.paused || st.waiting) return st.position;
  return st.position + ((serverNow() - st.updatedAt) / 1000) * st.rate;
}
const peerName = (id) => (S.peers.find((p) => p.id === id) || {}).name || "Кто-то";

/* ---------- connection ---------- */
const STUN_SERVERS = [
  { urls: "stun:stun.yandex.ru:3478" },
  { urls: "stun:stun.l.google.com:19302" },
  { urls: "stun:stun1.l.google.com:19302" },
  { urls: "stun:stun.cloudflare.com:3478" },
];

function swPost(msg, transfer) {
  if (navigator.serviceWorker && navigator.serviceWorker.controller) {
    if (transfer) navigator.serviceWorker.controller.postMessage(msg, transfer);
    else navigator.serviceWorker.controller.postMessage(msg);
  }
}

const basePath = new URL(".", location.href).pathname.replace(/\/+$/, "");
const sigParam = new URLSearchParams(location.search).get("signal");
if (sigParam) {
  try { localStorage.setItem("wf_signal_url", sigParam); } catch (_) {}
}

if ("serviceWorker" in navigator && !location.hostname.includes("functions.yandexcloud.net")) {
  const swPath = new URL("sw.js", location.href).pathname;
  navigator.serviceWorker.register(swPath).catch((err) => {
    console.warn("SW register:", err);
  });
  navigator.serviceWorker.addEventListener("message", (e) => {
    const d = e.data;
    if (!d || !S.p2pMedia || S.p2pMedia.readyState !== "open") return;
    if (d.type === "p2p_request_range") {
      S.p2pMedia.send(JSON.stringify({ id: d.reqId, type: "range", start: d.start, end: d.end }));
    } else if (d.type === "p2p_request_subs") {
      S.p2pMedia.send(JSON.stringify({ id: d.reqId, type: "subs", n: d.n }));
    } else if (d.type === "p2p_cancel") {
      S.p2pMedia.send(JSON.stringify({ id: d.reqId, type: "cancel" }));
    }
  });
}

let directReqCounter = 0;
let currentDirectReqId = 0;
const p2pSubsCallbacks = new Map();

function onMediaChannelMessage(e) {
  if (typeof e.data === "string") {
    try {
      const m = JSON.parse(e.data);
      if (S.p2pDirectCallback) {
        if (m.type === "meta" && S.p2pDirectCallback.onMeta) S.p2pDirectCallback.onMeta(m);
        else if (m.type === "error" && S.p2pDirectCallback.onError) S.p2pDirectCallback.onError(m.error || "ошибка");
      }
      if (m.type === "subs" && p2pSubsCallbacks.has(m.id)) {
        const cb = p2pSubsCallbacks.get(m.id);
        p2pSubsCallbacks.delete(m.id);
        if (m.content) cb(m.content);
      }
      if (m.type === "meta") swPost({ type: "meta", reqId: m.id, ...m });
      else if (m.type === "subs") swPost({ type: "subs", reqId: m.id, ...m });
      else if (m.type === "error") swPost({ type: "error", reqId: m.id, ...m });
    } catch (_) {}
    return;
  }
  const dv = new DataView(e.data);
  const reqId = dv.getUint32(0);
  const flag = dv.getUint8(4);
  if (flag === 0) {
    const chunk = e.data.slice(5);
    if (S.p2pDirectCallback) S.p2pDirectCallback.onChunk(chunk);
    swPost({ type: "chunk", reqId: reqId, chunk: chunk }, [chunk]);
  } else if (flag === 1) {
    if (S.p2pDirectCallback) S.p2pDirectCallback.onDone();
    swPost({ type: "done", reqId: reqId });
  } else if (flag === 2) {
    if (S.p2pDirectCallback) S.p2pDirectCallback.onError("ошибка потока");
    swPost({ type: "error", reqId: reqId, error: "stream error" });
  }
}

function loadP2PSubsDirect(subIdx, cb) {
  if (!S.p2pMedia || S.p2pMedia.readyState !== "open") {
    setTimeout(() => loadP2PSubsDirect(subIdx, cb), 800);
    return;
  }
  const reqId = ++directReqCounter;
  p2pSubsCallbacks.set(reqId, cb);
  S.p2pMedia.send(JSON.stringify({ id: reqId, type: "subs", n: subIdx }));
}

function loadP2PMediaDirect(media) {
  if (!media) return;

  if (!S.p2pMedia || S.p2pMedia.readyState !== "open") {
    showStatus("load", "⚡ Ожидание прямого P2P канала…");
    const check = setInterval(() => {
      if (S.p2pMedia && S.p2pMedia.readyState === "open") {
        clearInterval(check);
        loadP2PMediaDirect(media);
      }
    }, 400);
    return;
  }

  showStatus("load", "⚡ P2P подключён, запрашиваю фильм…");

  if (currentDirectReqId > 0) {
    try {
      S.p2pMedia.send(JSON.stringify({ id: currentDirectReqId, type: "cancel" }));
    } catch (_) {}
  }

  const reqId = ++directReqCounter;
  currentDirectReqId = reqId;
  const startTime = Date.now();
  const directChunks = [];
  let directReceived = 0;
  let directTotal = media.size || 0;

  const ext = (media.name || "").toLowerCase();
  let mime = "video/mp4";
  if (ext.endsWith(".webm")) mime = "video/webm";
  else if (ext.endsWith(".mkv")) mime = "video/x-matroska";

  S.p2pDirectCallback = {
    onMeta: (meta) => {
      if (meta.total) directTotal = meta.total;
      if (meta.mime) mime = meta.mime;
    },
    onChunk: (chunk) => {
      directChunks.push(chunk);
      directReceived += chunk.byteLength;

      const elapsed = (Date.now() - startTime) / 1000;
      const speedMBps = elapsed > 0.5 ? (directReceived / (1024 * 1024) / elapsed).toFixed(1) : "…";
      const recMB = (directReceived / (1024 * 1024)).toFixed(1);

      if (directTotal > 0) {
        const pct = Math.min(99, Math.round((directReceived / directTotal) * 100));
        const totMB = (directTotal / (1024 * 1024)).toFixed(1);
        showStatus("load", `⚡ P2P загрузка: ${pct}% (${recMB}/${totMB} МБ, ${speedMBps} МБ/с)`);
      } else {
        showStatus("load", `⚡ P2P загрузка: ${recMB} МБ (${speedMBps} МБ/с)`);
      }
    },
    onDone: () => {
      hideStatus("load");
      const blob = new Blob(directChunks, { type: mime });
      video.src = URL.createObjectURL(blob);
      video.load();
      toast("🎬 Фильм загружен и готов к просмотру!");
      sync(true);
      play();
    },
    onError: (err) => {
      showStatus("load", `Ошибка загрузки видео: ${err}`);
    },
  };

  try {
    S.p2pMedia.send(JSON.stringify({
      id: reqId,
      type: "blob",
      start: 0,
      end: -1,
    }));
  } catch (err) {
    showStatus("load", `Ошибка отправки запроса: ${err}`);
  }
}

function getSignalURL(room, action) {
  const signalParam = new URLSearchParams(location.search).get("signal") || localStorage.getItem("wf_signal_url");
  if (signalParam) {
    try {
      const u = new URL(signalParam);
      if (room) u.searchParams.set("room", room);
      if (action) u.searchParams.set("action", action);
      return u.toString();
    } catch (_) {}
  }
  if (location.hostname.includes("functions.yandexcloud.net")) {
    const u = new URL(location.origin + location.pathname);
    if (room) u.searchParams.set("room", room);
    if (action) u.searchParams.set("action", action);
    return u.toString();
  }
  const u = new URL("/api/signal", location.origin);
  if (room) u.searchParams.set("room", room);
  if (action) u.searchParams.set("action", action);
  return u.toString();
}

function postSignalURL() {
  const signalParam = new URLSearchParams(location.search).get("signal") || localStorage.getItem("wf_signal_url");
  if (signalParam) {
    return signalParam;
  }
  if (location.hostname.includes("functions.yandexcloud.net")) {
    return location.origin + location.pathname;
  }
  return "/api/signal";
}

function decodeBase64SDP(str) {
  if (!str) return "";
  let b64 = str.replace(/-/g, "+").replace(/_/g, "/").replace(/ /g, "+");
  while (b64.length % 4 !== 0) b64 += "=";
  return atob(b64);
}

function normalizeSDP(sdp) {
  if (!sdp) return "";
  let s = sdp;
  if (!s.includes("\n") && !s.includes("\r")) {
    s = s.replace(/ (?=[vosieupcbtzkam]=)/g, "\r\n");
  }
  const lines = s.split(/\r\n|\r|\n/);
  const cleaned = lines.map((l) => l.trim()).filter((l) => l.length > 0);
  return cleaned.join("\r\n") + "\r\n";
}

async function connectP2P(room) {
  S.p2pMode = true;
  showStatus("conn", "Подключение P2P (Yandex Cloud)…");
  try {
    let offerSDP = "";
    const hash = location.hash ? location.hash.slice(1) : "";
    const hashParams = new URLSearchParams(hash);
    if (hashParams.get("o") && !S.p2pOfferRetried) {
      try {
        offerSDP = decodeBase64SDP(hashParams.get("o"));
      } catch (e) {
        console.warn("[p2p] ошибка декодирования hash offer:", e);
      }
    }

    if (!offerSDP) {
      const res = await fetch(getSignalURL(room, "offer"));
      if (!res.ok) throw new Error("комната не найдена или закрыта");
      const offerData = await res.json();
      offerSDP = offerData.sdp || "";
    }
    if (!offerSDP) throw new Error("Offer не получен");

    offerSDP = normalizeSDP(offerSDP);

    const pc = new RTCPeerConnection({ iceServers: STUN_SERVERS });
    S.p2pPc = pc;

    pc.ondatachannel = (e) => {
      const ch = e.channel;
      if (ch.label === "control") {
        S.p2pControl = ch;
        ch.onopen = () => {
          hideStatus("conn");
          S.p2pMode = true;
          toast("⚡ Прямое P2P соединение установлено!");
          send({ type: "name", name: S.name });
          ping();
          setTimeout(ping, 100);
          setTimeout(ping, 250);
          setTimeout(ping, 500);
        };
        ch.onmessage = (ev) => {
          try { onMessage(JSON.parse(ev.data)); } catch (_) {}
        };
        ch.onclose = () => {
          showStatus("conn", "P2P связь потеряна, пробую снова…");
          S.p2pOfferRetried = true;
          setTimeout(() => connectP2P(room), 3000);
        };
      } else if (ch.label === "media") {
        S.p2pMedia = ch;
        ch.binaryType = "arraybuffer";
        ch.onmessage = onMediaChannelMessage;
        const triggerPlay = () => {
          if (S.media && (!video.src || video.src === location.href)) {
            loadP2PMediaDirect(S.media);
          }
        };
        if (ch.readyState === "open") triggerPlay();
        else ch.onopen = triggerPlay;
      }
    };

    try {
      await pc.setRemoteDescription({ type: "offer", sdp: offerSDP });
    } catch (sdpErr) {
      console.warn("[p2p] offer из ссылки устарел, запрашиваю свежий с сервера…", sdpErr);
      S.p2pOfferRetried = true;
      const res = await fetch(getSignalURL(room, "offer"));
      if (res.ok) {
        const offerData = await res.json();
        if (offerData.sdp) {
          offerSDP = normalizeSDP(offerData.sdp);
          await pc.setRemoteDescription({ type: "offer", sdp: offerSDP });
        } else {
          throw sdpErr;
        }
      } else {
        throw sdpErr;
      }
    }
    const answer = await pc.createAnswer();
    await pc.setLocalDescription(answer);

    await new Promise((resolve) => {
      if (pc.iceGatheringState === "complete") return resolve();
      const check = () => {
        if (pc.iceGatheringState === "complete") {
          pc.removeEventListener("icegatheringstatechange", check);
          resolve();
        }
      };
      pc.addEventListener("icegatheringstatechange", check);
      setTimeout(resolve, 2500);
    });

    const finalAnswerSDP = normalizeSDP(pc.localDescription.sdp);

    // 1. Отправляем Answer в YCF
    fetch(postSignalURL(), {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        action: "answer",
        room: room,
        sdp: finalAnswerSDP,
      }),
    }).catch((err) => console.warn("[p2p] ошибка POST answer на YCF:", err));

    // 2. Dual-write base64 directly to ntfy
    try {
      fetch("https://ntfy.sh/wf-" + encodeURIComponent(room) + "-answer", {
        method: "POST",
        body: btoa(finalAnswerSDP),
      }).catch(() => {});
    } catch (_) {}

    showStatus("conn", "Пробиваю NAT (STUN hole-punching)…");
  } catch (err) {
    showStatus("conn", `Ошибка P2P: ${err.message || err}. Пробую снова…`);
    setTimeout(() => connectP2P(room), 3000);
  }
}

function send(obj) {
  if (S.p2pControl && S.p2pControl.readyState === "open") {
    S.p2pControl.send(JSON.stringify(obj));
  } else if (S.ws && S.ws.readyState === 1) {
    S.ws.send(JSON.stringify(obj));
  }
}
function connect() {
  const p2pRoom = new URLSearchParams(location.search).get("room");
  if (p2pRoom) {
    connectP2P(p2pRoom);
    return;
  }
  const proto = location.protocol === "https:" ? "wss" : "ws";
  const ws = new WebSocket(`${proto}://${location.host}/ws?name=${encodeURIComponent(S.name)}`);
  S.ws = ws;
  ws.onopen = () => {
    S.reconnectDelay = 500;
    S.bestRtt = Infinity;
    hideStatus("conn");
    ping();
    setTimeout(ping, 100);
    setTimeout(ping, 250);
    setTimeout(ping, 500);
  };
  ws.onmessage = (e) => onMessage(JSON.parse(e.data));
  ws.onclose = () => {
    if (S.ws !== ws) return;
    showStatus("conn", "Связь потеряна, переподключаюсь…");
    video.pause();
    setTimeout(connect, S.reconnectDelay);
    S.reconnectDelay = Math.min(S.reconnectDelay * 2, 5000);
  };
}
function ping() { send({ type: "ping", t: Date.now() }); }
setInterval(ping, 2000);

function onPong(m) {
  const now = Date.now(), rtt = now - m.t;
  // Keep the sample with the lowest RTT: it gives the most precise offset.
  // Slowly forget old samples so clock drift / route changes are tracked.
  S.bestRtt *= 1.02;
  if (rtt <= S.bestRtt) {
    S.bestRtt = rtt;
    S.offset = m.server - (m.t + rtt / 2);
  }
}

function onMessage(m) {
  switch (m.type) {
    case "welcome":
      S.you = m.you; S.host = !!m.host;
      document.body.classList.toggle("is-host", S.host);
      S.peers = m.peers || [];
      if (m.server) S.offset = m.server - Date.now();
      setMedia(m.media || null);
      S.state = m.state; sync(true);
      renderPeers();
      $("messages").textContent = "";
      (m.history || []).forEach(addChat);
      if (m.tunnel) setTunnel(m.tunnel);
      break;
    case "pong": onPong(m); break;
    case "media": setMedia(m.media); break;
    case "state": onState(m); break;
    case "peers": S.peers = m.peers || []; renderPeers(); updateWaitingText(); break;
    case "chat": addChat(m.chat); break;
    case "reaction": floatEmoji(m.emoji, m.by === S.you ? "" : m.byName); break;
    case "tunnel": setTunnel(m.tunnel); break;
  }
}

function onState(m) {
  const prev = S.state;
  S.state = m.state;
  if (m.by && m.by !== S.you) {
    const who = m.byName || "Собеседник";
    if (m.action === "play") toast(`▶ ${who} продолжил(а)`);
    if (m.action === "pause") toast(`⏸ ${who} поставил(а) паузу`);
    if (m.action === "seek") toast(`⇄ ${who} перемотал(а) на ${fmt(m.state.position)}`);
    if (m.action === "rate") toast(`Скорость ×${m.state.rate}`);
  }
  if (m.action === "media") toast("Новый фильм загружен");
  sync(m.action === "seek" || m.action === "media" || (prev && prev.paused !== m.state.paused));
  if (m.state.paused) showControls();
  updateWaitingText();
}

/* ---------- media ---------- */
function setMedia(media) {
  const changed = (S.media && S.media.id) !== (media && media.id);
  S.media = media;
  $("empty").classList.toggle("hidden", !!media);
  $("title").textContent = media ? media.name : "";
  document.title = media ? `${media.name} · WatchFilms` : "WatchFilms";
  if (!changed) return;
  S.reportedBuf = false; S.bufSince = 0;
  while (video.firstChild) video.removeChild(video.firstChild);
  const sel = $("subs");
  sel.length = 1;
  if (!media) { video.removeAttribute("src"); video.load(); return; }

  (media.subtitles || []).forEach((s, i) => {
    const tr = document.createElement("track");
    tr.kind = "subtitles"; tr.label = s.label; tr.srclang = "ru";
    if (S.p2pMode && (!navigator.serviceWorker || !navigator.serviceWorker.controller)) {
      loadP2PSubsDirect(s.id, (vttText) => {
        const blob = new Blob([vttText], { type: "text/vtt" });
        tr.src = URL.createObjectURL(blob);
      });
    } else {
      tr.src = S.p2pMode ? `${basePath}/p2p/subs/${s.id}.vtt` : `/api/subs/${media.id}/${s.id}.vtt`;
    }
    video.appendChild(tr);
    sel.add(new Option(s.label, String(i)));
  });
  const saved = localStorage.getItem("wf_subs");
  sel.value = saved !== null && +saved < (media.subtitles || []).length ? saved : "-1";
  setTimeout(applySubs, 0);

  if (S.p2pMode) {
    if (navigator.serviceWorker && navigator.serviceWorker.controller) {
      video.src = `${basePath}/p2p/video`;
      showStatus("load", "Загружаю видео…");
    } else {
      loadP2PMediaDirect(media);
    }
  } else {
    video.src = `/api/video/${media.id}`;
    showStatus("load", "Загружаю видео…");
  }
}
function applySubs() {
  const v = +$("subs").value;
  Array.from(video.textTracks).forEach((t, i) => (t.mode = i === v ? "showing" : "disabled"));
}

/* ---------- synchronisation ---------- */
function play() {
  const p = video.play();
  if (p) p.then(() => { S.autoplayBlocked = false; $("tapJoin").classList.add("hidden"); })
    .catch((err) => {
      if (err && err.name === "NotAllowedError") {
        S.autoplayBlocked = true; $("tapJoin").classList.remove("hidden");
      }
    });
}

// Bring the local <video> in line with the shared state.
// force=true right after a discrete event (play/pause/seek) -> tight tolerance.
function sync(force = false) {
  const st = S.state;
  if (!st || !S.media || !S.joined || video.readyState === 0) return;
  const dur = isFinite(video.duration) ? video.duration : Infinity;
  const target = Math.min(Math.max(expectedPos(), 0), dur - 0.05);
  const drift = video.currentTime - target;
  const now = Date.now();

  if (st.paused || st.waiting || target >= dur - 0.1) {
    if (!video.paused) video.pause();
    video.playbackRate = st.rate;
    if (Math.abs(drift) > 0.06 && !video.seeking) video.currentTime = target;
    $("tapJoin").classList.add("hidden");
    return;
  }
  const limit = force ? 0.12 : 0.35;
  if (Math.abs(drift) > limit && (force || now - S.hardSeekAt > 600)) {
    video.currentTime = target;
    S.hardSeekAt = now;
    video.playbackRate = st.rate;
  } else if (Math.abs(drift) > 0.03) {
    // Small drift: nudge the speed by up to ±12% - quick and imperceptible.
    const k = Math.max(-0.12, Math.min(0.12, drift * 0.8));
    video.playbackRate = st.rate * (1 - k);
  } else {
    video.playbackRate = st.rate;
  }
  if (video.paused && !S.autoplayBlocked) play();
}
setInterval(() => sync(false), 120);

function bufferAhead() {
  const t = video.currentTime, b = video.buffered;
  for (let i = 0; i < b.length; i++) if (b.start(i) <= t + 0.5 && b.end(i) >= t) return b.end(i) - t;
  return 0;
}

// Tell the server when we stall, so playback freezes for both sides instead of
// drifting apart. Resume only once a healthy buffer is accumulated.
function checkBuffering() {
  const st = S.state;
  if (!st || !S.media || !S.joined || S.autoplayBlocked) return;
  let stalled = false;
  if (!st.paused && video.readyState > 0) {
    const nearEnd = isFinite(video.duration) && video.duration - video.currentTime < 3;
    stalled = S.reportedBuf
      ? video.seeking || !(video.readyState >= 3 || bufferAhead() >= 1.0 || nearEnd)
      : video.seeking || video.readyState < 2;
  }
  if (stalled) {
    if (!S.bufSince) S.bufSince = Date.now();
    if (!S.reportedBuf && Date.now() - S.bufSince > 800) {
      S.reportedBuf = true; send({ type: "buffering", value: true });
    }
  } else {
    S.bufSince = 0;
    if (S.reportedBuf) { S.reportedBuf = false; send({ type: "buffering", value: false }); }
  }
}
setInterval(checkBuffering, 250);

/* ---------- user actions (only these are sent; remote changes never echo) ---------- */
function localApply(patch) {
  S.state = { ...S.state, ...patch, updatedAt: serverNow() };
  sync(true);
}
function userTogglePlay() {
  if (!S.media || !S.state) return;
  if (S.autoplayBlocked) { S.autoplayBlocked = false; play(); }
  const pos = video.currentTime;
  if (S.state.paused) { localApply({ paused: false, position: pos }); send({ type: "play", position: pos }); }
  else { localApply({ paused: true, waiting: false, position: pos }); send({ type: "pause", position: pos }); }
  flashIcon();
}
function userSeek(pos) {
  if (!S.media || !S.state) return;
  const dur = isFinite(video.duration) ? video.duration : 0;
  pos = Math.max(0, dur ? Math.min(pos, dur - 0.5) : pos);
  localApply({ position: pos });
  send({ type: "seek", position: pos });
}

/* ---------- icons ---------- */
const ICON = {
  play: '<svg viewBox="0 0 24 24"><path d="M8 5.5v13a1 1 0 0 0 1.5.86l10.5-6.5a1 1 0 0 0 0-1.72L9.5 4.64A1 1 0 0 0 8 5.5z"/></svg>',
  pause: '<svg viewBox="0 0 24 24"><rect x="6" y="5" width="4" height="14" rx="1.2"/><rect x="14" y="5" width="4" height="14" rx="1.2"/></svg>',
  back: '<svg viewBox="0 0 24 24"><path d="M12 5V2L7 6l5 4V7a6 6 0 1 1-6 6H4a8 8 0 1 0 8-8z"/><text x="12" y="16.5" font-size="7" text-anchor="middle" font-family="sans-serif" font-weight="700">10</text></svg>',
  fwd: '<svg viewBox="0 0 24 24"><path d="M12 5V2l5 4-5 4V7a6 6 0 1 0 6 6h2a8 8 0 1 1-8-8z"/><text x="12" y="16.5" font-size="7" text-anchor="middle" font-family="sans-serif" font-weight="700">10</text></svg>',
  vol: '<svg viewBox="0 0 24 24"><path d="M4 9v6h4l5 4V5L8 9H4zm12.5 3a4.5 4.5 0 0 0-2.5-4v8a4.5 4.5 0 0 0 2.5-4zM14 3.2v2.1a7 7 0 0 1 0 13.4v2.1a9 9 0 0 0 0-17.6z"/></svg>',
  mute: '<svg viewBox="0 0 24 24"><path d="M4 9v6h4l5 4V5L8 9H4zm16.6 3 2.1-2.1-1.4-1.4-2.1 2.1-2.1-2.1-1.4 1.4 2.1 2.1-2.1 2.1 1.4 1.4 2.1-2.1 2.1 2.1 1.4-1.4z"/></svg>',
  chat: '<svg viewBox="0 0 24 24"><path d="M4 4h16a2 2 0 0 1 2 2v10a2 2 0 0 1-2 2H9l-5 4v-4a2 2 0 0 1-2-2V6a2 2 0 0 1 2-2z"/></svg>',
  full: '<svg viewBox="0 0 24 24"><path d="M4 9V4h5v2H6v3H4zm10-5h6v5h-2V6h-4V4zM4 15h2v3h3v2H4v-5zm14 3v-3h2v5h-5v-2h3z"/></svg>',
  exitfull: '<svg viewBox="0 0 24 24"><path d="M7 4h2v5H4V7h3V4zm8 0h2v3h3v2h-5V4zM4 15h5v5H7v-3H4v-2zm11 0h5v2h-3v3h-2v-5z"/></svg>',
};
$("btnBack").innerHTML = ICON.back;
$("btnFwd").innerHTML = ICON.fwd;
$("btnChat").innerHTML = ICON.chat;

/* ---------- controls rendering ---------- */
function render() {
  const dur = isFinite(video.duration) ? video.duration : 0;
  const cur = video.currentTime;
  const playing = S.state && !S.state.paused;
  const playIcon = playing ? "pause" : "play";
  if ($("btnPlay").dataset.i !== playIcon) { $("btnPlay").innerHTML = ICON[playIcon]; $("btnPlay").dataset.i = playIcon; }
  const muteIcon = video.muted || video.volume === 0 ? "mute" : "vol";
  if ($("btnMute").dataset.i !== muteIcon) { $("btnMute").innerHTML = ICON[muteIcon]; $("btnMute").dataset.i = muteIcon; }
  const fullIcon = document.fullscreenElement ? "exitfull" : "full";
  if ($("btnFull").dataset.i !== fullIcon) { $("btnFull").innerHTML = ICON[fullIcon]; $("btnFull").dataset.i = fullIcon; }

  const shown = seekDrag != null ? seekDrag : cur;
  const pct = dur ? (shown / dur) * 100 : 0;
  $("seekProgress").style.width = pct + "%";
  $("seekThumb").style.left = pct + "%";
  const ahead = bufferAhead();
  $("seekBuffer").style.width = dur ? Math.min(100, ((cur + ahead) / dur) * 100) + "%" : "0";
  $("tCur").textContent = fmt(shown);
  $("tDur").textContent = fmt(dur);
  if (S.media && dur) {
    const mbps = (S.media.size * 8) / dur / 1e6;
    $("netInfo").textContent = `буфер ${Math.round(ahead)} с`;
    $("netInfo").title = `Загружено вперёд: ${Math.round(ahead)} с\nБитрейт фильма ≈ ${mbps.toFixed(1)} Мбит/с — столько нужно исходящей скорости у хоста`;
  } else $("netInfo").textContent = "";
}
setInterval(render, 250);
video.addEventListener("timeupdate", render);

/* ---------- seek bar ---------- */
let seekDrag = null;
function seekFrac(e) {
  const r = $("seek").getBoundingClientRect();
  return Math.max(0, Math.min(1, (e.clientX - r.left) / r.width));
}
$("seek").addEventListener("pointermove", (e) => {
  const dur = video.duration || 0, f = seekFrac(e);
  $("seekTip").textContent = fmt(f * dur);
  $("seekTip").style.left = f * 100 + "%";
  if (seekDrag != null) { seekDrag = f * dur; render(); }
});
$("seek").addEventListener("pointerdown", (e) => {
  if (!video.duration) return;
  $("seek").setPointerCapture(e.pointerId);
  $("seek").classList.add("drag");
  seekDrag = seekFrac(e) * video.duration; render();
});
$("seek").addEventListener("pointerup", (e) => {
  if (seekDrag == null) return;
  $("seek").classList.remove("drag");
  const t = seekFrac(e) * video.duration;
  seekDrag = null;
  userSeek(t);
});

/* ---------- status / toasts ---------- */
const statuses = {};
const STATUS_ORDER = ["conn", "wait", "load"];
function showStatus(key, text) { statuses[key] = text; renderStatus(); }
function hideStatus(key) { delete statuses[key]; renderStatus(); }
function renderStatus() {
  const k = STATUS_ORDER.find((x) => statuses[x]);
  $("status").classList.toggle("hidden", !k);
  if (k) $("statusText").textContent = statuses[k];
}
function updateWaitingText() {
  if (!S.state || !S.state.waiting || S.state.paused) { hideStatus("wait"); return; }
  const others = S.peers.filter((p) => p.buffering && p.id !== S.you).map((p) => p.name);
  const me = S.peers.some((p) => p.buffering && p.id === S.you);
  showStatus("wait", others.length ? `Ждём, пока загрузится у ${others.join(", ")}…` : me ? "Подгружаю видео…" : "Синхронизация…");
}
function toast(text) {
  const t = el("div", "toast", text);
  $("toasts").appendChild(t);
  while ($("toasts").children.length > 3) $("toasts").firstChild.remove();
  setTimeout(() => { t.classList.add("out"); setTimeout(() => t.remove(), 300); }, 2600);
}
function flashIcon() { showControls(); }

/* ---------- auto-hide controls ---------- */
let idleTimer = 0;
function showControls() {
  $("stage").classList.remove("idle");
  clearTimeout(idleTimer);
  idleTimer = setTimeout(() => {
    const overControls = $("controls").matches(":hover") || $("seek").classList.contains("drag");
    if (S.state && !S.state.paused && !overControls) $("stage").classList.add("idle");
    else showControls();
  }, 2800);
}
$("stage").addEventListener("pointermove", showControls);
$("stage").addEventListener("pointerdown", showControls);

/* ---------- peers ---------- */
function renderPeers() {
  const box = $("peers"); box.textContent = "";
  S.peers.forEach((p) => {
    const d = el("div", "peer" + (p.buffering ? " buf" : ""), (p.name || "?").slice(0, 1).toUpperCase());
    d.style.background = colorFor(p.id);
    d.title = p.name + (p.host ? " (хост)" : "") + (p.id === S.you ? " (вы)" : "") + (p.buffering ? ": загружает…" : "");
    box.appendChild(d);
  });
  $("btnRename").textContent = S.name ? `${S.name} ✎` : "";
}

/* ---------- chat ---------- */
function chatOpen() { return !$("app").classList.contains("chat-closed"); }
function addChat(m) {
  if (!m) return;
  const box = $("messages");
  const nearBottom = box.scrollHeight - box.scrollTop - box.clientHeight < 80;
  let node;
  if (m.system) node = el("div", "msg sys", m.text);
  else {
    const mine = m.from === S.you;
    node = el("div", "msg" + (mine ? " me" : ""));
    if (!mine) { const w = el("span", "who", m.name); w.style.color = colorFor(m.from); node.appendChild(w); }
    node.appendChild(document.createTextNode(m.text));
    const d = new Date(m.ts);
    node.appendChild(el("span", "ts", `${d.getHours()}:${String(d.getMinutes()).padStart(2, "0")}`));
    if (!mine && S.joined && !chatOpen()) toast(`💬 ${m.name}: ${m.text.length > 60 ? m.text.slice(0, 60) + "…" : m.text}`);
  }
  box.appendChild(node);
  while (box.children.length > 300) box.firstChild.remove();
  if (nearBottom || m.from === S.you) box.scrollTop = box.scrollHeight;
}
$("composer").addEventListener("submit", (e) => {
  e.preventDefault();
  const v = $("msg").value.trim();
  if (!v) return;
  send({ type: "chat", text: v });
  $("msg").value = "";
});
function toggleChat() {
  $("app").classList.toggle("chat-closed");
  localStorage.setItem("wf_chat", chatOpen() ? "1" : "0");
}
if (localStorage.getItem("wf_chat") === "0") $("app").classList.add("chat-closed");

/* ---------- reactions ---------- */
const EMOJI = ["❤️", "😂", "😱", "🔥", "👏", "🍿", "😭", "🤯"];
EMOJI.forEach((e, i) => {
  const b = el("button", null, e);
  b.type = "button"; b.title = `Реакция (${i + 1})`;
  b.onclick = () => send({ type: "reaction", emoji: e });
  $("reactBar").appendChild(b);
});
function floatEmoji(emoji, name) {
  const n = el("div", "float-emoji", emoji);
  if (name) n.appendChild(el("small", null, name));
  n.style.left = 8 + Math.random() * 80 + "%";
  n.style.setProperty("--dx", (Math.random() * 120 - 60).toFixed(0) + "px");
  $("reactionsLayer").appendChild(n);
  setTimeout(() => n.remove(), 2700);
}

/* ---------- controls wiring ---------- */
$("btnPlay").onclick = userTogglePlay;
$("btnBack").onclick = () => userSeek(video.currentTime - SEEK_STEP);
$("btnFwd").onclick = () => userSeek(video.currentTime + SEEK_STEP);
$("tapJoin").onclick = () => { S.autoplayBlocked = false; $("tapJoin").classList.add("hidden"); sync(true); play(); };
video.addEventListener("click", userTogglePlay);
video.addEventListener("dblclick", toggleFull);
const vol = +(localStorage.getItem("wf_vol") ?? 1);
video.volume = vol; $("vol").value = vol;
$("vol").oninput = () => { video.volume = +$("vol").value; video.muted = false; localStorage.setItem("wf_vol", $("vol").value); };
$("btnMute").onclick = () => { video.muted = !video.muted; };
$("btnChat").onclick = toggleChat;
$("btnFull").onclick = toggleFull;
$("subs").onchange = () => { localStorage.setItem("wf_subs", $("subs").value); applySubs(); };
function toggleFull() {
  if (document.fullscreenElement) document.exitFullscreen();
  else $("app").requestFullscreen().catch(() => {});
}

video.addEventListener("loadedmetadata", () => { hideStatus("load"); sync(true); });
video.addEventListener("seeked", render);
video.addEventListener("error", () => {
  if (!S.media) return;
  hideStatus("load");
  const code = video.error && video.error.code;
  if (S.p2pMode && !S.p2pBlobTried) {
    S.p2pBlobTried = true;
    console.warn("[p2p] Ошибка воспроизведения, переключаюсь на прямую P2P загрузку…");
    loadP2PMediaDirect(S.media);
    return;
  }
  if (code === 4) {
    toast("⚠ Браузер не поддерживает кодек этого видео (рекомендуется H.264/MP4)");
  } else {
    toast("⚠ Ошибка загрузки видео");
  }
});

/* ---------- keyboard ---------- */
document.addEventListener("keydown", (e) => {
  const typing = e.target.closest && e.target.closest("input, select, textarea");
  if (typing) { if (e.key === "Escape") e.target.blur(); return; }
  if (document.querySelector("dialog[open]") || e.ctrlKey || e.metaKey || e.altKey) return;
  const k = e.key.toLowerCase();
  if (k === " " || k === "k" || k === "л") { e.preventDefault(); userTogglePlay(); }
  else if (k === "arrowleft") userSeek(video.currentTime - SEEK_STEP);
  else if (k === "arrowright") userSeek(video.currentTime + SEEK_STEP);
  else if (k === "arrowup") { e.preventDefault(); video.volume = Math.min(1, video.volume + 0.05); $("vol").value = video.volume; }
  else if (k === "arrowdown") { e.preventDefault(); video.volume = Math.max(0, video.volume - 0.05); $("vol").value = video.volume; }
  else if (k === "f" || k === "а") toggleFull();
  else if (k === "m" || k === "ь") video.muted = !video.muted;
  else if (k === "c" || k === "с") toggleChat();
  else if (k === "enter") { e.preventDefault(); if (!chatOpen()) toggleChat(); $("msg").focus(); }
  else if (/^[1-8]$/.test(k)) send({ type: "reaction", emoji: EMOJI[+k - 1] });
  else return;
  showControls();
});

/* ---------- dialogs ---------- */
document.querySelectorAll("[data-close]").forEach((b) => (b.onclick = () => b.closest("dialog").close()));
document.querySelectorAll("dialog").forEach((d) => d.addEventListener("click", (e) => {
  if (e.target === d && d.id !== "dlgName") d.close(); // click on backdrop
}));

// Name: the submit click is also the user gesture that unlocks autoplay with sound.
$("dlgName").addEventListener("cancel", (e) => { if (!S.joined) e.preventDefault(); });
$("nameForm").addEventListener("submit", () => {
  const n = $("nameInput").value.trim().slice(0, 24) || "Гость";
  S.name = n; localStorage.setItem("wf_name", n);
  if (!S.joined) {
    S.joined = true;
    connect();
  } else send({ type: "name", name: n });
  renderPeers();
});
$("btnRename").onclick = () => { $("nameInput").value = S.name; $("dlgName").showModal(); };

// Invite link
let tunnel = { status: "off" };
function setTunnel(t) {
  tunnel = t || tunnel;
  const prov = tunnel.provider ? ` (${tunnel.provider})` : "";
  const txt = {
    ready: `Отправьте эту ссылку другу${prov}. Она работает, пока открыто приложение.`,
    starting: tunnel.error || "Открываю публичную ссылку…",
    error: tunnel.error || "Не удалось открыть публичную ссылку",
    off: "Публичная ссылка выключена. Используйте ссылки для локальной сети / VPN ниже.",
  }[tunnel.status] || "";
  $("tunnelText").textContent = txt;
  $("inviteUrl").value = tunnel.status === "ready" ? tunnel.url : "";
  $("btnCopy").disabled = tunnel.status !== "ready";

  const provEl = $("tunnelProvider");
  if (provEl) {
    if (tunnel.status === "ready" && tunnel.provider) {
      provEl.textContent = "Сервис: " + tunnel.provider;
      provEl.className = "tunnel-badge" + (tunnel.provider.includes("P2P") ? " p2p" : "");
    } else if (tunnel.status === "starting") {
      provEl.textContent = tunnel.provider ? "Сервис: " + tunnel.provider : "Подключение…";
      provEl.className = "tunnel-badge dim" + (tunnel.provider && tunnel.provider.includes("P2P") ? " p2p" : "");
    } else {
      provEl.textContent = "";
    }
  }

  const btnSwitch = $("btnSwitchTunnel");
  if (btnSwitch) {
    btnSwitch.style.display = tunnel.status === "off" || (tunnel.provider && tunnel.provider.includes("P2P")) ? "none" : "";
  }

  const lan = $("lanList"); lan.textContent = "";
  (tunnel.lan || []).forEach((u) => lan.appendChild(el("div", null, u)));
  $("btnInvite").textContent = tunnel.status === "ready" ? "Пригласить" : tunnel.status === "error" ? "Ссылка ⚠" : "Пригласить…";
}
function openInvite() { setTunnel(); $("dlgInvite").showModal(); }
$("btnInvite").onclick = openInvite;
$("btnInvite2").onclick = openInvite;
$("btnCopy").onclick = async () => {
  const v = $("inviteUrl").value;
  try { await navigator.clipboard.writeText(v); }
  catch { $("inviteUrl").select(); document.execCommand("copy"); }
  $("btnCopy").textContent = "Скопировано ✓";
  setTimeout(() => ($("btnCopy").textContent = "Копировать"), 1600);
};

const btnSwitchTunnel = $("btnSwitchTunnel");
if (btnSwitchTunnel) {
  btnSwitchTunnel.onclick = async () => {
    btnSwitchTunnel.disabled = true;
    $("tunnelText").textContent = "Переключаю сервис туннеля…";
    try {
      await fetch("/api/tunnel/switch", { method: "POST" });
    } catch (e) {
      console.error(e);
    }
    setTimeout(() => { btnSwitchTunnel.disabled = false; }, 2000);
  };
}

const btnSaveP2P = $("btnSaveP2P");
const p2pInput = $("p2pInput");
if (p2pInput) {
  p2pInput.value = localStorage.getItem("wf_p2p_url") || "";
}
if (btnSaveP2P) {
  btnSaveP2P.onclick = async () => {
    const u = (p2pInput.value || "").trim();
    if (!u) return;
    localStorage.setItem("wf_p2p_url", u);
    btnSaveP2P.disabled = true;
    btnSaveP2P.textContent = "Подключение…";
    try {
      const r = await fetch("/api/p2p/config", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ url: u }),
      });
      if (r.ok) {
        toast("⚡ P2P активирован!");
        btnSaveP2P.textContent = "Сохранено ✓";
      } else {
        const e = await r.json().catch(() => ({}));
        toast("⚠ Ошибка: " + (e.error || "не удалось сохранить"));
        btnSaveP2P.textContent = "Включить P2P";
      }
    } catch (err) {
      toast("⚠ Ошибка сети: " + err);
      btnSaveP2P.textContent = "Включить P2P";
    }
    setTimeout(() => { btnSaveP2P.disabled = false; }, 1500);
  };
}

const btnSaveWeb = $("btnSaveWeb");
const webInput = $("webInput");
if (webInput) {
  webInput.value = localStorage.getItem("wf_web_url") || "";
}
if (btnSaveWeb) {
  btnSaveWeb.onclick = async () => {
    const u = (webInput.value || "").trim();
    localStorage.setItem("wf_web_url", u);
    btnSaveWeb.disabled = true;
    btnSaveWeb.textContent = "Сохранение…";
    try {
      const r = await fetch("/api/p2p/config", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ web: u }),
      });
      if (r.ok) {
        toast("🚀 Ссылка GitHub Pages сохранена!");
        btnSaveWeb.textContent = "Сохранено ✓";
      } else {
        toast("⚠ Ошибка обновления");
        btnSaveWeb.textContent = "Сохранить";
      }
    } catch (err) {
      toast("⚠ Ошибка сети: " + err);
      btnSaveWeb.textContent = "Сохранить";
    }
    setTimeout(() => { btnSaveWeb.disabled = false; }, 1500);
  };
}

// File browser (host only)
async function browse(path) {
  const r = await fetch("/api/fs?path=" + encodeURIComponent(path || ""));
  if (!r.ok) { toast("Нет доступа к файлам"); return; }
  const d = await r.json();
  localStorage.setItem("wf_dir", d.path);
  const roots = $("fsRoots"); roots.textContent = "";
  d.roots.forEach((x) => {
    const b = el("button", "btn ghost small", x.name);
    b.onclick = () => browse(x.path);
    roots.appendChild(b);
  });
  $("fsPath").textContent = d.path;
  $("fsUp").disabled = !d.parent;
  $("fsUp").onclick = () => browse(d.parent);
  const list = $("fsList"); list.textContent = "";
  if (d.error) list.appendChild(el("div", "fs-empty", d.error));
  d.dirs.forEach((x) => {
    const b = el("button", "fs-item");
    b.append(el("span", null, "📁"), el("span", "n", x.name));
    b.onclick = () => browse(x.path);
    list.appendChild(b);
  });
  d.files.forEach((x) => {
    const b = el("button", "fs-item file");
    b.append(el("span", null, "🎬"), el("span", "n", x.name), el("span", "dim", fmtSize(x.size)));
    b.onclick = () => openFile(x.path);
    list.appendChild(b);
  });
  if (!d.error && !d.dirs.length && !d.files.length) list.appendChild(el("div", "fs-empty", "Здесь нет папок и видеофайлов"));
  list.scrollTop = 0;
}
async function openFile(path) {
  const r = await fetch("/api/open", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ path }) });
  if (!r.ok) { const e = await r.json().catch(() => ({})); toast("⚠ " + (e.error || "Не удалось открыть файл")); return; }
  $("dlgFiles").close();
}
function openPicker() { $("dlgFiles").showModal(); browse(localStorage.getItem("wf_dir") || ""); }
$("btnPick").onclick = openPicker;
$("btnPick2").onclick = openPicker;

/* ---------- start ---------- */
$("btnPlay").innerHTML = ICON.play;
$("nameInput").value = S.name;
$("dlgName").showModal();
render();

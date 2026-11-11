/* WatchFilms P2P Service Worker: intercepts video range requests and streams from WebRTC */
"use strict";

self.addEventListener("install", (event) => {
  self.skipWaiting();
});

self.addEventListener("activate", (event) => {
  event.waitUntil(self.clients.claim());
});

let reqCounter = 0;
const pending = new Map();

self.addEventListener("message", (event) => {
  const d = event.data;
  if (!d || !d.reqId) return;
  const p = pending.get(d.reqId);
  if (!p) return;

  if (d.type === "meta") {
    if (p.onMeta) p.onMeta(d);
  } else if (d.type === "chunk") {
    if (p.onChunk) p.onChunk(d.chunk);
  } else if (d.type === "done") {
    if (p.onDone) p.onDone();
    pending.delete(d.reqId);
  } else if (d.type === "error") {
    if (p.onError) p.onError(d.error);
    pending.delete(d.reqId);
  } else if (d.type === "subs") {
    if (p.onSubs) p.onSubs(d);
    pending.delete(d.reqId);
  }
});

self.addEventListener("fetch", (event) => {
  const url = new URL(event.request.url);
  if (url.pathname.endsWith("/p2p/video")) {
    event.respondWith(handleP2PVideo(event.request, event.clientId));
  } else if (url.pathname.includes("/p2p/subs/")) {
    event.respondWith(handleP2PSubs(event.request, event.clientId));
  }
});

async function getClient(clientId) {
  if (clientId) {
    const c = await self.clients.get(clientId);
    if (c) return c;
  }
  const all = await self.clients.matchAll({ type: "window" });
  return all[0] || null;
}

async function handleP2PVideo(request, clientId) {
  const client = await getClient(clientId);
  if (!client) {
    return new Response("WatchFilms client window not found", { status: 503 });
  }

  const rangeHeader = request.headers.get("range");
  let start = 0, end = -1;
  if (rangeHeader) {
    const m = /bytes=(\d+)-(\d*)/.exec(rangeHeader);
    if (m) {
      start = parseInt(m[1], 10);
      if (m[2]) end = parseInt(m[2], 10);
    }
  }

  const reqId = ++reqCounter;

  return new Promise((resolve) => {
    let controllerRef = null;

    pending.set(reqId, {
      onMeta: (meta) => {
        const stream = new ReadableStream({
          start(controller) {
            controllerRef = controller;
          },
          cancel() {
            client.postMessage({ type: "p2p_cancel", reqId });
            pending.delete(reqId);
          },
        });

        const status = meta.status || 206;
        const headers = new Headers();
        headers.set("Content-Type", meta.mime || "video/mp4");
        headers.set("Accept-Ranges", "bytes");
        headers.set("Content-Range", `bytes ${meta.start}-${meta.end}/${meta.total}`);
        headers.set("Content-Length", String(meta.length));
        headers.set("Cache-Control", "no-cache");

        resolve(new Response(stream, {
          status: status,
          statusText: "Partial Content",
          headers: headers,
        }));
      },
      onChunk: (chunk) => {
        if (controllerRef) {
          controllerRef.enqueue(new Uint8Array(chunk));
        }
      },
      onDone: () => {
        if (controllerRef) {
          try { controllerRef.close(); } catch (_) {}
        }
      },
      onError: (err) => {
        if (controllerRef) {
          try { controllerRef.error(new Error(err)); } catch (_) {}
        }
        resolve(new Response(err || "Media error", { status: 500 }));
      },
    });

    client.postMessage({
      type: "p2p_request_range",
      reqId: reqId,
      start: start,
      end: end,
    });
  });
}

async function handleP2PSubs(request, clientId) {
  const client = await getClient(clientId);
  if (!client) return new Response("Client not found", { status: 503 });

  const url = new URL(request.url);
  const parts = url.pathname.split("/");
  const nStr = parts[parts.length - 1].replace(".vtt", "");
  const n = parseInt(nStr, 10) || 0;
  const reqId = ++reqCounter;

  return new Promise((resolve) => {
    pending.set(reqId, {
      onSubs: (res) => {
        resolve(new Response(res.content || "", {
          status: res.status || 200,
          headers: { "Content-Type": "text/vtt; charset=utf-8" },
        }));
      },
      onError: (err) => {
        resolve(new Response(err || "Subtitle error", { status: 404 }));
      },
    });

    client.postMessage({
      type: "p2p_request_subs",
      reqId: reqId,
      n: n,
    });
  });
}

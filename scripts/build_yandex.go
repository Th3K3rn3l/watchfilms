package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

func main() {
	htmlBytes, err := os.ReadFile("web/index.html")
	if err != nil {
		panic(err)
	}
	cssBytes, err := os.ReadFile("web/style.css")
	if err != nil {
		panic(err)
	}
	jsBytes, err := os.ReadFile("web/app.js")
	if err != nil {
		panic(err)
	}
	swBytes, err := os.ReadFile("web/sw.js")
	if err != nil {
		panic(err)
	}

	// Inline CSS and JS into index.html for zero extra round-trips
	htmlStr := string(htmlBytes)
	htmlStr = strings.Replace(htmlStr, `<link rel="stylesheet" href="/style.css">`, fmt.Sprintf("<style>\n%s\n</style>", string(cssBytes)), 1)
	htmlStr = strings.Replace(htmlStr, `<link rel="stylesheet" href="style.css">`, fmt.Sprintf("<style>\n%s\n</style>", string(cssBytes)), 1)
	htmlStr = strings.Replace(htmlStr, `<script src="/app.js"></script>`, fmt.Sprintf("<script>\n%s\n</script>", string(jsBytes)), 1)
	htmlStr = strings.Replace(htmlStr, `<script src="app.js"></script>`, fmt.Sprintf("<script>\n%s\n</script>", string(jsBytes)), 1)

	htmlJSON, _ := json.Marshal(htmlStr)
	cssJSON, _ := json.Marshal(string(cssBytes))
	jsJSON, _ := json.Marshal(string(jsBytes))
	swJSON, _ := json.Marshal(string(swBytes))

	template := `/**
 * WatchFilms P2P Signaling & Web UI Backend for Yandex Cloud Functions (Node.js 18/20/22).
 *
 * Полностью автономная серверная функция:
 * 1. Обслуживает сигнальный сервер WebRTC для прямого P2P соединения.
 * 2. Раздаёт автономный веб-интерфейс WatchFilms и Service Worker без необходимости внешнего хостинга.
 *
 * Не требует внешних зависимостей (чистый Node.js).
 */
'use strict';

const rooms = new Map();

// Периодическая очистка устаревших комнат (старше 2 часов)
const cleanTimer = setInterval(() => {
  const now = Date.now();
  for (const [id, r] of rooms.entries()) {
    if (now - (r.updatedAt || 0) > 2 * 3600 * 1000) {
      rooms.delete(id);
    }
  }
}, 60000);
if (cleanTimer && cleanTimer.unref) cleanTimer.unref();

const CORS = {
  'Access-Control-Allow-Origin': '*',
  'Access-Control-Allow-Methods': 'GET, POST, OPTIONS',
  'Access-Control-Allow-Headers': 'Content-Type, Accept',
  'Access-Control-Max-Age': '86400',
};

const STATIC_HTML = %s;
const STATIC_CSS = %s;
const STATIC_JS = %s;
const STATIC_SW = %s;

function reply(statusCode, headers, body, isBase64 = false) {
  return {
    statusCode,
    headers: { ...CORS, ...headers },
    body: typeof body === 'object' ? JSON.stringify(body) : body,
    isBase64Encoded: isBase64,
  };
}

function parseBody(event) {
  if (!event.body) return {};
  try {
    const raw = event.isBase64Encoded
      ? Buffer.from(event.body, 'base64').toString('utf8')
      : event.body;
    return typeof raw === 'string' ? JSON.parse(raw) : raw;
  } catch (_) {
    return {};
  }
}

async function persistStore(room, action, sdp) {
  try {
    const b64 = Buffer.from(sdp, 'utf8').toString('base64');
    await fetch('https://ntfy.sh/wf-' + encodeURIComponent(room) + '-' + action, {
      method: 'POST',
      body: b64,
      headers: { 'Title': action },
      signal: AbortSignal.timeout ? AbortSignal.timeout(2000) : undefined,
    });
  } catch (_) {}
}

async function persistGet(room, action) {
  try {
    const res = await fetch('https://ntfy.sh/wf-' + encodeURIComponent(room) + '-' + action + '/raw?poll=1&since=10m', {
      signal: AbortSignal.timeout ? AbortSignal.timeout(2000) : undefined,
    });
    if (res.ok) {
      const text = await res.text();
      if (text && text.trim()) {
        const lines = text.trim().split('\n');
        const latest = lines[lines.length - 1].trim();
        try {
          const decoded = Buffer.from(latest, 'base64').toString('utf8');
          if (decoded.includes('v=')) return decoded;
        } catch (_) {}
        if (latest.includes('v=')) return latest;
      }
    }
  } catch (_) {}
  return null;
}

exports.handler = async function (event, context) {
  const method = (event.httpMethod || 'GET').toUpperCase();
  const urlPath = event.url || event.path || '/';
  const params = event.queryStringParameters || {};

  if (method === 'OPTIONS') {
    return reply(204, {}, '');
  }

  // --- API Сигнального сервера P2P ---
  // Обрабатываем POST запросы или GET с параметром action
  const isSignal = urlPath.includes('/api/signal') || method === 'POST' || (params && params.action);
  if (isSignal) {
    if (method === 'POST') {
      const data = parseBody(event);
      const action = data.action;
      const room = data.room;
      const sdp = data.sdp;

      if (!room || !action || !sdp) {
        return reply(400, { 'Content-Type': 'application/json' }, { error: 'room, action, and sdp required' });
      }

      let r = rooms.get(room);
      if (!r) {
        r = { createdAt: Date.now() };
        rooms.set(room, r);
      }
      r[action] = sdp;
      r.updatedAt = Date.now();

      await persistStore(room, action, sdp);

      return reply(200, { 'Content-Type': 'application/json' }, { ok: true, action, room });
    }

    if (method === 'GET') {
      const room = params.room;
      const action = params.action; // "offer" или "answer"
      if (!room || !action) {
        return reply(400, { 'Content-Type': 'application/json' }, { error: 'room and action required' });
      }

      let r = rooms.get(room);
      let sdp = r && r[action];
      if (!sdp) {
        sdp = await persistGet(room, action);
        if (sdp) {
          if (!r) {
            r = { createdAt: Date.now() };
            rooms.set(room, r);
          }
          r[action] = sdp;
        }
      }

      if (!sdp) {
        return reply(404, { 'Content-Type': 'application/json' }, { error: 'not ready' });
      }

      return reply(200, { 'Content-Type': 'application/json' }, {
        action,
        room,
        sdp,
      });
    }
  }

  // --- Статические файлы ---
  if (urlPath.endsWith('/sw.js') || params.file === 'sw.js' || params.sw === '1') {
    return reply(200, {
      'Content-Type': 'application/javascript; charset=utf-8',
      'Service-Worker-Allowed': '/',
      'Cache-Control': 'public, max-age=3600',
    }, STATIC_SW);
  }

  if (urlPath.endsWith('/style.css') || params.file === 'style.css') {
    return reply(200, {
      'Content-Type': 'text/css; charset=utf-8',
      'Cache-Control': 'public, max-age=3600',
    }, STATIC_CSS);
  }

  if (urlPath.endsWith('/app.js') || params.file === 'app.js') {
    return reply(200, {
      'Content-Type': 'application/javascript; charset=utf-8',
      'Cache-Control': 'public, max-age=3600',
    }, STATIC_JS);
  }

  // Главная страница просмотра (index.html со встроенным CSS и JS)
  return reply(200, {
    'Content-Type': 'text/html; charset=utf-8',
    'Cache-Control': 'no-cache',
    'Content-Security-Policy': "default-src * 'unsafe-inline' 'unsafe-eval' data: blob:; worker-src * blob: data: 'self'; child-src * blob: data:;",
  }, STATIC_HTML);
};
`
	out := fmt.Sprintf(template, htmlJSON, cssJSON, jsJSON, swJSON)
	_ = os.MkdirAll("yandex", 0o755)
	if err := os.WriteFile("yandex/index.js", []byte(out), 0o644); err != nil {
		panic(err)
	}
	fmt.Println("Generated yandex/index.js successfully!")
}

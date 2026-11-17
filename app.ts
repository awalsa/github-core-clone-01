// app.ts — the forge's HTTP server: one process serving git smart HTTP,
// REST v3, GraphQL and MCP over the in-memory store. Only the Node standard
// library is used. No wall clock appears in any response.

import { createServer, type IncomingMessage, type Server, type ServerResponse } from 'node:http';
import { Store } from './store.ts';
import { gitInfoRefs, gitReceivePack, gitUploadPack, type GitResult } from './gitproto.ts';
import { restRoute, type RestCtx } from './rest.ts';
import { graphqlRoute } from './graphql.ts';
import { mcpRoute } from './mcp.ts';

const JSON_TYPE = 'application/json; charset=utf-8';
const MAX_BODY_BYTES = 64 * 1024 * 1024;

const store = new Store();
let requestCounter = 0;

function nextRequestId(): string {
  requestCounter += 1;
  return `req-${requestCounter}`;
}

function sendJson(res: ServerResponse, status: number, body: unknown, headers: Record<string, string> = {}): void {
  if (status === 204) {
    res.writeHead(204, { 'x-github-request-id': nextRequestId(), ...headers });
    res.end();
    return;
  }
  const text = `${JSON.stringify(body)}\n`;
  res.writeHead(status, {
    'content-type': JSON_TYPE,
    'content-length': Buffer.byteLength(text),
    'x-github-request-id': nextRequestId(),
    ...headers,
  });
  res.end(text);
}

function sendRawJson(res: ServerResponse, status: number, body: Buffer, headers: Record<string, string> = {}): void {
  res.writeHead(status, {
    'content-type': JSON_TYPE,
    'content-length': body.length,
    'x-github-request-id': nextRequestId(),
    ...headers,
  });
  res.end(body);
}

function sendRaw(res: ServerResponse, status: number, contentType: string, body: Buffer): void {
  res.writeHead(status, { 'content-type': contentType, 'content-length': body.length });
  res.end(body);
}

function readBody(req: IncomingMessage): Promise<Buffer> {
  return new Promise((resolve, reject) => {
    const chunks: Buffer[] = [];
    let total = 0;
    req.on('data', (chunk: Buffer) => {
      total += chunk.length;
      if (total > MAX_BODY_BYTES) {
        reject(new Error('body too large'));
        return;
      }
      chunks.push(chunk);
    });
    // Small bodies arrive in one chunk: return it without a concat copy.
    req.on('end', () => resolve(chunks.length === 1 ? chunks[0] : Buffer.concat(chunks)));
    req.on('error', reject);
  });
}

/** An empty, never-mutated query set for URLs without '?'. */
const EMPTY_PARAMS = new URLSearchParams();

/**
 * Split the request URL without a full URL parse when it is safe: no
 * percent-escapes and no dot segments, so the raw slice is exactly what
 * `new URL(...).pathname` would return. Anything else falls back to URL.
 */
function splitUrl(req: IncomingMessage): { path: string; query: URLSearchParams } {
  const raw = req.url ?? '/';
  try {
    const q = raw.indexOf('?');
    const path = q >= 0 ? raw.slice(0, q) : raw;
    if (!path.includes('%') && !path.includes('#') && !path.includes('/./') && !path.includes('/../')) {
      return { path, query: q >= 0 ? new URLSearchParams(raw.slice(q + 1)) : EMPTY_PARAMS };
    }
    const url = new URL(raw, 'http://entry');
    return { path: url.pathname, query: url.searchParams };
  } catch {
    return { path: '/', query: EMPTY_PARAMS };
  }
}

/** The token every API call must present (the env sets MAKEFUN_SHOP_TOKEN). */
const EXPECTED_TOKEN = process.env.MAKEFUN_SHOP_TOKEN ?? '';

function authorized(req: IncomingMessage): boolean {
  if (EXPECTED_TOKEN === '') return true;
  const header = req.headers.authorization ?? '';
  return header === `Bearer ${EXPECTED_TOKEN}`;
}

/** Build the server. It binds nothing until the caller listens. */
export function createApp(): Server {
  return createServer((req: IncomingMessage, res: ServerResponse) => {
    void (async () => {
      const method = req.method ?? 'GET';
      const { path, query } = splitUrl(req);
      try {
        // The env's boot check calls this twice: 200 {"ok":true}, no auth.
        if (method === 'GET' && path === '/healthz') {
          sendJson(res, 200, { ok: true });
          return;
        }

        // Everything else needs the shop token when one is configured.
        if (!authorized(req)) {
          sendJson(res, 401, { message: 'Bad credentials' });
          return;
        }

        const needsBody = method === 'POST' || method === 'PUT' || method === 'PATCH' || method === 'DELETE';
        const body = needsBody ? await readBody(req) : Buffer.alloc(0);

        // MCP: POST /mcp
        if (method === 'POST' && path === '/mcp') {
          const out = mcpRoute(store, body.toString('utf8'));
          if (out.status === 202) {
            res.writeHead(202);
            res.end();
            return;
          }
          sendJson(res, out.status, out.json);
          return;
        }

        // GraphQL: POST /api/graphql
        if (method === 'POST' && path === '/api/graphql') {
          const out = graphqlRoute(store, body.toString('utf8'));
          sendJson(res, out.status, out.json);
          return;
        }

        // REST v3: /api/v3[/...]
        if (path === '/api/v3' || path.startsWith('/api/v3/')) {
          const restPath = path === '/api/v3' ? '' : path.slice('/api/v3/'.length);
          const segments = restPath === '' ? [] : restPath.split('/').filter((s) => s !== '');
          const ctx: RestCtx = {
            method,
            segments,
            query,
            bodyText: body.toString('utf8'),
            host: req.headers.host ?? '127.0.0.1',
          };
          const out = restRoute(store, ctx);
          if (out.raw !== undefined) {
            sendRawJson(res, out.status, out.raw, out.headers ?? {});
            return;
          }
          sendJson(res, out.status, out.json, out.headers ?? {});
          return;
        }

        // Git smart HTTP: /{owner}/{repo}.git/...
        const gitMatch = path.match(/^\/([^/]+)\/([^/]+)\.git\/(info\/refs|git-upload-pack|git-receive-pack)$/);
        if (gitMatch) {
          const owner = decodeURIComponent(gitMatch[1]);
          const repoName = decodeURIComponent(gitMatch[2]);
          const action = gitMatch[3];
          let out: GitResult;
          const protoHeader = (req.headers['git-protocol'] ?? '').toString();
          const protocolVersion = protoHeader.replace(/^version=/, '') || undefined;
          if (action === 'info/refs' && method === 'GET') {
            out = gitInfoRefs(store, owner, repoName, query.get('service') ?? undefined, protocolVersion);
          } else if (action === 'git-upload-pack' && method === 'POST') {
            out = gitUploadPack(store, owner, repoName, body, protocolVersion);
          } else if (action === 'git-receive-pack' && method === 'POST') {
            out = gitReceivePack(store, owner, repoName, body);
          } else {
            sendJson(res, 404, { message: 'Not Found' });
            return;
          }
          sendRaw(res, out.status, out.contentType, out.body);
          return;
        }

        // Everything else: GitHub's 404 body.
        sendJson(res, 404, { message: 'Not Found' });
      } catch (err) {
        if (!res.headersSent) {
          sendJson(res, 500, { message: err instanceof Error ? err.message : 'internal error' });
        } else {
          res.destroy();
        }
      }
    })();
  });
}

// mcp.ts — Model Context Protocol over Streamable HTTP (POST /mcp).
// Modern revision 2026-07-28 first (server/discover, no handshake), with the
// legacy initialize path kept for older clients. Every tool result carries
// both a text JSON payload and structuredContent, and the tool set is a
// single constant so two tools/list answers serialize byte-identically.

import { FIXED_TS, type Store } from './store.ts';

const SERVER_NAME = 'github-core-clone-01-forge';
const SERVER_VERSION = '1.0.0';
const MODERN_VERSIONS = ['2026-07-28', '2025-11-25', '2025-06-18', '2025-03-26', '2024-11-05'];
const LEGACY_VERSIONS = ['2025-11-25', '2025-06-18', '2025-03-26', '2024-11-05'];

interface ToolDef {
  name: string;
  description: string;
  inputSchema: {
    type: 'object';
    properties: Record<string, { type: string; description: string }>;
    required: string[];
  };
}

const TOOLS: ToolDef[] = [
  {
    name: 'create_repo',
    description: 'Create a new repository owned by the authenticated user.',
    inputSchema: {
      type: 'object',
      properties: {
        name: { type: 'string', description: 'Repository name' },
        description: { type: 'string', description: 'Repository description' },
        private: { type: 'boolean', description: 'Whether the repository is private' },
      },
      required: ['name'],
    },
  },
  {
    name: 'get_repo',
    description: 'Get a repository by owner and name.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'Repository owner login' },
        repo: { type: 'string', description: 'Repository name' },
      },
      required: ['owner', 'repo'],
    },
  },
  {
    name: 'create_issue',
    description: 'Create an issue in a repository.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'Repository owner login' },
        repo: { type: 'string', description: 'Repository name' },
        title: { type: 'string', description: 'Issue title' },
        body: { type: 'string', description: 'Issue body' },
      },
      required: ['owner', 'repo', 'title'],
    },
  },
  {
    name: 'list_issues',
    description: 'List issues in a repository, optionally filtered by state.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'Repository owner login' },
        repo: { type: 'string', description: 'Repository name' },
        state: { type: 'string', description: 'Issue state: open, closed or all' },
      },
      required: ['owner', 'repo'],
    },
  },
  {
    name: 'create_pull_request',
    description: 'Create a pull request from a head branch into a base branch.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'Repository owner login' },
        repo: { type: 'string', description: 'Repository name' },
        title: { type: 'string', description: 'Pull request title' },
        body: { type: 'string', description: 'Pull request body' },
        head: { type: 'string', description: 'Head branch name' },
        base: { type: 'string', description: 'Base branch name' },
      },
      required: ['owner', 'repo', 'title', 'head', 'base'],
    },
  },
  {
    name: 'merge_pull_request',
    description: 'Merge a pull request by number using the squash method.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'Repository owner login' },
        repo: { type: 'string', description: 'Repository name' },
        pull_number: { type: 'number', description: 'Pull request number' },
      },
      required: ['owner', 'repo', 'pull_number'],
    },
  },
  {
    name: 'get_issue',
    description: 'Get one issue by number.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'Repository owner login' },
        repo: { type: 'string', description: 'Repository name' },
        issue_number: { type: 'number', description: 'Issue number' },
      },
      required: ['owner', 'repo', 'issue_number'],
    },
  },
  {
    name: 'list_pull_requests',
    description: 'List pull requests in a repository, optionally filtered by state.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'Repository owner login' },
        repo: { type: 'string', description: 'Repository name' },
        state: { type: 'string', description: 'Pull request state: open, closed or all' },
      },
      required: ['owner', 'repo'],
    },
  },
];

export interface McpResult {
  status: number;
  json: unknown;
}

function rpcError(id: unknown, code: number, message: string): McpResult {
  return { status: 200, json: { jsonrpc: '2.0', id: id ?? null, error: { code, message } } };
}

function rpcResult(id: unknown, result: unknown): McpResult {
  return { status: 200, json: { jsonrpc: '2.0', id, result } };
}

function toolText(structured: unknown, isError: boolean): McpResult['json'] {
  return {
    content: [{ type: 'text', text: typeof structured === 'string' ? structured : JSON.stringify(structured) }],
    structuredContent: typeof structured === 'object' && structured !== null ? structured : { result: structured },
    isError,
  };
}

function findTool(name: string): ToolDef | undefined {
  const norm = name.toLowerCase().replace(/[^a-z0-9]/g, '');
  return TOOLS.find((t) => t.name.replace(/[^a-z0-9]/g, '') === norm);
}

export function mcpRoute(store: Store, bodyText: string): McpResult {
  let req: unknown;
  try {
    req = JSON.parse(bodyText);
  } catch {
    return { status: 400, json: { jsonrpc: '2.0', id: null, error: { code: -32700, message: 'Parse error' } } };
  }
  const r = req as Record<string, unknown>;
  const id = 'id' in r ? r.id : null;
  const method = typeof r.method === 'string' ? r.method : '';
  const params = (r.params ?? {}) as Record<string, unknown>;

  // Notifications carry no id and expect no reply.
  if (method.startsWith('notifications/')) {
    return { status: 202, json: null };
  }

  if (method === 'server/discover') {
    return rpcResult(id, {
      supportedVersions: MODERN_VERSIONS,
      capabilities: { tools: {} },
      serverInfo: { name: SERVER_NAME, version: SERVER_VERSION },
      _meta: { 'io.modelcontextprotocol/serverInfo': { name: SERVER_NAME, version: SERVER_VERSION } },
    });
  }

  if (method === 'initialize') {
    const requested = typeof params.protocolVersion === 'string' ? params.protocolVersion : '';
    const chosen = LEGACY_VERSIONS.includes(requested) ? requested : '2025-11-25';
    return rpcResult(id, {
      protocolVersion: chosen,
      capabilities: { tools: {} },
      serverInfo: { name: SERVER_NAME, version: SERVER_VERSION },
    });
  }

  if (method === 'ping') {
    return rpcResult(id, {});
  }

  if (method === 'tools/list') {
    return rpcResult(id, { tools: TOOLS });
  }

  if (method === 'tools/call') {
    const name = typeof params.name === 'string' ? params.name : '';
    const args = (params.arguments ?? {}) as Record<string, unknown>;
    const tool = findTool(name);
    if (!tool) {
      return rpcError(id, -32602, `Unknown tool: ${name}`);
    }
    const missing = tool.inputSchema.required.filter((k) => args[k] === undefined || args[k] === null || args[k] === '');
    if (missing.length > 0) {
      return rpcResult(id, toolText({ error: `Missing required argument(s): ${missing.join(', ')}` }, true));
    }
    const owner = String(args.owner ?? store.viewerLogin);
    const repoName = String(args.repo ?? args.name ?? '');
    const repo = store.getRepo(owner, repoName);
    switch (tool.name) {
      case 'create_repo': {
        const name = String(args.name ?? '');
        const description = typeof args.description === 'string' ? args.description : '';
        const isPrivate = args.private === true;
        const { repo: created, error } = store.createRepo(name, description, isPrivate);
        if (error) {
          if (error === 'already exists') {
            const existing = store.getRepo(store.viewerLogin, name)!;
            const out = { id: existing.id, name: existing.name, full_name: `${existing.owner}/${existing.name}`, private: existing.private, description: existing.description, default_branch: existing.defaultBranch };
            return rpcResult(id, toolText(out, false));
          }
          return rpcResult(id, toolText({ error }, true));
        }
        const out = { id: created!.id, name: created!.name, full_name: `${created!.owner}/${created!.name}`, private: created!.private, description: created!.description, default_branch: created!.defaultBranch };
        return rpcResult(id, toolText(out, false));
      }
      case 'get_repo': {
        if (!repo) return rpcResult(id, toolText({ error: 'Not Found' }, true));
        const out = { id: repo.id, name: repo.name, full_name: `${repo.owner}/${repo.name}`, owner: { login: repo.owner }, private: repo.private, description: repo.description, default_branch: repo.defaultBranch };
        return rpcResult(id, toolText(out, false));
      }
      case 'create_issue': {
        if (!repo) return rpcResult(id, toolText({ error: 'Not Found' }, true));
        const title = String(args.title ?? '');
        const body = typeof args.body === 'string' ? args.body : '';
        const { issue, error } = store.createIssue(repo, title, body, []);
        if (error || !issue) return rpcResult(id, toolText({ error: error ?? 'failed' }, true));
        const out = { id: issue.id, number: issue.number, title: issue.title, body: issue.body, state: issue.state, created_at: FIXED_TS, url: `/api/v3/repos/${repo.owner}/${repo.name}/issues/${issue.number}` };
        return rpcResult(id, toolText(out, false));
      }
      case 'list_issues': {
        if (!repo) return rpcResult(id, toolText({ error: 'Not Found' }, true));
        const state = typeof args.state === 'string' ? args.state : 'open';
        const list = store.listIssues(repo, { state, sort: 'created', direction: 'desc' });
        const out = { issues: list.map((i) => ({ id: i.id, number: i.number, title: i.title, body: i.body, state: i.state })) };
        return rpcResult(id, toolText(out, false));
      }
      case 'create_pull_request': {
        if (!repo) return rpcResult(id, toolText({ error: 'Not Found' }, true));
        const title = String(args.title ?? '');
        const body = typeof args.body === 'string' ? args.body : '';
        const head = String(args.head ?? '');
        const base = String(args.base ?? '');
        const { pr, error } = store.createPull(repo, title, body, head, base);
        if (error || !pr) return rpcResult(id, toolText({ error: error ?? 'failed' }, true));
        const out = { id: pr.id, number: pr.number, pull_number: pr.number, title: pr.title, body: pr.body, state: pr.state, head: { ref: pr.headRef, sha: pr.headSha }, base: { ref: pr.baseRef, sha: pr.baseSha } };
        return rpcResult(id, toolText(out, false));
      }
      case 'merge_pull_request': {
        if (!repo) return rpcResult(id, toolText({ error: 'Not Found' }, true));
        const num = Number(args.pull_number ?? args.number ?? NaN);
        const pr = repo.pulls.get(num);
        if (!pr) return rpcResult(id, toolText({ error: 'Not Found' }, true));
        const { sha, error } = store.mergePull(repo, pr);
        if (error) return rpcResult(id, toolText({ error }, true));
        const out = { number: pr.number, pull_number: pr.number, merged: true, state: pr.state, sha, merge_commit_sha: sha };
        return rpcResult(id, toolText(out, false));
      }
      case 'get_issue': {
        if (!repo) return rpcResult(id, toolText({ error: 'Not Found' }, true));
        const num = Number(args.issue_number ?? args.number ?? NaN);
        const issue = repo.issues.get(num);
        if (!issue) return rpcResult(id, toolText({ error: 'Not Found' }, true));
        const out = { id: issue.id, number: issue.number, title: issue.title, body: issue.body, state: issue.state };
        return rpcResult(id, toolText(out, false));
      }
      case 'list_pull_requests': {
        if (!repo) return rpcResult(id, toolText({ error: 'Not Found' }, true));
        const state = typeof args.state === 'string' ? args.state : 'open';
        const list = store.listPulls(repo, state);
        const out = { pull_requests: list.map((p) => ({ id: p.id, number: p.number, title: p.title, body: p.body, state: p.state, merged: p.merged })) };
        return rpcResult(id, toolText(out, false));
      }
      default:
        return rpcError(id, -32602, `Unknown tool: ${name}`);
    }
  }

  return rpcError(id, -32601, `Method not found: ${method}`);
}

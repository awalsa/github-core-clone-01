// rest.ts — the REST v3 surface (/api/v3). Every reply is JSON with a
// non-empty X-GitHub-Request-Id; errors are {"message": string}.

import { FIXED_TS, FIXED_EPOCH_SEC, type Issue, type PullRequest, type Repo, type Store } from './store.ts';

export interface RestResult {
  status: number;
  json: unknown;
  headers?: Record<string, string>;
  /** Prebuilt JSON body bytes (with trailing newline); when set, `json` is ignored. */
  raw?: Buffer;
}

export interface RestCtx {
  method: string;
  segments: string[];
  query: URLSearchParams;
  bodyText: string;
  host: string;
}

function ok(status: number, json: unknown, headers?: Record<string, string>): RestResult {
  return { status, json, headers };
}

function err(status: number, message: string): RestResult {
  return { status, json: { message } };
}

function parseBody(ctx: RestCtx): { value: unknown; malformed: boolean } {
  if (!ctx.bodyText || ctx.bodyText.trim() === '') return { value: {}, malformed: false };
  try {
    return { value: JSON.parse(ctx.bodyText), malformed: false };
  } catch {
    return { value: null, malformed: true };
  }
}

function asRecord(v: unknown): Record<string, unknown> {
  return v !== null && typeof v === 'object' && !Array.isArray(v) ? (v as Record<string, unknown>) : {};
}

function repoView(repo: Repo): Record<string, unknown> {
  return {
    id: repo.id,
    name: repo.name,
    full_name: `${repo.owner}/${repo.name}`,
    owner: { login: repo.owner, id: 1000, type: 'User' },
    private: repo.private,
    description: repo.description,
    default_branch: repo.defaultBranch,
    created_at: FIXED_TS,
    updated_at: FIXED_TS,
    html_url: `https://example.invalid/${repo.owner}/${repo.name}`,
    url: `/api/v3/repos/${repo.owner}/${repo.name}`,
  };
}

function issueView(issue: Issue, repo: Repo): Record<string, unknown> {
  return {
    id: issue.id,
    number: issue.number,
    title: issue.title,
    body: issue.body,
    state: issue.state,
    labels: issue.labels.map((l) => {
      const lab = repo.labels.get(l);
      return { name: l, color: lab?.color ?? '000000' };
    }),
    user: { login: repo.owner, id: 1000 },
    created_at: FIXED_TS,
    updated_at: FIXED_TS,
    comments: issue.comments.length,
    url: `/api/v3/repos/${repo.owner}/${repo.name}/issues/${issue.number}`,
  };
}

function pullView(pr: PullRequest, repo: Repo): Record<string, unknown> {
  return {
    id: pr.id,
    number: pr.number,
    title: pr.title,
    body: pr.body,
    state: pr.state,
    head: { ref: pr.headRef, sha: pr.headSha, label: `${repo.owner}:${pr.headRef}` },
    base: { ref: pr.baseRef, sha: pr.baseSha, label: `${repo.owner}:${pr.baseRef}` },
    merged: pr.merged,
    merge_commit_sha: pr.mergeSha,
    merged_at: pr.mergedAt,
    user: { login: repo.owner, id: 1000 },
    created_at: FIXED_TS,
    updated_at: FIXED_TS,
    url: `/api/v3/repos/${repo.owner}/${repo.name}/pulls/${pr.number}`,
  };
}

function pageParams(query: URLSearchParams): { perPage: number; page: number } {
  const perPage = Math.min(100, Math.max(1, parseInt(query.get('per_page') ?? '30', 10) || 30));
  const page = Math.max(1, parseInt(query.get('page') ?? '1', 10) || 1);
  return { perPage, page };
}

function paginate<T>(items: T[], perPage: number, page: number): { slice: T[]; hasNext: boolean } {
  const start = (page - 1) * perPage;
  const slice = items.slice(start, start + perPage);
  return { slice, hasNext: start + perPage < items.length };
}

function linkHeader(base: string, perPage: number, page: number, hasNext: boolean): Record<string, string> {
  const links: string[] = [];
  if (hasNext) links.push(`<${base}?per_page=${perPage}&page=${page + 1}>; rel="next"`);
  if (page > 1) links.push(`<${base}?per_page=${perPage}&page=1>; rel="first"`);
  if (links.length === 0) return {};
  return { link: links.join(', ') };
}

export function restRoute(store: Store, ctx: RestCtx): RestResult {
  const { method, segments: s } = ctx;

  // GET /api/v3/ — API root
  if (s.length === 0) {
    if (method !== 'GET') return err(404, 'Not Found');
    return ok(200, { current_user_url: '/api/v3/user', rate_limit_url: '/api/v3/rate_limit' }, { 'x-oauth-scopes': 'repo, read:org' });
  }

  // GET /api/v3/meta
  if (s.length === 1 && s[0] === 'meta') {
    if (method !== 'GET') return err(404, 'Not Found');
    return ok(200, { installed_version: '1.0.0', verifiable_password_authentication: true });
  }

  // GET /api/v3/user ; POST /api/v3/user/repos ; GET /api/v3/user/repos
  if (s[0] === 'user') {
    if (s.length === 1) {
      if (method !== 'GET') return err(404, 'Not Found');
      return ok(200, { login: store.viewerLogin, id: 1000, type: 'User', name: store.viewerLogin, email: `${store.viewerLogin}@example.invalid` });
    }
    if (s.length === 2 && s[1] === 'repos') {
      if (method === 'POST') {
        const { value, malformed } = parseBody(ctx);
        if (malformed) return err(400, 'malformed JSON body');
        const b = asRecord(value);
        const name = typeof b.name === 'string' ? b.name : '';
        const description = typeof b.description === 'string' ? b.description : '';
        const isPrivate = b.private === true;
        const { repo, error } = store.createRepo(name, description, isPrivate);
        if (error === 'already exists') return err(422, `Repository ${store.viewerLogin}/${name} already exists`);
        if (error) return err(422, error);
        return ok(201, repoView(repo!));
      }
      if (method === 'GET') {
        const { perPage, page } = pageParams(ctx.query);
        const all = store.listViewerRepos();
        const { slice, hasNext } = paginate(all, perPage, page);
        const base = `http://${ctx.host}/api/v3/user/repos`;
        return ok(200, slice.map(repoView), linkHeader(base, perPage, page, hasNext));
      }
      return err(404, 'Not Found');
    }
    return err(404, 'Not Found');
  }

  // /api/v3/repos/{owner}/{repo}[...]
  if (s[0] === 'repos' && s.length >= 3) {
    const owner = decodeURIComponent(s[1]);
    const name = decodeURIComponent(s[2]);
    const repo = store.getRepo(owner, name);
    if (!repo) return err(404, 'Not Found');

    // /repos/{o}/{r}
    if (s.length === 3) {
      if (method === 'GET') return ok(200, repoView(repo));
      if (method === 'PATCH') {
        const { value, malformed } = parseBody(ctx);
        if (malformed) return err(400, 'malformed JSON body');
        const b = asRecord(value);
        if (typeof b.description === 'string') repo.description = b.description;
        if (typeof b.private === 'boolean') repo.private = b.private;
        return ok(200, repoView(repo));
      }
      if (method === 'DELETE') {
        store.deleteRepo(owner, name);
        return { status: 204, json: null };
      }
      return err(404, 'Not Found');
    }

    const tail = s.slice(3);

    // /git/refs
    if (tail[0] === 'git' && tail[1] === 'refs') {
      if (method === 'POST' && tail.length === 2) {
        const { value, malformed } = parseBody(ctx);
        if (malformed) return err(400, 'malformed JSON body');
        const b = asRecord(value);
        const ref = typeof b.ref === 'string' ? b.ref : '';
        const sha = typeof b.sha === 'string' ? b.sha : '';
        if (!ref.startsWith('refs/') || !/^[0-9a-f]{40}$/.test(sha) || !store.hasObject(sha)) {
          return err(422, 'invalid ref or sha');
        }
        if (store.getRef(repo, ref)) return err(422, 'ref already exists');
        store.setRef(repo, ref, sha);
        return ok(201, { ref, object: { sha } });
      }
      if (method === 'GET' && tail.length >= 4 && tail[2] === 'heads') {
        const branch = decodeURIComponent(tail.slice(3).join('/'));
        const sha = store.getBranchSha(repo, branch);
        if (!sha) return err(404, 'Not Found');
        return ok(200, { ref: `refs/heads/${branch}`, object: { sha } });
      }
      return err(404, 'Not Found');
    }

    // /branches
    if (tail[0] === 'branches') {
      if (method === 'GET' && tail.length === 1) {
        return ok(200, store.listBranches(repo).map((b) => ({ name: b.name, commit: { sha: b.sha } })));
      }
      if (method === 'GET' && tail.length >= 2) {
        const branch = decodeURIComponent(tail.slice(1).join('/'));
        const sha = store.getBranchSha(repo, branch);
        if (!sha) return err(404, 'Not Found');
        return ok(200, { name: branch, commit: { sha } });
      }
      return err(404, 'Not Found');
    }

    // /commits
    if (tail[0] === 'commits') {
      if (method === 'GET' && tail.length === 1) {
        const sha = ctx.query.get('sha') ?? undefined;
        const list = store.listCommits(repo, sha);
        if (list === undefined) return err(404, 'Not Found');
        return ok(200, list.map((c) => ({ sha: c.sha, commit: { message: c.message } })));
      }
      return err(404, 'Not Found');
    }

    // /contents/{path...}
    if (tail[0] === 'contents') {
      const filePath = tail.slice(1).map((p) => decodeURIComponent(p)).join('/');
      if (filePath === '') return err(404, 'Not Found');
      if (method === 'GET') {
        const ref = ctx.query.get('ref') ?? undefined;
        let tip: string | undefined;
        if (ref) {
          tip = store.resolveRefish(repo, ref);
          if (!tip) return err(404, 'Not Found');
        } else {
          tip = repo.refs.get(`refs/heads/${repo.defaultBranch}`);
          if (!tip) return err(404, 'Not Found');
        }
        const commitOid = store.resolveToCommit(tip);
        if (!commitOid) return err(404, 'Not Found');
        const files = store.filesAtCommit(commitOid);
        const entry = files.get(filePath);
        if (entry) {
          const obj = store.getObject(entry.oid);
          const data = obj?.data ?? Buffer.alloc(0);
          return ok(200, {
            type: 'file',
            encoding: 'base64',
            size: data.length,
            name: filePath.split('/').pop(),
            path: filePath,
            content: data.toString('base64'),
            sha: entry.oid,
          });
        }
        // directory listing
        const prefix = `${filePath}/`;
        const children = new Map<string, { name: string; type: 'file' | 'dir'; path: string; sha: string }>();
        for (const [p, f] of files) {
          if (p === filePath) continue;
          if (!p.startsWith(prefix)) continue;
          const rest = p.slice(prefix.length);
          const first = rest.split('/')[0];
          const childPath = `${filePath}/${first}`;
          if (rest.includes('/')) {
            if (!children.has(first)) children.set(first, { name: first, type: 'dir', path: childPath, sha: '' });
          } else {
            children.set(first, { name: first, type: 'file', path: childPath, sha: f.oid });
          }
        }
        if (children.size === 0) return err(404, 'Not Found');
        return ok(200, [...children.values()].sort((a, b) => (a.name < b.name ? -1 : 1)));
      }
      if (method === 'PUT') {
        const { value, malformed } = parseBody(ctx);
        if (malformed) return err(400, 'malformed JSON body');
        const b = asRecord(value);
        const message = typeof b.message === 'string' ? b.message : 'update';
        const contentB64 = typeof b.content === 'string' ? b.content : '';
        const branch = typeof b.branch === 'string' ? b.branch : repo.defaultBranch;
        const sha = typeof b.sha === 'string' ? b.sha : undefined;
        let content: Buffer;
        try {
          content = Buffer.from(contentB64.replace(/\s/g, ''), 'base64');
        } catch {
          return err(422, 'invalid content');
        }
        let tip = store.getBranchSha(repo, branch);
        if (!tip) {
          // No such branch: create it from the default branch tip (or start unborn).
          const defaultTip = store.getBranchSha(repo, repo.defaultBranch);
          if (defaultTip) {
            store.setRef(repo, `refs/heads/${branch}`, defaultTip);
            tip = defaultTip;
          }
        }
        const commitOid = tip ? store.resolveToCommit(tip) : undefined;
        const files = commitOid ? store.filesAtCommit(commitOid) : new Map();
        const existing = files.get(filePath);
        if (existing && sha && sha !== existing.oid) return err(409, 'sha does not match');
        if (existing && !sha) return err(409, 'sha required to update an existing file');
        const changes = new Map<string, Buffer | null>([[filePath, content]]);
        const newCommit = store.commitFiles(commitOid ?? null, changes, message, FIXED_EPOCH_SEC);
        store.setRef(repo, `refs/heads/${branch}`, newCommit);
        const blobOid = store.filesAtCommit(newCommit).get(filePath)!.oid;
        return ok(existing ? 200 : 201, {
          content: { name: filePath.split('/').pop(), path: filePath, sha: blobOid, size: content.length, type: 'file', encoding: 'base64' },
          commit: { sha: newCommit },
        });
      }
      if (method === 'DELETE') {
        const { value, malformed } = parseBody(ctx);
        if (malformed) return err(400, 'malformed JSON body');
        const b = asRecord(value);
        const message = typeof b.message === 'string' ? b.message : 'delete';
        const sha = typeof b.sha === 'string' ? b.sha : undefined;
        const branch = typeof b.branch === 'string' ? b.branch : repo.defaultBranch;
        const tip = store.getBranchSha(repo, branch);
        if (!tip) return err(404, 'Not Found');
        const commitOid = store.resolveToCommit(tip);
        if (!commitOid) return err(404, 'Not Found');
        const files = store.filesAtCommit(commitOid);
        const existing = files.get(filePath);
        if (!existing) return err(404, 'Not Found');
        if (sha && sha !== existing.oid) return err(409, 'sha does not match');
        const changes = new Map<string, Buffer | null>([[filePath, null]]);
        const newCommit = store.commitFiles(commitOid, changes, message, FIXED_EPOCH_SEC);
        store.setRef(repo, `refs/heads/${branch}`, newCommit);
        return ok(200, { commit: { sha: newCommit } });
      }
      return err(404, 'Not Found');
    }

    // /labels
    if (tail[0] === 'labels') {
      if (method === 'POST' && tail.length === 1) {
        const { value, malformed } = parseBody(ctx);
        if (malformed) return err(400, 'malformed JSON body');
        const b = asRecord(value);
        const name = typeof b.name === 'string' ? b.name : '';
        const color = typeof b.color === 'string' ? b.color : '000000';
        if (!name) return err(422, 'label name required');
        const label = store.createLabel(repo, name, color);
        return ok(201, label);
      }
      return err(404, 'Not Found');
    }

    // /issues...
    if (tail[0] === 'issues') {
      if (tail.length === 1) {
        if (method === 'POST') {
          const { value, malformed } = parseBody(ctx);
          if (malformed) return err(400, 'malformed JSON body');
          const b = asRecord(value);
          const title = typeof b.title === 'string' ? b.title : '';
          const body = typeof b.body === 'string' ? b.body : '';
          const labels = Array.isArray(b.labels) ? b.labels.filter((l): l is string => typeof l === 'string') : [];
          const { issue, error } = store.createIssue(repo, title, body, labels);
          if (error) return err(422, error);
          return ok(201, issueView(issue!, repo));
        }
        if (method === 'GET') {
          const state = ctx.query.get('state') ?? 'open';
          const sort = ctx.query.get('sort') ?? 'created';
          const direction = ctx.query.get('direction') ?? 'desc';
          const all = store.listIssues(repo, { state, sort, direction });
          const { perPage, page } = pageParams(ctx.query);
          const { slice, hasNext } = paginate(all, perPage, page);
          const base = `http://${ctx.host}/api/v3/repos/${owner}/${name}/issues`;
          return ok(200, slice.map((i) => issueView(i, repo)), linkHeader(base, perPage, page, hasNext));
        }
        return err(404, 'Not Found');
      }
      const num = parseInt(tail[1], 10);
      if (Number.isNaN(num)) return err(404, 'Not Found');
      const issue = store.getIssue(repo, num);
      if (tail.length === 2) {
        if (!issue) return err(404, 'Not Found');
        if (method === 'GET') {
          // The issue view is immutable between mutations: serve the cached
          // JSON bytes instead of rebuilding + stringifying per read.
          if (issue.viewCache === null) {
            issue.viewCache = Buffer.from(`${JSON.stringify(issueView(issue, repo))}\n`, 'utf8');
          }
          return { status: 200, json: null, raw: issue.viewCache };
        }
        if (method === 'PATCH') {
          const { value, malformed } = parseBody(ctx);
          if (malformed) return err(400, 'malformed JSON body');
          const b = asRecord(value);
          if (typeof b.state === 'string' && (b.state === 'open' || b.state === 'closed')) issue.state = b.state;
          if (typeof b.title === 'string') issue.title = b.title;
          if (typeof b.body === 'string') issue.body = b.body;
          issue.viewCache = null;
          return ok(200, issueView(issue, repo));
        }
        return err(404, 'Not Found');
      }
      if (tail.length === 3 && tail[2] === 'comments') {
        if (!issue) return err(404, 'Not Found');
        if (method === 'POST') {
          const { value, malformed } = parseBody(ctx);
          if (malformed) return err(400, 'malformed JSON body');
          const b = asRecord(value);
          const body = typeof b.body === 'string' ? b.body : '';
          const { comment, error } = store.createComment(repo, num, body);
          if (error) return err(404, error);
          return ok(201, { id: comment!.id, body: comment!.body, user: { login: repo.owner, id: 1000 }, created_at: FIXED_TS });
        }
        if (method === 'GET') {
          return ok(200, issue.comments.map((c) => ({ id: c.id, body: c.body, user: { login: repo.owner, id: 1000 }, created_at: FIXED_TS })));
        }
        return err(404, 'Not Found');
      }
      return err(404, 'Not Found');
    }

    // /pulls...
    if (tail[0] === 'pulls') {
      if (tail.length === 1) {
        if (method === 'POST') {
          const { value, malformed } = parseBody(ctx);
          if (malformed) return err(400, 'malformed JSON body');
          const b = asRecord(value);
          const title = typeof b.title === 'string' ? b.title : '';
          const body = typeof b.body === 'string' ? b.body : '';
          const head = typeof b.head === 'string' ? b.head : '';
          const base = typeof b.base === 'string' ? b.base : '';
          const { pr, error } = store.createPull(repo, title, body, head, base);
          if (error) return err(422, error);
          return ok(201, pullView(pr!, repo));
        }
        if (method === 'GET') {
          const state = ctx.query.get('state') ?? 'open';
          const all = store.listPulls(repo, state);
          const { perPage, page } = pageParams(ctx.query);
          const { slice, hasNext } = paginate(all, perPage, page);
          const base = `http://${ctx.host}/api/v3/repos/${owner}/${name}/pulls`;
          return ok(200, slice.map((p) => pullView(p, repo)), linkHeader(base, perPage, page, hasNext));
        }
        return err(404, 'Not Found');
      }
      const num = parseInt(tail[1], 10);
      if (Number.isNaN(num)) return err(404, 'Not Found');
      const pr = store.getPull(repo, num);
      if (tail.length === 2) {
        if (!pr) return err(404, 'Not Found');
        if (method === 'GET') return ok(200, pullView(pr, repo));
        return err(404, 'Not Found');
      }
      if (tail.length === 3 && tail[2] === 'files') {
        if (!pr) return err(404, 'Not Found');
        if (method !== 'GET') return err(404, 'Not Found');
        const changes = store.pullChanges(repo, pr);
        if (!changes) return err(404, 'Not Found');
        return ok(200, changes.map((c) => ({ filename: c.path, status: c.status, additions: c.additions, deletions: c.deletions, changes: c.additions + c.deletions })));
      }
      if (tail.length === 3 && tail[2] === 'merge') {
        if (!pr) return err(404, 'Not Found');
        if (method !== 'PUT') return err(404, 'Not Found');
        const { sha, error } = store.mergePull(repo, pr);
        if (error === 'conflict') return err(405, 'merge conflict');
        if (error) return err(405, error);
        return ok(200, { sha, merged: true, message: `Pull Request #${pr.number} merged` });
      }
      return err(404, 'Not Found');
    }

    return err(404, 'Not Found');
  }

  return err(404, 'Not Found');
}

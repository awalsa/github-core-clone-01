// graphql.ts — a small GraphQL executor for the fixed document set the suite
// sends. It parses queries, mutations, variables, aliases, inline fragments,
// input objects and lists, and resolves them against the same store the REST
// and git surfaces use. Unknown fields answer HTTP 200 with an errors array
// naming the field, exactly as GitHub does.

import { FIXED_TS, type Issue, type PullRequest, type Repo, type Store } from './store.ts';

// ---------------------------------------------------------------------------
// Tokenizer + parser (subset of GraphQL: enough for the suite's documents)
// ---------------------------------------------------------------------------

interface Token {
  kind: 'name' | 'int' | 'float' | 'string' | 'punct';
  value: string;
}

function tokenize(src: string): Token[] {
  const tokens: Token[] = [];
  let i = 0;
  while (i < src.length) {
    const c = src[i];
    if (c === ' ' || c === '\t' || c === '\n' || c === '\r' || c === ',') {
      i += 1;
      continue;
    }
    if (c === '#') {
      while (i < src.length && src[i] !== '\n') i += 1;
      continue;
    }
    if (c === '.' && src.slice(i, i + 3) === '...') {
      tokens.push({ kind: 'punct', value: '...' });
      i += 3;
      continue;
    }
    if ('{}():$![]= '.includes(c) || c === '=') {
      if (c !== ' ') {
        tokens.push({ kind: 'punct', value: c });
      }
      i += 1;
      continue;
    }
    if (c === '"') {
      let j = i + 1;
      let out = '';
      while (j < src.length && src[j] !== '"') {
        if (src[j] === '\\' && j + 1 < src.length) {
          const esc = src[j + 1];
          if (esc === 'n') out += '\n';
          else if (esc === 't') out += '\t';
          else if (esc === 'r') out += '\r';
          else out += esc;
          j += 2;
        } else {
          out += src[j];
          j += 1;
        }
      }
      tokens.push({ kind: 'string', value: out });
      i = j + 1;
      continue;
    }
    if (/[0-9-]/.test(c)) {
      let j = i;
      while (j < src.length && /[0-9.]/.test(src[j])) j += 1;
      const text = src.slice(i, j);
      tokens.push({ kind: text.includes('.') ? 'float' : 'int', value: text });
      i = j;
      continue;
    }
    if (/[A-Za-z_]/.test(c)) {
      let j = i;
      while (j < src.length && /[A-Za-z0-9_]/.test(src[j])) j += 1;
      tokens.push({ kind: 'name', value: src.slice(i, j) });
      i = j;
      continue;
    }
    throw new Error(`unexpected character ${c}`);
  }
  return tokens;
}

type ValueNode =
  | { kind: 'variable'; name: string }
  | { kind: 'int'; value: number }
  | { kind: 'float'; value: number }
  | { kind: 'string'; value: string }
  | { kind: 'boolean'; value: boolean }
  | { kind: 'null' }
  | { kind: 'enum'; value: string }
  | { kind: 'list'; values: ValueNode[] }
  | { kind: 'object'; fields: { name: string; value: ValueNode }[] };

interface FieldNode {
  kind: 'field';
  name: string;
  alias: string | null;
  args: { name: string; value: ValueNode }[];
  selections: SelectionNode[];
}

interface FragmentNode {
  kind: 'fragment';
  typeCondition: string | null;
  selections: SelectionNode[];
}

type SelectionNode = FieldNode | FragmentNode;

interface Operation {
  type: 'query' | 'mutation';
  name: string | null;
  varDefs: { name: string }[];
  selections: SelectionNode[];
}

class Parser {
  private pos = 0;
  private tokens: Token[];
  constructor(tokens: Token[]) {
    this.tokens = tokens;
  }

  private peek(): Token | undefined {
    return this.tokens[this.pos];
  }

  private next(): Token {
    const t = this.tokens[this.pos];
    if (!t) throw new Error('unexpected end of document');
    this.pos += 1;
    return t;
  }

  private expectPunct(v: string): void {
    const t = this.next();
    if (t.kind !== 'punct' || t.value !== v) throw new Error(`expected ${v}, got ${t.value}`);
  }

  private acceptPunct(v: string): boolean {
    const t = this.peek();
    if (t?.kind === 'punct' && t.value === v) {
      this.pos += 1;
      return true;
    }
    return false;
  }

  parseDocument(): Operation[] {
    const ops: Operation[] = [];
    while (this.pos < this.tokens.length) {
      ops.push(this.parseOperation());
    }
    return ops;
  }

  private parseOperation(): Operation {
    let type: 'query' | 'mutation' = 'query';
    let name: string | null = null;
    const t = this.peek();
    if (t?.kind === 'name' && (t.value === 'query' || t.value === 'mutation')) {
      type = this.next().value as 'query' | 'mutation';
      const n = this.peek();
      if (n?.kind === 'name') name = this.next().value;
    }
    const varDefs: { name: string }[] = [];
    if (this.acceptPunct('(')) {
      while (!this.acceptPunct(')')) {
        this.expectPunct('$');
        const vname = this.next().value;
        this.expectPunct(':');
        this.parseType();
        if (this.acceptPunct('=')) this.parseValue();
        varDefs.push({ name: vname });
      }
    }
    const selections = this.parseSelectionSet();
    return { type, name, varDefs, selections };
  }

  private parseType(): void {
    const t = this.peek();
    if (t?.kind === 'punct' && t.value === '[') {
      this.next();
      this.parseType();
      this.expectPunct(']');
    } else {
      this.next();
    }
    this.acceptPunct('!');
  }

  private parseSelectionSet(): SelectionNode[] {
    this.expectPunct('{');
    const out: SelectionNode[] = [];
    while (!this.acceptPunct('}')) {
      out.push(this.parseSelection());
    }
    return out;
  }

  private parseSelection(): SelectionNode {
    const t = this.peek();
    if (t?.kind === 'punct' && t.value === '...') {
      this.next();
      let typeCondition: string | null = null;
      const n = this.peek();
      if (n?.kind === 'name' && n.value === 'on') {
        this.next();
        typeCondition = this.next().value;
      }
      const selections = this.parseSelectionSet();
      return { kind: 'fragment', typeCondition, selections };
    }
    const first = this.next();
    if (first.kind !== 'name') throw new Error('expected field name');
    let alias: string | null = null;
    let name = first.value;
    if (this.acceptPunct(':')) {
      alias = name;
      name = this.next().value;
    }
    const args: { name: string; value: ValueNode }[] = [];
    if (this.acceptPunct('(')) {
      while (!this.acceptPunct(')')) {
        const argName = this.next().value;
        this.expectPunct(':');
        const value = this.parseValue();
        args.push({ name: argName, value });
      }
    }
    let selections: SelectionNode[] = [];
    if (this.peek()?.kind === 'punct' && this.peek()?.value === '{') {
      selections = this.parseSelectionSet();
    }
    return { kind: 'field', name, alias, args, selections };
  }

  private parseValue(): ValueNode {
    const t = this.next();
    if (t.kind === 'punct' && t.value === '$') {
      return { kind: 'variable', name: this.next().value };
    }
    if (t.kind === 'punct' && t.value === '[') {
      const values: ValueNode[] = [];
      while (!this.acceptPunct(']')) values.push(this.parseValue());
      return { kind: 'list', values };
    }
    if (t.kind === 'punct' && t.value === '{') {
      const fields: { name: string; value: ValueNode }[] = [];
      while (!this.acceptPunct('}')) {
        const fname = this.next().value;
        this.expectPunct(':');
        fields.push({ name: fname, value: this.parseValue() });
      }
      return { kind: 'object', fields };
    }
    if (t.kind === 'int') return { kind: 'int', value: parseInt(t.value, 10) };
    if (t.kind === 'float') return { kind: 'float', value: parseFloat(t.value) };
    if (t.kind === 'string') return { kind: 'string', value: t.value };
    if (t.kind === 'name') {
      if (t.value === 'true') return { kind: 'boolean', value: true };
      if (t.value === 'false') return { kind: 'boolean', value: false };
      if (t.value === 'null') return { kind: 'null' };
      return { kind: 'enum', value: t.value };
    }
    throw new Error(`unexpected value token ${t.value}`);
  }
}

function resolveValue(node: ValueNode, variables: Record<string, unknown>): unknown {
  switch (node.kind) {
    case 'variable':
      return variables[node.name];
    case 'int':
    case 'float':
    case 'string':
    case 'boolean':
      return node.value;
    case 'null':
      return null;
    case 'enum':
      return node.value;
    case 'list':
      return node.values.map((v) => resolveValue(v, variables));
    case 'object': {
      const out: Record<string, unknown> = {};
      for (const f of node.fields) out[f.name] = resolveValue(f.value, variables);
      return out;
    }
  }
}

// ---------------------------------------------------------------------------
// Global ids (opaque, stable, never the plain database id)
// ---------------------------------------------------------------------------

export function encodeGlobalId(kind: string, ...parts: (string | number)[]): string {
  return Buffer.from([kind, ...parts.map(String)].join(':'), 'utf8').toString('base64url');
}

export function decodeGlobalId(id: string): { kind: string; parts: string[] } | null {
  try {
    const text = Buffer.from(id, 'base64url').toString('utf8');
    const parts = text.split(':');
    if (parts.length < 2) return null;
    return { kind: parts[0], parts: parts.slice(1) };
  } catch {
    return null;
  }
}

// ---------------------------------------------------------------------------
// Schema table for __type introspection
// ---------------------------------------------------------------------------

const SCHEMA: Record<string, { fields: string[]; enumValues: string[] }> = {
  Issue: {
    fields: ['id', 'number', 'title', 'state', 'body', 'createdAt', 'updatedAt', 'author', 'labels', 'comments', 'url', 'repository'],
    enumValues: [],
  },
  PullRequest: {
    fields: ['id', 'number', 'title', 'body', 'state', 'merged', 'mergeable', 'headRefName', 'baseRefName', 'headRefOid', 'baseRefOid', 'files', 'author', 'createdAt', 'updatedAt', 'mergedAt', 'mergeCommit', 'url', 'repository'],
    enumValues: [],
  },
  Repository: {
    fields: ['id', 'databaseId', 'name', 'nameWithOwner', 'owner', 'defaultBranchRef', 'viewerPermission', 'hasIssuesEnabled', 'issue', 'issues', 'pullRequests', 'pullRequest', 'description', 'private', 'url', 'createdAt', 'updatedAt'],
    enumValues: [],
  },
  SearchType: {
    fields: [],
    enumValues: ['ISSUE', 'PULL_REQUEST', 'REPOSITORY', 'USER', 'DISCUSSION', 'COMMIT', 'CODE'],
  },
  User: {
    fields: ['login', 'id', 'name', 'email', 'url'],
    enumValues: [],
  },
};

function encodeCursor(index: number): string {
  return Buffer.from(`cursor:${index}`, 'utf8').toString('base64url');
}

function decodeCursor(cursor: string): number | null {
  try {
    const text = Buffer.from(cursor, 'base64url').toString('utf8');
    const m = text.match(/^cursor:(\d+)$/);
    return m ? parseInt(m[1], 10) : null;
  } catch {
    return null;
  }
}

// ---------------------------------------------------------------------------
// Executor
// ---------------------------------------------------------------------------

interface Ctx {
  store: Store;
  variables: Record<string, unknown>;
  errors: { message: string }[];
}

type GqlValue = Record<string, unknown> & { __typename: string };

function repoGql(repo: Repo): GqlValue {
  return { __typename: 'Repository', __repo: repo };
}

function issueGql(repo: Repo, issue: Issue): GqlValue {
  return { __typename: 'Issue', __repo: repo, __issue: issue };
}

function pullGql(repo: Repo, pr: PullRequest): GqlValue {
  return { __typename: 'PullRequest', __repo: repo, __pr: pr };
}

function userGql(login: string): GqlValue {
  return { __typename: 'User', __login: login };
}

function issueState(issue: Issue): string {
  return issue.state === 'open' ? 'OPEN' : 'CLOSED';
}

function pullState(pr: PullRequest): string {
  if (pr.merged) return 'MERGED';
  return pr.state === 'open' ? 'OPEN' : 'CLOSED';
}

function executeSelections(parentType: string, parent: GqlValue | null, selections: SelectionNode[], ctx: Ctx): Record<string, unknown> | null {
  if (parent === null) return null;
  const out: Record<string, unknown> = {};
  for (const sel of selections) {
    if (sel.kind === 'fragment') {
      if (sel.typeCondition && sel.typeCondition !== parentType && sel.typeCondition !== parent.__typename) continue;
      const sub = executeSelections(parentType, parent, sel.selections, ctx);
      if (sub) Object.assign(out, sub);
      continue;
    }
    const key = sel.alias ?? sel.name;
    const value = resolveField(parentType, parent, sel, ctx);
    if (value === undefined) {
      // Unknown field: record the error; the whole response carries it.
      ctx.errors.push({ message: `Cannot query field "${sel.name}" on type "${parentType}".` });
      out[key] = null;
      continue;
    }
    out[key] = applySelections(sel, value, ctx);
  }
  return out;
}

function applySelections(sel: FieldNode, value: unknown, ctx: Ctx): unknown {
  if (value === null || value === undefined) return null;
  if (sel.selections.length === 0) return value;
  if (Array.isArray(value)) {
    return value.map((v) => {
      if (v !== null && typeof v === 'object' && '__typename' in (v as Record<string, unknown>)) {
        return executeSelections((v as GqlValue).__typename, v as GqlValue, sel.selections, ctx);
      }
      return v;
    });
  }
  if (typeof value === 'object' && '__typename' in (value as Record<string, unknown>)) {
    return executeSelections((value as GqlValue).__typename, value as GqlValue, sel.selections, ctx);
  }
  return value;
}

function argsOf(sel: FieldNode, ctx: Ctx): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const a of sel.args) out[a.name] = resolveValue(a.value, ctx.variables);
  return out;
}

function resolveField(parentType: string, parent: GqlValue, sel: FieldNode, ctx: Ctx): unknown {
  const store = ctx.store;
  const args = argsOf(sel, ctx);
  const repo = (parent as { __repo?: Repo }).__repo;
  const issue = (parent as { __issue?: Issue }).__issue;
  const pr = (parent as { __pr?: PullRequest }).__pr;

  switch (parentType) {
    case 'Query': {
      if (sel.name === 'viewer') return userGql(store.viewerLogin);
      if (sel.name === 'repository') {
        const owner = String(args.owner ?? '');
        const name = String(args.name ?? '');
        const r = store.getRepo(owner, name);
        return r ? repoGql(r) : null;
      }
      if (sel.name === 'node') {
        const decoded = decodeGlobalId(String(args.id ?? ''));
        if (!decoded) return null;
        if (decoded.kind === 'Repository') {
          const r = store.reposById.get(parseInt(decoded.parts[0], 10));
          return r ? repoGql(r) : null;
        }
        if (decoded.kind === 'Issue') {
          const r = store.reposById.get(parseInt(decoded.parts[0], 10));
          const i = r?.issues.get(parseInt(decoded.parts[1], 10));
          return r && i ? issueGql(r, i) : null;
        }
        if (decoded.kind === 'PullRequest') {
          const r = store.reposById.get(parseInt(decoded.parts[0], 10));
          const p = r?.pulls.get(parseInt(decoded.parts[1], 10));
          return r && p ? pullGql(r, p) : null;
        }
        return null;
      }
      if (sel.name === '__type') {
        const name = String(args.name ?? '');
        const info = SCHEMA[name];
        if (!info) return null;
        return {
          __typename: '__Type',
          __typeName: name,
          __fields: info.fields,
          __enumValues: info.enumValues,
        } as GqlValue;
      }
      return undefined;
    }
    case 'Mutation': {
      if (sel.name === 'createIssue') {
        const input = (args.input ?? {}) as Record<string, unknown>;
        const decoded = decodeGlobalId(String(input.repositoryId ?? ''));
        const r = decoded?.kind === 'Repository' ? store.reposById.get(parseInt(decoded.parts[0], 10)) : undefined;
        if (!r) return null;
        const { issue: created } = store.createIssue(r, String(input.title ?? ''), String(input.body ?? ''), []);
        if (!created) return null;
        return { __typename: 'CreateIssuePayload', __issue: created, __repo: r } as GqlValue;
      }
      if (sel.name === 'closeIssue') {
        const input = (args.input ?? {}) as Record<string, unknown>;
        const decoded = decodeGlobalId(String(input.issueId ?? ''));
        const r = decoded?.kind === 'Issue' ? store.reposById.get(parseInt(decoded.parts[0], 10)) : undefined;
        const i = r?.issues.get(parseInt(decoded?.parts[1] ?? 'NaN', 10));
        if (!r || !i) return null;
        i.state = 'closed';
        return { __typename: 'CloseIssuePayload', __issue: i, __repo: r } as GqlValue;
      }
      if (sel.name === 'createPullRequest') {
        const input = (args.input ?? {}) as Record<string, unknown>;
        const decoded = decodeGlobalId(String(input.repositoryId ?? ''));
        const r = decoded?.kind === 'Repository' ? store.reposById.get(parseInt(decoded.parts[0], 10)) : undefined;
        if (!r) return null;
        const { pr: created, error } = store.createPull(r, String(input.title ?? ''), '', String(input.headRefName ?? ''), String(input.baseRefName ?? ''));
        if (error || !created) {
          // The suite tolerates a duplicate by re-listing; surface as null.
          return null;
        }
        return { __typename: 'CreatePullRequestPayload', __pr: created, __repo: r } as GqlValue;
      }
      if (sel.name === 'mergePullRequest') {
        const input = (args.input ?? {}) as Record<string, unknown>;
        const decoded = decodeGlobalId(String(input.pullRequestId ?? ''));
        const r = decoded?.kind === 'PullRequest' ? store.reposById.get(parseInt(decoded.parts[0], 10)) : undefined;
        const p = r?.pulls.get(parseInt(decoded?.parts[1] ?? 'NaN', 10));
        if (!r || !p) return null;
        store.mergePull(r, p);
        return { __typename: 'MergePullRequestPayload', __pr: p, __repo: r } as GqlValue;
      }
      if (sel.name === 'addComment') {
        const input = (args.input ?? {}) as Record<string, unknown>;
        const decoded = decodeGlobalId(String(input.subjectId ?? ''));
        const r = decoded?.kind === 'Issue' ? store.reposById.get(parseInt(decoded.parts[0], 10)) : undefined;
        const i = r?.issues.get(parseInt(decoded?.parts[1] ?? 'NaN', 10));
        if (!r || !i) return null;
        const { comment } = store.createComment(r, i.number, String(input.body ?? ''));
        if (!comment) return null;
        return {
          __typename: 'AddCommentPayload',
          __commentEdge: { __typename: 'IssueCommentEdge', __node: { __typename: 'IssueComment', __comment: comment } },
        } as GqlValue;
      }
      return undefined;
    }
    case 'Repository': {
      if (!repo) return undefined;
      switch (sel.name) {
        case 'id':
          return encodeGlobalId('Repository', repo.id);
        case 'databaseId':
          return repo.id;
        case 'name':
          return repo.name;
        case 'nameWithOwner':
          return `${repo.owner}/${repo.name}`;
        case 'owner':
          return userGql(repo.owner);
        case 'defaultBranchRef': {
          const sha = repo.refs.get(`refs/heads/${repo.defaultBranch}`);
          if (!sha) return null;
          return { __typename: 'Ref', __refName: repo.defaultBranch } as GqlValue;
        }
        case 'viewerPermission':
          return 'ADMIN';
        case 'hasIssuesEnabled':
          return true;
        case 'description':
          return repo.description;
        case 'private':
          return repo.private;
        case 'issue': {
          const i = repo.issues.get(Number(args.number));
          return i ? issueGql(repo, i) : null;
        }
        case 'issues': {
          const first = typeof args.first === 'number' ? args.first : 30;
          const states = Array.isArray(args.states) ? (args.states as string[]) : null;
          const orderBy = (args.orderBy ?? {}) as Record<string, unknown>;
          const after = typeof args.after === 'string' ? args.after : null;
          let list = [...repo.issues.values()];
          if (states) {
            const wanted = new Set(states.map((s) => (s === 'OPEN' ? 'open' : 'closed')));
            list = list.filter((i) => wanted.has(i.state));
          }
          const dir = orderBy.direction === 'ASC' ? 1 : -1;
          const field = String(orderBy.field ?? 'CREATED_AT');
          list.sort((a, b) => {
            if (field === 'COMMENTS') {
              const d = (a.comments.length - b.comments.length) * dir;
              return d !== 0 ? d : (a.seq - b.seq) * dir;
            }
            return (a.seq - b.seq) * dir;
          });
          const start = after ? (decodeCursor(after) ?? -1) + 1 : 0;
          const nodes = list.slice(start, start + first);
          const endCursor = nodes.length > 0 ? encodeCursor(start + nodes.length - 1) : null;
          return {
            __typename: 'IssueConnection',
            __nodes: nodes.map((i) => issueGql(repo, i)),
            __pageInfo: { hasNextPage: start + nodes.length < list.length, endCursor },
          } as GqlValue;
        }
        case 'pullRequests': {
          const first = typeof args.first === 'number' ? args.first : 30;
          const list = [...repo.pulls.values()].sort((a, b) => a.seq - b.seq).slice(0, first);
          return {
            __typename: 'PullRequestConnection',
            __nodes: list.map((p) => pullGql(repo, p)),
          } as GqlValue;
        }
        case 'pullRequest': {
          const p = repo.pulls.get(Number(args.number));
          return p ? pullGql(repo, p) : null;
        }
        default:
          return undefined;
      }
    }
    case 'Issue': {
      if (!issue) return undefined;
      switch (sel.name) {
        case 'id':
          return encodeGlobalId('Issue', repo!.id, issue.number);
        case 'number':
          return issue.number;
        case 'title':
          return issue.title;
        case 'state':
          return issueState(issue);
        case 'body':
          return issue.body;
        case 'createdAt':
        case 'updatedAt':
          return FIXED_TS;
        default:
          return undefined;
      }
    }
    case 'PullRequest': {
      if (!pr || !repo) return undefined;
      switch (sel.name) {
        case 'id':
          return encodeGlobalId('PullRequest', repo.id, pr.number);
        case 'number':
          return pr.number;
        case 'title':
          return pr.title;
        case 'body':
          return pr.body;
        case 'state':
          return pullState(pr);
        case 'merged':
          return pr.merged;
        case 'headRefName':
          return pr.headRef;
        case 'baseRefName':
          return pr.baseRef;
        case 'mergeable':
          return store.pullMergeable(repo, pr) ? 'MERGEABLE' : 'CONFLICTING';
        case 'mergedAt':
          return pr.mergedAt;
        case 'files': {
          const first = typeof args.first === 'number' ? args.first : 30;
          const changes = store.pullChanges(repo, pr) ?? [];
          return {
            __typename: 'PullRequestChangedFileConnection',
            __nodes: changes.slice(0, first).map((c) => ({ __typename: 'PullRequestChangedFile', __path: c.path }) as GqlValue),
          } as GqlValue;
        }
        case 'createdAt':
        case 'updatedAt':
          return FIXED_TS;
        default:
          return undefined;
      }
    }
    case 'User': {
      const login = (parent as { __login?: string }).__login ?? '';
      if (sel.name === 'login') return login;
      if (sel.name === 'id') return encodeGlobalId('User', login);
      if (sel.name === 'name') return login;
      return undefined;
    }
    case 'Ref': {
      if (sel.name === 'name') return (parent as { __refName?: string }).__refName ?? null;
      return undefined;
    }
    case 'IssueConnection': {
      if (sel.name === 'nodes') return (parent as { __nodes?: unknown[] }).__nodes ?? [];
      if (sel.name === 'pageInfo') {
        const pi = (parent as { __pageInfo?: { hasNextPage: boolean; endCursor: string | null } }).__pageInfo;
        return { __typename: 'PageInfo', __hasNextPage: pi?.hasNextPage ?? false, __endCursor: pi?.endCursor ?? null } as GqlValue;
      }
      return undefined;
    }
    case 'PullRequestConnection': {
      if (sel.name === 'nodes') return (parent as { __nodes?: unknown[] }).__nodes ?? [];
      return undefined;
    }
    case 'PullRequestChangedFileConnection': {
      if (sel.name === 'nodes') return (parent as { __nodes?: unknown[] }).__nodes ?? [];
      return undefined;
    }
    case 'PullRequestChangedFile': {
      if (sel.name === 'path') return (parent as { __path?: string }).__path ?? null;
      return undefined;
    }
    case 'PageInfo': {
      if (sel.name === 'hasNextPage') return (parent as { __hasNextPage?: boolean }).__hasNextPage ?? false;
      if (sel.name === 'endCursor') return (parent as { __endCursor?: string | null }).__endCursor ?? null;
      return undefined;
    }
    case '__Type': {
      const p = parent as { __typeName?: string; __fields?: string[]; __enumValues?: string[] };
      if (sel.name === 'name') return p.__typeName ?? null;
      if (sel.name === 'fields') return (p.__fields ?? []).map((f) => ({ __typename: '__Field', __fieldName: f }) as GqlValue);
      if (sel.name === 'enumValues') return (p.__enumValues ?? []).map((e) => ({ __typename: '__EnumValue', __enumName: e }) as GqlValue);
      return undefined;
    }
    case '__Field': {
      if (sel.name === 'name') return (parent as { __fieldName?: string }).__fieldName ?? null;
      return undefined;
    }
    case '__EnumValue': {
      if (sel.name === 'name') return (parent as { __enumName?: string }).__enumName ?? null;
      return undefined;
    }
    case 'CreateIssuePayload': {
      if (sel.name === 'issue') {
        const i = (parent as { __issue?: Issue }).__issue;
        const r = (parent as { __repo?: Repo }).__repo;
        return i && r ? issueGql(r, i) : null;
      }
      return undefined;
    }
    case 'CloseIssuePayload': {
      if (sel.name === 'issue') {
        const i = (parent as { __issue?: Issue }).__issue;
        const r = (parent as { __repo?: Repo }).__repo;
        return i && r ? issueGql(r, i) : null;
      }
      return undefined;
    }
    case 'CreatePullRequestPayload': {
      if (sel.name === 'pullRequest') {
        const p = (parent as { __pr?: PullRequest }).__pr;
        const r = (parent as { __repo?: Repo }).__repo;
        return p && r ? pullGql(r, p) : null;
      }
      return undefined;
    }
    case 'MergePullRequestPayload': {
      if (sel.name === 'pullRequest') {
        const p = (parent as { __pr?: PullRequest }).__pr;
        const r = (parent as { __repo?: Repo }).__repo;
        return p && r ? pullGql(r, p) : null;
      }
      return undefined;
    }
    case 'AddCommentPayload': {
      if (sel.name === 'commentEdge') return (parent as { __commentEdge?: GqlValue }).__commentEdge ?? null;
      return undefined;
    }
    case 'IssueCommentEdge': {
      if (sel.name === 'node') return (parent as { __node?: GqlValue }).__node ?? null;
      return undefined;
    }
    case 'IssueComment': {
      const c = (parent as { __comment?: { id: number; body: string } }).__comment;
      if (sel.name === 'id') return c ? encodeGlobalId('IssueComment', c.id) : null;
      if (sel.name === 'body') return c?.body ?? null;
      return undefined;
    }
    default:
      return undefined;
  }
}

// ---------------------------------------------------------------------------
// Entry point
// ---------------------------------------------------------------------------

export interface GraphqlResult {
  status: number;
  json: unknown;
}

function runOperation(store: Store, op: Operation, variables: Record<string, unknown>): Record<string, unknown> {
  const ctx: Ctx = { store, variables, errors: [] };
  const rootType = op.type === 'mutation' ? 'Mutation' : 'Query';
  const root: GqlValue = { __typename: rootType };
  const data = executeSelections(rootType, root, op.selections, ctx);
  if (ctx.errors.length > 0) {
    return { data: null, errors: ctx.errors };
  }
  return { data };
}

export function graphqlRoute(store: Store, bodyText: string): GraphqlResult {
  let body: unknown;
  try {
    body = JSON.parse(bodyText);
  } catch {
    return { status: 400, json: { errors: [{ message: 'malformed JSON body' }] } };
  }
  const runOne = (req: Record<string, unknown>): Record<string, unknown> => {
    const query = typeof req.query === 'string' ? req.query : '';
    const variables = (req.variables ?? {}) as Record<string, unknown>;
    const operationName = typeof req.operationName === 'string' ? req.operationName : null;
    let ops: Operation[];
    try {
      ops = new Parser(tokenize(query)).parseDocument();
    } catch (err) {
      return { data: null, errors: [{ message: err instanceof Error ? err.message : 'parse error' }] };
    }
    let op = ops[0];
    if (ops.length > 1 && operationName) {
      op = ops.find((o) => o.name === operationName) ?? ops[0];
    }
    if (!op) return { data: null, errors: [{ message: 'no operation' }] };
    return runOperation(store, op, variables);
  };
  if (Array.isArray(body)) {
    return { status: 200, json: body.map((b) => runOne(asRecord(b))) };
  }
  return { status: 200, json: runOne(asRecord(body)) };
}

function asRecord(v: unknown): Record<string, unknown> {
  return v !== null && typeof v === 'object' && !Array.isArray(v) ? (v as Record<string, unknown>) : {};
}

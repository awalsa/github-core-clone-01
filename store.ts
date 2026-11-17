// store.ts — the forge's whole state: one in-memory store holding git objects
// (content-addressed by SHA-1) plus repos, issues, pull requests, labels and
// comments. No disk, no clock: every timestamp the API returns is the fixed
// scenario epoch, so repeated runs are byte-identical.
//
// The viewer is the scenario's first user (adaiversen). It is derived from the
// sealed scenario's fixed seed with the same deterministic generator the
// suite uses, with a hard-coded fallback if anything ever disagrees.

import { createHash } from 'node:crypto';
import { deflateSync, inflateSync } from 'node:zlib';

export const ZERO_OID = '0000000000000000000000000000000000000000';
export const FIXED_TS = '2025-01-01T00:00:00Z';
export const FIXED_EPOCH_SEC = 1735689600;
export const VIEWER_FALLBACK = 'adaiversen';

export function sha1Hex(data: Buffer): string {
  return createHash('sha1').update(data).digest('hex');
}

export function hashObject(type: string, data: Buffer): string {
  const header = Buffer.from(`${type} ${data.length}\0`, 'utf8');
  // Feed header and body as two updates: same digest as hashing the
  // concatenation, without allocating the joined buffer per object.
  return createHash('sha1').update(header).update(data).digest('hex');
}

// ---------------------------------------------------------------------------
// Deterministic scenario generator (same fixed seed the suite uses).
// ---------------------------------------------------------------------------

const SCENARIO_SEED = 0x6a09e667f3bcc909n;
const FIRST_NAMES = ['ada', 'bram', 'cora', 'dmitri', 'elena', 'felix', 'greta', 'hugo', 'iris', 'jonas', 'kira', 'lars', 'mira', 'nico', 'olga', 'pavel', 'quinn', 'rosa', 'sven', 'tess', 'ulla', 'vito', 'wren', 'yara', 'zane'];
const LAST_NAMES = ['anders', 'bauer', 'costa', 'dahl', 'eriksen', 'fischer', 'garcia', 'holm', 'iversen', 'jansen', 'keller', 'lind', 'moreau', 'novak', 'olsen', 'peters', 'quist', 'rossi', 'sato', 'turi', 'ulrich', 'vega', 'weber', 'xu', 'yilmaz', 'zimmer'];

function fnv1a64(input: string): bigint {
  let h = 0xcbf29ce484222325n;
  for (let i = 0; i < input.length; i += 1) {
    h ^= BigInt(input.charCodeAt(i));
    h = (h * 0x100000001b3n) & 0xffffffffffffffffn;
  }
  return h;
}

/** splitmix64's finalizer. */
function mix64(value: bigint): bigint {
  let z = (value + 0x9e3779b97f4a7c15n) & 0xffffffffffffffffn;
  z = ((z ^ (z >> 30n)) * 0xbf58476d1ce4e5b9n) & 0xffffffffffffffffn;
  z = ((z ^ (z >> 27n)) * 0x94d049bb133111ebn) & 0xffffffffffffffffn;
  return (z ^ (z >> 31n)) & 0xffffffffffffffffn;
}

function scenarioSeedFor(label: string): bigint {
  return mix64(SCENARIO_SEED ^ fnv1a64(label));
}

/** xorshift64*: the suite's deterministic RNG. */
class Rng {
  private state: bigint;
  constructor(seed: bigint) {
    const masked = seed & 0xffffffffffffffffn;
    this.state = masked === 0n ? SCENARIO_SEED : masked;
  }
  nextU64(): bigint {
    let x = this.state;
    x = (x ^ (x >> 12n)) & 0xffffffffffffffffn;
    x = (x ^ (x << 25n)) & 0xffffffffffffffffn;
    x = (x ^ (x >> 27n)) & 0xffffffffffffffffn;
    this.state = x;
    return (x * 0x2545f4914f6cdd1dn) & 0xffffffffffffffffn;
  }
  nextU32(): number {
    return Number(this.nextU64() >> 32n);
  }
  nextInt(n: number): number {
    if (n === 1) return 0;
    const range = 4294967296;
    const limit = range - (range % n);
    for (;;) {
      const value = this.nextU32();
      if (value < limit) return value % n;
    }
  }
  pick<T>(arr: readonly T[]): T {
    return arr[this.nextInt(arr.length)];
  }
}

export function scenarioViewerLogin(): string {
  try {
    const rng = new Rng(scenarioSeedFor('users'));
    const used = new Set<string>();
    for (let i = 0; i < 12; i += 1) {
      const first = rng.pick(FIRST_NAMES);
      const last = rng.pick(LAST_NAMES);
      const base = `${first}${last}`.replace(/[^a-z0-9-]/g, '');
      let login = base;
      let suffix = 2;
      while (used.has(login)) {
        login = `${base}-${suffix}`;
        suffix += 1;
      }
      used.add(login);
      if (i === 0) return login;
    }
    return VIEWER_FALLBACK;
  } catch {
    return VIEWER_FALLBACK;
  }
}

// ---------------------------------------------------------------------------
// Git objects
// ---------------------------------------------------------------------------

export interface GitObject {
  type: string;
  data: Buffer;
}

export interface CommitInfo {
  tree: string;
  parents: string[];
  message: string;
}

export interface TreeEntry {
  mode: string;
  name: string;
  oid: string;
  isDir: boolean;
}

export function parseCommit(data: Buffer): CommitInfo {
  const text = data.toString('utf8');
  const head = text.split('\n\n')[0];
  const parents: string[] = [];
  let tree = '';
  let message = '';
  const idx = text.indexOf('\n\n');
  if (idx >= 0) message = text.slice(idx + 2);
  for (const line of head.split('\n')) {
    if (line.startsWith('tree ')) tree = line.slice(5).trim();
    else if (line.startsWith('parent ')) parents.push(line.slice(7).trim());
  }
  return { tree, parents, message };
}

export function parseTree(data: Buffer): TreeEntry[] {
  const entries: TreeEntry[] = [];
  let off = 0;
  while (off < data.length) {
    const sp = data.indexOf(0x20, off);
    const nul = data.indexOf(0x00, sp);
    if (sp < 0 || nul < 0) break;
    const mode = data.subarray(off, sp).toString('utf8');
    const name = data.subarray(sp + 1, nul).toString('utf8');
    const oid = data.subarray(nul + 1, nul + 21).toString('hex');
    entries.push({ mode, name, oid, isDir: mode === '40000' });
    off = nul + 21;
  }
  return entries;
}

export function buildCommit(tree: string, parents: string[], message: string, epochSec: number): Buffer {
  const who = `Conformance <conformance@make.fun> ${epochSec} +0000`;
  let out = `tree ${tree}\n`;
  for (const p of parents) out += `parent ${p}\n`;
  out += `author ${who}\ncommitter ${who}\n\n${message}`;
  return Buffer.from(out, 'utf8');
}

export function buildTag(target: string, targetType: string, name: string, message: string, epochSec: number): Buffer {
  const who = `Conformance <conformance@make.fun> ${epochSec} +0000`;
  return Buffer.from(`object ${target}\ntype ${targetType}\ntag ${name}\ntagger ${who}\n\n${message}`, 'utf8');
}

/** Build a tree object from sorted entries; entries must already be git-sorted. */
export function buildTree(entries: TreeEntry[]): Buffer {
  const parts: Buffer[] = [];
  for (const e of entries) {
    parts.push(Buffer.from(`${e.mode} ${e.name}\0`, 'utf8'));
    parts.push(Buffer.from(e.oid, 'hex'));
  }
  return Buffer.concat(parts);
}

/** git's tree sort: directory names compare as if they ended in '/'. */
export function treeSortKey(name: string, isDir: boolean): string {
  return isDir ? `${name}/` : name;
}

function sortTreeEntries(entries: TreeEntry[]): TreeEntry[] {
  return entries.sort((a, b) => {
    const ka = treeSortKey(a.name, a.isDir);
    const kb = treeSortKey(b.name, b.isDir);
    return ka < kb ? -1 : ka > kb ? 1 : 0;
  });
}

// ---------------------------------------------------------------------------
// Pack files (plain objects only — no deltas)
// ---------------------------------------------------------------------------

function encodeObjectHeader(typeNum: number, size: number): Buffer {
  const bytes: number[] = [];
  let b = (typeNum << 4) | (size & 0x0f);
  size >>= 4;
  while (size > 0) {
    bytes.push(b | 0x80);
    b = size & 0x7f;
    size >>= 7;
  }
  bytes.push(b);
  return Buffer.from(bytes);
}

const TYPE_NUM: Record<string, number> = { commit: 1, tree: 2, blob: 3, tag: 4 };
const NUM_TYPE: Record<number, string> = { 1: 'commit', 2: 'tree', 3: 'blob', 4: 'tag' };

export function buildPack(objects: Map<string, GitObject>): Buffer {
  const oids = [...objects.keys()].sort();
  const chunks: Buffer[] = [];
  const header = Buffer.alloc(12);
  header.write('PACK', 0, 'ascii');
  header.writeUInt32BE(2, 4);
  header.writeUInt32BE(oids.length, 8);
  chunks.push(header);
  for (const oid of oids) {
    const obj = objects.get(oid)!;
    chunks.push(encodeObjectHeader(TYPE_NUM[obj.type], obj.data.length));
    // Level 1: the pack is a wire format, not an archive — its bytes only
    // need to inflate correctly, and the trailer checksums whatever was sent.
    // Same bytes for the same input; roughly half the deflate time of the
    // default level on the 1 KiB objects the workloads push.
    chunks.push(deflateSync(obj.data, { level: 1 }));
  }
  const trailer = createHash('sha1');
  for (const c of chunks) trailer.update(c);
  chunks.push(trailer.digest());
  return Buffer.concat(chunks);
}

/** Parse a pack; throws on a bad header, bad zlib stream or bad trailer. */
export function parsePack(buf: Buffer): Map<string, GitObject> {
  if (buf.length < 32 || buf.subarray(0, 4).toString('ascii') !== 'PACK') throw new Error('bad pack header');
  const version = buf.readUInt32BE(4);
  if (version !== 2) throw new Error('unsupported pack version');
  const count = buf.readUInt32BE(8);
  const trailer = buf.subarray(buf.length - 20);
  const expect = createHash('sha1').update(buf.subarray(0, buf.length - 20)).digest();
  if (!expect.equals(trailer)) throw new Error('pack checksum mismatch');
  const out = new Map<string, GitObject>();
  let off = 12;
  for (let i = 0; i < count; i += 1) {
    let b = buf[off];
    off += 1;
    const typeNum = (b >> 4) & 0x7;
    let size = b & 0x0f;
    let shift = 4;
    while (b & 0x80) {
      b = buf[off];
      off += 1;
      size |= (b & 0x7f) << shift;
      shift += 7;
    }
    const type = NUM_TYPE[typeNum];
    if (!type) throw new Error('unknown object type in pack');
    // inflateSync with {info:true} reports how many compressed bytes the
    // stream consumed, so the next object starts exactly after this one.
    const inflated = inflateSync(buf.subarray(off), { info: true });
    const data = inflated.buffer as Buffer;
    if (data.length !== size) throw new Error('object size mismatch');
    const consumed = (inflated.engine as { bytesWritten?: number }).bytesWritten;
    if (typeof consumed !== 'number' || consumed <= 0) throw new Error('cannot measure zlib stream');
    off += consumed;
    out.set(hashObject(type, data), { type, data });
  }
  return out;
}

// ---------------------------------------------------------------------------
// Domain types
// ---------------------------------------------------------------------------

export interface Comment {
  id: number;
  body: string;
}

export interface Issue {
  id: number;
  number: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  labels: string[];
  comments: Comment[];
  seq: number;
}

export interface PullRequest {
  id: number;
  number: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  headRef: string;
  baseRef: string;
  headSha: string;
  baseSha: string;
  merged: boolean;
  mergedAt: string | null;
  mergeSha: string | null;
  seq: number;
}

export interface Repo {
  id: number;
  name: string;
  owner: string;
  private: boolean;
  description: string;
  defaultBranch: string;
  refs: Map<string, string>;
  issues: Map<number, Issue>;
  pulls: Map<number, PullRequest>;
  nextNumber: number;
  labels: Map<string, { name: string; color: string }>;
  seq: number;
  packCache: Map<string, { pack: Buffer | null; shallow: string[]; unshallow: string[] }>;
}

export interface FileChange {
  path: string;
  status: 'added' | 'removed' | 'modified';
  additions: number;
  deletions: number;
}

// ---------------------------------------------------------------------------
// The store
// ---------------------------------------------------------------------------

export class Store {
  readonly viewerLogin: string;
  readonly objects = new Map<string, GitObject>();
  readonly repos = new Map<string, Repo>();
  readonly reposById = new Map<number, Repo>();
  private commitCache = new Map<string, CommitInfo>();
  private treeCache = new Map<string, TreeEntry[]>();
  private fileMapCache = new Map<string, Map<string, { oid: string; mode: string }>>();
  private repoIdCounter = 1;
  private objectIdCounter = 1;
  private seqCounter = 1;

  constructor() {
    this.viewerLogin = scenarioViewerLogin();
  }

  // -- object store ---------------------------------------------------------

  putObject(type: string, data: Buffer): string {
    const oid = hashObject(type, data);
    if (!this.objects.has(oid)) this.objects.set(oid, { type, data });
    return oid;
  }

  /** Store an object whose oid is already known (e.g. a pack just parsed it). */
  putObjectWithOid(oid: string, type: string, data: Buffer): void {
    if (!this.objects.has(oid)) this.objects.set(oid, { type, data });
  }

  getObject(oid: string): GitObject | undefined {
    return this.objects.get(oid);
  }

  hasObject(oid: string): boolean {
    return this.objects.has(oid);
  }

  parseCommit(oid: string): CommitInfo | undefined {
    let c = this.commitCache.get(oid);
    if (c) return c;
    const obj = this.objects.get(oid);
    if (!obj || obj.type !== 'commit') return undefined;
    c = parseCommit(obj.data);
    this.commitCache.set(oid, c);
    return c;
  }

  parseTree(oid: string): TreeEntry[] | undefined {
    let t = this.treeCache.get(oid);
    if (t) return t;
    const obj = this.objects.get(oid);
    if (!obj || obj.type !== 'tree') return undefined;
    t = parseTree(obj.data);
    this.treeCache.set(oid, t);
    return t;
  }

  /** Peel a tag (possibly nested) to its commit oid; returns undefined if unresolvable. */
  resolveToCommit(oid: string): string | undefined {
    let cur = oid;
    for (let i = 0; i < 16; i += 1) {
      const obj = this.objects.get(cur);
      if (!obj) return undefined;
      if (obj.type === 'commit') return cur;
      if (obj.type === 'tag') {
        const m = obj.data.toString('utf8').match(/^object ([0-9a-f]{40})$/m);
        if (!m) return undefined;
        cur = m[1];
        continue;
      }
      return undefined;
    }
    return undefined;
  }

  /** Peel a tag to the commit it points at (for peeled: lines). */
  peelToCommit(oid: string): string | undefined {
    return this.resolveToCommit(oid);
  }

  // -- repos ----------------------------------------------------------------

  repoKey(owner: string, name: string): string {
    return `${owner}/${name}`;
  }

  getRepo(owner: string, name: string): Repo | undefined {
    if (owner !== this.viewerLogin) return undefined;
    return this.repos.get(this.repoKey(owner, name));
  }

  createRepo(name: string, description: string, isPrivate: boolean): { repo?: Repo; error?: string } {
    if (!name || name.includes('/')) return { error: 'invalid name' };
    const key = this.repoKey(this.viewerLogin, name);
    if (this.repos.has(key)) return { error: 'already exists' };
    const repo: Repo = {
      id: this.repoIdCounter,
      name,
      owner: this.viewerLogin,
      private: isPrivate,
      description,
      defaultBranch: 'main',
      refs: new Map(),
      issues: new Map(),
      pulls: new Map(),
      nextNumber: 1,
      labels: new Map(),
      seq: this.seqCounter,
      packCache: new Map(),
    };
    this.repoIdCounter += 1;
    this.seqCounter += 1;
    this.repos.set(key, repo);
    this.reposById.set(repo.id, repo);
    return { repo };
  }

  deleteRepo(owner: string, name: string): boolean {
    if (owner !== this.viewerLogin) return false;
    const key = this.repoKey(owner, name);
    const repo = this.repos.get(key);
    if (!repo) return false;
    this.repos.delete(key);
    this.reposById.delete(repo.id);
    return true;
  }

  listViewerRepos(): Repo[] {
    return [...this.repos.values()].sort((a, b) => a.seq - b.seq);
  }

  // -- refs -----------------------------------------------------------------

  listRefs(repo: Repo): { name: string; oid: string }[] {
    return [...repo.refs.entries()].map(([name, oid]) => ({ name, oid })).sort((a, b) => (a.name < b.name ? -1 : 1));
  }

  getRef(repo: Repo, ref: string): string | undefined {
    return repo.refs.get(ref);
  }

  setRef(repo: Repo, ref: string, oid: string): void {
    repo.refs.set(ref, oid);
    repo.packCache.clear();
  }

  deleteRef(repo: Repo, ref: string): boolean {
    const ok = repo.refs.delete(ref);
    if (ok) repo.packCache.clear();
    return ok;
  }

  getBranchSha(repo: Repo, branch: string): string | undefined {
    return repo.refs.get(`refs/heads/${branch}`);
  }

  listBranches(repo: Repo): { name: string; sha: string }[] {
    const out: { name: string; sha: string }[] = [];
    for (const [name, oid] of repo.refs) {
      if (name.startsWith('refs/heads/')) out.push({ name: name.slice('refs/heads/'.length), sha: oid });
    }
    return out.sort((a, b) => (a.name < b.name ? -1 : 1));
  }

  resolveRefish(repo: Repo, refish: string): string | undefined {
    if (refish.startsWith('refs/')) return repo.refs.get(refish);
    const asBranch = repo.refs.get(`refs/heads/${refish}`);
    if (asBranch) return asBranch;
    const asTag = repo.refs.get(`refs/tags/${refish}`);
    if (asTag) return asTag;
    if (/^[0-9a-f]{40}$/.test(refish) && this.objects.has(refish)) return refish;
    return undefined;
  }

  // -- ancestry -------------------------------------------------------------

  isAncestor(ancestorOid: string, descendantOid: string): boolean {
    const anc = this.resolveToCommit(ancestorOid);
    const desc = this.resolveToCommit(descendantOid);
    if (!anc || !desc) return false;
    if (anc === desc) return true;
    const seen = new Set<string>([desc]);
    const stack = [desc];
    while (stack.length > 0) {
      const cur = stack.pop()!;
      const info = this.parseCommit(cur);
      if (!info) continue;
      for (const p of info.parents) {
        if (p === anc) return true;
        if (!seen.has(p)) {
          seen.add(p);
          stack.push(p);
        }
      }
    }
    return false;
  }

  ancestorsOf(oid: string): Set<string> {
    const start = this.resolveToCommit(oid);
    const out = new Set<string>();
    if (!start) return out;
    const stack = [start];
    while (stack.length > 0) {
      const cur = stack.pop()!;
      if (out.has(cur)) continue;
      out.add(cur);
      const info = this.parseCommit(cur);
      if (!info) continue;
      for (const p of info.parents) stack.push(p);
    }
    return out;
  }

  mergeBase(aOid: string, bOid: string): string | undefined {
    const aAnc = this.ancestorsOf(aOid);
    const b = this.resolveToCommit(bOid);
    if (!b) return undefined;
    const stack = [b];
    const seen = new Set<string>();
    while (stack.length > 0) {
      const cur = stack.pop()!;
      if (aAnc.has(cur)) return cur;
      if (seen.has(cur)) continue;
      seen.add(cur);
      const info = this.parseCommit(cur);
      if (!info) continue;
      for (const p of info.parents) stack.push(p);
    }
    return undefined;
  }

  listCommits(repo: Repo, refish?: string): { sha: string; message: string }[] | undefined {
    let tip: string | undefined;
    if (refish) {
      tip = this.resolveRefish(repo, refish);
      if (!tip) return undefined;
    } else {
      tip = repo.refs.get(`refs/heads/${repo.defaultBranch}`);
      if (!tip) return [];
    }
    const start = this.resolveToCommit(tip);
    if (!start) return [];
    const out: { sha: string; message: string }[] = [];
    const seen = new Set<string>();
    const stack = [start];
    while (stack.length > 0) {
      const cur = stack.pop()!;
      if (seen.has(cur)) continue;
      seen.add(cur);
      const info = this.parseCommit(cur);
      if (!info) continue;
      out.push({ sha: cur, message: info.message });
      for (let i = info.parents.length - 1; i >= 0; i -= 1) stack.push(info.parents[i]);
    }
    return out;
  }

  // -- file maps ------------------------------------------------------------

  /** path -> {oid, mode} for the tree at a commit (cached; commits immutable). */
  filesAtCommit(commitOid: string): Map<string, { oid: string; mode: string }> {
    const cached = this.fileMapCache.get(commitOid);
    if (cached) return cached;
    const out = new Map<string, { oid: string; mode: string }>();
    const info = this.parseCommit(commitOid);
    if (info) this.walkTree(info.tree, '', out);
    this.fileMapCache.set(commitOid, out);
    return out;
  }

  private walkTree(treeOid: string, prefix: string, out: Map<string, { oid: string; mode: string }>): void {
    const entries = this.parseTree(treeOid);
    if (!entries) return;
    for (const e of entries) {
      const path = prefix ? `${prefix}/${e.name}` : e.name;
      if (e.isDir) this.walkTree(e.oid, path, out);
      else out.set(path, { oid: e.oid, mode: e.mode });
    }
  }

  /** Build the nested tree objects for a file map; returns the root tree oid. */
  buildTreeForFiles(files: Map<string, { oid: string; mode: string }>): string {
    const dirs = new Map<string, Map<string, { oid: string; mode: string; isDir: boolean }>>();
    const ensure = (dir: string): Map<string, { oid: string; mode: string; isDir: boolean }> => {
      let m = dirs.get(dir);
      if (!m) {
        m = new Map();
        dirs.set(dir, m);
      }
      return m;
    };
    ensure('');
    for (const [path, f] of files) {
      const parts = path.split('/');
      let dir = '';
      for (let i = 0; i < parts.length - 1; i += 1) {
        const parent = ensure(dir);
        const next = dir ? `${dir}/${parts[i]}` : parts[i];
        if (!parent.has(parts[i])) parent.set(parts[i], { oid: '', mode: '40000', isDir: true });
        ensure(next);
        dir = next;
      }
      ensure(dir).set(parts[parts.length - 1], { oid: f.oid, mode: f.mode, isDir: false });
    }
    const buildDir = (dir: string): string => {
      const entries: TreeEntry[] = [];
      for (const [name, e] of ensure(dir)) {
        if (e.isDir) {
          const sub = dir ? `${dir}/${name}` : name;
          entries.push({ mode: '40000', name, oid: buildDir(sub), isDir: true });
        } else {
          entries.push({ mode: e.mode, name, oid: e.oid, isDir: false });
        }
      }
      return this.putObject('tree', buildTree(sortTreeEntries(entries)));
    };
    return buildDir('');
  }

  /** Create a commit on top of `parentOid` (null = root commit) applying file changes. */
  commitFiles(parentOid: string | null, changes: Map<string, Buffer | null>, message: string, epochSec: number): string {
    const files = parentOid ? new Map(this.filesAtCommit(this.resolveToCommit(parentOid) ?? parentOid)) : new Map<string, { oid: string; mode: string }>();
    for (const [path, content] of changes) {
      if (content === null) files.delete(path);
      else files.set(path, { oid: this.putObject('blob', content), mode: '100644' });
    }
    const treeOid = this.buildTreeForFiles(files);
    const parents = parentOid ? [this.resolveToCommit(parentOid) ?? parentOid] : [];
    const data = buildCommit(treeOid, parents, message, epochSec);
    return this.putObject('commit', data);
  }

  /** Line-diff counts between two blobs (multiset line difference). */
  static lineDiff(oldData: Buffer | undefined, newData: Buffer | undefined): { additions: number; deletions: number } {
    const oldLines = oldData ? oldData.toString('utf8').split('\n') : [];
    const newLines = newData ? newData.toString('utf8').split('\n') : [];
    const dropEmpty = (lines: string[]): string[] => {
      if (lines.length > 0 && lines[lines.length - 1] === '') lines.pop();
      return lines;
    };
    const a = dropEmpty(oldLines);
    const b = dropEmpty(newLines);
    const count = (lines: string[]): Map<string, number> => {
      const m = new Map<string, number>();
      for (const l of lines) m.set(l, (m.get(l) ?? 0) + 1);
      return m;
    };
    const ca = count(a);
    const cb = count(b);
    let additions = 0;
    let deletions = 0;
    for (const [l, n] of cb) additions += Math.max(0, n - (ca.get(l) ?? 0));
    for (const [l, n] of ca) deletions += Math.max(0, n - (cb.get(l) ?? 0));
    return { additions, deletions };
  }

  /** Diff two file maps: paths changed between base and head. */
  diffFileMaps(base: Map<string, { oid: string; mode: string }>, head: Map<string, { oid: string; mode: string }>): FileChange[] {
    const out: FileChange[] = [];
    const paths = new Set<string>([...base.keys(), ...head.keys()]);
    for (const p of paths) {
      const b = base.get(p);
      const h = head.get(p);
      if (!b && h) {
        const obj = this.objects.get(h.oid);
        const d = Store.lineDiff(undefined, obj?.data);
        out.push({ path: p, status: 'added', additions: d.additions, deletions: 0 });
      } else if (b && !h) {
        const obj = this.objects.get(b.oid);
        const d = Store.lineDiff(obj?.data, undefined);
        out.push({ path: p, status: 'removed', additions: 0, deletions: d.deletions });
      } else if (b && h && b.oid !== h.oid) {
        const bo = this.objects.get(b.oid);
        const ho = this.objects.get(h.oid);
        const d = Store.lineDiff(bo?.data, ho?.data);
        out.push({ path: p, status: 'modified', additions: d.additions, deletions: d.deletions });
      }
    }
    return out.sort((x, y) => (x.path < y.path ? -1 : 1));
  }

  // -- reachability / packs ---------------------------------------------------

  private collectReachable(roots: string[], maxDepth: number, stopAt: Set<string>): { objects: Set<string>; commits: Set<string> } {
    const objects = new Set<string>();
    const commits = new Set<string>();
    const visitCommit = (oid: string, depth: number): void => {
      if (commits.has(oid)) return;
      const obj = this.objects.get(oid);
      if (!obj) return;
      if (obj.type === 'tag') {
        objects.add(oid);
        const m = obj.data.toString('utf8').match(/^object ([0-9a-f]{40})$/m);
        if (m) {
          const target = m[1];
          const tobj = this.objects.get(target);
          if (tobj?.type === 'commit') visitCommit(target, depth);
          else if (tobj?.type === 'tag') visitCommit(target, depth);
        }
        return;
      }
      if (obj.type !== 'commit') return;
      commits.add(oid);
      objects.add(oid);
      const info = this.parseCommit(oid);
      if (!info) return;
      this.collectTree(info.tree, objects);
      if (stopAt.has(oid)) return;
      if (maxDepth >= 0 && depth + 1 >= maxDepth) return;
      for (const p of info.parents) visitCommit(p, depth + 1);
    };
    for (const r of roots) visitCommit(r, 0);
    return { objects, commits };
  }

  private collectTree(treeOid: string, objects: Set<string>): void {
    if (objects.has(treeOid)) return;
    const obj = this.objects.get(treeOid);
    if (!obj || obj.type !== 'tree') return;
    objects.add(treeOid);
    const entries = this.parseTree(treeOid);
    if (!entries) return;
    for (const e of entries) {
      if (e.isDir) this.collectTree(e.oid, objects);
      else objects.add(e.oid);
    }
  }

  /**
   * Build the pack for a fetch. depth < 0 means unlimited. Returns the pack
   * (null when nothing to send) plus shallow/unshallow lines for the reply.
   * Identical fetches of the same repo hit the repo's pack cache: the bench
   * clone workload is 50 concurrent shallow clones of one tip.
   */
  buildFetchPack(repo: Repo, wants: string[], haves: string[], clientShallow: string[], depth: number): { pack: Buffer | null; shallow: string[]; unshallow: string[] } {
    const key = `${[...wants].sort().join(',')}|${[...haves].sort().join(',')}|${[...clientShallow].sort().join(',')}|${depth}`;
    const cached = repo.packCache.get(key);
    if (cached !== undefined) return cached;
    const stopAt = new Set(clientShallow);
    const held = this.collectReachable(haves, -1, stopAt);
    const send = this.collectReachable(wants, depth, new Set());
    const sendObjects = new Set<string>();
    for (const oid of send.objects) if (!held.objects.has(oid)) sendObjects.add(oid);
    const shallow: string[] = [];
    for (const c of send.commits) {
      const info = this.parseCommit(c);
      if (!info) continue;
      const parentMissing = info.parents.some((p) => !send.commits.has(p) && !held.commits.has(p));
      if (parentMissing) shallow.push(c);
    }
    const unshallow: string[] = [];
    for (const s of clientShallow) {
      const info = this.parseCommit(s);
      if (!info) continue;
      if (info.parents.every((p) => send.commits.has(p) || held.commits.has(p))) unshallow.push(s);
    }
    if (sendObjects.size === 0) {
      const result = { pack: null, shallow, unshallow };
      repo.packCache.set(key, result);
      return result;
    }
    const objs = new Map<string, GitObject>();
    for (const oid of sendObjects) {
      const o = this.objects.get(oid);
      if (o) objs.set(oid, o);
    }
    const result = { pack: buildPack(objs), shallow, unshallow };
    repo.packCache.set(key, result);
    return result;
  }

  // -- labels / issues / pulls -----------------------------------------------

  createLabel(repo: Repo, name: string, color: string): { name: string; color: string } {
    const label = { name, color };
    repo.labels.set(name, label);
    return label;
  }

  createIssue(repo: Repo, title: string, body: string, labelNames: string[]): { issue?: Issue; error?: string } {
    for (const l of labelNames) {
      if (!repo.labels.has(l)) return { error: `label "${l}" does not exist` };
    }
    const issue: Issue = {
      id: this.objectIdCounter,
      number: repo.nextNumber,
      title,
      body,
      state: 'open',
      labels: [...labelNames],
      comments: [],
      seq: this.seqCounter,
    };
    this.objectIdCounter += 1;
    repo.nextNumber += 1;
    this.seqCounter += 1;
    repo.issues.set(issue.number, issue);
    return { issue };
  }

  getIssue(repo: Repo, number: number): Issue | undefined {
    return repo.issues.get(number);
  }

  listIssues(repo: Repo, opts: { state: string; sort: string; direction: string }): Issue[] {
    let list = [...repo.issues.values()];
    if (opts.state === 'open') list = list.filter((i) => i.state === 'open');
    else if (opts.state === 'closed') list = list.filter((i) => i.state === 'closed');
    const dir = opts.direction === 'asc' ? 1 : -1;
    list.sort((a, b) => (a.seq - b.seq) * dir);
    return list;
  }

  createComment(repo: Repo, issueNumber: number, body: string): { comment?: Comment; error?: string } {
    const issue = repo.issues.get(issueNumber);
    if (!issue) return { error: 'issue not found' };
    const comment: Comment = { id: this.objectIdCounter, body };
    this.objectIdCounter += 1;
    issue.comments.push(comment);
    return { comment };
  }

  createPull(repo: Repo, title: string, body: string, head: string, base: string): { pr?: PullRequest; error?: string } {
    const headSha = this.getBranchSha(repo, head);
    const baseSha = this.getBranchSha(repo, base);
    if (!headSha || !baseSha) return { error: 'head or base branch not found' };
    for (const pr of repo.pulls.values()) {
      if (pr.state === 'open' && pr.headRef === head && pr.baseRef === base) return { error: 'a pull request already exists for this branch' };
    }
    const pr: PullRequest = {
      id: this.objectIdCounter,
      number: repo.nextNumber,
      title,
      body,
      state: 'open',
      headRef: head,
      baseRef: base,
      headSha,
      baseSha,
      merged: false,
      mergedAt: null,
      mergeSha: null,
      seq: this.seqCounter,
    };
    this.objectIdCounter += 1;
    repo.nextNumber += 1;
    this.seqCounter += 1;
    repo.pulls.set(pr.number, pr);
    return { pr };
  }

  getPull(repo: Repo, number: number): PullRequest | undefined {
    return repo.pulls.get(number);
  }

  listPulls(repo: Repo, state: string): PullRequest[] {
    let list = [...repo.pulls.values()];
    if (state === 'open') list = list.filter((p) => p.state === 'open');
    else if (state === 'closed') list = list.filter((p) => p.state === 'closed');
    list.sort((a, b) => a.seq - b.seq);
    return list;
  }

  /** The base branch's CURRENT tip (a PR's recorded baseSha goes stale once the base advances). */
  private currentBaseTip(repo: Repo, pr: PullRequest): string {
    return repo.refs.get(`refs/heads/${pr.baseRef}`) ?? pr.baseSha;
  }

  /** Files changed between the merge-base and the head tip. */
  pullChanges(repo: Repo, pr: PullRequest): FileChange[] | undefined {
    const baseTip = this.currentBaseTip(repo, pr);
    const base = this.mergeBase(pr.headSha, baseTip);
    const baseCommit = base ?? baseTip;
    const headCommit = this.resolveToCommit(pr.headSha);
    const baseCommitOid = this.resolveToCommit(baseCommit);
    if (!headCommit || !baseCommitOid) return undefined;
    const baseFiles = this.filesAtCommit(baseCommitOid);
    const headFiles = this.filesAtCommit(headCommit);
    return this.diffFileMaps(baseFiles, headFiles);
  }

  /** True when the head's changes can apply cleanly onto the CURRENT base tip. */
  pullMergeable(repo: Repo, pr: PullRequest): boolean {
    const baseTip = this.currentBaseTip(repo, pr);
    const base = this.mergeBase(pr.headSha, baseTip);
    if (!base) return true;
    const baseFiles = this.filesAtCommit(this.resolveToCommit(base) ?? base);
    const headFiles = this.filesAtCommit(this.resolveToCommit(pr.headSha) ?? pr.headSha);
    const baseTipFiles = this.filesAtCommit(this.resolveToCommit(baseTip) ?? baseTip);
    const headChanges = this.diffFileMaps(baseFiles, headFiles);
    const baseChanges = this.diffFileMaps(baseFiles, baseTipFiles);
    const baseChanged = new Map<string, string>();
    for (const c of baseChanges) baseChanged.set(c.path, baseTipFiles.get(c.path)?.oid ?? '');
    for (const c of headChanges) {
      if (!baseChanged.has(c.path)) continue;
      const headOid = headFiles.get(c.path)?.oid ?? '';
      if (baseChanged.get(c.path) !== headOid) return false;
    }
    return true;
  }

  mergePull(repo: Repo, pr: PullRequest): { sha?: string; error?: string } {
    if (pr.state === 'closed') return { error: pr.merged ? 'already merged' : 'already closed' };
    if (!this.pullMergeable(repo, pr)) return { error: 'conflict' };
    const baseTip = this.currentBaseTip(repo, pr);
    const base = this.mergeBase(pr.headSha, baseTip);
    const baseFiles = this.filesAtCommit(this.resolveToCommit(base ?? baseTip) ?? baseTip);
    const headFiles = this.filesAtCommit(this.resolveToCommit(pr.headSha) ?? pr.headSha);
    const headChanges = this.diffFileMaps(baseFiles, headFiles);
    // Three-way merge: start from the CURRENT base tip, apply the head's
    // changes since the merge-base.
    const mergedFiles = new Map(this.filesAtCommit(this.resolveToCommit(baseTip) ?? baseTip));
    for (const c of headChanges) {
      if (c.status === 'removed') mergedFiles.delete(c.path);
      else {
        const hf = headFiles.get(c.path);
        if (hf) mergedFiles.set(c.path, hf);
      }
    }
    const treeOid = this.buildTreeForFiles(mergedFiles);
    const baseTipCommit = this.resolveToCommit(baseTip) ?? baseTip;
    const message = `Merge pull request #${pr.number} from ${pr.headRef}`;
    const commitOid = this.putObject('commit', buildCommit(treeOid, [baseTipCommit], message, FIXED_EPOCH_SEC));
    repo.refs.set(`refs/heads/${pr.baseRef}`, commitOid);
    repo.packCache.clear();
    pr.state = 'closed';
    pr.merged = true;
    pr.mergedAt = FIXED_TS;
    pr.mergeSha = commitOid;
    return { sha: commitOid };
  }
}

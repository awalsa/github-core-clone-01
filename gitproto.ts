// gitproto.ts — git smart HTTP: info/refs advertisements, upload-pack (v2
// and v0) and receive-pack (v0). Packs carry plain objects only, never deltas.
// All framing follows the pkt-line format git itself uses.

import { createHash } from 'node:crypto';
import { clearRefCaches, parsePack, ZERO_OID, type Repo, type Store } from './store.ts';

export const UPLOAD_AD_CT = 'application/x-git-upload-pack-advertisement';
export const RECEIVE_AD_CT = 'application/x-git-receive-pack-advertisement';
export const UPLOAD_RESULT_CT = 'application/x-git-upload-pack-result';
export const RECEIVE_RESULT_CT = 'application/x-git-receive-pack-result';

/** Encode one pkt-line (payload includes its trailing newline when present). */
export function pktLine(payload: Buffer | string): Buffer {
  const data = typeof payload === 'string' ? Buffer.from(payload, 'utf8') : payload;
  const len = data.length + 4;
  const head = Buffer.alloc(4);
  head.write(len.toString(16).padStart(4, '0'), 'ascii');
  return Buffer.concat([head, data]);
}

export const FLUSH = Buffer.from('0000', 'ascii');
export const DELIM = Buffer.from('0001', 'ascii');

export function encodePktLines(lines: (Buffer | string)[]): Buffer {
  return Buffer.concat(lines.map((l) => pktLine(l)));
}

/** Decode a pkt-line stream into packets: {type:'data', data} | flush | delim. */
export type Pkt = { kind: 'data'; data: Buffer } | { kind: 'flush' } | { kind: 'delim' };

export function decodePktLines(buf: Buffer): Pkt[] {
  const out: Pkt[] = [];
  let off = 0;
  while (off + 4 <= buf.length) {
    const len = parseInt(buf.subarray(off, off + 4).toString('ascii'), 16);
    if (Number.isNaN(len)) throw new Error('not a pkt-line');
    if (len === 0) {
      out.push({ kind: 'flush' });
      off += 4;
      continue;
    }
    if (len === 1) {
      out.push({ kind: 'delim' });
      off += 4;
      continue;
    }
    if (len < 4 || off + len > buf.length) throw new Error('pkt-line length out of range');
    out.push({ kind: 'data', data: buf.subarray(off + 4, off + len) });
    off += len;
  }
  return out;
}

/** Split a byte buffer into sideband packets (band 1) of at most 65515 bytes. */
export function sidebandPackets(data: Buffer): Buffer {
  const chunks: Buffer[] = [];
  const MAX = 65515;
  for (let off = 0; off < data.length; off += MAX) {
    const slice = data.subarray(off, Math.min(off + MAX, data.length));
    const payload = Buffer.concat([Buffer.from([1]), slice]);
    chunks.push(pktLine(payload));
  }
  chunks.push(FLUSH);
  return Buffer.concat(chunks);
}

function advertisement(store: Store, repo: Repo, service: 'upload' | 'receive'): Buffer {
  const isUpload = service === 'upload';
  const ctService = isUpload ? 'git-upload-pack' : 'git-receive-pack';
  const caps = isUpload
    ? 'multi_ack thin-pack side-band side-band-64k ofs-delta shallow deepen-since deepen-not deepen-relative no-progress include-tag multi_ack_detailed no-done symref=HEAD:refs/heads/main'
    : 'report-status delete-refs side-band-64k ofs-delta object-format=sha1';
  const refs = store.listRefs(repo);
  // The "# service=" line is its own pkt-line stream, terminated by a flush.
  const lines: Buffer[] = [pktLine(`# service=${ctService}\n`), FLUSH];
  if (refs.length === 0) {
    lines.push(pktLine(`${ZERO_OID} capabilities^{}\0${caps}\n`));
  } else if (isUpload) {
    const head = repo.refs.get('refs/heads/main') ?? refs[0].oid;
    lines.push(pktLine(`${head} HEAD\0${caps}\n`));
    for (const r of refs) {
      lines.push(pktLine(`${r.oid} ${r.name}\n`));
      const obj = store.getObject(r.oid);
      if (obj?.type === 'tag') {
        const peeled = store.peelToCommit(r.oid);
        if (peeled) lines.push(pktLine(`${peeled} ${r.name}^{}\n`));
      }
    }
  } else {
    // receive-pack: the capabilities ride on the first ref line, no HEAD line.
    let first = true;
    for (const r of refs) {
      if (first) {
        lines.push(pktLine(`${r.oid} ${r.name}\0${caps}\n`));
        first = false;
      } else {
        lines.push(pktLine(`${r.oid} ${r.name}\n`));
      }
      const obj = store.getObject(r.oid);
      if (obj?.type === 'tag') {
        const peeled = store.peelToCommit(r.oid);
        if (peeled) lines.push(pktLine(`${peeled} ${r.name}^{}\n`));
      }
    }
  }
  lines.push(FLUSH);
  return Buffer.concat(lines);
}

/** The v2 advertisement bytes never depend on the repo: build them once, reuse forever. */
let V2_ADVERTISEMENT: Buffer | null = null;

function v2Advertisement(service: 'upload' | 'receive'): Buffer {
  // Protocol v2 advertisements start directly with "version 2": real
  // git-http-backend emits no "# service=" preamble for v2.
  void service;
  if (V2_ADVERTISEMENT === null) {
    const caps = ['ls-refs=unborn', 'fetch=shallow wait-for-done', 'object-format=sha1', 'ofs-delta'];
    const lines: Buffer[] = [pktLine('version 2\n')];
    for (const c of caps) lines.push(pktLine(`${c}\n`));
    lines.push(FLUSH);
    V2_ADVERTISEMENT = Buffer.concat(lines);
  }
  return V2_ADVERTISEMENT;
}

/** A valid pack with zero objects (used when a fetch has nothing to send). */
function emptyPack(): Buffer {
  const header = Buffer.alloc(12);
  header.write('PACK', 0, 'ascii');
  header.writeUInt32BE(2, 4);
  header.writeUInt32BE(0, 8);
  return Buffer.concat([header, createHash('sha1').update(header).digest()]);
}

export interface GitResult {
  status: number;
  contentType: string;
  body: Buffer;
}

function notFound(): GitResult {
  return { status: 404, contentType: 'application/json; charset=utf-8', body: Buffer.from(JSON.stringify({ message: 'Not Found' }) + '\n', 'utf8') };
}

export function gitInfoRefs(store: Store, owner: string, repoName: string, service: string | undefined, protocolVersion: string | undefined): GitResult {
  const repo = store.getRepo(owner, repoName);
  if (!repo) return notFound();
  if (service !== 'git-upload-pack' && service !== 'git-receive-pack') {
    return { status: 400, contentType: 'application/json; charset=utf-8', body: Buffer.from(JSON.stringify({ message: 'bad service' }) + '\n', 'utf8') };
  }
  const kind = service === 'git-upload-pack' ? 'upload' : 'receive';
  if (protocolVersion === '2') {
    return { status: 200, contentType: kind === 'upload' ? UPLOAD_AD_CT : RECEIVE_AD_CT, body: v2Advertisement(kind) };
  }
  // NOCACHE VARIANT: the v0 advertisement is rebuilt every time (the
  // fd45ff1 adCache is disabled for this experiment).
  return { status: 200, contentType: kind === 'upload' ? UPLOAD_AD_CT : RECEIVE_AD_CT, body: advertisement(store, repo, kind) };
}

// ---------------------------------------------------------------------------
// upload-pack
// ---------------------------------------------------------------------------

interface FetchRequest {
  command: 'ls-refs' | 'fetch';
  wants: string[];
  haves: string[];
  shallow: string[];
  depth: number;
  prefixes: string[];
  peel: boolean;
  symrefs: boolean;
  unborn: boolean;
}

function parseUploadPackRequest(buf: Buffer): FetchRequest | undefined {
  let pkts: Pkt[];
  try {
    pkts = decodePktLines(buf);
  } catch {
    return undefined;
  }
  const req: FetchRequest = { command: 'fetch', wants: [], haves: [], shallow: [], depth: -1, prefixes: [], peel: false, symrefs: false, unborn: false };
  let seenCommand = false;
  // A v0 fetch has no command line: it starts directly with a "want" line.
  const firstPkt = pkts[0];
  if (firstPkt?.kind === 'data' && firstPkt.data.toString('utf8').trim().startsWith('want ')) {
    seenCommand = true;
  }
  let inArgs = false;
  for (const p of pkts) {
    if (p.kind === 'flush') {
      // v0 fetch is two sections (wants, then haves+done) separated by a
      // flush: keep reading until 'done' or the end of the stream.
      if (req.command === 'fetch' && seenCommand) continue;
      break;
    }
    if (p.kind === 'delim') {
      inArgs = true;
      continue;
    }
    const line = p.data.toString('utf8').trim();
    if (!seenCommand) {
      if (line === 'command=ls-refs') req.command = 'ls-refs';
      else if (line === 'command=fetch') req.command = 'fetch';
      else return undefined;
      seenCommand = true;
      continue;
    }
    if (req.command === 'ls-refs') {
      if (line === 'peel') req.peel = true;
      else if (line === 'symrefs') req.symrefs = true;
      else if (line === 'unborn') req.unborn = true;
      else if (line.startsWith('ref-prefix ')) req.prefixes.push(line.slice('ref-prefix '.length));
    } else {
      // want/have/shallow lines carry only the oid as the first token; a v0
      // want line may also carry a capability list after the oid.
      const firstToken = (keyword: string): string | undefined => {
        if (!line.startsWith(keyword)) return undefined;
        return line.slice(keyword.length).trim().split(/\s+/)[0];
      };
      const want = firstToken('want ');
      if (want) req.wants.push(want);
      const have = firstToken('have ');
      if (have) req.haves.push(have);
      const shallow = firstToken('shallow ');
      if (shallow) req.shallow.push(shallow);
      if (line.startsWith('deepen ')) {
        const n = parseInt(line.slice(7).trim(), 10);
        if (!Number.isNaN(n) && n > 0) req.depth = n;
      } else if (line === 'done') break;
    }
    void inArgs;
  }
  if (!seenCommand) return undefined;
  return req;
}

function lsRefsResponse(store: Store, repo: Repo, req: FetchRequest): Buffer {
  // NOCACHE VARIANT: the fd45ff1 lsRefsCache is disabled for this experiment.
  const lines: Buffer[] = [];
  const refs = store.listRefs(repo);
  const headOid = repo.refs.get('refs/heads/main');
  const matches = (name: string): boolean => req.prefixes.length === 0 || req.prefixes.some((p) => name.startsWith(p));
  if (req.prefixes.length === 0 || req.prefixes.some((p) => 'HEAD'.startsWith(p))) {
    if (headOid) {
      lines.push(pktLine(`${headOid} HEAD symref-target:refs/heads/main\n`));
    } else if (req.unborn) {
      lines.push(pktLine('unborn HEAD symref-target:refs/heads/main\n'));
    }
  }
  for (const r of refs) {
    if (!matches(r.name)) continue;
    let line = `${r.oid} ${r.name}`;
    const obj = store.getObject(r.oid);
    if (obj?.type === 'tag') {
      const peeled = store.peelToCommit(r.oid);
      if (peeled) line += ` peeled:${peeled}`;
    }
    lines.push(pktLine(`${line}\n`));
  }
  lines.push(FLUSH);
  return Buffer.concat(lines);
}

// NOCACHE VARIANT: the fd45ff1 whole-fetch-reply cache is disabled for
// this experiment; fetch replies are always built fresh (the packCache
// inside buildFetchPack is untouched).
function fetchResponseDirect(store: Store, repo: Repo, req: FetchRequest, isV2: boolean): Buffer {
  return isV2 ? fetchResponseV2(store, repo, req) : fetchResponseV0(store, repo, req);
}

function fetchResponseV2(store: Store, repo: Repo, req: FetchRequest): Buffer {
  const { pack, shallow, unshallow } = store.buildFetchPack(repo, req.wants, req.haves, req.shallow, req.depth);
  // A stateless v2 fetch (wants + done in one request) carries no
  // acknowledgments section: the reply starts with shallow-info (when there
  // is one) and then the packfile section.
  const parts: Buffer[] = [];
  if (shallow.length > 0 || unshallow.length > 0) {
    parts.push(pktLine('shallow-info\n'));
    for (const s of shallow) parts.push(pktLine(`shallow ${s}\n`));
    for (const s of unshallow) parts.push(pktLine(`unshallow ${s}\n`));
    parts.push(DELIM);
  }
  if (pack) {
    parts.push(pktLine('packfile\n'));
    parts.push(sidebandPackets(pack));
  } else if (req.wants.length > 0) {
    // Nothing new to send, but the client asked for objects: answer with a
    // packfile section holding an empty pack, as real upload-pack does.
    parts.push(pktLine('packfile\n'));
    parts.push(sidebandPackets(emptyPack()));
  } else {
    // No refs at all (empty repo): no packfile section.
    parts.push(FLUSH);
  }
  return Buffer.concat(parts);
}

function fetchResponseV0(store: Store, repo: Repo, req: FetchRequest): Buffer {
  const { pack, shallow, unshallow } = store.buildFetchPack(repo, req.wants, req.haves, req.shallow, req.depth);
  const parts: Buffer[] = [];
  // ACK the last held have when the client offered one we have; else NAK.
  const heldHave = [...req.haves].reverse().find((h) => store.hasObject(h));
  if (heldHave) parts.push(pktLine(`ACK ${heldHave}\n`));
  else parts.push(pktLine('NAK\n'));
  for (const s of shallow) parts.push(pktLine(`shallow ${s}\n`));
  for (const s of unshallow) parts.push(pktLine(`unshallow ${s}\n`));
  if (pack) parts.push(sidebandPackets(pack));
  else if (req.wants.length > 0) parts.push(sidebandPackets(emptyPack()));
  else parts.push(FLUSH);
  return Buffer.concat(parts);
}

export function gitUploadPack(store: Store, owner: string, repoName: string, body: Buffer, protocolVersion: string | undefined): GitResult {
  const repo = store.getRepo(owner, repoName);
  if (!repo) return notFound();
  const req = parseUploadPackRequest(body);
  if (!req) {
    return { status: 400, contentType: 'application/json; charset=utf-8', body: Buffer.from(JSON.stringify({ message: 'malformed git request' }) + '\n', 'utf8') };
  }
  if (req.command === 'ls-refs') {
    return { status: 200, contentType: UPLOAD_RESULT_CT, body: lsRefsResponse(store, repo, req) };
  }
  const isV2 = protocolVersion === '2';
  const out = fetchResponseDirect(store, repo, req, isV2);
  return { status: 200, contentType: UPLOAD_RESULT_CT, body: out };
}

// ---------------------------------------------------------------------------
// receive-pack
// ---------------------------------------------------------------------------

interface PushCommand {
  oldOid: string;
  newOid: string;
  ref: string;
}

function parseReceivePack(body: Buffer): { commands: PushCommand[]; packStart: number } | undefined {
  let off = 0;
  const commands: PushCommand[] = [];
  while (off + 4 <= body.length) {
    const lenStr = body.subarray(off, off + 4).toString('ascii');
    const len = parseInt(lenStr, 16);
    if (Number.isNaN(len)) return undefined;
    if (len === 0) {
      off += 4;
      break;
    }
    if (len < 4 || off + len > body.length) return undefined;
    const line = body.subarray(off + 4, off + len).toString('utf8');
    const nul = line.indexOf('\0');
    const clean = (nul >= 0 ? line.slice(0, nul) : line).trim();
    const parts = clean.split(' ');
    if (parts.length !== 3) return undefined;
    commands.push({ oldOid: parts[0], newOid: parts[1], ref: parts[2] });
    off += len;
  }
  return { commands, packStart: off };
}

export function gitReceivePack(store: Store, owner: string, repoName: string, body: Buffer): GitResult {
  const repo = store.getRepo(owner, repoName);
  if (!repo) return notFound();
  const parsed = parseReceivePack(body);
  if (!parsed) {
    return { status: 400, contentType: 'application/json; charset=utf-8', body: Buffer.from(JSON.stringify({ message: 'malformed git request' }) + '\n', 'utf8') };
  }
  const { commands, packStart } = parsed;
  const packBytes = body.subarray(packStart);
  let unpackOk = true;
  let unpackError = '';
  if (packBytes.length > 0) {
    try {
      const objs = parsePack(packBytes);
      // parsePack already computed each oid; store them directly instead of
      // re-hashing every object in putObject.
      for (const [oid, obj] of objs) {
        if (!store.hasObject(oid)) store.putObjectWithOid(oid, obj.type, obj.data);
      }
    } catch (err) {
      unpackOk = false;
      unpackError = err instanceof Error ? err.message : 'pack parse failed';
    }
  }
  const report: string[] = [unpackOk ? 'unpack ok' : `unpack ${unpackError || 'failed'}`];
  for (const cmd of commands) {
    if (!unpackOk) {
      report.push(`ng ${cmd.ref} unpack failed`);
      continue;
    }
    const current = repo.refs.get(cmd.ref);
    if (cmd.newOid === ZERO_OID) {
      // delete
      if (!current) {
        report.push(`ng ${cmd.ref} ref does not exist`);
      } else if (cmd.oldOid !== ZERO_OID && cmd.oldOid !== current) {
        report.push(`ng ${cmd.ref} non-fast-forward`);
      } else {
        repo.refs.delete(cmd.ref);
        clearRefCaches(repo);
        report.push(`ok ${cmd.ref}`);
      }
      continue;
    }
    if (cmd.oldOid === ZERO_OID) {
      // create
      if (current) {
        report.push(`ng ${cmd.ref} already exists`);
      } else if (!store.hasObject(cmd.newOid)) {
        report.push(`ng ${cmd.ref} object not found`);
      } else {
        repo.refs.set(cmd.ref, cmd.newOid);
        clearRefCaches(repo);
        report.push(`ok ${cmd.ref}`);
      }
      continue;
    }
    // update
    if (!current) {
      report.push(`ng ${cmd.ref} ref does not exist`);
    } else if (current !== cmd.oldOid) {
      report.push(`ng ${cmd.ref} non-fast-forward`);
    } else if (!store.hasObject(cmd.newOid)) {
      report.push(`ng ${cmd.ref} object not found`);
    } else if (!store.isAncestor(current, cmd.newOid)) {
      report.push(`ng ${cmd.ref} non-fast-forward`);
    } else {
      repo.refs.set(cmd.ref, cmd.newOid);
      clearRefCaches(repo);
      report.push(`ok ${cmd.ref}`);
    }
  }
  const reportStream = Buffer.concat([encodePktLines(report.map((l) => `${l}\n`)), FLUSH]);
  return { status: 200, contentType: RECEIVE_RESULT_CT, body: sidebandPackets(reportStream) };
}

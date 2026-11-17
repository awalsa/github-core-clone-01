// main.ts — the process lifecycle for the forge.
//
// The harness (env) owns the trial: it hands this process an origin through
// MAKEFUN_ENV_URL, and this file's only job is to (a) start the Go forge
// binary on a kernel-chosen port, (b) tell the env that port, and (c) stay
// alive until the env reports the trial finished, then shut down cleanly.
// The forge itself lives in ./go/forge (a statically linked binary the
// rules allow as vendored source); this file only supervises it.
//
// Order of operations:
//   1. Read MAKEFUN_ENV_URL. Missing means there is no env at all: bail out
//      with exit code 1 instead of hanging.
//   2. Spawn ./go/forge. It binds 127.0.0.1:0 and prints PORT=<n> on its
//      stdout; the kernel picks the port, the env's own port (8080 in a
//      trial container) is never touched.
//   3. POST { port } to <env>/register and retry until it lands. A 200 means
//      the env took it; a 409 means some entry already claimed the slot, in
//      which case waiting is still correct. 400/405 are contract mistakes and
//      abort the run; anything else (env still booting, a timeout) retries.
//   4. Poll <env>/done until it answers {"done": true}. The env responds
//      immediately either way, so this is a plain poll with a pause between
//      tries that doubles up to a cap.
//   5. Stop the forge child and exit 0. The harness stops the env afterwards
//      and the scorer reads the export it writes.

import { spawn, type ChildProcess } from "node:child_process";
import { appendFileSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

/** Temporary debug log (removed before any submit). */
function dbg(line: string): void {
  try {
    appendFileSync("/tmp/gocore-entry-debug.log", `${new Date().toISOString()} ${line}\n`);
  } catch {
    // Debugging must never break the entry.
  }
}

/** One HTTP round trip may take at most this long before it is dropped. */
const REQUEST_BUDGET_MS = 30_000;
/** Pause after the first failed attempt, and the longest pause allowed. */
const FIRST_PAUSE_MS = 250;
const LONGEST_PAUSE_MS = 2_000;

const sleep = (ms: number): Promise<void> =>
  new Promise((resolve) => setTimeout(resolve, ms));

/** Next pause after a failed attempt: double the last one, up to the cap. */
function growPause(last: number): number {
  return Math.min(LONGEST_PAUSE_MS, last * 2);
}

const log = (line: string): void => {
  process.stdout.write(`[entry] ${line}\n`);
};

const complain = (line: string): void => {
  process.stderr.write(`[entry] ${line}\n`);
};

const envOrigin = (process.env.MAKEFUN_ENV_URL ?? "").replace(/\/+$/, "");
if (envOrigin === "") {
  complain("MAKEFUN_ENV_URL is missing, so there is no env to register with");
  process.exit(1);
}

const forgePath = path.join(
  path.dirname(fileURLToPath(import.meta.url)),
  "go",
  "forge",
);

/**
 * Start the forge binary and wait for it to print its port. The child's
 * stdin stays open (its dead-man's switch: it exits when stdin closes);
 * its stderr is inherited so forge logs reach the trial log.
 */
function startForge(): Promise<{ child: ChildProcess; port: number }> {
  return new Promise((resolve, reject) => {
    const child = spawn(forgePath, [], {
      stdio: ["pipe", "pipe", "inherit"],
    });
    let settled = false;
    let buffered = "";
    const fail = (err: Error): void => {
      if (settled) return;
      settled = true;
      child.kill("SIGKILL");
      reject(err);
    };
    child.once("error", (err) => fail(err));
    child.once("exit", (code) => {
      if (!settled) fail(new Error(`the forge binary exited early with code ${code}`));
    });
    child.stdout?.on("data", (chunk: Buffer) => {
      if (settled) return;
      buffered += chunk.toString("utf8");
      const match = buffered.match(/PORT=(\d+)/);
      if (match) {
        settled = true;
        resolve({ child, port: parseInt(match[1], 10) });
      }
    });
  });
}

/**
 * Tell the env which port the forge bound. Returns once the env has the
 * number (200) or another entry already holds the slot (409). Contract
 * errors (400/405) abort the run; everything else is retried, since the env
 * may still be starting.
 */
async function announce(port: number): Promise<void> {
  let pause = FIRST_PAUSE_MS;
  for (;;) {
    try {
      const answer: Awaited<ReturnType<typeof fetch>> = await fetch(`${envOrigin}/register`, {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({ port }),
        signal: AbortSignal.timeout(REQUEST_BUDGET_MS),
      });
      if (answer.status === 200 || answer.status === 409) return;
      if (answer.status === 400 || answer.status === 405) {
        const detail = await answer.text();
        throw new Error(`register refused with HTTP ${answer.status}: ${detail}`);
      }
    } catch (err) {
      if (err instanceof Error && err.message.startsWith("register refused with HTTP ")) throw err;
    }
    await sleep(pause);
    pause = growPause(pause);
  }
}

/** Poll the env until it says the trial is over; never return sooner. */
async function waitUntilDone(): Promise<void> {
  let pause = FIRST_PAUSE_MS;
  for (;;) {
    try {
      const answer = await fetch(`${envOrigin}/done`, {
        signal: AbortSignal.timeout(REQUEST_BUDGET_MS),
      });
      dbg(`done poll status=${answer.status}`);
      if (answer.ok) {
        const payload = (await answer.json()) as { done?: unknown };
        dbg(`done poll payload=${JSON.stringify(payload)}`);
        if (payload.done === true) return;
      }
    } catch {
      // The env is still starting, or the poll timed out: not done yet.
    }
    await sleep(pause);
    pause = growPause(pause);
  }
}

/** Stop the forge child: SIGTERM, then SIGKILL if it lingers. */
async function stopForge(child: ChildProcess): Promise<void> {
  if (child.exitCode !== null || child.signalCode !== null) return;
  child.kill("SIGTERM");
  await Promise.race([
    new Promise<void>((resolve) => child.once("exit", () => resolve())),
    sleep(2_000),
  ]);
  if (child.exitCode === null && child.signalCode === null) child.kill("SIGKILL");
  try {
    child.stdin?.end();
  } catch {
    // The child's stdin may already be gone; nothing to do.
  }
}

/** The whole life of the process: start the forge, register, wait, stop it. */
async function run(): Promise<void> {
  dbg(`start envOrigin=${envOrigin} forgePath=${forgePath} cwd=${process.cwd()}`);
  const { child, port } = await startForge();
  dbg(`forge started port=${port} pid=${child.pid} exitCode=${child.exitCode}`);
  log(`forge listening on port ${port}`);

  try {
    await announce(port);
    dbg(`announced childExitCode=${child.exitCode}`);
    log("registered; waiting for the env to say done");

    await waitUntilDone();
    dbg(`done childExitCode=${child.exitCode}`);
    log("done");
  } finally {
    await stopForge(child);
  }
}

run().then(
  () => process.exit(0),
  (err: unknown) => {
    dbg(`run failed: ${err instanceof Error ? err.message : String(err)}`);
    complain(err instanceof Error ? err.message : String(err));
    process.exit(1);
  },
);

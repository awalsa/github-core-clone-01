// main.ts — the process lifecycle for the forge.
//
// The harness (env) owns the trial: it hands this process an origin through
// MAKEFUN_ENV_URL, and this file's only job is to (a) start the server from
// app.ts on a kernel-chosen port, (b) tell the env that port, and (c) stay
// alive until the env reports the trial finished, then shut down cleanly.
// Nothing here implements the forge itself; the suite grades app.ts.
//
// Order of operations:
//   1. Read MAKEFUN_ENV_URL. Missing means there is no env at all: bail out
//      with exit code 1 instead of hanging.
//   2. Build the server and listen on port 0. The kernel picks the port; the
//      env's own port (8080 in a trial container) is never touched.
//   3. POST { port } to <env>/register and retry until it lands. A 200 means
//      the env took it; a 409 means some entry already claimed the slot, in
//      which case waiting is still correct. 400/405 are contract mistakes and
//      abort the run; anything else (env still booting, a timeout) retries.
//   4. Poll <env>/done until it answers {"done": true}. The env responds
//      immediately either way, so this is a plain poll with a pause between
//      tries that doubles up to a cap.
//   5. Close the server and exit 0. The harness stops the env afterwards and
//      the scorer reads the export it writes.

import { createApp } from "./app.ts";

type AppServer = ReturnType<typeof createApp>;

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

/** Start the server on a free port and hand back the port it bound. */
function bindServer(server: AppServer): Promise<number> {
  return new Promise((resolve, reject) => {
    server.once("error", reject);
    server.listen(0, () => {
      const where = server.address();
      if (where === null || typeof where === "string") {
        reject(new Error("the server did not end up on a TCP port"));
        return;
      }
      resolve(where.port);
    });
  });
}

/**
 * Tell the env which port the server bound. Returns once the env has the
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
      if (answer.ok) {
        const payload = (await answer.json()) as { done?: unknown };
        if (payload.done === true) return;
      }
    } catch {
      // The env is still starting, or the poll timed out: not done yet.
    }
    await sleep(pause);
    pause = growPause(pause);
  }
}

/** The whole life of the process: bind, register, wait, shut down. */
async function run(): Promise<void> {
  const server = createApp();
  const port = await bindServer(server);
  log(`listening on port ${port}`);

  await announce(port);
  log("registered; waiting for the env to say done");

  await waitUntilDone();
  server.close();
  log("done");
}

run().then(
  () => process.exit(0),
  (err: unknown) => {
    complain(err instanceof Error ? err.message : String(err));
    process.exit(1);
  },
);

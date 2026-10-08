import { test, expect } from 'bun:test';
import { createServer } from 'node:http';
import { spawn } from 'node:child_process';
import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { setTimeout as delay } from 'node:timers/promises';
import { runDurableNodeCases } from './durable-node-test-runner.mjs';
test('official Durable Node SQLite crash, replay, steering, stop and history cases', async () => {
  expect(await runDurableNodeCases('./durable-session-owner-cases.mjs')).toContain('fail 0');
}, 90000);

// Both network services below are synthetic loopback fixtures: the model server only echoes the
// requested model id back as the answer, and the operation endpoint only answers turn authority.
// No real account, canvas, credential or paid model runs, but the host process, the official
// pi-durable session document and its SQLite checkpoint are the packaged ones.
const MODEL_A = 'beeftv-model-a';
const MODEL_B = 'beeftv-model-b';
const hostRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const listen = (server) => new Promise((resolve, reject) => {
  server.once('error', reject);
  server.listen(0, '127.0.0.1', () => resolve(server.address().port));
});
const jsonBody = async (req) => { let text = ''; for await (const part of req) text += part; return JSON.parse(text || '{}'); };
const deferred = () => { let resolve; const promise = new Promise((yes) => { resolve = yes; }); return { promise, resolve }; };
const sse = (res, model, text) => {
  res.writeHead(200, { 'content-type': 'text/event-stream' });
  const frame = (delta, finish_reason) => res.write('data: ' + JSON.stringify({ id: 'synthetic', object: 'chat.completion.chunk', created: 1,
    model, choices: [{ index: 0, delta, finish_reason }] }) + '\n\n');
  frame({ role: 'assistant', content: text }, 'stop');
  res.end('data: [DONE]\n\n');
};

async function createDurableHostFixture(canvasId) {
  const directory = mkdtempSync(path.join(tmpdir(), 'beeftv durable model switch '));
  const hostToken = `synthetic-${canvasId}`;
  const fixture = { directory, canvasId, authorities: new Map(), bound: [], modelRequests: [], completions: [], mode: 'answer', hang: deferred(), log: '', child: null };

  const ops = createServer(async (req, res) => {
    res.setHeader('content-type', 'application/json');
    if (req.headers['x-beeftv-agent-token'] !== hostToken) { res.writeHead(403).end('{}'); return; }
    const body = await jsonBody(req);
    if (req.method === 'GET' && req.url === '/api/ops') {
      res.end(JSON.stringify({ code: 0, data: { ops: [{ id: 'canvas.get', summary: 'Read the synthetic canvas', readOnly: true, scope: 'canvas',
        params: { type: 'object', properties: { canvasId: { type: 'string' } } } }] } }));
      return;
    }
    const turn = /^\/api\/assistant\/runtime\/turns\/([^/?]+)(\/.*)?$/.exec(req.url) || [];
    const authority = fixture.authorities.get(decodeURIComponent(turn[1] || ''));
    if (req.method === 'GET' && turn[1]) { res.end(JSON.stringify({ code: 0, data: { ...(authority || { open: false }) } })); return; }
    if (turn[2] === '/bind-session') fixture.bound.push({ turnId: turn[1], sessionId: body.sessionId });
    if (turn[2] === '/complete') fixture.completions.push(turn[1]);
    res.end(JSON.stringify({ code: 0, data: {} }));
  });

  const model = createServer(async (req, res) => {
    const body = await jsonBody(req);
    fixture.modelRequests.push(body.model);
    if (fixture.mode === 'hang') {
      // Keep the official provider request in flight so the task stays checkpointed in `request`.
      res.writeHead(200, { 'content-type': 'text/event-stream' });
      fixture.hang.resolve();
      return;
    }
    sse(res, body.model, String(body.model));
  });

  const opsPort = await listen(ops);
  const modelPort = await listen(model);
  const reservation = createServer();
  const port = await listen(reservation);
  await new Promise((resolve) => reservation.close(resolve));
  const base = `http://127.0.0.1:${port}`;

  const waitFor = async (check, ms, label) => {
    const until = Date.now() + ms;
    while (Date.now() < until) { if (await check()) return; await delay(50); }
    throw new Error(`fixture condition timed out: ${label}\n${fixture.log.slice(-2000)}`);
  };

  fixture.launch = async (modelId) => {
    const child = spawn('node', [path.join(hostRoot, 'agent-host/server.mjs')], { cwd: hostRoot, stdio: ['ignore', 'pipe', 'pipe'],
      env: { ...process.env, BEEFTV_AGENT_DATA_DIR: directory, BEEFTV_AGENT_HOST_TOKEN: hostToken, BEEFTV_AGENT_PORT: String(port),
        BEEFTV_OPS_URL: `http://127.0.0.1:${opsPort}/api`, BEEFTV_AGENT_API: 'openai-completions', BEEFTV_AGENT_API_KEY: 'synthetic-only',
        BEEFTV_AGENT_BASE_URL: `http://127.0.0.1:${modelPort}/v1`, BEEFTV_AGENT_MODEL: modelId, BEEFTV_AGENT_TOTAL_REQUEST_BUDGET: '0',
        BEEFTV_AGENT_MAX_REQUESTS_PER_TURN: '40', BEEFTV_AGENT_TURN_TIMEOUT_MS: '60000' } });
    fixture.child = child;
    child.stdout.on('data', (chunk) => { fixture.log += chunk; });
    child.stderr.on('data', (chunk) => { fixture.log += chunk; });
    await waitFor(async () => {
      if (child.exitCode !== null) throw new Error(`host exited: ${fixture.log.slice(-2000)}`);
      try { return (await fetch(`${base}/health`, { signal: AbortSignal.timeout(500) })).ok; } catch { return false; }
    }, 20000, `host start with ${modelId}`);
  };

  fixture.stop = async () => {
    const child = fixture.child;
    fixture.child = null;
    if (!child || child.exitCode !== null) return;
    const exited = new Promise((resolve) => child.once('close', resolve));
    child.kill('SIGKILL');
    await exited;
  };

  // A hung turn is killed mid-request, so its own HTTP response dies with the host and is ignored.
  fixture.chatRaw = async (turnId, message) => {
    fixture.authorities.set(turnId, { canvasId, turnId, open: true, permissionMode: 'canvas' });
    const response = await fetch(`${base}/chat`, { method: 'POST', signal: AbortSignal.timeout(60000),
      headers: { 'content-type': 'application/json', 'X-Beeftv-Agent-Token': hostToken },
      body: JSON.stringify({ canvasId, message, turnId, revisionBefore: 1 }) });
    return (await response.text()).trim().split('\n').filter(Boolean).map((line) => JSON.parse(line));
  };
  fixture.chat = async (turnId, message) => {
    const lines = await fixture.chatRaw(turnId, message);
    const record = lines.find((line) => line.type === 'turn_end') || lines.find((line) => line.type === 'observer_error');
    expect(record, `no turn record; host log:\n${fixture.log.slice(-2000)}`).toBeDefined();
    return record;
  };

  fixture.history = async () => (await fetch(`${base}/history?canvasId=${encodeURIComponent(canvasId)}`,
    { headers: { 'X-Beeftv-Agent-Token': hostToken }, signal: AbortSignal.timeout(30000) })).json();
  // Recovery only starts when a route asks for progress; /history is the read that admits it.
  fixture.awaitSettledTurn = async (turnId) => {
    const until = Date.now() + 30000;
    while (Date.now() < until) {
      const history = await fixture.history();
      if (history.turns.some((turn) => turn.turnId === turnId)) return history;
      await delay(250);
    }
    throw new Error(`fixture condition timed out: turn ${turnId} settled\n${fixture.log.slice(-2000)}`);
  };
  fixture.arriveAtHungRequest = () => fixture.hang.promise;

  fixture.dispose = async () => {
    await fixture.stop();
    for (const server of [ops, model]) { if (server.listening) { server.closeAllConnections(); await new Promise((resolve) => server.close(resolve)); } }
    rmSync(directory, { recursive: true, force: true });
  };
  return fixture;
}

test('restarting the host keeps an existing durable session usable, with the same and with a switched model', async () => {
  const fixture = await createDurableHostFixture('switch-canvas');
  try {
    await fixture.launch(MODEL_A);
    const first = await fixture.chat('switchturn01', 'first turn on the old model');
    expect(first.reply, JSON.stringify(first)).toBe(MODEL_A);
    expect(first.errorReason ?? null).toBeNull();

    // A2: only the host restarts, the model id stays registered; the same session must keep working.
    await fixture.stop();
    await fixture.launch(MODEL_A);
    const sameModel = await fixture.chat('switchturn02', 'second turn after a same-model restart');
    expect(sameModel.reply, JSON.stringify(sameModel)).toBe(MODEL_A);
    expect(sameModel.sessionId).toBe(first.sessionId);

    // B1: the configured model changes and the host restarts; the next turn of the existing session
    // must follow the new registration instead of pinning the model the session was created with.
    await fixture.stop();
    await fixture.launch(MODEL_B);
    const switched = await fixture.chat('switchturn03', 'third turn after the model was switched');
    expect(switched.errorReason ?? null, JSON.stringify(switched)).toBeNull();
    expect(switched.reply, JSON.stringify(switched)).toBe(MODEL_B);
    expect(switched.sessionId).toBe(first.sessionId);
    expect(fixture.modelRequests).toEqual([MODEL_A, MODEL_A, MODEL_B]);
  } finally { await fixture.dispose(); }
}, 180000);

test('an in-flight request checkpoint on a withdrawn model settles as session_model_changed and the session recovers', async () => {
  const fixture = await createDurableHostFixture('inflight-canvas');
  try {
    await fixture.launch(MODEL_A);
    // Start a turn whose provider request never returns, then hard kill the host: the durable task
    // stays checkpointed in the `request` phase with the old model reference pinned in it.
    fixture.mode = 'hang';
    fixture.hang = deferred();
    // The handler is attached at creation: the socket resets when the host is killed, and a later
    // `.catch` would let bun report it as an unhandled rejection first.
    const hung = fixture.chatRaw('inflightturn0', 'this turn is still in flight when the host dies').catch(() => null);
    await fixture.arriveAtHungRequest();
    await delay(200);
    await fixture.stop();
    await hung;
    fixture.mode = 'answer';

    // A new host registers only the new model, so the stale in-flight checkpoint cannot be rewritten.
    await fixture.launch(MODEL_B);
    const history = await fixture.awaitSettledTurn('inflightturn0');
    const turn = history.turns.find((item) => item.turnId === 'inflightturn0');
    expect(turn.status).toBe('unanswered');
    expect(turn.errorReason).toBe('session_model_changed');

    // The session itself is not permanently poisoned: the following turn runs on the new model.
    const next = await fixture.chat('inflightturn1', 'next turn after the model switch');
    expect(next.errorReason ?? null, JSON.stringify(next)).toBeNull();
    expect(next.reply, JSON.stringify(next)).toBe(MODEL_B);
    expect(fixture.modelRequests).toEqual([MODEL_A, MODEL_B]);
  } finally { await fixture.dispose(); }
}, 180000);

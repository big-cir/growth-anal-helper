// growth-lab command line.
import { join } from 'node:path';
import { findWorkspaceDir, gitRoot, loadWorkspace } from './workspace.ts';
import { formatFinding, runPublicCheck } from './public-check.ts';
import { runCollect, runDerive, runTestDerived } from './snapshot/pipeline.ts';
import type { SourceAdapter } from './collect/sources/source.ts';
import { App } from './server/app.ts';
import { startServer } from './server/http.ts';
import { PUBLIC_EXAMPLE_DIR, assertOutputsIgnored, guardedPaths, type Workspace } from './workspace.ts';
import { seedDemo } from '../examples/demo/seed.ts';
import { localNow } from './collect/collector.ts';
import { accountCommand, ACCOUNT_USAGE, audited, pendingInputPasswords, readHidden } from './auth/cli.ts';
import { addAccount, readAccounts } from './auth/accounts.ts';
import { runVerify, VerifyCancelled } from './verify.ts';

const USAGE = `Usage: node src/cli.ts <command>
  serve [--port N]        Web server
  collect                 Collect from the source → finalize a snapshot
  derive                  Rebuild derived tables and the pseudonymized copy of the current snapshot
  test-derived            Derived-rule fixtures
  eval [--blind]          Agent accuracy evaluation (not implemented yet)
  verify [--week DATE]    Cross-check with the source (workspace verify.json; default: the Monday week 5 weeks before the cutoff)
  public-check [--history] [--require-denylist]
  demo                    Example workspace: seed → collect → serve
  ${ACCOUNT_USAGE}
                          (for the example workspace, prefix with GROWTH_LAB_WORKSPACE=examples/demo)`;

const NOT_YET = new Set(['eval']);

function publicCheck(args: string[]): number {
  for (const a of args) {
    if (a !== '--history' && a !== '--require-denylist') {
      console.error(`Unknown option: ${a}`);
      return 2;
    }
  }
  const repo = gitRoot(process.cwd());
  if (!repo) {
    console.error('Run this inside a git repository');
    return 2;
  }
  const result = runPublicCheck({
    repo,
    denylistFile: join(findWorkspaceDir(process.env, repo), 'public-denylist.txt'),
    history: args.includes('--history'),
    requireDenylist: args.includes('--require-denylist'),
  });
  for (const w of result.warnings) console.error(`Warning: ${w}`);
  for (const f of result.findings) console.error(formatFinding(f));
  if (!result.ok) {
    console.error(`public-check failed: ${result.findings.length} finding(s)`);
    return 1;
  }
  console.log(`public-check passed${args.includes('--history') ? ' (including history)' : ''}`);
  return 0;
}

async function collectCmd(): Promise<number> {
  const ws = loadWorkspace(findWorkspaceDir());
  let source: SourceAdapter | null = null;
  let interrupted = false;
  const onSigint = () => {
    interrupted = true;
    source?.abort();
  };
  process.on('SIGINT', onSigint);
  try {
    const r = await runCollect(ws, { log: (m) => console.log(m), onSource: (s) => { source = s; } });
    console.log(`Snapshot ${r.reused ? 'reused' : 'finalized'}: ${r.snapshotId}`);
    return 0;
  } catch (e) {
    if (interrupted) {
      console.error('Stopped: temporary files removed');
      return 130;
    }
    throw e;
  } finally {
    process.off('SIGINT', onSigint);
  }
}

async function verifyCmd(args: string[]): Promise<number> {
  const i = args.indexOf('--week');
  if (i >= 0 && args[i + 1] === undefined) throw new Error('--week needs a date (YYYY-MM-DD)');
  let source: SourceAdapter | null = null;
  const ac = new AbortController();
  const onSigint = () => {
    ac.abort();
    source?.abort();
  };
  process.on('SIGINT', onSigint);
  try {
    const r = await runVerify(loadWorkspace(findWorkspaceDir()), { week: i >= 0 ? args[i + 1] : undefined, onSource: (s) => { source = s; }, signal: ac.signal });
    console.log(`Snapshot ${r.snapshot_id}, week ${r.week_start.slice(0, 10)} ~ ${r.week_end.slice(0, 10)}`);
    for (const it of r.items) {
      console.log(`${it.ok ? 'match   ' : 'MISMATCH'}  ${it.id} (${it.title})`);
      if (it.source) console.log(`  source    ${JSON.stringify(it.source)}`);
      if (it.snapshot) console.log(`  snapshot  ${JSON.stringify(it.snapshot)}`);
      if (it.error) console.log(`  ${it.error}`);
    }
    console.log(`${r.items.filter((x) => x.ok).length} of ${r.items.length} match`);
    return r.ok ? 0 : 1;
  } catch (e) {
    if (e instanceof VerifyCancelled) {
      console.error('Stopped');
      return 130;
    }
    throw e;
  } finally {
    process.off('SIGINT', onSigint);
  }
}

function deriveCmd(): number {
  const r = runDerive(loadWorkspace(findWorkspaceDir()));
  console.log(`Snapshot ${r.reused ? 'reused' : 'finalized'}: ${r.snapshotId}`);
  return 0;
}

function testDerivedCmd(): number {
  const results = runTestDerived(loadWorkspace(findWorkspaceDir()));
  for (const r of results) console.log(`${r.ok ? 'pass' : 'FAIL'}  ${r.name}${r.message ? `\n  ${r.message}` : ''}`);
  const failed = results.filter((r) => !r.ok).length;
  console.log(`${results.length - failed} of ${results.length} passed`);
  return failed === 0 && results.length > 0 ? 0 : 1;
}

async function serve(ws: Workspace, args: string[]): Promise<number> {
  assertOutputsIgnored(guardedPaths(ws), ws.dir);
  if (pendingInputPasswords(ws.config.outDir)) throw new Error('accounts.input.json still has passwords. Run `node src/cli.ts account import` first');
  const i = args.indexOf('--port');
  const port = i >= 0 ? Number(args[i + 1]) : ws.config.server.port;
  if (!Number.isInteger(port) || port < 1 || port > 65535) throw new Error('--port must be 1-65535');
  const app = new App(ws);
  const handle = await startServer(app, port);
  console.log(`growth-lab: http://127.0.0.1:${handle.port}  (workspace ${ws.config.name})`);
  if (!ws.config.server.auth) console.log('Auth is off: anyone can use it as admin without signing in. Set server.auth to true in workspace.json before exposing it');
  if (!app.snapshot()) console.log('No snapshot yet. Run collect first');
  void app.startIsolationCheck().then(() => console.log(`Agent: ${app.agent.message}`));
  await new Promise<void>((resolve) => {
    const stop = () => resolve();
    process.once('SIGINT', stop);
    process.once('SIGTERM', stop);
  });
  console.log('Stopping the server…');
  await handle.close();
  return 0;
}

/** Create example data, collect it and start the server */
async function demo(args: string[]): Promise<number> {
  const ws = loadWorkspace(PUBLIC_EXAMPLE_DIR);
  if (ws.config.server.auth && readAccounts(ws.config.outDir).length === 0) {
    if (!process.stdin.isTTY) throw new Error('No accounts. Run `GROWTH_LAB_WORKSPACE=examples/demo node src/cli.ts account add <name> --role admin` first');
    console.log('Creating an admin account for the example server.');
    process.stdout.write('Name: ');
    const name = await new Promise<string>((r) => process.stdin.once('data', (b) => r(String(b).trim())));
    const a = await readHidden('Password (12+ characters): ');
    const b = await readHidden('Again: ');
    if (a !== b) throw new Error('The two entries differ');
    await audited(ws.config.outDir, name, () => addAccount(ws.config.outDir, name, 'admin', a));
  }
  const source = ws.config.datasource;
  if (source.kind !== 'sqlite') throw new Error('The example workspace source must be sqlite');
  const now = localNow().slice(0, 19);
  console.log(`Creating example data (as of ${now})`);
  console.log(JSON.stringify(seedDemo(source.path, { anchor: now })));
  const r = await runCollect(ws, { log: (m) => console.log(m) });
  console.log(`Snapshot ${r.reused ? 'reused' : 'finalized'}: ${r.snapshotId}`);
  return serve(ws, args);
}

async function main(argv: string[]): Promise<number> {
  const [cmd, ...rest] = argv;
  if (!cmd || cmd === '-h' || cmd === '--help') {
    console.log(USAGE);
    return cmd ? 0 : 2;
  }
  if (cmd === 'public-check') return publicCheck(rest);
  try {
    if (cmd === 'collect') return await collectCmd();
    if (cmd === 'derive') return deriveCmd();
    if (cmd === 'test-derived') return testDerivedCmd();
    if (cmd === 'verify') return await verifyCmd(rest);
    if (cmd === 'serve') return await serve(loadWorkspace(findWorkspaceDir()), rest);
    if (cmd === 'demo') return await demo(rest);
    if (cmd === 'account') {
      const ws = loadWorkspace(findWorkspaceDir());
      return await accountCommand(ws.config.outDir, rest);
    }
  } catch (e) {
    console.error(`${cmd} failed: ${(e as Error).message}`);
    return 1;
  }
  if (NOT_YET.has(cmd)) {
    console.error(`${cmd}: not implemented yet`);
    return 2;
  }
  console.error(`Unknown command: ${cmd}\n${USAGE}`);
  return 2;
}

process.exitCode = await main(process.argv.slice(2));

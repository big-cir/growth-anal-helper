// growth-lab 명령줄.
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

const USAGE = `사용법: node src/cli.ts <명령>
  serve [--port N]        웹 서버
  collect                 소스 수집 → 스냅샷 확정
  derive                  현재 스냅샷의 파생·가명 사본 재생성
  test-derived            파생 규칙 픽스처
  eval [--blind]          에이전트 정확도 평가
  verify                  소스 교차 검증
  public-check [--history] [--require-denylist]
  demo                    예제 워크스페이스로 시드 → 수집 → serve
  ${ACCOUNT_USAGE}
                          (예제 워크스페이스 계정은 GROWTH_LAB_WORKSPACE=examples/demo를 앞에 붙임)`;

const NOT_YET = new Set(['eval', 'verify']);

function publicCheck(args: string[]): number {
  for (const a of args) {
    if (a !== '--history' && a !== '--require-denylist') {
      console.error(`알 수 없는 옵션: ${a}`);
      return 2;
    }
  }
  const repo = gitRoot(process.cwd());
  if (!repo) {
    console.error('git 저장소 안에서 실행하세요');
    return 2;
  }
  const result = runPublicCheck({
    repo,
    denylistFile: join(findWorkspaceDir(process.env, repo), 'public-denylist.txt'),
    history: args.includes('--history'),
    requireDenylist: args.includes('--require-denylist'),
  });
  for (const w of result.warnings) console.error(`경고: ${w}`);
  for (const f of result.findings) console.error(formatFinding(f));
  if (!result.ok) {
    console.error(`public-check 실패: ${result.findings.length}건`);
    return 1;
  }
  console.log(`public-check 통과${args.includes('--history') ? ' (이력 포함)' : ''}`);
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
    console.log(`스냅샷 ${r.reused ? '재사용' : '확정'}: ${r.snapshotId}`);
    return 0;
  } catch (e) {
    if (interrupted) {
      console.error('중단됨: 임시 파일을 지웠습니다');
      return 130;
    }
    throw e;
  } finally {
    process.off('SIGINT', onSigint);
  }
}

function deriveCmd(): number {
  const r = runDerive(loadWorkspace(findWorkspaceDir()));
  console.log(`스냅샷 ${r.reused ? '재사용' : '확정'}: ${r.snapshotId}`);
  return 0;
}

function testDerivedCmd(): number {
  const results = runTestDerived(loadWorkspace(findWorkspaceDir()));
  for (const r of results) console.log(`${r.ok ? '통과' : '실패'}  ${r.name}${r.message ? `\n  ${r.message}` : ''}`);
  const failed = results.filter((r) => !r.ok).length;
  console.log(`${results.length}개 중 ${results.length - failed}개 통과`);
  return failed === 0 && results.length > 0 ? 0 : 1;
}

async function serve(ws: Workspace, args: string[]): Promise<number> {
  assertOutputsIgnored(guardedPaths(ws), ws.dir);
  if (pendingInputPasswords(ws.config.outDir)) throw new Error('accounts.input.json에 비밀번호가 남아 있어요. 먼저 `node src/cli.ts account import`를 실행하세요');
  const i = args.indexOf('--port');
  const port = i >= 0 ? Number(args[i + 1]) : ws.config.server.port;
  if (!Number.isInteger(port) || port < 1 || port > 65535) throw new Error('--port는 1~65535');
  const app = new App(ws);
  const handle = await startServer(app, port);
  console.log(`growth-lab: http://127.0.0.1:${handle.port}  (워크스페이스 ${ws.config.name})`);
  if (!app.snapshot()) console.log('스냅샷이 없습니다. collect를 먼저 실행하세요');
  void app.startIsolationCheck().then(() => console.log(`에이전트: ${app.agent.message}`));
  await new Promise<void>((resolve) => {
    const stop = () => resolve();
    process.once('SIGINT', stop);
    process.once('SIGTERM', stop);
  });
  console.log('서버를 멈추는 중…');
  await handle.close();
  return 0;
}

/** 예제 데이터를 만들고 수집한 뒤 서버를 띄운다 */
async function demo(args: string[]): Promise<number> {
  const ws = loadWorkspace(PUBLIC_EXAMPLE_DIR);
  if (readAccounts(ws.config.outDir).length === 0) {
    if (!process.stdin.isTTY) throw new Error('계정이 없어요. 먼저 `GROWTH_LAB_WORKSPACE=examples/demo node src/cli.ts account add <이름> --role admin`을 실행하세요');
    console.log('예제 서버에 로그인할 관리자 계정을 만듭니다.');
    process.stdout.write('이름: ');
    const name = await new Promise<string>((r) => process.stdin.once('data', (b) => r(String(b).trim())));
    const a = await readHidden('비밀번호(12자 이상): ');
    const b = await readHidden('한 번 더: ');
    if (a !== b) throw new Error('두 입력이 달라요');
    await audited(ws.config.outDir, name, () => addAccount(ws.config.outDir, name, 'admin', a));
  }
  const source = ws.config.source;
  if (source.type !== 'sqlite') throw new Error('예제 워크스페이스의 소스는 sqlite여야 함');
  const now = localNow().slice(0, 19);
  console.log(`가상 데이터 생성 (기준 ${now})`);
  console.log(JSON.stringify(seedDemo(source.path, { anchor: now })));
  const r = await runCollect(ws, { log: (m) => console.log(m) });
  console.log(`스냅샷 ${r.reused ? '재사용' : '확정'}: ${r.snapshotId}`);
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
    if (cmd === 'serve') return await serve(loadWorkspace(findWorkspaceDir()), rest);
    if (cmd === 'demo') return await demo(rest);
    if (cmd === 'account') {
      const ws = loadWorkspace(findWorkspaceDir());
      return await accountCommand(ws.config.outDir, rest);
    }
  } catch (e) {
    console.error(`${cmd} 실패: ${(e as Error).message}`);
    return 1;
  }
  if (NOT_YET.has(cmd)) {
    console.error(`${cmd}: 아직 구현되지 않음`);
    return 2;
  }
  console.error(`알 수 없는 명령: ${cmd}\n${USAGE}`);
  return 2;
}

process.exitCode = await main(process.argv.slice(2));

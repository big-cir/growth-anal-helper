// 로컬 명령 소스(mysql 클라이언트 등). SQL은 stdin으로 넘긴다.
import { spawn, type ChildProcess } from 'node:child_process';
import { createInterface } from 'node:readline';
import type { CommandSource } from '../../workspace.ts';
import type { RawValue, SelectResult, SourceAdapter } from './source.ts';
import { parseLine } from './tsv.ts';

export class CommandSourceAdapter implements SourceAdapter {
  readonly wrapText = true;
  private readonly cfg: CommandSource;
  private readonly logStderr: (line: string) => void;
  private child: ChildProcess | null = null;

  constructor(cfg: CommandSource, logStderr: (line: string) => void = () => {}) {
    this.cfg = cfg;
    this.logStderr = logStderr;
  }

  async now(): Promise<string> {
    let value: string | null = null;
    await this.selectStream(this.cfg.nowQuery ?? 'SELECT NOW(6) AS now', (v) => {
      if (value !== null) throw new Error('nowQuery가 두 행 이상을 냄');
      value = v[0];
    });
    if (value === null) throw new Error('nowQuery가 값을 내지 않음');
    return value;
  }

  selectStream(sql: string, onRow: (values: RawValue[]) => void, onColumns?: (columns: string[]) => void): Promise<SelectResult> {
    const t0 = performance.now();
    const [cmd, ...args] = this.cfg.localCommand;
    const ignore = this.cfg.ignoreStderrPattern ? new RegExp(this.cfg.ignoreStderrPattern) : null;
    return new Promise((resolve, reject) => {
      const child = spawn(cmd, args, { stdio: ['pipe', 'pipe', 'pipe'] });
      this.child = child;
      let columns: string[] | null = null;
      let rows = 0;
      let failure: Error | null = null;
      const stop = (e: Error) => {
        if (!failure) failure = e;
        child.kill('SIGKILL');
      };

      createInterface({ input: child.stdout! }).on('line', (line) => {
        if (failure) return;
        try {
          const values = parseLine(line);
          if (columns === null) {
            columns = values.map((v) => v ?? 'NULL');
            onColumns?.(columns);
            return;
          }
          if (values.length !== columns.length) throw new Error(`열 개수 불일치: ${values.length} ≠ ${columns.length} (행 ${rows + 1})`);
          onRow(values);
          rows++;
        } catch (e) {
          stop(e as Error);
        }
      });
      createInterface({ input: child.stderr! }).on('line', (line) => {
        if (!ignore || !ignore.test(line)) this.logStderr(line);
      });
      child.on('error', (e) => stop(e));
      child.on('close', (code, signal) => {
        this.child = null;
        if (failure) return reject(failure);
        if (code !== 0) return reject(new Error(`소스 명령 실패 (종료 코드 ${code ?? signal}). 자세한 내용은 진단 로그`));
        resolve({ columns: columns ?? [], rows, ms: Math.round(performance.now() - t0) });
      });
      child.stdin!.on('error', () => {});
      child.stdin!.end([...this.cfg.preamble, `${sql};`, ''].join('\n'));
    });
  }

  abort(): void {
    this.child?.kill('SIGKILL');
  }
}

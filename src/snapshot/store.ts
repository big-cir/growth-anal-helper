// 스냅샷 파일 위치와 current 포인터.
import { closeSync, existsSync, fsyncSync, openSync, readFileSync, renameSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';

export type Current = { snapshot_id: string; file: string; agent_file: string };

export function snapshotsDir(outRoot: string): string {
  return join(outRoot, 'snapshots');
}

export function snapshotFiles(dir: string, id: string) {
  return { real: join(dir, `${id}.sqlite`), agent: join(dir, `${id}.agent.sqlite`), map: join(dir, `${id}.pseudo-map.sqlite`) };
}

export function fsyncPath(p: string): void {
  const fd = openSync(p, 'r');
  try {
    fsyncSync(fd);
  } finally {
    closeSync(fd);
  }
}

export function readCurrent(dir: string): Current | null {
  const p = join(dir, 'current.json');
  if (!existsSync(p)) return null;
  const c = JSON.parse(readFileSync(p, 'utf8')) as Current;
  return { ...c, file: join(dir, c.file), agent_file: join(dir, c.agent_file) };
}

export function writeCurrent(dir: string, id: string): void {
  const tmp = join(dir, 'current.json.tmp');
  writeFileSync(tmp, JSON.stringify({ snapshot_id: id, file: `${id}.sqlite`, agent_file: `${id}.agent.sqlite` }, null, 2) + '\n');
  fsyncPath(tmp);
  renameSync(tmp, join(dir, 'current.json'));
  fsyncPath(dir);
}

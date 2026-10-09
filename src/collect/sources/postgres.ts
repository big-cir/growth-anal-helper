// PostgreSQL source: connects directly, no client program. Read-only session; results are streamed in text format.
import { createHash, createHmac, pbkdf2Sync, randomBytes, timingSafeEqual } from 'node:crypto';
import type { ServerDatasource } from '../../workspace.ts';
import { cstr, Cursor, Wire, WireError } from './wire.ts';
import { singleValue, type RawValue, type SelectResult, type SourceAdapter } from './source.ts';

const SSL_REQUEST = 80877103;
const PROTOCOL_3 = 196608;

const md5 = (s: string | Buffer) => createHash('md5').update(s).digest('hex');
const hmac = (key: Buffer, s: string) => createHmac('sha256', key).update(s).digest();

function serverError(body: Buffer): WireError {
  const c = new Cursor(body);
  const f: Record<string, string> = {};
  while (!c.done) {
    const t = c.u8();
    if (t === 0) break;
    f[String.fromCharCode(t)] = c.cstr();
  }
  return new WireError(`PostgreSQL error ${f.C ?? ''}: ${f.M ?? '(no message)'}`);
}

class Conn {
  readonly wire: Wire;

  constructor(wire: Wire) {
    this.wire = wire;
  }

  async message(): Promise<{ type: string; body: Buffer }> {
    const h = await this.wire.read(5);
    return { type: String.fromCharCode(h[0]), body: await this.wire.read(h.readInt32BE(1) - 4) };
  }

  send(type: string, body: Buffer): void {
    const h = Buffer.alloc(5);
    h.write(type, 0, 'latin1');
    h.writeInt32BE(body.length + 4, 1);
    this.wire.write(Buffer.concat([h, body]));
  }

  private sendUntyped(body: Buffer): void {
    const h = Buffer.alloc(4);
    h.writeInt32BE(body.length + 4, 0);
    this.wire.write(Buffer.concat([h, body]));
  }

  async startup(ds: ServerDatasource): Promise<void> {
    const req = Buffer.alloc(4);
    req.writeInt32BE(SSL_REQUEST, 0);
    this.sendUntyped(req);
    const answer = (await this.wire.read(1))[0];
    if (answer === 0x53) await this.wire.startTls();
    else if (answer !== 0x4e) throw new WireError('malformed PostgreSQL response (SSL request)');

    const params: [string, string][] = [
      ['user', ds.user], ['database', ds.database], ['client_encoding', 'UTF8'], ['DateStyle', 'ISO'],
      ['options', '-c default_transaction_read_only=on'], ['application_name', 'growth-lab'],
    ];
    const ver = Buffer.alloc(4);
    ver.writeInt32BE(PROTOCOL_3, 0);
    this.sendUntyped(Buffer.concat([ver, ...params.flatMap(([k, v]) => [cstr(k), cstr(v)]), Buffer.from([0])]));

    let scram: { nonce: string; bare: string; serverSig?: Buffer } | null = null;
    for (;;) {
      const { type, body } = await this.message();
      if (type === 'E') throw serverError(body);
      if (type === 'Z') return;
      if (type !== 'R') continue;
      const code = body.readInt32BE(0);
      if (code === 0) continue;
      if (code === 3) {
        this.send('p', cstr(ds.password));
      } else if (code === 5) {
        this.send('p', cstr(`md5${md5(Buffer.concat([Buffer.from(md5(ds.password + ds.user)), body.subarray(4, 8)]))}`));
      } else if (code === 10) {
        const mechs = new Cursor(body, 4);
        const list: string[] = [];
        while (!mechs.done) {
          const m = mechs.cstr();
          if (m) list.push(m);
        }
        if (!list.includes('SCRAM-SHA-256')) throw new WireError(`unsupported PostgreSQL auth method: ${list.join(', ')}`);
        const nonce = randomBytes(18).toString('base64');
        scram = { nonce, bare: `n=*,r=${nonce}` };
        const first = Buffer.from(`n,,${scram.bare}`, 'utf8');
        const len = Buffer.alloc(4);
        len.writeInt32BE(first.length, 0);
        this.send('p', Buffer.concat([cstr('SCRAM-SHA-256'), len, first]));
      } else if (code === 11 && scram) {
        const serverFirst = body.subarray(4).toString('utf8');
        const attr = Object.fromEntries(serverFirst.split(',').map((kv) => [kv[0], kv.slice(2)]));
        const iterations = Number(attr.i);
        if (!attr.r?.startsWith(scram.nonce) || !attr.s || !Number.isInteger(iterations) || iterations < 1) throw new WireError('malformed PostgreSQL SCRAM response');
        const salted = pbkdf2Sync(Buffer.from(ds.password, 'utf8'), Buffer.from(attr.s, 'base64'), iterations, 32, 'sha256');
        const clientKey = hmac(salted, 'Client Key');
        const withoutProof = `c=biws,r=${attr.r}`;
        const authMessage = `${scram.bare},${serverFirst},${withoutProof}`;
        const sig = hmac(createHash('sha256').update(clientKey).digest(), authMessage);
        const proof = Buffer.from(clientKey.map((x, i) => x ^ sig[i]));
        scram.serverSig = hmac(hmac(salted, 'Server Key'), authMessage);
        this.send('p', Buffer.from(`${withoutProof},p=${proof.toString('base64')}`, 'utf8'));
      } else if (code === 12 && scram?.serverSig) {
        const v = /(?:^|,)v=([^,]+)/.exec(body.subarray(4).toString('utf8'))?.[1];
        const got = v ? Buffer.from(v, 'base64') : Buffer.alloc(0);
        if (got.length !== scram.serverSig.length || !timingSafeEqual(got, scram.serverSig)) throw new WireError('PostgreSQL server signature mismatch');
      } else {
        throw new WireError(`unsupported PostgreSQL auth method (code ${code})`);
      }
    }
  }

  async query(sql: string, onRow: (values: RawValue[]) => void, onColumns?: (columns: string[]) => void): Promise<{ columns: string[]; rows: number }> {
    this.send('Q', cstr(sql));
    let columns: string[] | null = null;
    let rows = 0;
    for (;;) {
      const { type, body } = await this.message();
      if (type === 'E') throw serverError(body);
      if (type === 'Z') return { columns: columns ?? [], rows };
      if (type === 'G' || type === 'H') throw new WireError('COPY is not accepted');
      if (type === 'T') {
        if (columns) throw new WireError('more than one result (one statement only)');
        const c = new Cursor(body);
        const n = c.i16be();
        columns = [];
        for (let i = 0; i < n; i++) {
          columns.push(c.cstr());
          c.bytes(18);
        }
        onColumns?.(columns);
      } else if (type === 'D') {
        const c = new Cursor(body);
        const n = c.i16be();
        const values: RawValue[] = [];
        for (let i = 0; i < n; i++) {
          const len = c.i32be();
          values.push(len < 0 ? null : c.bytes(len).toString('utf8'));
        }
        onRow(values);
        rows++;
      }
    }
  }

  quit(): void {
    try {
      this.send('X', Buffer.alloc(0));
    } catch {}
    this.wire.destroy();
  }
}

export class PostgresSource implements SourceAdapter {
  readonly dialect = 'postgres';
  private readonly ds: ServerDatasource;
  private wire: Wire | null = null;
  private aborted = false;

  constructor(ds: ServerDatasource) {
    this.ds = ds;
  }

  now(): Promise<string> {
    return singleValue(this, "SELECT to_char(LOCALTIMESTAMP, 'YYYY-MM-DD HH24:MI:SS.US') AS now");
  }

  async selectStream(sql: string, onRow: (values: RawValue[]) => void, onColumns?: (columns: string[]) => void): Promise<SelectResult> {
    const t0 = performance.now();
    if (this.aborted) throw new WireError('cancelled');
    const wire = await Wire.open(this.ds.host, this.ds.port);
    this.wire = wire;
    if (this.aborted) wire.destroy(new WireError('cancelled'));
    const conn = new Conn(wire);
    try {
      await conn.startup(this.ds);
      const r = await conn.query(sql, onRow, onColumns);
      return { ...r, ms: Math.round(performance.now() - t0) };
    } finally {
      this.wire = null;
      conn.quit();
    }
  }

  abort(): void {
    this.aborted = true;
    this.wire?.destroy(new WireError('cancelled'));
  }
}

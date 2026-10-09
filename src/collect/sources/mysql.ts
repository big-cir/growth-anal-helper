// MySQL source: connects directly, no client program. Read-only session; results are streamed in text format.
import { constants, createHash, publicEncrypt } from 'node:crypto';
import type { ServerDatasource } from '../../workspace.ts';
import { cstr, Cursor, Wire, WireError } from './wire.ts';
import { singleValue, type RawValue, type SelectResult, type SourceAdapter } from './source.ts';

const CAP = {
  LONG_PASSWORD: 0x1, LONG_FLAG: 0x4, CONNECT_WITH_DB: 0x8, PROTOCOL_41: 0x200, SSL: 0x800,
  TRANSACTIONS: 0x2000, SECURE_CONNECTION: 0x8000, MULTI_RESULTS: 0x20000, PLUGIN_AUTH: 0x80000, PLUGIN_AUTH_LENENC: 0x200000,
};
const UTF8MB4_GENERAL_CI = 45;
const MAX_PAYLOAD = 0xffffff;

const sha1 = (...b: Buffer[]) => createHash('sha1').update(Buffer.concat(b)).digest();
const sha256 = (...b: Buffer[]) => createHash('sha256').update(Buffer.concat(b)).digest();
const xor = (a: Buffer, b: Buffer) => Buffer.from(a.map((x, i) => x ^ b[i % b.length]));

function authResponse(plugin: string, password: string, scramble: Buffer): Buffer {
  if (password === '') return Buffer.alloc(0);
  const pw = Buffer.from(password, 'utf8');
  if (plugin === 'mysql_native_password') return xor(sha1(pw), sha1(scramble, sha1(sha1(pw))));
  if (plugin === 'caching_sha2_password') return xor(sha256(pw), sha256(sha256(sha256(pw)), scramble));
  throw new WireError(`unsupported MySQL auth plugin: ${plugin}`);
}

function serverError(b: Buffer): WireError {
  const c = new Cursor(b, 1);
  const code = c.u16le();
  if (b[c.pos] === 0x23) c.bytes(6);
  return new WireError(`MySQL error ${code}: ${c.rest().toString('utf8')}`);
}

/** Length-encoded integer. 0xfb is NULL */
function lenInt(c: Cursor): number | null {
  const f = c.u8();
  if (f < 0xfb) return f;
  if (f === 0xfb) return null;
  if (f === 0xfc) return c.u16le();
  if (f === 0xfd) {
    const v = c.b.readUIntLE(c.pos, 3);
    c.pos += 3;
    return v;
  }
  const v = Number(c.b.readBigUInt64LE(c.pos));
  c.pos += 8;
  return v;
}

function lenStr(c: Cursor): Buffer | null {
  const n = lenInt(c);
  return n === null ? null : c.bytes(n);
}

class Conn {
  readonly wire: Wire;
  private seq = 0;

  constructor(wire: Wire) {
    this.wire = wire;
  }

  async packet(): Promise<Buffer> {
    const parts: Buffer[] = [];
    for (;;) {
      const h = await this.wire.read(4);
      const len = h.readUIntLE(0, 3);
      this.seq = (h[3] + 1) & 0xff;
      parts.push(await this.wire.read(len));
      if (len < MAX_PAYLOAD) break;
    }
    return parts.length === 1 ? parts[0] : Buffer.concat(parts);
  }

  send(payload: Buffer, reset = false): void {
    if (payload.length >= MAX_PAYLOAD) throw new WireError('SQL too large to send');
    if (reset) this.seq = 0;
    const h = Buffer.alloc(4);
    h.writeUIntLE(payload.length, 0, 3);
    h[3] = this.seq;
    this.seq = (this.seq + 1) & 0xff;
    this.wire.write(Buffer.concat([h, payload]));
  }

  async handshake(ds: ServerDatasource): Promise<void> {
    const first = await this.packet();
    if (first[0] === 0xff) throw serverError(first);
    const c = new Cursor(first);
    if (c.u8() !== 10) throw new WireError('unsupported MySQL server (not handshake v10)');
    c.cstr();
    c.u32le();
    let scramble = c.bytes(8);
    c.u8();
    let caps = c.u16le();
    let plugin = 'mysql_native_password';
    if (!c.done) {
      c.u8();
      c.u16le();
      caps = (caps | (c.u16le() << 16)) >>> 0;
      const authLen = c.u8();
      c.bytes(10);
      if (caps & CAP.SECURE_CONNECTION) {
        const part = c.bytes(Math.max(13, authLen - 8));
        scramble = Buffer.concat([scramble, part.at(-1) === 0 ? part.subarray(0, -1) : part]);
      }
      if (caps & CAP.PLUGIN_AUTH) plugin = c.cstr() || plugin;
    }
    if (!(caps & CAP.PROTOCOL_41) || !(caps & CAP.SECURE_CONNECTION)) throw new WireError('MySQL server too old');

    const tls = (caps & CAP.SSL) !== 0;
    const mine = (CAP.LONG_PASSWORD | CAP.LONG_FLAG | CAP.CONNECT_WITH_DB | CAP.PROTOCOL_41 | CAP.TRANSACTIONS | CAP.SECURE_CONNECTION
      | CAP.MULTI_RESULTS | CAP.PLUGIN_AUTH | (caps & CAP.PLUGIN_AUTH_LENENC) | (tls ? CAP.SSL : 0)) >>> 0;
    const head = Buffer.alloc(32);
    head.writeUInt32LE(mine, 0);
    head.writeUInt32LE(MAX_PAYLOAD + 1, 4);
    head[8] = UTF8MB4_GENERAL_CI;
    if (tls) {
      this.send(head);
      await this.wire.startTls();
    }
    const auth = authResponse(plugin, ds.password, scramble);
    this.send(Buffer.concat([head, cstr(ds.user), Buffer.from([auth.length]), auth, cstr(ds.database), cstr(plugin)]));

    for (;;) {
      const b = await this.packet();
      if (b[0] === 0x00) return;
      if (b[0] === 0xff) throw serverError(b);
      if (b[0] === 0xfe) {
        const s = new Cursor(b, 1);
        plugin = s.cstr();
        const data = s.rest();
        scramble = data.at(-1) === 0 ? data.subarray(0, -1) : data;
        this.send(authResponse(plugin, ds.password, scramble));
        continue;
      }
      if (b[0] === 0x01 && plugin === 'caching_sha2_password' && b[1] === 3) continue;
      if (b[0] === 0x01 && plugin === 'caching_sha2_password' && b[1] === 4) {
        const pw = cstr(ds.password);
        if (tls) {
          this.send(pw);
        } else {
          // Unencrypted connection: encrypt the password with the server's public key
          this.send(Buffer.from([2]));
          const key = await this.packet();
          if (key[0] !== 0x01) throw new WireError('did not receive the MySQL server public key');
          this.send(publicEncrypt({ key: key.subarray(1).toString('utf8'), padding: constants.RSA_PKCS1_OAEP_PADDING }, xor(pw, scramble)));
        }
        continue;
      }
      throw new WireError(`unsupported MySQL auth step (${plugin})`);
    }
  }

  async query(sql: string, onRow?: (values: RawValue[]) => void, onColumns?: (columns: string[]) => void): Promise<{ columns: string[]; rows: number }> {
    this.send(Buffer.concat([Buffer.from([0x03]), Buffer.from(sql, 'utf8')]), true);
    const first = await this.packet();
    if (first[0] === 0xff) throw serverError(first);
    if (first[0] === 0x00) return { columns: [], rows: 0 };
    if (first[0] === 0xfb) throw new WireError('LOCAL INFILE requests are refused');
    const n = lenInt(new Cursor(first))!;
    const columns: string[] = [];
    for (let i = 0; i < n; i++) {
      const c = new Cursor(await this.packet());
      for (let k = 0; k < 4; k++) lenStr(c);
      columns.push(lenStr(c)!.toString('utf8'));
    }
    const eof = await this.packet();
    if (eof[0] !== 0xfe) throw new WireError('malformed MySQL response (end of column definitions)');
    onColumns?.(columns);
    let rows = 0;
    for (;;) {
      const b = await this.packet();
      if (b[0] === 0xfe && b.length < 9) break;
      if (b[0] === 0xff) throw serverError(b);
      const c = new Cursor(b);
      const values: RawValue[] = [];
      for (let i = 0; i < n; i++) values.push(lenStr(c)?.toString('utf8') ?? null);
      onRow?.(values);
      rows++;
    }
    return { columns, rows };
  }

  quit(): void {
    try {
      this.send(Buffer.from([0x01]), true);
    } catch {}
    this.wire.destroy();
  }
}

export class MysqlSource implements SourceAdapter {
  readonly dialect = 'mysql';
  private readonly ds: ServerDatasource;
  private wire: Wire | null = null;
  private aborted = false;

  constructor(ds: ServerDatasource) {
    this.ds = ds;
  }

  now(): Promise<string> {
    return singleValue(this, 'SELECT NOW(6) AS now');
  }

  async selectStream(sql: string, onRow: (values: RawValue[]) => void, onColumns?: (columns: string[]) => void): Promise<SelectResult> {
    const t0 = performance.now();
    if (this.aborted) throw new WireError('cancelled');
    const wire = await Wire.open(this.ds.host, this.ds.port);
    this.wire = wire;
    if (this.aborted) wire.destroy(new WireError('cancelled'));
    const conn = new Conn(wire);
    try {
      await conn.handshake(this.ds);
      await conn.query('SET SESSION TRANSACTION READ ONLY');
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

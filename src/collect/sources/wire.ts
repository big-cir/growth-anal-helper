// DB socket: reads wait until enough bytes arrive. Upgrades to TLS when the server supports it.
import { connect as netConnect, isIP, type Socket } from 'node:net';
import { connect as tlsConnect } from 'node:tls';

const CONNECT_TIMEOUT_MS = 30_000;

export class WireError extends Error {}

export class Wire {
  private sock!: Socket;
  private buf = Buffer.alloc(0);
  private wake: (() => void) | null = null;
  private failure: Error | null = null;
  private closed = false;
  private readonly host: string;

  private constructor(sock: Socket, host: string) {
    this.host = host;
    this.attach(sock);
  }

  static open(host: string, port: number): Promise<Wire> {
    return new Promise((resolve, reject) => {
      const sock = netConnect({ host, port });
      const fail = (e: Error) => {
        clearTimeout(timer);
        sock.destroy();
        reject(new WireError(`cannot connect to the database (${host}:${port}): ${e.message}`));
      };
      const timer = setTimeout(() => fail(new Error('timed out')), CONNECT_TIMEOUT_MS);
      sock.once('error', fail);
      sock.once('connect', () => {
        clearTimeout(timer);
        sock.removeListener('error', fail);
        resolve(new Wire(sock, host));
      });
    });
  }

  private attach(sock: Socket): void {
    this.sock = sock;
    sock.on('data', (d: Buffer) => {
      this.buf = this.buf.length ? Buffer.concat([this.buf, d]) : d;
      this.notify();
    });
    sock.on('error', (e) => {
      this.failure ??= new WireError(`database connection error: ${e.message}`);
      this.notify();
    });
    sock.on('close', () => {
      this.closed = true;
      this.notify();
    });
  }

  private notify(): void {
    const w = this.wake;
    this.wake = null;
    w?.();
  }

  async read(n: number): Promise<Buffer> {
    if (this.failure) throw this.failure;
    while (this.buf.length < n) {
      if (this.failure) throw this.failure;
      if (this.closed) throw new WireError('database connection closed');
      await new Promise<void>((r) => { this.wake = r; });
    }
    const out = this.buf.subarray(0, n);
    this.buf = this.buf.subarray(n);
    return out;
  }

  write(b: Buffer): void {
    this.sock.write(b);
  }

  /** The server certificate is not verified (same as the mysql and psql defaults) */
  async startTls(): Promise<void> {
    const raw = this.sock;
    raw.removeAllListeners('data');
    raw.removeAllListeners('error');
    raw.removeAllListeners('close');
    const tls = tlsConnect({ socket: raw, rejectUnauthorized: false, servername: isIP(this.host) ? undefined : this.host });
    await new Promise<void>((resolve, reject) => {
      tls.once('secureConnect', () => {
        tls.removeListener('error', reject);
        resolve();
      });
      tls.once('error', (e) => reject(new WireError(`TLS connection failed: ${e.message}`)));
    });
    this.attach(tls);
  }

  destroy(reason?: Error): void {
    if (reason) this.failure ??= reason;
    this.sock.destroy();
    this.notify();
  }
}

/** Packet body reader */
export class Cursor {
  pos = 0;
  readonly b: Buffer;

  constructor(b: Buffer, pos = 0) {
    this.b = b;
    this.pos = pos;
  }

  get done(): boolean {
    return this.pos >= this.b.length;
  }
  u8(): number {
    return this.b[this.pos++];
  }
  u16le(): number {
    const v = this.b.readUInt16LE(this.pos);
    this.pos += 2;
    return v;
  }
  u32le(): number {
    const v = this.b.readUInt32LE(this.pos);
    this.pos += 4;
    return v;
  }
  i16be(): number {
    const v = this.b.readInt16BE(this.pos);
    this.pos += 2;
    return v;
  }
  i32be(): number {
    const v = this.b.readInt32BE(this.pos);
    this.pos += 4;
    return v;
  }
  bytes(n: number): Buffer {
    const v = this.b.subarray(this.pos, this.pos + n);
    this.pos += n;
    return v;
  }
  /** Up to NUL (or the end if there is none) */
  cstr(): string {
    let end = this.b.indexOf(0, this.pos);
    if (end < 0) end = this.b.length;
    const v = this.b.toString('utf8', this.pos, end);
    this.pos = end + 1;
    return v;
  }
  rest(): Buffer {
    return this.bytes(this.b.length - this.pos);
  }
}

export const cstr = (s: string): Buffer => Buffer.from(`${s}\0`, 'utf8');

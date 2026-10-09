// Fake MySQL and PostgreSQL servers for tests: check auth per the real protocol and return fixed results.
import { createServer, type AddressInfo, type Server, type Socket } from 'node:net';
import { constants, createHash, createHmac, generateKeyPairSync, pbkdf2Sync, privateDecrypt, randomBytes } from 'node:crypto';

export type Table = { columns: string[]; rows: (string | null)[][] };
export type FakeDb = { port: number; queries: string[]; startup: Record<string, string>[]; close: () => Promise<void> };

/** Reads that wait until enough bytes arrive */
function reader(sock: Socket) {
  let buf = Buffer.alloc(0);
  let wake: (() => void) | null = null;
  let closed = false;
  sock.on('data', (d) => { buf = Buffer.concat([buf, d]); wake?.(); });
  sock.on('close', () => { closed = true; wake?.(); });
  sock.on('error', () => {});
  return async (n: number): Promise<Buffer | null> => {
    while (buf.length < n) {
      if (closed) return null;
      await new Promise<void>((r) => { wake = r; });
      wake = null;
    }
    const out = buf.subarray(0, n);
    buf = buf.subarray(n);
    return out;
  };
}

function listen(server: Server, queries: string[], startup: Record<string, string>[]): Promise<FakeDb> {
  return new Promise((resolve) => server.listen(0, '127.0.0.1', () => resolve({
    port: (server.address() as AddressInfo).port, queries, startup,
    close: () => new Promise((r) => server.close(() => r())),
  })));
}

const sha1 = (...b: Buffer[]) => createHash('sha1').update(Buffer.concat(b)).digest();
const sha256 = (...b: Buffer[]) => createHash('sha256').update(Buffer.concat(b)).digest();
const xor = (a: Buffer, b: Buffer) => Buffer.from(a.map((x, i) => x ^ b[i % b.length]));

function lenenc(b: Buffer | null): Buffer {
  if (b === null) return Buffer.from([0xfb]);
  if (b.length < 0xfb) return Buffer.concat([Buffer.from([b.length]), b]);
  const h = Buffer.from([0xfc, 0, 0]);
  h.writeUInt16LE(b.length, 1);
  return Buffer.concat([h, b]);
}

/** plugin: auth method the server announces first. fullAuth: caching_sha2 requires RSA full auth */
export function fakeMysql(o: { user: string; password: string; plugin?: 'mysql_native_password' | 'caching_sha2_password'; fullAuth?: boolean; table: Table; now?: string }): Promise<FakeDb> {
  const plugin = o.plugin ?? 'mysql_native_password';
  const queries: string[] = [];
  const startup: Record<string, string>[] = [];
  const { publicKey, privateKey } = generateKeyPairSync('rsa', { modulusLength: 2048 });
  const server = createServer(async (sock) => {
    const read = reader(sock);
    let seq = 0;
    const send = (p: Buffer) => {
      const h = Buffer.alloc(4);
      h.writeUIntLE(p.length, 0, 3);
      h[3] = seq++ & 0xff;
      sock.write(Buffer.concat([h, p]));
    };
    const recv = async () => {
      const h = await read(4);
      if (!h) return null;
      seq = h[3] + 1;
      return read(h.readUIntLE(0, 3));
    };
    const ok = () => send(Buffer.from([0, 0, 0, 2, 0, 0, 0]));
    const err = (msg: string) => send(Buffer.concat([Buffer.from([0xff, 0x28, 0x04]), Buffer.from(`#28000${msg}`)]));
    const eof = () => send(Buffer.from([0xfe, 0, 0, 2, 0]));

    const scramble = randomBytes(20).map((x) => (x % 94) + 33);
    const caps = 0x8 | 0x200 | 0x8000 | 0x80000 | 0x200000;
    const hs = Buffer.alloc(13);
    hs.writeUInt16LE(caps & 0xffff, 0);
    hs[2] = 45;
    hs.writeUInt16LE(caps >>> 16, 5);
    hs[7] = 21;
    send(Buffer.concat([Buffer.from([10]), Buffer.from('8.0.36-fake\0'), Buffer.alloc(4), scramble.subarray(0, 8), Buffer.from([0]), hs.subarray(0, 3), Buffer.from([2, 0]), hs.subarray(5, 8), Buffer.alloc(10), scramble.subarray(8), Buffer.from([0]), Buffer.from(`${plugin}\0`)]));

    const resp = await recv();
    if (!resp) return;
    let at = 32;
    const user = resp.toString('utf8', at, resp.indexOf(0, at));
    at = resp.indexOf(0, at) + 1;
    const auth = resp.subarray(at + 1, at + 1 + resp[at]);
    at += 1 + resp[at];
    const database = resp.toString('utf8', at, resp.indexOf(0, at));
    startup.push({ user, database });
    const pw = Buffer.from(o.password);
    const expected = plugin === 'mysql_native_password' ? xor(sha1(pw), sha1(scramble, sha1(sha1(pw)))) : xor(sha256(pw), sha256(sha256(sha256(pw)), scramble));
    if (user !== o.user || !auth.equals(expected)) return err(`Access denied for user '${user}'`);
    if (plugin === 'caching_sha2_password') {
      if (!o.fullAuth) {
        send(Buffer.from([1, 3]));
      } else {
        send(Buffer.from([1, 4]));
        const req = await recv();
        if (!req || req[0] !== 2) return err('expected key request');
        send(Buffer.concat([Buffer.from([1]), Buffer.from(publicKey.export({ type: 'spki', format: 'pem' }))]));
        const enc = await recv();
        if (!enc) return;
        const plain = xor(privateDecrypt({ key: privateKey, padding: constants.RSA_PKCS1_OAEP_PADDING }, enc), scramble);
        if (!plain.equals(Buffer.concat([pw, Buffer.from([0])]))) return err('bad full auth');
      }
    }
    ok();

    for (;;) {
      const p = await recv();
      if (!p || p[0] === 0x01) return sock.end();
      const sql = p.subarray(1).toString('utf8');
      queries.push(sql);
      if (/^SET /i.test(sql)) { ok(); continue; }
      if (/fail/.test(sql)) { err('Unknown column'); continue; }
      const t: Table = /NOW\(6\)/.test(sql) ? { columns: ['now'], rows: [[o.now ?? '2024-03-20 12:00:00.123456']] } : o.table;
      send(Buffer.from([t.columns.length]));
      for (const c of t.columns) send(Buffer.concat([...['def', 'db', 't', 't', c, c].map((x) => lenenc(Buffer.from(x))), Buffer.from([0x0c, 45, 0, 0, 0, 0, 0, 253, 0, 0, 0, 0, 0])]));
      eof();
      for (const r of t.rows) send(Buffer.concat(r.map((v) => lenenc(v === null ? null : Buffer.from(v)))));
      eof();
    }
  });
  return listen(server, queries, startup);
}

/** auth: SCRAM-SHA-256 or md5 */
export function fakePostgres(o: { user: string; password: string; auth?: 'scram' | 'md5'; table: Table; now?: string }): Promise<FakeDb> {
  const queries: string[] = [];
  const startup: Record<string, string>[] = [];
  const server = createServer(async (sock) => {
    const read = reader(sock);
    const send = (type: string, body: Buffer) => {
      const h = Buffer.alloc(5);
      h.write(type, 0, 'latin1');
      h.writeInt32BE(body.length + 4, 1);
      sock.write(Buffer.concat([h, body]));
    };
    const int32 = (n: number) => { const b = Buffer.alloc(4); b.writeInt32BE(n, 0); return b; };
    const int16 = (n: number) => { const b = Buffer.alloc(2); b.writeInt16BE(n, 0); return b; };
    const recv = async () => {
      const h = await read(5);
      if (!h) return null;
      return { type: String.fromCharCode(h[0]), body: (await read(h.readInt32BE(1) - 4))! };
    };
    const error = (msg: string) => send('E', Buffer.concat([Buffer.from(`SERROR\0C28P01\0M${msg}\0`), Buffer.from([0])]));

    const sslLen = await read(4);
    if (!sslLen) return;
    await read(sslLen.readInt32BE(0) - 4);
    sock.write('N');
    const len = await read(4);
    if (!len) return;
    const body = (await read(len.readInt32BE(0) - 4))!;
    const parts = body.subarray(4).toString('utf8').split('\0');
    const params: Record<string, string> = {};
    for (let i = 0; i + 1 < parts.length && parts[i]; i += 2) params[parts[i]] = parts[i + 1];
    startup.push(params);

    if (o.auth === 'md5') {
      const salt = randomBytes(4);
      send('R', Buffer.concat([int32(5), salt]));
      const m = await recv();
      const inner = createHash('md5').update(o.password + o.user).digest('hex');
      const want = `md5${createHash('md5').update(Buffer.concat([Buffer.from(inner), salt])).digest('hex')}`;
      if (!m || m.body.toString('utf8').replace(/\0$/, '') !== want || params.user !== o.user) return error('password authentication failed');
    } else {
      send('R', Buffer.concat([int32(10), Buffer.from('SCRAM-SHA-256\0\0')]));
      const init = await recv();
      if (!init) return;
      const c = init.body;
      const mechEnd = c.indexOf(0);
      const clientFirst = c.subarray(mechEnd + 5).toString('utf8');
      const bare = clientFirst.slice(3);
      const clientNonce = /r=([^,]+)/.exec(bare)![1];
      const nonce = clientNonce + randomBytes(12).toString('base64');
      const salt = randomBytes(16);
      const serverFirst = `r=${nonce},s=${salt.toString('base64')},i=4096`;
      send('R', Buffer.concat([int32(11), Buffer.from(serverFirst)]));
      const fin = await recv();
      if (!fin) return;
      const clientFinal = fin.body.toString('utf8');
      const withoutProof = clientFinal.slice(0, clientFinal.indexOf(',p='));
      const proof = Buffer.from(clientFinal.slice(clientFinal.indexOf(',p=') + 3), 'base64');
      const salted = pbkdf2Sync(o.password, salt, 4096, 32, 'sha256');
      const hmac = (k: Buffer, s: string) => createHmac('sha256', k).update(s).digest();
      const authMessage = `${bare},${serverFirst},${withoutProof}`;
      const storedKey = sha256(hmac(salted, 'Client Key'));
      const clientKey = xor(proof, hmac(storedKey, authMessage));
      if (!sha256(clientKey).equals(storedKey) || !withoutProof.endsWith(nonce) || params.user !== o.user) return error('password authentication failed');
      send('R', Buffer.concat([int32(12), Buffer.from(`v=${hmac(hmac(salted, 'Server Key'), authMessage).toString('base64')}`)]));
    }
    send('R', int32(0));
    send('S', Buffer.from('server_version\x0016.2\0'));
    send('K', Buffer.alloc(8));
    send('Z', Buffer.from('I'));

    for (;;) {
      const m = await recv();
      if (!m || m.type === 'X') return sock.end();
      const sql = m.body.toString('utf8').replace(/\0$/, '');
      queries.push(sql);
      if (/fail/.test(sql)) {
        error('column "x" does not exist');
      } else {
        const t: Table = /LOCALTIMESTAMP/.test(sql) ? { columns: ['now'], rows: [[o.now ?? '2024-03-20 12:00:00.123456']] } : o.table;
        send('T', Buffer.concat([int16(t.columns.length), ...t.columns.map((c) => Buffer.concat([Buffer.from(`${c}\0`), Buffer.alloc(18)]))]));
        for (const r of t.rows) send('D', Buffer.concat([int16(r.length), ...r.map((v) => (v === null ? int32(-1) : Buffer.concat([int32(Buffer.byteLength(v)), Buffer.from(v)])))]));
        send('C', Buffer.from(`SELECT ${t.rows.length}\0`));
      }
      send('Z', Buffer.from('I'));
    }
  });
  return listen(server, queries, startup);
}

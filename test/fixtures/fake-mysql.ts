// 가짜 mysql 클라이언트: 시나리오별 배치 출력.
import { readFileSync, writeFileSync } from 'node:fs';

const scenario = process.argv[2];
const sqlLog = process.argv[3];
const input = readFileSync(0, 'utf8');
if (sqlLog) writeFileSync(sqlLog, input);

const out = (s: string) => process.stdout.write(s);
switch (scenario) {
  case 'now':
    out('now\n2024-03-20 12:00:00.123456\n');
    break;
  case 'rows':
    process.stderr.write('mysql: [Warning] Using a password on the command line interface can be insecure.\n');
    process.stderr.write('real diagnostic line\n');
    out('id\tname\tnote\n');
    out('1\tsAlice\tsNULL\n');
    out('2\tsTab\\there\tNULL\n');
    out('3\tsLine\\nBreak\\\\x\tsok\n');
    break;
  case 'badheader':
    out('id\tWRONG\tts\n1\tsA\t2024-01-01 00:00:00\n');
    break;
  case 'shortrow':
    out('id\tname\tnote\n1\tsA\n');
    break;
  case 'fail':
    process.stderr.write('ERROR 1146 (42S02): Table does not exist\n');
    process.exit(1);
}

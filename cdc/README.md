# growth-lab-cdc

A separate process that reads a MySQL binlog. It is its own Go module and does not import growth-lab code.

```bash
cd cdc
go build -o ../bin/growth-lab-cdc ./cmd/growth-lab-cdc
go test ./...
```

The root `scripts/go test ./...` does not include this module; run its tests here.

## Commands

`inspect` reads the binlog in a range and counts changed rows per table in committed transactions. It applies nothing and prints counts only.

```bash
bin/growth-lab-cdc inspect --defaults-file client.cnf --database-include-list app --table-include-list 'app\.orders,app\.users'
```

- `--database-include-list` (required) and `--table-include-list` (optional) take the names and format of Debezium's `database.include.list` and `table.include.list`: comma-separated regular expressions matched against the whole database or `db.table` name. Without a table list, every table of the matched databases is followed. System databases are never followed.
- What differs from Debezium: the database list is required; the table set is fixed when the read starts (a table that appears later stops the read); patterns use Go regular expression syntax.
- Only row events are counted. A statement-format change in a followed database is counted as a statement record and left out of the row counts. A statement is matched by the database selected when it ran, so one that names a followed table while another database is selected is not seen.
- A schema change (`CREATE`, `ALTER`, `DROP`, `RENAME`, `TRUNCATE`) in a followed database stops the read.

The source account needs `REPLICATION SLAVE`, `REPLICATION CLIENT` and `SELECT` on the followed databases. The server must use `binlog_format=ROW` and `binlog_row_image=FULL`.


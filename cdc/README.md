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

`init` builds a fresh copy of the tables on a target MySQL server and swaps it in; `run` then keeps it up to date.

```bash
bin/growth-lab-cdc init --defaults-file source.cnf --target-defaults-file target.cnf --database app --tables orders,users
```

- Needs only `SELECT` and the replication privileges on the source: no global or table locks. It records the binlog position before and just after opening a consistent snapshot, copies the tables from that snapshot into `growth_lab_cdc_staging` (primary key only), replays the binlog from the first position by overwriting by key until the first transaction end past the second, then replays as `run` does.
- It then rebuilds the secondary indexes, catches up again and swaps the staging tables in with one `RENAME TABLE`. The previous tables move to `growth_lab_cdc_old`; removing them is left to you. Without previous tables it is a first install.
- The phase is stored with the position. Run `init` again after a stop or a crash and it resumes: a copy starts over, a replay continues, an index rebuild adds what is missing, and a swap is completed or found done from where this init's tables are, by their InnoDB table ids (reading them needs the `PROCESS` privilege on the target). Anything else, such as a table replaced by hand, stops it.
- `init` and `run` never write at the same time: both hold a named lock on the target, and `run` starts only once `init` has finished. Taking the lock raises a number in `growth_lab_cdc.lock_epoch` that every write checks in its transaction, so a process that lost the lock cannot write even before it notices. Table changes (creating, indexing, swapping) run on the lock's own connection, so the lock outlasts them.
- The copy must finish within half the source's binlog retention (or `--max-copy-minutes`), so the replay still finds its start.

`run` applies source changes to the same tables on a target MySQL server and stores its position there.

```bash
bin/growth-lab-cdc run --defaults-file source.cnf --target-defaults-file target.cnf \
  --database app --tables orders,users --from mysql-bin.000012:4567
```

- `--tables` are exact names. After `init`, `run` continues from the stored position. Without `init`, the first run needs `--from`, the source position the target's data was taken at; later runs continue from the position stored in the target (`growth_lab_cdc.position`). The table set and the table definitions are stored too, and a later run with a different set or changed definitions stops. A position stored by a version without `init` stops too: remove its row and run `init`.
- Each source transaction becomes one target transaction together with the new position, so a crash or a lost connection never applies a transaction twice or skips one.
- Inserts are inserted, updates and deletes match the row's previous values (strings by bytes). A duplicate key, or an update or delete that does not match exactly one row, means the target differs from the source, and the run stops instead of overwriting.
- It also stops on schema changes, statement-format changes outside the system databases, tables with triggers or foreign keys in the target, unsupported column types, and a binlog that does not continue from the stored position. Connection failures and uncertain commits reconnect from the stored position.
- Ctrl+C or SIGTERM finishes the transaction being applied, stores the position and exits.

The target account needs to create the `growth_lab_cdc` database and write the followed tables.

### Integration tests

They need two MySQL servers they may write to (they create and drop the databases `app`, `app_other` and `growth_lab_cdc` on both):

```bash
cd cdc
go build -o ../bin/growth-lab-cdc ./cmd/growth-lab-cdc
CDC_SRC_CNF=source.cnf CDC_DST_CNF=target.cnf CDC_BIN=$PWD/../bin/growth-lab-cdc go test -tags integration ./internal/cdc/
```

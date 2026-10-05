# Testing on Windows

## First, the honest part

The code did not build on Windows at all until now — it used `mmap`, `openat`
and `flock`, none of which exist there. That is fixed: Windows gets its own
page backend and its own file-open path.

What was verified here, on Linux:

- **It compiles for Windows** (`amd64` and `386`), and `go vet` is clean for
  Windows including all test files.
- **The full test suite passes against the exact page backend Windows will
  use.** That backend is selectable on any platform with `-tags byodb_pread`,
  so all 4,900 lines of tests — crash injection, concurrency, `SIGKILL`
  recovery — ran against it, under the race detector.

What was **not** verified, because there is no Windows machine here:

- The Windows file-open and locking path (`kv/open_windows.go`, about 50
  lines). It cross-compiles and the API use is straightforward, but it has
  never run.
- Anything about Windows filesystem behaviour: `FlushFileBuffers` durability
  on your particular disk, antivirus interference, paths with spaces.

So: the engine is well covered, the Windows-specific 50 lines are not. If
something fails, **that file is the first place to look.** Step 3 below tests
it directly.

## What you need

Go 1.21 or newer. Check with `go version`. If it is missing, get it from
<https://go.dev/dl/> — the `.msi` installer puts `go` on your `PATH`.

Extract `byodb-complete.tar.gz` (Windows 10+ can do this with
`tar -xzf byodb-complete.tar.gz` in PowerShell, or use 7-Zip). Then:

```powershell
cd byodb
```

## Step 1 — Build and run the tests

```powershell
go build ./...
go test ./...
```

Expect roughly:

```
ok      byodb/btree        2.0s
ok      byodb/integration  4.5s
ok      byodb/kv           15.0s
ok      byodb/ql           0.9s
ok      byodb/relational   5.5s
```

Times will differ; Windows is generally slower here because every page read is
a system call rather than a memory access.

Then the stricter run — the race detector needs `gcc` on Windows, so it is
optional:

```powershell
go vet ./...
go test -race ./...     # needs gcc: install from https://jmeubank.github.io/tdm-gcc/
```

If `-race` complains about a missing C compiler, skip it. The important runs
are the ones above.

## Step 2 — Use it

```powershell
go build -o byodb.exe ./cmd/byodb
.\byodb.exe test.db
```

Then type this in, one statement at a time (statements end with `;` and may
span lines):

```sql
create table people (
    country bytes,
    id int64,
    name bytes,
    age int64,
    index (age),
    primary key (country, id)
);

insert into people (country, id, name, age) values
    ('in', 1, 'arjun', 30),
    ('in', 2, 'bhavna', 45),
    ('uk', 1, 'clara', 22),
    ('uk', 2, 'david', 61);

select name, age from people index by country = 'in';
select name from people index by age > 25 filter country != 'uk';
update people set age = age + 1 index by (country, id) = ('in', 1);
delete from people filter age > 60;
select country, id, name, age from people;
.schema people
.quit
```

Reopen it — `.\byodb.exe test.db` — and run the last `select` again. The rows
should still be there. That is the whole point of the exercise.

A single statement from the command line works too:

```powershell
.\byodb.exe test.db "select name from people index by age > 25;"
```

Note that `'` is fine inside a PowerShell double-quoted string, which is why
the examples quote the statement with `"`.

## Step 3 — Test the Windows-specific parts

These are the two things Linux could not check.

### Exclusive access

On Unix this is an advisory `flock`. On Windows it is a `CreateFile` with a
share mode of zero, which is a different mechanism entirely — so it is worth
confirming directly.

Open the database in one PowerShell window:

```powershell
.\byodb.exe test.db
```

Leave it at the prompt. In a **second** window:

```powershell
.\byodb.exe test.db "select name from people;"
```

**Expected:**

```
cannot open test.db: kv: database is already open in another process: test.db
```

If instead it opens successfully, the exclusivity is not working and **you
should stop and report it** — two writers will silently destroy each other's
data. That is the single most important thing to check on Windows.

Close the first window and confirm the second now works.

### Durability across a real kill

The test suite does this automatically (`TestKillMidWrite` in
`integration/`), and it uses `Process.Kill`, which is `TerminateProcess` on
Windows. To watch it happen by hand:

```powershell
# Window 1: write rows one at a time
.\byodb.exe crash.db "create table log (id int64, v bytes, primary key (id));"
1..2000 | ForEach-Object { .\byodb.exe crash.db "insert into log (id, v) values ($_, 'row-$_');" | Out-Null }
```

While that runs, close the window or kill the process from Task Manager. Then:

```powershell
.\byodb.exe crash.db "select id from log;"
```

Every row the loop finished should be there, and the file should open without
complaint. A partially written row, or a refusal to open, would be a real
failure.

## Step 4 — Check performance is sane

```powershell
go test -run "^$" -bench . -benchmem ./integration/
```

On Linux with `mmap` this gives about:

| | |
|---|---|
| Point query | ~620 ns |
| Batched insert | ~18 µs |

Windows uses explicit reads with a page cache in front, so expect point
queries to be somewhat slower — perhaps 1–3 µs — and scans similar. **An order
of magnitude worse would be worth reporting**; it would suggest the cache is
not doing its job.

You can measure the same backend on Linux for a direct comparison:

```
go test -run "^$" -bench . -tags byodb_pread ./integration/
```

## If something goes wrong

**It does not compile.** Paste the error. Anything mentioning `syscall` is a
platform gap I have missed.

**A test fails.** Run it alone with detail:

```powershell
go test -run TestNameThatFailed -v ./kv/
```

Go's failure output names the file and line, and the messages are written to
say what the expected behaviour was, not just what differed.

**`TestKillMidWrite` fails or hangs.** It launches a subprocess of the test
binary. Windows Defender sometimes delays that. Re-run it alone; if it fails
consistently, report it — it is the most Windows-sensitive test.

**Antivirus locks the file.** Defender occasionally holds a handle briefly
after a process exits, which can look like the exclusivity check misfiring. Add
your working folder to Defender's exclusions if you see intermittent
`already open in another process` errors with nothing else running.

**Paths with spaces.** Quote them: `.\byodb.exe "C:\My Data\test.db"`.

## The easy alternative

If any of this turns into a fight, WSL2 runs the Linux build unchanged and with
the faster `mmap` backend:

```powershell
wsl --install                 # once, then reboot
wsl
sudo apt install golang-go
cd /mnt/c/path/to/byodb && go test ./...
```

That is worth knowing about, but it does dodge the question — it tests the
Linux paths, not the Windows ones. If the point is to know whether this works
on Windows, run Step 3 natively.

## What changed to make this work

| File | Purpose |
|---|---|
| `kv/pages_mmap.go` | Memory-mapped page reads. Everything except Windows. |
| `kv/pages_pread.go` | Explicit reads with a FIFO page cache. Windows, and any platform with `-tags byodb_pread`. |
| `kv/open_windows.go` | `CreateFile` with exclusive sharing — the open and the lock in one step. |
| `kv/open_unix.go` | Directory fsync plus `flock`. |
| `kv/open_linux.go` | `openat`, which is Linux-only in the standard library. |
| `kv/open_other.go` | Plain open, for macOS and the BSDs. |

The build-tag on the portable backend is the part that matters for confidence:
without it, the Windows code path would only ever run on Windows, which is
exactly where it is hardest to test.

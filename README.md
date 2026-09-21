# file-locks

Exclusive advisory file locks for Go, on linux, darwin and windows.

```go
import fslock "github.com/dolthub/file-locks"
```

The package is `fslock` and its interface is the one
`github.com/dolthub/fslock` presents, so replacing that library is a change of
import path and nothing else.

```go
lck, err := fslock.New(filepath.Join(dir, "LOCK"))
if err != nil {
        return err
}
defer lck.Close()

if err := lck.LockWithTimeout(100 * time.Millisecond); err != nil {
        if errors.Is(err, fslock.ErrTimeout) {
                return openReadOnly(dir) // someone else is working here
        }
        return err
}
defer lck.Unlock()

return update(dir)
```

Go 1.23 or newer. The only dependency is `golang.org/x/sys`.

## The interface

| | |
| --- | --- |
| `fslock.New(path) (*Lock, error)` | open (creating) the lock file; takes no lock |
| `lck.TryLock() error` | one attempt; `ErrLocked` if someone else has it |
| `lck.LockWithTimeout(d) error` | wait up to `d`, then `ErrTimeout`; `d <= 0` means one attempt |
| `lck.Lock() error` | wait as long as it takes |
| `lck.LockWithContext(ctx) error` | wait until `ctx` says otherwise — the one addition |
| `lck.Unlock() error` | give the lock back |
| `lck.Close() error` | unlock if held, then close the file |

### Errors

Every error is an `*fslock.Error` carrying an operation, a path, a `Kind` and
a cause. Two of them are values to compare against:

```go
err == fslock.ErrLocked           // TryLock, when someone else holds it
errors.Is(err, fslock.ErrTimeout) // a wait that ran out of time
```

Both forms work: the sentinels are returned as those exact values, and an
error of the same `Kind` matches them through `errors.Is`.

For everything else there are two questions worth asking, and the error
answers them without anyone enumerating kinds:

```go
fslock.IsTimeout(err)   // waiting ran out, nothing is wrong
fslock.IsTemporary(err) // trying again later might work
```

`IsTemporary` is true for contention, for timeouts, for a lock file that kept
being replaced, and for system calls like `ENOLCK` that can come and go. It is
false for a closed or misused `Lock`, a cancelled wait, and failures such as a
missing directory. The same pair are methods on `*Error`.

## What it guarantees

- **Exclusion.** While a `Lock` is held, no other `Lock` in this process and no
  other process using this package can take it.
- **Same-process exclusion is not an accident.** Two `Lock`s in one process
  exclude each other exactly as two processes do. POSIX record locks (`fcntl`)
  belong to the *process* and would quietly hand the second one a lock that is
  already held; this package does not use them.
- **Nothing to clean up.** The OS owns the lock, so it is dropped when the
  `Lock` is unlocked or closed, when the process exits, and when the process is
  killed or crashes. This package never removes a lock file, and a lock file
  left behind by a dead process is not stale — it is just a file.

## Two races it is careful about

**A descriptor must not reach a child process.** The `os` package notes a race
between opening a file and marking it close-on-exec — "There's a race here with
fork/exec, which we are content to live with", in `os/file_unix.go` and again in
`os/root_unix.go` where `(*os.Root).OpenFile` gets its descriptor. `os` is
content to live with it because an inherited descriptor is usually just a leak.
For a lock it is not: an `flock(2)` lock belongs to the open file description, so
a child that inherits one keeps the lock alive after the process that took it is
gone — a program that shells out while holding a lock would strand locks behind
it. So lock files are opened with `os.OpenFile`, which passes `O_CLOEXEC`
atomically where the platform has it and takes the fork lock where it does not,
and every later system call goes through the file's `SyscallConn` rather than a
descriptor kept on the side.

**A lock on an unlinked file excludes nobody.** Opening a lock file and locking
it are two steps, and the path can be replaced in between: someone unlinks it,
creates a new one, and now two holders hold locks on two different files, each
believing it has the only one. After taking the lock and before reporting
success, this package checks that the file it locked is still the file at the
path, and reopens and retries if it is not. On windows the question does not
arise — an open lock file cannot be unlinked — so the check simply always
passes.

Both have tests that fail if the protection is removed.

## How it works

| GOOS | primitive | waiting |
| --- | --- | --- |
| linux, darwin | `flock(2)`, `LOCK_EX` | retry with jittered backoff (1ms doubling to 16ms) |
| windows | `LockFileEx` on a one-byte range | `WaitForMultipleObjects` over the request's event and a cancellation event |

Both primitives belong to the open file description or handle rather than to
the process, and both are released by the kernel when the process goes away.

The difference in waiting is forced by the platforms. A thread blocked in
`flock(2)` cannot be told the caller gave up — there is no handle to signal and
no cancellation to hand it — so on unix a bounded wait is a retry loop, which
also means a wait wakes up on a schedule rather than the instant the lock is
free. `LockFileEx` takes an `OVERLAPPED`, so on windows the wait happens in the
kernel and a second event, signalled when the context ends, breaks it.

Before the OS primitive, an acquire takes a slot in a process-wide table keyed
by the file's identity on disk. On a local filesystem that is redundant. It is
there for filesystems where the primitive is weaker than advertised — Linux
implements `flock` on NFS with POSIX record locks — so that same-process
exclusion holds regardless of where the lock file lives.

Platforms other than the three above do not compile. A file lock that silently
fails to exclude is worse than one that is missing.

## Limits

- The lock is **advisory**: it binds the participants that ask for it, and
  nothing else.
- On **NFS or SMB**, cross-process exclusion is only as good as the mount.
  Same-process exclusion still holds. Keep lock files on local storage.
- Waiters are **not queued**: the one that has waited longest has no claim to
  go first.
- A `Lock` is **not recursive**: taking one it already holds is an error, not a
  second acquisition.

## locktool

For trying things by hand, and for testing another program's locking from a
shell:

```
go run ./cmd/locktool hold <path> [duration]   take the lock and keep it
go run ./cmd/locktool try  <path>              is it free? (exit 3 if not)
go run ./cmd/locktool wait <path> <duration>   wait for it   (exit 3 if not)
```

## Testing

```
go test ./...
go test -race ./...
```

The suite uses real processes: it re-executes the test binary, kills a holder
outright to check the lock dies with it, and has one child leak-test that takes
a lock, starts a process that outlives it, and dies — the lock has to go.

## License

Apache 2.0. See [LICENSE](LICENSE).

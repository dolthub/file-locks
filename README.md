# file-locks

Cross-platform advisory file locking for Go, in one small package.

`filelock` coordinates access to a resource among cooperating processes using an
advisory lock on a file. It is built for the case a database has: several
processes, or several goroutines, that must take turns updating files in a
directory, where the loser of the race needs to find out quickly and do
something else.

```go
import filelock "github.com/dolthub/file-locks"
```

Supported on **linux**, **darwin** and **windows**. Go 1.23 or newer. The only
dependency is `golang.org/x/sys`.

## Using it

A `Guard` is one participant's claim on one lock file. Open it once, then take
and give back the claim as often as you like.

```go
g, err := filelock.Open(filepath.Join(dir, "LOCK"))
if err != nil {
        return err
}
defer g.Close()

if err := g.AcquireWithin(ctx, 100*time.Millisecond); err != nil {
        if errors.Is(err, filelock.ErrWaitExpired) {
                // Someone else is working in dir. Fall back to read-only.
                return openReadOnly(dir)
        }
        return err
}
defer g.Release()

return update(dir)
```

Three ways to take the claim, depending on how long you are willing to wait:

| | |
| --- | --- |
| `g.AcquireNow() (bool, error)` | one attempt; `false, nil` means someone else has it |
| `g.AcquireWithin(ctx, limit) error` | wait up to `limit`, then [`ErrWaitExpired`] |
| `g.Acquire(ctx) error` | wait until `ctx` says otherwise |

and the rest of the type:

| | |
| --- | --- |
| `filelock.Open(path, opts...) (*Guard, error)` | open (creating) the lock file; takes no claim |
| `g.Release() error` | give the claim back |
| `g.Close() error` | release if held, then close the handle |
| `g.Holding() bool`, `g.Path() string` | what this guard is doing, and to what |
| `g.ReadHolder() (Holder, error)` | who holds it, if they left a stamp |
| `filelock.Hold(ctx, path, limit, fn) error` | open, acquire, run `fn`, release, close |

Options to `Open`: `WithHolderStamp()`, `WithFileMode(mode)`,
`WithRetrySchedule(min, max)`.

Errors, all matched with `errors.Is`: `ErrWaitExpired` (which also reports as
`context.DeadlineExceeded`), `ErrAlreadyHolding`, `ErrNotHolding`,
`ErrGuardClosed`, `ErrNoHolderStamp`. Running out of time is the only outcome
most callers need to branch on, and it is not a failure — the lock file is
fine, and trying again later may well work.

## What it guarantees

- **Exclusion.** While a guard holds the claim, no other guard in this process
  and no other process using this package can take it.
- **Same-process exclusion is not an accident.** Two guards in one process
  exclude each other exactly as two processes do. This is the part most file
  locking gets wrong: POSIX record locks (`fcntl`) belong to the *process*, so
  a second guard would be told it won.
- **Nothing to clean up.** The OS owns the claim, so it is dropped when the
  guard is released or closed, when the process exits, and when the process is
  killed or crashes. The package never removes the lock file, and a lock file
  left behind by a dead process is not stale — it is just a file.
- **A wait can always be abandoned.** Every waiting call takes a
  `context.Context` and honours it, including cancellation, because waiting is
  retrying rather than blocking in the kernel.

A `*Guard` is safe for concurrent use. It is not recursive: acquiring one that
is already acquired reports `ErrAlreadyHolding` rather than appearing to
succeed.

## How it works

| GOOS | primitive |
| --- | --- |
| linux, darwin | `flock(2)`, `LOCK_EX \| LOCK_NB` |
| windows | `LockFileEx` on a one-byte range, `LOCKFILE_EXCLUSIVE_LOCK \| LOCKFILE_FAIL_IMMEDIATELY` |

Both belong to the open file description or handle rather than to the process,
which is what makes same-process exclusion work, and both are released by the
kernel when the process goes away.

Waiting is retrying with jittered backoff (1ms doubling to 16ms by default)
rather than parking in a blocking `flock` or an overlapped `LockFileEx`. A
thread parked in the kernel cannot be told to stop, so a cancelled wait would
either leak or come back later and take a claim nobody is waiting for any more.
The cost of the choice is that there is no FIFO fairness: waiters do not queue.

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
- The **holder stamp** (`WithHolderStamp`) is a comment for people reading
  error messages, never an input to a decision. It can describe a holder that
  has already gone.

## locktool

A small command for trying things by hand, and for testing another program's
locking from a shell:

```
go run ./cmd/locktool hold <path> [duration]   take the claim and keep it
go run ./cmd/locktool try  <path>              is it free? (exit 3 if not)
go run ./cmd/locktool wait <path> <duration>   wait for it   (exit 3 if not)
go run ./cmd/locktool who  <path>              print the holder stamp
```

## Testing

```
go test ./...
go test -race ./...
```

The suite exercises the cross-process guarantees for real: it re-executes the
test binary as a second process, and kills a holder outright to check that the
claim dies with it.

## License

Apache 2.0. See [LICENSE](LICENSE).

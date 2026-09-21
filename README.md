# file-locks

Exclusive advisory file locks for Go, on linux, darwin and windows.

```go
import fslock "github.com/dolthub/file-locks"
```

The package is `fslock` and keeps the interface of `github.com/dolthub/fslock`,
so replacing that library is a change of import path.

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
```

`New`, `Lock`, `TryLock`, `LockWithTimeout`, `Unlock`, `Close`, `ErrLocked` and
`ErrTimeout` are as they were. `LockWithContext(ctx)` is the one addition.

Every error is an `*fslock.Error` with `IsTimeout()` and `IsTemporary()`
methods, and matching `fslock.IsTimeout(err)` / `fslock.IsTemporary(err)`
helpers. `ErrLocked` and `ErrTimeout` are returned as those exact values, so
`err == fslock.ErrLocked` works alongside `errors.Is`.

The lock is `flock(2)` on unix and `LockFileEx` on windows. Both belong to the
open file rather than to the process, so two `Lock`s in one process exclude
each other exactly as two processes do, with no extra bookkeeping, and the
kernel releases the lock when the process exits or is killed. A wait is a retry
loop on unix, where nothing can interrupt `flock(2)`, and a kernel wait on
windows, where `LockFileEx` can be broken by an event.

Caveats: the lock is advisory; over NFS or SMB the guarantees are only as good
as the mount, so keep lock files on local storage; waiters are not queued; a
`Lock` is not recursive. Other platforms do not compile.

Apache 2.0.

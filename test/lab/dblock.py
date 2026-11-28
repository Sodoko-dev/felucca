#!/usr/bin/env python3
"""Hold SQLite's write lock on a database for N seconds (test/lab/persist.sh, FLU-5).

    sudo -u <service user> python3 dblock.py <db> <seconds> [release-file]

Opens the database with a 30 s busy timeout, takes the write lock with
`BEGIN IMMEDIATE` (a RESERVED lock: readers in WAL mode go on, every other
writer gets SQLITE_BUSY after its own busy timeout), prints `LOCKED <time>`,
and holds it until <seconds> have passed or <release-file> exists. Then it rolls
back (nothing was written) and prints `RELEASED <time>`. SIGTERM, SIGINT and
SIGHUP release it the same way; a process that is killed outright releases it
too, because SQLite's locks are POSIX locks of the process.

Run it as the user the control plane runs as: a connection opened as root can
create the -wal and -shm files owned by root, which the service then cannot
open for writing.
"""
import os
import signal
import sqlite3
import sys
import time


def stamp():
    t = time.time()
    return time.strftime("%H:%M:%S", time.localtime(t)) + ".%03d" % int((t % 1) * 1000)


class Stop(Exception):
    pass


def on_signal(signum, _frame):
    raise Stop("signal %d" % signum)


def main():
    if len(sys.argv) < 3:
        print(__doc__.strip().splitlines()[2].strip(), file=sys.stderr)
        return 2
    db, secs = sys.argv[1], float(sys.argv[2])
    release = sys.argv[3] if len(sys.argv) > 3 else None
    for s in (signal.SIGTERM, signal.SIGINT, signal.SIGHUP):
        signal.signal(s, on_signal)
    con = sqlite3.connect(db, timeout=30, isolation_level=None)
    locked = False
    try:
        t0 = time.time()
        con.execute("BEGIN IMMEDIATE")
        locked = True
        print("LOCKED %s (waited %.3fs)" % (stamp(), time.time() - t0), flush=True)
        end = time.time() + secs
        while time.time() < end:
            if release and os.path.exists(release):
                print("release file seen", flush=True)
                break
            time.sleep(0.05)
    except Stop as exc:
        print("stopping early: %s" % exc, flush=True)
    except sqlite3.Error as exc:
        print("ERROR %s" % exc, flush=True)
        return 1
    finally:
        if locked:
            try:
                con.execute("ROLLBACK")
            except sqlite3.Error as exc:
                print("rollback: %s" % exc, flush=True)
        con.close()
        print("RELEASED %s" % stamp(), flush=True)
    return 0


if __name__ == "__main__":
    sys.exit(main())

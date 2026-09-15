# Chromium lifetime guardian

`xhs-browser-guardian.c` is the fixed child-lifetime primitive approved by Flywheel ruling d40b384d. Compile with `cc -std=c11 -Wall -Wextra -Werror -O2`. It is not yet wired into provider startup; production still uses the existing direct owner until the pinned launcher and durable profile reconciliation are connected.

Arguments: trusted budget in milliseconds (1–240000), 64 lowercase hex nonce, absolute pinned browser executable and its fixed arguments. The caller must verify binary pins, create a private profile identity containing account/generation/UID and the same nonce, and open an exclusive empty 0600 cleanup file. No model values may select commands or duration.

Inherited descriptors:

| FD | Purpose |
| --- | --- |
| 3 | Chromium CDP command read pipe |
| 4 | Chromium CDP response write pipe |
| 5 | Provider-liveness read pipe; provider alone owns writer |
| 6 | Exclusive empty cleanup file |
| 7 | Startup status write pipe (`START <child-pid>`) |

The child receives only CDP 3/4 from this contract. Guardian closes its copies and never reads CDP. It resets SIGCHLD so it retains an unreaped child even if that child exits naturally. Liveness EOF, unexpected input or a hard deadline triggers a kill of exactly that reserved child process group, then waitpid, then a nonce/PID cleanup receipt and fsync. It has no network or policy code and never scans process tables. Exit 64 means invalid contract, 70 an internal/cleanup failure, 71 exec failed after child cleanup; a successful child launch and confirmed cleanup exit 0.

A missing/empty/malformed/mismatched receipt must never authorize profile removal or startup recovery. Provider integration must bind this receipt to the durable identity and exact profile inode before cleanup. If guardian itself dies, retain the existing refusal of unaccounted profiles; do not recover by PID scans. Real-process tests use synthetic children and do not launch Chrome or exercise a real account.

#define _POSIX_C_SOURCE 200809L
#include <errno.h>
#include <fcntl.h>
#include <limits.h>
#include <poll.h>
#include <signal.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/stat.h>
#include <sys/types.h>
#include <sys/wait.h>
#include <time.h>
#include <unistd.h>

/* Fixed inherited contract: Chromium CDP input/output 3/4, provider-liveness
 * read pipe 5, exclusive empty cleanup file 6, startup status write pipe 7.
 * No sockets, network, policy decisions or PID lookup. The caller pins this
 * binary and Chromium and creates the profile identity before spawning us. */
static int64_t monotonic_ms(void) {
    struct timespec t;
    if (clock_gettime(CLOCK_MONOTONIC, &t) != 0) return -1;
    return (int64_t)t.tv_sec * 1000 + t.tv_nsec / 1000000;
}
static int write_all(int fd, const char *data, size_t size) {
    while (size) {
        ssize_t n = write(fd, data, size);
        if (n < 0 && errno == EINTR) continue;
        if (n <= 0) return -1;
        data += n; size -= (size_t)n;
    }
    return 0;
}
static int pipe_fd(int fd, int access) {
    struct stat s;
    int flags = fcntl(fd, F_GETFL);
    return flags >= 0 && (flags & O_ACCMODE) == access &&
        fstat(fd, &s) == 0 && S_ISFIFO(s.st_mode);
}
int main(int argc, char **argv) {
    if (argc < 4 || getuid() == 0 || getuid() != geteuid() || getgid() != getegid()) return 64;
    char *end = NULL;
    errno = 0;
    long budget = strtol(argv[1], &end, 10);
    if (errno || !end || *end || budget < 1 || budget > 240000 ||
        strlen(argv[2]) != 64 || strspn(argv[2], "0123456789abcdef") != 64 || argv[3][0] != '/') return 64;
    if (!pipe_fd(3, O_RDONLY) || !pipe_fd(4, O_WRONLY) ||
        !pipe_fd(5, O_RDONLY) || !pipe_fd(7, O_WRONLY)) return 64;
    struct stat receipt;
    int flags = fcntl(6, F_GETFL);
    if (flags < 0 || (flags & O_ACCMODE) == O_RDONLY ||
        fstat(6, &receipt) != 0 || !S_ISREG(receipt.st_mode) ||
        receipt.st_uid != geteuid() || (receipt.st_mode & 07777) != 0600 ||
        receipt.st_nlink != 1 || receipt.st_size != 0 || lseek(6, 0, SEEK_SET) != 0) return 64;
    if (signal(SIGCHLD, SIG_DFL) == SIG_ERR || signal(SIGPIPE, SIG_IGN) == SIG_ERR) return 70;
    int exec_status[2];
    if (pipe(exec_status) != 0) return 70;
    if (fcntl(exec_status[1], F_SETFD, FD_CLOEXEC) != 0) return 70;
    int64_t now = monotonic_ms();
    if (now < 0) return 70;
    int64_t deadline = now + budget;
    pid_t child = fork();
    if (child < 0) return 70;
    if (child == 0) {
        close(exec_status[0]);
        close(5); close(6); close(7);
        if (setpgid(0, 0) != 0 || signal(SIGPIPE, SIG_DFL) == SIG_ERR) goto failed_exec;
        execv(argv[3], argv + 3);
failed_exec:;
        char error = 'E';
        (void)write(exec_status[1], &error, 1);
        _exit(127);
    }
    close(exec_status[1]);
    close(3); close(4);
    /* The child also sets its group before exec. Never reap before the kill;
     * even a naturally exited child reserves its PID and group identity. */
    (void)setpgid(child, child);
    char startup_error;
    ssize_t started;
    do { started = read(exec_status[0], &startup_error, 1); } while (started < 0 && errno == EINTR);
    close(exec_status[0]);
    int valid_start = started == 0;
    char status[64];
    int length = snprintf(status, sizeof(status), "START %ld\n", (long)child);
    int should_wait = valid_start && length > 0 && (size_t)length < sizeof(status) &&
        write_all(7, status, (size_t)length) == 0;
    close(7);
    while (should_wait) {
        now = monotonic_ms();
        if (now < 0 || now >= deadline) break;
        struct pollfd live = { .fd = 5, .events = POLLIN | POLLHUP };
        int result = poll(&live, 1, (int)(deadline - now));
        if (result < 0 && errno == EINTR) continue;
        if (result != 0) break; /* EOF, unexpected bytes, invalid fd or error all clean up. */
    }
    close(5);
    int killed = kill(-child, SIGKILL);
    int kill_ok = killed == 0 || errno == ESRCH;
    int child_status;
    pid_t reaped;
    do { reaped = waitpid(child, &child_status, 0); } while (reaped < 0 && errno == EINTR);
    if (!kill_ok || reaped != child) return 70;
    char proof[256];
    length = snprintf(proof, sizeof(proof),
        "{\"schemaVersion\":1,\"nonce\":\"%s\",\"pid\":%ld,\"cleaned\":true}\n", argv[2], (long)child);
    if (length <= 0 || (size_t)length >= sizeof(proof) ||
        write_all(6, proof, (size_t)length) != 0 || fsync(6) != 0 || close(6) != 0) return 70;
    return valid_start ? 0 : 71;
}
